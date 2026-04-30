# Testing Guide

This guide covers all testing strategies for the Lakekeeper operator: unit tests, integration tests, and end-to-end (E2E) tests.

## Quick Reference

```bash
# Run all unit tests with coverage
make test

# Run E2E tests (creates Kind cluster, builds operator, runs tests)
make test-e2e

# Run specific test
go test -v ./internal/controller -run TestLakekeeperReconciler

# Run E2E test against existing cluster
go test -v ./test/e2e -ginkgo.focus "should deploy Lakekeeper"
```

## Testing Strategy

The Lakekeeper operator uses a **three-layer testing approach**:

| Test Type | Scope | Framework | What It Tests |
|-----------|-------|-----------|---------------|
| **Unit Tests** | Individual functions/methods | Go testing + Ginkgo/Gomega | Business logic, error handling, helpers |
| **Integration Tests** | Controller reconciliation | envtest (fake K8s API) | CRD logic, status updates, resource creation |
| **E2E Tests** | Full system | Real Kubernetes cluster | Actual deployments, networking, Lakekeeper runtime |

### When to Use Each Test Type

**Unit Tests** - Use for:
- Validation logic
- Helper functions
- Error handling
- Edge cases
- Fast feedback during development

**Integration Tests** - Use for:
- Controller reconciliation loops
- CRD spec/status updates
- Kubernetes resource creation (Deployments, Services, etc.)
- Owner references and garbage collection
- Finalizer logic

**E2E Tests** - Use for:
- Pod execution and container runtime
- Network connectivity (services, endpoints)
- Application-level behavior (health checks, APIs)
- Multi-component interactions (init containers, migrations)
- Real-world deployment scenarios

## Unit Tests

### Location
- `api/v1alpha1/*_test.go` - API validation tests
- `internal/controller/*_test.go` - Controller unit tests

### Running Unit Tests

```bash
# All unit tests
make test

# With coverage
go test ./... -coverprofile=cover.out
go tool cover -html=cover.out

# Verbose output
go test -v ./internal/controller

# Specific test
go test -v ./internal/controller -run TestValidateExternalDependencies
```

### Example: API Validation Unit Test

```go
var _ = Describe("Lakekeeper Validation", func() {
    It("should reject invalid database type", func() {
        lk := &Lakekeeper{
            Spec: LakekeeperSpec{
                Database: DatabaseConfig{
                    Type: "invalid",
                },
            },
        }
        Expect(lk.ValidateCreate()).To(HaveOccurred())
    })
})
```

### Coverage Goals

- **Target**: 80%+ coverage for business logic
- **Mandatory**: All validation functions must have tests
- **Focus**: Error paths and edge cases

## Integration Tests

### Location
- `internal/controller/*_test.go` - Controller integration tests using envtest

### Setup

Integration tests use **envtest**, which runs a real Kubernetes API server and etcd locally:

```go
var _ = BeforeSuite(func() {
    By("bootstrapping test environment")
    testEnv = &envtest.Environment{
        CRDDirectoryPaths: []string{
            filepath.Join("..", "..", "config", "crd", "bases"),
        },
    }
    cfg, err := testEnv.Start()
    Expect(err).NotTo(HaveOccurred())
    
    k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
    Expect(err).NotTo(HaveOccurred())
})
```

### Running Integration Tests

```bash
# Run integration tests
make test

# Or directly
go test ./internal/controller -v

# With race detector
go test -race ./internal/controller
```

### Example: Controller Integration Test

```go
It("should create Deployment for Lakekeeper", func() {
    lakekeeper := &lakekeeperv1alpha1.Lakekeeper{
        ObjectMeta: metav1.ObjectMeta{
            Name:      "test-lakekeeper",
            Namespace: "default",
        },
        Spec: lakekeeperv1alpha1.LakekeeperSpec{
            Image: "quay.io/lakekeeper/catalog:latest",
            Database: lakekeeperv1alpha1.DatabaseConfig{
                Type: "postgres",
                Postgres: &lakekeeperv1alpha1.PostgresConfig{
                    Host:     "postgres",
                    Database: "lakekeeper",
                    User:     ptr.To("lakekeeper"),
                    PasswordSecretRef: corev1.SecretKeySelector{
                        LocalObjectReference: corev1.LocalObjectReference{Name: "db-credentials"},
                        Key:                 "password",
                    },
                    EncryptionKeySecretRef: corev1.SecretKeySelector{
                        LocalObjectReference: corev1.LocalObjectReference{Name: "db-credentials"},
                        Key:                 "encryption-key",
                    },
                },
            },
            Authorization: lakekeeperv1alpha1.AuthorizationConfig{
                Backend: "allowall",
            },
        },
    }
    
    Expect(k8sClient.Create(ctx, lakekeeper)).To(Succeed())
    
    Eventually(func(g Gomega) {
        deployment := &appsv1.Deployment{}
        g.Expect(k8sClient.Get(ctx, types.NamespacedName{
            Name:      "test-lakekeeper",
            Namespace: "default",
        }, deployment)).To(Succeed())
        
        g.Expect(deployment.Spec.Template.Spec.Containers).To(HaveLen(1))
        g.Expect(deployment.Spec.Template.Spec.Containers[0].Image).To(Equal("quay.io/lakekeeper/catalog:latest"))
    }, timeout, interval).Should(Succeed())
})
```

### What Integration Tests Verify

✅ **Resource Creation**
- Deployments created with correct spec
- Services created with correct ports

✅ **Status Updates**
- Status conditions updated correctly
- Replica counts tracked
- Error conditions reflected in status

✅ **Reconciliation Logic**
- Idempotent reconciliation
- Proper error handling and requeuing
- Finalizer logic for cleanup

❌ **NOT Verified by Integration Tests**
- Pod execution (no actual containers run)
- Network routing (fake API, no real services)
- Application behavior (Lakekeeper doesn't actually start)

## E2E Tests

E2E tests are **cluster-agnostic** and work with any Kubernetes cluster. Tests automatically detect your cluster type and adapt accordingly.

### Location
- `test/e2e/e2e_test.go` - End-to-end tests on real clusters
- `test/e2e/README.md` - Quick reference guide
- `test/utils/utils.go` - Test helpers with cluster detection

### Cluster Detection

E2E tests automatically detect the type of Kubernetes cluster (k3d, kind, or minikube) to properly load container images before running tests.

#### Detection Priority

Cluster type detection follows this priority order:

**1. Environment Variables (Highest Priority)**

Environment variables represent **explicit user intent** and always take precedence over auto-detection:

- `K3D_CLUSTER=<cluster-name>` → Forces k3d with specified cluster name
- `KIND_CLUSTER=<cluster-name>` → Forces kind with specified cluster name  
- `MINIKUBE_PROFILE=<profile-name>` → Forces minikube with specified profile

**When to use:**
- Your cluster doesn't follow standard naming conventions (e.g., k3d cluster named "my-cluster" without "k3d-" prefix)
- You want to explicitly override auto-detection
- You're testing with multiple clusters and want to be explicit about which one to use

**2. Kubectl Context Auto-Detection (Default)**

If no environment variables are set, the framework attempts to detect cluster type from the kubectl context name:

- Context contains `k3d-` → Detected as k3d (cluster name extracted by removing prefix)
- Context contains `kind-` → Detected as kind (cluster name extracted by removing prefix)
- Context equals `minikube` → Detected as minikube

**Standard cluster creation patterns:**
```bash
# k3d - creates context "k3d-lakekeeper-test"
k3d cluster create lakekeeper-test

# kind - creates context "kind-lakekeeper-test"  
kind create cluster --name lakekeeper-test

# minikube - creates context "minikube"
minikube start
```

**3. Fallback**

If detection fails (no env vars, no matching context patterns), image loading will fail with a helpful error message:

```
unable to detect cluster type (kind/k3d/minikube).
Current context: <context-name>.
Please load image manually or skip with IMAGE_LOAD_SKIP=true
```

#### Example Scenarios

**Scenario 1: Standard k3d Cluster (Auto-Detection Works)**

```bash
# Create cluster with standard naming
k3d cluster create lakekeeper-test
# Context: k3d-lakekeeper-test

# Run tests - auto-detects k3d
go test -v ./test/e2e

# Detection flow:
# 1. No env vars set
# 2. Context "k3d-lakekeeper-test" matches pattern → k3d detected ✅
# 3. Image loaded with: k3d image import <image> --cluster lakekeeper-test
```

**Scenario 2: Custom k3d Cluster Name (Need Env Var)**

```bash
# Create cluster with custom naming (no k3d- prefix in context)
k3d cluster create my-cluster
# Context: my-cluster (or custom context name)

# Run tests - auto-detection FAILS
go test -v ./test/e2e
# ❌ Error: unable to detect cluster type

# SOLUTION: Set environment variable (explicit intent)
K3D_CLUSTER=my-cluster go test -v ./test/e2e
```

**Scenario 3: Multiple Clusters (Explicit Selection)**

```bash
# Multiple clusters exist; current context points to dev-cluster
# Want to test against test-cluster instead
K3D_CLUSTER=test-cluster go test -v ./test/e2e
```

**Scenario 4: Cloud Cluster (Skip Image Loading)**

```bash
IMAGE_LOAD_SKIP=true IMG=myregistry/operator:v1.0.0 go test -v ./test/e2e
```

#### Best Practices

✅ **Use environment variables for:**
- Non-standard cluster names
- Explicit cluster selection
- CI/CD environments where you want deterministic behavior

✅ **Rely on auto-detection for:**
- Standard cluster naming (`k3d-*`, `kind-*`)
- Local development with a single cluster

✅ **Skip image loading (`IMAGE_LOAD_SKIP=true`) for:**
- Cloud clusters (EKS, GKE, AKS)
- Images already present in the cluster

### Prerequisites

**Required**:
- Kubernetes cluster (Kind, k3d, minikube, or cloud)
- `kubectl` configured with cluster access
- Docker or Podman for building operator image

**Images**:
- Operator image: Built and loaded/pushed to cluster
- Lakekeeper image: `quay.io/lakekeeper/catalog:latest`
- PostgreSQL image: `postgres:16-alpine`

### Running E2E Tests

E2E tests automatically detect your cluster type from `kubectl config current-context`:

**Full E2E with Makefile** (uses Kind):
```bash
# Creates Kind cluster, builds operator, installs CRDs, runs tests
make test-e2e
```

**With k3d** (recommended for fast local dev):
```bash
# Create k3d cluster
k3d cluster create lakekeeper-test

# Run tests - auto-detects k3d and uses `k3d image import`
go test -v ./test/e2e

# With podman instead of docker
CONTAINER_TOOL=podman go test -v ./test/e2e

# Cleanup
k3d cluster delete lakekeeper-test
```

**With Kind** (manual):
```bash
# Create Kind cluster
kind create cluster --name lakekeeper-test

# Run tests - auto-detects kind and uses `kind load docker-image`
KIND_CLUSTER=lakekeeper-test go test -v ./test/e2e

# Cleanup
kind delete cluster --name lakekeeper-test
```

**With minikube**:
```bash
minikube start --kubernetes-version=v1.33.0

# Build with minikube's docker daemon
eval $(minikube docker-env)
make docker-build IMG=example.com/lakekeeper-operator:v0.0.1

# Run tests - auto-detects minikube (assumes image already built)
go test -v ./test/e2e

# Cleanup
minikube delete
```

**With cloud cluster** (EKS, GKE, AKS):
```bash
# Push operator image to registry
docker build -t myregistry/lakekeeper-operator:v0.0.1 .
docker push myregistry/lakekeeper-operator:v0.0.1

# Skip automatic image loading and use registry image
IMG=myregistry/lakekeeper-operator:v0.0.1 IMAGE_LOAD_SKIP=true go test -v ./test/e2e
```

**Run specific scenarios**:
```bash
# Focus on deployment tests
go test -v ./test/e2e -ginkgo.focus "Lakekeeper Instance Lifecycle"

# Focus on API tests
go test -v ./test/e2e -ginkgo.focus "Management API"

# Skip slow tests
go test -v ./test/e2e -ginkgo.skip "deletion"
```

### Environment Variables

E2E tests support several environment variables for flexibility:

| Variable | Purpose | Default | Example |
|----------|---------|---------|---------|
| `IMAGE_LOAD_SKIP` | Skip automatic image loading to cluster | `false` | `IMAGE_LOAD_SKIP=true` |
| `CERT_MANAGER_INSTALL_SKIP` | Skip CertManager installation | `false` | `CERT_MANAGER_INSTALL_SKIP=true` |
| `IMG` | Operator image to deploy | `example.com/lakekeeper-operator:v0.0.1` | `IMG=myregistry/operator:v1.0.0` |
| `KIND_CLUSTER` | Kind cluster name (for detection) | `lakekeeper-operator-test-e2e` | `KIND_CLUSTER=my-cluster` |
| `K3D_CLUSTER` | k3d cluster name (for detection) | Auto-detected | `K3D_CLUSTER=my-k3d-cluster` |
| `CONTAINER_TOOL` | Container tool to use | `docker` | `CONTAINER_TOOL=podman` |

**Common usage patterns**:

```bash
# Testing with k3d and podman (auto image loading)
CONTAINER_TOOL=podman go test -v ./test/e2e

# Using cloud cluster with registry
IMG=myregistry/lakekeeper-operator:v0.0.1 IMAGE_LOAD_SKIP=true go test -v ./test/e2e

# Skip CertManager if already installed
CERT_MANAGER_INSTALL_SKIP=true go test -v ./test/e2e

# Specify specific kind cluster
KIND_CLUSTER=my-kind-cluster go test -v ./test/e2e
```

**Image loading behavior**:

- **Automatic** (default): Tests detect cluster type and load images automatically
  - kind: Uses `kind load docker-image`
  - k3d: Uses `k3d image import`
  - minikube: Assumes image built with `eval $(minikube docker-env)`
- **Skip**: Set `IMAGE_LOAD_SKIP=true` when:
  - Using cloud cluster with registry
  - Image already loaded manually
  - Using private registry the cluster can access

### E2E Test Scenarios

#### Scenario 1: Lakekeeper Instance Lifecycle

**Test: "should deploy Lakekeeper with PostgreSQL and pass health checks"**

Verifies:
- ✅ Lakekeeper CR creates Deployment
- ✅ Pod starts and runs successfully
- ✅ Migration Job (`<name>-migrate`) completes database migrations
- ✅ Main service (port 8181) is created
- ✅ Metrics service (port 9000) is created
- ✅ `/health` endpoint responds with 200 OK
- ✅ `/metrics` endpoint returns Prometheus format
- ✅ Status conditions show `Ready=True`
- ✅ Replica counts match in status

**Test: "should handle Lakekeeper instance deletion cleanly"**

Verifies:
- ✅ Deleting Lakekeeper CR triggers cleanup
- ✅ Deployment is garbage collected
- ✅ Services are garbage collected
- ✅ No orphaned pods remain

#### Scenario 2: Lakekeeper Configuration Updates

**Test: "should handle custom server ports"**

Verifies:
- ✅ Updating `listenPort` and `metricsPort` triggers rolling update
- ✅ Services updated with custom ports
- ✅ Endpoints respond on new ports

**Test: "should handle resource limits"**

Verifies:
- ✅ CPU/memory requests and limits applied to pod
- ✅ Pod continues running with resource constraints

#### Scenario 3: Lakekeeper Management API

**Test: "should expose working management API"**

Verifies:
- ✅ `/management/v1/info` endpoint is accessible
- ✅ Returns valid JSON with server information

### E2E Test Structure

```go
// Use Serial + Ordered for multi-suite test files
var _ = Describe("Manager", Serial, Ordered, func() {
    // Suite 1: Manager setup/teardown
    BeforeAll(func() {
        // Install CRDs, deploy controller
    })
    AfterAll(func() {
        // Cleanup runs AFTER all suites complete
    })
})

var _ = Describe("Lakekeeper Instance Lifecycle", Serial, Ordered, func() {
    // Suite 2: Application tests (depends on CRDs from Suite 1)
    var namespace string
    
    BeforeAll(func() {
        // Generate unique test namespace
        namespace = fmt.Sprintf("lakekeeper-e2e-%s", utils.RandString(8))
        
        // Create namespace
        cmd := exec.Command("kubectl", "create", "namespace", namespace)
        _, err := utils.Run(cmd)
        Expect(err).NotTo(HaveOccurred())
        
        // Deploy PostgreSQL
        deployPostgres(namespace)
        
        // Wait for PostgreSQL to be ready
        Eventually(func(g Gomega) {
            cmd := exec.Command("kubectl", "get", "statefulset", "postgres",
                "-n", namespace, "-o", "jsonpath={.status.readyReplicas}")
            output, err := utils.Run(cmd)
            g.Expect(err).NotTo(HaveOccurred())
            g.Expect(output).To(Equal("1"))
        }, 3*time.Minute, 5*time.Second).Should(Succeed())
        
        // Create database credentials secret
        // ... secret creation ...
    })
    
    AfterAll(func() {
        // Important: Wait for deletion to complete
        cmd := exec.Command("kubectl", "delete", "namespace", namespace)
        _ = utils.Run(cmd)  // Synchronous - waits for completion
    })
    
    It("should deploy Lakekeeper with PostgreSQL", func() {
        By("Creating Lakekeeper CR")
        // ... test implementation ...
    })
})
```

**Important Ginkgo Patterns:**

- **`Serial`**: Forces top-level Describe blocks to run sequentially (not in parallel)
- **`Ordered`**: Runs specs within a Describe block in order
- **Cleanup**: Use synchronous `kubectl delete` (without `--wait=false`) to ensure complete cleanup
- **ClusterRoleBindings**: Always delete cluster-scoped resources explicitly (not garbage collected with namespace)

### What E2E Tests Verify

✅ **Pod Execution**
- Lakekeeper pods actually start and run
- Migration Job executes and completes successfully
- Main container runs `lakekeeper serve`
- Pod stays healthy with liveness/readiness probes

✅ **Service Networking**
- Main service (port 8181) is created and routable
- Metrics service (port 9000) is created and routable
- Custom ports work correctly
- Services reach pods via selectors

✅ **Lakekeeper Runtime**
- `/health` endpoint responds with 200 OK
- `/metrics` endpoint returns Prometheus metrics
- `/management/v1/info` returns server info JSON
- Database connectivity works (migrations succeed)

✅ **Operator Behavior**
- Deployment created with correct spec
- Owner references set for garbage collection
- Status conditions updated (Ready, Progressing, Degraded)
- Replica counts tracked in status
- Configuration changes trigger rolling updates
- Deletion triggers cleanup

✅ **Multi-Resource Coordination**
- PostgreSQL dependency is required
- Secret references are validated
- Migration Job completes before Deployment is created
- Services match deployment selectors

### What E2E Tests Don't Cover

❌ **Individual controller methods** (use unit tests)
❌ **Fake Kubernetes API** (use integration tests with envtest)
❌ **Multi-tenancy isolation** (planned for future)
❌ **Operator upgrades** (planned for future)
❌ **Scale testing** (planned for future)
❌ **External dependencies** (Vault, OpenFGA, NATS, Kafka - planned)

## Debugging Test Failures

### Unit/Integration Test Failures

```bash
# Run with verbose output
go test -v ./internal/controller

# Run specific test
go test -v ./internal/controller -run TestReconcileDeployment

# Debug with delve
dlv test ./internal/controller -- -test.run TestReconcileDeployment

# Check envtest logs
export KUBEBUILDER_ASSETS=$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest use 1.33.0 --bin-dir /tmp/envtest -p path)
go test -v ./internal/controller
```

### E2E Test Failures

**Check test namespace**:
```bash
# Find test namespace (created with random suffix)
kubectl get ns | grep lakekeeper-e2e

# View Lakekeeper resources
kubectl get lakekeeper -n lakekeeper-e2e-<random>
kubectl describe lakekeeper -n lakekeeper-e2e-<random> <name>
```

**Check pod logs**:
```bash
# Main container logs
kubectl logs -n lakekeeper-e2e-<random> <pod-name>

# Init container (migrate) logs
kubectl logs -n lakekeeper-e2e-<random> <pod-name> -c migrate

# Previous pod logs (if crashed)
kubectl logs -n lakekeeper-e2e-<random> <pod-name> --previous
```

**Check events**:
```bash
kubectl get events -n lakekeeper-e2e-<random> --sort-by=.lastTimestamp

# Events for specific resource
kubectl describe deployment -n lakekeeper-e2e-<random> <deployment-name>
```

## Common E2E Issues and Solutions

### Image Loading Problems

**Problem: "executable file not found in $PATH: kind"**

You're using k3d/minikube but tests are trying to use Kind commands.

**Solution**: Tests auto-detect cluster type. If detection fails, set `IMAGE_LOAD_SKIP=true` and load manually:

```bash
# For k3d
k3d image import example.com/lakekeeper-operator:v0.0.1 --cluster <cluster-name>

# For kind
kind load docker-image example.com/lakekeeper-operator:v0.0.1 --name <cluster-name>

# Then run tests
IMAGE_LOAD_SKIP=true go test -v ./test/e2e
```

**Problem: "no pull access to image" / ImagePullBackOff**

Image wasn't loaded to local cluster or pushed to accessible registry.

**Solution for local clusters**:
```bash
# Build image first
make docker-build IMG=example.com/lakekeeper-operator:v0.0.1

# Load to cluster (auto-detected)
go test -v ./test/e2e

# Or load manually for k3d
k3d image import example.com/lakekeeper-operator:v0.0.1 --cluster <name>

# Or load manually for kind
kind load docker-image example.com/lakekeeper-operator:v0.0.1 --name <name>

# For minikube: build with minikube's docker daemon
eval $(minikube docker-env)
make docker-build IMG=example.com/lakekeeper-operator:v0.0.1
```

**Solution for cloud clusters**:
```bash
# Push to registry
docker tag example.com/lakekeeper-operator:v0.0.1 myregistry/operator:v0.0.1
docker push myregistry/operator:v0.0.1

# Skip auto-loading and use registry image
IMG=myregistry/operator:v0.0.1 IMAGE_LOAD_SKIP=true go test -v ./test/e2e
```

### Cluster Detection Issues

See [Cluster Detection](#cluster-detection) above for the full detection priority, example scenarios, and troubleshooting.

### Container Tool Issues

**Problem: Using podman instead of docker**

**Solution**: Set `CONTAINER_TOOL=podman`:

```bash
CONTAINER_TOOL=podman make docker-build
CONTAINER_TOOL=podman go test -v ./test/e2e

# Or create alias
alias docker=podman
export CONTAINER_TOOL=podman
```

**Problem: "permission denied" with podman socket**

**Solution**: Start podman socket and set DOCKER_HOST:

```bash
systemctl --user start podman.socket
export DOCKER_HOST=unix:///run/user/$UID/podman/podman.sock
CONTAINER_TOOL=podman go test -v ./test/e2e
```

### CertManager Issues

**Problem: CertManager CRD conflicts or webhook errors**

**Solution**: Skip installation if already present:

```bash
CERT_MANAGER_INSTALL_SKIP=true go test -v ./test/e2e
```

**Check PostgreSQL**:
```bash
# PostgreSQL logs
kubectl logs -n lakekeeper-e2e-<random> postgres-0

# Connect to PostgreSQL
kubectl exec -it -n lakekeeper-e2e-<random> postgres-0 -- psql -U postgres
```

**Port forward for local debugging**:
```bash
# Forward Lakekeeper main service
kubectl port-forward -n lakekeeper-e2e-<random> svc/<service-name> 8181:8181

# Test endpoints locally
curl http://localhost:8181/health
curl http://localhost:8181/management/v1/info
```

## Coverage Reports

### Generate Coverage

```bash
# Unit and integration tests coverage
go test ./... -coverprofile=cover.out

# View in terminal
go tool cover -func=cover.out

# Generate HTML report
go tool cover -html=cover.out -o coverage.html
open coverage.html  # macOS
xdg-open coverage.html  # Linux
```

### Coverage Targets

- **Overall**: 80%+ coverage
- **Controllers**: 85%+ coverage
- **API validation**: 90%+ coverage
- **Utils**: 95%+ coverage

### CI Integration

GitHub Actions automatically:
- Runs unit tests with coverage
- Uploads coverage to Codecov (when configured)
- Fails PR if coverage drops below threshold

## Test Development Best Practices

### General Guidelines

✅ **DO**:
- Write tests before or alongside implementation (TDD/BDD)
- Use table-driven tests for multiple scenarios
- Test both happy paths and error cases
- Use `Eventually` for async assertions
- Use descriptive test names
- Clean up resources in `AfterEach`/`AfterAll`
- Make tests idempotent and repeatable

❌ **DON'T**:
- Test Kubernetes internals (trust the framework)
- Use sleeps instead of `Eventually`
- Ignore test failures
- Leave orphaned test resources
- Write flaky tests
- Test implementation details (test behavior)

### Table-Driven Tests

```go
It("should validate database types", func() {
    tests := []struct {
        name      string
        dbType    string
        shouldErr bool
    }{
        {"valid postgres", "postgres", false},
        {"valid sqlite", "sqlite", false},
        {"invalid type", "mysql", true},
        {"empty type", "", true},
    }
    
    for _, tt := range tests {
        By(tt.name)
        lk := &Lakekeeper{
            Spec: LakekeeperSpec{
                Database: DatabaseConfig{Type: tt.dbType},
            },
        }
        
        err := lk.ValidateCreate()
        if tt.shouldErr {
            Expect(err).To(HaveOccurred())
        } else {
            Expect(err).NotTo(HaveOccurred())
        }
    }
})
```

### Async Assertions

```go
// Good - uses Eventually
Eventually(func(g Gomega) {
    deployment := &appsv1.Deployment{}
    err := k8sClient.Get(ctx, namespacedName, deployment)
    g.Expect(err).NotTo(HaveOccurred())
    g.Expect(deployment.Status.ReadyReplicas).To(Equal(int32(1)))
}, timeout, interval).Should(Succeed())

// Bad - uses sleep
deployment := &appsv1.Deployment{}
time.Sleep(30 * time.Second)
k8sClient.Get(ctx, namespacedName, deployment)
Expect(deployment.Status.ReadyReplicas).To(Equal(int32(1)))  // Flaky!
```

## CI/CD Integration

### GitHub Actions Workflow

```yaml
name: Test
on: [push, pull_request]

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.24'
          cache: true
      
      - name: Run unit tests
        run: make test
      
      - name: Upload coverage
        uses: codecov/codecov-action@v4
        with:
          file: ./cover.out
  
  e2e:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.24'
      
      - name: Run E2E tests
        run: make test-e2e
```

## Next Steps

### Short Term
- [ ] Add coverage threshold enforcement (80%+)
- [ ] Add test artifacts collection (logs, events) on E2E failure
- [ ] Add mutation testing (go-mutesting)

### Medium Term
- [ ] Add multi-tenancy isolation tests
- [ ] Add operator upgrade tests
- [ ] Add failure recovery tests (pod deletion, API unavailability)
- [ ] Test different Lakekeeper configurations (OpenFGA, Vault, etc.)

### Long Term
- [ ] Add scale/performance tests
- [ ] Add chaos engineering tests (Chaos Mesh)
- [ ] Test entity CRDs (Warehouse, Namespace, etc.) when implemented
- [ ] Add security/RBAC boundary tests

## Resources

- **Ginkgo Documentation**: https://onsi.github.io/ginkgo/
- **Gomega Matchers**: https://onsi.github.io/gomega/
- **envtest**: https://book.kubebuilder.io/reference/envtest
- **Testing Best Practices**: https://go.dev/doc/effective_go#testing
- **Operator Testing**: https://sdk.operatorframework.io/docs/building-operators/golang/testing/

For quick E2E test commands, see [test/e2e/README.md](../test/e2e/README.md).
