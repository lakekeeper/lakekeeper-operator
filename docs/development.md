# Development Guide

Guide for developing and contributing to the Lakekeeper operator.

## Table of Contents

- [Prerequisites](#prerequisites)
- [Local Development Setup](#local-development-setup)
- [Development Workflow](#development-workflow)
- [Build Commands](#build-commands)
- [Running Locally](#running-locally)
- [Code Style](#code-style)
- [Contributing](#contributing)

## Prerequisites

- **Go**: 1.24.0+
- **Docker**: 17.03+
- **kubectl**: v1.21+
- **Kubernetes cluster**: v1.21+ (Kind, k3d, minikube, or cloud cluster)
- **operator-sdk**: v1.42.2+ (optional, for scaffolding)

### Install Tools

```bash
# Install operator-sdk (optional)
brew install operator-sdk

# Install Kind (recommended for local testing)
brew install kind
```

## Local Development Setup

1. **Clone the repository**:
   ```bash
   git clone https://github.com/lakekeeper/lakekeeper-operator.git
   cd lakekeeper-operator
   ```

2. **Install dependencies**:
   ```bash
   go mod download
   ```

3. **Generate code and manifests**:
   ```bash
   make manifests generate
   ```

4. **Create a test cluster** (optional):
   ```bash
   make setup-test-e2e  # Creates Kind cluster
   ```

## Development Workflow

### 1. Create New API/CRD

**Always use operator-sdk CLI** for scaffolding:

```bash
# Create new API and controller
operator-sdk create api \
  --group lakekeeper \
  --version v1alpha1 \
  --kind <YourKind> \
  --resource \
  --controller

# Add webhook support
operator-sdk create webhook \
  --group lakekeeper \
  --version v1alpha1 \
  --kind <YourKind> \
  --defaulting \
  --programmatic-validation
```

### 2. Edit API Types

Edit `api/v1alpha1/<kind>_types.go`:

```go
type LakekeeperSpec struct {
    // Add your fields here
    Image string `json:"image,omitempty"`
    
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=10
    Replicas *int32 `json:"replicas,omitempty"`
}
```

Add kubebuilder markers for validation:
- `+kubebuilder:validation:Required` - Required field
- `+kubebuilder:validation:Pattern=<regex>` - Regex validation
- `+kubebuilder:validation:Minimum=<n>` - Numeric minimum
- `+kubebuilder:validation:Maximum=<n>` - Numeric maximum

### 3. Generate Manifests

```bash
# Regenerate CRDs, DeepCopy methods, and RBAC
make manifests generate
```

This updates:
- `config/crd/bases/*.yaml` - CRD manifests
- `api/*/zz_generated.deepcopy.go` - DeepCopy methods
- `config/rbac/role.yaml` - RBAC rules

### 4. Implement Controller Logic

Edit `internal/controller/<kind>_controller.go`:

```go
func (r *LakekeeperReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    log := log.FromContext(ctx)
    
    // 1. Fetch the resource
    lakekeeper := &lakekeeperv1alpha1.Lakekeeper{}
    if err := r.Get(ctx, req.NamespacedName, lakekeeper); err != nil {
        if apierrors.IsNotFound(err) {
            return ctrl.Result{}, nil
        }
        return ctrl.Result{}, err
    }
    
    // 2. Handle deletion with finalizers
    if !lakekeeper.DeletionTimestamp.IsZero() {
        return r.reconcileDelete(ctx, lakekeeper)
    }
    
    // 3. Add finalizer if missing
    if !controllerutil.ContainsFinalizer(lakekeeper, finalizerName) {
        controllerutil.AddFinalizer(lakekeeper, finalizerName)
        if err := r.Update(ctx, lakekeeper); err != nil {
            return ctrl.Result{}, err
        }
        return ctrl.Result{Requeue: true}, nil
    }
    
    // 4. Reconcile owned resources
    if err := r.reconcileDeployment(ctx, lakekeeper); err != nil {
        return ctrl.Result{}, err
    }
    
    // 5. Update status
    return r.updateStatus(ctx, lakekeeper)
}
```

**Controller Best Practices**:
- Always check `DeletionTimestamp` for deletion handling
- Use finalizers for cleanup logic
- Keep reconcile logic idempotent
- Use `ctrl.Result{Requeue: true}` for retries
- Update status separately from spec changes
- Set owner references on created resources

### 5. Write Tests

**Unit tests** (`internal/controller/<kind>_controller_test.go`):
```go
It("should create Deployment", func() {
    lakekeeper := &lakekeeperv1alpha1.Lakekeeper{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "test",
            Namespace: "default",
        },
        Spec: lakekeeperv1alpha1.LakekeeperSpec{
            Image: "lakekeeper:test",
        },
    }
    
    Expect(k8sClient.Create(ctx, lakekeeper)).To(Succeed())
    
    deployment := &appsv1.Deployment{}
    Eventually(func() error {
        return k8sClient.Get(ctx, client.ObjectKeyFromObject(lakekeeper), deployment)
    }).Should(Succeed())
})
```

**E2E tests** (`test/e2e/e2e_test.go`):
```go
It("should deploy real Lakekeeper", func() {
    By("Creating Lakekeeper CR")
    kubectl("apply", "-f", "samples/lakekeeper.yaml", "-n", namespace)
    
    By("Waiting for deployment")
    Eventually(func() string {
        out, _ := kubectl("get", "deployment", "lakekeeper", "-n", namespace, "-o", "jsonpath={.status.readyReplicas}")
        return out
    }, timeout, interval).Should(Equal("1"))
})
```

### 6. Run Tests

```bash
# Unit tests
make test

# E2E tests
make test-e2e

# Specific test
go test ./internal/controller/ -run TestLakekeeperController
```

## Build Commands

### Code Generation

```bash
make manifests        # Generate CRDs and RBAC manifests
make generate         # Generate DeepCopy methods
make fmt              # Format code
make vet              # Run go vet
make lint             # Run golangci-lint
make lint-fix         # Fix linting issues automatically
```

### Testing

```bash
make test             # Unit tests with coverage
make test-e2e         # End-to-end tests on Kind
make setup-test-e2e   # Create Kind cluster for manual E2E testing
make cleanup-test-e2e # Delete Kind test cluster
```

### Build and Deploy

```bash
make build            # Build manager binary
make run              # Run locally against current kubectl context
make docker-build     # Build container image
make docker-push      # Push to registry
make install          # Install CRDs to cluster
make deploy           # Deploy operator to cluster
make undeploy         # Remove operator from cluster
make uninstall        # Remove CRDs from cluster
make build-installer  # Generate dist/install.yaml
```

### Full Development Cycle Example

```bash
# 1. Make changes to API or controller
vim api/v1alpha1/lakekeeper_types.go
vim internal/controller/lakekeeper_controller.go

# 2. Regenerate code
make manifests generate

# 3. Run tests
make test

# 4. Build and test locally
make docker-build IMG=lakekeeper-operator:dev
kind load docker-image lakekeeper-operator:dev --name lakekeeper-operator-test-e2e
make install deploy IMG=lakekeeper-operator:dev

# 5. Test against real cluster
kubectl apply -f config/samples/lakekeeper_v1alpha1_lakekeeper.yaml

# 6. Check logs
kubectl logs -n lakekeeper-operator-system deployment/lakekeeper-operator-controller-manager
```

## Running Locally

### Option 1: Run Against Current Cluster

```bash
# Install CRDs
make install

# Run controller locally (uses kubeconfig)
make run
```

The controller runs on your machine but manages resources in the cluster.

### Option 2: Debug in VS Code

Create `.vscode/launch.json`:
```json
{
    "version": "0.2.0",
    "configurations": [
        {
            "name": "Debug Operator",
            "type": "go",
            "request": "launch",
            "mode": "auto",
            "program": "${workspaceFolder}/cmd/main.go",
            "env": {
                "KUBECONFIG": "${env:HOME}/.kube/config"
            },
            "args": []
        }
    ]
}
```

Set breakpoints and press F5 to debug.

### Option 3: Deploy to Kind Cluster

```bash
make setup-test-e2e
make docker-build IMG=lakekeeper-operator:dev
kind load docker-image lakekeeper-operator:dev --name lakekeeper-operator-test-e2e
make install deploy IMG=lakekeeper-operator:dev
```

## Code Style

### Go Conventions

- Follow [Effective Go](https://go.dev/doc/effective_go)
- Use `gofmt` for formatting (included in `make fmt`)
- Keep functions small and focused (KISS principle)
- Avoid premature optimization (YAGNI principle)
- Prefer composition over inheritance
- Use interfaces for testability

### Project Structure

```
.
├── api/                    # API type definitions
│   └── v1alpha1/          # API version
├── cmd/                   # Main application entry point
├── config/                # Kustomize manifests
│   ├── crd/              # CRD definitions
│   ├── rbac/             # RBAC manifests
│   ├── manager/          # Manager deployment
│   └── samples/          # Example CRs
├── internal/
│   └── controller/       # Controller implementations
├── test/
│   ├── e2e/              # End-to-end tests
│   └── utils/            # Test utilities
└── docs/                 # Documentation
```

### Error Handling

```go
// Wrap errors with context
if err := r.Create(ctx, deployment); err != nil {
    return ctrl.Result{}, fmt.Errorf("failed to create deployment: %w", err)
}

// Log errors before returning
if err := r.doSomething(); err != nil {
    log.Error(err, "Failed to do something")
    return ctrl.Result{}, err
}

// Return requeue for transient errors
if err := r.checkExternalDependency(); err != nil {
    log.Info("Dependency not ready, requeuing", "error", err)
    return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}
```

### Testing Patterns

```go
// Use descriptive test names
It("should create Deployment with correct image", func() {
    // ...
})

// Use Eventually for async operations
Eventually(func() error {
    return k8sClient.Get(ctx, key, obj)
}, timeout, interval).Should(Succeed())

// Use Consistently for stability checks
Consistently(func() int {
    return len(deployment.Spec.Template.Spec.Containers)
}, duration, interval).Should(Equal(1))
```

## Contributing

### Workflow

1. Fork the repository
2. Create a feature branch: `git checkout -b feature/my-feature`
3. Make changes following code style guidelines
4. Add/update tests
5. Run `make manifests generate fmt vet lint test`
6. Commit with conventional commits: `git commit -m "feat: add new feature"`
7. Push and create pull request

### Commit Messages

Use [Conventional Commits](https://www.conventionalcommits.org/):

- `feat:` - New feature
- `fix:` - Bug fix
- `docs:` - Documentation changes
- `test:` - Test changes
- `refactor:` - Code refactoring
- `chore:` - Build/tooling changes

Examples:
```
feat: add warehouse CRD and controller
fix: handle nil pointer in status update
docs: update development guide
test: add e2e test for custom ports
```

## See Also

- [Testing Guide](testing.md) - Comprehensive testing documentation
- [Architecture](architecture.md) - Operator design overview
- [Operator SDK Docs](https://sdk.operatorframework.io/)
- [Kubebuilder Book](https://book.kubebuilder.io/)
