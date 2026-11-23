package configloader

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// apiStoreExample returns the fenced yaml block after the
// <!-- api-store-example --> marker in docs/configuration.md.
func apiStoreExample(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "configuration.md"))
	require.NoError(t, err)
	_, after, found := strings.Cut(string(raw), "<!-- api-store-example -->\n```yaml\n")
	require.True(t, found, "docs/configuration.md has no api store example")
	block, _, found := strings.Cut(after, "```")
	require.True(t, found, "the api store example is not closed")
	return block
}

func TestLoadConfigAcceptsDocumentedAPIStore(t *testing.T) {
	adapterPath, taskPath := createTestConfigFiles(t, t.TempDir(), apiStoreExample(t), `schema_version: "2.0"`)
	config, err := LoadConfig(
		WithAdapterConfigPath(adapterPath),
		WithTaskConfigPath(taskPath),
		WithSkipSemanticValidation(),
	)
	require.NoError(t, err)
	assert.Equal(t, StoreDefinition{
		Type: StoreTypeAPI, URL: "https://desire-store-gateway.example.com",
	}, config.Stores["desire-store"])
}

func TestLoadConfigReadsAPIStoreFields(t *testing.T) {
	adapterYAML := strings.Replace(apiStoreExample(t), "    url: https://desire-store-gateway.example.com\n",
		`    url: https://gateway.example/prefix/
    token_path: /var/run/secrets/custom/token
    token_scheme: ServiceAccount
    ca_file: /etc/hyperfleet/gateway-ca/ca.crt
`, 1)
	adapterPath, taskPath := createTestConfigFiles(t, t.TempDir(), adapterYAML, `schema_version: "2.0"`)
	config, err := LoadConfig(
		WithAdapterConfigPath(adapterPath),
		WithTaskConfigPath(taskPath),
		WithSkipSemanticValidation(),
	)
	require.NoError(t, err)
	assert.Equal(t, StoreDefinition{
		Type: StoreTypeAPI, URL: "https://gateway.example/prefix/", TokenPath: "/var/run/secrets/custom/token",
		TokenScheme: "ServiceAccount", CAFile: "/etc/hyperfleet/gateway-ca/ca.crt",
	}, config.Stores["desire-store"])
}

func TestAdapterConfigValidationOfAPIStore(t *testing.T) {
	tests := []struct {
		name     string
		store    string
		errorMsg string
	}{
		{name: "non-loopback HTTP",
			store:    "type: api\n    url: http://gateway.example",
			errorMsg: "stores.desired.url is invalid for api store: url must use https outside loopback"},
		{name: "memory with token", store: "type: memory\n    token_path: /t/token",
			errorMsg: "stores.desired.token_path, token_scheme and ca_file are only valid for api stores"},
		{name: "redis with CA", store: "type: redis\n    url: redis://redis:6379\n    ca_file: /c/ca.crt",
			errorMsg: "stores.desired.token_path, token_scheme and ca_file are only valid for api stores"},
		{name: "trailing slash URL", store: "type: api\n    url: https://gateway.example/"},
		{name: "path prefix URL", store: "type: api\n    url: https://gateway.example/prefix"},
		{name: "all optional fields", store: "type: api\n    url: https://gateway.example\n" +
			"    token_path: /t/token\n    token_scheme: Bearer\n    ca_file: /c/ca.crt"},
		{
			name: "missing URL", store: "type: api",
			errorMsg: "stores.desired.url is required for api store",
		},
		{
			name: "URL with query", store: "type: api\n    url: https://gateway.example?x=1",
			errorMsg: `stores.desired.url is invalid for api store: url must not have a query`,
		},
		{
			name: "non-HTTP URL", store: "type: api\n    url: redis://gateway.example:6379",
			errorMsg: `stores.desired.url is invalid for api store: url must use http or https`,
		},
		{
			name: "URL with user info", store: "type: api\n    url: https://user:secret@gateway.example",
			errorMsg: "stores.desired.url is invalid for api store: url must not contain user info",
		},
		{
			name: "relative token_path", store: "type: api\n    url: https://gateway.example\n    token_path: token",
			errorMsg: `stores.desired.token_path must be an absolute path, got "token"`,
		},
		{
			name: "relative ca_file", store: "type: api\n    url: https://gateway.example\n    ca_file: ca.crt",
			errorMsg: `stores.desired.ca_file must be an absolute path, got "ca.crt"`,
		},
		{
			name: "token_scheme with a space", store: "type: api\n    url: https://gateway.example\n" +
				"    token_scheme: \"Bearer x\"",
			errorMsg: `stores.desired.token_scheme must be a single word of HTTP token characters, got "Bearer x"`,
		},
		{
			name: "token_scheme with a control character", store: "type: api\n    url: https://gateway.example\n" +
				"    token_scheme: \"Bearer\\u0001\"",
			errorMsg: `stores.desired.token_scheme must be a single word of HTTP token characters, got "Bearer\x01"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := `
adapter:
  name: test-adapter
stores:
  desired:
    ` + tt.store + `
transports:
  remote:
    type: remote
    store: desired
    target_cluster: cluster-1
    resource_plurals:
      "v1/ConfigMap": configmaps
`
			var config AdapterConfig
			require.NoError(t, yaml.Unmarshal([]byte(raw), &config))
			err := NewAdapterConfigValidator(&config, "").ValidateStructure()
			if tt.errorMsg == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tt.errorMsg)
			assert.NotContains(t, err.Error(), "secret")
		})
	}
}

func TestAPIStoreURLErrorsDoNotEchoCredentials(t *testing.T) {
	var config AdapterConfig
	require.NoError(t, yaml.Unmarshal([]byte(`
adapter:
  name: test-adapter
stores:
  desired:
    type: api
    url: redis://user:secret@gateway.example
transports:
  remote:
    type: remote
    store: desired
    target_cluster: cluster-1
    resource_plurals:
      "v1/ConfigMap": configmaps
`), &config))
	err := NewAdapterConfigValidator(&config, "").ValidateStructure()
	require.EqualError(t, err, "stores.desired.url is invalid for api store: url must not contain user info")
}
