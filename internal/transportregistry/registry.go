// Package transportregistry builds the configured transport clients and owns
// the resources whose lifetimes they require.
package transportregistry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/configloader"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/desireclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/k8sclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/pkg/utils"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire"
	"github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire/store/memory"
	redisstore "github.com/openshift-hyperfleet/hyperfleet-applier/pkg/desire/store/redis"
	"github.com/redis/go-redis/v9"
)

const redisPingTimeout = 5 * time.Second

// Runtime is the configured client registry and the resources it owns.
type Runtime struct {
	Registry transportclient.Registry
	closers  []io.Closer
}

// Build constructs each declared transport and its backing stores.
func Build(ctx context.Context, config *configloader.Config) (*Runtime, error) {
	if config == nil {
		return nil, fmt.Errorf("transport registry config is required")
	}
	if err := validateNamedTransportConfig(config); err != nil {
		return nil, fmt.Errorf("validate named transport configuration: %w", err)
	}

	runtime := &Runtime{Registry: make(transportclient.Registry)}
	stores, err := runtime.buildStores(ctx, config.Stores)
	if err != nil {
		closeAfterBuildFailure(ctx, runtime)
		return nil, err
	}

	// Every local transport is built from clients.kubernetes, so all of them,
	// including the implicit one, share a single client.
	var kubernetesClient transportclient.TransportClient
	for _, name := range utils.SortedMapKeys(config.Transports) {
		definition := config.Transports[name]
		client := kubernetesClient
		if definition.Type != configloader.TransportTypeKubernetes || client == nil {
			client, err = buildTransport(ctx, config, definition, stores)
			if err != nil {
				closeAfterBuildFailure(ctx, runtime)
				return nil, fmt.Errorf("build transport %q: %w", name, err)
			}
			if definition.Type == configloader.TransportTypeKubernetes {
				kubernetesClient = client
			}
		}
		runtime.Registry[configloader.NormalizeRegistryName(name)] = client
	}
	if _, configured := runtime.Registry[configloader.TransportClientKubernetes]; !configured &&
		needsImplicitKubernetes(config) {
		if kubernetesClient == nil {
			kubernetesClient, err = buildKubernetes(ctx, config.Clients.Kubernetes)
			if err != nil {
				closeAfterBuildFailure(ctx, runtime)
				return nil, fmt.Errorf("build transport %q: %w", configloader.TransportClientKubernetes, err)
			}
		}
		runtime.Registry[configloader.TransportClientKubernetes] = kubernetesClient
	}

	return runtime, nil
}

func closeAfterBuildFailure(ctx context.Context, runtime *Runtime) {
	if err := runtime.Close(); err != nil {
		slog.WarnContext(ctx, "failed to close transport registry after build failure", "error", err)
	}
}

// validateNamedTransportConfig applies the loader's routing invariants so
// callers that build a registry from a config which did not pass through
// LoadConfig still get the same guardrails.
func validateNamedTransportConfig(config *configloader.Config) error {
	return configloader.ValidateConfigRouting(config)
}

// BuildRecording builds a registry for dry-run execution without creating any
// network clients. Each configured name uses client directly.
func BuildRecording(
	config *configloader.Config,
	client transportclient.TransportClient,
) (*Runtime, error) {
	if config == nil {
		return nil, fmt.Errorf("transport registry config is required")
	}
	if client == nil {
		return nil, fmt.Errorf("recording transport client is required")
	}
	if err := validateNamedTransportConfig(config); err != nil {
		return nil, fmt.Errorf("validate named transport configuration: %w", err)
	}

	runtime := &Runtime{Registry: make(transportclient.Registry)}
	for name := range config.Transports {
		runtime.Registry[configloader.NormalizeRegistryName(name)] = client
	}
	if needsImplicitKubernetes(config) {
		runtime.Registry[configloader.TransportClientKubernetes] = client
	}
	return runtime, nil
}

// needsImplicitKubernetes reports whether any resource omits its transport or
// names local Kubernetes, so a local client must be registered.
func needsImplicitKubernetes(config *configloader.Config) bool {
	for _, resource := range config.Resources {
		if configloader.NormalizeRegistryName(resource.GetTransportName()) == configloader.TransportClientKubernetes {
			return true
		}
	}
	return false
}

// Close releases all resources created by Build. It attempts every close and
// returns the first error encountered.
func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	var firstErr error
	for index := len(r.closers) - 1; index >= 0; index-- {
		if err := r.closers[index].Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	r.closers = nil
	return firstErr
}

func (r *Runtime) buildStores(
	ctx context.Context,
	definitions map[string]configloader.StoreDefinition,
) (map[string]desire.SpecStore, error) {
	stores := make(map[string]desire.SpecStore, len(definitions))
	for _, name := range utils.SortedMapKeys(definitions) {
		definition := definitions[name]
		switch definition.Type {
		case configloader.StoreTypeMemory:
			stores[configloader.NormalizeRegistryName(name)] = memory.New()
		case configloader.StoreTypeRedis:
			options, err := redis.ParseURL(definition.URL)
			if err != nil {
				return nil, fmt.Errorf("build store %q: redis URL is invalid", name)
			}
			client := redis.NewClient(options)
			pingCtx, cancel := context.WithTimeout(ctx, redisPingTimeout)
			err = client.Ping(pingCtx).Err()
			cancel()
			if err != nil {
				if closeErr := client.Close(); closeErr != nil {
					slog.WarnContext(ctx, "failed to close Redis client after ping failure", "error", closeErr)
				}
				return nil, fmt.Errorf("build store %q: ping Redis: %w", name, err)
			}
			r.closers = append(r.closers, client)
			stores[configloader.NormalizeRegistryName(name)] = redisstore.New(client)
		default:
			return nil, fmt.Errorf("build store %q: unsupported type %q", name, definition.Type)
		}
	}
	return stores, nil
}

func buildTransport(
	ctx context.Context,
	config *configloader.Config,
	definition configloader.TransportDefinition,
	stores map[string]desire.SpecStore,
) (transportclient.TransportClient, error) {
	switch definition.Type {
	case configloader.TransportTypeKubernetes:
		return buildKubernetes(ctx, config.Clients.Kubernetes)
	case configloader.TransportTypeRemote:
		store, ok := stores[configloader.NormalizeRegistryName(definition.Store)]
		if !ok {
			return nil, fmt.Errorf("store %q is not configured", definition.Store)
		}
		return desireclient.NewClient(store, config.Adapter.Name), nil
	default:
		return nil, fmt.Errorf("unsupported type %q", definition.Type)
	}
}

func buildKubernetes(
	ctx context.Context,
	config configloader.KubernetesConfig,
) (*k8sclient.Client, error) {
	return k8sclient.NewClient(ctx, k8sclient.ClientConfig{
		KubeConfigPath: config.KubeConfigPath,
		QPS:            config.QPS,
		Burst:          config.Burst,
	})
}
