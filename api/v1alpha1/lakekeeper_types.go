/*
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
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// DatabaseType defines the type of database backend.
// +kubebuilder:validation:Enum=postgres
type DatabaseType string

const (
	// DatabaseTypePostgres represents PostgreSQL database backend.
	DatabaseTypePostgres DatabaseType = "postgres"
)

// SecretStoreType defines the type of secret store backend.
// +kubebuilder:validation:Enum=postgres;vault-kv2
type SecretStoreType string

const (
	// SecretStoreTypePostgres uses database encryption for secrets (default).
	SecretStoreTypePostgres SecretStoreType = "postgres"
	// SecretStoreTypeVaultKV2 uses HashiCorp Vault KV2 backend.
	SecretStoreTypeVaultKV2 SecretStoreType = "vault-kv2"
)

// AuthzBackend defines the authorization backend type.
// Values match Lakekeeper's LAKEKEEPER__AUTHZ_BACKEND environment variable.
// See: https://docs.lakekeeper.io/docs/nightly/configuration/#authorization
// +kubebuilder:validation:Enum=allowall;openfga;cedar
type AuthzBackend string

const (
	// AuthzBackendAllowAll permits all operations (development only).
	// Corresponds to LAKEKEEPER__AUTHZ_BACKEND=allowall
	AuthzBackendAllowAll AuthzBackend = "allowall"
	// AuthzBackendOpenFGA uses OpenFGA for authorization.
	// Corresponds to LAKEKEEPER__AUTHZ_BACKEND=openfga
	AuthzBackendOpenFGA AuthzBackend = "openfga"
	// AuthzBackendCedar uses AWS Cedar for authorization (LAKEKEEPER+ only).
	// Corresponds to LAKEKEEPER__AUTHZ_BACKEND=cedar
	AuthzBackendCedar AuthzBackend = "cedar"
)

// LakekeeperSpec defines the desired state of Lakekeeper.
type LakekeeperSpec struct {
	// Image specifies the Lakekeeper container image.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Replicas is the number of Lakekeeper pods to run.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Resources defines CPU/memory resource requirements for Lakekeeper pods.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// Database configures the database backend for Lakekeeper.
	// +kubebuilder:validation:Required
	Database DatabaseConfig `json:"database"`

	// Server configures the Lakekeeper server behavior.
	// +optional
	Server *ServerConfig `json:"server,omitempty"`

	// SecretStore configures where Lakekeeper stores sensitive credentials.
	// Defaults to postgres (database encryption) if not specified.
	// +optional
	SecretStore *SecretStoreConfig `json:"secretStore,omitempty"`

	// Authentication configures identity verification methods.
	// At least one authentication method is required unless using allowall authorization.
	// +optional
	Authentication *AuthenticationConfig `json:"authentication,omitempty"`

	// Authorization configures access control.
	// +kubebuilder:validation:Required
	Authorization AuthorizationConfig `json:"authorization"`

	// Idempotency configures request idempotency handling.
	// +optional
	Idempotency *IdempotencyConfig `json:"idempotency,omitempty"`

	// TaskQueues configures background task processing.
	// +optional
	TaskQueues *TaskQueuesConfig `json:"taskQueues,omitempty"`

	// RequestLimits configures HTTP request size and timeout limits.
	// +optional
	RequestLimits *RequestLimitsConfig `json:"requestLimits,omitempty"`

	// SSL configures TLS/SSL certificates for the server.
	// +optional
	SSL *SSLConfig `json:"ssl,omitempty"`

	// Bootstrap configures automatic server initialization.
	// +optional
	Bootstrap *BootstrapConfig `json:"bootstrap,omitempty"`
}

// DatabaseConfig configures the database connection.
// +kubebuilder:validation:XValidation:rule="self.type == 'postgres' ? has(self.postgres) : true",message="postgres configuration is required when type is postgres"
type DatabaseConfig struct {
	// Type specifies the database backend type.
	// Currently only postgres is supported.
	// +kubebuilder:validation:Required
	Type DatabaseType `json:"type"`

	// Postgres contains PostgreSQL-specific configuration.
	// Required when type is postgres.
	// +optional
	Postgres *PostgresConfig `json:"postgres,omitempty"`
}

// PostgresConfig contains PostgreSQL connection details.
// +kubebuilder:validation:XValidation:rule="(has(self.user) && !has(self.userSecretRef)) || (!has(self.user) && has(self.userSecretRef))",message="exactly one of user or userSecretRef must be specified"
type PostgresConfig struct {
	// Host is the PostgreSQL server hostname or IP.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// Port is the PostgreSQL server port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=5432
	// +optional
	Port *int32 `json:"port,omitempty"`

	// Database is the PostgreSQL database name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Database string `json:"database"`

	// User is the PostgreSQL username as a plain string.
	// Note: This value is stored unencrypted in etcd. For sensitive usernames,
	// use userSecretRef instead. Exactly one of user or userSecretRef must be specified.
	// +optional
	User *string `json:"user,omitempty"`

	// UserSecretRef references a secret containing the PostgreSQL username.
	// Use this for sensitive usernames that should not be stored in plain text.
	// Exactly one of user or userSecretRef must be specified.
	// +optional
	UserSecretRef *corev1.SecretKeySelector `json:"userSecretRef,omitempty"`

	// PasswordSecretRef references a secret containing the PostgreSQL password.
	// +kubebuilder:validation:Required
	PasswordSecretRef corev1.SecretKeySelector `json:"passwordSecretRef"`

	// EncryptionKeySecretRef references a secret containing the database encryption key.
	// This is required for encrypting sensitive data at rest in PostgreSQL.
	// +kubebuilder:validation:Required
	EncryptionKeySecretRef corev1.SecretKeySelector `json:"encryptionKeySecretRef"`

	// ReadHost is the PostgreSQL read replica hostname (optional).
	// +optional
	ReadHost *string `json:"readHost,omitempty"`

	// MaxConnections is the maximum number of database connections in the pool.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxConnections *int32 `json:"maxConnections,omitempty"`

	// MinIdleConnections is the minimum number of idle connections to maintain.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinIdleConnections *int32 `json:"minIdleConnections,omitempty"`

	// ConnectionTimeout is the database connection timeout (e.g., "30s", "1m").
	// +kubebuilder:validation:Pattern=`^\d+[smh]$`
	// +optional
	ConnectionTimeout *string `json:"connectionTimeout,omitempty"`

	// SSLMode specifies the PostgreSQL SSL connection mode.
	// Defaults to "require" (TLS enforced, no certificate verification).
	// Use "verify-ca" or "verify-full" for stricter certificate validation.
	// Only set to "prefer", "allow", or "disable" in development environments without TLS.
	// +kubebuilder:validation:Enum=disable;allow;prefer;require;verify-ca;verify-full
	// +kubebuilder:default=require
	// +optional
	SSLMode *string `json:"sslMode,omitempty"`

	// SSLRootCertSecretRef references a secret containing the SSL root certificate.
	// +optional
	SSLRootCertSecretRef *corev1.SecretKeySelector `json:"sslRootCertSecretRef,omitempty"`
}

// ServerConfig configures the Lakekeeper server behavior.
type ServerConfig struct {
	// BaseURI is the base URL where Lakekeeper is accessible.
	// Used for generating OAuth redirects and links.
	// +kubebuilder:validation:Pattern=`^https?://[a-zA-Z0-9.-]+(:[0-9]+)?(/.*)?$`
	// +optional
	BaseURI *string `json:"baseURI,omitempty"`

	// ListenPort is the port for the main REST API (catalog + management).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=8181
	// +optional
	ListenPort *int32 `json:"listenPort,omitempty"`

	// MetricsPort is the port for Prometheus metrics.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=9000
	// +optional
	MetricsPort *int32 `json:"metricsPort,omitempty"`

	// EnableDefaultProject creates a default project on first startup.
	// +kubebuilder:default=true
	// +optional
	EnableDefaultProject *bool `json:"enableDefaultProject,omitempty"`

	// Pagination configures default pagination behavior.
	// +optional
	Pagination *PaginationConfig `json:"pagination,omitempty"`

	// CORS configures Cross-Origin Resource Sharing.
	// +optional
	CORS *CORSConfig `json:"cors,omitempty"`

	// ReservedNamespaces is a list of namespace names that cannot be created by users.
	// +optional
	ReservedNamespaces []string `json:"reservedNamespaces,omitempty"`

	// StorageSystemCredentials configures cloud provider credential handling.
	// +optional
	StorageSystemCredentials *StorageSystemCredentialsConfig `json:"storageSystemCredentials,omitempty"`
}

// PaginationConfig defines pagination defaults.
type PaginationConfig struct {
	// DefaultSize is the default page size when not specified.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=25
	// +optional
	DefaultSize *int32 `json:"defaultSize,omitempty"`

	// MaxSize is the maximum allowed page size.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=500
	// +optional
	MaxSize *int32 `json:"maxSize,omitempty"`
}

// CORSConfig defines CORS policy.
type CORSConfig struct {
	// AllowOrigins is a list of allowed origin domains.
	// Use ["*"] to allow all origins (not recommended for production).
	// +optional
	AllowOrigins []string `json:"allowOrigins,omitempty"`

	// AllowHeaders is a list of allowed request headers.
	// +optional
	AllowHeaders []string `json:"allowHeaders,omitempty"`

	// AllowMethods is a list of allowed HTTP methods.
	// +optional
	AllowMethods []string `json:"allowMethods,omitempty"`
}

// StorageSystemCredentialsConfig configures cloud provider credential mechanisms.
type StorageSystemCredentialsConfig struct {
	// EnableAWS enables AWS credential handling.
	// +optional
	EnableAWS *bool `json:"enableAWS,omitempty"`

	// AWS contains AWS-specific credential configuration.
	// Only used if enableAWS is true.
	// +optional
	AWS *AWSConfig `json:"aws,omitempty"`

	// EnableAzure enables Azure credential handling.
	// +optional
	EnableAzure *bool `json:"enableAzure,omitempty"`

	// Azure contains Azure-specific credential configuration.
	// Only used if enableAzure is true.
	// +optional
	Azure *AzureConfig `json:"azure,omitempty"`

	// EnableGCP enables GCP credential handling.
	// +optional
	EnableGCP *bool `json:"enableGCP,omitempty"`

	// GCP contains GCP-specific credential configuration.
	// Only used if enableGCP is true.
	// +optional
	GCP *GCPConfig `json:"gcp,omitempty"`
}

// AWSConfig contains AWS credential configuration.
type AWSConfig struct {
	// AssumeRoleRequireExternalID requires external ID for IAM role assumption.
	// Enhances security by preventing the confused deputy problem.
	// +kubebuilder:default=false
	// +optional
	AssumeRoleRequireExternalID *bool `json:"assumeRoleRequireExternalID,omitempty"`

	// AllowDirectSystemCredentials allows using IAM instance profile or environment credentials.
	// +kubebuilder:default=true
	// +optional
	AllowDirectSystemCredentials *bool `json:"allowDirectSystemCredentials,omitempty"`
}

// AzureConfig contains Azure credential configuration.
// Note: Field names are based on Azure Workload Identity patterns.
// Verify against Lakekeeper documentation before use in production.
type AzureConfig struct {
	// AllowManagedIdentity allows using Azure Managed Identity for authentication.
	// +kubebuilder:default=true
	// +optional
	AllowManagedIdentity *bool `json:"allowManagedIdentity,omitempty"`

	// AllowWorkloadIdentity allows using Azure Workload Identity (federated identity).
	// +kubebuilder:default=true
	// +optional
	AllowWorkloadIdentity *bool `json:"allowWorkloadIdentity,omitempty"`
}

// GCPConfig contains GCP credential configuration.
// Note: Field names are based on GCP Workload Identity patterns.
// Verify against Lakekeeper documentation before use in production.
type GCPConfig struct {
	// AllowWorkloadIdentity allows using GCP Workload Identity for authentication.
	// +kubebuilder:default=true
	// +optional
	AllowWorkloadIdentity *bool `json:"allowWorkloadIdentity,omitempty"`

	// AllowServiceAccountKey allows using service account key files for authentication.
	// +kubebuilder:default=false
	// +optional
	AllowServiceAccountKey *bool `json:"allowServiceAccountKey,omitempty"`
}

// SecretStoreConfig configures where Lakekeeper stores secrets.
// +kubebuilder:validation:XValidation:rule="self.type == 'vault-kv2' ? has(self.vaultKV2) : true",message="vaultKV2 configuration is required when type is vault-kv2"
type SecretStoreConfig struct {
	// Type specifies the secret store backend.
	// Defaults to postgres if not specified.
	// +kubebuilder:validation:Enum=postgres;vault-kv2
	// +kubebuilder:default=postgres
	// +optional
	Type SecretStoreType `json:"type,omitempty"`

	// VaultKV2 contains HashiCorp Vault KV2 configuration.
	// Required when type is vault-kv2.
	// +optional
	VaultKV2 *VaultKV2Config `json:"vaultKV2,omitempty"`
}

// VaultKV2Config contains Vault KV2 backend configuration.
// +kubebuilder:validation:XValidation:rule="(has(self.user) && !has(self.userSecretRef)) || (!has(self.user) && has(self.userSecretRef))",message="exactly one of user or userSecretRef must be specified"
type VaultKV2Config struct {
	// URL is the Vault server address.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https?://[a-zA-Z0-9.-]+(:[0-9]+)?$`
	URL string `json:"url"`

	// User is the Vault username as a plain string.
	// Note: This value is stored unencrypted in etcd. For sensitive usernames,
	// use userSecretRef instead. Exactly one of user or userSecretRef must be specified.
	// +optional
	User *string `json:"user,omitempty"`

	// UserSecretRef references a secret containing the Vault username.
	// Use this for sensitive usernames that should not be stored in plain text.
	// Exactly one of user or userSecretRef must be specified.
	// +optional
	UserSecretRef *corev1.SecretKeySelector `json:"userSecretRef,omitempty"`

	// PasswordSecretRef references a secret containing the Vault password.
	// +kubebuilder:validation:Required
	PasswordSecretRef corev1.SecretKeySelector `json:"passwordSecretRef"`

	// SecretMount is the Vault KV2 mount path.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	SecretMount string `json:"secretMount"`
}

// AuthenticationConfig configures identity verification.
type AuthenticationConfig struct {
	// OpenID configures OpenID Connect authentication.
	// +optional
	OpenID *OpenIDConfig `json:"openID,omitempty"`

	// Kubernetes configures Kubernetes ServiceAccount token authentication.
	// +optional
	Kubernetes *KubernetesAuthConfig `json:"kubernetes,omitempty"`
}

// OpenIDConfig contains OpenID Connect configuration.
type OpenIDConfig struct {
	// ProviderURI is the OpenID Connect provider's issuer URL.
	// Must use HTTPS for security (HTTP is not supported).
	// Example: https://login.microsoftonline.com/<tenant-id>/v2.0
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https://[a-zA-Z0-9.-]+(:[0-9]+)?(/.*)?$`
	ProviderURI string `json:"providerURI"`

	// Audience is the expected audience claim in JWT tokens.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Audience string `json:"audience"`

	// SubjectClaim specifies the JWT claim to use as the user identifier.
	// +kubebuilder:default="sub"
	// +optional
	SubjectClaim *string `json:"subjectClaim,omitempty"`

	// RolesClaim specifies the JWT claim containing user roles.
	// +kubebuilder:default="roles"
	// +optional
	RolesClaim *string `json:"rolesClaim,omitempty"`

	// Scopes is a list of OAuth scopes to request.
	// +optional
	Scopes []string `json:"scopes,omitempty"`
}

// KubernetesAuthConfig contains Kubernetes authentication configuration.
type KubernetesAuthConfig struct {
	// Enabled enables Kubernetes ServiceAccount token authentication.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled"`

	// Audiences is a list of acceptable token audiences.
	// +optional
	Audiences []string `json:"audiences,omitempty"`

	// AcceptLegacyTokens allows authentication with legacy (non-bound) tokens.
	// Should be false in production for security.
	// +kubebuilder:default=false
	// +optional
	AcceptLegacyTokens *bool `json:"acceptLegacyTokens,omitempty"`
}

// AuthorizationConfig configures access control.
// +kubebuilder:validation:XValidation:rule="self.backend == 'openfga' ? has(self.openFGA) : true",message="openFGA configuration is required when backend is openfga"
// +kubebuilder:validation:XValidation:rule="self.backend == 'cedar' ? has(self.cedar) : true",message="cedar configuration is required when backend is cedar"
type AuthorizationConfig struct {
	// Backend specifies the authorization backend.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=allowall;openfga;cedar
	Backend AuthzBackend `json:"backend"`

	// OpenFGA contains OpenFGA authorization configuration.
	// Required when backend is openfga.
	// +optional
	OpenFGA *OpenFGAConfig `json:"openFGA,omitempty"`

	// Cedar contains Cedar authorization configuration.
	// Required when backend is cedar (LAKEKEEPER+ only).
	// +optional
	Cedar *CedarConfig `json:"cedar,omitempty"`
}

// OpenFGAConfig contains OpenFGA configuration.
type OpenFGAConfig struct {
	// Endpoint is the OpenFGA API endpoint.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https?://[a-zA-Z0-9.-]+(:[0-9]+)?$`
	Endpoint string `json:"endpoint"`

	// StoreName is the OpenFGA store name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	StoreName string `json:"storeName"`

	// AuthorizationModelVersion specifies the OpenFGA model version to use.
	// +optional
	AuthorizationModelVersion *string `json:"authorizationModelVersion,omitempty"`

	// ClientID is the OAuth client ID for OpenFGA authentication.
	// +optional
	ClientID *string `json:"clientID,omitempty"`

	// ClientSecretRef references a secret containing the OAuth client secret.
	// +optional
	ClientSecretRef *corev1.SecretKeySelector `json:"clientSecretRef,omitempty"`
}

// CedarConfig contains Cedar authorization configuration.
type CedarConfig struct {
	// PolicySource specifies where Cedar policies are loaded from.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	PolicySource string `json:"policySource"`

	// EntitySource specifies where Cedar entities are loaded from.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	EntitySource string `json:"entitySource"`

	// UserDerivations configures how user entities are derived from authentication.
	// LAKEKEEPER+ only.
	// +optional
	UserDerivations map[string]string `json:"userDerivations,omitempty"`
}

// IdempotencyConfig configures request idempotency handling.
type IdempotencyConfig struct {
	// Enabled enables idempotency key tracking.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Lifetime is the duration an idempotency key remains valid (ISO-8601 format).
	// Examples: "PT30M" (30 minutes), "PT1H" (1 hour), "P1D" (1 day).
	// +kubebuilder:validation:Pattern=`^P(?:\d+Y)?(?:\d+M)?(?:\d+D)?(?:T(?:\d+H)?(?:\d+M)?(?:\d+(?:\.\d+)?S)?)?$`
	// +kubebuilder:default="PT30M"
	// +optional
	Lifetime *string `json:"lifetime,omitempty"`

	// GracePeriod is the window after lifetime for accepting retries (ISO-8601 format).
	// +kubebuilder:validation:Pattern=`^P(?:\d+Y)?(?:\d+M)?(?:\d+D)?(?:T(?:\d+H)?(?:\d+M)?(?:\d+(?:\.\d+)?S)?)?$`
	// +optional
	GracePeriod *string `json:"gracePeriod,omitempty"`

	// CleanupTimeout is how long to wait before cleaning up expired keys (ISO-8601 format).
	// +kubebuilder:validation:Pattern=`^P(?:\d+Y)?(?:\d+M)?(?:\d+D)?(?:T(?:\d+H)?(?:\d+M)?(?:\d+(?:\.\d+)?S)?)?$`
	// +optional
	CleanupTimeout *string `json:"cleanupTimeout,omitempty"`
}

// TaskQueuesConfig configures background task processing.
type TaskQueuesConfig struct {
	// PollInterval is how often to poll for tasks (e.g., "5s", "500ms").
	// +kubebuilder:validation:Pattern=`^\d+(ms|s)$`
	// +optional
	PollInterval *string `json:"pollInterval,omitempty"`

	// TabularExpirationWorkers is the number of workers for table expiration tasks.
	// +kubebuilder:validation:Minimum=1
	// +optional
	TabularExpirationWorkers *int32 `json:"tabularExpirationWorkers,omitempty"`

	// TabularPurgeWorkers is the number of workers for table purge tasks.
	// +kubebuilder:validation:Minimum=1
	// +optional
	TabularPurgeWorkers *int32 `json:"tabularPurgeWorkers,omitempty"`

	// ExpireSnapshotsWorkers is the number of workers for snapshot expiration (LAKEKEEPER+ only).
	// +kubebuilder:validation:Minimum=1
	// +optional
	ExpireSnapshotsWorkers *int32 `json:"expireSnapshotsWorkers,omitempty"`
}

// RequestLimitsConfig configures HTTP request limits.
type RequestLimitsConfig struct {
	// MaxRequestBodySize is the maximum request body size (e.g., "10MB", "1GB").
	// +kubebuilder:validation:Pattern=`^\d+[KMGT]?B$`
	// +optional
	MaxRequestBodySize *string `json:"maxRequestBodySize,omitempty"`

	// MaxRequestTime is the maximum time to process a request (e.g., "30s", "5m").
	// +kubebuilder:validation:Pattern=`^\d+[smh]$`
	// +optional
	MaxRequestTime *string `json:"maxRequestTime,omitempty"`
}

// BootstrapConfig configures automatic Lakekeeper server initialization.
type BootstrapConfig struct {
	// Enabled controls whether the operator automatically bootstraps the Lakekeeper
	// server when it first becomes ready. Defaults to false (nil is treated as false).
	// When true, the operator will POST to /management/v1/bootstrap after the
	// deployment is ready, setting the initial administrator.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// CheckInterval controls how often the operator polls /management/v1/info to detect
	// bootstrap state. This applies regardless of whether the operator performs the
	// bootstrap itself (spec.bootstrap.enabled=true) or merely observes it (e.g. allowall
	// mode or manual bootstrap). Polling stops permanently once bootstrap is confirmed
	// (BootstrappedAt is set).
	// Must be a valid Go duration string (e.g. "15s", "1m", "2m30s").
	// +kubebuilder:default="15s"
	// +kubebuilder:validation:Pattern=`^\d+(\.\d+)?(ns|us|µs|ms|s|m|h)(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))*$`
	// +optional
	CheckInterval string `json:"checkInterval,omitempty"`
}

// SSLConfig configures TLS/SSL certificates.
type SSLConfig struct {
	// CertFileSecretRef references a secret containing the TLS certificate file.
	// +optional
	CertFileSecretRef *corev1.SecretKeySelector `json:"certFileSecretRef,omitempty"`

	// CertDirConfigMapRef references a ConfigMap containing a directory of certificates.
	// +optional
	CertDirConfigMapRef *corev1.ConfigMapKeySelector `json:"certDirConfigMapRef,omitempty"`
}

// LakekeeperStatus defines the observed state of Lakekeeper.
type LakekeeperStatus struct {
	// Conditions represent the latest available observations of the Lakekeeper's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// BootstrappedAt is the time when the Lakekeeper server was successfully bootstrapped.
	// +optional
	BootstrappedAt *metav1.Time `json:"bootstrappedAt,omitempty"`

	// ServerID is the Lakekeeper server's unique UUID, set after a successful bootstrap.
	// +optional
	ServerID *string `json:"serverID,omitempty"`

	// MigrationJob is the name of the current migration Job.
	// Tracks which migration Job corresponds to the current spec.
	// +optional
	MigrationJob string `json:"migrationJob,omitempty"`

	// ObservedGeneration reflects the generation most recently observed by the controller.
	// +optional
	ObservedGeneration *int64 `json:"observedGeneration,omitempty"`

	// Replicas is the actual number of pods.
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// ReadyReplicas is the number of ready pods.
	// +optional
	ReadyReplicas *int32 `json:"readyReplicas,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=lk
// +kubebuilder:printcolumn:name="Image",type="string",JSONPath=".spec.image",description="Lakekeeper container image"
// +kubebuilder:printcolumn:name="Replicas",type="integer",JSONPath=".spec.replicas",description="Desired replicas"
// +kubebuilder:printcolumn:name="Ready",type="integer",JSONPath=".status.readyReplicas",description="Ready replicas"
// +kubebuilder:printcolumn:name="Bootstrapped",type="date",JSONPath=".status.bootstrappedAt",description="Bootstrap timestamp"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// Lakekeeper is the Schema for the lakekeepers API.
type Lakekeeper struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   LakekeeperSpec   `json:"spec,omitempty"`
	Status LakekeeperStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// LakekeeperList contains a list of Lakekeeper.
type LakekeeperList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Lakekeeper `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Lakekeeper{}, &LakekeeperList{})
}
