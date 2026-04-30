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
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	lakekeeperv1alpha1 "github.com/lakekeeper/lakekeeper-operator/api/v1alpha1"
)

const (
	testImageV2             = "lakekeeper:v2.0.0"
	managementInfoPath      = "/management/v1/info"
	managementBootstrapPath = "/management/v1/bootstrap"
)

// Test helper functions

// completeMigrationJob finds the migration Job for a Lakekeeper and marks it as succeeded.
// This simulates the Job controller completing the migration in envtest.
//
//nolint:unparam // timeout parameter is part of the test helper signature
func completeMigrationJob(ctx context.Context, lk *lakekeeperv1alpha1.Lakekeeper, timeout time.Duration) error {
	// Calculate the expected job name using the same hash logic as the controller
	specHash := HashMigrationRelevantSpec(lk.Spec)
	expectedJobName := fmt.Sprintf("%s-migrate-%s", lk.Name, specHash[:8])

	// Wait for Job to be created
	migrationJob := &batchv1.Job{}
	Eventually(func() bool {
		err := k8sClient.Get(ctx, types.NamespacedName{
			Name:      expectedJobName,
			Namespace: lk.Namespace,
		}, migrationJob)
		return err == nil
	}, timeout, time.Millisecond*250).Should(BeTrue(), fmt.Sprintf("Migration Job %s should be created", expectedJobName))

	// Mark Job as succeeded (simulates Job controller)
	now := metav1.Now()
	migrationJob.Status.Active = 0
	migrationJob.Status.Succeeded = 1

	// Only set startTime if not already set
	if migrationJob.Status.StartTime == nil {
		migrationJob.Status.StartTime = &now
	}

	// Only set completionTime if not already set
	if migrationJob.Status.CompletionTime == nil {
		migrationJob.Status.CompletionTime = &now
	}

	migrationJob.Status.Conditions = []batchv1.JobCondition{
		{
			Type:               batchv1.JobSuccessCriteriaMet,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: now,
		},
		{
			Type:               batchv1.JobComplete,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: now,
		},
	}
	err := k8sClient.Status().Update(ctx, migrationJob)
	if err != nil {
		return err
	}

	return nil
}

// simulateMigrationJobFailure marks the migration Job as failed.
// This simulates the Job controller marking a Job as failed after retries.
//
//nolint:unparam // timeout parameter is part of the test helper signature
func simulateMigrationJobFailure(ctx context.Context, lk *lakekeeperv1alpha1.Lakekeeper, failureCount int32, timeout time.Duration) error {
	// Find Job by label selector (same as completeMigrationJob)
	jobList := &batchv1.JobList{}
	var migrationJob *batchv1.Job

	Eventually(func() bool {
		if err := k8sClient.List(ctx, jobList,
			client.InNamespace(lk.Namespace),
			client.MatchingLabels{
				"app.kubernetes.io/name":      "lakekeeper",
				"app.kubernetes.io/component": "migration",
				"app.kubernetes.io/instance":  lk.Name,
			}); err != nil {
			return false
		}

		for i := range jobList.Items {
			migrationJob = &jobList.Items[i]
			return true
		}
		return false
	}, timeout, time.Millisecond*250).Should(BeTrue(), "Migration Job should be created")

	// Mark Job as failed
	now := metav1.Now()
	migrationJob.Status.Active = 0
	migrationJob.Status.Succeeded = 0
	migrationJob.Status.Failed = failureCount

	if migrationJob.Status.StartTime == nil {
		migrationJob.Status.StartTime = &now
	}

	// K8s 1.33+ requires FailureTarget condition before Failed condition
	migrationJob.Status.Conditions = []batchv1.JobCondition{
		{
			Type:               batchv1.JobFailureTarget,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             "FailureTarget",
			Message:            "The Job has reached failure target",
		},
		{
			Type:               batchv1.JobFailed,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             "BackoffLimitExceeded",
			Message:            "Job has reached the specified backoff limit",
		},
	}

	err := k8sClient.Status().Update(ctx, migrationJob)
	if err != nil {
		return err
	}

	return nil
}

// simulateMigrationJobInProgress marks the migration Job as active/running.
//
//nolint:unparam // timeout parameter is part of the test helper signature
func simulateMigrationJobInProgress(ctx context.Context, lk *lakekeeperv1alpha1.Lakekeeper, timeout time.Duration) error {
	jobList := &batchv1.JobList{}
	var migrationJob *batchv1.Job

	Eventually(func() bool {
		if err := k8sClient.List(ctx, jobList,
			client.InNamespace(lk.Namespace),
			client.MatchingLabels{
				"app.kubernetes.io/name":      "lakekeeper",
				"app.kubernetes.io/component": "migration",
				"app.kubernetes.io/instance":  lk.Name,
			}); err != nil {
			return false
		}

		for i := range jobList.Items {
			migrationJob = &jobList.Items[i]
			return true
		}
		return false
	}, timeout, time.Millisecond*250).Should(BeTrue(), "Migration Job should be created")

	// Mark Job as active/running
	now := metav1.Now()
	migrationJob.Status.Active = 1
	migrationJob.Status.Succeeded = 0
	migrationJob.Status.Failed = 0

	if migrationJob.Status.StartTime == nil {
		migrationJob.Status.StartTime = &now
	}

	err := k8sClient.Status().Update(ctx, migrationJob)
	return err
}

// Helper function to convert []EnvVar to map for easy assertions
func toEnvMap(envVars []corev1.EnvVar) map[string]corev1.EnvVar {
	m := make(map[string]corev1.EnvVar)
	for _, e := range envVars {
		m[e.Name] = e
	}
	return m
}

// Helper function to check if env var exists with expected value
func expectEnvValue(envMap map[string]corev1.EnvVar, name, expectedValue string) {
	env, exists := envMap[name]
	Expect(exists).To(BeTrue(), "Expected env var %s to exist", name)
	Expect(env.Value).To(Equal(expectedValue), "Expected env var %s to have value %s", name, expectedValue)
}

// Helper function to check if env var exists with secret reference
func expectEnvSecret(envMap map[string]corev1.EnvVar, name, secretName, secretKey string) {
	env, exists := envMap[name]
	Expect(exists).To(BeTrue(), "Expected env var %s to exist", name)
	Expect(env.ValueFrom).ToNot(BeNil(), "Expected env var %s to have ValueFrom", name)
	Expect(env.ValueFrom.SecretKeyRef).ToNot(BeNil(), "Expected env var %s to have SecretKeyRef", name)
	Expect(env.ValueFrom.SecretKeyRef.Name).To(Equal(secretName), "Expected secret name %s", secretName)
	Expect(env.ValueFrom.SecretKeyRef.Key).To(Equal(secretKey), "Expected secret key %s", secretKey)
}

var _ = Describe("Lakekeeper Controller", func() {
	const (
		timeout  = time.Second * 10
		interval = time.Millisecond * 250
	)

	Context("When reconciling a resource", func() {
		const resourceName = "test-lakekeeper"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

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

			By("Creating the Lakekeeper resource")
			lakekeeper := &lakekeeperv1alpha1.Lakekeeper{}
			err = k8sClient.Get(ctx, typeNamespacedName, lakekeeper)
			if err != nil && errors.IsNotFound(err) {
				resource := &lakekeeperv1alpha1.Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
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
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			By("Cleaning up the Lakekeeper resource")
			resource := &lakekeeperv1alpha1.Lakekeeper{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			if err == nil {
				// Delete the resource
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

				// Reconcile to handle finalizer removal
				controllerReconciler := &LakekeeperReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}
				_, _ = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: typeNamespacedName,
				})

				// Wait for deletion to complete
				Eventually(func() bool {
					err := k8sClient.Get(ctx, typeNamespacedName, &lakekeeperv1alpha1.Lakekeeper{})
					return errors.IsNotFound(err)
				}, timeout, interval).Should(BeTrue())
			}

			By("Cleaning up the secret")
			secret := &corev1.Secret{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "db-secret", Namespace: "default"}, secret)
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

		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &LakekeeperReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// First reconcile - adds finalizer
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile - creates resources
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Completing the migration Job")
			lakekeeper := &lakekeeperv1alpha1.Lakekeeper{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, lakekeeper)).To(Succeed())
			Expect(completeMigrationJob(ctx, lakekeeper, timeout)).To(Succeed())

			// Reconcile again to see completed job and create deployment
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the Deployment is created")
			deployment := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, typeNamespacedName, deployment)
			}, timeout, interval).Should(Succeed())

			Expect(deployment.Spec.Replicas).To(Equal(ptr.To(int32(1))))
			Expect(deployment.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(deployment.Spec.Template.Spec.Containers[0].Name).To(Equal("lakekeeper"))
			Expect(deployment.Spec.Template.Spec.Containers[0].Image).To(Equal("lakekeeper:latest"))
			// No initContainers - migrations run as Jobs
			Expect(deployment.Spec.Template.Spec.InitContainers).To(BeEmpty())

			By("Verifying the main Service is created")
			mainService := &corev1.Service{}
			Eventually(func() error {
				return k8sClient.Get(ctx, typeNamespacedName, mainService)
			}, timeout, interval).Should(Succeed())

			Expect(mainService.Spec.Ports).To(HaveLen(1))
			Expect(mainService.Spec.Ports[0].Name).To(Equal("http"))
			Expect(mainService.Spec.Ports[0].Port).To(Equal(int32(8181))) // Default port

			By("Verifying the metrics Service is created")
			metricsService := &corev1.Service{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      resourceName + "-metrics",
					Namespace: "default",
				}, metricsService)
			}, timeout, interval).Should(Succeed())

			Expect(metricsService.Spec.Ports).To(HaveLen(1))
			Expect(metricsService.Spec.Ports[0].Name).To(Equal("metrics"))
			Expect(metricsService.Spec.Ports[0].Port).To(Equal(int32(9000))) // Default metrics port

			By("Verifying the status is updated")
			Expect(k8sClient.Get(ctx, typeNamespacedName, lakekeeper)).To(Succeed())
			Eventually(func() bool {
				err := k8sClient.Get(ctx, typeNamespacedName, lakekeeper)
				if err != nil {
					return false
				}
				return lakekeeper.Status.ObservedGeneration != nil
			}, timeout, interval).Should(BeTrue())

			Expect(lakekeeper.Status.ObservedGeneration).To(Equal(ptr.To(lakekeeper.Generation)))
			Expect(lakekeeper.Status.Conditions).NotTo(BeEmpty())
		})

		It("should set Degraded condition when secrets are missing", func() {
			By("Creating a Lakekeeper resource with missing secret")
			missingSecretLK := &lakekeeperv1alpha1.Lakekeeper{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-missing-secret",
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
								LocalObjectReference: corev1.LocalObjectReference{Name: "nonexistent-secret"},
								Key:                  "password",
							},
							EncryptionKeySecretRef: corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: "nonexistent-secret"},
								Key:                  "encryption-key",
							},
						},
					},
					Authorization: lakekeeperv1alpha1.AuthorizationConfig{
						Backend: lakekeeperv1alpha1.AuthzBackendAllowAll,
					},
				},
			}
			Expect(k8sClient.Create(ctx, missingSecretLK)).To(Succeed())

			By("Reconciling the resource")
			controllerReconciler := &LakekeeperReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// First reconcile - adds finalizer
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-missing-secret",
					Namespace: "default",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile - should fail due to missing secret
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-missing-secret",
					Namespace: "default",
				},
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("nonexistent-secret"))

			By("Verifying the Degraded condition is set")
			lakekeeper := &lakekeeperv1alpha1.Lakekeeper{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-missing-secret",
					Namespace: "default",
				}, lakekeeper)
				if err != nil {
					return false
				}
				return meta.IsStatusConditionTrue(lakekeeper.Status.Conditions, TypeDegraded)
			}, timeout, interval).Should(BeTrue())

			Expect(meta.IsStatusConditionFalse(lakekeeper.Status.Conditions, TypeReady)).To(BeTrue())

			By("Cleaning up the test resource")
			Expect(k8sClient.Delete(ctx, missingSecretLK)).To(Succeed())
		})

		It("should handle custom server configuration", func() {
			By("Creating a Lakekeeper resource with custom server config")
			customConfigLK := &lakekeeperv1alpha1.Lakekeeper{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-custom-config",
					Namespace: "default",
				},
				Spec: lakekeeperv1alpha1.LakekeeperSpec{
					Image:    "lakekeeper:latest",
					Replicas: ptr.To(int32(2)),
					Server: &lakekeeperv1alpha1.ServerConfig{
						BaseURI:     ptr.To("https://lakekeeper.example.com"),
						ListenPort:  ptr.To(int32(8181)),
						MetricsPort: ptr.To(int32(9000)),
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
			Expect(k8sClient.Create(ctx, customConfigLK)).To(Succeed())

			By("Reconciling the resource")
			controllerReconciler := &LakekeeperReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// First reconcile - adds finalizer
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-custom-config",
					Namespace: "default",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile - creates resources
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-custom-config",
					Namespace: "default",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Completing the migration Job")
			customConfigLK = &lakekeeperv1alpha1.Lakekeeper{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      "test-custom-config",
				Namespace: "default",
			}, customConfigLK)).To(Succeed())
			Expect(completeMigrationJob(ctx, customConfigLK, timeout)).To(Succeed())

			// Reconcile again to see completed job and create deployment
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-custom-config",
					Namespace: "default",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the Deployment uses custom configuration")
			deployment := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-custom-config",
					Namespace: "default",
				}, deployment)
			}, timeout, interval).Should(Succeed())

			Expect(deployment.Spec.Replicas).To(Equal(ptr.To(int32(2))))

			// Verify environment variables contain custom config
			envVars := deployment.Spec.Template.Spec.Containers[0].Env
			baseURIFound := false
			listenPortFound := false
			for _, env := range envVars {
				if env.Name == "LAKEKEEPER__BASE_URI" && env.Value == "https://lakekeeper.example.com" {
					baseURIFound = true
				}
				if env.Name == "LAKEKEEPER__LISTEN_PORT" && env.Value == "8181" {
					listenPortFound = true
				}
			}
			Expect(baseURIFound).To(BeTrue())
			Expect(listenPortFound).To(BeTrue())

			By("Verifying the Services use custom ports")
			mainService := &corev1.Service{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-custom-config",
					Namespace: "default",
				}, mainService)
			}, timeout, interval).Should(Succeed())

			Expect(mainService.Spec.Ports[0].Port).To(Equal(int32(8181)))

			metricsService := &corev1.Service{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-custom-config-metrics",
					Namespace: "default",
				}, metricsService)
			}, timeout, interval).Should(Succeed())

			Expect(metricsService.Spec.Ports[0].Port).To(Equal(int32(9000)))

			By("Cleaning up the test resource")
			Expect(k8sClient.Delete(ctx, customConfigLK)).To(Succeed())
		})

		It("should add and remove finalizers correctly", func() {
			By("Triggering reconciliation")
			controllerReconciler := &LakekeeperReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// First reconcile - adds finalizer
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Second reconcile - creates resources
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Completing the migration Job")
			finalizerLK := &lakekeeperv1alpha1.Lakekeeper{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, finalizerLK)).To(Succeed())
			Expect(completeMigrationJob(ctx, finalizerLK, timeout)).To(Succeed())

			// Reconcile again to see completed job
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the finalizer is added")
			lakekeeper := &lakekeeperv1alpha1.Lakekeeper{}
			Eventually(func() bool {
				err := k8sClient.Get(ctx, typeNamespacedName, lakekeeper)
				if err != nil {
					return false
				}
				for _, finalizer := range lakekeeper.Finalizers {
					if finalizer == finalizerName {
						return true
					}
				}
				return false
			}, timeout, interval).Should(BeTrue())

			By("Deleting the resource")
			Expect(k8sClient.Delete(ctx, lakekeeper)).To(Succeed())

			By("Triggering reconciliation for deletion")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the resource is eventually deleted")
			Eventually(func() bool {
				err := k8sClient.Get(ctx, typeNamespacedName, &lakekeeperv1alpha1.Lakekeeper{})
				return errors.IsNotFound(err)
			}, timeout, interval).Should(BeTrue())
		})

		It("should update deployment when image changes", func() {
			By("Reconciling the initial resource")
			controllerReconciler := &LakekeeperReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Create initial deployment
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Completing the initial migration Job")
			lakekeeperInitial := &lakekeeperv1alpha1.Lakekeeper{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, lakekeeperInitial)).To(Succeed())
			Expect(completeMigrationJob(ctx, lakekeeperInitial, timeout)).To(Succeed())

			// Reconcile again to see completed job and create deployment
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Updating the image in the spec")
			lakekeeper := &lakekeeperv1alpha1.Lakekeeper{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, lakekeeper)).To(Succeed())

			lakekeeper.Spec.Image = testImageV2
			Expect(k8sClient.Update(ctx, lakekeeper)).To(Succeed())

			By("Reconciling to apply the change")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Completing the new migration Job")
			Expect(k8sClient.Get(ctx, typeNamespacedName, lakekeeper)).To(Succeed())
			Expect(completeMigrationJob(ctx, lakekeeper, timeout)).To(Succeed())

			// Reconcile again to see completed job and update deployment
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the deployment image is updated")
			deployment := &appsv1.Deployment{}
			Eventually(func() string {
				err := k8sClient.Get(ctx, typeNamespacedName, deployment)
				if err != nil {
					return ""
				}
				if len(deployment.Spec.Template.Spec.Containers) > 0 {
					return deployment.Spec.Template.Spec.Containers[0].Image
				}
				return ""
			}, timeout, interval).Should(Equal(testImageV2))
		})

		It("should handle multiple Lakekeeper instances in same namespace", func() {
			By("Creating a second Lakekeeper instance")
			secondLK := &lakekeeperv1alpha1.Lakekeeper{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-lakekeeper-2",
					Namespace: "default",
				},
				Spec: lakekeeperv1alpha1.LakekeeperSpec{
					Image:    "lakekeeper:latest",
					Replicas: ptr.To(int32(1)),
					Database: lakekeeperv1alpha1.DatabaseConfig{
						Type: lakekeeperv1alpha1.DatabaseTypePostgres,
						Postgres: &lakekeeperv1alpha1.PostgresConfig{
							Host:     "postgres.default.svc",
							Database: "lakekeeper2",
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
			Expect(k8sClient.Create(ctx, secondLK)).To(Succeed())

			By("Reconciling both instances")
			controllerReconciler := &LakekeeperReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			// Reconcile first instance
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Completing first migration Job")
			firstLK := &lakekeeperv1alpha1.Lakekeeper{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, firstLK)).To(Succeed())
			Expect(completeMigrationJob(ctx, firstLK, timeout)).To(Succeed())

			// Reconcile again to create first deployment
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// Reconcile second instance - add finalizer
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-lakekeeper-2",
					Namespace: "default",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Reconcile second instance - create deployment
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-lakekeeper-2",
					Namespace: "default",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Completing second migration Job")
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      "test-lakekeeper-2",
				Namespace: "default",
			}, secondLK)).To(Succeed())
			Expect(completeMigrationJob(ctx, secondLK, timeout)).To(Succeed())

			// Reconcile again to create second deployment
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-lakekeeper-2",
					Namespace: "default",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying both deployments exist")
			deployment1 := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, typeNamespacedName, deployment1)
			}, timeout, interval).Should(Succeed())

			deployment2 := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-lakekeeper-2",
					Namespace: "default",
				}, deployment2)
			}, timeout, interval).Should(Succeed())

			By("Verifying no naming conflicts")
			Expect(deployment1.Name).To(Equal("test-lakekeeper"))
			Expect(deployment2.Name).To(Equal("test-lakekeeper-2"))

			By("Cleaning up the second instance")
			Expect(k8sClient.Delete(ctx, secondLK)).To(Succeed())
		})

		It("should apply resource requirements from spec", func() {
			By("Creating a Lakekeeper with resource requirements")
			resourceLK := &lakekeeperv1alpha1.Lakekeeper{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-resources",
					Namespace: "default",
				},
				Spec: lakekeeperv1alpha1.LakekeeperSpec{
					Image:    "lakekeeper:latest",
					Replicas: ptr.To(int32(1)),
					Resources: &corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    *ptr.To(resource.MustParse("100m")),
							corev1.ResourceMemory: *ptr.To(resource.MustParse("128Mi")),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    *ptr.To(resource.MustParse("500m")),
							corev1.ResourceMemory: *ptr.To(resource.MustParse("512Mi")),
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
			Expect(k8sClient.Create(ctx, resourceLK)).To(Succeed())

			By("Reconciling the resource")
			controllerReconciler := &LakekeeperReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-resources",
					Namespace: "default",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-resources",
					Namespace: "default",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Completing the migration Job")
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      "test-resources",
				Namespace: "default",
			}, resourceLK)).To(Succeed())
			Expect(completeMigrationJob(ctx, resourceLK, timeout)).To(Succeed())

			// Reconcile again to create deployment
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-resources",
					Namespace: "default",
				},
			})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying resource requirements in deployment")
			deployment := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-resources",
					Namespace: "default",
				}, deployment)
			}, timeout, interval).Should(Succeed())

			Expect(deployment.Spec.Template.Spec.Containers).To(HaveLen(1))
			container := deployment.Spec.Template.Spec.Containers[0]
			Expect(container.Resources.Requests).NotTo(BeNil())
			Expect(container.Resources.Limits).NotTo(BeNil())

			By("Cleaning up the test resource")
			Expect(k8sClient.Delete(ctx, resourceLK)).To(Succeed())
		})

	})
})
