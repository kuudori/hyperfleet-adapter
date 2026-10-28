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
	"k8s.io/apimachinery/pkg/runtime/schema"
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
    target_cluster: cluster-1
    resource_plurals:
      "v1/ConfigMap": configmaps
    store: desired-memory
  remote-secondary:
    type: remote
    target_cluster: cluster-1
    resource_plurals:
      "v1/ConfigMap": configmaps
    store: desired-memory
`)
	config.Resources = []configloader.Resource{testRemoteResource("remote", "remote-primary")}
	config.SchemaVersion = "2.0"

	runtime, err := Build(t.Context(), config)
	require.NoError(t, err)
	require.NotNil(t, runtime)
	t.Cleanup(func() { assert.NoError(t, runtime.Close()) })

	primary, err := runtime.Registry.Get("remote-primary")
	require.NoError(t, err)
	assert.NotNil(t, primary.Target)

	secondary, err := runtime.Registry.Get("remote-secondary")
	require.NoError(t, err)
	assert.NotNil(t, secondary.Target)

	_, err = runtime.Registry.Get(configloader.TransportClientKubernetes)
	require.Error(t, err)

	primaryResult, err := primary.Client.ApplyResource(
		t.Context(),
		testConfigMapManifest,
		nil,
		testTransportContext(),
	)
	require.NoError(t, err)
	assert.Equal(t, manifest.OperationCreate, primaryResult.Operation)

	secondaryResult, err := secondary.Client.ApplyResource(
		t.Context(),
		testConfigMapManifest,
		nil,
		testTransportContext(),
	)
	require.NoError(t, err)
	assert.Equal(t, manifest.OperationSkip, secondaryResult.Operation,
		"transports using the same named store must see the same desired state")
}

func TestBuildRecordingUsesLocalDefaultOnlyWhenReferenced(t *testing.T) {
	config := &configloader.Config{
		SchemaVersion: "2.0",
		Stores: map[string]configloader.StoreDefinition{
			"desired-memory": {Type: configloader.StoreTypeMemory},
		},
		Transports: map[string]configloader.TransportDefinition{
			"remote-primary": testRemoteDefinition("desired-memory"),
		},
		Resources: []configloader.Resource{
			{Name: "local", Manifest: testConfigMapManifestMap()},
			testRemoteResource("remote", "remote-primary"),
		},
	}
	recorder := dryrun.NewDryrunTransportClient()
	runtime, err := BuildRecording(config, recorder)
	require.NoError(t, err)
	local, err := runtime.Registry.Get(configloader.TransportClientKubernetes)
	require.NoError(t, err)
	remote, err := runtime.Registry.Get("remote-primary")
	require.NoError(t, err)
	assert.Same(t, recorder, local.Client)
	assert.Same(t, recorder, remote.Client)
	assert.Nil(t, local.Target)
	assert.NotNil(t, remote.Target)

	config.Resources = config.Resources[1:]
	runtime, err = BuildRecording(config, recorder)
	require.NoError(t, err)
	_, err = runtime.Registry.Get(configloader.TransportClientKubernetes)
	require.Error(t, err)
}

func TestBuildRecordingRejectsRemoteNamedKubernetes(t *testing.T) {
	config := &configloader.Config{Transports: map[string]configloader.TransportDefinition{
		"kubernetes": {Type: configloader.TransportTypeRemote},
	}}
	runtime, err := BuildRecording(config, dryrun.NewDryrunTransportClient())
	require.ErrorContains(t, err, "name kubernetes is reserved for local Kubernetes")
	assert.Nil(t, runtime)
}

func TestBuildOmitsKubernetesForSingleUnreferencedNamedTransport(t *testing.T) {
	config := loadRuntimeConfig(t, `
adapter:
  name: test-adapter
stores:
  desired-memory:
    type: memory
transports:
  remote:
    type: remote
    target_cluster: cluster-1
    resource_plurals:
      "v1/ConfigMap": configmaps
    store: desired-memory
`)

	runtime, err := Build(t.Context(), config)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, runtime.Close()) })

	named, err := runtime.Registry.Get("remote")
	require.NoError(t, err)
	_, err = runtime.Registry.Get(configloader.TransportClientKubernetes)
	require.Error(t, err)
	assert.NotNil(t, named.Client)
}

func TestBuildOmitsKubernetesForMultipleUnreferencedNamedTransports(t *testing.T) {
	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter"},
		Stores: map[string]configloader.StoreDefinition{
			"desired-memory": {Type: configloader.StoreTypeMemory},
		},
		Transports: map[string]configloader.TransportDefinition{
			"remote-primary":   testRemoteDefinition("desired-memory"),
			"remote-secondary": testRemoteDefinition("desired-memory"),
		},
	}

	runtime, err := Build(t.Context(), config)

	require.NoError(t, err)
	require.NotNil(t, runtime)
	t.Cleanup(func() { assert.NoError(t, runtime.Close()) })
	_, err = runtime.Registry.Get(configloader.TransportClientKubernetes)
	require.Error(t, err)
}

func TestBuildRejectsCanonicalStoreNameCollisions(t *testing.T) {
	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter"},
		Stores: map[string]configloader.StoreDefinition{
			"Desired-Memory": {Type: configloader.StoreTypeMemory},
			"desired-memory": {Type: configloader.StoreTypeMemory},
		},
		Transports: map[string]configloader.TransportDefinition{
			"remote": {
				Type:  configloader.TransportTypeRemote,
				Store: "desired-memory",
			},
		},
	}

	runtime, err := Build(t.Context(), config)

	require.Error(t, err)
	assert.Nil(t, runtime)
	assert.ErrorContains(t, err, "validate named transport configuration")
	assert.ErrorContains(t, err, `resolve to the same name "desired-memory"`)
}

func TestBuildRecordingRejectsCanonicalStoreNameCollisions(t *testing.T) {
	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter"},
		Stores: map[string]configloader.StoreDefinition{
			"Desired-Memory": {Type: configloader.StoreTypeMemory},
			"desired-memory": {Type: configloader.StoreTypeMemory},
		},
		Transports: map[string]configloader.TransportDefinition{
			"remote": {
				Type:  configloader.TransportTypeRemote,
				Store: "desired-memory",
			},
		},
	}

	runtime, err := BuildRecording(config, dryrun.NewDryrunTransportClient())

	require.Error(t, err)
	assert.Nil(t, runtime)
	assert.ErrorContains(t, err, "validate named transport configuration")
	assert.ErrorContains(t, err, `resolve to the same name "desired-memory"`)
}

func TestBuildMatchesStoreReferencesCaseInsensitively(t *testing.T) {
	config := loadRuntimeConfig(t, `
adapter:
  name: test-adapter
stores:
  Desired-Memory:
    type: memory
transports:
  Remote-Primary:
    type: remote
    target_cluster: cluster-1
    resource_plurals:
      "v1/ConfigMap": configmaps
    store: Desired-Memory
`)

	runtime, err := Build(t.Context(), config)
	require.NoError(t, err)
	require.NotNil(t, runtime)
	t.Cleanup(func() { assert.NoError(t, runtime.Close()) })

	entry, err := runtime.Registry.Get("remote-primary")
	require.NoError(t, err)
	assert.NotNil(t, entry.Client)
}

func TestBuildRejectsCanonicalTransportNameCollisions(t *testing.T) {
	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter"},
		Stores: map[string]configloader.StoreDefinition{
			"desired-memory": {Type: configloader.StoreTypeMemory},
		},
		Transports: map[string]configloader.TransportDefinition{
			"Remote-Primary": {Type: configloader.TransportTypeRemote, Store: "desired-memory"},
			"remote-primary": {Type: configloader.TransportTypeRemote, Store: "desired-memory"},
		},
	}

	runtime, err := Build(t.Context(), config)

	require.Error(t, err)
	assert.Nil(t, runtime)
	assert.ErrorContains(t, err, "validate named transport configuration")
	assert.ErrorContains(t, err, `resolve to the same name "remote-primary"`)
}

func TestBuildRecordingRejectsCanonicalTransportNameCollisions(t *testing.T) {
	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter"},
		Stores: map[string]configloader.StoreDefinition{
			"desired-memory": {Type: configloader.StoreTypeMemory},
		},
		Transports: map[string]configloader.TransportDefinition{
			"Remote-Primary": {Type: configloader.TransportTypeRemote, Store: "desired-memory"},
			"remote-primary": {Type: configloader.TransportTypeRemote, Store: "desired-memory"},
		},
	}

	runtime, err := BuildRecording(config, dryrun.NewDryrunTransportClient())

	require.Error(t, err)
	assert.Nil(t, runtime)
	assert.ErrorContains(t, err, "validate named transport configuration")
	assert.ErrorContains(t, err, `resolve to the same name "remote-primary"`)
}

func TestBuildNormalizesConfiguredTransportNames(t *testing.T) {
	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter"},
		Stores: map[string]configloader.StoreDefinition{
			"Desired-Memory": {Type: configloader.StoreTypeMemory},
		},
		Transports: map[string]configloader.TransportDefinition{
			"Remote-Primary": testRemoteDefinition("Desired-Memory"),
		},
	}

	runtime, err := Build(t.Context(), config)
	require.NoError(t, err)
	require.NotNil(t, runtime)
	t.Cleanup(func() { assert.NoError(t, runtime.Close()) })

	entry, err := runtime.Registry.Get("remote-primary")
	require.NoError(t, err)
	assert.NotNil(t, entry.Client)

	_, err = runtime.Registry.Get(configloader.TransportClientKubernetes)
	require.Error(t, err)
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
    target_cluster: cluster-1
    resource_plurals:
      "v1/ConfigMap": configmaps
    store: unreachable
`)

	runtime, err := Build(t.Context(), config)
	require.Error(t, err)
	assert.Nil(t, runtime)
	assert.Contains(t, err.Error(), "unreachable")
}

func TestBuildDoesNotExposeCredentialsFromInvalidRedisURL(t *testing.T) {
	const password = "super-secret"
	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter"},
		Stores: map[string]configloader.StoreDefinition{
			"credentials": {
				Type: configloader.StoreTypeRedis,
				URL:  "redis://adapter:" + password + "@redis.example.com:%",
			},
		},
		Transports: map[string]configloader.TransportDefinition{
			"remote": {Type: configloader.TransportTypeRemote, Store: "credentials"},
		},
	}

	runtime, err := Build(t.Context(), config)

	require.Error(t, err)
	assert.Nil(t, runtime)
	assert.ErrorContains(t, err, "credentials")
	assert.NotContains(t, err.Error(), password)
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
    target_cluster: cluster-1
    resource_plurals:
      "v1/ConfigMap": configmaps
    store: redis-store
`)

	runtime, err := Build(t.Context(), config)
	require.NoError(t, err)
	require.NotNil(t, runtime)

	entry, err := runtime.Registry.Get("remote")
	require.NoError(t, err)
	assert.NotNil(t, entry.Client)
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
					"remote": testRemoteDefinition("missing"),
				},
			},
			want: "references unknown store \"missing\"",
		},
		{
			name: "remote transport has no target cluster",
			config: &configloader.Config{
				Stores: map[string]configloader.StoreDefinition{
					"desired-memory": {Type: configloader.StoreTypeMemory},
				},
				Transports: map[string]configloader.TransportDefinition{
					"remote": {
						Type:            configloader.TransportTypeRemote,
						Store:           "desired-memory",
						ResourcePlurals: map[string]string{"v1/ConfigMap": "configmaps"},
					},
				},
			},
			want: "target_cluster is required for remote transport",
		},
		{
			name: "remote transport has no resource plurals",
			config: &configloader.Config{
				Stores: map[string]configloader.StoreDefinition{
					"desired-memory": {Type: configloader.StoreTypeMemory},
				},
				Transports: map[string]configloader.TransportDefinition{
					"remote": {
						Type:          configloader.TransportTypeRemote,
						Store:         "desired-memory",
						TargetCluster: "cluster-1",
					},
				},
			},
			want: "resource_plurals is required for remote transport",
		},
		{
			name: "resource kind has no plural on its remote transport",
			config: &configloader.Config{
				SchemaVersion: "2.0",
				Stores: map[string]configloader.StoreDefinition{
					"desired-memory": {Type: configloader.StoreTypeMemory},
				},
				Transports: map[string]configloader.TransportDefinition{
					"remote": testRemoteDefinition("desired-memory"),
				},
				Resources: []configloader.Resource{{
					Name:      "namespace",
					Transport: configloader.NamedTransport("remote"),
					Manifest:  map[string]interface{}{"apiVersion": "v1", "kind": "Namespace"},
				}},
			},
			want: "GVK v1/Namespace has no resource_plurals mapping",
		},
		{
			name: "unsupported transport type",
			config: &configloader.Config{
				Transports: map[string]configloader.TransportDefinition{
					"unknown": {Type: "unsupported"},
				},
			},
			want: "type \"unsupported\" is unsupported",
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

func TestBuildRegistersLocalKubernetesWithoutTransports(t *testing.T) {
	kubeconfigPath := writeTestKubeconfig(t)
	config := &configloader.Config{
		Clients: configloader.ClientsConfig{
			Kubernetes: configloader.KubernetesConfig{
				KubeConfigPath: kubeconfigPath,
			},
		},
		Resources: []configloader.Resource{{Name: "local", Manifest: testConfigMapManifestMap()}},
	}

	runtime, err := Build(t.Context(), config)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, runtime.Close()) })

	entry, err := runtime.Registry.Get(configloader.TransportClientKubernetes)
	require.NoError(t, err)
	assert.NotNil(t, entry.Client)
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
    target_cluster: cluster-1
    resource_plurals:
      "v1/ConfigMap": configmaps
    store: desired-memory
  remote-secondary:
    type: remote
    target_cluster: cluster-1
    resource_plurals:
      "v1/ConfigMap": configmaps
    store: desired-memory
`)
	recorder := dryrun.NewDryrunTransportClient()

	runtime, err := BuildRecording(config, recorder)
	require.NoError(t, err)
	require.NotNil(t, runtime)

	for _, name := range []string{"remote-primary", "remote-secondary"} {
		entry, err := runtime.Registry.Get(name)
		require.NoErrorf(t, err, "expected recording client for %q", name)
		assert.Same(t, recorder, entry.Client)
	}
}

func TestBuildRecordingWithoutTransports(t *testing.T) {
	recorder := dryrun.NewDryrunTransportClient()

	runtime, err := BuildRecording(&configloader.Config{
		Resources: []configloader.Resource{{Name: "local", Manifest: testConfigMapManifestMap()}},
	}, recorder)
	require.NoError(t, err)
	entry, err := runtime.Registry.Get(configloader.TransportClientKubernetes)
	require.NoError(t, err)
	assert.Same(t, recorder, entry.Client)
	_, err = runtime.Registry.Get("maestro")
	require.Error(t, err, "no maestro transport is registered")

	runtime, err = BuildRecording(&configloader.Config{}, recorder)
	require.NoError(t, err)
	assert.Empty(t, runtime.Registry)
}

func TestTarget(t *testing.T) {
	assert.Nil(t, Target("local", configloader.TransportDefinition{Type: configloader.TransportTypeKubernetes}))

	definition := testRemoteDefinition("desired-memory")
	definition.TargetCluster = "{{ .clusterName }}"
	target := Target("Remote-Primary", definition)
	require.NotNil(t, target)

	configMap := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	vars := map[string]any{"clusterName": "cluster-1"}
	route, err := target(configMap, vars)
	require.NoError(t, err)
	assert.Equal(t, &desireclient.TransportContext{ManagementCluster: "cluster-1", Resource: "configmaps"}, route)

	missing := definition
	missing.TargetCluster = "{{ .missing }}"
	_, err = Target("Remote-Primary", missing)(configMap, vars)
	require.ErrorContains(t, err, `render transport "remote-primary" target_cluster`)

	for _, invalid := range []string{"", "Bad_Cluster"} {
		_, err = target(configMap, map[string]any{"clusterName": invalid})
		require.ErrorContains(t, err, "must be a Kubernetes DNS-1123 label")
	}

	_, err = target(schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, vars)
	require.ErrorContains(t, err, "has no resource_plurals mapping")
}

func loadRuntimeConfig(t *testing.T, adapterYAML string) *configloader.Config {
	t.Helper()

	adapterPath, taskPath := createRuntimeConfigFiles(t, adapterYAML, `schema_version: "2.0"`)
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

func TestBuildReusesDeclaredKubernetesTransportForOmittedResources(t *testing.T) {
	config := &configloader.Config{
		Adapter: configloader.AdapterInfo{Name: "test-adapter"},
		Clients: configloader.ClientsConfig{
			Kubernetes: configloader.KubernetesConfig{KubeConfigPath: writeTestKubeconfig(t)},
		},
		Transports: map[string]configloader.TransportDefinition{
			"local": {Type: configloader.TransportTypeKubernetes},
		},
		Resources: []configloader.Resource{{Name: "local", Manifest: testConfigMapManifestMap()}},
	}

	runtime, err := Build(t.Context(), config)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, runtime.Close()) })

	declared, err := runtime.Registry.Get("local")
	require.NoError(t, err)
	implicit, err := runtime.Registry.Get(configloader.TransportClientKubernetes)
	require.NoError(t, err)
	assert.Same(t, declared.Client, implicit.Client, "omitted resources must share the declared local client")
	assert.Nil(t, declared.Target)
	assert.Nil(t, implicit.Target)
}

func TestBuildSharesClientAcrossNamedKubernetesTransports(t *testing.T) {
	config := &configloader.Config{
		SchemaVersion: "2.0",
		Clients: configloader.ClientsConfig{
			Kubernetes: configloader.KubernetesConfig{KubeConfigPath: writeTestKubeconfig(t)},
		},
		Transports: map[string]configloader.TransportDefinition{
			"local-primary":   {Type: configloader.TransportTypeKubernetes},
			"local-secondary": {Type: configloader.TransportTypeKubernetes},
		},
		Resources: []configloader.Resource{
			{Name: "named-local", Transport: configloader.NamedTransport("local-primary"), Manifest: testConfigMapManifestMap()},
			{Name: "default-local", Manifest: testConfigMapManifestMap()},
		},
	}

	runtime, err := Build(t.Context(), config)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, runtime.Close()) })

	primary, err := runtime.Registry.Get("local-primary")
	require.NoError(t, err)
	secondary, err := runtime.Registry.Get("local-secondary")
	require.NoError(t, err)
	implicit, err := runtime.Registry.Get(configloader.TransportClientKubernetes)
	require.NoError(t, err)
	assert.Same(t, primary.Client, secondary.Client)
	assert.Same(t, primary.Client, implicit.Client)
}

func testRemoteDefinition(store string) configloader.TransportDefinition {
	return configloader.TransportDefinition{
		Type:            configloader.TransportTypeRemote,
		Store:           store,
		TargetCluster:   "cluster-1",
		ResourcePlurals: map[string]string{"v1/ConfigMap": "configmaps"},
	}
}

func testRemoteResource(name, transport string) configloader.Resource {
	return configloader.Resource{
		Name:      name,
		Transport: configloader.NamedTransport(transport),
		Manifest:  testConfigMapManifestMap(),
	}
}

func testConfigMapManifestMap() map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": "test", "namespace": "default"},
	}
}

func writeTestKubeconfig(t *testing.T) string {
	t.Helper()
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
	return kubeconfigPath
}
