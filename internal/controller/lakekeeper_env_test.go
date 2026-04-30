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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

	Context("When reconciling a resource", func() {
		ctx := context.Background()

		BeforeEach(func() {
			By("Creating the required secrets")
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
			err := k8sClient.Get(ctx, types.NamespacedName{Name: secret.Name, Namespace: secret.Namespace}, &corev1.Secret{})
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			}

			// Create vault-secret for Vault tests
			vaultSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "vault-secret",
					Namespace: "default",
				},
				StringData: map[string]string{
					"password": "vault-password",
				},
			}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: vaultSecret.Name, Namespace: vaultSecret.Namespace}, &corev1.Secret{})
			if err != nil && errors.IsNotFound(err) {
				Expect(k8sClient.Create(ctx, vaultSecret)).To(Succeed())
			}
		})

		AfterEach(func() {
			By("Cleaning up the secret")
			secret := &corev1.Secret{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "db-secret", Namespace: "default"}, secret)
			if err == nil {
				Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
			}

			By("Cleaning up the vault-secret")
			vaultSec := &corev1.Secret{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "vault-secret", Namespace: "default"}, vaultSec)
			if err == nil {
				Expect(k8sClient.Delete(ctx, vaultSec)).To(Succeed())
			}
		})

		Context("Environment Variables", func() {
			It("should correctly map database config to environment variables", func() {
				By("Creating a Lakekeeper with full postgres config")
				pgLK := &lakekeeperv1alpha1.Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pg-env",
						Namespace: "default",
					},
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Image:    "lakekeeper:latest",
						Replicas: ptr.To(int32(1)),
						Database: lakekeeperv1alpha1.DatabaseConfig{
							Type: lakekeeperv1alpha1.DatabaseTypePostgres,
							Postgres: &lakekeeperv1alpha1.PostgresConfig{
								Host:     "postgres.example.com",
								Port:     ptr.To(int32(5433)),
								Database: "test_lakekeeper",
								User:     ptr.To("testuser"),
								PasswordSecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
									Key:                  "password",
								},
								EncryptionKeySecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-secret"},
									Key:                  "encryption-key",
								},
								MaxConnections:     ptr.To(int32(20)),
								MinIdleConnections: ptr.To(int32(5)),
								ConnectionTimeout:  ptr.To("30s"),
								SSLMode:            ptr.To("verify-full"),
							},
						},
						Authorization: lakekeeperv1alpha1.AuthorizationConfig{
							Backend: lakekeeperv1alpha1.AuthzBackendAllowAll,
						},
					},
				}
				Expect(k8sClient.Create(ctx, pgLK)).To(Succeed())

				By("Reconciling the resource")
				controllerReconciler := &LakekeeperReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-pg-env",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-pg-env",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Completing the migration Job")
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-pg-env",
					Namespace: "default",
				}, pgLK)).To(Succeed())
				Expect(completeMigrationJob(ctx, pgLK, timeout)).To(Succeed())

				// Reconcile again to create deployment
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-pg-env",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying database environment variables")
				deployment := &appsv1.Deployment{}
				Eventually(func() error {
					return k8sClient.Get(ctx, types.NamespacedName{
						Name:      "test-pg-env",
						Namespace: "default",
					}, deployment)
				}, timeout, interval).Should(Succeed())

				envVars := deployment.Spec.Template.Spec.Containers[0].Env
				envMap := make(map[string]corev1.EnvVar)
				for _, env := range envVars {
					envMap[env.Name] = env
				}

				// Verify critical postgres env vars
				Expect(envMap).To(HaveKey("LAKEKEEPER__PG_HOST_W"))
				Expect(envMap["LAKEKEEPER__PG_HOST_W"].Value).To(Equal("postgres.example.com"))

				Expect(envMap).To(HaveKey("LAKEKEEPER__PG_PORT"))
				Expect(envMap["LAKEKEEPER__PG_PORT"].Value).To(Equal("5433"))

				Expect(envMap).To(HaveKey("LAKEKEEPER__PG_DATABASE"))
				Expect(envMap["LAKEKEEPER__PG_DATABASE"].Value).To(Equal("test_lakekeeper"))

				Expect(envMap).To(HaveKey("LAKEKEEPER__PG_USER"))
				Expect(envMap["LAKEKEEPER__PG_USER"].Value).To(Equal("testuser"))

				Expect(envMap).To(HaveKey("LAKEKEEPER__PG_PASSWORD"))
				Expect(envMap["LAKEKEEPER__PG_PASSWORD"].ValueFrom).NotTo(BeNil())
				Expect(envMap).To(HaveKey("LAKEKEEPER__PG_SSL_MODE"))
				Expect(envMap["LAKEKEEPER__PG_SSL_MODE"].Value).To(Equal("verify-full"))

				By("Cleaning up")
				Expect(k8sClient.Delete(ctx, pgLK)).To(Succeed())
			})

			It("should correctly map OpenFGA authorization config", func() {
				By("Creating a Lakekeeper with OpenFGA")
				fgaLK := &lakekeeperv1alpha1.Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-openfga",
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
							Backend: lakekeeperv1alpha1.AuthzBackendOpenFGA,
							OpenFGA: &lakekeeperv1alpha1.OpenFGAConfig{
								Endpoint:  "http://openfga:8080",
								StoreName: "lakekeeper",
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, fgaLK)).To(Succeed())

				By("Reconciling the resource")
				controllerReconciler := &LakekeeperReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-openfga",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-openfga",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Completing the migration Job")
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-openfga",
					Namespace: "default",
				}, fgaLK)).To(Succeed())
				Expect(completeMigrationJob(ctx, fgaLK, timeout)).To(Succeed())

				// Reconcile again to create deployment
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-openfga",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying OpenFGA environment variables")
				deployment := &appsv1.Deployment{}
				Eventually(func() error {
					return k8sClient.Get(ctx, types.NamespacedName{
						Name:      "test-openfga",
						Namespace: "default",
					}, deployment)
				}, timeout, interval).Should(Succeed())

				envVars := deployment.Spec.Template.Spec.Containers[0].Env
				envMap := make(map[string]corev1.EnvVar)
				for _, env := range envVars {
					envMap[env.Name] = env
				}

				Expect(envMap).To(HaveKey("LAKEKEEPER__AUTHZ_BACKEND"))
				Expect(envMap["LAKEKEEPER__AUTHZ_BACKEND"].Value).To(Equal("openfga"))

				Expect(envMap).To(HaveKey("LAKEKEEPER__OPENFGA_ENDPOINT"))
				Expect(envMap["LAKEKEEPER__OPENFGA_ENDPOINT"].Value).To(Equal("http://openfga:8080"))

				Expect(envMap).To(HaveKey("LAKEKEEPER__OPENFGA_STORE_NAME"))
				Expect(envMap["LAKEKEEPER__OPENFGA_STORE_NAME"].Value).To(Equal("lakekeeper"))

				By("Cleaning up")
				Expect(k8sClient.Delete(ctx, fgaLK)).To(Succeed())
			})

			It("should set all expected environment variables for full config", func() {
				By("Creating a Lakekeeper with comprehensive configuration")
				fullConfigLK := &lakekeeperv1alpha1.Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-full-config",
						Namespace: "default",
					},
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Image:    "lakekeeper:latest",
						Replicas: ptr.To(int32(1)),
						Server: &lakekeeperv1alpha1.ServerConfig{
							BaseURI:    ptr.To("https://lakekeeper.example.com"),
							ListenPort: ptr.To(int32(8080)),
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
				Expect(k8sClient.Create(ctx, fullConfigLK)).To(Succeed())

				By("Reconciling the resource")
				controllerReconciler := &LakekeeperReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-full-config",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-full-config",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Completing the migration Job")
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-full-config",
					Namespace: "default",
				}, fullConfigLK)).To(Succeed())
				Expect(completeMigrationJob(ctx, fullConfigLK, timeout)).To(Succeed())

				// Reconcile again to create deployment
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-full-config",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying presence of critical environment variables")
				deployment := &appsv1.Deployment{}
				Eventually(func() error {
					return k8sClient.Get(ctx, types.NamespacedName{
						Name:      "test-full-config",
						Namespace: "default",
					}, deployment)
				}, timeout, interval).Should(Succeed())

				envVars := deployment.Spec.Template.Spec.Containers[0].Env
				envMap := make(map[string]corev1.EnvVar)
				for _, env := range envVars {
					envMap[env.Name] = env
				}

				// Verify presence of critical env vars from different sections
				expectedEnvVars := []string{
					"LAKEKEEPER__BASE_URI",
					"LAKEKEEPER__LISTEN_PORT",
					"LAKEKEEPER__PG_HOST_W",
					"LAKEKEEPER__PG_DATABASE",
					"LAKEKEEPER__PG_PASSWORD",
					"LAKEKEEPER__PG_ENCRYPTION_KEY",
					"LAKEKEEPER__AUTHZ_BACKEND",
				}

				for _, expectedVar := range expectedEnvVars {
					Expect(envMap).To(HaveKey(expectedVar), "Expected env var %s to be present", expectedVar)
				}

				By("Cleaning up")
				Expect(k8sClient.Delete(ctx, fullConfigLK)).To(Succeed())
			})
		})

		Context("Additional Environment Variable Coverage", func() {
			It("should correctly map OpenID authentication config", func() {
				By("Creating a Lakekeeper with OpenID config")
				oidcLK := &lakekeeperv1alpha1.Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-oidc",
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
						Authentication: &lakekeeperv1alpha1.AuthenticationConfig{
							OpenID: &lakekeeperv1alpha1.OpenIDConfig{
								ProviderURI: "https://auth.example.com",
								Audience:    "lakekeeper-api",
							},
						},
						Authorization: lakekeeperv1alpha1.AuthorizationConfig{
							Backend: lakekeeperv1alpha1.AuthzBackendAllowAll,
						},
					},
				}
				Expect(k8sClient.Create(ctx, oidcLK)).To(Succeed())

				By("Reconciling the resource")
				controllerReconciler := &LakekeeperReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-oidc",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-oidc",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Completing the migration Job")
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-oidc",
					Namespace: "default",
				}, oidcLK)).To(Succeed())
				Expect(completeMigrationJob(ctx, oidcLK, timeout)).To(Succeed())

				// Reconcile again to create deployment
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-oidc",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Getting deployment and verifying env vars")
				deployment := &appsv1.Deployment{}
				Eventually(func() error {
					return k8sClient.Get(ctx, types.NamespacedName{
						Name:      "test-oidc",
						Namespace: "default",
					}, deployment)
				}, timeout, interval).Should(Succeed())

				envVars := deployment.Spec.Template.Spec.Containers[0].Env
				envMap := make(map[string]corev1.EnvVar)
				for _, env := range envVars {
					envMap[env.Name] = env
				}

				By("Verifying LAKEKEEPER__OPENID_PROVIDER_URI, LAKEKEEPER__OPENID_AUDIENCE")
				Expect(envMap).To(HaveKey("LAKEKEEPER__OPENID_PROVIDER_URI"))
				Expect(envMap["LAKEKEEPER__OPENID_PROVIDER_URI"].Value).To(Equal("https://auth.example.com"))

				Expect(envMap).To(HaveKey("LAKEKEEPER__OPENID_AUDIENCE"))
				Expect(envMap["LAKEKEEPER__OPENID_AUDIENCE"].Value).To(Equal("lakekeeper-api"))

				By("Cleaning up")
				Expect(k8sClient.Delete(ctx, oidcLK)).To(Succeed())
			})

			It("should correctly map Vault secret store config", func() {
				By("Creating a Lakekeeper with Vault KV2")
				vaultLK := &lakekeeperv1alpha1.Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-vault",
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
						SecretStore: &lakekeeperv1alpha1.SecretStoreConfig{
							Type: lakekeeperv1alpha1.SecretStoreTypeVaultKV2,
							VaultKV2: &lakekeeperv1alpha1.VaultKV2Config{
								URL:         "https://vault.example.com",
								User:        ptr.To("lakekeeper-user"),
								SecretMount: "secret",
								PasswordSecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "vault-secret"},
									Key:                  "password",
								},
							},
						},
						Authorization: lakekeeperv1alpha1.AuthorizationConfig{
							Backend: lakekeeperv1alpha1.AuthzBackendAllowAll,
						},
					},
				}
				Expect(k8sClient.Create(ctx, vaultLK)).To(Succeed())

				By("Reconciling the resource")
				controllerReconciler := &LakekeeperReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-vault",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-vault",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Completing the migration Job")
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-vault",
					Namespace: "default",
				}, vaultLK)).To(Succeed())
				Expect(completeMigrationJob(ctx, vaultLK, timeout)).To(Succeed())

				// Reconcile again to create deployment
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-vault",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Getting deployment and verifying env vars")
				deployment := &appsv1.Deployment{}
				Eventually(func() error {
					return k8sClient.Get(ctx, types.NamespacedName{
						Name:      "test-vault",
						Namespace: "default",
					}, deployment)
				}, timeout, interval).Should(Succeed())

				envVars := deployment.Spec.Template.Spec.Containers[0].Env
				envMap := make(map[string]corev1.EnvVar)
				for _, env := range envVars {
					envMap[env.Name] = env
				}

				By("Verifying LAKEKEEPER__SECRET_BACKEND=vault-kv2")
				Expect(envMap).To(HaveKey("LAKEKEEPER__SECRET_BACKEND"))
				Expect(envMap["LAKEKEEPER__SECRET_BACKEND"].Value).To(Equal("vault-kv2"))

				By("Verifying LAKEKEEPER__VAULT_URL, LAKEKEEPER__VAULT_MOUNT")
				Expect(envMap).To(HaveKey("LAKEKEEPER__VAULT_URL"))
				Expect(envMap["LAKEKEEPER__VAULT_URL"].Value).To(Equal("https://vault.example.com"))

				Expect(envMap).To(HaveKey("LAKEKEEPER__VAULT_SECRET_MOUNT"))
				Expect(envMap["LAKEKEEPER__VAULT_SECRET_MOUNT"].Value).To(Equal("secret"))

				By("Cleaning up")
				Expect(k8sClient.Delete(ctx, vaultLK)).To(Succeed())
			})
		})

		Context("Secret Validation Edge Cases", func() {
			It("should fail when secret exists but key is missing", func() {
				By("Creating a secret with wrong keys")
				wrongKeySecret := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "wrong-key-secret",
						Namespace: "default",
					},
					StringData: map[string]string{
						"wrong-password": "supersecret",
						"wrong-key":      "encryption-key-value",
					},
				}
				Expect(k8sClient.Create(ctx, wrongKeySecret)).To(Succeed())

				By("Creating Lakekeeper referencing non-existent key")
				wrongKeyLK := &lakekeeperv1alpha1.Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-wrong-key",
						Namespace: "default",
					},
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Image: "lakekeeper:latest",
						Database: lakekeeperv1alpha1.DatabaseConfig{
							Type: lakekeeperv1alpha1.DatabaseTypePostgres,
							Postgres: &lakekeeperv1alpha1.PostgresConfig{
								Host:     "postgres.default.svc",
								Database: "lakekeeper",
								User:     ptr.To("lakekeeper"),
								PasswordSecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "wrong-key-secret"},
									Key:                  "password", // Key doesn't exist
								},
								EncryptionKeySecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "wrong-key-secret"},
									Key:                  "encryption-key", // Key doesn't exist
								},
							},
						},
						Authorization: lakekeeperv1alpha1.AuthorizationConfig{
							Backend: lakekeeperv1alpha1.AuthzBackendAllowAll,
						},
					},
				}
				Expect(k8sClient.Create(ctx, wrongKeyLK)).To(Succeed())

				By("Reconciling the resource")
				controllerReconciler := &LakekeeperReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				// First reconcile - adds finalizer
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-wrong-key",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				// Second reconcile - should fail due to missing key
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-wrong-key",
						Namespace: "default",
					},
				})
				Expect(err).To(HaveOccurred())

				By("Verifying Degraded condition is set")
				lakekeeper := &lakekeeperv1alpha1.Lakekeeper{}
				Eventually(func() bool {
					err := k8sClient.Get(ctx, types.NamespacedName{
						Name:      "test-wrong-key",
						Namespace: "default",
					}, lakekeeper)
					if err != nil {
						return false
					}
					return meta.IsStatusConditionTrue(lakekeeper.Status.Conditions, TypeDegraded)
				}, timeout, interval).Should(BeTrue())

				By("Cleaning up")
				Expect(k8sClient.Delete(ctx, wrongKeyLK)).To(Succeed())
				Expect(k8sClient.Delete(ctx, wrongKeySecret)).To(Succeed())
			})

			It("should recover when missing secret becomes available", func() {
				By("Creating Lakekeeper without creating secret first")
				recoveryLK := &lakekeeperv1alpha1.Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-recovery",
						Namespace: "default",
					},
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Image: "lakekeeper:latest",
						Database: lakekeeperv1alpha1.DatabaseConfig{
							Type: lakekeeperv1alpha1.DatabaseTypePostgres,
							Postgres: &lakekeeperv1alpha1.PostgresConfig{
								Host:     "postgres.default.svc",
								Database: "lakekeeper",
								User:     ptr.To("lakekeeper"),
								PasswordSecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "late-secret"},
									Key:                  "password",
								},
								EncryptionKeySecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "late-secret"},
									Key:                  "encryption-key",
								},
							},
						},
						Authorization: lakekeeperv1alpha1.AuthorizationConfig{
							Backend: lakekeeperv1alpha1.AuthzBackendAllowAll,
						},
					},
				}
				Expect(k8sClient.Create(ctx, recoveryLK)).To(Succeed())

				By("Reconciling - should be degraded")
				controllerReconciler := &LakekeeperReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				// First reconcile - adds finalizer
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-recovery",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				// Second reconcile - should fail
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-recovery",
						Namespace: "default",
					},
				})
				Expect(err).To(HaveOccurred())

				By("Creating the missing secret")
				lateSecret := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "late-secret",
						Namespace: "default",
					},
					StringData: map[string]string{
						"password":       "supersecret",
						"encryption-key": "encryption-key-value",
					},
				}
				Expect(k8sClient.Create(ctx, lateSecret)).To(Succeed())

				By("Reconciling again - should succeed now")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-recovery",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Completing the migration Job")
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-recovery",
					Namespace: "default",
				}, recoveryLK)).To(Succeed())
				Expect(completeMigrationJob(ctx, recoveryLK, timeout)).To(Succeed())

				// Reconcile again to create deployment
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-recovery",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Updating deployment status to simulate ready pods")
				deployment := &appsv1.Deployment{}
				Eventually(func() error {
					return k8sClient.Get(ctx, types.NamespacedName{
						Name:      "test-recovery",
						Namespace: "default",
					}, deployment)
				}, timeout, interval).Should(Succeed())

				deployment.Status.Replicas = 1
				deployment.Status.ReadyReplicas = 1
				deployment.Status.UpdatedReplicas = 1
				deployment.Status.AvailableReplicas = 1
				Expect(k8sClient.Status().Update(ctx, deployment)).To(Succeed())

				By("Reconciling to update status after deployment is ready")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-recovery",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying Ready condition is set")
				lakekeeper := &lakekeeperv1alpha1.Lakekeeper{}
				Eventually(func() bool {
					err := k8sClient.Get(ctx, types.NamespacedName{
						Name:      "test-recovery",
						Namespace: "default",
					}, lakekeeper)
					if err != nil {
						return false
					}
					return meta.IsStatusConditionTrue(lakekeeper.Status.Conditions, TypeReady)
				}, timeout, interval).Should(BeTrue())

				By("Cleaning up")
				Expect(k8sClient.Delete(ctx, recoveryLK)).To(Succeed())
				Expect(k8sClient.Delete(ctx, lateSecret)).To(Succeed())
			})
		})

	})
})
