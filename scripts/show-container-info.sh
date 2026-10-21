#!/usr/bin/env bash
# Display container runtime information

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/container-runtime.sh"

CONTAINER_RUNTIME=$(detect_container_runtime)
PODMAN_SOCK=""

if [ "$CONTAINER_RUNTIME" = "podman" ]; then
    PODMAN_SOCK=$(find_podman_socket)
fi

display_container_info "$CONTAINER_RUNTIME" "$PODMAN_SOCK"
