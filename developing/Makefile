# Variables
CLUSTER_NAME := tilt
KIND_CONTEXT := kind-$(CLUSTER_NAME)
REGISTRY_NAME := tilt-registry

# Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

# Tool Binaries
KUSTOMIZE ?= $(LOCALBIN)/kustomize
YQ ?= $(LOCALBIN)/yq

# Tool Versions
KUSTOMIZE_VERSION ?= v5.5.0
YQ_VERSION ?= v4.53.2

# Export KIND_EXPERIMENTAL_PROVIDER to honor it if set in user's environment
# (e.g., KIND_EXPERIMENTAL_PROVIDER=podman for podman support)
export KIND_EXPERIMENTAL_PROVIDER

# Export ENABLE_REGISTRY to honor it if set in user's environment
# (e.g., ENABLE_REGISTRY=true to use a local container registry)
export ENABLE_REGISTRY
export REGISTRY_NAME

# Install yq (only when ENABLE_REGISTRY=true)
.PHONY: yq
yq:
	@if [ "$$ENABLE_REGISTRY" = "true" ]; then \
		$(MAKE) $(YQ); \
	fi
$(YQ): $(LOCALBIN)
	$(call go-install-tool,$(YQ),github.com/mikefarah/yq/v4,$(YQ_VERSION))

# Check if tilt is installed
.PHONY: check-tilt
check-tilt:
	@if ! command -v tilt >/dev/null 2>&1; then \
		echo "ERROR: tilt is not installed. Please install tilt first:"; \
		echo "  curl -fsSL https://raw.githubusercontent.com/tilt-dev/tilt/master/scripts/install.sh | bash"; \
		echo "  or visit: https://docs.tilt.dev/install.html"; \
		exit 1; \
	fi

# Ensure kind cluster exists and context is set before running tilt
.PHONY: setup-kind
setup-kind: yq
	@echo "Setting up Kind cluster..."
	@if [ -n "$$KIND_EXPERIMENTAL_PROVIDER" ]; then \
		echo "Using KIND_EXPERIMENTAL_PROVIDER=$$KIND_EXPERIMENTAL_PROVIDER"; \
	fi
	@if [ "$$ENABLE_REGISTRY" = "true" ]; then \
		echo "Using local container registry (ENABLE_REGISTRY=true)"; \
	fi
	@YQ_BIN="$(YQ)" ./scripts/setup-kind.sh

# Install cert-manager (depends on kind cluster being set up)
.PHONY: setup-cert-manager
setup-cert-manager: setup-kind
	@echo "Setting up cert-manager..."
	@./scripts/setup-cert-manager.sh

# Install Istio and gateway resources (depends on cert-manager for TLS certificates)
.PHONY: setup-istio
setup-istio: setup-cert-manager
	@echo "Setting up Istio..."
	@./scripts/setup-istio.sh

# Run tilt up with kind cluster, cert-manager, and Istio setup
.PHONY: tilt-up
tilt-up: check-tilt setup-istio kustomize
	@echo "Starting Tilt..."
	@tilt up

# Stop Tilt
.PHONY: tilt-down
tilt-down: check-tilt kustomize
	@echo "Stopping Tilt..."
	@tilt down

# Tear down the local registry container (only when ENABLE_REGISTRY was used)
.PHONY: teardown-registry
teardown-registry:
	@echo "Removing local registry..."
	@if [ "$$KIND_EXPERIMENTAL_PROVIDER" = "podman" ]; then \
		podman rm -f $(REGISTRY_NAME) 2>/dev/null || true; \
	else \
		docker rm -f $(REGISTRY_NAME) 2>/dev/null || true; \
	fi

# Install kustomize
.PHONY: kustomize
kustomize: $(KUSTOMIZE)
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
# $4 - (optional) extra ldflags to set with the installation
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
ldflags=$(4) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${ldflags:+-ldflags "$${ldflags}"} $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef

# Remove downloaded tool binaries
.PHONY: clean
clean:
	rm -rf $(LOCALBIN)
	@echo "INFO: '$(LOCALBIN)' successfully cleaned."
