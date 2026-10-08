package configloader

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const remoteAdapterYAML = `
adapter:
  name: test-adapter
  version: "2.0.0"
stores:
  desired-memory:
    type: memory
transports:
  remote-primary:
    type: remote
    store: desired-memory
    target_cluster: "{{ .clusterId }}"
    resource_plurals:
      "v1/ConfigMap": configmaps
      "v1/Namespace": namespaces
`

const remoteTaskYAML = `
schema_version: "2.0"
params:
  - name: clusterId
    source: event.id
resources:
  - name: configMap
    transport: Remote-Primary
    manifest:
      apiVersion: v1
      kind: ConfigMap
      metadata: {name: test, namespace: default}
    discovery: {by_name: test, namespace: default}
  - name: namespace
    transport: remote-primary
    manifest:
      apiVersion: v1
      kind: Namespace
      metadata: {name: test}
    discovery: {by_name: test}
`

func loadRemoteConfig(t *testing.T, adapterYAML, taskYAML string) (*Config, error) {
	t.Helper()
	adapterPath, taskPath := createTestConfigFiles(t, t.TempDir(), adapterYAML, taskYAML)
	return LoadConfig(WithAdapterConfigPath(adapterPath), WithTaskConfigPath(taskPath), WithSkipSemanticValidation())
}

func TestLoadRemoteTransportTwoKinds(t *testing.T) {
	config, err := loadRemoteConfig(t, remoteAdapterYAML, remoteTaskYAML)
	require.NoError(t, err)
	assert.Equal(t, "2.0", config.SchemaVersion)
	assert.Equal(t, "Remote-Primary", config.Resources[0].GetTransportName())
	assert.Equal(t, TransportClientKubernetes, (&Resource{}).GetTransportName())
	definition, ok := TransportDefinitionByName(config.Transports, config.Resources[0].GetTransportName())
	require.True(t, ok)
	for _, resource := range config.Resources {
		gvk, err := resource.StaticGVK()
		require.NoError(t, err)
		plural, mapped := definition.PluralForGVK(gvk)
		require.True(t, mapped)
		assert.Contains(t, []string{"configmaps", "namespaces"}, plural)
	}
}

func TestLoadRemoteTransportGroupedGVKAndLocalDefault(t *testing.T) {
	adapter := strings.Replace(remoteAdapterYAML, `"v1/Namespace": namespaces`, `"apps/v1/Deployment": deployments`, 1)
	task := strings.Replace(remoteTaskYAML, `transport: remote-primary
    manifest:
      apiVersion: v1
      kind: Namespace`, `transport: remote-primary
    manifest:
      apiVersion: apps/v1
      kind: Deployment`, 1)
	config, err := loadRemoteConfig(t, adapter, task)
	require.NoError(t, err)
	definition, ok := TransportDefinitionByName(config.Transports, "remote-primary")
	require.True(t, ok)
	gvk, err := config.Resources[1].StaticGVK()
	require.NoError(t, err)
	plural, mapped := definition.PluralForGVK(gvk)
	require.True(t, mapped)
	assert.Equal(t, "deployments", plural)

	localTask := strings.Replace(remoteTaskYAML, "transport: Remote-Primary", "", 1)
	localConfig, err := loadRemoteConfig(t, remoteAdapterYAML, localTask)
	require.NoError(t, err)
	assert.Equal(t, TransportClientKubernetes, localConfig.Resources[0].GetTransportName())
}

func TestLoadRemoteTransportDottedGroupGVK(t *testing.T) {
	adapter := strings.Replace(remoteAdapterYAML, `"v1/Namespace": namespaces`,
		`"hypershift.openshift.io/v1beta1/HostedCluster": hostedclusters`, 1)
	task := strings.Replace(remoteTaskYAML, `apiVersion: v1
      kind: Namespace`, `apiVersion: hypershift.openshift.io/v1beta1
      kind: HostedCluster`, 1)
	config, err := loadRemoteConfig(t, adapter, task)
	require.NoError(t, err)
	definition, ok := TransportDefinitionByName(config.Transports, config.Resources[1].GetTransportName())
	require.True(t, ok)
	gvk, err := config.Resources[1].StaticGVK()
	require.NoError(t, err)
	assert.Equal(t, schema.GroupVersionKind{
		Group: "hypershift.openshift.io", Version: "v1beta1", Kind: "HostedCluster",
	}, gvk)
	plural, mapped := definition.PluralForGVK(gvk)
	require.True(t, mapped)
	assert.Equal(t, "hostedclusters", plural)
}

func TestLoadRemoteTransportRejectsInvalidRoutingWithSemanticValidationSkipped(t *testing.T) {
	replaceAdapter := func(old, replacement string) string {
		return strings.Replace(remoteAdapterYAML, old, replacement, 1)
	}
	replaceTask := func(old, replacement string) string {
		return strings.Replace(remoteTaskYAML, old, replacement, 1)
	}
	cases := []struct {
		name, adapter, task, want string
	}{
		{
			name: "unknown name", adapter: remoteAdapterYAML,
			task: replaceTask("Remote-Primary", "missing"),
			want: `resources[0].transport references unknown transport "missing"; available: [kubernetes remote-primary]`,
		},
		{
			name: "blank name", adapter: remoteAdapterYAML,
			task: replaceTask("Remote-Primary", `""`),
			want: "resources[0].transport must name a transport",
		},
		{
			name: "null name", adapter: remoteAdapterYAML,
			task: replaceTask("Remote-Primary", "null"),
			want: "resources[0].transport must name a transport",
		},
		{
			name: "null name through merge key", adapter: remoteAdapterYAML,
			task: `
schema_version: "2.0"
params:
  - name: clusterId
    source: event.id
resources:
  - name: configMap
    transport: remote-primary
    manifest:
      apiVersion: v1
      kind: ConfigMap
      metadata:
        name: test
        namespace: default
        annotations: &routing {transport: null}
    discovery: {by_name: test, namespace: default}
  - <<: *routing
    name: namespace
    manifest: {apiVersion: v1, kind: Namespace, metadata: {name: test}}
    discovery: {by_name: test}
`,
			want: "resources[1].transport must name a transport",
		},
		{
			name:    "missing mapping",
			adapter: replaceAdapter(`"v1/ConfigMap": configmaps`, `"apps/v1/Deployment": deployments`),
			task:    remoteTaskYAML,
			want:    "resources[0].manifest GVK v1/ConfigMap has no resource_plurals mapping",
		},
		{
			name: "missing plural map",
			adapter: replaceAdapter("    resource_plurals:\n      \"v1/ConfigMap\": configmaps\n"+
				"      \"v1/Namespace\": namespaces\n", ""),
			task: remoteTaskYAML, want: "resource_plurals is required for remote transport",
		},
		{
			name: "malformed GVK key", adapter: replaceAdapter("v1/ConfigMap", "v1ConfigMap"),
			task: remoteTaskYAML, want: "must be a static apiVersion/Kind",
		},
		{
			name: "invalid plural", adapter: replaceAdapter("configmaps", "ConfigMaps"),
			task: remoteTaskYAML, want: "must be a Kubernetes DNS-1035 label",
		},
		{
			name:    "missing target",
			adapter: replaceAdapter(`target_cluster: "{{ .clusterId }}"`, `target_cluster: ""`),
			task:    remoteTaskYAML, want: "target_cluster is required",
		},
		{
			name:    "invalid literal target",
			adapter: replaceAdapter(`target_cluster: "{{ .clusterId }}"`, `target_cluster: "Invalid_Cluster"`),
			task:    remoteTaskYAML, want: "target_cluster must be a Kubernetes DNS-1123 label",
		},
		{
			name: "undefined target variable", adapter: replaceAdapter(".clusterId", ".missing"),
			task: remoteTaskYAML, want: `target_cluster uses undefined variable "missing"`,
		},
		{
			name: "target uses post payload", adapter: replaceAdapter(".clusterId", ".clusterPayload"),
			task: remoteTaskYAML + "post:\n  payloads:\n    - name: clusterPayload\n      build: {id: x}\n",
			want: `target_cluster uses undefined variable "clusterPayload"`,
		},
		{
			name: "target uses resource alias", adapter: replaceAdapter(".clusterId", ".resources.configMap"),
			task: remoteTaskYAML, want: `target_cluster uses undefined variable "resources.configMap"`,
		},
		{
			name: "target uses expression-only precondition", adapter: replaceAdapter(".clusterId", ".isReady"),
			task: replaceTask("resources:\n", "preconditions:\n  - name: isReady\n    expression: \"true\"\nresources:\n"),
			want: `target_cluster uses undefined variable "isReady"`,
		},
		{
			name: "missing manifest", adapter: remoteAdapterYAML,
			task: replaceTask("    manifest:\n      apiVersion: v1\n      kind: ConfigMap\n"+
				"      metadata: {name: test, namespace: default}\n", ""),
			want: "resources[0].manifest is required",
		},
		{
			name: "recreate on change", adapter: remoteAdapterYAML,
			task: replaceTask("transport: Remote-Primary\n", "transport: Remote-Primary\n    recreate_on_change: true\n"),
			want: "resources[0].recreate_on_change is unsupported for remote transport",
		},
		{
			name: "dynamic GVK", adapter: remoteAdapterYAML,
			task: replaceTask("kind: ConfigMap", `kind: "{{ .kind }}"`),
			want: "resources[0].manifest: line 2: kind must be a literal value",
		},
		{
			name: "dynamic GVK on a local resource", adapter: remoteAdapterYAML,
			task: strings.NewReplacer("transport: Remote-Primary", "transport: kubernetes",
				"kind: ConfigMap", `kind: "{{ .kind }}"`).Replace(remoteTaskYAML),
			want: "resources[0].manifest: line 2: kind must be a literal value",
		},
		{
			name: "legacy block", adapter: remoteAdapterYAML,
			task: replaceTask("transport: Remote-Primary", "transport: {client: remote-primary}"),
			want: `resources[0].transport: this is a v1 configuration shape`,
		},
		{
			name: "colliding GVK keys",
			adapter: replaceAdapter(`"v1/Namespace": namespaces`,
				"\"v1/Namespace\": namespaces\n      \" V1/configmap \" : configmaps"),
			task: remoteTaskYAML, want: "resolve to the same GVK",
		},
		{
			name: "reserved local name", adapter: replaceAdapter("remote-primary:", "kubernetes:"),
			task: remoteTaskYAML, want: "name kubernetes is reserved for local Kubernetes",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadRemoteConfig(t, tc.adapter, tc.task)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestLoadRemoteTransportValidatesManifestRefAfterReading(t *testing.T) {
	dir := t.TempDir()
	adapterPath, taskPath := createTestConfigFiles(t, dir, remoteAdapterYAML, `
schema_version: "2.0"
params:
  - name: clusterId
    source: event.id
resources:
  - name: configMap
    transport: remote-primary
    manifest: {ref: configmap.yaml}
    discovery: {by_name: test}
`)
	manifest := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "configmap.yaml"), manifest, 0600))
	config, err := LoadConfig(
		WithAdapterConfigPath(adapterPath), WithTaskConfigPath(taskPath), WithSkipSemanticValidation(),
	)
	require.NoError(t, err)
	gvk, err := config.Resources[0].StaticGVK()
	require.NoError(t, err)
	assert.Equal(t, "ConfigMap", gvk.Kind)
}

func TestLoadRemoteTransportTargetClusterAcceptsRuntimeParams(t *testing.T) {
	adapter := strings.Replace(remoteAdapterYAML, ".clusterId", ".clusterStatus.id", 1)
	task := strings.Replace(remoteTaskYAML, "resources:\n", `preconditions:
  - name: clusterStatus
    api_call:
      method: GET
      url: "/clusters/{{ .clusterId }}"
resources:
`, 1)
	_, err := loadRemoteConfig(t, adapter, task)
	require.NoError(t, err)
}

func TestPluralForGVKMatchesDirectlyConstructedMixedCaseKeys(t *testing.T) {
	definition := TransportDefinition{ResourcePlurals: map[string]string{"apps/v1/Deployment": "deployments"}}
	plural, ok := definition.PluralForGVK(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	require.True(t, ok)
	assert.Equal(t, "deployments", plural)
}

func TestValidateConfigRoutingRejectsRemoteDeleteWithoutByName(t *testing.T) {
	for name, discovery := range map[string]*DiscoveryConfig{
		"no discovery":       nil,
		"selector discovery": {BySelectors: &SelectorConfig{LabelSelector: map[string]string{"app": "test"}}},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateConfigRouting(&Config{
				SchemaVersion: schemaVersionV2,
				Stores:        map[string]StoreDefinition{"desired-memory": {Type: StoreTypeMemory}},
				Transports: map[string]TransportDefinition{"remote-primary": {
					Type: TransportTypeRemote, Store: "desired-memory", TargetCluster: "cluster-1",
					ResourcePlurals: map[string]string{"v1/ConfigMap": "configmaps"},
				}},
				Resources: []Resource{{
					Name:      "configMap",
					Transport: NamedTransport("remote-primary"),
					Manifest:  map[string]interface{}{"apiVersion": "v1", "kind": "ConfigMap"},
					Discovery: discovery,
					Lifecycle: &ResourceLifecycle{Delete: &LifecycleDelete{}},
				}},
			})
			require.ErrorContains(t, err, "resources[0].lifecycle.delete: "+ErrMsgDesireSelectorDeleteUnsupported)
		})
	}
}
