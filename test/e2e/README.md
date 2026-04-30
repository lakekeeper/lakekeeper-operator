# E2E Test Quick Reference

## Run E2E Tests

E2E tests work with **any Kubernetes cluster** (Kind, k3d, minikube, cloud). Tests use your current `kubectl` context.

```bash
# Full end-to-end (creates Kind cluster, builds operator, runs tests)
make test-e2e

# Run tests against existing cluster (any type!)
go test -v ./test/e2e

# Run specific test
go test -v ./test/e2e -ginkgo.focus "should deploy Lakekeeper"

# With podman instead of docker
CONTAINER_TOOL=podman go test -v ./test/e2e
```

## Cluster Setup

E2E tests are **cluster-agnostic** - use whichever cluster you prefer:

**Cluster Detection Priority:**
1. **Environment variables** (explicit user intent - highest priority)
   - `K3D_CLUSTER=my-cluster` → forces k3d with cluster "my-cluster"
   - `KIND_CLUSTER=my-cluster` → forces kind with cluster "my-cluster"  
   - `MINIKUBE_PROFILE=my-profile` → forces minikube with profile "my-profile"
2. **Kubectl context auto-detection** (pattern matching)
   - Context contains `k3d-` → auto-detect as k3d
   - Context contains `kind-` → auto-detect as kind
   - Context is `minikube` → auto-detect as minikube
3. **Fallback:** If unable to detect, image loading will fail with helpful error

**Use environment variables when:**
- Your cluster doesn't follow standard naming (e.g., k3d cluster named "my-cluster" without "k3d-" prefix)
- You want to explicitly override auto-detection  
- You're testing with multiple clusters and want to be explicit

### Kind (Default in Makefile)
```bash
make setup-test-e2e    # Creates Kind cluster
# Cluster name: lakekeeper-operator-test-e2e (or set KIND_CLUSTER env var)

# Load image for Kind
kind load docker-image example.com/lakekeeper-operator:v0.0.1 --name lakekeeper-operator-test-e2e

# Override detection if needed
KIND_CLUSTER=my-custom-kind go test -v ./test/e2e
```

### k3d (Recommended for fast local dev)
```bash
# Create k3d cluster (standard naming - auto-detects)
k3d cluster create lakekeeper-test  # Creates context "k3d-lakekeeper-test"

# Create k3d cluster (custom naming - needs env var)
k3d cluster create my-cluster  # Creates context "k3d-my-cluster" (auto-detects!)
# OR if context is just "my-cluster":
K3D_CLUSTER=my-cluster go test -v ./test/e2e

# Run tests (auto-detects k3d!)
go test -v ./test/e2e

# OR with podman
CONTAINER_TOOL=podman go test -v ./test/e2e

# Load image for k3d (happens automatically, or manual):
k3d image import example.com/lakekeeper-operator:v0.0.1 --cluster lakekeeper-test
```

### minikube
```bash
minikube start --kubernetes-version=v1.33.0

# Build with minikube's docker daemon
eval $(minikube docker-env)
make docker-build IMG=example.com/lakekeeper-operator:v0.0.1

# Run tests (auto-detects minikube!)
go test -v ./test/e2e

# Custom profile
minikube start --kubernetes-version=v1.33.0 --profile=test-profile
MINIKUBE_PROFILE=test-profile go test -v ./test/e2e
```

### Cloud (EKS, GKE, AKS)
```bash
# Push image to registry
docker push myregistry/lakekeeper-operator:v0.0.1

# Run tests with IMAGE_LOAD_SKIP=true
IMG=myregistry/lakekeeper-operator:v0.0.1 IMAGE_LOAD_SKIP=true go test -v ./test/e2e
```

## What Gets Tested

1. **Lakekeeper Deployment**: Real pods, init containers, database migrations
2. **Service Networking**: Main API (8080), metrics (9090), custom ports
3. **Health Checks**: `/health`, `/metrics`, `/management/v1/info`
4. **Configuration**: Resource limits, port changes, rolling updates
5. **Lifecycle**: Creation, updates, deletion, garbage collection

## Test Structure

```
test/e2e/
├── e2e_suite_test.go     # Test suite setup
├── e2e_test.go           # Manager tests + Lakekeeper E2E tests
└── scenarios/            # (future) Complex multi-resource scenarios

test/utils/
└── utils.go              # Helper functions (Run, RandString, etc.)
```

## Key Test Scenarios

### Test 1: Basic Deployment
- PostgreSQL → Lakekeeper CR → Deployment → Services → Health checks

### Test 2: Clean Deletion
- Delete CR → Verify deployment cleaned up → Verify no orphans

### Test 3: Custom Ports
- Update ports → Rolling update → Verify services → Test endpoints

### Test 4: Resource Limits
- Patch resources → Verify pod spec → Ensure pod still runs

### Test 5: Management API
- Deploy Lakekeeper → Call `/management/v1/info` → Verify response

## Debugging Failed Tests

```bash
# Check test namespace (created with random suffix)
kubectl get ns | grep lakekeeper-e2e

# View Lakekeeper resources
kubectl get lakekeeper -n lakekeeper-e2e-<random>

# Check pod logs
kubectl logs -n lakekeeper-e2e-<random> <pod-name>

# Check init container logs
kubectl logs -n lakekeeper-e2e-<random> <pod-name> -c migrate

# Check events
kubectl get events -n lakekeeper-e2e-<random> --sort-by=.lastTimestamp
```

## Environment Variables

```bash
# Skip CertManager installation (if already installed)
export CERT_MANAGER_INSTALL_SKIP=true

# Skip automatic image loading (for cloud clusters or manual loading)
export IMAGE_LOAD_SKIP=true

# Use specific Kind cluster (for auto-detection)
export KIND_CLUSTER=my-test-cluster

# Use specific k3d cluster (for auto-detection)
export K3D_CLUSTER=my-k3d-cluster

# Use specific container tool (docker or podman)
export CONTAINER_TOOL=podman

# Use specific operator image
export IMG=myregistry/lakekeeper-operator:v0.0.1
```

## CI/CD Integration

E2E tests are designed to run in CI pipelines:

```yaml
# GitHub Actions example
- name: Run E2E Tests
  run: |
    make setup-test-e2e
    make test-e2e
    make cleanup-test-e2e
```

## Image Loading

The test suite automatically detects your cluster type and loads images appropriately:

- **Kind**: Uses `kind load docker-image`
- **k3d**: Uses `k3d image import`
- **Minikube**: Assumes image built with `eval $(minikube docker-env)`
- **Cloud/Registry**: Use `IMAGE_LOAD_SKIP=true` and ensure images are pullable

**Manual Image Loading** (if auto-detection fails):

```bash
# Skip automatic loading
export IMAGE_LOAD_SKIP=true

# Then manually load based on your cluster:
kind load docker-image example.com/lakekeeper-operator:v0.0.1 --name <cluster-name>
# OR
k3d image import example.com/lakekeeper-operator:v0.0.1 --cluster <cluster-name>
# OR
docker push myregistry/lakekeeper-operator:v0.0.1  # For cloud
```

## Dependencies

**Cluster**:
- Kubernetes 1.28+ (tested with 1.33)
- StorageClass for PostgreSQL PVCs
- Sufficient resources (2 CPU, 4GB RAM recommended)
- **Any cluster type**: Kind, k3d, minikube, EKS, GKE, AKS, etc.

**Images**:
- Operator: Built locally and loaded/pushed to cluster
- Lakekeeper: `quay.io/lakekeeper/catalog:latest`
- PostgreSQL: `postgres:16-alpine`

**Tools**:
- kubectl (required)
- docker or podman (for building images)
- **One of**: kind, k3d, minikube, or cloud cluster access
- go 1.24+ (for running tests
**Images**:
- Operator: Built locally and loaded to cluster
- Lakekeeper: `quay.io/lakekeeper/catalog:latest`
- PostgreSQL: `postgres:16-alpine`

**Tools**:
- kubectl
- Kind (or alternative cluster)
- Docker (for image building)
