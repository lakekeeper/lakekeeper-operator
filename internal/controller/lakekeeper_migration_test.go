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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
		})

		AfterEach(func() {
			By("Cleaning up the secret")
			secret := &corev1.Secret{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "db-secret", Namespace: "default"}, secret)
			if err == nil {
				Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
			}
		})

		Context("Migration Job Failures", func() {
			It("should set Degraded condition when migration Job fails", func() {
				By("Creating Lakekeeper instance")
				failureLK := &lakekeeperv1alpha1.Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-migration-failure",
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
				Expect(k8sClient.Create(ctx, failureLK)).To(Succeed())

				By("Reconciling to create migration Job")
				controllerReconciler := &LakekeeperReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				// First reconcile - adds finalizer
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-migration-failure",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				// Second reconcile - creates migration Job
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-migration-failure",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Simulating migration Job failure with 3 failures")
				Expect(simulateMigrationJobFailure(ctx, failureLK, 3, timeout)).To(Succeed())

				By("Reconciling after Job failure")
				result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-migration-failure",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())        // Permanent failure halts requeue; no error returned
				Expect(result.RequeueAfter).To(BeZero()) // No automatic requeue — user must change spec
				By("Verifying status conditions reflect failure")
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-migration-failure",
					Namespace: "default",
				}, failureLK)).To(Succeed())

				migratedCondition := meta.FindStatusCondition(failureLK.Status.Conditions, TypeMigrated)
				Expect(migratedCondition).NotTo(BeNil())
				Expect(migratedCondition.Status).To(Equal(metav1.ConditionFalse))
				Expect(migratedCondition.Reason).To(Equal("MigrationFailed"))

				degradedCondition := meta.FindStatusCondition(failureLK.Status.Conditions, TypeDegraded)
				Expect(degradedCondition).NotTo(BeNil())
				Expect(degradedCondition.Status).To(Equal(metav1.ConditionTrue))

				readyCondition := meta.FindStatusCondition(failureLK.Status.Conditions, TypeReady)
				Expect(readyCondition).NotTo(BeNil())
				Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))

				By("Verifying no Deployment was created")
				deployment := &appsv1.Deployment{}
				err = k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-migration-failure",
					Namespace: "default",
				}, deployment)
				Expect(errors.IsNotFound(err)).To(BeTrue(), "Deployment should not exist when migration fails")

				By("Cleaning up")
				Expect(k8sClient.Delete(ctx, failureLK)).To(Succeed())
			})

			It("should requeue when migration Job is still in progress", func() {
				By("Creating Lakekeeper instance")
				progressLK := &lakekeeperv1alpha1.Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-migration-progress",
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
				Expect(k8sClient.Create(ctx, progressLK)).To(Succeed())

				By("Reconciling to create migration Job")
				controllerReconciler := &LakekeeperReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				// First reconcile - adds finalizer
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-migration-progress",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				// Second reconcile - creates migration Job
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-migration-progress",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Simulating migration Job in progress")
				Expect(simulateMigrationJobInProgress(ctx, progressLK, timeout)).To(Succeed())

				By("Reconciling while Job is in progress")
				result, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-migration-progress",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(10*time.Second), "Should requeue after 10 seconds")

				By("Verifying status shows migration in progress")
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-migration-progress",
					Namespace: "default",
				}, progressLK)).To(Succeed())

				migratedCondition := meta.FindStatusCondition(progressLK.Status.Conditions, TypeMigrated)
				Expect(migratedCondition).NotTo(BeNil())
				Expect(migratedCondition.Status).To(Equal(metav1.ConditionFalse))
				Expect(migratedCondition.Reason).To(Equal("MigrationInProgress"))

				By("Cleaning up")
				Expect(k8sClient.Delete(ctx, progressLK)).To(Succeed())
			})

			It("should delete failed Job and create new one on spec change", func() {
				By("Creating Lakekeeper instance")
				retryLK := &lakekeeperv1alpha1.Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-migration-retry",
						Namespace: "default",
					},
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Image:    "lakekeeper:v1.0.0",
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
				Expect(k8sClient.Create(ctx, retryLK)).To(Succeed())

				By("Creating and failing first migration Job")
				controllerReconciler := &LakekeeperReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				// First reconcile - adds finalizer
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-migration-retry",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				// Second reconcile - creates migration Job
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-migration-retry",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				Expect(simulateMigrationJobFailure(ctx, retryLK, 3, timeout)).To(Succeed())

				By("Getting the failed Job name")
				jobList := &batchv1.JobList{}
				Expect(k8sClient.List(ctx, jobList,
					client.InNamespace("default"),
					client.MatchingLabels{
						"app.kubernetes.io/name":      "lakekeeper",
						"app.kubernetes.io/component": "migration",
						"app.kubernetes.io/instance":  "test-migration-retry",
					})).To(Succeed())
				Expect(jobList.Items).To(HaveLen(1))
				failedJobName := jobList.Items[0].Name

				By("Changing image to trigger new migration (spec change)")
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-migration-retry",
					Namespace: "default",
				}, retryLK)).To(Succeed())
				retryLK.Spec.Image = "lakekeeper:v1.0.1" // Change image version
				Expect(k8sClient.Update(ctx, retryLK)).To(Succeed())

				By("Reconciling - should detect spec change and create new Job")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-migration-retry",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				Expect(err).NotTo(HaveOccurred())

				By("Verifying new Job was created with different hash")
				Eventually(func() int {
					jobList := &batchv1.JobList{}
					err := k8sClient.List(ctx, jobList,
						client.InNamespace("default"),
						client.MatchingLabels{
							"app.kubernetes.io/name":      "lakekeeper",
							"app.kubernetes.io/component": "migration",
							"app.kubernetes.io/instance":  "test-migration-retry",
						})
					if err != nil {
						return -1 // Return sentinel value on error to fail the assertion
					}
					return len(jobList.Items)
				}, timeout, interval).Should(Equal(2), "Should have both old failed Job and new Job")

				By("Verifying old failed Job still exists")
				err = k8sClient.Get(ctx, types.NamespacedName{
					Name:      failedJobName,
					Namespace: "default",
				}, &batchv1.Job{})
				Expect(err).NotTo(HaveOccurred(), "Old failed Job should still exist")

				By("Cleaning up")
				Expect(k8sClient.Delete(ctx, retryLK)).To(Succeed())
			})

			It("should not re-run migration when completed Job is TTL-cleaned", func() {
				By("Creating Lakekeeper instance")
				ttlLK := &lakekeeperv1alpha1.Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-migration-ttl",
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
				Expect(k8sClient.Create(ctx, ttlLK)).To(Succeed())

				controllerReconciler := &LakekeeperReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}
				ttlNN := types.NamespacedName{Name: "test-migration-ttl", Namespace: "default"}

				By("Reconciling to add finalizer")
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: ttlNN})
				Expect(err).NotTo(HaveOccurred())

				By("Reconciling to create migration Job")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: ttlNN})
				Expect(err).NotTo(HaveOccurred())

				By("Completing the migration Job")
				Expect(k8sClient.Get(ctx, ttlNN, ttlLK)).To(Succeed())
				Expect(completeMigrationJob(ctx, ttlLK, timeout)).To(Succeed())

				By("Reconciling to process the completed Job")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: ttlNN})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying TypeMigrated is True and MigrationJob is set in status")
				Expect(k8sClient.Get(ctx, ttlNN, ttlLK)).To(Succeed())
				Expect(meta.IsStatusConditionTrue(ttlLK.Status.Conditions, TypeMigrated)).To(BeTrue())
				Expect(ttlLK.Status.MigrationJob).NotTo(BeEmpty())

				// ttlLK now carries the "already migrated" status: TypeMigrated=True and
				// MigrationJob=<expectedJobName>. To accurately model TTL cleanup (the Job
				// is gone from the API server but no batch controller runs in envtest to
				// clear the job-tracking finalizer), we use a fake client that simply has
				// no Jobs. This lets needsMigration hit the IsNotFound branch – exactly
				// what the real controller sees after Kubernetes removes the TTL-expired Job.
				By("Simulating TTL cleanup via a fake client with no Jobs")
				fakeClient := fake.NewClientBuilder().
					WithScheme(k8sClient.Scheme()).
					Build()
				fakeReconciler := &LakekeeperReconciler{Client: fakeClient, Scheme: fakeClient.Scheme()}

				By("Asserting needsMigration returns false for the TTL-cleaned Job")
				needs, err := fakeReconciler.needsMigration(ctx, ttlLK)
				Expect(err).NotTo(HaveOccurred())
				Expect(needs).To(BeFalse(),
					"needsMigration must not schedule a new migration when the spec was already successfully migrated, even if the Job was TTL-cleaned")

				By("Verifying needsMigration consistently returns false (no re-migration triggered)")
				Consistently(func() bool {
					n, callErr := fakeReconciler.needsMigration(ctx, ttlLK)
					Expect(callErr).NotTo(HaveOccurred())
					return n
				}, time.Second*2, interval).Should(BeFalse(),
					"No new migration should be scheduled for an already-migrated spec after TTL cleanup")

				By("Cleaning up")
				Expect(k8sClient.Delete(ctx, ttlLK)).To(Succeed())
			})
		})
	})
})

var _ = Describe("Lakekeeper Controller", func() {

	// Migration Job Unit Tests
	Context("Migration Job Functions", func() {
		Context("HashMigrationRelevantSpec", func() {
			It("should produce consistent hash for same spec", func() {
				spec := lakekeeperv1alpha1.LakekeeperSpec{
					Image: "lakekeeper:v1.0.0",
					Database: lakekeeperv1alpha1.DatabaseConfig{
						Type: lakekeeperv1alpha1.DatabaseTypePostgres,
						Postgres: &lakekeeperv1alpha1.PostgresConfig{
							Host:     "postgres.example.com",
							Database: "lakekeeper",
						},
					},
				}

				hash1 := HashMigrationRelevantSpec(spec)
				hash2 := HashMigrationRelevantSpec(spec)

				Expect(hash1).To(Equal(hash2))
				Expect(hash1).NotTo(BeEmpty())
			})

			It("should produce different hash when image changes", func() {
				spec1 := lakekeeperv1alpha1.LakekeeperSpec{
					Image: "lakekeeper:v1.0.0",
					Database: lakekeeperv1alpha1.DatabaseConfig{
						Type: lakekeeperv1alpha1.DatabaseTypePostgres,
						Postgres: &lakekeeperv1alpha1.PostgresConfig{
							Host:     "postgres.example.com",
							Database: "lakekeeper",
						},
					},
				}

				spec2 := lakekeeperv1alpha1.LakekeeperSpec{
					Image: testImageV2,
					Database: lakekeeperv1alpha1.DatabaseConfig{
						Type: lakekeeperv1alpha1.DatabaseTypePostgres,
						Postgres: &lakekeeperv1alpha1.PostgresConfig{
							Host:     "postgres.example.com",
							Database: "lakekeeper",
						},
					},
				}

				hash1 := HashMigrationRelevantSpec(spec1)
				hash2 := HashMigrationRelevantSpec(spec2)

				Expect(hash1).NotTo(Equal(hash2))
			})

			It("should produce different hash when database config changes", func() {
				spec1 := lakekeeperv1alpha1.LakekeeperSpec{
					Image: "lakekeeper:v1.0.0",
					Database: lakekeeperv1alpha1.DatabaseConfig{
						Type: lakekeeperv1alpha1.DatabaseTypePostgres,
						Postgres: &lakekeeperv1alpha1.PostgresConfig{
							Host:     "postgres1.example.com",
							Database: "lakekeeper",
						},
					},
				}

				spec2 := lakekeeperv1alpha1.LakekeeperSpec{
					Image: "lakekeeper:v1.0.0",
					Database: lakekeeperv1alpha1.DatabaseConfig{
						Type: lakekeeperv1alpha1.DatabaseTypePostgres,
						Postgres: &lakekeeperv1alpha1.PostgresConfig{
							Host:     "postgres2.example.com",
							Database: "lakekeeper",
						},
					},
				}

				hash1 := HashMigrationRelevantSpec(spec1)
				hash2 := HashMigrationRelevantSpec(spec2)

				Expect(hash1).NotTo(Equal(hash2))
			})

			It("should produce same hash when only replicas change (not migration-relevant)", func() {
				spec1 := lakekeeperv1alpha1.LakekeeperSpec{
					Image:    "lakekeeper:v1.0.0",
					Replicas: ptr.To(int32(1)),
					Database: lakekeeperv1alpha1.DatabaseConfig{
						Type: lakekeeperv1alpha1.DatabaseTypePostgres,
						Postgres: &lakekeeperv1alpha1.PostgresConfig{
							Host:     "postgres.example.com",
							Database: "lakekeeper",
						},
					},
				}

				spec2 := lakekeeperv1alpha1.LakekeeperSpec{
					Image:    "lakekeeper:v1.0.0",
					Replicas: ptr.To(int32(3)),
					Database: lakekeeperv1alpha1.DatabaseConfig{
						Type: lakekeeperv1alpha1.DatabaseTypePostgres,
						Postgres: &lakekeeperv1alpha1.PostgresConfig{
							Host:     "postgres.example.com",
							Database: "lakekeeper",
						},
					},
				}

				hash1 := HashMigrationRelevantSpec(spec1)
				hash2 := HashMigrationRelevantSpec(spec2)

				Expect(hash1).To(Equal(hash2))
			})

			It("should produce different hash when readHost changes (emitted as LAKEKEEPER__PG_HOST_R)", func() {
				spec1 := lakekeeperv1alpha1.LakekeeperSpec{
					Image: "lakekeeper:v1.0.0",
					Database: lakekeeperv1alpha1.DatabaseConfig{
						Type: lakekeeperv1alpha1.DatabaseTypePostgres,
						Postgres: &lakekeeperv1alpha1.PostgresConfig{
							Host:     "postgres.example.com",
							Database: "lakekeeper",
						},
					},
				}

				spec2 := lakekeeperv1alpha1.LakekeeperSpec{
					Image: "lakekeeper:v1.0.0",
					Database: lakekeeperv1alpha1.DatabaseConfig{
						Type: lakekeeperv1alpha1.DatabaseTypePostgres,
						Postgres: &lakekeeperv1alpha1.PostgresConfig{
							Host:     "postgres.example.com",
							Database: "lakekeeper",
							ReadHost: ptr.To("replica.example.com"),
						},
					},
				}

				hash1 := HashMigrationRelevantSpec(spec1)
				hash2 := HashMigrationRelevantSpec(spec2)

				Expect(hash1).NotTo(Equal(hash2), "readHost is included in hash because it is emitted as LAKEKEEPER__PG_HOST_R")
			})

			It("should use hash-based naming", func() {
				spec := lakekeeperv1alpha1.LakekeeperSpec{
					Image: "lakekeeper:v1.0.0",
					Database: lakekeeperv1alpha1.DatabaseConfig{
						Type: lakekeeperv1alpha1.DatabaseTypePostgres,
						Postgres: &lakekeeperv1alpha1.PostgresConfig{
							Host:     "postgres.example.com",
							Database: "lakekeeper",
							User:     ptr.To("lakekeeper"),
							PasswordSecretRef: corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: "db-password",
								},
								Key: "password",
							},
							EncryptionKeySecretRef: corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: "db-encryption-key",
								},
								Key: "key",
							},
						},
					},
					Authorization: lakekeeperv1alpha1.AuthorizationConfig{
						Backend: lakekeeperv1alpha1.AuthzBackendAllowAll,
					},
				}

				hash := HashMigrationRelevantSpec(spec)
				expectedJobName := "test-lakekeeper-migrate-" + hash[:8]

				// Verify the name pattern is correct
				Expect(expectedJobName).To(HavePrefix("test-lakekeeper-migrate-"))
				Expect(expectedJobName).To(HaveLen(len("test-lakekeeper-migrate-") + 8))

				// Verify when the spec changes, the job name changes
				spec2 := spec
				spec2.Image = testImageV2
				hash2 := HashMigrationRelevantSpec(spec2)
				expectedJobName2 := "test-lakekeeper-migrate-" + hash2[:8]

				Expect(expectedJobName2).NotTo(Equal(expectedJobName))
			})
		})

		Context("isJobComplete", func() {
			It("should return true when job has succeeded", func() {
				job := &batchv1.Job{
					Status: batchv1.JobStatus{
						Succeeded: 1,
						Failed:    0,
					},
				}

				Expect(isJobComplete(job)).To(BeTrue())
			})

			It("should return true when job has failed 3 times", func() {
				job := &batchv1.Job{
					Status: batchv1.JobStatus{
						Succeeded: 0,
						Failed:    3,
					},
				}

				Expect(isJobComplete(job)).To(BeTrue())
			})

			It("should return false when job is still running", func() {
				job := &batchv1.Job{
					Status: batchv1.JobStatus{
						Active:    1,
						Succeeded: 0,
						Failed:    0,
					},
				}

				Expect(isJobComplete(job)).To(BeFalse())
			})

			It("should return false when job has failed less than 3 times", func() {
				job := &batchv1.Job{
					Status: batchv1.JobStatus{
						Succeeded: 0,
						Failed:    2,
						Active:    0,
					},
				}

				Expect(isJobComplete(job)).To(BeFalse())
			})
		})

		Context("isJobSuccessful", func() {
			It("should return true when job has succeeded", func() {
				job := &batchv1.Job{
					Status: batchv1.JobStatus{
						Succeeded: 1,
					},
				}

				Expect(isJobSuccessful(job)).To(BeTrue())
			})

			It("should return false when job has failed", func() {
				job := &batchv1.Job{
					Status: batchv1.JobStatus{
						Failed: 3,
					},
				}

				Expect(isJobSuccessful(job)).To(BeFalse())
			})

			It("should return false when job is still running", func() {
				job := &batchv1.Job{
					Status: batchv1.JobStatus{
						Active: 1,
					},
				}

				Expect(isJobSuccessful(job)).To(BeFalse())
			})
		})
	})
})
