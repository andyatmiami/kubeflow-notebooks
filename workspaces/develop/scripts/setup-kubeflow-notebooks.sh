#!/bin/bash
#
# kubeflow-notebooks setup script
# Based on: https://github.com/andyatmiami/kubeflow-notebooks/wiki/kubeflow-notebooks-within-Central-Dashboard
#
# This script automates the setup of kubeflow notebooks within Central Dashboard
# for local development using kind cluster.

set -euo pipefail

# Colors for output
readonly RED='\033[0;31m'
readonly GREEN='\033[0;32m'
readonly YELLOW='\033[0;33m'
readonly ORANGE='\033[38;5;208m'
readonly BLUE='\033[0;34m'
readonly NC='\033[0m' # No Color

# Script variables
readonly cluster_name="kubeflow"
readonly script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly project_root="$(cd "${script_dir}/../../.." && pwd)"

# Remote manifest repository
readonly remote_manifest_repo="https://github.com/kubeflow/manifests"

# Default values for image customization
default_image_registry="localhost"
default_tag_suffix="e2e-istio"

# Image customization variables (can be overridden by command line arguments)
image_registry="$default_image_registry"
tag_suffix="$default_tag_suffix"

# Force mode flag (can be overridden by command line arguments)
force_mode=false

# Helper function to run commands with indented output
indent_output() {
    local indent="    "  # 4 spaces for indentation
    local cmd="$*"
    local exit_code

    # Run the command and pipe output through sed to prepend indentation
    if eval "$cmd" 2>&1 | sed "s/^/${indent}/"; then
        exit_code=0
    else
        exit_code=${PIPESTATUS[0]}
    fi

    return $exit_code
}

# Logging functions
log_info() {
    echo -e "${BLUE}[INFO]${NC} $*"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $*"
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $*"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $*"
}

log_prompt() {
    echo -e "${ORANGE}[PROMPT]${NC} $*"
}

# Check if command exists
command_exists() {
    command -v "$1" >/dev/null 2>&1
}

# Check ulimit settings
check_ulimit() {
    log_info "Checking ulimit settings..."

    local soft_limit
    local hard_limit
    local soft_limit_num
    local hard_limit_num
    local issues_found=false

    # Get soft limit
    soft_limit=$(ulimit -Sn 2>/dev/null || echo "unknown")
    # Get hard limit
    hard_limit=$(ulimit -Hn 2>/dev/null || echo "unknown")

    if [ "$soft_limit" = "unknown" ] || [ "$hard_limit" = "unknown" ]; then
        log_warning "Could not determine ulimit values - this may indicate a system configuration issue"
        issues_found=true
    else
        log_info "Current ulimit settings:"
        log_info "  Soft limit: $soft_limit"
        log_info "  Hard limit: $hard_limit"

        # Check soft limit (should be at least 4096)
        if [ "$soft_limit" != "unlimited" ] && [ "$soft_limit" -lt 4096 ]; then
            log_warning "Soft limit ($soft_limit) is below recommended minimum of 4096"
            log_warning "  - Increase soft limit: ulimit -Sn 4096"
            issues_found=true
        fi

        # Check hard limit (should be at least 65535)
        if [ "$hard_limit" != "unlimited" ] && [ "$hard_limit" -lt 65535 ]; then
            log_warning "Hard limit ($hard_limit) is below recommended minimum of 65535"
            log_warning "  - Increase hard limit: ulimit -Hn 65535"
            issues_found=true
        fi
    fi



    if [ "$issues_found" = true ]; then
        log_warning "The currently defined limits may cause 'too many open files' errors during kubeflow deployment"

        if [ "$force_mode" = true ]; then
            log_info "Force mode enabled, continuing with current ulimit settings"
        else
            log_prompt "Do you want to continue with the current ulimit settings? (y/N): "
            read -r
            if [[ ! $REPLY =~ ^[Yy]$ ]]; then
                log_info "Aborting setup due to insufficient ulimit settings"
                exit 1
            fi
            log_info "Continuing with current ulimit settings"
        fi
    else
        log_success "ulimit settings are sufficient for kubeflow deployment"
    fi
}

# Verify required applications
verify_applications() {
    log_info "Verifying required applications..."

    local missing_apps=()

    # Check Go
    if ! command_exists go; then
        missing_apps+=("go")
    else
        local go_version
        go_version=$(go version)
        log_success "Go found: $go_version"
    fi

    # Check Node.js
    if ! command_exists node; then
        missing_apps+=("node")
    else
        local node_version
        node_version=$(node --version)
        log_success "Node.js found: $node_version"
    fi

    # Check kubectl
    if ! command_exists kubectl; then
        missing_apps+=("kubectl")
    else
        local kubectl_version
        kubectl_version=$(kubectl version --client | head -n1)
        log_success "kubectl found: $kubectl_version"
    fi

    if [ -n "${CONTAINER_ENGINE:-}" ]; then
        log_info "CONTAINER_ENGINE is set to: $CONTAINER_ENGINE"
        if [ "$CONTAINER_ENGINE" = "podman" ]; then
            if command_exists podman; then
                log_success "Podman found: $(podman version | head -n1)"
            else
                log_error "CONTAINER_ENGINE is set to 'podman' but podman is not available"
                missing_apps+=("podman")
            fi
        elif [ "$CONTAINER_ENGINE" = "docker" ]; then
            if command_exists docker; then
                log_success "Docker found: $(docker version | head -n1)"
            else
                log_error "CONTAINER_ENGINE is set to 'docker' but docker is not available"
                missing_apps+=("docker")
            fi
        else
            log_error "CONTAINER_ENGINE is set to '$CONTAINER_ENGINE' but only 'podman' or 'docker' are supported"
            missing_apps+=("valid container engine")
        fi
    else
        log_info "CONTAINER_ENGINE not set, detecting available container engine..."
        if command_exists podman; then
            local podman_version
            podman_version=$(podman version | head -n1)
            log_success "Podman found: $podman_version"
            readonly CONTAINER_ENGINE="podman"
        elif command_exists docker; then
            local docker_version
            docker_version=$(docker version | head -n1)
            log_success "Docker found: $docker_version"
            readonly CONTAINER_ENGINE="docker"
        else
            missing_apps+=("podman or docker")
        fi
    fi

    # Check kind
    if ! command_exists kind; then
        missing_apps+=("kind")
    else
        local kind_version
        kind_version=$(kind version)
        log_success "kind found: $kind_version"
    fi

    # Check make (GNU make preferred on macOS)
    if command_exists gmake; then
        local gmake_version
        gmake_version=$(gmake --version | head -n1)
        log_success "GNU make found: $gmake_version"
        readonly MAKE_CMD="gmake"
    elif command_exists make; then
        local make_version
        make_version=$(make --version | head -n1)
        log_success "make found: $make_version"
        readonly MAKE_CMD="make"
    else
        missing_apps+=("make")
    fi

    # Check kustomize
    if ! command_exists kustomize; then
        missing_apps+=("kustomize")
    else
        local kustomize_version
        kustomize_version=$(kustomize version | head -n1)
        log_success "kustomize found: $kustomize_version"
    fi

    # Check yq (needed for dex configmap modification)
    if ! command_exists yq; then
        missing_apps+=("yq")
    else
        local yq_version
        yq_version=$(yq --version)
        log_success "yq found: $yq_version"
    fi

    # Check jq (needed for centraldashboard configmap modification)
    if ! command_exists jq; then
        missing_apps+=("jq")
    else
        local jq_version
        jq_version=$(jq --version)
        log_success "jq found: $jq_version"
    fi

    if [ ${#missing_apps[@]} -ne 0 ]; then
        log_error "Missing required applications: ${missing_apps[*]}"
        log_info "Please install the missing applications and run this script again."
        exit 1
    fi
}

# Install kind if not present
install_kind() {
    if ! command_exists kind; then
        log_info "Installing kind..."
        indent_output go install sigs.k8s.io/kind@v0.27.0
        export PATH="$PATH:$(go env GOPATH)/bin"
        log_success "kind installed successfully"
    fi
}

# Install kustomize if not present
install_kustomize() {
    if ! command_exists kustomize; then
        log_info "Installing kustomize..."
        indent_output go install sigs.k8s.io/kustomize/kustomize/v5@latest
        export PATH="$PATH:$(go env GOPATH)/bin"
        log_success "kustomize installed successfully"
    fi
}

# Install yq if not present
install_yq() {
    if ! command_exists yq; then
        log_info "Installing yq..."
        indent_output go install github.com/mikefarah/yq/v4@latest
        export PATH="$PATH:$(go env GOPATH)/bin"
        log_success "yq installed successfully"
    fi
}

# Install jq if not present
install_jq() {
    if ! command_exists jq; then
        log_info "Installing jq..."
        if command_exists brew; then
            indent_output brew install jq
        elif command_exists apt-get; then
            indent_output sudo apt-get update && sudo apt-get install -y jq
        elif command_exists yum; then
            indent_output sudo yum install -y jq
        else
            log_error "jq is required but no package manager found. Please install jq manually."
            exit 1
        fi
        log_success "jq installed successfully"
    fi
}

# Clean up existing clusters
cleanup_existing_clusters() {
    log_info "Checking for existing kind clusters..."

    local existing_clusters
    existing_clusters=$(KIND_EXPERIMENTAL_PROVIDER="$CONTAINER_ENGINE" kind get clusters 2>/dev/null || true)

    if [ -n "$existing_clusters" ]; then
        log_warning "Found existing clusters:"
        indent_output echo "$existing_clusters"

        if [ "$force_mode" = true ]; then
            log_info "Force mode enabled, deleting existing clusters..."
            echo "$existing_clusters" | while read -r cluster; do
                if [ -n "$cluster" ]; then
                    log_info "Deleting cluster: $cluster"
                    indent_output KIND_EXPERIMENTAL_PROVIDER="$CONTAINER_ENGINE" kind delete cluster --name "$cluster"
                fi
            done
            log_success "All existing clusters deleted"
        else
            log_prompt "Do you want to delete existing clusters? (y/N): "
            read -r
            if [[ $REPLY =~ ^[Yy]$ ]]; then
                log_info "Deleting existing clusters..."
                echo "$existing_clusters" | while read -r cluster; do
                    if [ -n "$cluster" ]; then
                        log_info "Deleting cluster: $cluster"
                        indent_output KIND_EXPERIMENTAL_PROVIDER="$CONTAINER_ENGINE" kind delete cluster --name "$cluster"
                    fi
                done
                log_success "All existing clusters deleted"
            else
                log_info "Skipping cluster deletion"
            fi
        fi
    else
        log_info "No existing clusters found"
    fi
}

# Create kind cluster
create_kind_cluster() {
    log_info "Creating kind cluster: $cluster_name"

    indent_output KIND_EXPERIMENTAL_PROVIDER="$CONTAINER_ENGINE" kind create cluster --name="$cluster_name" --config=<(cat <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  image: kindest/node:v1.32.0@sha256:c48c62eac5da28cdadcf560d1d8616cfa6783b58f0d94cf63ad1bf49600cb027
  kubeadmConfigPatches:
  - |
    kind: ClusterConfiguration
    apiServer:
      extraArgs:
        "service-account-issuer": "https://kubernetes.default.svc"
        "service-account-signing-key-file": "/etc/kubernetes/pki/sa.key"
EOF
)

    log_success "Kind cluster created successfully"
}

# Check if container registry credentials are available for a given registry
# This approach tests the actual login capability rather than parsing config files
check_container_registry_credentials() {
    local registry="$1"
    local podman_auth_file=""
    local docker_config_file=""

    # Generate list of possible registry identifiers to check
    local registry_variants=("$registry")

    # Add Docker Hub variants if this is a Docker Hub registry
    if [[ "$registry" =~ ^(docker\.io|index\.docker\.io|https://index\.docker\.io/v1/?)$ ]]; then
        registry_variants+=("docker.io" "index.docker.io" "https://index.docker.io/v1/")
    fi

    # For Docker, only check Docker config
    if [ "$CONTAINER_ENGINE" = "docker" ]; then
        docker_config_file="$HOME/.docker/config.json"
        if [ -f "$docker_config_file" ]; then
            for variant in "${registry_variants[@]}"; do
                if jq -e --arg registry "$variant" '.auths | has($registry)' "$docker_config_file" >/dev/null 2>&1; then
                    log_info "Container registry credentials found for registry: $registry (matched variant: $variant)"
                    return 0
                fi
            done
        fi
        log_info "No container registry credentials found for registry: $registry"
        return 1
    fi

    # For Podman, check both Podman auth file and Docker config (fallback)
    if [ "$CONTAINER_ENGINE" = "podman" ]; then
        podman_auth_file="${XDG_RUNTIME_DIR:-$HOME/.local/share/containers}/auth.json"
        docker_config_file="$HOME/.docker/config.json"

        # Check Podman auth file first
        if [ -f "$podman_auth_file" ]; then
            for variant in "${registry_variants[@]}"; do
                if jq -e --arg registry "$variant" '.auths | has($registry)' "$podman_auth_file" >/dev/null 2>&1; then
                    log_info "Container registry credentials found for registry: $registry (matched variant: $variant)"
                    return 0
                fi
            done
        fi

        # Fall back to Docker config file
        if [ -f "$docker_config_file" ]; then
            for variant in "${registry_variants[@]}"; do
                if jq -e --arg registry "$variant" '.auths | has($registry)' "$docker_config_file" >/dev/null 2>&1; then
                    log_info "Container registry credentials found for registry: $registry (matched variant: $variant)"
                    return 0
                fi
            done
        fi

        log_info "No container registry credentials found for registry: $registry"
        return 1
    fi
}

# Prompt user for container registry credentials
prompt_container_registry_credentials() {
    local registry="$1"
    local username=""
    local password=""

    log_info "Collecting container registry credentials for registry: $registry"
    log_info "Leave username and password empty for anonymous login"

    # Prompt for username
    log_prompt "Username (empty for anonymous): "
    read -r username

    # Prompt for password (hidden input)
    log_prompt "Password (empty for anonymous): "
    # Use stty to hide password input
    stty -echo
    read -r password
    stty echo
    printf "\n"

    # Store credentials in variables (they will be passed to the login function)
    # Empty values are allowed for anonymous logins
    CONTAINER_REGISTRY_USERNAME="$username"
    CONTAINER_REGISTRY_PASSWORD="$password"
}

# Perform container registry login with provided credentials
perform_container_registry_login() {
    local registry="$1"
    local username="$2"
    local password="$3"

    log_info "Logging into container registry: $registry"

    # Check if this is an anonymous login (empty username and password)
    if [ -z "$username" ] && [ -z "$password" ]; then
        log_info "Skipping login for anonymous access to registry: $registry"
        log_info "Anonymous access will be attempted when pulling/pushing images"
        # For anonymous access, we don't need to login - just return success
        # The container engine will attempt anonymous access when needed
        log_success "Anonymous access configured for registry: $registry"
        return 0
    else
        # For authenticated login, use --username and --password-stdin
        if echo "$password" | "$CONTAINER_ENGINE" login --username "$username" --password-stdin "$registry" 2>&1; then
            log_success "Successfully logged into container registry: $registry"
            return 0
        else
            log_error "Failed to login to container registry: $registry"
            return 1
        fi
    fi
}

# Login to container registry with credential checking and user input
login_container_registry() {
    local registry="${1:-docker.io}"
    local username=""
    local password=""

    log_info "Checking container registry credentials for registry: $registry"

    # Check if credentials already exist
    if check_container_registry_credentials "$registry"; then
        log_success "Valid container registry credentials found for registry: $registry"
        return 0
    fi

    # No valid credentials found, prompt user
    log_warning "No valid container registry credentials found for registry: $registry"

    if [ "$force_mode" = true ]; then
        log_error "Force mode enabled but no container registry credentials available for registry: $registry"
        log_error "Please run '$CONTAINER_ENGINE login $registry' manually or provide credentials interactively"
        return 1
    fi

    # Prompt for credentials
    prompt_container_registry_credentials "$registry"
    username="$CONTAINER_REGISTRY_USERNAME"
    password="$CONTAINER_REGISTRY_PASSWORD"

    # Perform login with provided credentials
    if perform_container_registry_login "$registry" "$username" "$password"; then
        log_success "Container registry login completed successfully"
        return 0
    else
        log_error "Container registry login failed"
        return 1
    fi
}

# Install cert-manager
install_cert_manager() {
    log_info "Installing cert-manager..."
    indent_output kubectl apply -k "${remote_manifest_repo}/common/cert-manager/base"

    # Give cert-manager time to create resources
    log_info "Waiting for cert-manager resources to be created..."
    sleep 30

    log_info "Waiting for cert-manager to be ready..."
    indent_output kubectl wait --for=condition=Ready pod -l app.kubernetes.io/instance=cert-manager --timeout=180s -n cert-manager

    # Wait for cert-manager webhook to be ready before applying kubeflow-issuer
    log_info "Waiting for cert-manager webhook to be ready..."
    indent_output kubectl wait --for=condition=Available deployment -l app.kubernetes.io/instance=cert-manager --timeout=180s -n cert-manager

    # Now apply kubeflow-issuer after webhook is ready
    log_info "Installing kubeflow-issuer..."
    indent_output kubectl apply -k "${remote_manifest_repo}/common/cert-manager/kubeflow-issuer/base"

    log_success "Cert-manager installed and configured"
}

# Install Istio
install_istio() {
    # Create kubeflow namespace (istio requires it to be created first now)
    log_info "Creating kubeflow namespace..."
    indent_output kubectl apply -k "${remote_manifest_repo}/common/kubeflow-namespace/base"

    log_info "Installing Istio..."
    indent_output kubectl apply -k "${remote_manifest_repo}/common/istio/istio-crds/base"
    indent_output kubectl apply -k "${remote_manifest_repo}/common/istio/istio-namespace/base"

    # For most platforms (Kind, Minikube, AKS, EKS, etc.)
    indent_output kubectl apply -k "${remote_manifest_repo}/common/istio/istio-install/overlays/oauth2-proxy"

    log_info "Waiting for all Istio Pods to become ready..."
    indent_output kubectl wait --for=condition=Ready pods --all -n istio-system --timeout 300s

    log_success "Istio installed and ready"
}

# Install OAuth2 proxy
install_oauth2_proxy() {
    log_info "Installing oauth2-proxy..."
    # Using m2m-dex-only overlay for most clusters
    indent_output kubectl apply -k "${remote_manifest_repo}/common/oauth2-proxy/overlays/m2m-dex-only/"
    indent_output kubectl wait --for=condition=Ready pod -l 'app.kubernetes.io/name=oauth2-proxy' --timeout=180s -n oauth2-proxy

    log_success "OAuth2 proxy installed and ready"
}

# Install and configure Dex
install_dex() {
    log_info "Installing dex..."
    indent_output kubectl apply -k "${remote_manifest_repo}/common/dex/overlays/oauth2-proxy"
    indent_output kubectl wait --for=condition=Ready pods --all --timeout=180s -n auth

    # Modify dex configmap to enable password grant type for local testing
    log_info "Modifying dex configmap to enable password grant type..."
    indent_output kubectl get configmap dex -n auth -o yaml | \
    yq eval '
        (.data["config.yaml"] | from_yaml) as $cfg |
        (
            $cfg.staticClients[0].redirectURIs += [
            "http://localhost:8080/oauth2/callback",
            "http://kubeflow.local:8080/oauth2/callback",
            "http://[::1]:8080/oauth2/callback"
            ] |
            $cfg.staticClients[0].redirectURIs = ($cfg.staticClients[0].redirectURIs | unique) |
            $cfg.oauth2.passwordConnector = "local"
        ) |
        .data["config.yaml"] = ($cfg | to_yaml)
    ' - | kubectl apply -f -

    # Restart dex deployment to apply config changes
    log_info "Restarting dex deployment to apply config changes..."
    indent_output kubectl rollout restart deployment dex -n auth
    indent_output kubectl wait --for=condition=Ready pods --all --timeout=180s -n auth

    log_success "Dex installed and configured"
}

# Install Kubeflow base components
install_kubeflow_base() {
    log_info "Installing Kubeflow base components..."

    # Install network policies
    log_info "Installing network policies..."
    indent_output kubectl apply -k "${remote_manifest_repo}/common/networkpolicies/base"

    # Install kubeflow roles
    log_info "Installing kubeflow roles..."
    indent_output kubectl apply -k "${remote_manifest_repo}/common/kubeflow-roles/base"

    # Install kubeflow istio resources
    log_info "Installing kubeflow istio resources..."
    indent_output kubectl apply -k "${remote_manifest_repo}/common/istio/kubeflow-istio-resources/base"

    log_success "Kubeflow base components installed"
}

# Install and configure Central Dashboard
install_central_dashboard() {
    log_info "Installing centraldashboard..."
    indent_output kubectl apply -k "${remote_manifest_repo}/applications/centraldashboard/overlays/oauth2-proxy"

    # Wait for centraldashboard to be ready
    log_info "Waiting for centraldashboard to be ready..."
    indent_output kubectl wait --for=condition=ready pod -l app=centraldashboard -n kubeflow --timeout=300s

    # Wait for Istio to process the centraldashboard VirtualService
    log_info "Waiting for Istio to process centraldashboard VirtualService..."
    sleep 10

    # Modify centraldashboard configmap to add Notebooks v2 menu items
    log_info "Modifying centraldashboard configmap to add Notebooks v2 menu items..."
    indent_output kubectl get configmap centraldashboard-config -n kubeflow -o json | \
    jq '.data.links |= (
      fromjson |
      .menuLinks += [{
        "icon": "book",
        "items": [
          {
            "text": "Workspaces",
            "link": "/workspaces/workspaces"
          },
          {
            "text": "Workspace Kinds",
            "link": "/workspaces/workspacekinds"
          }
        ],
        "text": "Notebooks v2",
        "type": "section"
      }] |
      tojson
    )' | \
    kubectl apply -f -

    # Restart centraldashboard deployment to apply config changes
    log_info "Restarting centraldashboard deployment to apply config changes..."
    indent_output kubectl rollout restart -n kubeflow deployment.app/centraldashboard

    log_success "Central Dashboard installed and configured"
}

# Install Profile Controller and create default profile
install_profile_controller() {
    log_info "Installing profile-controller + kfam..."
    indent_output kubectl apply -k "${remote_manifest_repo}/applications/profiles/upstream/overlays/kubeflow"

    # Wait for profile-controller to be ready
    log_info "Waiting for profile-controller to be ready..."
    indent_output kubectl wait --for=condition=ready pod -l kustomize.component=profiles -n kubeflow --timeout=300s

    # Create Profile manifest
    log_info "Creating Profile manifest..."
    indent_output cat <<EOF | kubectl apply -f -
apiVersion: kubeflow.org/v1beta1
kind: Profile
metadata:
  name: kubeflow-default-profile
spec:
  owner:
    kind: User
    name: user@example.com
  plugins: []
  resourceQuotaSpec: {}
EOF

    # Wait for the kubeflow-default-profile namespace to be created
    log_info "Waiting for kubeflow-default-profile namespace to be created..."
    indent_output kubectl wait --for=jsonpath='{.status.phase}'=Active namespace kubeflow-default-profile --timeout=180s

    log_success "Profile Controller installed and default profile created"
}

# Install PodDefaults
install_poddefaults() {
    log_info "Installing PodDefaults..."
    indent_output kubectl apply -k "${remote_manifest_repo}/applications/admission-webhook/upstream/overlays/cert-manager"

    log_info "Waiting for PodDefaults to be ready..."
    indent_output kubectl wait --for=condition=ready pod -l app=poddefaults -n kubeflow --timeout=300s

    log_success "PodDefaults installed and ready"
}

# Install Notebooks v1
install_legacy_notebooks() {
    log_info "Installing Notebooks v1..."
    indent_output kubectl apply -k "${remote_manifest_repo}/applications/jupyter/notebook-controller/upstream/overlays/kubeflow"

    log_info "Waiting for Notebooks v1 to be ready..."
    indent_output kubectl wait --for=condition=ready pod -l app=notebook-controller -n kubeflow --timeout=300s

    log_success "Notebooks v1 installed and ready"
}

# Install core Kubeflow dependencies
install_kubeflow_dependencies() {
    log_info "Installing core Kubeflow dependencies..."

    install_cert_manager
    install_istio
    install_oauth2_proxy
    install_dex
    install_kubeflow_base
    install_central_dashboard
    install_poddefaults
    install_legacy_notebooks
    install_profile_controller

    log_success "Core Kubeflow dependencies installed"
}

# Generate content-based hash for a directory
generate_content_hash() {
    local dir="$1"
    local exclude_dirs="${2:-}"
    local hash
    local find_cmd

    # Check if directory exists
    if [ ! -d "$dir" ]; then
        log_error "Directory does not exist: $dir"
        return 1
    fi

    # Build find command with exclusions if provided
    find_cmd="find . -type f"
    if [ -n "$exclude_dirs" ]; then
        # Convert comma-separated list to find exclusions
        IFS=',' read -ra exclude_array <<< "$exclude_dirs"
        for exclude_dir in "${exclude_array[@]}"; do
            # Trim whitespace
            exclude_dir=$(echo "$exclude_dir" | xargs)
            if [ -n "$exclude_dir" ]; then
                find_cmd="$find_cmd -not -path \"./$exclude_dir/*\""
            fi
        done
    fi

    # Generate hash based on all file contents in the directory (excluding specified dirs)
    hash=$(cd "$dir" && eval "$find_cmd" -exec sha256sum {} \; 2>/dev/null | sort -k 2 | sha256sum | awk '{print $1}' | head -c 8)

    printf "%s" "$hash"
}

# Generate image tag with content hash
generate_image_tag() {
    local dir="$1"
    local exclude_dirs="${2:-}"
    local component
    local content_hash

    # Extract component name from directory path (e.g., "workspaces/controller" -> "controller")
    component=$(basename "$dir")
    content_hash=$(generate_content_hash "$dir" "$exclude_dirs")
    echo "${image_registry:+$image_registry/}kubeflow-notebooks-v2:${component}-${tag_suffix}-${content_hash}"
}

# Check if image exists in local registry
image_exists_locally() {
    local img="$1"
    local exists=false

    if [ "$CONTAINER_ENGINE" = "podman" ]; then
        if podman image exists "$img" 2>/dev/null; then
            exists=true
        fi
    else
        if docker image inspect "$img" >/dev/null 2>&1; then
            exists=true
        fi
    fi

    if [ "$exists" = true ]; then
        log_info "Image exists locally: $img"
        return 0
    else
        log_info "Image not found locally: $img"
        return 1
    fi
}

# Retry mechanism with exponential backoff for network operations
retry_with_backoff() {
    local max_attempts=3
    local attempt=1
    local sleep_times=(10 30 60)  # Sleep times in seconds for each retry
    local cmd="$*"
    local exit_code

    while [ $attempt -le $max_attempts ]; do
        log_info "Attempt $attempt of $max_attempts: $cmd"

        # Use existing indent_output function
        if indent_output "$cmd"; then
            log_success "Command succeeded on attempt $attempt"
            return 0
        else
            exit_code=$?
            if [ $attempt -eq $max_attempts ]; then
                log_error "Command failed after $max_attempts attempts with exit code $exit_code"
                return $exit_code
            else
                local sleep_time=${sleep_times[$((attempt-1))]}
                log_warning "Command failed on attempt $attempt (exit code: $exit_code), retrying in ${sleep_time}s..."
                sleep $sleep_time
            fi
        fi

        attempt=$((attempt + 1))
    done
}

# Load image into kind cluster (handles both Docker and Podman)
load_image_into_kind() {
    local img="$1"
    local temp_file=""

    log_info "Loading image into kind cluster: $img"

    if [ "$CONTAINER_ENGINE" = "podman" ]; then
        # For Podman, we need to save the image to a tar file first
        temp_file=$(mktemp)
        log_info "Saving Podman image to temporary file: $temp_file"

        if indent_output podman save --format docker-archive -o "$temp_file" "$img"; then
            indent_output KIND_EXPERIMENTAL_PROVIDER=podman kind load image-archive "$temp_file" --name "$cluster_name"
            rm -f "$temp_file"
            log_success "Image loaded into kind cluster via Podman"
        else
            log_error "Failed to save Podman image: $img"
            rm -f "$temp_file"
            return 1
        fi
    else
        # For Docker, use the standard approach
        indent_output KIND_EXPERIMENTAL_PROVIDER="$CONTAINER_ENGINE" kind load docker-image "$img" --name "$cluster_name"
        log_success "Image loaded into kind cluster via Docker"
    fi
}

# Build and deploy controller component
build_and_deploy_controller() {
    log_info "Building controller..."
    local img
    img=$(generate_image_tag "workspaces/controller")
    log_info "Generated image tag: $img"

    pushd workspaces/controller >/dev/null

    # Check if image already exists
    local image_exists
    if image_exists_locally "$img"; then
        image_exists=true
    else
        image_exists=false
    fi

    if [ "$force_mode" = true ] || [ "$image_exists" = false ]; then
        if [ "$force_mode" = true ]; then
            log_info "Force mode enabled, rebuilding controller image without cache"
        else
            log_info "Building controller image..."
        fi

        # Set no-cache flag for force mode
        local build_args=""
        if [ "$force_mode" = true ]; then
            build_args="DOCKER_BUILDKIT=1 CONTAINER_BUILD_ARGS=--no-cache"
        fi

        # Use retry mechanism for docker build
        retry_with_backoff "IMG=\"$img\" CONTAINER_TOOL=\"$CONTAINER_ENGINE\" $build_args $MAKE_CMD docker-build"
    else
        log_info "Controller image already exists locally, skipping build"
    fi

    # Load image into kind cluster
    load_image_into_kind "$img"

    # Install controller
    log_info "Installing controller..."
    indent_output $MAKE_CMD install

    # Deploy controller
    log_info "Deploying controller..."
    indent_output IMG="$img" $MAKE_CMD deploy

    popd >/dev/null

    # Restart deployment to ensure fresh image is used
    log_info "Restarting controller deployment..."
    indent_output kubectl rollout restart deployment workspace-controller-controller-manager -n workspace-controller-system

    # Validate controller deployment
    log_info "Validating controller deployment..."
    indent_output kubectl wait --for=condition=ready pod -l control-plane=controller-manager -n workspace-controller-system --timeout=300s
    log_success "Controller built, deployed, and validated"
}

# Build and deploy backend component
build_and_deploy_backend() {
    log_info "Building backend..."
    local img
    img=$(generate_image_tag "workspaces/backend")
    log_info "Generated image tag: $img"

    pushd workspaces/backend >/dev/null

    # Check if image already exists
    local image_exists
    if image_exists_locally "$img"; then
        image_exists=true
    else
        image_exists=false
    fi

    if [ "$force_mode" = true ] || [ "$image_exists" = false ]; then
        if [ "$force_mode" = true ]; then
            log_info "Force mode enabled, rebuilding backend image without cache"
        else
            log_info "Building backend image..."
        fi

        # Set no-cache flag for force mode
        local build_args=""
        if [ "$force_mode" = true ]; then
            build_args="DOCKER_BUILDKIT=1 CONTAINER_BUILD_ARGS=--no-cache"
        fi

        # Use retry mechanism for docker build
        retry_with_backoff "IMG=\"$img\" CONTAINER_TOOL=\"$CONTAINER_ENGINE\" $build_args $MAKE_CMD docker-build"
    else
        log_info "Backend image already exists locally, skipping build"
    fi

    # Load image into kind cluster
    load_image_into_kind "$img"

    # Deploy backend
    log_info "Deploying backend..."
    indent_output IMG="$img" $MAKE_CMD deploy

    popd >/dev/null

    # Wait for Istio to process the backend VirtualService
    log_info "Waiting for Istio to process backend VirtualService..."
    sleep 10

    # Restart deployment to ensure fresh image is used
    log_info "Restarting backend deployment..."
    indent_output kubectl rollout restart deployment workspaces-backend -n kubeflow-workspaces

    # Validate backend deployment
    log_info "Validating backend deployment..."
    indent_output kubectl wait --for=condition=ready pod -l app.kubernetes.io/component=api -n kubeflow-workspaces --timeout=300s
    log_success "Backend built, deployed, and validated"
}

# Build and deploy frontend component
build_and_deploy_frontend() {
    log_info "Building frontend..."
    local img
    img=$(generate_image_tag "workspaces/frontend" "node_modules,dist")
    log_info "Generated image tag: $img"

    pushd workspaces/frontend >/dev/null

    # Check if image already exists
    local image_exists
    if image_exists_locally "$img"; then
        image_exists=true
    else
        image_exists=false
    fi

    if [ "$force_mode" = true ] || [ "$image_exists" = false ]; then
        if [ "$force_mode" = true ]; then
            log_info "Force mode enabled, rebuilding frontend image without cache"
        else
            log_info "Building frontend image..."
        fi

        # Build container image with retry mechanism
        log_info "Building frontend container image..."
        local build_args=""
        if [ "$force_mode" = true ]; then
            build_args="--no-cache"
        fi
        retry_with_backoff "$CONTAINER_ENGINE build $build_args -f Dockerfile -t \"$img\" ."
    else
        log_info "Frontend image already exists locally, skipping build"
    fi

    # Load image into kind cluster
    load_image_into_kind "$img"

    # Deploy frontend using kustomize (simulating make deploy)
    log_info "Deploying frontend..."
    pushd manifests/kustomize/overlays/istio >/dev/null
    indent_output kustomize edit set image "workspaces-frontend=$img"
    popd >/dev/null
    indent_output kubectl apply -k manifests/kustomize/overlays/istio

    popd >/dev/null

    # Wait for Istio to process the frontend VirtualService
    log_info "Waiting for Istio to process frontend VirtualService..."
    sleep 10

    # Restart deployment to ensure fresh image is used
    log_info "Restarting frontend deployment..."
    indent_output kubectl rollout restart deployment workspaces-frontend -n kubeflow-workspaces

    # Validate frontend deployment
    log_info "Validating frontend deployment..."
    indent_output kubectl wait --for=condition=ready pod -l app.kubernetes.io/component=ui -n kubeflow-workspaces --timeout=300s
    log_success "Frontend built, deployed, and validated"
}

# Install kubeflow notebooks components
install_notebook_components() {
    log_info "Installing kubeflow notebooks components..."
    log_info "Using image registry: $image_registry"
    log_info "Using tag suffix: $tag_suffix"

    cd "$project_root"

    # Build controller
    build_and_deploy_controller

    # Build backend
    build_and_deploy_backend

    # Build frontend
    build_and_deploy_frontend

    log_success "All components built, deployed, and validated"
}



# Main function
main() {
    log_info "Starting kubeflow notebooks setup..."

    # Check ulimit settings
    check_ulimit

    # Verify applications
    verify_applications

    # Install kind if needed
    install_kind

    # Install kustomize if needed
    install_kustomize

    # Install yq if needed
    install_yq

    # Install jq if needed
    install_jq

    # Clean up existing clusters
    cleanup_existing_clusters

    # Create kind cluster
    create_kind_cluster

    # Login to container registry
    login_container_registry

    # Install core Kubeflow dependencies
    install_kubeflow_dependencies

    # Install notebooks components
    install_notebook_components

    log_success "kubeflow notebooks setup completed successfully!"
    log_info ""
    log_info "=== ACCESSING THE CENTRAL DASHBOARD ==="
    log_info "To access the Central Dashboard, you must first run:"
    log_info "  kubectl -n istio-system port-forward svc/istio-ingressgateway 8080:80"
    log_info ""
    log_info "Then access the dashboard at: http://localhost:8080"
    log_info ""
    log_info "When prompted for authentication, use these credentials:"
    log_info "  Username: user@example.com"
    log_info "  Password: 12341234"
}

# Help function
show_help() {
    cat <<EOF
Usage: $0 [OPTIONS]

Setup script for kubeflow notebooks within Central Dashboard.

OPTIONS:
    -h, --help                    Show this help message
    -v, --verbose                 Enable verbose output
    -f, --force                   Force mode: auto-answer prompts with 'y' and always rebuild images
    -r, --registry REGISTRY       Image registry hostname (default: $default_image_registry)
    -t, --tag-suffix SUFFIX      Image tag suffix (default: $default_tag_suffix)

EXAMPLES:
    $0                                    # Use default settings
    $0 -r my-registry.com -t v1.0.0      # Custom registry and tag
    $0 --registry localhost --tag-suffix dev

This script will:
1. Verify required applications (go, node, kubectl, container engine, kind, make, kustomize)
2. Install kind and kustomize if not present
3. Clean up existing kind clusters (with confirmation)
4. Create a new kind cluster for kubeflow
5. Login to container registry
6. Install core Kubeflow dependencies using kubeflow/manifests remote URLs:
   - cert-manager (via kubectl apply -k)
   - Istio (via kubectl apply -k)
   - oauth2-proxy (via kubectl apply -k)
   - dex (via kubectl apply -k) + configmap modification for password grant type
   - centraldashboard (via kubectl apply -k) + configmap modification for Notebooks v2 menu
   - profile-controller + kfam (via kubectl apply -k applications/profiles/upstream/overlays/kubeflow)
   - PodDefaults (via kubectl apply -k)
   - Profile manifest creation for admin user
7. Build and deploy kubeflow notebooks components with content-based image tags:
   - controller: \${registry}/kubeflow-notebooks-v2:controller-\${tag_suffix}-\${hash} (via make)
   - backend: \${registry}/kubeflow-notebooks-v2:backend-\${tag_suffix}-\${hash} (via make)
   - frontend: \${registry}/kubeflow-notebooks-v2:frontend-\${tag_suffix}-\${hash} (via npm + container engine + kustomize)
   - Images are only rebuilt if they don't exist locally or source code has changed
8. Validate all components

Prerequisites:
- Go 1.23+
- Node.js 22+
- kubectl
- container engine (podman or docker)
- make or gmake (GNU make preferred on macOS)
- kustomize 5.4.3+ (will be installed if missing)
- yq (will be installed if missing)
- jq (will be installed if missing)

EOF
}

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        -h|--help)
            show_help
            exit 0
            ;;
        -v|--verbose)
            set -x
            shift
            ;;
        -f|--force)
            force_mode=true
            shift
            ;;
        -r|--registry)
            if [[ -z "$2" || "$2" =~ ^- ]]; then
                log_error "Registry option requires a value"
                show_help
                exit 1
            fi
            image_registry="$2"
            shift 2
            ;;
        -t|--tag-suffix)
            if [[ -z "$2" || "$2" =~ ^- ]]; then
                log_error "Tag suffix option requires a value"
                show_help
                exit 1
            fi
            tag_suffix="$2"
            shift 2
            ;;
        *)
            log_error "Unknown option: $1"
            show_help
            exit 1
            ;;
    esac
done

# Run main function
main "$@"