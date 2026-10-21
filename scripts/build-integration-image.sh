#!/usr/bin/env bash
# Build integration test image with envtest

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/container-runtime.sh"

CONTAINER_RUNTIME=$(detect_container_runtime)
CONTAINER_PLATFORM=$(detect_container_platform)

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "🔨 Building Integration Test Image"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

if [ "$CONTAINER_RUNTIME" = "none" ]; then
    echo "❌ ERROR: No container runtime found (docker/podman required)"
    exit 1
fi

echo "📦 Building image: localhost/hyperfleet-integration-test:latest"
echo "   Platform: $CONTAINER_PLATFORM"
echo "   This downloads ~100MB of Kubernetes binaries (one-time operation)"
echo ""

BUILD_ARGS=(
    --platform "$CONTAINER_PLATFORM"
    -t localhost/hyperfleet-integration-test:latest
    -f test/Dockerfile.integration
)

if [ "$CONTAINER_RUNTIME" = "podman" ]; then
    PROXY_HTTP=$(get_podman_proxy "HTTP_PROXY")
    PROXY_HTTPS=$(get_podman_proxy "HTTPS_PROXY")

    if [ -n "$PROXY_HTTP" ] || [ -n "$PROXY_HTTPS" ]; then
        echo "   Using proxy configuration"
        [ -n "$PROXY_HTTP" ] && BUILD_ARGS+=(--build-arg "HTTP_PROXY=$PROXY_HTTP")
        [ -n "$PROXY_HTTPS" ] && BUILD_ARGS+=(--build-arg "HTTPS_PROXY=$PROXY_HTTPS")
    fi
fi

$CONTAINER_RUNTIME build "${BUILD_ARGS[@]}" .

echo ""
echo "✅ Integration test image built successfully!"
echo "   Image: localhost/hyperfleet-integration-test:latest"
echo ""
