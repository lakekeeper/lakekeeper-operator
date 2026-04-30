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

package controller

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	lakekeeperv1alpha1 "github.com/lakekeeper/lakekeeper-operator/api/v1alpha1"
)

var _ = Describe("Lakekeeper Controller", func() {
	const (
		timeout  = time.Second * 10
		interval = time.Millisecond * 250
	)

	// Environment Variable Builder Unit Tests
	Context("Environment Variable Builders", func() {
		var reconciler *LakekeeperReconciler

		BeforeEach(func() {
			reconciler = &LakekeeperReconciler{}
		})

		Context("buildServerEnvVars", func() {
			It("should return empty when server config is nil", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Server: nil,
					},
				}

				envVars := reconciler.buildServerEnvVars(lk)
				Expect(envVars).To(BeEmpty())
			})

			It("should correctly map basic server configuration", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Server: &lakekeeperv1alpha1.ServerConfig{
							BaseURI:     ptr.To("https://lakekeeper.example.com"),
							ListenPort:  ptr.To(int32(8080)),
							MetricsPort: ptr.To(int32(9090)),
						},
					},
				}

				envVars := reconciler.buildServerEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__BASE_URI", "https://lakekeeper.example.com")
				expectEnvValue(envMap, "LAKEKEEPER__LISTEN_PORT", "8080")
				expectEnvValue(envMap, "LAKEKEEPER__METRICS__PORT", "9090")
			})

			It("should correctly format CORS origins as comma-separated list", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Server: &lakekeeperv1alpha1.ServerConfig{
							CORS: &lakekeeperv1alpha1.CORSConfig{
								AllowOrigins: []string{"https://example.com", "https://app.example.com", "https://admin.example.com"},
							},
						},
					},
				}

				envVars := reconciler.buildServerEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__CORS_ALLOW_ORIGINS", "https://example.com,https://app.example.com,https://admin.example.com")
			})

			It("should handle empty CORS origins", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Server: &lakekeeperv1alpha1.ServerConfig{
							CORS: &lakekeeperv1alpha1.CORSConfig{
								AllowOrigins: []string{},
							},
						},
					},
				}

				envVars := reconciler.buildServerEnvVars(lk)
				envMap := toEnvMap(envVars)

				_, exists := envMap["LAKEKEEPER__CORS_ALLOW_ORIGINS"]
				Expect(exists).To(BeFalse())
			})

			It("should correctly format reserved namespaces as comma-separated list", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Server: &lakekeeperv1alpha1.ServerConfig{
							ReservedNamespaces: []string{"system", "default", "reserved"},
						},
					},
				}

				envVars := reconciler.buildServerEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__RESERVED_NAMESPACES", "system,default,reserved")
			})

			It("should map pagination configuration", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Server: &lakekeeperv1alpha1.ServerConfig{
							Pagination: &lakekeeperv1alpha1.PaginationConfig{
								DefaultSize: ptr.To(int32(25)),
								MaxSize:     ptr.To(int32(500)),
							},
						},
					},
				}

				envVars := reconciler.buildServerEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__PAGE_SIZE_DEFAULT", "25")
				expectEnvValue(envMap, "LAKEKEEPER__PAGE_SIZE_MAX", "500")
			})

			It("should map enable default project flag", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Server: &lakekeeperv1alpha1.ServerConfig{
							EnableDefaultProject: ptr.To(true),
						},
					},
				}

				envVars := reconciler.buildServerEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__ENABLE_DEFAULT_PROJECT", "true")
			})

			It("should map AWS storage credentials configuration", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Server: &lakekeeperv1alpha1.ServerConfig{
							StorageSystemCredentials: &lakekeeperv1alpha1.StorageSystemCredentialsConfig{
								EnableAWS: ptr.To(true),
								AWS: &lakekeeperv1alpha1.AWSConfig{
									AssumeRoleRequireExternalID:  ptr.To(true),
									AllowDirectSystemCredentials: ptr.To(false),
								},
							},
						},
					},
				}

				envVars := reconciler.buildServerEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__STORAGE_CREDENTIAL_BACKEND__AWS__ENABLED", "true")
				expectEnvValue(envMap, "LAKEKEEPER__STORAGE_CREDENTIAL_BACKEND__AWS__ASSUME_ROLE_REQUIRE_EXTERNAL_ID", "true")
				expectEnvValue(envMap, "LAKEKEEPER__STORAGE_CREDENTIAL_BACKEND__AWS__ALLOW_DIRECT_SYSTEM_CREDENTIALS", "false")
			})

			It("should map Azure and GCP storage credentials", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Server: &lakekeeperv1alpha1.ServerConfig{
							StorageSystemCredentials: &lakekeeperv1alpha1.StorageSystemCredentialsConfig{
								EnableAzure: ptr.To(true),
								EnableGCP:   ptr.To(true),
							},
						},
					},
				}

				envVars := reconciler.buildServerEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__STORAGE_CREDENTIAL_BACKEND__AZURE__ENABLED", "true")
				expectEnvValue(envMap, "LAKEKEEPER__STORAGE_CREDENTIAL_BACKEND__GCP__ENABLED", "true")
			})
		})

		Context("buildDatabaseEnvVars", func() {
			It("should return empty for non-postgres database type", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Database: lakekeeperv1alpha1.DatabaseConfig{
							Type:     lakekeeperv1alpha1.DatabaseTypePostgres,
							Postgres: nil, // This would fail validation, but testing nil case
						},
					},
				}

				envVars := reconciler.buildDatabaseEnvVars(lk)
				Expect(envVars).To(BeEmpty())
			})

			It("should correctly map postgres configuration with plain user", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Database: lakekeeperv1alpha1.DatabaseConfig{
							Type: lakekeeperv1alpha1.DatabaseTypePostgres,
							Postgres: &lakekeeperv1alpha1.PostgresConfig{
								Host:     "postgres.example.com",
								Port:     ptr.To(int32(5433)),
								Database: "lakekeeper_db",
								User:     ptr.To("dbuser"),
								PasswordSecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
									Key:                  "password",
								},
								EncryptionKeySecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
									Key:                  "encryption-key",
								},
								SSLMode: ptr.To("verify-full"),
							},
						},
					},
				}

				envVars := reconciler.buildDatabaseEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__PG_HOST_W", "postgres.example.com")
				expectEnvValue(envMap, "LAKEKEEPER__PG_PORT", "5433")
				expectEnvValue(envMap, "LAKEKEEPER__PG_DATABASE", "lakekeeper_db")
				expectEnvValue(envMap, "LAKEKEEPER__PG_USER", "dbuser")
				expectEnvValue(envMap, "LAKEKEEPER__PG_SSL_MODE", "verify-full")

				// Verify password from secret
				expectEnvSecret(envMap, "LAKEKEEPER__PG_PASSWORD", "db-secret", "password")
			})
			It("should use default port 5432 when not specified", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Database: lakekeeperv1alpha1.DatabaseConfig{
							Type: lakekeeperv1alpha1.DatabaseTypePostgres,
							Postgres: &lakekeeperv1alpha1.PostgresConfig{
								Host:     "postgres.example.com",
								Database: "lakekeeper",
								PasswordSecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
									Key:                  "password",
								},
								EncryptionKeySecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
									Key:                  "encryption-key",
								},
							},
						},
					},
				}

				envVars := reconciler.buildDatabaseEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__PG_PORT", "5432")
			})

			It("should use default SSL mode 'require' when not specified", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Database: lakekeeperv1alpha1.DatabaseConfig{
							Type: lakekeeperv1alpha1.DatabaseTypePostgres,
							Postgres: &lakekeeperv1alpha1.PostgresConfig{
								Host:     "postgres.example.com",
								Database: "lakekeeper",
								PasswordSecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
									Key:                  "password",
								},
								EncryptionKeySecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
									Key:                  "encryption-key",
								},
							},
						},
					},
				}

				envVars := reconciler.buildDatabaseEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__PG_SSL_MODE", "require")
			})

			It("should handle user from secret reference", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Database: lakekeeperv1alpha1.DatabaseConfig{
							Type: lakekeeperv1alpha1.DatabaseTypePostgres,
							Postgres: &lakekeeperv1alpha1.PostgresConfig{
								Host:     "postgres.example.com",
								Database: "lakekeeper",
								UserSecretRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
									Key:                  "username",
								},
								PasswordSecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
									Key:                  "password",
								},
								EncryptionKeySecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
									Key:                  "encryption-key",
								},
							},
						},
					},
				}

				envVars := reconciler.buildDatabaseEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvSecret(envMap, "LAKEKEEPER__PG_USER", "db-secret", "username")
			})

			It("should handle read replica configuration", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Database: lakekeeperv1alpha1.DatabaseConfig{
							Type: lakekeeperv1alpha1.DatabaseTypePostgres,
							Postgres: &lakekeeperv1alpha1.PostgresConfig{
								Host:     "postgres-primary.example.com",
								Database: "lakekeeper",
								PasswordSecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
									Key:                  "password",
								},
								EncryptionKeySecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
									Key:                  "encryption-key",
								},
								ReadHost: ptr.To("postgres-replica.example.com"),
							},
						},
					},
				}

				envVars := reconciler.buildDatabaseEnvVars(lk)
				envMap := toEnvMap(envVars)

				// Only host has read/write variants, port is shared
				expectEnvValue(envMap, "LAKEKEEPER__PG_HOST_R", "postgres-replica.example.com")
			})

			It("should map connection pool settings", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Database: lakekeeperv1alpha1.DatabaseConfig{
							Type: lakekeeperv1alpha1.DatabaseTypePostgres,
							Postgres: &lakekeeperv1alpha1.PostgresConfig{
								Host:     "postgres.example.com",
								Database: "lakekeeper",
								PasswordSecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
									Key:                  "password",
								},
								EncryptionKeySecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
									Key:                  "encryption-key",
								},
								MaxConnections:     ptr.To(int32(50)),
								MinIdleConnections: ptr.To(int32(10)),
								ConnectionTimeout:  ptr.To("30s"),
							},
						},
					},
				}

				envVars := reconciler.buildDatabaseEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__PG_MAX_CONNECTIONS", "50")
				expectEnvValue(envMap, "LAKEKEEPER__PG_MIN_IDLE_CONNECTIONS", "10")
				expectEnvValue(envMap, "LAKEKEEPER__PG_CONNECTION_TIMEOUT", "30s")
			})

			It("should map SSL root certificate", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Database: lakekeeperv1alpha1.DatabaseConfig{
							Type: lakekeeperv1alpha1.DatabaseTypePostgres,
							Postgres: &lakekeeperv1alpha1.PostgresConfig{
								Host:     "postgres.example.com",
								Database: "lakekeeper",
								PasswordSecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
									Key:                  "password",
								},
								EncryptionKeySecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
									Key:                  "encryption-key",
								},
								SSLRootCertSecretRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-ssl-secret"},
									Key:                  "ca.crt",
								},
							},
						},
					},
				}

				envVars := reconciler.buildDatabaseEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__PG_SSL_ROOT_CERT", "/etc/lakekeeper/ssl/pg-root-cert/ca.crt")
			})
		})

		Context("buildSecretStoreEnvVars", func() {
			It("should return empty when secret store is nil", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						SecretStore: nil,
					},
				}

				envVars := reconciler.buildSecretStoreEnvVars(lk)
				Expect(envVars).To(BeEmpty())
			})

			It("should correctly map Vault KV2 configuration with plain user", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						SecretStore: &lakekeeperv1alpha1.SecretStoreConfig{
							Type: lakekeeperv1alpha1.SecretStoreTypeVaultKV2,
							VaultKV2: &lakekeeperv1alpha1.VaultKV2Config{
								URL:         "https://vault.example.com:8200",
								User:        ptr.To("vault-user"),
								SecretMount: "lakekeeper",
								PasswordSecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "vault-secret"},
									Key:                  "password",
								},
							},
						},
					},
				}

				envVars := reconciler.buildSecretStoreEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__SECRET_BACKEND", "vault-kv2")
				expectEnvValue(envMap, "LAKEKEEPER__VAULT_URL", "https://vault.example.com:8200")
				expectEnvValue(envMap, "LAKEKEEPER__VAULT_USER", "vault-user")
				expectEnvValue(envMap, "LAKEKEEPER__VAULT_SECRET_MOUNT", "lakekeeper")
				expectEnvSecret(envMap, "LAKEKEEPER__VAULT_PASSWORD", "vault-secret", "password")
			})

			It("should handle Vault user from secret reference", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						SecretStore: &lakekeeperv1alpha1.SecretStoreConfig{
							Type: lakekeeperv1alpha1.SecretStoreTypeVaultKV2,
							VaultKV2: &lakekeeperv1alpha1.VaultKV2Config{
								URL: "https://vault.example.com:8200",
								UserSecretRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "vault-secret"},
									Key:                  "username",
								},
								SecretMount: "lakekeeper",
								PasswordSecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "vault-secret"},
									Key:                  "password",
								},
							},
						},
					},
				}

				envVars := reconciler.buildSecretStoreEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvSecret(envMap, "LAKEKEEPER__VAULT_USER", "vault-secret", "username")
			})
		})

		Context("buildAuthorizationEnvVars", func() {
			It("should map allowall backend", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Authorization: lakekeeperv1alpha1.AuthorizationConfig{
							Backend: lakekeeperv1alpha1.AuthzBackendAllowAll,
						},
					},
				}

				envVars := reconciler.buildAuthorizationEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__AUTHZ_BACKEND", "allowall")
			})

			It("should correctly map OpenFGA configuration", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Authorization: lakekeeperv1alpha1.AuthorizationConfig{
							Backend: lakekeeperv1alpha1.AuthzBackendOpenFGA,
							OpenFGA: &lakekeeperv1alpha1.OpenFGAConfig{
								Endpoint:                  "http://openfga:8081",
								StoreName:                 "lakekeeper",
								AuthorizationModelVersion: ptr.To("v1.13"),
								ClientID:                  ptr.To("lakekeeper-client"),
								ClientSecretRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "openfga-secret"},
									Key:                  "client-secret",
								},
							},
						},
					},
				}

				envVars := reconciler.buildAuthorizationEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__AUTHZ_BACKEND", "openfga")
				expectEnvValue(envMap, "LAKEKEEPER__OPENFGA_ENDPOINT", "http://openfga:8081")
				expectEnvValue(envMap, "LAKEKEEPER__OPENFGA_STORE_NAME", "lakekeeper")
				expectEnvValue(envMap, "LAKEKEEPER__OPENFGA_AUTHORIZATION_MODEL_VERSION", "v1.13")
				expectEnvValue(envMap, "LAKEKEEPER__OPENFGA_CLIENT_ID", "lakekeeper-client")
				expectEnvSecret(envMap, "LAKEKEEPER__OPENFGA_CLIENT_SECRET", "openfga-secret", "client-secret")
			})

			It("should handle OpenFGA without optional fields", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Authorization: lakekeeperv1alpha1.AuthorizationConfig{
							Backend: lakekeeperv1alpha1.AuthzBackendOpenFGA,
							OpenFGA: &lakekeeperv1alpha1.OpenFGAConfig{
								Endpoint:  "http://openfga:8081",
								StoreName: "lakekeeper",
							},
						},
					},
				}

				envVars := reconciler.buildAuthorizationEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__AUTHZ_BACKEND", "openfga")
				expectEnvValue(envMap, "LAKEKEEPER__OPENFGA_ENDPOINT", "http://openfga:8081")
				expectEnvValue(envMap, "LAKEKEEPER__OPENFGA_STORE_NAME", "lakekeeper")

				// Verify optional fields are not present
				_, exists := envMap["LAKEKEEPER__OPENFGA_AUTHORIZATION_MODEL_VERSION"]
				Expect(exists).To(BeFalse())
				_, exists = envMap["LAKEKEEPER__OPENFGA_CLIENT_ID"]
				Expect(exists).To(BeFalse())
				_, exists = envMap["LAKEKEEPER__OPENFGA_CLIENT_SECRET"]
				Expect(exists).To(BeFalse())
			})
		})

		Context("buildAuthenticationEnvVars", func() {
			It("should return empty when authentication is nil", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Authentication: nil,
					},
				}

				envVars := reconciler.buildAuthenticationEnvVars(lk)
				Expect(envVars).To(BeEmpty())
			})

			It("should correctly map OpenID configuration", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Authentication: &lakekeeperv1alpha1.AuthenticationConfig{
							OpenID: &lakekeeperv1alpha1.OpenIDConfig{
								ProviderURI:  "https://auth.example.com",
								Audience:     "lakekeeper",
								SubjectClaim: ptr.To("sub"),
								RolesClaim:   ptr.To("roles"),
								Scopes:       []string{"openid", "profile", "email"},
							},
						},
					},
				}

				envVars := reconciler.buildAuthenticationEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__OPENID_PROVIDER_URI", "https://auth.example.com")
				expectEnvValue(envMap, "LAKEKEEPER__OPENID_AUDIENCE", "lakekeeper")
				expectEnvValue(envMap, "LAKEKEEPER__OPENID_SUBJECT_CLAIM", "sub")
				expectEnvValue(envMap, "LAKEKEEPER__OPENID_ROLES_CLAIM", "roles")
				expectEnvValue(envMap, "LAKEKEEPER__OPENID_SCOPES", "openid,profile,email")
			})

			It("should handle OpenID without optional fields", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Authentication: &lakekeeperv1alpha1.AuthenticationConfig{
							OpenID: &lakekeeperv1alpha1.OpenIDConfig{
								ProviderURI: "https://auth.example.com",
								Audience:    "lakekeeper",
							},
						},
					},
				}

				envVars := reconciler.buildAuthenticationEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__OPENID_PROVIDER_URI", "https://auth.example.com")
				expectEnvValue(envMap, "LAKEKEEPER__OPENID_AUDIENCE", "lakekeeper")

				// Verify optional fields are not present
				_, exists := envMap["LAKEKEEPER__OPENID_SUBJECT_CLAIM"]
				Expect(exists).To(BeFalse())
				_, exists = envMap["LAKEKEEPER__OPENID_ROLES_CLAIM"]
				Expect(exists).To(BeFalse())
				_, exists = envMap["LAKEKEEPER__OPENID_SCOPES"]
				Expect(exists).To(BeFalse())
			})

			It("should correctly map Kubernetes authentication configuration", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Authentication: &lakekeeperv1alpha1.AuthenticationConfig{
							Kubernetes: &lakekeeperv1alpha1.KubernetesAuthConfig{
								Enabled:            true,
								Audiences:          []string{"lakekeeper", "api"},
								AcceptLegacyTokens: ptr.To(false),
							},
						},
					},
				}

				envVars := reconciler.buildAuthenticationEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__K8S_AUTH_ENABLED", "true")
				expectEnvValue(envMap, "LAKEKEEPER__K8S_AUTH_AUDIENCES", "lakekeeper,api")
				expectEnvValue(envMap, "LAKEKEEPER__K8S_AUTH_ACCEPT_LEGACY_TOKENS", "false")
			})

			It("should handle both OpenID and Kubernetes auth together", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Authentication: &lakekeeperv1alpha1.AuthenticationConfig{
							OpenID: &lakekeeperv1alpha1.OpenIDConfig{
								ProviderURI: "https://auth.example.com",
								Audience:    "lakekeeper",
							},
							Kubernetes: &lakekeeperv1alpha1.KubernetesAuthConfig{
								Enabled: true,
							},
						},
					},
				}

				envVars := reconciler.buildAuthenticationEnvVars(lk)
				envMap := toEnvMap(envVars)

				// Both should be present
				expectEnvValue(envMap, "LAKEKEEPER__OPENID_PROVIDER_URI", "https://auth.example.com")
				expectEnvValue(envMap, "LAKEKEEPER__K8S_AUTH_ENABLED", "true")
			})
		})

		Context("buildIdempotencyEnvVars", func() {
			It("should return empty when idempotency is nil", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Idempotency: nil,
					},
				}

				envVars := reconciler.buildIdempotencyEnvVars(lk)
				Expect(envVars).To(BeEmpty())
			})

			It("should correctly map idempotency configuration", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Idempotency: &lakekeeperv1alpha1.IdempotencyConfig{
							Enabled:        ptr.To(true),
							Lifetime:       ptr.To("24h"),
							GracePeriod:    ptr.To("1h"),
							CleanupTimeout: ptr.To("30m"),
						},
					},
				}

				envVars := reconciler.buildIdempotencyEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__IDEMPOTENCY_ENABLED", "true")
				expectEnvValue(envMap, "LAKEKEEPER__IDEMPOTENCY_LIFETIME", "24h")
				expectEnvValue(envMap, "LAKEKEEPER__IDEMPOTENCY_GRACE_PERIOD", "1h")
				expectEnvValue(envMap, "LAKEKEEPER__IDEMPOTENCY_CLEANUP_TIMEOUT", "30m")
			})

			It("should handle partial idempotency configuration", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Idempotency: &lakekeeperv1alpha1.IdempotencyConfig{
							Enabled: ptr.To(false),
						},
					},
				}

				envVars := reconciler.buildIdempotencyEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__IDEMPOTENCY_ENABLED", "false")

				// Verify other fields are not present
				_, exists := envMap["LAKEKEEPER__IDEMPOTENCY_LIFETIME"]
				Expect(exists).To(BeFalse())
			})
		})

		Context("buildTaskQueuesEnvVars", func() {
			It("should return empty when task queues is nil", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						TaskQueues: nil,
					},
				}

				envVars := reconciler.buildTaskQueuesEnvVars(lk)
				Expect(envVars).To(BeEmpty())
			})

			It("should correctly map task queue configuration", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						TaskQueues: &lakekeeperv1alpha1.TaskQueuesConfig{
							PollInterval:             ptr.To("5s"),
							TabularExpirationWorkers: ptr.To(int32(4)),
							TabularPurgeWorkers:      ptr.To(int32(2)),
							ExpireSnapshotsWorkers:   ptr.To(int32(3)),
						},
					},
				}

				envVars := reconciler.buildTaskQueuesEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__TASK_QUEUE_POLL_INTERVAL", "5s")
				expectEnvValue(envMap, "LAKEKEEPER__TASK_QUEUE_TABULAR_EXPIRATION_WORKERS", "4")
				expectEnvValue(envMap, "LAKEKEEPER__TASK_QUEUE_TABULAR_PURGE_WORKERS", "2")
				expectEnvValue(envMap, "LAKEKEEPER__TASK_QUEUE_EXPIRE_SNAPSHOTS_WORKERS", "3")
			})

			It("should handle partial task queue configuration", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						TaskQueues: &lakekeeperv1alpha1.TaskQueuesConfig{
							PollInterval: ptr.To("10s"),
						},
					},
				}

				envVars := reconciler.buildTaskQueuesEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__TASK_QUEUE_POLL_INTERVAL", "10s")

				// Verify optional worker fields are not present
				_, exists := envMap["LAKEKEEPER__TASK_QUEUE_TABULAR_EXPIRATION_WORKERS"]
				Expect(exists).To(BeFalse())
			})
		})

		Context("buildRequestLimitsEnvVars", func() {
			It("should return empty when request limits is nil", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						RequestLimits: nil,
					},
				}

				envVars := reconciler.buildRequestLimitsEnvVars(lk)
				Expect(envVars).To(BeEmpty())
			})

			It("should correctly map request limits configuration", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						RequestLimits: &lakekeeperv1alpha1.RequestLimitsConfig{
							MaxRequestBodySize: ptr.To("10MB"),
							MaxRequestTime:     ptr.To("30s"),
						},
					},
				}

				envVars := reconciler.buildRequestLimitsEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__MAX_REQUEST_BODY_SIZE", "10MB")
				expectEnvValue(envMap, "LAKEKEEPER__MAX_REQUEST_TIME", "30s")
			})

			It("should handle partial request limits configuration", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						RequestLimits: &lakekeeperv1alpha1.RequestLimitsConfig{
							MaxRequestBodySize: ptr.To("5MB"),
						},
					},
				}

				envVars := reconciler.buildRequestLimitsEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__MAX_REQUEST_BODY_SIZE", "5MB")

				// Verify optional field is not present
				_, exists := envMap["LAKEKEEPER__MAX_REQUEST_TIME"]
				Expect(exists).To(BeFalse())
			})
		})

		Context("buildSSLEnvVars", func() {
			It("should return empty when SSL is nil", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						SSL: nil,
					},
				}

				envVars := reconciler.buildSSLEnvVars(lk)
				Expect(envVars).To(BeEmpty())
			})

			It("should correctly map SSL certificate reference", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						SSL: &lakekeeperv1alpha1.SSLConfig{
							CertFileSecretRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: "ssl-secret"},
								Key:                  "tls.crt",
							},
						},
					},
				}

				envVars := reconciler.buildSSLEnvVars(lk)
				envMap := toEnvMap(envVars)

				expectEnvValue(envMap, "LAKEKEEPER__SSL_CERT_FILE", "/etc/lakekeeper/ssl/lakekeeper-ssl-cert/tls.crt")
			})

			It("should return empty when SSL cert reference is nil", func() {
				lk := &lakekeeperv1alpha1.Lakekeeper{
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						SSL: &lakekeeperv1alpha1.SSLConfig{
							CertFileSecretRef: nil,
						},
					},
				}

				envVars := reconciler.buildSSLEnvVars(lk)
				Expect(envVars).To(BeEmpty())
			})
		})
	})

	Context("Migration Job container resources", func() {
		ctx := context.Background()

		BeforeEach(func() {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "db-secret",
					Namespace: "default",
				},
				StringData: map[string]string{
					"password":       "supersecret",
					"encryption-key": "encryption-key-value",
				},
			}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "db-secret", Namespace: "default"}, &corev1.Secret{})
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			}
		})

		AfterEach(func() {
			secret := &corev1.Secret{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "db-secret", Namespace: "default"}, secret)
			if err == nil {
				Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
			}
		})

		It("should propagate spec.resources to the migration Job container", func() {
			By("Creating a Lakekeeper with resource requirements")
			lkWithResources := &lakekeeperv1alpha1.Lakekeeper{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-migrate-resources",
					Namespace: "default",
				},
				Spec: lakekeeperv1alpha1.LakekeeperSpec{
					Image:    "lakekeeper:latest",
					Replicas: ptr.To(int32(1)),
					Resources: &corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
					Database: lakekeeperv1alpha1.DatabaseConfig{
						Type: lakekeeperv1alpha1.DatabaseTypePostgres,
						Postgres: &lakekeeperv1alpha1.PostgresConfig{
							Host:     "postgres.default.svc",
							Database: "lakekeeper",
							User:     ptr.To("lakekeeper"),
							PasswordSecretRef: corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
								Key:                  "password",
							},
							EncryptionKeySecretRef: corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
								Key:                  "encryption-key",
							},
						},
					},
					Authorization: lakekeeperv1alpha1.AuthorizationConfig{
						Backend: lakekeeperv1alpha1.AuthzBackendAllowAll,
					},
				},
			}
			Expect(k8sClient.Create(ctx, lkWithResources)).To(Succeed())

			controllerReconciler := &LakekeeperReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// First reconcile - adds finalizer
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "test-migrate-resources", Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile - creates migration Job
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "test-migrate-resources", Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the migration Job container inherits resource requirements")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-migrate-resources", Namespace: "default"}, lkWithResources)).To(Succeed())
			specHash := HashMigrationRelevantSpec(lkWithResources.Spec)
			jobName := fmt.Sprintf("%s-migrate-%s", lkWithResources.Name, specHash[:8])

			job := &batchv1.Job{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: "default"}, job)
			}, timeout, interval).Should(Succeed())

			Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))
			container := job.Spec.Template.Spec.Containers[0]
			Expect(container.Resources.Requests).NotTo(BeNil())
			Expect(container.Resources.Limits).NotTo(BeNil())
			Expect(container.Resources.Requests[corev1.ResourceCPU]).To(Equal(resource.MustParse("100m")))
			Expect(container.Resources.Requests[corev1.ResourceMemory]).To(Equal(resource.MustParse("128Mi")))
			Expect(container.Resources.Limits[corev1.ResourceCPU]).To(Equal(resource.MustParse("500m")))
			Expect(container.Resources.Limits[corev1.ResourceMemory]).To(Equal(resource.MustParse("512Mi")))

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, lkWithResources)).To(Succeed())
		})

		It("should have empty resources in the migration Job container when spec.resources is nil", func() {
			By("Creating a Lakekeeper without resource requirements")
			lkNoResources := &lakekeeperv1alpha1.Lakekeeper{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-migrate-no-resources",
					Namespace: "default",
				},
				Spec: lakekeeperv1alpha1.LakekeeperSpec{
					Image:    "lakekeeper:latest",
					Replicas: ptr.To(int32(1)),
					Database: lakekeeperv1alpha1.DatabaseConfig{
						Type: lakekeeperv1alpha1.DatabaseTypePostgres,
						Postgres: &lakekeeperv1alpha1.PostgresConfig{
							Host:     "postgres.default.svc",
							Database: "lakekeeper",
							User:     ptr.To("lakekeeper"),
							PasswordSecretRef: corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
								Key:                  "password",
							},
							EncryptionKeySecretRef: corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
								Key:                  "encryption-key",
							},
						},
					},
					Authorization: lakekeeperv1alpha1.AuthorizationConfig{
						Backend: lakekeeperv1alpha1.AuthzBackendAllowAll,
					},
				},
			}
			Expect(k8sClient.Create(ctx, lkNoResources)).To(Succeed())

			controllerReconciler := &LakekeeperReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			nn := types.NamespacedName{Name: "test-migrate-no-resources", Namespace: "default"}

			// First reconcile - adds finalizer
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile - creates migration Job
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the migration Job container has no resource requirements")
			Expect(k8sClient.Get(ctx, nn, lkNoResources)).To(Succeed())
			Expect(lkNoResources.Spec.Resources).To(BeNil())

			specHash := HashMigrationRelevantSpec(lkNoResources.Spec)
			jobName := fmt.Sprintf("%s-migrate-%s", lkNoResources.Name, specHash[:8])

			job := &batchv1.Job{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: "default"}, job)
			}, timeout, interval).Should(Succeed())

			Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))
			container := job.Spec.Template.Spec.Containers[0]
			Expect(container.Resources.Requests).To(BeNil())
			Expect(container.Resources.Limits).To(BeNil())

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, lkNoResources)).To(Succeed())
		})
	})

	Context("Scale-to-zero (spec.replicas: 0)", func() {
		ctx := context.Background()

		BeforeEach(func() {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "db-secret",
					Namespace: "default",
				},
				StringData: map[string]string{
					"password":       "supersecret",
					"encryption-key": "encryption-key-value",
				},
			}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "db-secret", Namespace: "default"}, &corev1.Secret{})
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			}
		})

		AfterEach(func() {
			secret := &corev1.Secret{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "db-secret", Namespace: "default"}, secret)
			if err == nil {
				Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
			}
		})

		It("should set Ready=True/ScaledToZero and Degraded=False/Healthy when replicas is 0", func() {
			By("Creating a Lakekeeper with zero replicas")
			scaledToZeroLK := &lakekeeperv1alpha1.Lakekeeper{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-scaled-to-zero",
					Namespace: "default",
				},
				Spec: lakekeeperv1alpha1.LakekeeperSpec{
					Image:    "lakekeeper:latest",
					Replicas: ptr.To(int32(0)),
					Database: lakekeeperv1alpha1.DatabaseConfig{
						Type: lakekeeperv1alpha1.DatabaseTypePostgres,
						Postgres: &lakekeeperv1alpha1.PostgresConfig{
							Host:     "postgres.default.svc",
							Database: "lakekeeper",
							User:     ptr.To("lakekeeper"),
							PasswordSecretRef: corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
								Key:                  "password",
							},
							EncryptionKeySecretRef: corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
								Key:                  "encryption-key",
							},
						},
					},
					Authorization: lakekeeperv1alpha1.AuthorizationConfig{
						Backend: lakekeeperv1alpha1.AuthzBackendAllowAll,
					},
				},
			}
			Expect(k8sClient.Create(ctx, scaledToZeroLK)).To(Succeed())

			controllerReconciler := &LakekeeperReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// First reconcile - adds finalizer
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "test-scaled-to-zero", Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile - creates migration Job
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "test-scaled-to-zero", Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Completing the migration Job")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-scaled-to-zero", Namespace: "default"}, scaledToZeroLK)).To(Succeed())
			Expect(completeMigrationJob(ctx, scaledToZeroLK, timeout)).To(Succeed())

			// Third reconcile - processes migration, creates deployment, updates status
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "test-scaled-to-zero", Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the Ready condition is True/ScaledToZero")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-scaled-to-zero", Namespace: "default"}, scaledToZeroLK)).To(Succeed())
			readyCond := meta.FindStatusCondition(scaledToZeroLK.Status.Conditions, TypeReady)
			Expect(readyCond).NotTo(BeNil())
			Expect(readyCond.Status).To(Equal(metav1.ConditionTrue))
			Expect(readyCond.Reason).To(Equal("ScaledToZero"))

			By("Verifying the Degraded condition is False/Healthy")
			degradedCond := meta.FindStatusCondition(scaledToZeroLK.Status.Conditions, TypeDegraded)
			Expect(degradedCond).NotTo(BeNil())
			Expect(degradedCond.Status).To(Equal(metav1.ConditionFalse))
			Expect(degradedCond.Reason).To(Equal("Healthy"))

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, scaledToZeroLK)).To(Succeed())
		})

		It("should not requeue for bootstrap when replicas is 0 and BootstrappedAt is nil", func() {
			By("Creating a Lakekeeper with zero replicas")
			zeroReplicaLK := &lakekeeperv1alpha1.Lakekeeper{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-zero-norequeue",
					Namespace: "default",
				},
				Spec: lakekeeperv1alpha1.LakekeeperSpec{
					Image:    "lakekeeper:latest",
					Replicas: ptr.To(int32(0)),
					Database: lakekeeperv1alpha1.DatabaseConfig{
						Type: lakekeeperv1alpha1.DatabaseTypePostgres,
						Postgres: &lakekeeperv1alpha1.PostgresConfig{
							Host:     "postgres.default.svc",
							Database: "lakekeeper",
							User:     ptr.To("lakekeeper"),
							PasswordSecretRef: corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
								Key:                  "password",
							},
							EncryptionKeySecretRef: corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
								Key:                  "encryption-key",
							},
						},
					},
					Authorization: lakekeeperv1alpha1.AuthorizationConfig{
						Backend: lakekeeperv1alpha1.AuthzBackendAllowAll,
					},
				},
			}
			Expect(k8sClient.Create(ctx, zeroReplicaLK)).To(Succeed())

			controllerReconciler := &LakekeeperReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Run through finalizer and migration Job creation
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "test-zero-norequeue", Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "test-zero-norequeue", Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Completing the migration Job")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "test-zero-norequeue", Namespace: "default"}, zeroReplicaLK)).To(Succeed())
			Expect(completeMigrationJob(ctx, zeroReplicaLK, timeout)).To(Succeed())

			// Final reconcile — must NOT requeue when replicas==0 and BootstrappedAt==nil
			result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: "test-zero-norequeue", Namespace: "default"},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue).To(BeFalse(), "should not set Requeue=true when scaled to zero") //nolint:staticcheck
			Expect(result.RequeueAfter).To(BeZero(), "should not requeue for bootstrap when scaled to zero")

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, zeroReplicaLK)).To(Succeed())
		})
	})
})
