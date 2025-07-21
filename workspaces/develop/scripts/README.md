# Development Scripts

This directory contains scripts for setting up and managing the kubeflow-notebooks development environment.

## Scripts

### `setup-kubeflow-notebooks.sh`

A comprehensive setup script that automates the installation and configuration of kubeflow notebooks within Central Dashboard for local development.

#### Features

- **POSIX-compliant**: Works across different Unix-like systems
- **Comprehensive validation**: Checks for all required dependencies
- **Interactive cleanup**: Safely removes existing clusters with user confirmation
- **Colored output**: Clear visual feedback with color-coded messages
- **Error handling**: Robust error handling with proper exit codes
- **Modular design**: Organized into logical functions for easy maintenance
- **Smart registry authentication**: Automatically checks for existing credentials and prompts for login when needed
- **Retry mechanism**: Built-in retry with exponential backoff for network operations
- **Anonymous login support**: Supports both authenticated and anonymous container registry access

#### Prerequisites

Before running the script, ensure you have the following applications installed:

- **Go** 1.23+ - For building Go applications
- **Node.js** 22+ - For building frontend components
- **kubectl** - For Kubernetes cluster management
- **podman** or **docker** - For container operations (podman preferred)
- **make** or **gmake** - For build automation (GNU make preferred on macOS)
- **kustomize** 5.4.3+ - For applying kubeflow/manifests (will be installed if missing)
- **yq** - For modifying dex configmap (will be installed if missing)
- **jq** - For modifying centraldashboard configmap (will be installed if missing)

#### Usage

```bash
# Basic usage (uses default settings)
./setup-kubeflow-notebooks.sh

# Show help
./setup-kubeflow-notebooks.sh --help

# Enable verbose output
./setup-kubeflow-notebooks.sh --verbose

# Custom image registry and tag suffix
./setup-kubeflow-notebooks.sh -r my-registry.com -t v1.0.0

# Long form options
./setup-kubeflow-notebooks.sh --registry localhost --tag-suffix dev
```

#### What the script does

1. **Verifies applications**: Checks for all required dependencies
2. **Installs tools**: Installs kind and kustomize if not present
3. **Cleans up clusters**: Removes existing kind clusters (with confirmation)
4. **Creates cluster**: Sets up a new kind cluster for kubeflow
5. **Container registry**: Intelligently checks for existing credentials and prompts for login when needed (supports both authenticated and anonymous access)
6. **Installs dependencies**: Installs core Kubeflow components using kubeflow/manifests remote URLs:
   - cert-manager (via kubectl apply -k)
   - Istio (via kubectl apply -k)
   - oauth2-proxy (via kubectl apply -k)
   - dex (via kubectl apply -k) + configmap modification for password grant type
   - centraldashboard (via kubectl apply -k) + configmap modification for Notebooks v2 menu
7. **Builds components**: Builds and deploys kubeflow notebooks components with custom images (includes retry mechanism for network operations):
   - controller: `${registry}/kubeflow-notebooks-v2:controller-${tag_suffix}` (via make with retry)
   - backend: `${registry}/kubeflow-notebooks-v2:backend-${tag_suffix}` (via make with retry)
   - frontend: `${registry}/kubeflow-notebooks-v2:frontend-${tag_suffix}` (via docker build with retry)
8. **Validates setup**: Ensures all components are running correctly

#### Output

The script provides detailed, color-coded output:

- 🔵 **Blue**: Informational messages
- 🟢 **Green**: Success messages
- 🟡 **Yellow**: Warning messages
- 🔴 **Red**: Error messages

#### Troubleshooting

If the script fails:

1. **Check prerequisites**: Ensure all required applications are installed
2. **Check cluster status**: Verify the kind cluster is running
3. **Check resource limits**: Ensure sufficient system resources
4. **Check network**: Verify internet connectivity for downloading components (the script includes automatic retry for network operations)
5. **Check permissions**: Ensure proper permissions for container operations
6. **Check registry credentials**: Verify container registry authentication if using private registries

#### Manual Steps

After running the script, you may need to:

1. **Configure authentication**: Set up proper authentication for Central Dashboard
2. **Configure ingress**: Set up ingress rules for external access
3. **Configure storage**: Set up persistent storage if needed
4. **Configure networking**: Configure Istio service mesh as needed

#### Image Customization

The script supports customizing image registries and tags for kubeflow notebooks components:

- **Default Registry**: `localhost`
- **Default Tag Suffix**: `e2e-istio`
- **Controller Image**: `${registry}/kubeflow-notebooks-v2:controller-${tag_suffix}`
- **Backend Image**: `${registry}/kubeflow-notebooks-v2:backend-${tag_suffix}`
- **Frontend Image**: `${registry}/kubeflow-notebooks-v2:frontend-${tag_suffix}`

Use `-r` or `--registry` to specify a custom registry and `-t` or `--tag-suffix` to specify a custom tag suffix.

#### Container Registry Authentication

The script includes intelligent container registry authentication:

- **Automatic credential detection**: Checks for existing credentials in both Docker and Podman configuration files
- **Interactive prompts**: Prompts for username and password when credentials are not found
- **Anonymous access support**: Allows empty credentials for anonymous registry access
- **Docker Hub compatibility**: Handles various Docker Hub registry formats (docker.io, index.docker.io, etc.)
- **Multi-engine support**: Works with both Docker and Podman container engines
- **Force mode handling**: In force mode, requires existing credentials or manual login

The script will automatically detect if you're already logged in to your container registry and skip the login process if valid credentials are found.

#### Notes

- The script is designed for local development using kind clusters
- It uses the experimental branch with cherry-picked commits from various PRs
- Resource requirements can be significant for a full Kubeflow setup
- The script includes proper error handling and cleanup procedures
- Uses kubeflow/manifests remote URLs for installing core dependencies via kubectl apply -k
- Follows the official kubeflow/manifests installation process for cert-manager, Istio, oauth2-proxy, dex, and centraldashboard
- No need to clone the kubeflow/manifests repository locally
- Frontend uses simplified docker build process with retry mechanism (npm dependencies are handled automatically by the Dockerfile)

## Contributing

When adding new scripts:

1. Follow POSIX compliance standards
2. Include proper error handling
3. Add colored output for better UX
4. Include help documentation
5. Test across different environments
6. Add appropriate comments and documentation