# Lakekeeper Operator Development Guidelines

## Project Overview

Kubernetes Operator for [Lakekeeper](https://docs.lakekeeper.io/) — an Apache Iceberg REST Catalog. Manages full lifecycle of Lakekeeper instances and entities.

- **Stack:** operator-sdk v1.42.2 / Kubebuilder v4, Go 1.24+, domain `k8s.lakekeeper.io`
- **Scope:** Instance deployment, entity management (Projects, Warehouses, Namespaces, Users, Roles), multi-tenancy
- **Not managed:** External dependencies (Postgres, Vault, OpenFGA) — operator receives connection details via user-provided Secrets/ConfigMaps
- **Target:** Level 5 (Autopilot) operator capability

## Agent Delegation (CRITICAL)

**Your role: ORCHESTRATOR. Never implement directly — always delegate.**

| Task | Agent |
|------|-------|
| Any code implementation, debugging, test fixes, refactoring | **engineer** |
| CRD design, API patterns, architecture decisions | **architect** |
| Code review, compliance checks | **reviewer** |
| CI/CD, Helm charts, build automation | **platform** |
| E2E test creation | **tester** |
| Read-only codebase exploration | **Explore** |
| Domain-specific docs (architecture rationale, API reference, testing strategy, ops runbooks) | relevant agent (architect / engineer / tester / platform) |
| User-facing guides, README, onboarding, cross-cutting prose | **writer** |

**When an agent produces broken output:** Do NOT fix it directly. Assess what went wrong (use Explore or read key files), identify what's already correct, then re-delegate with a richer prompt that includes the specific failure, what to preserve, and explicit constraints to prevent the same mistake.

## Key Constraints (pass to all agents)

- **Always use `operator-sdk` CLI** for new APIs/controllers/webhooks — never create files in `api/` or `controllers/` manually
- **Validate, don't create** — operator validates that user Secrets/ConfigMaps exist; never provisions them
- **Generic field names** — `database` not `postgres`, `secretStore` not `vault`
- **Use go-lakekeeper SDK** for Lakekeeper Management API calls — no raw HTTP
- **Store external IDs in status**, not spec
- **After any API change:** run `make manifests generate`

## CRD Naming

Use `Lakekeeper`-prefixed names to avoid K8s keyword conflicts: `LakekeeperWarehouse`, `LakekeeperProject`, etc. Domain: `lakekeeper.k8s.lakekeeper.io`.

## Build & Test Commands

```bash
make manifests generate   # Regenerate CRDs and DeepCopy after API changes
make fmt vet              # Format and validate
make lint                 # Run golangci-lint
make test                 # Unit/integration tests (with -count=1, no caching)
make test-e2e             # E2E on Kind cluster (creates/destroys cluster)
make run                  # Run locally against current kubectl context
make build                # Build manager binary
make install              # Install CRDs to cluster
make deploy IMG=...       # Deploy operator to cluster
```

## References

- [Lakekeeper Docs](https://docs.lakekeeper.io/docs/nightly/concepts/)
- [Lakekeeper API](https://docs.lakekeeper.io/docs/nightly/api/management/)
- [Operator SDK Tutorial](https://sdk.operatorframework.io/docs/building-operators/golang/tutorial/)
- [Kubebuilder Book](https://book.kubebuilder.io/)
