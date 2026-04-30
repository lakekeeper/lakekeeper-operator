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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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
		})

		AfterEach(func() {
			By("Cleaning up the secret")
			secret := &corev1.Secret{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "db-secret", Namespace: "default"}, secret)
			if err == nil {
				Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
			}
		})

		Context("Service Reconciliation", func() {
			It("should create separate metrics service on custom port", func() {
				By("Creating Lakekeeper with custom metrics port")
				metricsLK := &lakekeeperv1alpha1.Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-metrics-service",
						Namespace: "default",
					},
					Spec: lakekeeperv1alpha1.LakekeeperSpec{
						Image: "lakekeeper:latest",
						Server: &lakekeeperv1alpha1.ServerConfig{
							ListenPort:  ptr.To(int32(8000)),
							MetricsPort: ptr.To(int32(9100)),
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
				Expect(k8sClient.Create(ctx, metricsLK)).To(Succeed())

				By("Reconciling the resource")
				controllerReconciler := &LakekeeperReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-metrics-service",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-metrics-service",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Completing the migration Job")
				Expect(completeMigrationJob(ctx, metricsLK, timeout)).To(Succeed())

				// Reconcile again after migration completes
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-metrics-service",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying main service exists with correct port")
				mainService := &corev1.Service{}
				Eventually(func() error {
					return k8sClient.Get(ctx, types.NamespacedName{
						Name:      "test-metrics-service",
						Namespace: "default",
					}, mainService)
				}, timeout, interval).Should(Succeed())

				Expect(mainService.Spec.Ports).To(HaveLen(1))
				Expect(mainService.Spec.Ports[0].Name).To(Equal("http"))
				Expect(mainService.Spec.Ports[0].Port).To(Equal(int32(8000)))

				By("Verifying metrics service exists with correct port")
				metricsService := &corev1.Service{}
				Eventually(func() error {
					return k8sClient.Get(ctx, types.NamespacedName{
						Name:      "test-metrics-service-metrics",
						Namespace: "default",
					}, metricsService)
				}, timeout, interval).Should(Succeed())

				Expect(metricsService.Spec.Ports).To(HaveLen(1))
				Expect(metricsService.Spec.Ports[0].Name).To(Equal("metrics"))
				Expect(metricsService.Spec.Ports[0].Port).To(Equal(int32(9100)))

				By("Cleaning up")
				Expect(k8sClient.Delete(ctx, metricsLK)).To(Succeed())
			})
		})

		Context("Status Condition Transitions", func() {
			It("should transition from Progressing to Ready", func() {
				By("Creating Lakekeeper instance")
				progressLK := &lakekeeperv1alpha1.Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-progressing",
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

				By("Reconciling to create deployment")
				controllerReconciler := &LakekeeperReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-progressing",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-progressing",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Completing the migration Job")
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-progressing",
					Namespace: "default",
				}, progressLK)).To(Succeed())
				Expect(completeMigrationJob(ctx, progressLK, timeout)).To(Succeed())

				// Reconcile again to create deployment
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-progressing",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Updating deployment status to ready")
				lakekeeper := &lakekeeperv1alpha1.Lakekeeper{}
				deployment := &appsv1.Deployment{}
				Eventually(func() error {
					return k8sClient.Get(ctx, types.NamespacedName{
						Name:      "test-progressing",
						Namespace: "default",
					}, deployment)
				}, timeout, interval).Should(Succeed())

				deployment.Status.Replicas = 1
				deployment.Status.ReadyReplicas = 1
				deployment.Status.UpdatedReplicas = 1
				deployment.Status.AvailableReplicas = 1
				Expect(k8sClient.Status().Update(ctx, deployment)).To(Succeed())

				By("Reconciling to update status")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-progressing",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying Ready condition is set")
				Eventually(func() bool {
					err := k8sClient.Get(ctx, types.NamespacedName{
						Name:      "test-progressing",
						Namespace: "default",
					}, lakekeeper)
					if err != nil {
						return false
					}
					return meta.IsStatusConditionTrue(lakekeeper.Status.Conditions, TypeReady)
				}, timeout, interval).Should(BeTrue())

				By("Cleaning up")
				Expect(k8sClient.Delete(ctx, progressLK)).To(Succeed())
			})

			It("should set Degraded condition when deployment has no ready replicas", func() {
				By("Creating Lakekeeper instance")
				degradedLK := &lakekeeperv1alpha1.Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-degraded",
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
				Expect(k8sClient.Create(ctx, degradedLK)).To(Succeed())

				By("Reconciling to create deployment")
				controllerReconciler := &LakekeeperReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-degraded",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-degraded",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Completing the migration Job")
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-degraded",
					Namespace: "default",
				}, degradedLK)).To(Succeed())
				Expect(completeMigrationJob(ctx, degradedLK, timeout)).To(Succeed())

				// Reconcile again to create deployment
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-degraded",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Updating deployment status with 0 ready replicas")
				deployment := &appsv1.Deployment{}
				Eventually(func() error {
					return k8sClient.Get(ctx, types.NamespacedName{
						Name:      "test-degraded",
						Namespace: "default",
					}, deployment)
				}, timeout, interval).Should(Succeed())

				deployment.Status.Replicas = 1
				deployment.Status.ReadyReplicas = 0 // No ready replicas
				deployment.Status.UpdatedReplicas = 1
				deployment.Status.AvailableReplicas = 0
				Expect(k8sClient.Status().Update(ctx, deployment)).To(Succeed())

				By("Reconciling to update status")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-degraded",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying Degraded condition is set")
				lakekeeper := &lakekeeperv1alpha1.Lakekeeper{}
				Eventually(func() bool {
					err := k8sClient.Get(ctx, types.NamespacedName{
						Name:      "test-degraded",
						Namespace: "default",
					}, lakekeeper)
					if err != nil {
						return false
					}
					return meta.IsStatusConditionTrue(lakekeeper.Status.Conditions, TypeDegraded)
				}, timeout, interval).Should(BeTrue())

				By("Cleaning up")
				Expect(k8sClient.Delete(ctx, degradedLK)).To(Succeed())
			})
		})

		Context("ObservedGeneration Tracking", func() {
			It("should increment observedGeneration on spec changes", func() {
				By("Creating Lakekeeper instance")
				genLK := &lakekeeperv1alpha1.Lakekeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-generation",
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
				Expect(k8sClient.Create(ctx, genLK)).To(Succeed())

				By("Reconciling to create initial state")
				controllerReconciler := &LakekeeperReconciler{
					Client: k8sClient,
					Scheme: k8sClient.Scheme(),
				}

				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-generation",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-generation",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Completing the initial migration Job")
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-generation",
					Namespace: "default",
				}, genLK)).To(Succeed())
				Expect(completeMigrationJob(ctx, genLK, timeout)).To(Succeed())

				// Reconcile again to create deployment
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-generation",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying initial observedGeneration")
				lakekeeper := &lakekeeperv1alpha1.Lakekeeper{}
				Eventually(func() bool {
					err := k8sClient.Get(ctx, types.NamespacedName{
						Name:      "test-generation",
						Namespace: "default",
					}, lakekeeper)
					if err != nil {
						return false
					}
					return lakekeeper.Status.ObservedGeneration != nil &&
						*lakekeeper.Status.ObservedGeneration == lakekeeper.Generation
				}, timeout, interval).Should(BeTrue())

				initialGeneration := lakekeeper.Generation
				Expect(*lakekeeper.Status.ObservedGeneration).To(Equal(initialGeneration))

				By("Updating spec to change image version")
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-generation",
					Namespace: "default",
				}, lakekeeper)).To(Succeed())

				lakekeeper.Spec.Image = "lakekeeper:v2.0.0"
				Expect(k8sClient.Update(ctx, lakekeeper)).To(Succeed())

				By("Verifying generation incremented after update")
				Eventually(func() bool {
					err := k8sClient.Get(ctx, types.NamespacedName{
						Name:      "test-generation",
						Namespace: "default",
					}, lakekeeper)
					if err != nil {
						return false
					}
					return lakekeeper.Generation > initialGeneration
				}, timeout, interval).Should(BeTrue())

				newGeneration := lakekeeper.Generation
				Expect(newGeneration).To(Equal(initialGeneration + 1))

				By("Reconciling to update observedGeneration")
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-generation",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Completing the new migration Job after spec change")
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-generation",
					Namespace: "default",
				}, lakekeeper)).To(Succeed())
				Expect(completeMigrationJob(ctx, lakekeeper, timeout)).To(Succeed())

				// Reconcile again to update deployment
				_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      "test-generation",
						Namespace: "default",
					},
				})
				Expect(err).NotTo(HaveOccurred())

				By("Verifying observedGeneration matches new generation")
				Eventually(func() bool {
					err := k8sClient.Get(ctx, types.NamespacedName{
						Name:      "test-generation",
						Namespace: "default",
					}, lakekeeper)
					if err != nil {
						return false
					}
					return lakekeeper.Status.ObservedGeneration != nil &&
						*lakekeeper.Status.ObservedGeneration == newGeneration
				}, timeout, interval).Should(BeTrue())

				By("Cleaning up")
				Expect(k8sClient.Delete(ctx, genLK)).To(Succeed())
			})
		})

	})
})

// syncBootstrapStatus unit tests use an httptest server to avoid real network calls.
// These tests verify the bootstrap tracking behaviour described in the spec:
//   - When the server reports bootstrapped=true the operator ALWAYS sets BootstrappedAt/ServerID,
//     regardless of spec.bootstrap (nil, disabled, or enabled).
//   - When spec.bootstrap.enabled=true and server is not bootstrapped, the operator performs POST.
//   - 409 Conflict on POST is treated as a concurrent-bootstrap race and re-fetches info.
var _ = Describe("syncBootstrapStatus", func() {
	var (
		r   *LakekeeperReconciler
		lk  *lakekeeperv1alpha1.Lakekeeper
		ts  *httptest.Server
		ctx context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
		lk = &lakekeeperv1alpha1.Lakekeeper{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-bootstrap",
				Namespace: "default",
			},
			Spec: lakekeeperv1alpha1.LakekeeperSpec{
				Image: "lakekeeper:latest",
				Database: lakekeeperv1alpha1.DatabaseConfig{
					Type: lakekeeperv1alpha1.DatabaseTypePostgres,
					Postgres: &lakekeeperv1alpha1.PostgresConfig{
						Host:     "postgres",
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
				Authorization: lakekeeperv1alpha1.AuthorizationConfig{
					Backend: lakekeeperv1alpha1.AuthzBackendAllowAll,
				},
				// Bootstrap intentionally omitted (nil)
			},
		}
	})

	AfterEach(func() {
		if ts != nil {
			ts.Close()
			ts = nil
		}
	})

	Context("when the server reports bootstrapped=true (e.g. allowall auto-bootstrap)", func() {
		BeforeEach(func() {
			ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == managementInfoPath {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(infoResponse{
						Bootstrapped: true,
						ServerID:     "server-id-123",
					})
				}
			}))
			r = &LakekeeperReconciler{
				getBaseURL: func(_ *lakekeeperv1alpha1.Lakekeeper) string {
					return ts.URL
				},
			}
		})

		It("should set BootstrappedAt and Bootstrapped=True when spec.bootstrap is nil", func() {
			lk.Spec.Bootstrap = nil

			Expect(r.syncBootstrapStatus(ctx, lk)).To(Succeed())

			Expect(lk.Status.BootstrappedAt).NotTo(BeNil(),
				"BootstrappedAt must be set whenever the server reports bootstrapped=true")
			Expect(lk.Status.ServerID).NotTo(BeNil())
			Expect(*lk.Status.ServerID).To(Equal("server-id-123"))
			cond := meta.FindStatusCondition(lk.Status.Conditions, TypeBootstrapped)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue),
				"Bootstrapped condition must be True when server reports bootstrapped=true")
			Expect(cond.Reason).To(Equal("Bootstrapped"))
		})

		It("should set BootstrappedAt and Bootstrapped=True when spec.bootstrap.enabled is false", func() {
			lk.Spec.Bootstrap = &lakekeeperv1alpha1.BootstrapConfig{Enabled: ptr.To(false)}

			Expect(r.syncBootstrapStatus(ctx, lk)).To(Succeed())

			Expect(lk.Status.BootstrappedAt).NotTo(BeNil(),
				"BootstrappedAt must be set whenever the server reports bootstrapped=true")
			Expect(lk.Status.ServerID).NotTo(BeNil())
			Expect(*lk.Status.ServerID).To(Equal("server-id-123"))
			cond := meta.FindStatusCondition(lk.Status.Conditions, TypeBootstrapped)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("Bootstrapped"))
		})

		It("should set BootstrappedAt and Bootstrapped=True when spec.bootstrap.enabled is true", func() {
			lk.Spec.Bootstrap = &lakekeeperv1alpha1.BootstrapConfig{Enabled: ptr.To(true)}

			Expect(r.syncBootstrapStatus(ctx, lk)).To(Succeed())

			Expect(lk.Status.BootstrappedAt).NotTo(BeNil(),
				"BootstrappedAt must be set when spec.bootstrap.enabled=true and server reports bootstrapped")
			Expect(lk.Status.ServerID).NotTo(BeNil())
			Expect(*lk.Status.ServerID).To(Equal("server-id-123"))
			cond := meta.FindStatusCondition(lk.Status.Conditions, TypeBootstrapped)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("Bootstrapped"))
		})
	})

	Context("when the server is unreachable", func() {
		BeforeEach(func() {
			r = &LakekeeperReconciler{
				// 192.0.2.1 (RFC 5737 TEST-NET-1) is guaranteed non-routable.
				getBaseURL: func(_ *lakekeeperv1alpha1.Lakekeeper) string {
					return "http://192.0.2.1:8181"
				},
			}
		})

		It("should return nil and set Bootstrapped=False/ServerNotReachable", func() {
			lk.Spec.Bootstrap = nil

			// Use a short deadline so the test completes quickly even if the OS
			// does not return EHOSTUNREACH immediately.
			testCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			Expect(r.syncBootstrapStatus(testCtx, lk)).To(Succeed(),
				"transient network errors must not be returned as errors")

			Expect(lk.Status.BootstrappedAt).To(BeNil())
			cond := meta.FindStatusCondition(lk.Status.Conditions, TypeBootstrapped)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("ServerNotReachable"))
		})
	})

	Context("when spec.bootstrap.enabled=true and server is not yet bootstrapped", func() {
		It("should POST /management/v1/bootstrap and update status on success (204)", func() {
			var getCount int32
			ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				switch req.URL.Path {
				case managementInfoPath:
					w.Header().Set("Content-Type", "application/json")
					if atomic.AddInt32(&getCount, 1) == 1 {
						_ = json.NewEncoder(w).Encode(infoResponse{Bootstrapped: false, ServerID: ""})
					} else {
						_ = json.NewEncoder(w).Encode(infoResponse{Bootstrapped: true, ServerID: "test-server-id"})
					}
				case managementBootstrapPath:
					w.WriteHeader(http.StatusNoContent)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))

			r = &LakekeeperReconciler{
				getBaseURL: func(_ *lakekeeperv1alpha1.Lakekeeper) string {
					return ts.URL
				},
			}
			lk.Spec.Bootstrap = &lakekeeperv1alpha1.BootstrapConfig{Enabled: ptr.To(true)}

			Expect(r.syncBootstrapStatus(ctx, lk)).To(Succeed())

			Expect(lk.Status.BootstrappedAt).NotTo(BeNil(),
				"BootstrappedAt must be set after auto-bootstrap")
			Expect(lk.Status.ServerID).NotTo(BeNil())
			Expect(*lk.Status.ServerID).To(Equal("test-server-id"))
			cond := meta.FindStatusCondition(lk.Status.Conditions, TypeBootstrapped)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("Bootstrapped"))
		})

		It("should handle 409 Conflict (concurrent bootstrap race) and update status", func() {
			// Override: first GET must return not-bootstrapped so we proceed to POST
			var getCount int32
			ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				switch req.URL.Path {
				case managementInfoPath:
					w.Header().Set("Content-Type", "application/json")
					if atomic.AddInt32(&getCount, 1) == 1 {
						_ = json.NewEncoder(w).Encode(infoResponse{Bootstrapped: false, ServerID: ""})
					} else {
						// Re-fetch after 409 returns bootstrapped=true
						_ = json.NewEncoder(w).Encode(infoResponse{Bootstrapped: true, ServerID: "race-server-id"})
					}
				case managementBootstrapPath:
					// Simulate concurrent bootstrap: another actor already bootstrapped
					w.WriteHeader(http.StatusConflict)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))

			r = &LakekeeperReconciler{
				getBaseURL: func(_ *lakekeeperv1alpha1.Lakekeeper) string {
					return ts.URL
				},
			}
			lk.Spec.Bootstrap = &lakekeeperv1alpha1.BootstrapConfig{Enabled: ptr.To(true)}

			Expect(r.syncBootstrapStatus(ctx, lk)).To(Succeed())

			Expect(lk.Status.BootstrappedAt).NotTo(BeNil(),
				"BootstrappedAt must be set after 409 Conflict (concurrent bootstrap)")
			Expect(lk.Status.ServerID).NotTo(BeNil())
			Expect(*lk.Status.ServerID).To(Equal("race-server-id"))
			cond := meta.FindStatusCondition(lk.Status.Conditions, TypeBootstrapped)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("Bootstrapped"))
		})
	})

	Context("when spec.bootstrap.Enabled is false and server is not bootstrapped", func() {
		It("should set NotBootstrapped condition and not set BootstrappedAt", func() {
			ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == managementInfoPath {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(infoResponse{Bootstrapped: false, ServerID: ""})
				}
			}))
			r = &LakekeeperReconciler{
				getBaseURL: func(_ *lakekeeperv1alpha1.Lakekeeper) string {
					return ts.URL
				},
			}
			lk.Spec.Bootstrap = &lakekeeperv1alpha1.BootstrapConfig{Enabled: ptr.To(false)}

			Expect(r.syncBootstrapStatus(ctx, lk)).To(Succeed())

			Expect(lk.Status.BootstrappedAt).To(BeNil())
			Expect(lk.Status.ServerID).To(BeNil())
			cond := meta.FindStatusCondition(lk.Status.Conditions, TypeBootstrapped)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("NotBootstrapped"))
		})
	})

	Context("when spec.bootstrap is nil and server is not bootstrapped", func() {
		It("should set Bootstrapped=False/NotBootstrapped and return nil", func() {
			ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == managementInfoPath {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(infoResponse{Bootstrapped: false, ServerID: ""})
				}
			}))
			r = &LakekeeperReconciler{
				getBaseURL: func(_ *lakekeeperv1alpha1.Lakekeeper) string {
					return ts.URL
				},
			}
			lk.Spec.Bootstrap = nil

			Expect(r.syncBootstrapStatus(ctx, lk)).To(Succeed())

			Expect(lk.Status.BootstrappedAt).To(BeNil())
			Expect(lk.Status.ServerID).To(BeNil())
			cond := meta.FindStatusCondition(lk.Status.Conditions, TypeBootstrapped)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("NotBootstrapped"))
		})
	})

	// Issue #1: consistency lag — POST returns 204 but re-fetch still returns bootstrapped=false.
	Context("when spec.bootstrap.enabled=true, POST returns 204, but re-fetch still returns bootstrapped=false (consistency lag)", func() {
		It("should return nil, leave BootstrappedAt nil, and never set Bootstrapped=True", func() {
			ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				switch req.URL.Path {
				case managementInfoPath:
					// Both the initial GET and the post-bootstrap re-fetch return bootstrapped=false.
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(infoResponse{Bootstrapped: false, ServerID: ""})
				case managementBootstrapPath:
					w.WriteHeader(http.StatusNoContent)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			r = &LakekeeperReconciler{
				getBaseURL: func(_ *lakekeeperv1alpha1.Lakekeeper) string {
					return ts.URL
				},
			}
			lk.Spec.Bootstrap = &lakekeeperv1alpha1.BootstrapConfig{Enabled: ptr.To(true)}

			Expect(r.syncBootstrapStatus(ctx, lk)).To(Succeed(),
				"consistency lag must not be returned as an error")

			Expect(lk.Status.BootstrappedAt).To(BeNil(),
				"BootstrappedAt must remain nil when re-fetch still returns bootstrapped=false")
			cond := meta.FindStatusCondition(lk.Status.Conditions, TypeBootstrapped)
			if cond != nil {
				Expect(cond.Status).NotTo(Equal(metav1.ConditionTrue),
					"Bootstrapped condition must never be True when the server still reports bootstrapped=false")
			}
		})
	})

	// Issue #6: early-return guard — BootstrappedAt already set → no HTTP calls.
	Context("when BootstrappedAt is already set (idempotency guard)", func() {
		It("should return nil without making any HTTP calls", func() {
			unreachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				Fail("unexpected HTTP call: syncBootstrapStatus must return early when BootstrappedAt is already set")
			}))
			defer unreachable.Close()

			r2 := &LakekeeperReconciler{
				getBaseURL: func(_ *lakekeeperv1alpha1.Lakekeeper) string {
					return unreachable.URL
				},
			}
			now := metav1.Now()
			lk.Status.BootstrappedAt = &now

			Expect(r2.syncBootstrapStatus(ctx, lk)).To(Succeed())
		})
	})

	// Issue #5: getBootstrapCheckInterval unit tests.
	Context("getBootstrapCheckInterval", func() {
		It("should return bootstrapPollInterval (15s) when spec.bootstrap is nil", func() {
			rec := &LakekeeperReconciler{}
			lkNil := &lakekeeperv1alpha1.Lakekeeper{}
			Expect(rec.getBootstrapCheckInterval(lkNil)).To(Equal(bootstrapPollInterval))
		})

		It("should return bootstrapPollInterval when spec.bootstrap.checkInterval is empty", func() {
			rec := &LakekeeperReconciler{}
			lkEmpty := &lakekeeperv1alpha1.Lakekeeper{
				Spec: lakekeeperv1alpha1.LakekeeperSpec{
					Bootstrap: &lakekeeperv1alpha1.BootstrapConfig{},
				},
			}
			Expect(rec.getBootstrapCheckInterval(lkEmpty)).To(Equal(bootstrapPollInterval))
		})

		It("should return the custom interval when spec.bootstrap.checkInterval is set", func() {
			rec := &LakekeeperReconciler{}
			lkCustom := &lakekeeperv1alpha1.Lakekeeper{
				Spec: lakekeeperv1alpha1.LakekeeperSpec{
					Bootstrap: &lakekeeperv1alpha1.BootstrapConfig{
						CheckInterval: "30s",
					},
				},
			}
			Expect(rec.getBootstrapCheckInterval(lkCustom)).To(Equal(30 * time.Second))
		})
	})
})
