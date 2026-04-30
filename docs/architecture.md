# Architecture

> **Note**: This document is a work in progress. The operator is in early development.

## Overview

The Lakekeeper Operator is a Kubernetes operator that manages the lifecycle of [Lakekeeper](https://docs.lakekeeper.io/) instances - an Apache Iceberg REST Catalog implementation - and their associated resources.

**Operator Scope**: This operator focuses on Kubernetes-level resource management. It does NOT provision external dependencies (PostgreSQL, Vault, OpenFGA, etc.). These must be provided externally with connection details passed via Secrets.

## Lakekeeper Entity Hierarchy

Understanding the Lakekeeper data model is crucial for operator design:

```
Server (singleton UUID, auto-generated)
  └── Project (multi-tenant isolation)
      ├── Warehouse (query engine connection point + storage config)
      │   └── Namespace (hierarchical, contains tables/views)
      │       └── Tables & Views (Iceberg objects)
      ├── User (identity from external IdP)
      └── Role (permission grouping, can be nested)
```

**Lakekeeper APIs**:
- `/catalog/*` - Iceberg REST API (query engines connect here)
- `/management/*` - Lakekeeper management API (entity CRUD, permissions)

See: https://docs.lakekeeper.io/docs/nightly/concepts/

## CRD Design

### Current State

| CRD Kind | API Group | Version | Status | Purpose |
|----------|-----------|---------|--------|---------|
| `Lakekeeper` | `lakekeeper.k8s.lakekeeper.io` | v1alpha1 | ✅ Implemented | Main instance deployment and configuration |

### Planned CRDs

| CRD Kind | API Group | Version | Status | Purpose |
|----------|-----------|---------|--------|---------|
| `LakekeeperProject` | `lakekeeper.k8s.lakekeeper.io` | v1alpha1 | 📋 Planned | Project within a Lakekeeper instance |
| `LakekeeperWarehouse` | `lakekeeper.k8s.lakekeeper.io` | v1alpha1 | 📋 Planned | Warehouse configuration and lifecycle |
| `LakekeeperNamespace` | `lakekeeper.k8s.lakekeeper.io` | v1alpha1 | 📋 Planned | Namespace within a warehouse |
| `LakekeeperUser` | `lakekeeper.k8s.lakekeeper.io` | v1alpha1 | 📋 Planned | User provisioning/reference |
| `LakekeeperRole` | `lakekeeper.k8s.lakekeeper.io` | v1alpha1 | 📋 Planned | Role definitions and assignments |

**Naming Strategy**: Prefixed names (`LakekeeperWarehouse` not `Warehouse`) to avoid conflicts with common Kubernetes resource types.

## Lakekeeper CRD

### Responsibilities

The `Lakekeeper` controller manages:

1. **Deployment**: Creates and manages the Lakekeeper Deployment; environment variables are built inline from spec — no ConfigMap is created
2. **Services**: Creates the main API service (default port **8181**) and the metrics service (default port **9000**)
3. **Migrations**: Runs database migrations as standalone Kubernetes **Jobs** before the Deployment starts
4. **Secret Validation**: Validates that all `SecretKeySelector` references in the spec exist and contain the expected keys
5. **Bootstrap Detection**: Polls `GET /management/v1/info` once pods are ready to detect or trigger server bootstrap
6. **Status**: Reports deployment health, replica counts, migration job name, and bootstrap timestamp

### Owned Resources

```
Lakekeeper CR
  ├── Deployment (<name>)                    — main container: lakekeeper serve
  ├── Service (<name>)                       — main API on configured port (default 8181)
  ├── Service (<name>-metrics)               — metrics on configured port (default 9000)
  └── Job (<name>-migrate-<hash>)            — ephemeral, created per unique spec hash
```

All owned resources carry an owner reference pointing to the Lakekeeper CR, enabling automatic garbage collection when the CR is deleted. Migration Jobs additionally have a TTL of **10 minutes** after completion.

### External Dependencies

The operator validates references but does NOT create:
- **Database** (PostgreSQL): User provides Secrets with password and encryption key
- **Secret Store** (Vault KV2): User provides Secret with Vault credentials
- **Authorization** (OpenFGA): User provides Secret with OAuth client secret
- **Server TLS**: User provides Secret with certificate file

### Status Conditions

```go
type LakekeeperStatus struct {
    Conditions         []metav1.Condition `json:"conditions,omitempty"`
    BootstrappedAt     *metav1.Time       `json:"bootstrappedAt,omitempty"`
    ServerID           *string            `json:"serverID,omitempty"`
    MigrationJob       string             `json:"migrationJob,omitempty"`
    ObservedGeneration *int64             `json:"observedGeneration,omitempty"`
    Replicas           *int32             `json:"replicas,omitempty"`
    ReadyReplicas      *int32             `json:"readyReplicas,omitempty"`
}
```

**Condition Types**:

| Condition | Meaning |
|-----------|---------|
| `Ready` | Overall health — deployment ready and operational |
| `Degraded` | Unhealthy — missing secrets, failed migration, deployment unavailable, etc. |
| `Migrated` | Database migration succeeded for the current spec hash |
| `Bootstrapped` | Server has been bootstrapped (by operator or externally) |
| `ConfigWarning` | One or more configured fields are not yet implemented |

## Controller Pattern

### Reconciliation Loop

```
┌─────────────────────────────────────────────────────┐
│  Reconcile triggered (create/update/delete/watch)   │
└─────────────────┬───────────────────────────────────┘
                  │
                  ▼
┌─────────────────────────────────────────────────────┐
│  1. Fetch Lakekeeper CR                             │
│     - NotFound? → Return (resource deleted)         │
└─────────────────┬───────────────────────────────────┘
                  │
                  ▼
┌─────────────────────────────────────────────────────┐
│  2. Check DeletionTimestamp                         │
│     - Set? → Run finalizer cleanup, remove          │
│       finalizer, return                             │
└─────────────────┬───────────────────────────────────┘
                  │
                  ▼
┌─────────────────────────────────────────────────────┐
│  3. Add Finalizer (if missing)                      │
│     → Update CR, requeue                            │
└─────────────────┬───────────────────────────────────┘
                  │
                  ▼
┌─────────────────────────────────────────────────────┐
│  4. Validate Secrets                                │
│     - All SecretKeySelector refs must exist with    │
│       the specified key present                     │
│     - Failure → set Degraded, return error          │
└─────────────────┬───────────────────────────────────┘
                  │
                  ▼
┌─────────────────────────────────────────────────────┐
│  5. Check for unimplemented fields                  │
│     - Sets ConfigWarning condition if any are       │
│       configured but not yet implemented            │
└─────────────────┬───────────────────────────────────┘
                  │
                  ▼
┌─────────────────────────────────────────────────────┐
│  6. Reconcile Migration Job (hash-based)            │
│     - Compute spec hash; derive Job name            │
│     - Job missing → create Job, set Migrated=False, │
│       RequeueAfter 10s                              │
│     - Job running → set Migrated=False,             │
│       RequeueAfter 10s                              │
│     - Job failed (3 retries) → set Degraded,        │
│       halt (no requeue)                             │
│     - Job succeeded → set Migrated=True, continue  │
└─────────────────┬───────────────────────────────────┘
                  │
                  ▼
┌─────────────────────────────────────────────────────┐
│  7. Reconcile Deployment (CreateOrUpdate)           │
│     - Env vars built inline from spec               │
│     - SSL certs mounted from Secrets as volumes     │
└─────────────────┬───────────────────────────────────┘
                  │
                  ▼
┌─────────────────────────────────────────────────────┐
│  8. Reconcile Services (CreateOrUpdate)             │
│     - Main service + metrics service                │
└─────────────────┬───────────────────────────────────┘
                  │
                  ▼
┌─────────────────────────────────────────────────────┐
│  9. Update Status                                   │
│     - Replica counts from Deployment status         │
│     - Call syncBootstrapStatus if ReadyReplicas > 0 │
│     - Set Ready / Degraded conditions               │
└─────────────────┬───────────────────────────────────┘
                  │
                  ▼
┌─────────────────────────────────────────────────────┐
│  10. Return                                         │
│     - BootstrappedAt == nil?                        │
│       → RequeueAfter poll interval (default 15s)   │
│     - BootstrappedAt set? → Done (no requeue)       │
└─────────────────────────────────────────────────────┘
```

### Idempotency

All reconcile operations are idempotent:
- Migration Jobs use a spec hash in their name — same spec always resolves to the same Job name
- Deployment and Services use `CreateOrUpdate`
- Status updates are done via `r.Status().Update()` after spec changes

### Owner References

All created resources have owner references pointing to the Lakekeeper CR:
```go
ctrl.SetControllerReference(lakekeeper, deployment, r.Scheme)
```

This enables automatic garbage collection when the CR is deleted. Migration Jobs are additionally cleaned up after a 10-minute TTL (`ttlSecondsAfterFinished: 600`).

## Job-Based Migration

Database migrations run as standalone Kubernetes Jobs, not as init containers. This allows the operator to track migration outcomes independently of the Deployment lifecycle.

### Job Naming

Each Job is named using the first 8 characters of a SHA-256 hash of the migration-relevant spec fields:

```
<lakekeeper-name>-migrate-<first-8-chars-of-sha256-hash>
```

For example: `my-lk-migrate-a3f2c1d8`

### Fields That Affect the Hash (trigger a new migration Job)

| Field | Reason |
|-------|--------|
| `spec.image` | New Lakekeeper version may include new migrations |
| `spec.database.type` | Different backend type |
| `spec.database.postgres.host` | Different database instance |
| `spec.database.postgres.port` | Different database instance |
| `spec.database.postgres.database` | Different database name |
| `spec.database.postgres.user` | Different database user |
| `spec.database.postgres.readHost` | Read replica config |

### Fields That Do NOT Affect the Hash

- `spec.replicas`, `spec.resources` — don't affect database schema
- `spec.server.*` — server configuration, not database schema
- `spec.authorization.*` — authorization config, not database schema
- Secret values (passwords, encryption keys) — credential rotation is not a migration trigger
- Connection tuning (`sslMode`, `maxConnections`, `connectionTimeout`)

### Job Lifecycle

- **Backoff limit**: 3 retries before the Job is marked as permanently failed
- **TTL**: 600 seconds (10 minutes) after completion — old Jobs are preserved briefly for audit history, then garbage-collected automatically
- **Idempotency**: `lakekeeper migrate` is idempotent; running it again on an up-to-date schema is safe
- When a new spec hash requires a new Job, the old Job (different name) is left untouched until its TTL expires

### Migration Job Spec (simplified)

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: my-lk-migrate-a3f2c1d8
spec:
  backoffLimit: 3
  ttlSecondsAfterFinished: 600
  template:
    spec:
      restartPolicy: OnFailure
      containers:
      - name: migrate
        image: <spec.image>
        args: ["migrate"]
        env:
          # All Lakekeeper env vars built inline from spec
          - name: LAKEKEEPER__PG_HOST_W
            value: "db.example.com"
          - name: LAKEKEEPER__PG_PASSWORD
            valueFrom:
              secretKeyRef:
                name: db-credentials
                key: password
          # ... (other env vars)
```

## Bootstrap

After the Deployment has at least one ready pod (`ReadyReplicas > 0`), the operator polls `GET /management/v1/info` to detect whether the server has been bootstrapped.

### Detection

Bootstrap detection is **unconditional** — it runs regardless of `spec.bootstrap.enabled`. If the info endpoint reports `bootstrapped: true` (whether the operator bootstrapped the server or an external actor did), the operator:

1. Sets `status.bootstrappedAt` to the current time
2. Sets `status.serverID` to the server's UUID
3. Sets the `Bootstrapped=True` condition

Once `status.bootstrappedAt` is set, polling stops permanently.

### Automatic Bootstrap

When `spec.bootstrap.enabled: true` (default: `false`) and the server is not yet bootstrapped, the operator calls `POST /management/v1/bootstrap`. A `409 Conflict` response (another actor bootstrapped concurrently) is treated as success — the operator re-fetches info to confirm.

### Bootstrap Configuration

```yaml
spec:
  bootstrap:
    enabled: true          # Default: false
    checkInterval: "15s"   # Default: 15s — how often to poll /management/v1/info
```

### Bootstrap Poll Interval

The `checkInterval` controls how often the reconciler requeues itself to check bootstrap state. Polling ceases as soon as `status.bootstrappedAt` is set, regardless of how bootstrap was performed.

## Health Probes

Both liveness and readiness probes use the named port `"http"`, which resolves to the configured listen port (default **8181**):

```yaml
livenessProbe:
  httpGet:
    path: /health
    port: http        # resolves to spec.server.listenPort (default 8181)
  initialDelaySeconds: 30
  periodSeconds:      10
  timeoutSeconds:     5
  failureThreshold:   3

readinessProbe:
  httpGet:
    path: /health
    port: http        # resolves to spec.server.listenPort (default 8181)
  initialDelaySeconds: 10
  periodSeconds:      5
  timeoutSeconds:     3
  failureThreshold:   3
```

## Environment Variables

All Lakekeeper configuration is passed as environment variables directly in the Deployment (and migration Job) spec — no ConfigMap is created. Secret values are injected via `valueFrom.secretKeyRef`. The mapping covers:

| Spec Section | Environment Variable Prefix |
|---|---|
| `spec.server.*` | `LAKEKEEPER__` |
| `spec.database.postgres.*` | `LAKEKEEPER__PG_*` |
| `spec.secretStore.vaultKV2.*` | `LAKEKEEPER__VAULT_*` |
| `spec.authorization.*` | `LAKEKEEPER__AUTHZ_BACKEND`, `LAKEKEEPER__OPENFGA_*` |
| `spec.authentication.openID.*` | `LAKEKEEPER__OPENID_*` |
| `spec.authentication.kubernetes.*` | `LAKEKEEPER__K8S_AUTH_*` |
| `spec.idempotency.*` | `LAKEKEEPER__IDEMPOTENCY_*` |
| `spec.taskQueues.*` | `LAKEKEEPER__TASK_QUEUE_*` |
| `spec.requestLimits.*` | `LAKEKEEPER__MAX_REQUEST_*` |
| `spec.ssl.*` | `LAKEKEEPER__SSL_CERT_FILE` |

SSL certificates (`spec.ssl.certFileSecretRef`, `spec.database.postgres.sslRootCertSecretRef`) are mounted into the pod as read-only volumes from the referenced Secrets, and the corresponding env vars point to the mount paths.

## Multi-Tenancy Considerations

For SaaS deployments, consider:

### Namespace Isolation

Each customer's Lakekeeper in a separate Kubernetes namespace:
```
namespace: lakekeeper-customer-123
  ├── Lakekeeper CR (my-lk)
  ├── Secret (db-credentials)
  └── Secret (encryption-key)
```

### Resource Naming

Include a tenant identifier in resource names to avoid confusion:
```
lakekeeper-customer-123        ← Deployment + main Service
lakekeeper-customer-123-metrics ← metrics Service
lakekeeper-customer-123-migrate-<hash> ← migration Jobs
```

### RBAC

Tenant-specific ServiceAccounts and RBAC rules to restrict access.

### Deployment Automation

Script or higher-level tooling to provision per-tenant namespaces, Secrets, and Lakekeeper CRs.

## Future Enhancements

- **Secret Watching**: The controller currently does not watch referenced Secrets for changes.
  Adding a `Watches()` on referenced Secrets would trigger immediate reconciliation when a Secret
  is created, updated, or deleted — improving recovery time when secrets become available or change.
- **Webhooks**: Admission validation and defaulting
- **Entity CRDs**: Project, Warehouse, Namespace, User, Role
- **Lakekeeper API Integration**: Use [go-lakekeeper SDK](https://github.com/baptistegh/go-lakekeeper) for entity management
- **Horizontal Scaling**: Multi-replica support with leader election for management operations
- **Backup/Restore**: Automated backup of Lakekeeper metadata
- **Metrics**: Custom Prometheus metrics for operator health
- **Observability**: Structured logging, distributed tracing

## References

- [Lakekeeper Documentation](https://docs.lakekeeper.io/)
- [Lakekeeper Management API](https://docs.lakekeeper.io/docs/nightly/api/management/)
- [Operator SDK](https://sdk.operatorframework.io/)
- [Kubebuilder Book](https://book.kubebuilder.io/)
- [Operator Capability Levels](https://sdk.operatorframework.io/docs/overview/operator-capabilities/)
