#!/usr/bin/env bash
# Run integration tests with testcontainers

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
source "$SCRIPT_DIR/container-runtime.sh"

CONTAINER_RUNTIME=$(detect_container_runtime)
TEST_TIMEOUT="${TEST_TIMEOUT:-30m}"

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "🐳 Running Integration Tests with Testcontainers"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

if [ "$CONTAINER_RUNTIME" = "none" ]; then
    display_runtime_error
fi

echo "✅ Container runtime: $CONTAINER_RUNTIME"

if [ "$CONTAINER_RUNTIME" = "podman" ]; then
    PODMAN_SOCK=$(find_podman_socket)
    if [ -n "$PODMAN_SOCK" ]; then
        export DOCKER_HOST="unix://$PODMAN_SOCK"
        export TESTCONTAINERS_RYUK_DISABLED=true
        echo "   Using Podman socket: $DOCKER_HOST"
    else
        echo "⚠️  WARNING: Podman socket not found, tests may fail"
    fi
fi

echo ""
echo "🚀 Starting integration tests..."
echo "   Checking integration image configuration..."

# Check and set integration image
if [ -z "$INTEGRATION_ENVTEST_IMAGE" ]; then
    echo "   INTEGRATION_ENVTEST_IMAGE not set, building local image"
    echo ""
    cd "$PROJECT_ROOT"
    make image-integration-test
    echo ""

    INTEGRATION_ENVTEST_IMAGE="localhost/hyperfleet-integration-test:latest"
fi

echo "   Using INTEGRATION_ENVTEST_IMAGE=$INTEGRATION_ENVTEST_IMAGE"
echo ""

# Setup environment for tests
export INTEGRATION_ENVTEST_IMAGE

if [ "$CONTAINER_RUNTIME" = "podman" ]; then
    echo "📡 Detecting proxy configuration from Podman machine..."
    echo "   Setting TESTCONTAINERS_RYUK_DISABLED=true (Podman compatibility)"

    PROXY_HTTP=$(get_podman_proxy "HTTP_PROXY")
    PROXY_HTTPS=$(get_podman_proxy "HTTPS_PROXY")

    if [ -n "$PROXY_HTTP" ] || [ -n "$PROXY_HTTPS" ]; then
        echo "   Using HTTP_PROXY=$PROXY_HTTP"
        echo "   Using HTTPS_PROXY=$PROXY_HTTPS"
        export HTTP_PROXY="$PROXY_HTTP"
        export HTTPS_PROXY="$PROXY_HTTPS"
    fi

    export TESTCONTAINERS_RYUK_DISABLED=true
    export TESTCONTAINERS_LOG_LEVEL=INFO
fi

# Run tests
cd "$PROJECT_ROOT"
go test -v -count=1 -tags=integration ./test/integration/... -timeout "$TEST_TIMEOUT"

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "✅ Integration tests passed!"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
