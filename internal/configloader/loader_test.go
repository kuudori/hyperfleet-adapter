package configloader

import (
	"bytes"
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const testAdapterConfigYAML = `
adapter:
  name: test-adapter
  version: "0.1.0"
clients:
  hyperfleet_api:
    base_url: "https://test.example.com"
    timeout: 2s
  kubernetes:
    api_version: "v1"
`

// createTestConfigFiles creates temporary adapter and task config files for testing
func createTestConfigFiles(t *testing.T, tmpDir string, adapterYAML, taskYAML string) (adapterPath, taskPath string) {
	t.Helper()

	adapterPath = filepath.Join(tmpDir, "adapter-config.yaml")
	taskPath = filepath.Join(tmpDir, "task-config.yaml")

	err := os.WriteFile(adapterPath, []byte(adapterYAML), 0644)
	require.NoError(t, err)

	err = os.WriteFile(taskPath, []byte(taskYAML), 0644)
	require.NoError(t, err)

	return adapterPath, taskPath
}

func TestLoadConfig(t *testing.T) {
	tmpDir := t.TempDir()

	adapterYAML := `
adapter:
  name: deployment-config
  version: "0.1.0"
clients:
  hyperfleet_api:
    base_url: "https://test.example.com"
    timeout: 2s
    retry_attempts: 3
    retry_backoff: exponential
  kubernetes:
    api_version: "v1"
`

	taskYAML := `
schema_version: "2.0"
params:
  - name: "clusterId"
    source: "event.id"
    type: "string"
    required: true
preconditions:
  - name: "clusterStatus"
    api_call:
      method: "GET"
      url: "https://api.example.com/clusters/{{ .clusterId }}"
resources:
  - name: "testNamespace"
    manifest:
      apiVersion: v1
      kind: Namespace
      metadata:
        name: "test-ns"
    discovery:
      namespace: "*"
      by_name: "test-ns"
`

	adapterPath, taskPath := createTestConfigFiles(t, tmpDir, adapterYAML, taskYAML)

	// Test loading
	config, err := LoadConfig(
		WithAdapterConfigPath(adapterPath),
		WithTaskConfigPath(taskPath),
		WithSkipSemanticValidation(),
	)
	require.NoError(t, err)
	require.NotNil(t, config)

	// Verify merged config fields
	// Adapter info comes from adapter config
	assert.Equal(t, "deployment-config", config.Adapter.Name)
	assert.Equal(t, "0.1.0", config.Adapter.Version)
	// Clients config comes from adapter config
	assert.Equal(t, "https://test.example.com", config.Clients.HyperfleetAPI.BaseURL)
	assert.Equal(t, 2*time.Second, config.Clients.HyperfleetAPI.Timeout)
	// Task fields come from task config
	require.Len(t, config.Params, 1)
	assert.Equal(t, "clusterId", config.Params[0].Name)
	require.Len(t, config.Preconditions, 1)
	assert.Equal(t, "clusterStatus", config.Preconditions[0].Name)
	require.Len(t, config.Resources, 1)
	assert.Equal(t, "testNamespace", config.Resources[0].Name)
}

func TestLoadConfigMissingAdapterConfig(t *testing.T) {
	tmpDir := t.TempDir()
	taskPath := filepath.Join(tmpDir, "task-config.yaml")
	err := os.WriteFile(taskPath, []byte(`{}`), 0644)
	require.NoError(t, err)

	config, err := LoadConfig(
		WithAdapterConfigPath("/nonexistent/adapter-config.yaml"),
		WithTaskConfigPath(taskPath),
	)
	assert.Error(t, err)
	assert.Nil(t, config)
	assert.Contains(t, err.Error(), "failed to load adapter config")
}

func TestLoadConfigMissingTaskConfig(t *testing.T) {
	tmpDir := t.TempDir()
	adapterPath := filepath.Join(tmpDir, "adapter-config.yaml")
	err := os.WriteFile(adapterPath, []byte(`
adapter:
  name: test-adapter
  version: "1.0.0"
clients:
  hyperfleet_api:
    timeout: 5s
  kubernetes:
    api_version: v1
`), 0644)
	require.NoError(t, err)

	config, err := LoadConfig(
		WithAdapterConfigPath(adapterPath),
		WithTaskConfigPath("/nonexistent/task-config.yaml"),
	)
	assert.Error(t, err)
	assert.Nil(t, config)
	assert.Contains(t, err.Error(), "failed to load task config")
}

func TestAdapterConfigValidation(t *testing.T) {
	tests := []struct {
		name      string
		yaml      string
		errorMsg  string
		wantError bool
	}{
		{
			name: "valid minimal adapter config",
			yaml: `
adapter:
  name: test-adapter
  version: "1.0.0"
clients:
  hyperfleet_api:
    timeout: 5s
  kubernetes:
    api_version: "v1"
`,
			wantError: false,
		},
		{
			name: "missing adapter.name",
			yaml: `
adapter:
  version: "1.0.0"
clients:
  hyperfleet_api:
    timeout: 5s
`,
			wantError: true,
			errorMsg:  "name is required",
		},
		{
			name: "missing adapter.version",
			yaml: `
adapter:
  name: test-adapter
clients:
  hyperfleet_api:
    timeout: 5s
`,
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var config AdapterConfig
			err := yaml.Unmarshal([]byte(tt.yaml), &config)
			require.NoError(t, err)

			validator := NewAdapterConfigValidator(&config, "")
			err = validator.ValidateStructure()

			if tt.wantError {
				assert.Error(t, err)
				if tt.errorMsg != "" {
					assert.Contains(t, err.Error(), tt.errorMsg)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestTaskConfigValidation(t *testing.T) {
	tests := []struct {
		name      string
		yaml      string
		errorMsg  string
		wantError bool
	}{
		{
			name:      "valid minimal task config",
			yaml:      `{}`,
			wantError: false,
		},
		{
			name: "valid task config with params",
			yaml: `
params:
  - name: "clusterId"
    source: "event.id"
    required: true
`,
			wantError: false,
		},
		{
			name: "parameter without name",
			yaml: `
params:
  - source: "event.id"
`,
			wantError: true,
			errorMsg:  "params[0].name is required",
		},
		{
			name: "parameter without source",
			yaml: `
params:
  - name: "clusterId"
    required: true
`,
			wantError: true,
			errorMsg:  "source is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var config AdapterTaskConfig
			err := yaml.Unmarshal([]byte(tt.yaml), &config)
			require.NoError(t, err)

			validator := newTaskConfigValidator(&config, fsTaskFiles(nil), false)
			err = validator.ValidateStructure()

			if tt.wantError {
				assert.Error(t, err)
				if tt.errorMsg != "" {
					assert.Contains(t, err.Error(), tt.errorMsg)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidatePreconditionsInTaskConfig(t *testing.T) {
	tests := []struct {
		name      string
		yaml      string
		errorMsg  string
		wantError bool
	}{
		{
			name: "valid precondition with API call",
			yaml: `
preconditions:
  - name: "checkCluster"
    api_call:
      method: "GET"
      url: "https://api.example.com/clusters"
`,
			wantError: false,
		},
		{
			name: "precondition without name",
			yaml: `
preconditions:
  - api_call:
      method: "GET"
      url: "https://api.example.com/clusters"
`,
			wantError: true,
			errorMsg:  "preconditions[0].name is required",
		},
		{
			name: "precondition without apiCall or expression",
			yaml: `
preconditions:
  - name: "checkCluster"
`,
			wantError: true,
			errorMsg:  "preconditions[0]: must specify api_call, conditions",
		},
		{
			name: "API call without method",
			yaml: `
preconditions:
  - name: "checkCluster"
    api_call:
      url: "https://api.example.com/clusters"
`,
			wantError: true,
			errorMsg:  "method is required",
		},
		{
			name: "API call with invalid method",
			yaml: `
preconditions:
  - name: "checkCluster"
    api_call:
      method: "INVALID"
      url: "https://api.example.com/clusters"
`,
			wantError: true,
			errorMsg:  "is invalid (allowed:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var config AdapterTaskConfig
			err := yaml.Unmarshal([]byte(tt.yaml), &config)
			require.NoError(t, err)

			validator := newTaskConfigValidator(&config, fsTaskFiles(nil), false)
			err = validator.ValidateStructure()

			if tt.wantError {
				assert.Error(t, err)
				if tt.errorMsg != "" {
					assert.Contains(t, err.Error(), tt.errorMsg)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidateResourcesInTaskConfig(t *testing.T) {
	tests := []struct {
		name      string
		yaml      string
		errorMsg  string
		wantError bool
	}{
		{
			name: "valid resource with manifest",
			yaml: `
resources:
  - name: "testNamespace"
    manifest:
      apiVersion: v1
      kind: Namespace
      metadata:
        name: "test-ns"
    discovery:
      namespace: "*"
      by_name: "test-ns"
`,
			wantError: false,
		},
		{
			name: "resource without name",
			yaml: `
resources:
  - manifest:
      apiVersion: v1
      kind: Namespace
    discovery:
      by_name: "test-ns"
`,
			wantError: true,
			errorMsg:  "resources[0].name is required",
		},
		{
			name: "resource without manifest - kubernetes transport requires manifest in semantic validation",
			yaml: `
resources:
  - name: "testNamespace"
    discovery:
      by_name: "test-ns"
`,
			wantError: false, // Manifest is no longer structurally required (validated semantically based on transport type)
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var config AdapterTaskConfig
			err := yaml.Unmarshal([]byte(tt.yaml), &config)
			require.NoError(t, err)

			validator := newTaskConfigValidator(&config, fsTaskFiles(nil), false)
			err = validator.ValidateStructure()

			if tt.wantError {
				assert.Error(t, err)
				if tt.errorMsg != "" {
					assert.Contains(t, err.Error(), tt.errorMsg)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestMergeConfigs(t *testing.T) {
	adapterCfg := &AdapterConfig{
		Adapter: AdapterInfo{
			Name:    "adapter-deployment",
			Version: "1.0.0",
		},
		Clients: ClientsConfig{
			HyperfleetAPI: HyperfleetAPIConfig{
				BaseURL:       "https://api.example.com",
				Timeout:       5 * time.Second,
				RetryAttempts: 3,
			},
			Kubernetes: KubernetesConfig{
				APIVersion: "v1",
			},
		},
	}

	taskCfg := &AdapterTaskConfig{
		Params: []Parameter{
			{Name: "clusterId", Source: StringSource("event.id"), Required: true},
		},
		Preconditions: []Precondition{
			{ActionBase: ActionBase{Name: "checkStatus"}},
		},
		Resources: []Resource{
			{Name: "namespace"},
		},
	}

	merged := Merge(adapterCfg, taskCfg)

	// Verify merged config
	// Adapter info from adapter config
	assert.Equal(t, "adapter-deployment", merged.Adapter.Name)
	assert.Equal(t, "1.0.0", merged.Adapter.Version)
	// Clients from adapter config
	assert.Equal(t, "https://api.example.com", merged.Clients.HyperfleetAPI.BaseURL)
	assert.Equal(t, 5*time.Second, merged.Clients.HyperfleetAPI.Timeout)
	// Task fields from task config
	require.Len(t, merged.Params, 1)
	assert.Equal(t, "clusterId", merged.Params[0].Name)
	require.Len(t, merged.Preconditions, 1)
	assert.Equal(t, "checkStatus", merged.Preconditions[0].Name)
	require.Len(t, merged.Resources, 1)
	assert.Equal(t, "namespace", merged.Resources[0].Name)
}

func TestGetRequiredParams(t *testing.T) {
	config := &Config{
		Params: []Parameter{
			{Name: "clusterId", Source: StringSource("event.id"), Required: true},
			{Name: "optional", Source: StringSource("event.optional"), Required: false},
		},
	}

	requiredParams := config.GetRequiredParams()
	assert.Len(t, requiredParams, 1)
	assert.Equal(t, "clusterId", requiredParams[0].Name)
}

func TestGetResourceByName(t *testing.T) {
	config := &Config{
		Resources: []Resource{
			{Name: "namespace1"},
			{Name: "namespace2"},
		},
	}

	resource := config.GetResourceByName("namespace1")
	assert.NotNil(t, resource)
	assert.Equal(t, "namespace1", resource.Name)

	resource = config.GetResourceByName("nonexistent")
	assert.Nil(t, resource)
}

func TestGetPreconditionByName(t *testing.T) {
	config := &Config{
		Preconditions: []Precondition{
			{ActionBase: ActionBase{Name: "precond1"}},
			{ActionBase: ActionBase{Name: "precond2"}},
		},
	}

	precond := config.GetPreconditionByName("precond1")
	assert.NotNil(t, precond)
	assert.Equal(t, "precond1", precond.Name)

	precond = config.GetPreconditionByName("nonexistent")
	assert.Nil(t, precond)
}

func TestValidateAdapterVersion(t *testing.T) {
	ctx := context.Background()

	config := &AdapterConfig{
		Adapter: AdapterInfo{
			Name:    "test-adapter",
			Version: "1.0.0",
		},
	}

	// Exact match
	err := ValidateAdapterVersion(ctx, config, "1.0.0")
	assert.NoError(t, err)

	// Patch version differs - should pass (bug fix release)
	err = ValidateAdapterVersion(ctx, config, "1.0.5")
	assert.NoError(t, err)

	// Minor version differs - should fail
	err = ValidateAdapterVersion(ctx, config, "1.1.0")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "adapter version mismatch")

	// Major version differs - should fail
	err = ValidateAdapterVersion(ctx, config, "2.0.0")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "adapter version mismatch")

	// Empty expected version (skip validation)
	err = ValidateAdapterVersion(ctx, config, "")
	assert.NoError(t, err)

	// Dev build versions (0.0.0-* skip validation)
	err = ValidateAdapterVersion(ctx, config, "0.0.0-dev")
	assert.NoError(t, err)

	err = ValidateAdapterVersion(ctx, config, "0.0.0-master")
	assert.NoError(t, err)

	err = ValidateAdapterVersion(ctx, config, "v0.0.0-dev")
	assert.NoError(t, err)

	// Empty config version (not provided in adapter config - skip validation)
	noVersionConfig := &AdapterConfig{
		Adapter: AdapterInfo{
			Name: "test-adapter",
		},
	}
	err = ValidateAdapterVersion(ctx, noVersionConfig, "1.0.0")
	assert.NoError(t, err)

	// Pre-release version with same major.minor - should pass
	err = ValidateAdapterVersion(ctx, config, "1.0.1-rc.1")
	assert.NoError(t, err)

	// Non-semver config version - should warn and skip validation
	invalidConfig := &AdapterConfig{
		Adapter: AdapterInfo{
			Name:    "test-adapter",
			Version: "not-a-version",
		},
	}
	buf := captureSlogOutput(t)

	err = ValidateAdapterVersion(ctx, invalidConfig, "1.0.0")
	assert.NoError(t, err)
	assert.Contains(t, buf.String(), "skipping adapter version validation")
	assert.Contains(t, buf.String(), "config version is not valid semver")

	// Non-semver binary version - should warn and skip validation
	buf.Reset()
	err = ValidateAdapterVersion(ctx, config, "not-a-version")
	assert.NoError(t, err)
	assert.Contains(t, buf.String(), "skipping adapter version validation")
	assert.Contains(t, buf.String(), "binary version is not valid semver")

	// Non-semver binary version "dev" (Konflux pipeline case)
	buf.Reset()
	err = ValidateAdapterVersion(ctx, config, "dev")
	assert.NoError(t, err)
	assert.Contains(t, buf.String(), "skipping adapter version validation")
	assert.Contains(t, buf.String(), "binary version is not valid semver")

	// Non-semver config version with valid binary version
	devConfig := &AdapterConfig{
		Adapter: AdapterInfo{
			Name:    "test-adapter",
			Version: "dev",
		},
	}
	buf.Reset()
	err = ValidateAdapterVersion(ctx, devConfig, "1.0.0")
	assert.NoError(t, err)
	assert.Contains(t, buf.String(), "skipping adapter version validation")
	assert.Contains(t, buf.String(), "config version is not valid semver")

	// Both versions non-semver (config parse fails first, so only config warning emitted)
	buf.Reset()
	err = ValidateAdapterVersion(ctx, devConfig, "latest")
	assert.NoError(t, err)
	assert.Contains(t, buf.String(), "skipping adapter version validation")
	assert.Contains(t, buf.String(), "config version is not valid semver")
}

// captureSlogOutput installs a text-handler slog logger as the process default
// for the duration of the test and returns the buffer it writes to, so tests
// can assert on log output produced via slog.WarnContext/slog.ErrorContext.
// The previous default logger is restored automatically via t.Cleanup.
func captureSlogOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestValidateFileReferencesInTaskConfig(t *testing.T) {
	// Create temporary directory with test files
	tmpDir := t.TempDir()

	// Create a test template file
	templatePath := filepath.Join(tmpDir, "templates")
	require.NoError(t, os.MkdirAll(templatePath, 0755))
	templateFile := filepath.Join(templatePath, "test-template.yaml")
	require.NoError(t, os.WriteFile(templateFile, []byte("test: value"), 0644))

	tests := []struct {
		name    string
		config  *AdapterTaskConfig
		errMsg  string
		baseDir string
		wantErr bool
	}{
		{
			name: "valid payload buildRef",
			config: &AdapterTaskConfig{
				Post: &PostConfig{
					Payloads: []Payload{
						{Name: "test", BuildRef: "templates/test-template.yaml"},
					},
				},
			},
			baseDir: tmpDir,
			wantErr: false,
		},
		{
			name: "invalid payload buildRef - file not found",
			config: &AdapterTaskConfig{
				Post: &PostConfig{
					Payloads: []Payload{
						{Name: "test", BuildRef: "templates/nonexistent.yaml"},
					},
				},
			},
			baseDir: tmpDir,
			wantErr: true,
			errMsg:  "does not exist",
		},
		{
			name: "invalid payload buildRef - is a directory",
			config: &AdapterTaskConfig{
				Post: &PostConfig{
					Payloads: []Payload{
						{Name: "test", BuildRef: "templates"},
					},
				},
			},
			baseDir: tmpDir,
			wantErr: true,
			errMsg:  "is a directory",
		},
		{
			name: "valid manifest.ref",
			config: &AdapterTaskConfig{
				Resources: []Resource{
					{
						Name: "test",
						Manifest: map[string]interface{}{
							"ref": "templates/test-template.yaml",
						},
					},
				},
			},
			baseDir: tmpDir,
			wantErr: false,
		},
		{
			name: "invalid manifest.ref - file not found",
			config: &AdapterTaskConfig{
				Resources: []Resource{
					{
						Name: "test",
						Manifest: map[string]interface{}{
							"ref": "templates/nonexistent.yaml",
						},
					},
				},
			},
			baseDir: tmpDir,
			wantErr: true,
			errMsg:  "does not exist",
		},
		{
			name: "valid multiple payloads with buildRef",
			config: &AdapterTaskConfig{
				Post: &PostConfig{
					Payloads: []Payload{
						{Name: "payload1", BuildRef: "templates/test-template.yaml"},
						{Name: "payload2", BuildRef: "templates/test-template.yaml"},
					},
				},
			},
			baseDir: tmpDir,
			wantErr: false,
		},
		{
			name: "no file references - should pass",
			config: &AdapterTaskConfig{
				Params: []Parameter{
					{Name: "test", Source: StringSource("event.test")},
				},
			},
			baseDir: tmpDir,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator := newTaskConfigValidator(tt.config, dirTaskFiles(tt.baseDir), false)
			err := validator.ValidateFileReferences()
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestLoadConfigWithFileReferences(t *testing.T) {
	// Create temporary directory
	tmpDir := t.TempDir()

	// Create a template file
	templateDir := filepath.Join(tmpDir, "templates")
	require.NoError(t, os.MkdirAll(templateDir, 0755))
	templateFile := filepath.Join(templateDir, "status-payload.yaml")
	require.NoError(t, os.WriteFile(templateFile, []byte(`
status: "{{ .status }}"
`), 0644))

	// Create adapter config file
	adapterYAML := testAdapterConfigYAML
	adapterPath := filepath.Join(tmpDir, "adapter-config.yaml")
	require.NoError(t, os.WriteFile(adapterPath, []byte(adapterYAML), 0644))

	// Create task config file with build_ref
	taskYAML := `
schema_version: "2.0"
params:
  - name: "clusterId"
    source: "event.id"
resources:
  - name: "testNamespace"
    manifest:
      apiVersion: v1
      kind: Namespace
      metadata:
        name: test
    discovery:
      namespace: "*"
      by_name: "test"
post:
  payloads:
    - name: "statusPayload"
      build_ref: "templates/status-payload.yaml"
`
	taskPath := filepath.Join(tmpDir, "task-config.yaml")
	require.NoError(t, os.WriteFile(taskPath, []byte(taskYAML), 0644))

	// Load should succeed because template file exists
	config, err := LoadConfig(
		WithAdapterConfigPath(adapterPath),
		WithTaskConfigPath(taskPath),
		WithSkipSemanticValidation(),
	)
	require.NoError(t, err)
	require.NotNil(t, config)
	assert.Equal(t, "test-adapter", config.Adapter.Name)

	// Verify buildRef content was loaded
	require.NotNil(t, config.Post)
	require.Len(t, config.Post.Payloads, 1)
	assert.NotNil(t, config.Post.Payloads[0].BuildRefContent)

	// Now test with non-existent buildRef
	taskYAMLBad := `
schema_version: "2.0"
params:
  - name: "clusterId"
    source: "event.id"
resources:
  - name: "testNamespace"
    manifest:
      apiVersion: v1
      kind: Namespace
      metadata:
        name: test
    discovery:
      namespace: "*"
      by_name: "test"
post:
  payloads:
    - name: "statusPayload"
      build_ref: "templates/nonexistent.yaml"
`
	taskPathBad := filepath.Join(tmpDir, "task-config-bad.yaml")
	require.NoError(t, os.WriteFile(taskPathBad, []byte(taskYAMLBad), 0644))

	// Load should fail because template file doesn't exist
	config, err = LoadConfig(
		WithAdapterConfigPath(adapterPath),
		WithTaskConfigPath(taskPathBad),
		WithSkipSemanticValidation(),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
	assert.Nil(t, config)
}

func TestLoadFileReferencesContent(t *testing.T) {
	// Create temporary directory
	tmpDir := t.TempDir()
	templateDir := filepath.Join(tmpDir, "templates")
	require.NoError(t, os.MkdirAll(templateDir, 0755))

	// Create a buildRef template file
	buildRefFile := filepath.Join(templateDir, "status-payload.yaml")
	require.NoError(t, os.WriteFile(buildRefFile, []byte(`
status: "{{ .status }}"
message: "Operation completed"
`), 0644))

	// Create a manifest.ref template file
	manifestRefFile := filepath.Join(templateDir, "deployment.yaml")
	require.NoError(t, os.WriteFile(manifestRefFile, []byte(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: "{{ .name }}"
  namespace: "{{ .namespace }}"
spec:
  replicas: 1
`), 0644))

	// Create adapter config
	adapterYAML := testAdapterConfigYAML
	adapterPath := filepath.Join(tmpDir, "adapter-config.yaml")
	require.NoError(t, os.WriteFile(adapterPath, []byte(adapterYAML), 0644))

	// Create task config file with both build_ref and manifest.ref
	taskYAML := `
schema_version: "2.0"
params:
  - name: "clusterId"
    source: "event.id"
resources:
  - name: "deployment"
    manifest:
      ref: "templates/deployment.yaml"
    discovery:
      namespace: "*"
      by_selectors:
        label_selector:
          app: "test"
post:
  payloads:
    - name: "statusPayload"
      build_ref: "templates/status-payload.yaml"
`
	taskPath := filepath.Join(tmpDir, "task-config.yaml")
	require.NoError(t, os.WriteFile(taskPath, []byte(taskYAML), 0644))

	// Load config
	config, err := LoadConfig(
		WithAdapterConfigPath(adapterPath),
		WithTaskConfigPath(taskPath),
		WithSkipSemanticValidation(),
	)
	require.NoError(t, err)
	require.NotNil(t, config)

	// Verify manifest.ref was loaded as raw string (for Go template support)
	require.Len(t, config.Resources, 1)
	manifestStr, ok := config.Resources[0].Manifest.(string)
	require.True(t, ok, "Manifest should be a raw string after loading ref")
	assert.Contains(t, manifestStr, "apiVersion: apps/v1")
	assert.Contains(t, manifestStr, "kind: Deployment")
	assert.Contains(t, manifestStr, "{{ .name }}")
	assert.Contains(t, manifestStr, "{{ .namespace }}")

	// Verify buildRef content was loaded into BuildRefContent
	require.NotNil(t, config.Post)
	require.Len(t, config.Post.Payloads, 1)
	assert.NotNil(t, config.Post.Payloads[0].BuildRefContent)
	assert.Equal(t, "{{ .status }}", config.Post.Payloads[0].BuildRefContent["status"])
	assert.Equal(t, "Operation completed", config.Post.Payloads[0].BuildRefContent["message"])
	// Original BuildRef path should still be preserved
	assert.Equal(t, "templates/status-payload.yaml", config.Post.Payloads[0].BuildRef)
}

func TestValidateResourceDiscoveryInTaskConfig(t *testing.T) {
	// Helper to create a valid task config with given resources
	configWithResources := func(resources []Resource) *AdapterTaskConfig {
		return &AdapterTaskConfig{
			Resources: resources,
		}
	}

	tests := []struct {
		name      string
		errMsg    string
		resources []Resource
		wantErr   bool
	}{
		{
			name: "valid - manifest.ref with discovery bySelectors",
			resources: []Resource{
				{
					Name:     "test",
					Manifest: map[string]interface{}{"ref": "templates/test.yaml"},
					Discovery: &DiscoveryConfig{
						Namespace: "test-ns",
						BySelectors: &SelectorConfig{
							LabelSelector: map[string]string{"app": "test"},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "valid - manifest.ref with discovery byName",
			resources: []Resource{
				{
					Name:     "test",
					Manifest: map[string]interface{}{"ref": "templates/test.yaml"},
					Discovery: &DiscoveryConfig{
						Namespace: "*",
						ByName:    "my-resource",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "valid - inline manifest with discovery",
			resources: []Resource{
				{
					Name: "test",
					Manifest: map[string]interface{}{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
					},
					Discovery: &DiscoveryConfig{
						Namespace: "test-ns",
						ByName:    "my-configmap",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "invalid - inline manifest missing discovery",
			resources: []Resource{
				{
					Name: "test",
					Manifest: map[string]interface{}{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
					},
					// Missing discovery - required for all resources
				},
			},
			wantErr: true,
			errMsg:  "discovery is required",
		},
		{
			name: "invalid - manifest.ref missing discovery",
			resources: []Resource{
				{
					Name:     "test",
					Manifest: map[string]interface{}{"ref": "templates/test.yaml"},
					// Missing discovery
				},
			},
			wantErr: true,
			errMsg:  "discovery is required",
		},
		{
			name: "valid - manifest.ref with discovery missing namespace (all namespaces)",
			resources: []Resource{
				{
					Name:     "test",
					Manifest: map[string]interface{}{"ref": "templates/test.yaml"},
					Discovery: &DiscoveryConfig{
						// Empty namespace means all namespaces
						ByName: "my-resource",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "invalid - manifest.ref missing byName or bySelectors",
			resources: []Resource{
				{
					Name:     "test",
					Manifest: map[string]interface{}{"ref": "templates/test.yaml"},
					Discovery: &DiscoveryConfig{
						Namespace: "test-ns",
						// Missing byName and bySelectors
					},
				},
			},
			wantErr: true,
			errMsg:  "resources[0].discovery: must have either 'by_name' or 'by_selectors' set",
		},
		{
			name: "invalid - bySelectors without labelSelector defined",
			resources: []Resource{
				{
					Name:     "test",
					Manifest: map[string]interface{}{"ref": "templates/test.yaml"},
					Discovery: &DiscoveryConfig{
						Namespace:   "test-ns",
						BySelectors: &SelectorConfig{
							// Empty selectors
						},
					},
				},
			},
			wantErr: true,
			errMsg:  "resources[0].discovery.by_selectors.label_selector is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := configWithResources(tt.resources)
			errs := ValidateStruct(config)
			if tt.wantErr {
				require.True(t, errs != nil && errs.HasErrors(), "expected error but got none")
				assert.Contains(t, errs.Error(), tt.errMsg)
			} else {
				assert.True(t, errs == nil || !errs.HasErrors(), "unexpected error: %v", errs)
			}
		})
	}
}

func TestConditionValuesAlias(t *testing.T) {
	// Test that both "value" and "values" YAML keys are supported
	tests := []struct {
		name     string
		expected interface{}
		yaml     string
	}{
		{
			name: "value with single item",
			yaml: `
field: status
operator: equals
value: "Ready"
`,
			expected: "Ready",
		},
		{
			name: "value with list",
			yaml: `
field: status
operator: in
value:
  - "Ready"
  - "Running"
`,
			expected: []interface{}{"Ready", "Running"},
		},
		{
			name: "values with list (alias)",
			yaml: `
field: status
operator: in
values:
  - "Ready"
  - "Running"
`,
			expected: []interface{}{"Ready", "Running"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cond Condition
			err := yaml.Unmarshal([]byte(tt.yaml), &cond)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, cond.Value)
		})
	}
}

// TestConditionValueAndValuesError verifies that specifying both value and values is an error
func TestConditionValueAndValuesError(t *testing.T) {
	yamlContent := `
field: status
operator: in
value: "ignored"
values:
  - "Used"
`
	var cond Condition
	err := yaml.Unmarshal([]byte(yamlContent), &cond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "condition has both 'value' and 'values' keys")
}

// =============================================================================
// Transport Config Tests
// =============================================================================

func TestLoadConfigWithManifestRefGoTemplates(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a manifest file with structural Go templates ({{ if }}, {{ range }})
	manifestFile := filepath.Join(tmpDir, "manifest-with-templates.yaml")
	require.NoError(t, os.WriteFile(manifestFile, []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: "{{ .name }}"
  namespace: "{{ .namespace }}"
{{ if .addLabels }}
  labels:
    app: "{{ .appName }}"
{{ end }}
data:
  key: value
`), 0644))

	adapterYAML := testAdapterConfigYAML
	adapterPath := filepath.Join(tmpDir, "adapter-config.yaml")
	require.NoError(t, os.WriteFile(adapterPath, []byte(adapterYAML), 0644))

	taskYAML := `
schema_version: "2.0"
params:
  - name: "name"
    source: "event.name"
resources:
  - name: "configMap"
    manifest:
      ref: "manifest-with-templates.yaml"
    discovery:
      namespace: "*"
      by_name: "test"
`
	taskPath := filepath.Join(tmpDir, "task-config.yaml")
	require.NoError(t, os.WriteFile(taskPath, []byte(taskYAML), 0644))

	config, err := LoadConfig(
		WithAdapterConfigPath(adapterPath),
		WithTaskConfigPath(taskPath),
		WithSkipSemanticValidation(),
	)
	require.NoError(t, err)
	require.NotNil(t, config)

	// Manifest ref with Go templates should be stored as raw string
	require.Len(t, config.Resources, 1)
	manifestStr, ok := config.Resources[0].Manifest.(string)
	require.True(t, ok, "Manifest should be a raw string")
	assert.Contains(t, manifestStr, "{{ if .addLabels }}")
	assert.Contains(t, manifestStr, "{{ .name }}")
	assert.Contains(t, manifestStr, "{{ end }}")
}

func TestLoadConfigWithInlineStringManifest(t *testing.T) {
	tmpDir := t.TempDir()

	adapterYAML := testAdapterConfigYAML
	adapterPath := filepath.Join(tmpDir, "adapter-config.yaml")
	require.NoError(t, os.WriteFile(adapterPath, []byte(adapterYAML), 0644))

	// Inline manifest using YAML multiline string syntax for Go templates
	taskYAML := `
schema_version: "2.0"
params:
  - name: "name"
    source: "event.name"
resources:
  - name: "configMap"
    manifest: |
      apiVersion: v1
      kind: ConfigMap
      metadata:
        name: "{{ .name }}"
      {{ if .addLabels }}
        labels:
          app: "{{ .appName }}"
      {{ end }}
    discovery:
      namespace: "*"
      by_name: "test"
`
	taskPath := filepath.Join(tmpDir, "task-config.yaml")
	require.NoError(t, os.WriteFile(taskPath, []byte(taskYAML), 0644))

	config, err := LoadConfig(
		WithAdapterConfigPath(adapterPath),
		WithTaskConfigPath(taskPath),
		WithSkipSemanticValidation(),
	)
	require.NoError(t, err)
	require.NotNil(t, config)

	// Inline manifest with YAML multiline string should be stored as string
	require.Len(t, config.Resources, 1)
	manifestStr, ok := config.Resources[0].Manifest.(string)
	require.True(t, ok, "Inline manifest with multiline string should be stored as string")
	assert.Contains(t, manifestStr, "{{ if .addLabels }}")
	assert.Contains(t, manifestStr, "{{ .name }}")
}

func TestLoadRawFile(t *testing.T) {
	tmpDir := t.TempDir()

	t.Run("loads file as raw string preserving template syntax", func(t *testing.T) {
		content := `apiVersion: v1
kind: ConfigMap
{{ if .condition }}
metadata:
  name: "{{ .name }}"
{{ end }}`
		filePath := filepath.Join(tmpDir, "template.yaml")
		require.NoError(t, os.WriteFile(filePath, []byte(content), 0644))

		result, err := loadRawFile(dirTaskFiles(tmpDir), "template.yaml")
		require.NoError(t, err)
		assert.Equal(t, content, result)
	})

	t.Run("file not found", func(t *testing.T) {
		_, err := loadRawFile(dirTaskFiles(tmpDir), "nonexistent.yaml")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to read file")
	})

	t.Run("path traversal blocked", func(t *testing.T) {
		_, err := loadRawFile(dirTaskFiles(tmpDir), "../../../etc/passwd")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "escapes base directory")
	})
}

func TestAccessorsWithStringManifest(t *testing.T) {
	t.Run("HasManifestRef returns false for string manifest", func(t *testing.T) {
		r := &Resource{
			Name:     "test",
			Manifest: "apiVersion: v1\nkind: ConfigMap",
		}
		assert.False(t, r.HasManifestRef())
	})

	t.Run("GetManifestRef returns empty for string manifest", func(t *testing.T) {
		r := &Resource{
			Name:     "test",
			Manifest: "apiVersion: v1\nkind: ConfigMap",
		}
		assert.Equal(t, "", r.GetManifestRef())
	})

	t.Run("UnmarshalManifest returns error for string manifest", func(t *testing.T) {
		r := &Resource{
			Name:     "test",
			Manifest: "apiVersion: v1\nkind: ConfigMap",
		}
		result, err := r.UnmarshalManifest()
		assert.Nil(t, result)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "manifest is not a map")
	})

	t.Run("HasManifestRef still works for map manifest", func(t *testing.T) {
		r := &Resource{
			Name:     "test",
			Manifest: map[string]interface{}{"ref": "templates/test.yaml"},
		}
		assert.True(t, r.HasManifestRef())
		assert.Equal(t, "templates/test.yaml", r.GetManifestRef())
	})
}

func TestLoadConfigManifestRefPlainYAML(t *testing.T) {
	// Verify backward compatibility: ref files with plain YAML (no templates)
	// still work correctly when loaded as raw strings
	tmpDir := t.TempDir()

	manifestFile := filepath.Join(tmpDir, "plain-manifest.yaml")
	require.NoError(t, os.WriteFile(manifestFile, []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: "my-config"
  namespace: "default"
data:
  key: value
`), 0644))

	adapterYAML := testAdapterConfigYAML
	adapterPath := filepath.Join(tmpDir, "adapter-config.yaml")
	require.NoError(t, os.WriteFile(adapterPath, []byte(adapterYAML), 0644))

	taskYAML := `
schema_version: "2.0"
params:
  - name: "clusterId"
    source: "event.id"
resources:
  - name: "configMap"
    manifest:
      ref: "plain-manifest.yaml"
    discovery:
      namespace: "*"
      by_name: "my-config"
`
	taskPath := filepath.Join(tmpDir, "task-config.yaml")
	require.NoError(t, os.WriteFile(taskPath, []byte(taskYAML), 0644))

	config, err := LoadConfig(
		WithAdapterConfigPath(adapterPath),
		WithTaskConfigPath(taskPath),
		WithSkipSemanticValidation(),
	)
	require.NoError(t, err)
	require.NotNil(t, config)

	// Plain YAML ref is now stored as string (not map)
	require.Len(t, config.Resources, 1)
	manifestStr, ok := config.Resources[0].Manifest.(string)
	require.True(t, ok, "Manifest ref should be stored as raw string")
	assert.Contains(t, manifestStr, "apiVersion: v1")
	assert.Contains(t, manifestStr, "name: \"my-config\"")
}

func TestParameterSourceUnmarshal(t *testing.T) {
	t.Run("string scalar source", func(t *testing.T) {
		var cfg AdapterTaskConfig
		err := yaml.Unmarshal([]byte(`
params:
  - name: clusterId
    source: "event.id"
`), &cfg)
		require.NoError(t, err)
		require.Len(t, cfg.Params, 1)
		p := cfg.Params[0]
		assert.True(t, p.Source.IsString())
		assert.Equal(t, "event.id", p.Source.StringVal)
		assert.False(t, p.Source.IsAPICall())
		assert.False(t, p.Source.IsExpression())
	})

	t.Run("api_call mapping source", func(t *testing.T) {
		var cfg AdapterTaskConfig
		err := yaml.Unmarshal([]byte(`
params:
  - name: clusterData
    source:
      api_call:
        method: GET
        url: /clusters/abc
        timeout: 10s
`), &cfg)
		require.NoError(t, err)
		require.Len(t, cfg.Params, 1)
		p := cfg.Params[0]
		assert.True(t, p.Source.IsAPICall())
		require.NotNil(t, p.Source.APICall)
		assert.Equal(t, "GET", p.Source.APICall.Method)
		assert.Equal(t, "/clusters/abc", p.Source.APICall.URL)
		assert.False(t, p.Source.IsString())
		assert.False(t, p.Source.IsExpression())
	})

	t.Run("expression mapping source", func(t *testing.T) {
		var cfg AdapterTaskConfig
		err := yaml.Unmarshal([]byte(`
params:
  - name: isActive
    source:
      expression: "clusterData.status == \"Active\""
`), &cfg)
		require.NoError(t, err)
		require.Len(t, cfg.Params, 1)
		p := cfg.Params[0]
		assert.True(t, p.Source.IsExpression())
		assert.Contains(t, p.Source.Expression, "clusterData.status")
		assert.False(t, p.Source.IsString())
		assert.False(t, p.Source.IsAPICall())
	})

	t.Run("both api_call and expression in mapping is an error", func(t *testing.T) {
		var cfg AdapterTaskConfig
		err := yaml.Unmarshal([]byte(`
params:
  - name: bad
    source:
      api_call:
        method: GET
        url: /clusters
      expression: "true"
`), &cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "only one of")
	})

	t.Run("mapping with neither api_call nor expression is an error", func(t *testing.T) {
		var cfg AdapterTaskConfig
		err := yaml.Unmarshal([]byte(`
params:
  - name: bad
    source:
      unknown_key: value
`), &cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mapping must contain one of")
	})

	t.Run("empty expression is an error", func(t *testing.T) {
		var cfg AdapterTaskConfig
		err := yaml.Unmarshal([]byte(`
params:
  - name: bad
    source:
      expression: ""
`), &cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "expression must not be empty")
	})

	t.Run("empty expression with file triggers mutual exclusivity", func(t *testing.T) {
		var cfg AdapterTaskConfig
		err := yaml.Unmarshal([]byte(`
params:
  - name: bad
    source:
      expression: ""
      file:
        path: "/some/file"
`), &cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "only one of")
	})
}

const taskOnlyYAML = `
schema_version: "2.0"
params:
  - name: "clusterId"
    source: "event.id"
    type: "string"
    required: true
resources:
  - name: "deployment"
    manifest:
      ref: "templates/deployment.yaml"
    discovery:
      namespace: "*"
      by_selectors:
        label_selector:
          app: "test"
post:
  payloads:
    - name: "statusPayload"
      build_ref: "templates/status-payload.yaml"
`

const taskOnlyManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: "{{ .clusterId }}"
spec:
  replicas: 1
`

const taskOnlyPayload = `status: "{{ .clusterId }}"
message: "Operation completed"
`

func taskOnlyFS() fstest.MapFS {
	return fstest.MapFS{
		"templates/deployment.yaml":     {Data: []byte(taskOnlyManifest)},
		"templates/status-payload.yaml": {Data: []byte(taskOnlyPayload)},
	}
}

func writeMapFS(t *testing.T, dir string, fsys fstest.MapFS) {
	t.Helper()
	for name, file := range fsys {
		full := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0755))
		require.NoError(t, os.WriteFile(full, file.Data, 0644))
	}
}

func taskOnlyRefYAML(ref string) string {
	return `
schema_version: "2.0"
resources:
  - name: "deployment"
    manifest:
      ref: "` + ref + `"
    discovery:
      namespace: "*"
      by_name: "test"
`
}

func TestLoadTaskConfig(t *testing.T) {
	ctx := t.Context()

	t.Run("loads file references from the supplied FS", func(t *testing.T) {
		config, err := LoadTaskConfig(ctx, []byte(taskOnlyYAML),
			WithFS(taskOnlyFS()), WithAdapterName("library-adapter"))
		require.NoError(t, err)
		require.NotNil(t, config)

		assert.Equal(t, "2.0", config.SchemaVersion)
		assert.Equal(t, "library-adapter", config.Adapter.Name)
		assert.Equal(t, ClientsConfig{}, config.Clients)
		assert.Nil(t, config.Transports)
		assert.Nil(t, config.Stores)

		require.Len(t, config.Resources, 1)
		assert.Equal(t, taskOnlyManifest, config.Resources[0].Manifest)

		require.NotNil(t, config.Post)
		require.Len(t, config.Post.Payloads, 1)
		payload := config.Post.Payloads[0]
		assert.Equal(t, "templates/status-payload.yaml", payload.BuildRef)
		assert.Equal(t, "{{ .clusterId }}", payload.BuildRefContent["status"])
		assert.Equal(t, "Operation completed", payload.BuildRefContent["message"])
	})

	t.Run("adapter name is empty without the option", func(t *testing.T) {
		config, err := LoadTaskConfig(ctx, []byte(taskOnlyYAML), WithFS(taskOnlyFS()))
		require.NoError(t, err)
		assert.Equal(t, "", config.Adapter.Name)
	})

	t.Run("file reference errors name the field", func(t *testing.T) {
		fsys := fstest.MapFS{
			"manifests/job.yaml": {Data: []byte(taskOnlyManifest)},
		}
		tests := []struct {
			name   string
			ref    string
			fsys   fs.FS
			errMsg string
		}{
			{
				name:   "no file system",
				ref:    "manifests/job.yaml",
				errMsg: `file reference "manifests/job.yaml" needs a file system; none was supplied`,
			},
			{
				name:   "missing file",
				ref:    "manifests/missing.yaml",
				fsys:   fsys,
				errMsg: `referenced file "manifests/missing.yaml" does not exist`,
			},
			{
				name:   "directory",
				ref:    "manifests",
				fsys:   fsys,
				errMsg: `referenced path "manifests" is a directory, not a file`,
			},
			{
				name:   "escape",
				ref:    "../x.yaml",
				fsys:   fsys,
				errMsg: `path "../x.yaml" escapes base directory`,
			},
			{
				name:   "absolute",
				ref:    "/x.yaml",
				fsys:   fsys,
				errMsg: `path "/x.yaml" must be relative to the task config file system`,
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				var opts []TaskLoadOption
				if tt.fsys != nil {
					opts = append(opts, WithFS(tt.fsys))
				}
				config, err := LoadTaskConfig(ctx, []byte(taskOnlyRefYAML(tt.ref)), opts...)
				require.Error(t, err)
				assert.Nil(t, config)
				assert.Contains(t, err.Error(), "task config file reference validation failed")
				assert.Contains(t, err.Error(), "resources[0].manifest.ref: "+tt.errMsg)
			})
		}
	})
}

func TestLoadTaskConfigMatchesLoadConfig(t *testing.T) {
	load := func(t *testing.T, taskYAML string) (fromTask, fromLoad *Config, taskErr, loadErr error) {
		t.Helper()
		tmpDir := t.TempDir()
		writeMapFS(t, tmpDir, taskOnlyFS())
		adapterPath, taskPath := createTestConfigFiles(t, tmpDir, testAdapterConfigYAML, taskYAML)

		fromTask, taskErr = LoadTaskConfig(t.Context(), []byte(taskYAML), WithFS(taskOnlyFS()))
		fromLoad, loadErr = LoadConfig(WithAdapterConfigPath(adapterPath), WithTaskConfigPath(taskPath))
		return fromTask, fromLoad, taskErr, loadErr
	}

	t.Run("valid task config gives equal task fields", func(t *testing.T) {
		fromTask, fromLoad, taskErr, loadErr := load(t, taskOnlyYAML)
		require.NoError(t, taskErr)
		require.NoError(t, loadErr)
		assert.Equal(t, fromLoad.SchemaVersion, fromTask.SchemaVersion)
		assert.Equal(t, fromLoad.Params, fromTask.Params)
		assert.Equal(t, fromLoad.Preconditions, fromTask.Preconditions)
		assert.Equal(t, fromLoad.Resources, fromTask.Resources)
		assert.Equal(t, fromLoad.Post, fromTask.Post)
	})

	resourceYAML := `
resources:
  - name: "namespace"
    manifest:
      apiVersion: v1
      kind: Namespace
      metadata:
        name: "test"
    discovery:
      by_name: "test"
`
	tests := []struct {
		name     string
		taskYAML string
		errMsg   string
	}{
		{
			name:     "unknown field (strict decode)",
			taskYAML: "schema_version: \"2.0\"\nunknown_field: true\n" + resourceYAML,
			errMsg:   "failed to load task config: failed to parse task config YAML",
		},
		{
			name:     "param without source (structural)",
			taskYAML: "schema_version: \"2.0\"\nparams:\n  - name: \"region\"\n" + resourceYAML,
			errMsg:   "task config validation failed: params[0].source is required",
		},
		{
			name:     "missing schema_version",
			taskYAML: resourceYAML,
			errMsg:   "failed to load task config: schema_version is required",
		},
		{
			name: "resource without a name (structural)",
			taskYAML: `
schema_version: "2.0"
resources:
  - manifest:
      apiVersion: v1
      kind: Namespace
      metadata:
        name: "test"
    discovery:
      by_name: "test"
`,
			errMsg: "task config validation failed: resources[0].name is required",
		},
		{
			name: "unknown transport",
			taskYAML: `
schema_version: "2.0"
resources:
  - name: "namespace"
    transport: remote-primary
    manifest:
      apiVersion: v1
      kind: Namespace
      metadata:
        name: "test"
    discovery:
      by_name: "test"
`,
			errMsg: `resource transport validation failed: resources[0].transport references unknown transport "remote-primary"`,
		},
		{
			name: "invalid CEL expression (semantic)",
			taskYAML: `
schema_version: "2.0"
params:
  - name: "broken"
    source:
      expression: "event.id +"
` + resourceYAML,
			errMsg: "params[0].source.expression: CEL parse error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fromTask, fromLoad, taskErr, loadErr := load(t, tt.taskYAML)
			assert.Nil(t, fromTask)
			assert.Nil(t, fromLoad)
			require.Error(t, taskErr)
			require.Error(t, loadErr)
			assert.Contains(t, loadErr.Error(), tt.errMsg)
			assert.Equal(t, loadErr.Error(), taskErr.Error())
		})
	}
}

func TestLoadTaskConfigSkipsHostEnv(t *testing.T) {
	const envName = "HYPERFLEET_TEST_LOAD_TASK_CONFIG_UNSET"
	t.Setenv(envName, "")
	require.NoError(t, os.Unsetenv(envName))

	taskYAML := `
schema_version: "2.0"
params:
  - name: "region"
    source: "env.` + envName + `"
    required: true
resources:
  - name: "namespace"
    manifest:
      apiVersion: v1
      kind: Namespace
      metadata:
        name: "test"
    discovery:
      by_name: "test"
`

	config, err := LoadTaskConfig(t.Context(), []byte(taskYAML))
	require.NoError(t, err)
	require.Len(t, config.Params, 1)
	assert.Equal(t, "env."+envName, config.Params[0].Source.StringVal)

	tmpDir := t.TempDir()
	adapterPath, taskPath := createTestConfigFiles(t, tmpDir, testAdapterConfigYAML, taskYAML)
	_, err = LoadConfig(WithAdapterConfigPath(adapterPath), WithTaskConfigPath(taskPath))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "required environment variable "+envName+" is not set")
}

func TestLoadConfigTaskConfigPathFromEnv(t *testing.T) {
	tmpDir := t.TempDir()
	writeMapFS(t, tmpDir, taskOnlyFS())
	adapterPath, taskPath := createTestConfigFiles(t, tmpDir, testAdapterConfigYAML, taskOnlyYAML)
	t.Setenv(EnvTaskConfigPath, taskPath)

	config, err := LoadConfig(WithAdapterConfigPath(adapterPath))
	require.NoError(t, err)
	require.Len(t, config.Resources, 1)
	assert.Equal(t, taskOnlyManifest, config.Resources[0].Manifest)
}
