// Package transportregistry builds the configured transport clients and owns
// the resources whose lifetimes they require.
package transportregistry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/configloader"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/desireclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/k8sclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/maestroclient"
	"github.com/openshift-hyperfleet/hyperfleet-adapter/internal/transportclient"
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

	runtime := &Runtime{Registry: make(transportclient.Registry)}
	if len(config.Transports) == 0 {
		if err := runtime.buildLegacy(ctx, config); err != nil {
			closeAfterBuildFailure(ctx, runtime)
			return nil, err
		}
		return runtime, nil
	}

	stores, err := runtime.buildStores(ctx, config.Stores)
	if err != nil {
		closeAfterBuildFailure(ctx, runtime)
		return nil, err
	}

	for _, name := range sortedTransportNames(config.Transports) {
		definition := config.Transports[name]
		client, err := buildTransport(ctx, config, definition, stores)
		if err != nil {
			closeAfterBuildFailure(ctx, runtime)
			return nil, fmt.Errorf("build transport %q: %w", name, err)
		}
		runtime.Registry[name] = client
	}

	return runtime, nil
}

func closeAfterBuildFailure(ctx context.Context, runtime *Runtime) {
	if err := runtime.Close(); err != nil {
		slog.WarnContext(ctx, "failed to close transport registry after build failure", "error", err)
	}
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

	runtime := &Runtime{Registry: make(transportclient.Registry)}
	for name := range config.Transports {
		runtime.Registry[name] = client
	}
	// The executor still needs its legacy singleton key even when the deployment
	// uses named entries. Mirror the production Maestro-first choice here, but
	// without constructing any network clients.
	if config.Clients.Maestro != nil {
		runtime.Registry[configloader.TransportClientMaestro] = client
	} else {
		runtime.Registry[configloader.TransportClientKubernetes] = client
	}
	return runtime, nil
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

func (r *Runtime) buildLegacy(ctx context.Context, config *configloader.Config) error {
	// Preserve the old selection behavior: Maestro is the sole default when it
	// is configured; Kubernetes is otherwise the default.
	if config.Clients.Maestro != nil {
		client, err := buildMaestro(ctx, config.Clients.Maestro)
		if err != nil {
			return fmt.Errorf("build transport %q: %w", configloader.TransportClientMaestro, err)
		}
		r.Registry[configloader.TransportClientMaestro] = client
		r.closers = append(r.closers, client)
		return nil
	}

	client, err := buildKubernetes(ctx, config.Clients.Kubernetes)
	if err != nil {
		return fmt.Errorf("build transport %q: %w", configloader.TransportClientKubernetes, err)
	}
	r.Registry[configloader.TransportClientKubernetes] = client
	return nil
}

func (r *Runtime) buildStores(
	ctx context.Context,
	definitions map[string]configloader.StoreDefinition,
) (map[string]desire.SpecStore, error) {
	stores := make(map[string]desire.SpecStore, len(definitions))
	for _, name := range sortedStoreNames(definitions) {
		definition := definitions[name]
		switch definition.Type {
		case configloader.StoreTypeMemory:
			stores[name] = memory.New()
		case configloader.StoreTypeRedis:
			options, err := redis.ParseURL(definition.URL)
			if err != nil {
				return nil, fmt.Errorf("build store %q: parse Redis URL: %w", name, err)
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
			stores[name] = redisstore.New(client)
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
		store, ok := stores[definition.Store]
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

func buildMaestro(
	ctx context.Context,
	config *configloader.MaestroClientConfig,
) (*maestroclient.Client, error) {
	maestroConfig := &maestroclient.Config{
		MaestroServerAddr: config.HTTPServerAddress,
		GRPCServerAddr:    config.GRPCServerAddress,
		SourceID:          config.SourceID,
		Insecure:          config.Insecure,
	}
	if config.Timeout != "" {
		timeout, err := time.ParseDuration(config.Timeout)
		if err != nil {
			return nil, fmt.Errorf("invalid maestro timeout %q: %w", config.Timeout, err)
		}
		maestroConfig.HTTPTimeout = timeout
	}
	if config.ServerHealthinessTimeout != "" {
		timeout, err := time.ParseDuration(config.ServerHealthinessTimeout)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid maestro serverHealthinessTimeout %q: %w",
				config.ServerHealthinessTimeout,
				err,
			)
		}
		maestroConfig.ServerHealthinessTimeout = timeout
	}
	if config.Auth.TLSConfig != nil {
		maestroConfig.CAFile = config.Auth.TLSConfig.CAFile
		maestroConfig.ClientCertFile = config.Auth.TLSConfig.CertFile
		maestroConfig.ClientKeyFile = config.Auth.TLSConfig.KeyFile
		maestroConfig.HTTPCAFile = config.Auth.TLSConfig.HTTPCAFile
	}
	return maestroclient.NewMaestroClient(ctx, maestroConfig)
}

func sortedStoreNames(definitions map[string]configloader.StoreDefinition) []string {
	return slices.Sorted(maps.Keys(definitions))
}

func sortedTransportNames(definitions map[string]configloader.TransportDefinition) []string {
	return slices.Sorted(maps.Keys(definitions))
}
