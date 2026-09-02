package transportregistry

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/configloader"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/desireclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/dryrun"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/manifest"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildCreatesRemoteClientsForEachConfiguredName(t *testing.T) {
	config := loadRuntimeConfig(t, `
adapter:
  name: test-adapter
stores:
  desired-memory:
    type: memory
transports:
  remote-primary:
    type: remote
    store: desired-memory
  remote-secondary:
    type: remote
    store: desired-memory
`)

	runtime, err := Build(t.Context(), config)
	require.NoError(t, err)
	require.NotNil(t, runtime)
	t.Cleanup(func() { assert.NoError(t, runtime.Close()) })

	primary, err := runtime.Registry.Get("remote-primary")
	require.NoError(t, err)
	assert.NotNil(t, primary)

	secondary, err := runtime.Registry.Get("remote-secondary")
	require.NoError(t, err)
	assert.NotNil(t, secondary)

	primaryResult, err := primary.ApplyResource(
		t.Context(),
		testConfigMapManifest,
		nil,
		testTransportContext(),
	)
	require.NoError(t, err)
	assert.Equal(t, manifest.OperationCreate, primaryResult.Operation)

	secondaryResult, err := secondary.ApplyResource(
		t.Context(),
		testConfigMapManifest,
		nil,
		testTransportContext(),
	)
	require.NoError(t, err)
	assert.Equal(t, manifest.OperationSkip, secondaryResult.Operation,
		"transports using the same named store must see the same desired state")
}

func TestBuildPingsRedisStoreBeforeReturning(t *testing.T) {
	config := loadRuntimeConfig(t, `
adapter:
  name: test-adapter
stores:
  unreachable:
    type: redis
    url: redis://127.0.0.1:1/0
transports:
  remote:
    type: remote
    store: unreachable
`)

	runtime, err := Build(t.Context(), config)
	require.Error(t, err)
	assert.Nil(t, runtime)
	assert.Contains(t, err.Error(), "unreachable")
}

func TestBuildCreatesAndClosesReachableRedisStore(t *testing.T) {
	server := miniredis.RunT(t)
	config := loadRuntimeConfig(t, `
adapter:
  name: test-adapter
stores:
  redis-store:
    type: redis
    url: redis://`+server.Addr()+`/0
transports:
  remote:
    type: remote
    store: redis-store
`)

	runtime, err := Build(t.Context(), config)
	require.NoError(t, err)
	require.NotNil(t, runtime)

	client, err := runtime.Registry.Get("remote")
	require.NoError(t, err)
	assert.NotNil(t, client)
	assert.NoError(t, runtime.Close())
}

func TestBuildRejectsMissingConfiguration(t *testing.T) {
	runtime, err := Build(t.Context(), nil)

	require.Error(t, err)
	assert.Nil(t, runtime)
	assert.Contains(t, err.Error(), "config is required")
}

func TestBuildRejectsInvalidNamedTransportBeforeStartingService(t *testing.T) {
	tests := []struct {
		name   string
		config *configloader.Config
		want   string
	}{
		{
			name: "remote transport references absent store",
			config: &configloader.Config{
				Transports: map[string]configloader.TransportDefinition{
					"remote": {
						Type:  configloader.TransportTypeRemote,
						Store: "missing",
					},
				},
			},
			want: "store \"missing\" is not configured",
		},
		{
			name: "unsupported transport type",
			config: &configloader.Config{
				Transports: map[string]configloader.TransportDefinition{
					"unknown": {Type: "unsupported"},
				},
			},
			want: "unsupported type \"unsupported\"",
		},
		{
			name: "Kubernetes transport has unusable kubeconfig",
			config: &configloader.Config{
				Transports: map[string]configloader.TransportDefinition{
					"kubernetes": {Type: configloader.TransportTypeKubernetes},
				},
				Clients: configloader.ClientsConfig{
					Kubernetes: configloader.KubernetesConfig{
						KubeConfigPath: "/does/not/exist",
					},
				},
			},
			want: "failed to load kubeconfig",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime, err := Build(t.Context(), tt.config)

			require.Error(t, err)
			assert.Nil(t, runtime)
			assert.ErrorContains(t, err, tt.want)
		})
	}
}

func TestBuildLegacyRejectsInvalidMaestroConfiguration(t *testing.T) {
	config := &configloader.Config{Clients: configloader.ClientsConfig{
		Maestro: &configloader.MaestroClientConfig{Timeout: "not-a-duration"},
	}}

	runtime, err := Build(t.Context(), config)

	require.Error(t, err)
	assert.Nil(t, runtime)
	assert.ErrorContains(t, err, "invalid maestro timeout")
}

func TestBuildPreservesLegacyKubernetesDefault(t *testing.T) {
	kubeconfigPath := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(kubeconfigPath, []byte(`
apiVersion: v1
kind: Config
clusters:
  - name: test
    cluster:
      server: https://127.0.0.1:65535
contexts:
  - name: test
    context:
      cluster: test
      user: test
current-context: test
users:
  - name: test
    user:
      token: test-token
`), 0644))
	config := &configloader.Config{Clients: configloader.ClientsConfig{
		Kubernetes: configloader.KubernetesConfig{
			KubeConfigPath: kubeconfigPath,
		},
	}}

	runtime, err := Build(t.Context(), config)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, runtime.Close()) })

	client, err := runtime.Registry.Get(configloader.TransportClientKubernetes)
	require.NoError(t, err)
	assert.NotNil(t, client)
}

func TestRuntimeCloseAttemptsAllClosersAndIsIdempotent(t *testing.T) {
	firstErr := errors.New("first close failed")
	first := &testCloser{err: firstErr}
	second := &testCloser{err: errors.New("second close failed")}
	runtime := &Runtime{closers: []io.Closer{first, second}}

	err := runtime.Close()

	assert.ErrorIs(
		t,
		err,
		second.err,
		"closers are released in reverse construction order",
	)
	assert.Equal(t, 1, first.calls)
	assert.Equal(t, 1, second.calls)
	assert.NoError(t, runtime.Close())
	assert.Equal(t, 1, first.calls)
	assert.Equal(t, 1, second.calls)
}

func TestBuildRecordingRejectsMissingInputs(t *testing.T) {
	tests := []struct {
		client transportclient.TransportClient
		config *configloader.Config
		name   string
	}{
		{name: "nil config", client: dryrun.NewDryrunTransportClient()},
		{name: "nil recording client", config: &configloader.Config{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime, err := BuildRecording(tt.config, tt.client)

			require.Error(t, err)
			assert.Nil(t, runtime)
		})
	}
}

func TestBuildRecordingMapsConfiguredNamesAndCompatibilityDefault(
	t *testing.T,
) {
	config := loadRuntimeConfig(t, `
adapter:
  name: test-adapter
stores:
  desired-memory:
    type: memory
transports:
  remote-primary:
    type: remote
    store: desired-memory
  remote-secondary:
    type: remote
    store: desired-memory
`)
	recorder := dryrun.NewDryrunTransportClient()

	runtime, err := BuildRecording(config, recorder)
	require.NoError(t, err)
	require.NotNil(t, runtime)

	for _, name := range []string{"remote-primary", "remote-secondary", "kubernetes"} {
		client, err := runtime.Registry.Get(name)
		require.NoErrorf(t, err, "expected recording client for %q", name)
		assert.Same(t, recorder, client)
	}
}

func TestBuildRecordingPreservesLegacyKubernetesAndMaestroDefaults(
	t *testing.T,
) {
	tests := []struct {
		name   string
		config string
		key    string
	}{
		{
			name: "legacy Kubernetes configuration",
			config: `
adapter:
  name: test-adapter
clients:
  kubernetes:
    api_version: v1
`,
			key: "kubernetes",
		},
		{
			name: "legacy Maestro configuration",
			config: `
adapter:
  name: test-adapter
clients:
  maestro:
    source_id: test-adapter
`,
			key: "maestro",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime, err := BuildRecording(
				loadRuntimeConfig(t, tt.config),
				dryrun.NewDryrunTransportClient(),
			)
			require.NoError(t, err)
			require.NotNil(t, runtime)

			client, err := runtime.Registry.Get(tt.key)
			require.NoError(t, err)
			assert.NotNil(t, client)
		})
	}
}

func loadRuntimeConfig(t *testing.T, adapterYAML string) *configloader.Config {
	t.Helper()

	adapterPath, taskPath := createRuntimeConfigFiles(t, adapterYAML, `{}`)
	config, err := configloader.LoadConfig(
		configloader.WithAdapterConfigPath(adapterPath),
		configloader.WithTaskConfigPath(taskPath),
		configloader.WithSkipSemanticValidation(),
	)
	require.NoError(t, err)
	return config
}

func createRuntimeConfigFiles(
	t *testing.T,
	adapterYAML, taskYAML string,
) (string, string) {
	t.Helper()

	tmpDir := t.TempDir()
	adapterPath := filepath.Join(tmpDir, "adapter-config.yaml")
	taskPath := filepath.Join(tmpDir, "task-config.yaml")
	require.NoError(t, os.WriteFile(adapterPath, []byte(adapterYAML), 0644))
	require.NoError(t, os.WriteFile(taskPath, []byte(taskYAML), 0644))
	return adapterPath, taskPath
}

func testTransportContext() *desireclient.TransportContext {
	return &desireclient.TransportContext{
		ManagementCluster: "management-cluster",
		Resource:          "configmaps",
	}
}

var testConfigMapManifest = []byte(`{
  "apiVersion": "v1",
  "kind": "ConfigMap",
  "metadata": {
    "name": "registry-test",
    "namespace": "default",
    "annotations": {"hyperfleet.io/generation": "1"}
  }
}`)

type testCloser struct {
	err   error
	calls int
}

func (c *testCloser) Close() error {
	c.calls++
	return c.err
}
