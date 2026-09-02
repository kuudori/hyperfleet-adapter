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
			errorMsg: `transports.unknown.type "unsupported" is unsupported (supported: kubernetes, remote)`,
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
			errorMsg: "transports.remote.store is required for remote transport",
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
			errorMsg: `transports.remote.store references unknown store "missing"`,
		},
		{
			name: "store is not referenced by a remote transport",
			yaml: `
adapter:
  name: test-adapter
transports:
  kubernetes:
    type: kubernetes
stores:
  adapter-desires:
    type: memory
`,
			errorMsg: "stores.adapter-desires is not referenced by any remote transport",
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
			errorMsg: `stores.desired.type "unsupported" is unsupported (supported: memory, redis)`,
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
			errorMsg: "stores.desired.url is required for redis store",
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
			errorMsg: "stores.desired.url is invalid for redis store",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var config AdapterConfig
			err := yaml.Unmarshal([]byte(tt.yaml), &config)
			require.NoError(t, err)

			err = NewAdapterConfigValidator(&config, "").ValidateStructure()
			require.EqualError(t, err, tt.errorMsg)
		})
	}
}

func TestAdapterConfigValidationDoesNotExposeCredentialsFromInvalidRedisURL(t *testing.T) {
	const password = "super-secret"
	config := &AdapterConfig{
		Adapter: AdapterInfo{Name: "test-adapter"},
		Stores: map[string]StoreDefinition{
			"credentials": {
				Type: StoreTypeRedis,
				URL:  "redis://adapter:" + password + "@redis.example.com:%",
			},
		},
	}

	err := NewAdapterConfigValidator(config, "").ValidateStructure()

	require.Error(t, err)
	assert.ErrorContains(t, err, "stores.credentials.url is invalid")
	assert.NotContains(t, err.Error(), password)
}

func TestAdapterConfigValidationRequiresTLSForRedisCredentials(t *testing.T) {
	tests := []struct {
		name     string
		redisURL string
		wantErr  string
	}{
		{
			name:     "credentials over redis",
			redisURL: "redis://adapter:super-secret@redis.example.com:6379/0",
			wantErr:  "must use rediss:// when credentials are configured",
		},
		{
			name:     "no credentials over redis",
			redisURL: "redis://redis.example.com:6379/0",
		},
		{
			name:     "credentials over rediss",
			redisURL: "rediss://adapter:super-secret@redis.example.com:6379/0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := &AdapterConfig{
				Adapter: AdapterInfo{Name: "test-adapter"},
				Stores: map[string]StoreDefinition{
					"desired": {Type: StoreTypeRedis, URL: tt.redisURL},
				},
				Transports: map[string]TransportDefinition{
					"remote": {Type: TransportTypeRemote, Store: "desired"},
				},
			}

			err := NewAdapterConfigValidator(config, "").ValidateStructure()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.wantErr)
			assert.NotContains(t, err.Error(), "super-secret")
		})
	}
}

func TestAdapterConfigValidationRejectsLegacyMaestroWithNamedTransports(t *testing.T) {
	config := &AdapterConfig{
		Adapter: AdapterInfo{Name: "test-adapter"},
		Clients: ClientsConfig{Maestro: new(MaestroClientConfig)},
		Transports: map[string]TransportDefinition{
			"remote": {Type: TransportTypeRemote, Store: "desired"},
		},
	}

	err := NewAdapterConfigValidator(config, "").ValidateStructure()

	require.Error(t, err)
	assert.ErrorContains(t, err, "clients.maestro cannot be configured when transports are set")
}
