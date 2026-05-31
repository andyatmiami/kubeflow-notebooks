#!/usr/bin/env bash
# Setup script for Kind cluster
# This script checks if a Kind cluster exists and creates it if needed

set -euo pipefail

CLUSTER_NAME="tilt"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEVELOPING_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
KIND_CONFIG="${DEVELOPING_DIR}/kind-1-35.yaml"

ENABLE_REGISTRY="${ENABLE_REGISTRY:-false}"
REGISTRY_PORT="${REGISTRY_PORT:-5001}"
YQ_BIN="${YQ_BIN:-yq}"

# Check if kind command exists
if ! command -v kind >/dev/null 2>&1; then
  echo "ERROR: kind is not installed. Please install kind first:"
  echo "  brew install kind  # macOS"
  echo "  or visit: https://kind.sigs.k8s.io/docs/user/quick-start/#installation"
  exit 1
fi

# Start the local registry container before cluster creation
if [ "${ENABLE_REGISTRY}" = "true" ]; then
  "${SCRIPT_DIR}/setup-registry.sh" start
fi

# Create the Kind cluster if it doesn't exist
if ! kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
  if [ "${ENABLE_REGISTRY}" = "true" ]; then
    # Merge a containerd registry config patch into the base kind.yaml.
    # This enables containerd's config_path so per-node hosts.toml files
    # (written by setup-registry.sh) are picked up.
    KIND_CONFIG_WITH_REGISTRY=$(mktemp)
    trap 'rm -f "${KIND_CONFIG_WITH_REGISTRY}"' EXIT

    CONTAINERD_PATCH='[plugins."io.containerd.grpc.v1.cri".registry]
  config_path = "/etc/containerd/certs.d"'

    export CONTAINERD_PATCH
    "${YQ_BIN}" eval-all \
      '. as $doc | $doc | .containerdConfigPatches = ($doc.containerdConfigPatches // []) + [strenv(CONTAINERD_PATCH)]' \
      "${KIND_CONFIG}" > "${KIND_CONFIG_WITH_REGISTRY}"

    echo "Creating Kind cluster '${CLUSTER_NAME}' with local registry..."
    kind create cluster --name "${CLUSTER_NAME}" --config "${KIND_CONFIG_WITH_REGISTRY}" --wait 120s
  else
    echo "Creating Kind cluster '${CLUSTER_NAME}' with config from ${KIND_CONFIG}..."
    kind create cluster --name "${CLUSTER_NAME}" --config "${KIND_CONFIG}" --wait 120s
  fi
  echo "Kind cluster created successfully"
else
  echo "Kind cluster '${CLUSTER_NAME}' already exists"
fi

# Wire the registry to the Kind cluster
if [ "${ENABLE_REGISTRY}" = "true" ]; then
  CLUSTER_NAME="${CLUSTER_NAME}" "${SCRIPT_DIR}/setup-registry.sh" configure
fi

# Ensure kubectl context is set to the Kind cluster
kubectl config use-context "kind-${CLUSTER_NAME}" || {
  echo "ERROR: Failed to set kubectl context to kind-${CLUSTER_NAME}"
  exit 1
}

# Configure StorageClasses with Notebooks labels and annotations
echo "Configuring StorageClasses for the Notebooks UI..."

# Label and annotate the default 'standard' StorageClass
kubectl label storageclass standard \
  "notebooks.kubeflow.org/can-use=true" \
  --overwrite
kubectl annotate storageclass standard \
  "notebooks.kubeflow.org/display-name=Standard (Local Path)" \
  "notebooks.kubeflow.org/description=Local path provisioner for development. Data is stored on the node and not replicated." \
  --overwrite

# Create an additional 'premium-local' StorageClass (same provisioner, but not usable, for testing)
kubectl apply -f - <<EOF
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: premium-local
  labels:
    notebooks.kubeflow.org/can-use: "false"
  annotations:
    notebooks.kubeflow.org/display-name: "Premium Local (Local Path)"
    notebooks.kubeflow.org/description: "Simulated premium storage for development. Not enabled for use."
provisioner: rancher.io/local-path
reclaimPolicy: Delete
volumeBindingMode: WaitForFirstConsumer
EOF

echo "Kind cluster setup complete"
