# Lakekeeper Operator

> [!WARNING]
> **Work in Progress** — This operator is under active development and is not yet production-ready. APIs, CRDs, and behavior may change without notice. Use at your own risk and expect rough edges.

A Kubernetes operator for managing [Lakekeeper](https://docs.lakekeeper.io/) - an Apache Iceberg REST Catalog implementation.

## Description

The Lakekeeper Operator automates the deployment, configuration, and lifecycle management of Lakekeeper instances on Kubernetes. It handles instance deployment, database migrations, service configuration, and health monitoring, allowing you to focus on data catalog management rather than infrastructure.

**Key Features**:
- 🚀 Automated deployment of Lakekeeper instances
- 🔄 Database migration management via Kubernetes Jobs
- 🔍 Health and metrics endpoint configuration
- 🎯 Declarative deployment and server configuration via Kubernetes CRDs
- 🛡️ Validation of external dependencies (PostgreSQL, Vault, etc.)
- 📊 Status reporting and condition management

**Operator Capability Level**: Level 2 (Seamless Upgrades) - working towards Level 5 (Autopilot)

## Technology Stack

- **Operator Framework**: operator-sdk v1.42.2 with Kubebuilder v4
- **Go Version**: 1.24.0+
- **Kubernetes**: v1.21+ (tested on v1.33.0)
- **API Domain**: `k8s.lakekeeper.io`

## Quick Start

### Prerequisites
- **Go**: v1.24.0+
- **Docker**: v17.03+
- **kubectl**: v1.21+
- **Kubernetes cluster**: v1.21+ (Kind, k3d, minikube, or cloud cluster)

### Install the Operator

**From source** (currently the only available method):
```sh
git clone https://github.com/lakekeeper/lakekeeper-operator.git
cd lakekeeper-operator
make docker-build IMG=lakekeeper-operator:dev
make install deploy IMG=lakekeeper-operator:dev
```

### Deploy a Lakekeeper Instance

```sh
# Create database credentials secret
kubectl create secret generic lakekeeper-db-credentials \
  --from-literal=password=yourpassword \
  --from-literal=encryption-key=your-32-byte-encryption-key!

# Deploy Lakekeeper instance
kubectl apply -f config/samples/lakekeeper_v1alpha1_lakekeeper.yaml
```

See [config/samples/](config/samples/) for example configurations.

## Documentation

- **[Development Guide](docs/development.md)** - Local development setup, build commands, coding guidelines
- **[Testing Guide](docs/testing.md)** - Unit tests, E2E tests, debugging
- **[Architecture](docs/architecture.md)** - Operator design, CRD hierarchy, controller patterns
- **[E2E Quick Reference](test/e2e/README.md)** - Quick commands for running E2E tests

## Development

### Build and Test Locally

```sh
# Run unit and integration tests
make test

# Run E2E tests (creates Kind cluster)
make test-e2e

# Build operator image
make docker-build IMG=lakekeeper-operator:dev

# Run locally against current cluster
make install run
```

### Deploy to Local Cluster

```sh
# Create Kind cluster
make setup-test-e2e

# Build and deploy operator
make docker-build IMG=lakekeeper-operator:dev
kind load docker-image lakekeeper-operator:dev --name lakekeeper-operator-test-e2e
make install deploy IMG=lakekeeper-operator:dev

# Create sample Lakekeeper instance
kubectl apply -f config/samples/lakekeeper_v1alpha1_lakekeeper_minimal.yaml
```

## Testing

The operator uses a three-layer testing approach:

- **Unit Tests** (`make test`) - Business logic, validation, helpers
- **Integration Tests** (envtest) - Controller reconciliation, resource creation
- **E2E Tests** (`make test-e2e`) - Full system tests on real Kubernetes clusters

See [docs/testing.md](docs/testing.md) for comprehensive testing documentation.

## Roadmap

The following capabilities are planned for future releases:

- **Catalog entity management** — Kubernetes CRDs for managing Lakekeeper entities declaratively:
  - `LakekeeperProject` — manage Projects
  - `LakekeeperWarehouse` — manage Warehouses
  - `LakekeeperNamespace` — manage Namespaces
  - `LakekeeperUser` / `LakekeeperRole` — manage Users and Roles
- **Helm chart distribution** — a Helm chart exists in this repository but is not yet published to a chart repository

## Contributing

We welcome contributions! Please see our [Development Guide](docs/development.md) for details on:
- Setting up your development environment
- Code style and conventions
- Testing requirements
- Submitting pull requests

**Commit Message Format**: Use [Conventional Commits](https://www.conventionalcommits.org/)
```
feat: add warehouse CRD and controller
fix: handle nil pointer in status update
docs: update development guide
test: add e2e test for custom ports
```

## Releasing

The operator uses semantic versioning. Distribution methods:
- **Direct YAML** _(planned)_: `kubectl apply -f https://github.com/lakekeeper/lakekeeper-operator/releases/latest/download/install.yaml`
- **Helm Chart** _(planned)_: via Helm repository
- **OLM Bundle** _(planned)_: via OperatorHub.io

Build the installer bundle manually:

```sh
# Build installer bundle
make build-installer IMG=ghcr.io/lakekeeper/lakekeeper-operator:v0.1.0

# The dist/install.yaml contains all manifests for installation
```

## Resources

- **Lakekeeper Documentation**: https://docs.lakekeeper.io/
- **Lakekeeper Management API**: https://docs.lakekeeper.io/docs/nightly/api/management/
- **Operator SDK**: https://sdk.operatorframework.io/
- **Kubebuilder Book**: https://book.kubebuilder.io/

Run `make help` for all available make targets.

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

