#!/usr/bin/env bash
# Container runtime detection and utility functions

set -e

# Detect available container runtime (docker or podman)
detect_container_runtime() {
    if docker info >/dev/null 2>&1; then
        echo "docker"
    elif podman info >/dev/null 2>&1; then
        echo "podman"
    else
        echo "none"
    fi
}

# Find Podman socket for testcontainers compatibility
find_podman_socket() {
    local sock
    sock=$(find /var/folders -name "podman-machine-*-api.sock" 2>/dev/null | head -1)
    if [ -z "$sock" ]; then
        sock=$(find ~/.local/share/containers/podman/machine -name "*.sock" 2>/dev/null | head -1)
    fi
    echo "$sock"
}

# Get proxy configuration from Podman machine
get_podman_proxy() {
    local proxy_type=$1
    podman machine ssh "echo \$$proxy_type" 2>/dev/null || echo ""
}

# Check if Podman is in rootful mode
is_podman_rootful() {
    local rootful
    rootful=$(podman machine inspect --format '{{.Rootful}}' 2>/dev/null || echo "unknown")
    echo "$rootful"
}

# Display container runtime information
display_container_info() {
    local runtime=$1
    local podman_sock=$2

    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "Container Runtime Information"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "Runtime detected: $runtime"

    if [ "$runtime" = "podman" ]; then
        echo "Podman socket: $podman_sock"
        if [ -n "$podman_sock" ]; then
            echo "DOCKER_HOST: unix://$podman_sock"
            echo "TESTCONTAINERS_RYUK_DISABLED: true"
        fi
    fi
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
}

# Detect the container platform (e.g., linux/arm64, linux/amd64)
# Maps host architecture to the corresponding container platform string.
detect_container_platform() {
    local arch
    arch=$(uname -m)
    case "$arch" in
        x86_64)  echo "linux/amd64" ;;
        aarch64) echo "linux/arm64" ;;
        arm64)   echo "linux/arm64" ;;
        *)       echo "linux/amd64" ;;
    esac
}

# Display error for missing container runtime
display_runtime_error() {
    echo "❌ ERROR: Neither Docker nor Podman is running"
    echo ""
    echo "Please start Docker or Podman:"
    echo "  Docker: Start Docker Desktop or run 'dockerd'"
    echo "  Podman: Run 'podman machine start'"
    echo ""
    exit 1
}

# Display K3s rootless warning
display_k3s_rootless_warning() {
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "⚠️  WARNING: Podman is in ROOTLESS mode"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    echo "K3s requires rootful Podman or proper cgroup v2 delegation for testcontainers."
    echo "Rootless Podman may fail with errors like:"
    echo "  • 'failed to find cpuset cgroup (v2)'"
    echo "  • 'container exited with code 1 or 255'"
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "✅ RECOMMENDED: Use pre-built envtest instead (works in all environments)"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    echo "  make test-integration"
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "🔧 ALTERNATIVE: Switch Podman to rootful mode"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    echo "  # Stop Podman machine and switch to rootful mode with adequate resources"
    echo "  podman machine stop"
    echo "  podman machine set --rootful=true --cpus 4 --memory 4096"
    echo "  podman machine start"
    echo ""
    echo "  # Verify it's rootful"
    echo "  podman machine inspect --format '{{.Rootful}}'  # Should output: true"
    echo ""
    echo "  # Then run K3s tests"
    echo "  make test-integration-k3s"
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
    echo "⚠️  Stopping here to prevent K3s failures. This is not a build error!"
    exit 1
}
