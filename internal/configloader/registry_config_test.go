package configloader

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestLoadConfigCopiesNamedTransportAndStoreDefinitions(t *testing.T) {
	tmpDir := t.TempDir()

	adapterPath, taskPath := createTestConfigFiles(t, tmpDir, `
adapter:
  name: test-adapter
  version: "1.0.0"
clients:
  hyperfleet_api:
    timeout: 5s
  kubernetes:
    api_version: v1
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
`, `{}`)

	config, err := LoadConfig(
		WithAdapterConfigPath(adapterPath),
		WithTaskConfigPath(taskPath),
		WithSkipSemanticValidation(),
	)
	require.NoError(t, err)
	require.NotNil(t, config)

	assert.Len(t, config.Stores, 1)
	assert.Equal(t, StoreDefinition{Type: StoreTypeMemory}, config.Stores["desired-memory"])
	assert.Len(t, config.Transports, 2)
	assert.Equal(
		t,
		TransportDefinition{Type: TransportTypeRemote, Store: "desired-memory"},
		config.Transports["remote-primary"],
	)
	assert.Equal(
		t,
		TransportDefinition{Type: TransportTypeRemote, Store: "desired-memory"},
		config.Transports["remote-secondary"],
	)
}

func TestConfigRedactedRedactsRedisPasswordWithoutMutatingOriginal(t *testing.T) {
	config := &Config{
		Stores: map[string]StoreDefinition{
			"authenticated": {
				Type: StoreTypeRedis,
				URL:  "rediss://adapter:super-secret@redis.example.com:6379/0",
			},
			"anonymous": {Type: StoreTypeRedis, URL: "redis://redis.example.com:6379/1"},
			"invalid":   {Type: StoreTypeRedis, URL: "not a URL"},
		},
	}

	redacted := config.Redacted()
	require.NotNil(t, redacted)
	require.NotNil(t, redacted.Stores)

	assert.Equal(
		t,
		"rediss://adapter:%2A%2AREDACTED%2A%2A@redis.example.com:6379/0",
		redacted.Stores["authenticated"].URL,
	)
	assert.Equal(t, "redis://redis.example.com:6379/1", redacted.Stores["anonymous"].URL)
	assert.Equal(t, "not a URL", redacted.Stores["invalid"].URL)
	assert.Equal(
		t,
		"rediss://adapter:super-secret@redis.example.com:6379/0",
		config.Stores["authenticated"].URL,
	)
}

func TestAdapterConfigValidationRejectsInvalidNamedRegistryDefinitions(t *testing.T) {
	const unsupportedError = "unsupported"

	tests := []struct {
		name     string
		yaml     string
		errorMsg string
	}{
		{
			name: "unsupported transport type",
			yaml: `
adapter:
  name: test-adapter
transports:
  unknown:
    type: unsupported
`,
			errorMsg: unsupportedError,
		},
		{
			name: "remote transport without store",
			yaml: `
adapter:
  name: test-adapter
transports:
  remote:
    type: remote
`,
			errorMsg: "store is required",
		},
		{
			name: "remote transport references missing store",
			yaml: `
adapter:
  name: test-adapter
transports:
  remote:
    type: remote
    store: missing
`,
			errorMsg: "missing",
		},
		{
			name: "unsupported store type",
			yaml: `
adapter:
  name: test-adapter
stores:
  desired:
    type: unsupported
`,
			errorMsg: unsupportedError,
		},
		{
			name: "redis store without URL",
			yaml: `
adapter:
  name: test-adapter
stores:
  desired:
    type: redis
`,
			errorMsg: "url is required",
		},
		{
			name: "redis store with malformed URL",
			yaml: `
adapter:
  name: test-adapter
stores:
  desired:
    type: redis
    url: not-a-redis-url
`,
			errorMsg: "redis",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var config AdapterConfig
			err := yaml.Unmarshal([]byte(tt.yaml), &config)
			require.NoError(t, err)

			err = NewAdapterConfigValidator(&config, "").ValidateStructure()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errorMsg)
		})
	}
}
