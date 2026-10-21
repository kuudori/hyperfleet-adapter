# Integration Tests

This directory contains integration tests for the HyperFleet adapter components.

## Overview

Integration tests verify functionality against real Kubernetes API servers running in containers using [Testcontainers](https://golang.testcontainers.org/).

### Available Test Strategies

We provide **two independent testing strategies** optimized for different use cases:

| Strategy | Command | Speed | Privileges | Best For |
|----------|---------|-------|------------|----------|
| **Pre-built Envtest** | `make test-integration` | ~192s | ✅ Unprivileged | CI/CD, Prow, GitHub Actions |
| **K3s** | `make test-integration-k3s` | ~27s **(7x faster)** | ⚠️ May need privileged | Local dev, performance |

## Quick Start

```bash
# Strategy 1: Pre-built Envtest (default - unprivileged, CI/CD friendly)
make test-integration

# Strategy 2: K3s (faster - for local development or privileged CI)
make test-integration-k3s
```

**Choose based on your environment:**
- **CI/CD with privilege restrictions?** → Use `make test-integration`
- **Local development or privileged CI/CD?** → Use `make test-integration-k3s`

## Prerequisites

### Required

- **Go 1.24+**
- **Container Runtime**: Either Docker or Podman must be running
  - **Docker**: Install [Docker Desktop](https://www.docker.com/products/docker-desktop) - Verify: `docker info`
  - **Podman**: Install [Podman Desktop](https://podman-desktop.io/) or CLI - Verify: `podman info`

### ✅ Container Runtime Support

**Both Docker and Podman are fully supported!** The Makefile automatically detects which runtime you have and configures it appropriately.

**Tested On:**
- ✅ Docker Desktop (macOS, Linux, Windows)
- ✅ Podman Desktop (macOS, Linux)
- ✅ Rootful Podman (macOS)
- ✅ Rootless Podman (Linux)
- ✅ Corporate networks with proxy

## Strategy 1: Pre-built Envtest (Default)

### Description

- Uses a pre-built Docker/Podman image with Kubernetes API server and etcd
- Image built from `test/Dockerfile.integration`
- **Runs in unprivileged containers** - works in restrictive CI/CD environments
- Guaranteed compatibility across all platforms

### When to Use

- ✅ CI/CD platforms with security restrictions (e.g., Prow, GitHub Actions)
- ✅ Environments that don't allow privileged containers
- ✅ When you need guaranteed compatibility
- ✅ Default choice for most scenarios

### Usage

```bash
# Run tests with pre-built envtest (default)
make test-integration

# Build the integration image manually (optional)
make image-integration-test

# Use custom image in CI/CD
INTEGRATION_ENVTEST_IMAGE=quay.io/your-org/integration-test:v1 make test-integration
```

### How It Works

1. **Image Build**: `test/Dockerfile.integration` creates image with:
   - Go 1.25
   - Kubernetes 1.30.x binaries (etcd, kube-apiserver, kubectl)
   - Pre-generated service account keys
   - Startup script that launches etcd and kube-apiserver

2. **Container Setup**: Each test suite starts a fresh container
3. **API Server Start**: Container automatically starts etcd and kube-apiserver
4. **Client Creation**: Test creates k8s client using container's API server endpoint
5. **Test Execution**: Tests run against the real Kubernetes API
6. **Cleanup**: Containers automatically terminated after tests complete

### Performance

- **First Run**: ~5-10 seconds (pulls/builds image if needed)
- **Subsequent Runs**: ~19s per test suite
- **Total**: ~192s for all tests (10 suites, 24 test cases)
- **Reason**: Container startup + API server initialization overhead

### Environment Variables

- `INTEGRATION_ENVTEST_IMAGE`: Image to use (default: `localhost/hyperfleet-integration-test:latest`)
- `HTTP_PROXY`, `HTTPS_PROXY`: Automatically detected from Podman machine
- `TESTCONTAINERS_RYUK_DISABLED`: Automatically set to `true` for Podman

## Strategy 2: K3s (Faster)

> ⚠️ **Note:** K3s requires proper cgroup v2 support. If you encounter errors like "failed to find cpuset cgroup", use `make test-integration` (pre-built envtest) instead. See troubleshooting section below.

> 🔍 **Automatic Check:** The Makefile automatically checks if Podman is in rootful mode before running K3s tests. If rootless Podman is detected, it will exit gracefully with instructions to either use `make test-integration` or switch to rootful mode.

### Description

- Uses [K3s](https://k3s.io/) - lightweight Kubernetes distribution
- **Full Kubernetes cluster** with controllers, networking, CRDs
- **Requires cgroup v2 support** - may not work in all environments
- **Requires rootful Podman on macOS** - automatically checked by Makefile
- Much faster (~7x) than envtest for test execution

### When to Use

- ✅ Local development for fast iteration
- ✅ CI/CD environments that support privileged containers
- ✅ When you need full Kubernetes features (controllers, webhooks, etc.)
- ✅ Performance-critical testing scenarios

### Usage

```bash
# Run tests with K3s strategy
make test-integration-k3s
```

### How It Works

1. **Makefile** sets `INTEGRATION_STRATEGY=k3s`
2. **Container Setup**: Testcontainers spins up a K3s container
3. **Kubernetes Ready**: K3s initializes with full cluster (scheduler, controller manager, etc.)
4. **Test Execution**: Tests run against the real K3s cluster
5. **Cleanup**: K3s containers automatically terminated

### Performance

- **Startup**: ~2-3s per test suite
- **Total**: ~27s for all tests (10 suites, 24 test cases)
- **Speedup**: ~7x faster than pre-built envtest

### Privilege Requirements & Compatibility

- **Docker Desktop**: Usually works ✅
- **Podman (macOS rootful)**: Works ✅ (automatically verified by Makefile)
- **Podman (macOS rootless)**: Automatically detected and blocked with helpful instructions ⚠️
- **Podman (Linux rootless)**: May fail with cgroup v2 errors ⚠️
- **CI/CD**: May fail with cgroup or privilege errors ⚠️

**Automatic Podman Rootful Check (macOS):**

The Makefile automatically checks if Podman is running in rootful mode:
- ✅ **Rootful mode detected**: Tests proceed normally
- ⚠️ **Rootless mode detected**: Exits gracefully with instructions to either:
  - Use `make test-integration` (recommended)
  - Or switch to rootful Podman (see instructions in output)

**Common K3s Errors:**
```
Error: failed to find cpuset cgroup (v2)
Error: container exited with code 1 or 255
```

**If you see these errors, use `make test-integration` (pre-built envtest) instead.**

### Environment Variables

- `INTEGRATION_STRATEGY`: Set to `k3s` (automatically set by make target)
- `HTTP_PROXY`, `HTTPS_PROXY`: Automatically detected from Podman machine
- `TESTCONTAINERS_RYUK_DISABLED`: Automatically set to `true` for Podman

## Running Tests

### Using Make (Recommended)

```bash
# Run with pre-built envtest (default)
make test-integration

# Run with K3s strategy
make test-integration-k3s

# Run all tests (unit + integration)
make test-all
```

The Makefile automatically handles:
- Container runtime detection (Docker or Podman)
- Image building (for envtest strategy)
- Proxy configuration (for Podman)
- Environment variable setup
- Cleanup

### Using Go Command

Only if you want to run tests directly:

```bash
# Pre-built Envtest strategy
INTEGRATION_ENVTEST_IMAGE=localhost/hyperfleet-integration-test:latest \
  go test -v -tags=integration ./test/integration/... -timeout 30m

# K3s strategy
INTEGRATION_STRATEGY=k3s \
  go test -v -tags=integration ./test/integration/... -timeout 30m
```

**Note**: Always prefer `make test-integration` for proper setup.

## Test Coverage

The integration tests cover:

### K8s Client Tests (`test/integration/k8sclient/`)

- **CRUD Operations**: Create, Get, List, Update, Delete resources
- **Patch Operations**: Strategic merge patch, JSON merge patch
- **Error Scenarios**: NotFound, AlreadyExists, validation errors
- **Multiple Resource Types**: Namespaces, ConfigMaps, Services, Pods, Secrets
- **Label Selectors**: Filtering resources by labels
- **Full Lifecycle**: End-to-end resource management

## Test Structure

```
test/integration/
├── README.md                          # This file
└── k8s_client/
    ├── helper_selector.go             # Strategy selection
    ├── helper_envtest_prebuilt.go     # Pre-built envtest implementation
    ├── helper_testcontainers_k3s.go   # K3s implementation
    └── client_integration_test.go     # Test cases (strategy-agnostic)
```

## Implementation Details

### Strategy Selection

Tests use a unified interface (`TestEnv`) with automatic strategy selection:

```go
// test/integration/k8sclient/helper_selector.go
func SetupTestEnv(t *testing.T) TestEnv {
    strategy := os.Getenv("INTEGRATION_STRATEGY")

    switch strategy {
    case "k3s":
        // Use K3s testcontainers
        return &TestEnvK3s{...}
    default:
        // Use pre-built envtest image
        return SetupTestEnvPrebuilt(t)
    }
}
```

### Writing Tests

Tests are strategy-agnostic and work with both approaches:

```go
//go:build integration

func TestMyFeature(t *testing.T) {
    // SetupTestEnv automatically selects strategy based on env vars
    env := SetupTestEnv(t)
    defer env.Cleanup(t)

    // Use the unified interface
    client := env.GetClient()
    ctx := env.GetContext()

    // Your test logic...
    ns := &unstructured.Unstructured{...}
    created, err := client.CreateResource(ctx, ns)
    // ...
}
```

## Performance Comparison

### Test Suite Breakdown

```
Pre-built Envtest:
├── TestIntegration_NewClient:       19.21s
├── TestIntegration_CreateResource:  18.61s
├── TestIntegration_GetResource:     18.57s
├── TestIntegration_ListResources:   18.49s
├── TestIntegration_UpdateResource:  18.44s
├── TestIntegration_DeleteResource:  19.02s
├── TestIntegration_ListWithLabels:  18.91s
├── TestIntegration_PatchResource:   19.18s
├── TestIntegration_ErrorScenarios:  18.75s
└── TestIntegration_DifferentTypes:  19.71s
Total: ~192s

K3s:
├── TestIntegration_NewClient:       2.56s
├── TestIntegration_CreateResource:  2.63s
├── TestIntegration_GetResource:     2.48s
├── TestIntegration_ListResources:   2.51s
├── TestIntegration_UpdateResource:  2.47s
├── TestIntegration_DeleteResource:  2.59s
├── TestIntegration_ListWithLabels:  2.54s
├── TestIntegration_PatchResource:   2.62s
├── TestIntegration_ErrorScenarios:  2.68s
└── TestIntegration_DifferentTypes:  3.28s
Total: ~27s (7x faster)
```

### Why K3s is Faster

- Optimized Kubernetes distribution
- Shared container runtime optimizations
- Lighter weight than full kube-apiserver + etcd setup
- Better resource utilization

### Why Pre-built Envtest is Slower

- Cold container start for each test suite
- Full etcd + kube-apiserver startup
- More conservative health checks
- More isolated (each test gets truly fresh environment)

## CI/CD Integration

### GitHub Actions

```yaml
name: Integration Tests

on: [push, pull_request]

jobs:
  test-envtest:
    name: Integration Tests (Envtest)
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      - uses: actions/setup-go@v4
        with:
          go-version: '1.24'
      - name: Run integration tests
        run: make test-integration

  test-k3s:
    name: Integration Tests (K3s)
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      - uses: actions/setup-go@v4
        with:
          go-version: '1.24'
      - name: Run integration tests (K3s)
        run: make test-integration-k3s
```

### GitLab CI

```yaml
integration-tests-envtest:
  image: golang:1.25
  services:
    - docker:dind
  variables:
    # Optional: Use pre-pushed image
    INTEGRATION_ENVTEST_IMAGE: quay.io/your-org/integration-test:v1
  script:
    - make test-integration

integration-tests-k3s:
  image: golang:1.25
  services:
    - docker:dind
  script:
    - make test-integration-k3s
```

### Prow (OpenShift CI)

```yaml
# .prow.yaml
presubmits:
  - name: pull-hyperfleet-adapter-integration-envtest
    decorate: true
    always_run: true
    spec:
      containers:
      - image: golang:1.25
        command:
        - make
        args:
        - test-integration
        env:
        - name: INTEGRATION_ENVTEST_IMAGE
          value: quay.io/your-org/integration-test:v1

  # Optional: K3s strategy if privileged containers are available
  - name: pull-hyperfleet-adapter-integration-k3s
    decorate: true
    optional: true  # Mark as optional in case privileges aren't available
    spec:
      containers:
      - image: golang:1.25
        command:
        - make
        args:
        - test-integration-k3s
        securityContext:
          privileged: true  # May be needed depending on Prow configuration
```

### Building and Pushing Integration Image

For CI/CD, you can pre-build and push the integration image to avoid build time:

```bash
# Build the integration image
docker build -f test/Dockerfile.integration \
  -t quay.io/your-org/integration-test:v1 .

# Push to registry
docker push quay.io/your-org/integration-test:v1

# Use in CI/CD
INTEGRATION_ENVTEST_IMAGE=quay.io/your-org/integration-test:v1 \
  make test-integration
```

## Troubleshooting

### Container Runtime Issues

**Error**: `Neither Docker nor Podman is running`

**Solution**:
- **Docker**: Start Docker Desktop or run `sudo dockerd`
- **Podman**: Run `podman machine start`

Verify: `docker info` or `podman info`

### Pre-built Envtest Issues

**Error**: `INTEGRATION_ENVTEST_IMAGE not set`

**Solution**:
- Use `make test-integration` instead of `go test` directly
- Or build image manually: `make image-integration-test`

**Container creation timeout:**

**Solution**:
- Check container runtime is running
- Check proxy settings if behind corporate firewall
- The Makefile auto-detects proxy from Podman machine

### K3s Issues

**Timeout waiting for K3s:**

**Solution**:
- Check if privileged containers are allowed in your environment
- Try Docker Desktop instead of Podman
- Check proxy settings: `make test-integration-k3s` auto-detects

**API server not ready:**

**Solution**:
- Default timeout is 5 minutes - should be sufficient
- Check container logs: `podman logs <container-id>`
- Check for image pull failures (proxy issues)

### Port Conflicts

**Error**: `address already in use`

**Solution**:
- Testcontainers uses random ports, so this is rare
- Check for stale containers: `docker ps -a`
- Clean up: `docker container prune`

### Resource Constraints

**Error**: `failed to start container` or slow tests

**Solution**:
- Ensure Docker/Podman has sufficient resources (2GB+ RAM recommended)
- For pre-built envtest: 512MB per container is allocated
- Close other resource-intensive applications
- Increase Docker Desktop resource limits in settings

### Proxy Configuration

**Behind a corporate proxy?**

The Makefile automatically detects and configures proxy settings from your Podman machine:

```bash
# Makefile does this automatically:
PROXY_HTTP=$(podman machine ssh 'echo $HTTP_PROXY' 2>/dev/null)
PROXY_HTTPS=$(podman machine ssh 'echo $HTTPS_PROXY' 2>/dev/null)
```

If tests still fail to pull images:
1. Verify proxy settings in your Podman machine: `podman machine ssh env | grep -i proxy`
2. Restart Podman machine: `podman machine stop && podman machine start`
3. Use pre-pushed `INTEGRATION_ENVTEST_IMAGE` to avoid image pulls

## Choosing the Right Strategy

### Decision Tree

```
Start
  │
  ├─ Running in CI/CD?
  │   ├─ Yes → Privileged containers available?
  │   │   ├─ Yes → Try make test-integration-k3s (faster)
  │   │   └─ No  → Use make test-integration (guaranteed to work)
  │   │
  │   └─ No (Local Dev) → Want fastest iteration?
  │       ├─ Yes → Use make test-integration-k3s
  │       └─ No  → Use make test-integration (matches CI)
```

### Recommendations

**For CI/CD:**
- **Start with `make test-integration`** (envtest) - guaranteed compatibility
- **Test `make test-integration-k3s`** on your platform - if it works, use it for speed
- **Run both** in parallel for comprehensive coverage

**For Local Development:**
- **Use `make test-integration-k3s`** for fast iteration (~27s)
- **Periodically test `make test-integration`** to ensure CI compatibility

**For Both:**
You can test both strategies locally to ensure compatibility:

```bash
# Test with pre-built envtest (CI/CD simulation)
make test-integration

# Test with K3s (performance)
make test-integration-k3s
```

## Performance Optimization Tips

### 1. Pre-push Integration Image

For CI/CD, pre-build and push the integration image to avoid build time:

```bash
# One-time: Build and push
docker build -f test/Dockerfile.integration -t quay.io/your-org/integration-test:v1 .
docker push quay.io/your-org/integration-test:v1

# Use in all CI/CD runs (saves ~10-30s per run)
INTEGRATION_ENVTEST_IMAGE=quay.io/your-org/integration-test:v1 make test-integration
```

### 2. Parallel Test Execution

Since each test gets a fresh container, you can run tests in parallel:

```bash
go test -v -tags=integration -parallel 4 ./test/integration/k8sclient/...
```

**Note**: This increases resource usage but can speed up total runtime.

### 3. Cache Container Images

Most CI/CD platforms cache pulled images between runs. Ensure caching is enabled:

- **GitHub Actions**: Images are cached by default
- **GitLab CI**: Use `docker:dind` service with cache
- **Prow**: Configure image caching in cluster

## Additional Resources

- [Testcontainers for Go](https://golang.testcontainers.org/)
- [K3s Documentation](https://docs.k3s.io/)
- [K3s Testcontainers Module](https://golang.testcontainers.org/modules/k3s/)
- [Kubernetes envtest](https://book.kubebuilder.io/reference/envtest.html)
- [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime)

## Summary

✅ **Two separate, independent testing strategies**

✅ **Use `make test-integration` (envtest)** for:
- CI/CD with privilege restrictions (Prow, GitHub Actions)
- Guaranteed compatibility across all platforms
- When unprivileged containers are required

✅ **Use `make test-integration-k3s`** for:
- Local development (7x faster)
- CI/CD with privileged container support
- When you need full Kubernetes features

✅ **Test both strategies** to ensure compatibility across environments
