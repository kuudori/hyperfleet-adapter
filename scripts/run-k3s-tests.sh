#!/usr/bin/env bash
# Run integration tests with K3s (requires privileged containers)

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
source "$SCRIPT_DIR/container-runtime.sh"

CONTAINER_RUNTIME=$(detect_container_runtime)
TEST_TIMEOUT="${TEST_TIMEOUT:-30m}"

if [ "$CONTAINER_RUNTIME" = "none" ]; then
    echo "⚠️  ERROR: No container runtime found (docker/podman)"
    echo "   Please install Docker Desktop or Podman to run integration tests"
    exit 1
fi

echo "✅ Container runtime: $CONTAINER_RUNTIME"

if [ "$CONTAINER_RUNTIME" = "podman" ]; then
    PODMAN_SOCK=$(find_podman_socket)
    if [ -n "$PODMAN_SOCK" ]; then
        export DOCKER_HOST="unix://$PODMAN_SOCK"
        echo "   Using Podman socket: $DOCKER_HOST"
    else
        echo "⚠️  WARNING: Podman socket not found, tests may fail"
    fi

    echo ""
    echo "🔍 Checking Podman configuration for K3s compatibility..."

    ROOTFUL=$(is_podman_rootful)
    if [ "$ROOTFUL" = "false" ]; then
        display_k3s_rootless_warning
    elif [ "$ROOTFUL" = "true" ]; then
        echo "   ✅ Podman is in ROOTFUL mode (compatible with K3s)"
    else
        echo "   ⚠️  Could not determine Podman mode (machine may not be running)"
    fi
fi

echo ""
echo "🚀 Starting K3s integration tests..."
echo "   Strategy: K3s (testcontainers)"
echo "   Note: This may require privileged containers"
echo "   Note: K3s startup takes 30-60 seconds"

# Setup environment for tests
export INTEGRATION_STRATEGY=k3s

if [ "$CONTAINER_RUNTIME" = "podman" ]; then
    echo "📡 Detecting proxy configuration from Podman machine..."
    echo "   Setting TESTCONTAINERS_RYUK_DISABLED=true (Podman compatibility)"

    PROXY_HTTP=$(get_podman_proxy "HTTP_PROXY")
    PROXY_HTTPS=$(get_podman_proxy "HTTPS_PROXY")

    echo ""
    if [ -n "$PROXY_HTTP" ] || [ -n "$PROXY_HTTPS" ]; then
        echo "   Using HTTP_PROXY=$PROXY_HTTP"
        echo "   Using HTTPS_PROXY=$PROXY_HTTPS"
        export HTTP_PROXY="$PROXY_HTTP"
        export HTTPS_PROXY="$PROXY_HTTPS"
    fi

    export TESTCONTAINERS_RYUK_DISABLED=true
    export TESTCONTAINERS_LOG_LEVEL=INFO
else
    echo ""
fi

# Run tests
cd "$PROJECT_ROOT"
go test -v -count=1 -tags=integration ./test/integration/... -timeout "$TEST_TIMEOUT"

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "✅ K3s integration tests passed!"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
