#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MANIFESTS_DIR="${SCRIPT_DIR}/../manifests"
KUBECTL="${KUBECTL:-kubectl}"

echo "Setting up e2e test environment..."

echo "  Applying RBAC manifests..."
${KUBECTL} apply -f "${MANIFESTS_DIR}/e2e-rbac.yaml"

echo "  Applying seed data..."
${KUBECTL} apply -f "${MANIFESTS_DIR}/e2e-seed-data.yaml"

echo "  Waiting for seed WorkspaceKind to be accepted..."
${KUBECTL} wait --for=jsonpath='{.metadata.name}'=jupyterlab \
  workspacekind/jupyterlab --timeout=60s

echo "  Waiting for PVC to be created..."
${KUBECTL} wait --for=jsonpath='{.status.phase}'=Bound \
  pvc/home-volume -n e2e-test --timeout=60s || \
  echo "  (PVC may be Pending without a default StorageClass — this is OK for testing)"

echo "✓ E2E test environment ready"
