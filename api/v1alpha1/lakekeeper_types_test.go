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
	"context"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var (
	ctx       context.Context
	cancel    context.CancelFunc
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
)

const (
	// testDBUser is the default database username used in tests
	testDBUser = "lakekeeper"
)

func TestV1Alpha1Types(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "API v1alpha1 Types Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.TODO())

	var err error
	err = AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	// Retrieve the first found binary directory to allow running tests from IDEs
	if getFirstFoundEnvTestBinaryDir() != "" {
		testEnv.BinaryAssetsDirectory = getFirstFoundEnvTestBinaryDir()
	}

	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())
})

var _ = AfterSuite(func() {
	By("tearing down the test environment")
	cancel()
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})

func getFirstFoundEnvTestBinaryDir() string {
	basePath := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		logf.Log.Error(err, "Failed to read directory", "path", basePath)
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}

var _ = Describe("Lakekeeper Types", func() {
	// Integration tests that verify CRD validation rules
	Describe("CRD Validation", func() {
		var namespace string

		BeforeEach(func() {
			namespace = "default"
		})

		AfterEach(func() {
			// Cleanup any created resources
			lkList := &LakekeeperList{}
			_ = k8sClient.List(ctx, lkList, client.InNamespace(namespace))
			for _, lk := range lkList.Items {
				_ = k8sClient.Delete(ctx, &lk)
			}
		})

		Context("PostgresConfig User/UserSecretRef Mutual Exclusivity", func() {
			It("should reject Lakekeeper with both user and userSecretRef", func() {
				userStr := "testuser"
				lk := &Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "invalid-both-user",
						Namespace: namespace,
					},
					Spec: LakekeeperSpec{
						Image: "quay.io/lakekeeper/catalog:latest",
						Database: DatabaseConfig{
							Type: DatabaseTypePostgres,
							Postgres: &PostgresConfig{
								Host:     "postgres.default.svc",
								Database: "lakekeeper",
								User:     &userStr,
								UserSecretRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"},
									Key:                  "username",
								},
								PasswordSecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"},
									Key:                  "password",
								},
								EncryptionKeySecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"},
									Key:                  "encryption-key",
								},
							},
						},
						Authorization: AuthorizationConfig{
							Backend: AuthzBackendAllowAll,
						},
					},
				}

				err := k8sClient.Create(ctx, lk)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("exactly one of user or userSecretRef"))
			})

			It("should reject Lakekeeper with neither user nor userSecretRef", func() {
				lk := &Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "invalid-no-user",
						Namespace: namespace,
					},
					Spec: LakekeeperSpec{
						Image: "quay.io/lakekeeper/catalog:latest",
						Database: DatabaseConfig{
							Type: DatabaseTypePostgres,
							Postgres: &PostgresConfig{
								Host:     "postgres.default.svc",
								Database: "lakekeeper",
								// Both user and userSecretRef missing
								PasswordSecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"},
									Key:                  "password",
								},
								EncryptionKeySecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"},
									Key:                  "encryption-key",
								},
							},
						},
						Authorization: AuthorizationConfig{
							Backend: AuthzBackendAllowAll,
						},
					},
				}

				err := k8sClient.Create(ctx, lk)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("exactly one of user or userSecretRef"))
			})

			It("should accept valid Lakekeeper with user as plain string", func() {
				userStr := "lakekeeper"
				lk := &Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "valid-plain-user",
						Namespace: namespace,
					},
					Spec: LakekeeperSpec{
						Image: "quay.io/lakekeeper/catalog:latest",
						Database: DatabaseConfig{
							Type: DatabaseTypePostgres,
							Postgres: &PostgresConfig{
								Host:     "postgres.default.svc",
								Database: "lakekeeper",
								User:     &userStr,
								PasswordSecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"},
									Key:                  "password",
								},
								EncryptionKeySecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"},
									Key:                  "encryption-key",
								},
							},
						},
						Authorization: AuthorizationConfig{
							Backend: AuthzBackendAllowAll,
						},
					},
				}

				err := k8sClient.Create(ctx, lk)
				Expect(err).NotTo(HaveOccurred())

				// Cleanup
				Expect(k8sClient.Delete(ctx, lk)).To(Succeed())
			})

			It("should accept valid Lakekeeper with userSecretRef", func() {
				lk := &Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "valid-secret-user",
						Namespace: namespace,
					},
					Spec: LakekeeperSpec{
						Image: "quay.io/lakekeeper/catalog:latest",
						Database: DatabaseConfig{
							Type: DatabaseTypePostgres,
							Postgres: &PostgresConfig{
								Host:     "postgres.default.svc",
								Database: "lakekeeper",
								UserSecretRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"},
									Key:                  "username",
								},
								PasswordSecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"},
									Key:                  "password",
								},
								EncryptionKeySecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"},
									Key:                  "encryption-key",
								},
							},
						},
						Authorization: AuthorizationConfig{
							Backend: AuthzBackendAllowAll,
						},
					},
				}

				err := k8sClient.Create(ctx, lk)
				Expect(err).NotTo(HaveOccurred())

				// Cleanup
				Expect(k8sClient.Delete(ctx, lk)).To(Succeed())
			})
		})

		Context("DatabaseConfig Conditional Requirements", func() {
			It("should reject database config without postgres when type is postgres", func() {
				lk := &Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "invalid-no-postgres-config",
						Namespace: namespace,
					},
					Spec: LakekeeperSpec{
						Image: "quay.io/lakekeeper/catalog:latest",
						Database: DatabaseConfig{
							Type: DatabaseTypePostgres,
							// Postgres config missing!
						},
						Authorization: AuthorizationConfig{
							Backend: AuthzBackendAllowAll,
						},
					},
				}

				err := k8sClient.Create(ctx, lk)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("postgres configuration is required"))
			})
		})

		Context("SecretStoreConfig Conditional Requirements", func() {
			It("should reject secret store config without vaultKV2 when type is vault-kv2", func() {
				userStr := testDBUser
				lk := &Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "invalid-no-vault-config",
						Namespace: namespace,
					},
					Spec: LakekeeperSpec{
						Image: "quay.io/lakekeeper/catalog:latest",
						Database: DatabaseConfig{
							Type: DatabaseTypePostgres,
							Postgres: &PostgresConfig{
								Host:     "postgres.default.svc",
								Database: "lakekeeper",
								User:     &userStr,
								PasswordSecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"},
									Key:                  "password",
								},
								EncryptionKeySecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"},
									Key:                  "encryption-key",
								},
							},
						},
						SecretStore: &SecretStoreConfig{
							Type: SecretStoreTypeVaultKV2,
							// VaultKV2 config missing!
						},
						Authorization: AuthorizationConfig{
							Backend: AuthzBackendAllowAll,
						},
					},
				}

				err := k8sClient.Create(ctx, lk)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("vaultKV2 configuration is required"))
			})
		})

		Context("AuthorizationConfig Conditional Requirements", func() {
			It("should reject authorization without openFGA when backend is openfga", func() {
				userStr := testDBUser
				lk := &Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "invalid-no-openfga-config",
						Namespace: namespace,
					},
					Spec: LakekeeperSpec{
						Image: "quay.io/lakekeeper/catalog:latest",
						Database: DatabaseConfig{
							Type: DatabaseTypePostgres,
							Postgres: &PostgresConfig{
								Host:     "postgres.default.svc",
								Database: "lakekeeper",
								User:     &userStr,
								PasswordSecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"},
									Key:                  "password",
								},
								EncryptionKeySecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"},
									Key:                  "encryption-key",
								},
							},
						},
						Authorization: AuthorizationConfig{
							Backend: AuthzBackendOpenFGA,
							// OpenFGA config missing!
						},
					},
				}

				err := k8sClient.Create(ctx, lk)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("openFGA configuration is required"))
			})

			It("should reject authorization without cedar when backend is cedar", func() {
				userStr := "lakekeeper"
				lk := &Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "invalid-no-cedar-config",
						Namespace: namespace,
					},
					Spec: LakekeeperSpec{
						Image: "quay.io/lakekeeper/catalog:latest",
						Database: DatabaseConfig{
							Type: DatabaseTypePostgres,
							Postgres: &PostgresConfig{
								Host:     "postgres.default.svc",
								Database: "lakekeeper",
								User:     &userStr,
								PasswordSecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"},
									Key:                  "password",
								},
								EncryptionKeySecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"},
									Key:                  "encryption-key",
								},
							},
						},
						Authorization: AuthorizationConfig{
							Backend: AuthzBackendCedar,
							// Cedar config missing!
						},
					},
				}

				err := k8sClient.Create(ctx, lk)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("cedar configuration is required"))
			})
		})

		Context("VaultKV2Config User/UserSecretRef Mutual Exclusivity", func() {
			It("should reject VaultKV2 with both user and userSecretRef", func() {
				lakekeeperUser := testDBUser
				vaultUser := "vault-user"
				lk := &Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "invalid-vault-both-user",
						Namespace: namespace,
					},
					Spec: LakekeeperSpec{
						Image: "quay.io/lakekeeper/catalog:latest",
						Database: DatabaseConfig{
							Type: DatabaseTypePostgres,
							Postgres: &PostgresConfig{
								Host:     "postgres.default.svc",
								Database: "lakekeeper",
								User:     &lakekeeperUser,
								PasswordSecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"},
									Key:                  "password",
								},
								EncryptionKeySecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "db-creds"},
									Key:                  "encryption-key",
								},
							},
						},
						SecretStore: &SecretStoreConfig{
							Type: SecretStoreTypeVaultKV2,
							VaultKV2: &VaultKV2Config{
								URL:  "https://vault.example.com",
								User: &vaultUser,
								UserSecretRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "vault-creds"},
									Key:                  "username",
								},
								PasswordSecretRef: corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "vault-creds"},
									Key:                  "password",
								},
								SecretMount: "secret",
							},
						},
						Authorization: AuthorizationConfig{
							Backend: AuthzBackendAllowAll,
						},
					},
				}

				err := k8sClient.Create(ctx, lk)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("exactly one of user or userSecretRef"))
			})
		})
	})
})
