#!/usr/bin/env bash
# Setup script for local container registry
# Based on: https://kind.sigs.k8s.io/docs/user/local-registry/
#
# Usage:
#   setup-registry.sh start     - Create/start the registry container (and configure Podman if needed)
#   setup-registry.sh configure - Wire the registry to the Kind cluster (hosts.toml, network, ConfigMap)
#
# Required environment variables:
#   CLUSTER_NAME   - Name of the Kind cluster
#   REGISTRY_NAME  - Name of the registry container
#   REGISTRY_PORT  - Host port for the registry (default: 5001)

set -euo pipefail

REGISTRY_NAME="${REGISTRY_NAME:-tilt-registry}"
REGISTRY_PORT="${REGISTRY_PORT:-5001}"

# Detect container runtime (podman or docker)
if [ "${KIND_EXPERIMENTAL_PROVIDER:-}" = "podman" ]; then
  CONTAINER_CLI="podman"
else
  CONTAINER_CLI="docker"
fi

cmd_start() {
  # Configure Podman to allow insecure (HTTP) pushes to the local registry.
  # The Docker-compat API requires a service restart to pick up new registries.conf.
  if [ "${CONTAINER_CLI}" = "podman" ]; then
    REGISTRIES_CONF="/etc/containers/registries.conf.d/kubeflow-local-registry.conf"
    if ! podman machine ssh test -f "${REGISTRIES_CONF}" 2>/dev/null; then
      echo "Configuring Podman to allow insecure push to localhost:${REGISTRY_PORT}..."
      podman machine ssh "sudo tee ${REGISTRIES_CONF} >/dev/null" <<CONF
[[registry]]
location = "localhost:${REGISTRY_PORT}"
insecure = true
CONF
      echo "Restarting Podman API service to pick up registry config..."
      podman machine ssh "systemctl --user restart podman.service"
      sleep 2
    fi
  fi

  # Create or start the registry container
  if ${CONTAINER_CLI} inspect "${REGISTRY_NAME}" >/dev/null 2>&1; then
    echo "Registry container '${REGISTRY_NAME}' already exists"
    if [ "$(${CONTAINER_CLI} inspect -f '{{.State.Running}}' "${REGISTRY_NAME}" 2>/dev/null)" != "true" ]; then
      echo "Starting registry container..."
      ${CONTAINER_CLI} start "${REGISTRY_NAME}"
    fi
  else
    echo "Creating registry container '${REGISTRY_NAME}' on port ${REGISTRY_PORT}..."
    ${CONTAINER_CLI} run -d --restart=always \
      -p "127.0.0.1:${REGISTRY_PORT}:5000" \
      --name "${REGISTRY_NAME}" \
      docker.io/library/registry:2
  fi
}

cmd_configure() {
  local cluster_name="${CLUSTER_NAME:?CLUSTER_NAME must be set}"

  # Write per-node hosts.toml so containerd resolves localhost:<port> to the registry container
  REGISTRY_DIR="/etc/containerd/certs.d/localhost:${REGISTRY_PORT}"
  for node in $(kind get nodes --name "${cluster_name}"); do
    ${CONTAINER_CLI} exec "${node}" mkdir -p "${REGISTRY_DIR}"
    cat <<TOML | ${CONTAINER_CLI} exec -i "${node}" cp /dev/stdin "${REGISTRY_DIR}/hosts.toml"
[host."http://${REGISTRY_NAME}:5000"]
TOML
  done

  # Connect the registry container to the Kind network if not already connected
  if ! ${CONTAINER_CLI} network inspect kind 2>/dev/null | grep -q "${REGISTRY_NAME}"; then
    echo "Connecting registry to Kind network..."
    ${CONTAINER_CLI} network connect kind "${REGISTRY_NAME}" 2>/dev/null || true
  fi

  # Create the KEP-1755 local-registry-hosting ConfigMap
  # This tells Tilt (and other tools) where the local registry is.
  echo "Creating local-registry-hosting ConfigMap..."
  kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-registry-hosting
  namespace: kube-public
data:
  localRegistryHosting.v1: |
    host: "localhost:${REGISTRY_PORT}"
    help: "https://kind.sigs.k8s.io/docs/user/local-registry/"
EOF
}

case "${1:-}" in
  start)     cmd_start ;;
  configure) cmd_configure ;;
  *)
    echo "Usage: $0 {start|configure}" >&2
    exit 1
    ;;
esac
