# Lakekeeper Operator

A Kubernetes operator for managing [Lakekeeper](https://docs.lakekeeper.io/) — an Apache Iceberg REST Catalog — automating the deployment, configuration, and lifecycle of Lakekeeper instances and (planned) their catalog entities.

## Commands

- `make test` — unit + integration tests (envtest, runs with `-count=1`, no cache). Runs `manifests generate fmt vet` first.
- `make test-e2e` — E2E suite against a Kind cluster (creates and tears it down). Tests are cluster-agnostic via the current kubectl context.
- `make lint` / `make lint-fix` — `golangci-lint` (config `.golangci.yml`).
- `make manifests generate` — regenerate CRDs and DeepCopy code; run after any change to API types.
- `make fmt vet` — format and vet.
- `make build` / `make run` — build the manager binary / run the controller locally against the current kubectl context.
- `make docker-build IMG=...` / `make docker-buildx IMG=...` — single- / multi-arch image build.
- `make install` / `make deploy IMG=...` — install CRDs / deploy the operator to the current cluster.
- `make helm-deploy IMG=...` / `make helm-lint` — deploy via the Helm chart / lint it (`charts/lakekeeper-operator`).
- `make build-installer IMG=...` — generate the consolidated `dist/install.yaml`.

## Code style

- Go 1.24+. Format with `gofmt` + `goimports` (enforced via `make fmt` and the `golangci-lint` formatters).
- Lint with `golangci-lint` v2 (`.golangci.yml`): `revive`, `staticcheck`, `gocyclo`, `dupl`, `lll`, `ginkgolinter`, `errcheck`, `goconst`, and more. `make lint` must be green before reporting work done.
- `revive` enforces `comment-spacings` and `import-shadowing`.

## Project conventions

Operator-specific rules that the generic skills above do not encode:

- **Scaffold with the `operator-sdk` CLI** for new APIs / controllers / webhooks — never hand-create files under `api/` or `internal/controller/`. Run `make manifests generate` after any API type change.
- **Validate, don't provision.** The operator only checks that user-provided Secrets / ConfigMaps exist; it never creates them. External dependencies (Postgres, Vault, OpenFGA) are out of scope.
- **Generic field names** in CRD specs — `database` not `postgres`, `secretStore` not `vault`.
- **Store external IDs (Lakekeeper UUIDs) in `.status`, never in `.spec`.**
- **CRD naming:** `Lakekeeper`-prefixed kinds to avoid Kubernetes keyword conflicts (`LakekeeperWarehouse`, `LakekeeperProject`, …). API domain: `k8s.lakekeeper.io`.
- **Lakekeeper Management API** calls go through the `go-lakekeeper` SDK — no raw HTTP. (Not yet wired in; confirm the current import path before adding it.)
- **Stack:** operator-sdk v1.42.2 / Kubebuilder v4, controller-runtime, Ginkgo v2 + Gomega, envtest.

<!-- team-init: managed sections -->
<!-- Safe to re-run /team-init to update this block; do not edit by hand. -->

## Discipline

The following marketplace skills are part of this team's working
agreement. Their descriptions auto-trigger on relevant phrasing;
the imperative directives below are intentional belt-and-braces team
policy and duplicate the auto-trigger on purpose.

- Actively invoke `dev-discipline:technical-writing-discipline` whenever drafting or editing project-internal or user-facing prose (READMEs, ADRs, runbooks, release notes).
- Actively invoke `dev-discipline:tdd-bdd` whenever starting implementation work that involves writing or modifying tests.
- Actively invoke `dev-discipline:e2e-testing` when designing E2E test scope or organising integration / E2E tests. Pairs with `k8s-operator-dev:e2e-testing` on operator repos (strategy layer + framework mechanics).
- Actively invoke `go-dev:go-idioms` when writing or reviewing Go code.
- Actively invoke `k8s-operator-dev:operator-architecture` for pre-implementation design decisions on CRDs, multi-tenancy, and external-dependency boundaries.
- Actively invoke `k8s-operator-dev:operator-sdk-patterns` when implementing reconcilers, finalizers, status writes, or owner references.
- Actively invoke `k8s-operator-dev:ginkgo-test-contexts` when writing Ginkgo tests for controllers — separate unit specs from envtest-backed integration specs.
- Actively invoke `k8s-operator-dev:e2e-testing` when writing real-cluster end-to-end tests against the operator.
- Actively invoke `platform-engineering:helm-charts` when authoring or modifying Helm charts shipped from this repo.
- Actively invoke `platform-engineering:ci-cd-pipelines` when authoring or modifying GitHub Actions workflows.
- Actively invoke `platform-engineering:release-automation` when changing release tooling, changelog generation, or tag-driven publish flows.
- Actively invoke `lakekeeper-knowledge:lakekeeper-concepts` whenever reasoning about Lakekeeper entities (Server / Project / Warehouse / Namespace / Role) or the Management API surface.

## Commits and branches

- **Commit messages**: Conventional Commits — `<type>(<scope>): <subject>`.
  Types: `feat`, `fix`, `docs`, `refactor`, `test`, `chore`, `ci`,
  `build`, `perf`. Scope is the package or component the commit
  primarily touches.
- **Branch names**: `user/type/name` — e.g.
  `<user>/feat/operator-finalizer`, `<user>/fix/helm-values-bug`.

## Personal vs team rules

This file (`.claude/CLAUDE.md`) carries **team** rules — checked in,
applies to every collaborator and every Claude Code session in this
repo.

**Personal** learnings — your own working preferences, shorthand
decoded for you, project-specific context only relevant to your
sessions — live in your auto-memory directory:

```
~/.claude/projects/<project-slug>/memory/
```

`<project-slug>` is a placeholder each collaborator resolves
locally — it's the absolute repo path with each `/` replaced by `-`
(e.g. `/Users/alice/code/foo` → `-Users-alice-code-foo`). List
`~/.claude/projects/` on your machine to see the actual directory.

That directory is local to your machine and never propagates via
this file. Use it for anything that's about *you*, not about the
team.

<!-- /team-init -->
