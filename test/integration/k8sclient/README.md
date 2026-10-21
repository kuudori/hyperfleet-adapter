# K8s Client Integration Tests

Integration tests for the Kubernetes client (`internal/k8sclient`) using real Kubernetes API servers.

## Quick Start

```bash
# Default strategy (pre-built envtest - unprivileged, CI/CD friendly)
make test-integration

# K3s strategy (faster, may need privileges)
make test-integration-k3s
```

## Documentation

📖 **See [test/integration/README.md](../README.md) for complete documentation**, including:

- Testing strategies comparison
- Setup instructions
- CI/CD integration
- Performance analysis
- Troubleshooting guide

## Test Coverage

These integration tests verify:

- ✅ **CRUD Operations**: Create, Get, List, Update, Delete
- ✅ **Patch Operations**: Strategic merge patch, JSON merge patch
- ✅ **Error Handling**: NotFound, AlreadyExists, validation errors
- ✅ **Resource Types**: Namespaces, ConfigMaps, Services, Pods, Secrets
- ✅ **Label Selectors**: Filtering and querying
- ✅ **Full Lifecycle**: End-to-end resource management

## Test Files

```
test/integration/k8sclient/
├── README.md                       # This file
├── helper_selector.go              # Strategy selection
├── helper_envtest_prebuilt.go      # Pre-built testing with envtest implementation
├── helper_testcontainers_k3s.go    # Testing with k3s implementation
└── client_integration_test.go      # Test cases
```

## Running Tests

### Recommended: Use Make Targets

```bash
# Run all integration tests with pre-built envtest (default)
make test-integration

# Run all integration tests with K3s (faster)
make test-integration-k3s
```

The Makefile automatically handles:
- Image building (for envtest strategy)
- Proxy detection (for Podman)
- Environment variable setup
- Container runtime detection

### Advanced: Run k8s_client Tests Only

If you need to run only k8s_client tests:

```bash
# Pre-built envtest strategy
INTEGRATION_ENVTEST_IMAGE=localhost/hyperfleet-integration-test:latest \
  go test -v -tags=integration ./test/integration/k8sclient/... -timeout 30m

# K3s strategy
INTEGRATION_STRATEGY=k3s \
  go test -v -tags=integration ./test/integration/k8sclient/... -timeout 30m
```

**Note**: Direct `go test` requires manual setup. Use `make test-integration` for proper configuration.

## Test Results

**Pre-built Envtest Strategy:**
```text
PASS
ok  github.com/openshift-hyperfleet/hyperfleet-adapter/test/integration/k8sclient  192.048s
```
- 10 test suites, each creating fresh containers
- ~19s per test suite (container startup + API server initialization)

**K3s Strategy:**
```text
PASS
ok  github.com/openshift-hyperfleet/hyperfleet-adapter/test/integration/k8sclient  26.148s
```
- 10 test suites, each creating fresh K3s clusters
- ~2-3s per test suite
- **7x faster** than envtest

**Note about Test Caching:**
If you see `(cached)` in the output, Go is reusing previous test results. To force a fresh run:
```bash
go clean -testcache && make test-integration
```

## Writing New Tests

Tests are strategy-agnostic and work with both approaches:

```go
//go:build integration

package k8sclient_integration

import (
    "testing"
    "github.com/stretchr/testify/require"
    "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestIntegration_MyFeature(t *testing.T) {
    // SetupTestEnv automatically selects strategy based on INTEGRATION_STRATEGY env var
    env := SetupTestEnv(t)
    defer env.Cleanup(t)

    // Use the unified interface
    client := env.GetClient()
    ctx := env.GetContext()

    // Create a namespace
    ns := &unstructured.Unstructured{
        Object: map[string]interface{}{
            "apiVersion": "v1",
            "kind":       "Namespace",
            "metadata": map[string]interface{}{
                "name": "test-ns",
            },
        },
    }

    created, err := client.CreateResource(ctx, ns)
    require.NoError(t, err)
    require.NotNil(t, created)

    // Your test logic...
}
```

## Performance Comparison

| Strategy | Total Time | Per Test Suite | Speedup |
|----------|------------|----------------|---------|
| **Pre-built Envtest** | ~192s | ~19s | Baseline |
| **K3s** | ~27s | ~2-3s | **7x faster** |

## Troubleshooting

### Common Issues

**Tests fail to start:**
- Ensure Docker or Podman is running: `docker info` or `podman info`
- Use `make test-integration` instead of running `go test` directly

**INTEGRATION_ENVTEST_IMAGE not set:**
- Always use `make test-integration` which handles image building automatically

**K3s timeout or cgroup errors:**
```
Error: failed to find cpuset cgroup (v2)
Error: container exited with code 1 or 255
```

**Solution:** K3s requires proper cgroup v2 support which may not be available in all environments.

**Use pre-built envtest instead:**
```bash
make test-integration  # Works in all environments
```

**Or fix K3s setup:**
- Docker Desktop: Enable virtualization framework in settings
- Podman (macOS): Switch to rootful mode with adequate resources:
  ```bash
  podman machine stop
  podman machine set --rootful=true --cpus 4 --memory 4096
  podman machine start
  ```
- Podman (Linux): Enable cgroup delegation (see main documentation)

**When to use each strategy:**
- `make test-integration` → CI/CD, unfamiliar environments, guaranteed to work
- `make test-integration-k3s` → Local development with proper Docker/Podman setup

### Getting Help

See the main documentation for detailed troubleshooting:
- [test/integration/README.md](../README.md)

## Additional Resources

- [Main Integration Test Documentation](../README.md)
- [k8sclient Package Documentation](../../../internal/k8sclient/README.md)
- [Testcontainers for Go](https://golang.testcontainers.org/)
- [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime)
