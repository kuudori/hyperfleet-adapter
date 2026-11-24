package transportregistry

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/configloader"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/desireclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/desirestore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	testServiceAccount = "system:serviceaccount:hyperfleet:cluster-adapter"
	testSchemaVersion  = "2.0"
	kindConfigMap      = "ConfigMap"
	kindCluster        = "Cluster"
	fieldKind          = "kind"
)

func writeProjectedToken(t *testing.T, sub string) string {
	t.Helper()
	enc := base64.RawURLEncoding
	token := enc.EncodeToString([]byte(`{"alg":"RS256"}`)) + "." +
		enc.EncodeToString([]byte(`{"aud":["hyperfleet-desire-store"],"sub":"`+sub+`"}`)) + ".c2ln"
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte(token), 0o600))
	return path
}

func apiStoreConfig(t *testing.T, storeFields string) *configloader.Config {
	t.Helper()
	config := loadRuntimeConfig(t, `
adapter:
  name: test-adapter
stores:
  desire-store:
    type: api
    url: https://gateway.example
`+storeFields+`
transports:
  remote-primary:
    type: remote
    target_cluster: cluster-1
    resource_plurals:
      "v1/ConfigMap": configmaps
    store: desire-store
`)
	config.Resources = []configloader.Resource{testRemoteResource("remote", "remote-primary")}
	config.SchemaVersion = testSchemaVersion
	return config
}

func TestBuildCreatesAPIStoreWithTokenSubjectAsOwner(t *testing.T) {
	tokenPath := writeProjectedToken(t, testServiceAccount)
	config := apiStoreConfig(t, "    token_path: "+tokenPath)

	runtime, err := Build(t.Context(), config)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, runtime.Close()) })
	entry, err := runtime.Registry.Get("remote-primary")
	require.NoError(t, err)
	assert.IsType(t, &desireclient.Client{}, entry.Client)

	stores, err := (&Runtime{}).buildStores(t.Context(), config)
	require.NoError(t, err)
	store := stores["desire-store"]
	assert.IsType(t, &desirestore.Store{}, store.store)
	assert.Equal(t, testServiceAccount, store.owner, "the owner is the token subject, not adapter.name")
}

// TestBuiltAPITransportWritesAsTokenSubject applies through the built
// transport. The store client refuses, before any request, a write whose
// owner is not its token subject, so a PUT reaching the server shows that
// the transport writes as the subject.
func TestBuiltAPITransportWritesAsTokenSubject(t *testing.T) {
	var mu sync.Mutex
	var puts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPut {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			writeTestBody(t, w, `{"type":"t","title":"Not Found","status":404,"code":"HYPERFLEET-NTF-001"}`)
			return
		}
		id := path.Base(r.URL.Path)
		mu.Lock()
		puts = append(puts, id)
		mu.Unlock()
		parts := strings.SplitN(id, ":", 5)
		if len(parts) != 5 {
			t.Errorf("unexpected desire id %q", id)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		writeTestBody(t, w, fmt.Sprintf(`{"id":%q,"type":%q,"group":%q,"resource":%q,"namespace":%q,"name":%q,`+
			`"version":1,"generation":1,"owner":%q,"resource_id":"cls-1","target_version":"v1"}`,
			id, parts[0], parts[1], parts[2], parts[3], parts[4], testServiceAccount))
	}))
	t.Cleanup(srv.Close)
	config := apiStoreConfig(t, "    token_path: "+writeProjectedToken(t, testServiceAccount))
	definition := config.Stores["desire-store"]
	definition.URL = srv.URL
	config.Stores["desire-store"] = definition

	runtime, err := Build(t.Context(), config)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, runtime.Close()) })
	entry, err := runtime.Registry.Get("remote-primary")
	require.NoError(t, err)
	tc := testTransportContext()
	tc.ResourceID = "cls-1"
	_, err = entry.Client.ApplyResource(t.Context(), testConfigMapManifest, nil, tc)
	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"read::configmaps:default:registry-test", "apply::configmaps:default:registry-test"}, puts)
}

func writeTestBody(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	if _, err := w.Write([]byte(body)); err != nil {
		t.Logf("write response: %v", err)
	}
}

func TestBuildAPIStoreFailures(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-token")
	_, err := Build(t.Context(), apiStoreConfig(t, "    token_path: "+missing))
	require.ErrorContains(t, err, `build store "desire-store"`)
	require.ErrorContains(t, err, "enable adapterConfig.desireStore.auth or set stores.desire-store.token_path")

	emptyCA := filepath.Join(t.TempDir(), "ca.crt")
	require.NoError(t, os.WriteFile(emptyCA, nil, 0o600))
	tokenPath := writeProjectedToken(t, testServiceAccount)
	_, err = Build(t.Context(), apiStoreConfig(t, "    token_path: "+tokenPath+"\n    ca_file: "+emptyCA))
	require.ErrorContains(t, err, "contains no PEM certificate")
}

func TestTargetSetsResourceIDFromEvent(t *testing.T) {
	target := Target("remote-primary", testRemoteDefinition("desired-memory"))
	configMap := schema.GroupVersionKind{Version: "v1", Kind: kindConfigMap}

	var dryRunEvent struct {
		Data map[string]any `json:"data"`
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "test", "testdata", "dryrun", "event.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &dryRunEvent))

	tests := []struct {
		name  string
		event any
		want  string
	}{
		{"cluster event", map[string]any{"id": "2b7f4c3a", fieldKind: kindCluster}, "2b7f4c3a"},
		{"nodepool event", map[string]any{
			"id": "np-42", fieldKind: "NodePool",
			"owner_references": map[string]any{"id": "cls-1", fieldKind: kindCluster},
		}, "np-42"},
		{"dry-run event", dryRunEvent.Data, "abc123"},
		{"event without id", map[string]any{fieldKind: kindCluster}, ""},
		{"id that is not a string", map[string]any{"id": 42}, ""},
		{"no event", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route, err := target(configMap, map[string]any{configloader.FieldEvent: tt.event})
			require.NoError(t, err)
			tc, ok := route.(*desireclient.TransportContext)
			require.True(t, ok)
			assert.Equal(t, tt.want, tc.ResourceID)
		})
	}
}

// TestChartTokenPathMatchesStoreDefault keeps the chart's projected token
// mount and the store's default token_path the same, so an api store works
// with adapterConfig.desireStore.auth enabled and no token_path.
func TestChartTokenPathMatchesStoreDefault(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "values.yaml"))
	require.NoError(t, err)
	var values struct {
		AdapterConfig struct {
			DesireStore struct {
				Auth struct {
					TokenPath string `yaml:"tokenPath"`
					Audience  string `yaml:"audience"`
				} `yaml:"auth"`
			} `yaml:"desireStore"`
		} `yaml:"adapterConfig"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &values))
	auth := values.AdapterConfig.DesireStore.Auth
	assert.Equal(t, desirestore.DefaultTokenPath, auth.TokenPath)
	assert.Equal(t, "hyperfleet-desire-store", auth.Audience)
	assert.True(t, strings.HasPrefix(auth.TokenPath, "/"))
}
