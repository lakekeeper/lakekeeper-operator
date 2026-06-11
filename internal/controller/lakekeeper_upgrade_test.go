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
	"fmt"
	"net/http"
	"net/http/httptest"
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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	lakekeeperv1alpha1 "github.com/lakekeeper/lakekeeper-operator/api/v1alpha1"
)

// Integration specs (envtest) for the read-only-gated upgrade state machine.
// These drive Reconcile directly and manipulate Deployment/Job .Status to simulate
// the cluster components envtest does not run.

var _ = Describe("Lakekeeper upgrade choreography (integration)", func() {
	const (
		timeout   = time.Second * 10
		interval  = time.Millisecond * 250
		namespace = "default"
		imageN    = "ghcr.io/lakekeeper/catalog:v0.12.3"
		imageNp1  = "ghcr.io/lakekeeper/catalog:v0.12.4"
	)

	var (
		ctx        context.Context
		reconciler *LakekeeperReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()

		By("Creating the required db secret")
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "db-secret", Namespace: namespace},
			StringData: map[string]string{
				"password":       "supersecret",
				"encryption-key": "encryption-key-value",
			},
		}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: secret.Name, Namespace: namespace},
			&corev1.Secret{}); err != nil && errors.IsNotFound(err) {
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		}

		reconciler = &LakekeeperReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			// Point the management API at a closed port so bootstrap/version probes
			// fail fast instead of hanging on cluster DNS.
			getBaseURL: func(_ *lakekeeperv1alpha1.Lakekeeper) string { return "http://127.0.0.1:1" },
		}
	})

	AfterEach(func() {
		secret := &corev1.Secret{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: "db-secret", Namespace: namespace},
			secret); err == nil {
			Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
		}
	})

	// newLakekeeper builds an allowall Lakekeeper CR on the given image.
	newLakekeeper := func(name, image string, replicas int32, strategy lakekeeperv1alpha1.UpgradeStrategy) *lakekeeperv1alpha1.Lakekeeper {
		lk := &lakekeeperv1alpha1.Lakekeeper{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: lakekeeperv1alpha1.LakekeeperSpec{
				Image:    image,
				Replicas: ptr.To(replicas),
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
		if strategy != "" {
			lk.Spec.Upgrade = &lakekeeperv1alpha1.UpgradeConfig{Strategy: strategy}
		}
		return lk
	}

	reconcileOnce := func(name string) (reconcile.Result, error) {
		return reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
		})
	}

	getLK := func(name string) *lakekeeperv1alpha1.Lakekeeper {
		lk := &lakekeeperv1alpha1.Lakekeeper{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, lk)).To(Succeed())
		return lk
	}

	getDeployment := func(name string) *appsv1.Deployment {
		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, dep)).To(Succeed())
		return dep
	}

	// simulateRolloutComplete marks the named Deployment as fully rolled out so
	// deploymentRolloutComplete returns true on the next reconcile.
	simulateRolloutComplete := func(name string) {
		dep := &appsv1.Deployment{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, dep)
		}, timeout, interval).Should(Succeed())
		desired := int32(1)
		if dep.Spec.Replicas != nil {
			desired = *dep.Spec.Replicas
		}
		dep.Status.ObservedGeneration = dep.Generation
		dep.Status.Replicas = desired
		dep.Status.UpdatedReplicas = desired
		dep.Status.AvailableReplicas = desired
		dep.Status.ReadyReplicas = desired
		dep.Status.UnavailableReplicas = 0
		Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())
	}

	// driveToSteady brings a freshly-created CR to a steady, ready state on its
	// current image: finalizer, migration, deployment, ready status.
	driveToSteady := func(lk *lakekeeperv1alpha1.Lakekeeper) {
		Expect(k8sClient.Create(ctx, lk)).To(Succeed())
		_, err := reconcileOnce(lk.Name) // finalizer
		Expect(err).NotTo(HaveOccurred())
		_, err = reconcileOnce(lk.Name) // migration Job
		Expect(err).NotTo(HaveOccurred())
		Expect(completeMigrationJob(ctx, getLK(lk.Name), timeout)).To(Succeed())
		_, err = reconcileOnce(lk.Name) // deployment
		Expect(err).NotTo(HaveOccurred())
		simulateRolloutComplete(lk.Name)
		_, err = reconcileOnce(lk.Name) // status
		Expect(err).NotTo(HaveOccurred())
	}

	envOf := func(dep *appsv1.Deployment) map[string]corev1.EnvVar {
		return toEnvMap(dep.Spec.Template.Spec.Containers[0].Env)
	}

	migrationJobExists := func(lk *lakekeeperv1alpha1.Lakekeeper) (*batchv1.Job, bool) {
		hash := HashMigrationRelevantSpec(lk.Spec)
		job := &batchv1.Job{}
		name := fmt.Sprintf("%s-migrate-%s", lk.Name, hash[:8])
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, job)
		return job, err == nil
	}

	countMigrationJobs := func(instance string) int {
		jobs := &batchv1.JobList{}
		Expect(k8sClient.List(ctx, jobs,
			client.InNamespace(namespace),
			client.MatchingLabels{
				"app.kubernetes.io/component": "migration",
				"app.kubernetes.io/instance":  instance,
			})).To(Succeed())
		return len(jobs.Items)
	}

	reconcileUntilPhase := func(name string, phase lakekeeperv1alpha1.UpgradePhase) {
		Eventually(func(g Gomega) {
			_, err := reconcileOnce(name)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(getLK(name).Status.UpgradePhase).To(Equal(phase))
		}, timeout, interval).Should(Succeed())
	}

	// driveUpgradeToCompletion reconciles repeatedly, nudging the simulated cluster
	// (completing in-progress migrate Jobs and marking rollouts done) until the
	// upgrade converges to targetImage with the phase cleared.
	driveUpgradeToCompletion := func(name, targetImage string) {
		Eventually(func(g Gomega) {
			_, err := reconcileOnce(name)
			g.Expect(err).NotTo(HaveOccurred())
			lk := getLK(name)
			if job, ok := migrationJobExists(lk); ok && job.Status.Succeeded == 0 && job.Status.Failed == 0 {
				_ = completeMigrationJob(ctx, lk, timeout)
			}
			simulateRolloutComplete(name)
			g.Expect(getLK(name).Status.UpgradePhase).To(BeEmpty())
			g.Expect(getDeployment(name).Spec.Template.Spec.Containers[0].Image).To(Equal(targetImage))
		}, timeout*3, interval).Should(Succeed())
	}

	Context("ReadOnlyMigration strategy (default)", func() {
		It("walks Quiescing → Migrating → RollingOut → done on an image upgrade", func() {
			name := "upgrade-happy"
			lk := newLakekeeper(name, imageN, 2, "")
			driveToSteady(lk)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, lk) })

			By("capturing the steady-state migration Job name (image N)")
			steadyLK := getLK(name)
			_, hadJobN := migrationJobExists(steadyLK)
			Expect(hadJobN).To(BeTrue())

			By("patching spec.image to N+1")
			toUpdate := getLK(name)
			toUpdate.Spec.Image = imageNp1
			Expect(k8sClient.Update(ctx, toUpdate)).To(Succeed())

			By("entering Quiescing: old image kept, read-only flag added, no new-image Job yet")
			Eventually(func(g Gomega) {
				_, err := reconcileOnce(name)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(getLK(name).Status.UpgradePhase).To(Equal(lakekeeperv1alpha1.UpgradePhaseQuiescing))
			}, timeout, interval).Should(Succeed())

			lkQ := getLK(name)
			Expect(lkQ.Status.UpgradeTargetImage).To(Equal(imageNp1))
			Expect(meta.IsStatusConditionTrue(lkQ.Status.Conditions, TypeUpgrading)).To(BeTrue())

			Eventually(func(g Gomega) {
				_, err := reconcileOnce(name)
				g.Expect(err).NotTo(HaveOccurred())
				dep := getDeployment(name)
				g.Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal(imageN))
				g.Expect(envOf(dep)).To(HaveKeyWithValue("LAKEKEEPER__MAINTENANCE_MODE",
					corev1.EnvVar{Name: "LAKEKEEPER__MAINTENANCE_MODE", Value: "read-only"}))
			}, timeout, interval).Should(Succeed())

			By("verifying the surge strategy is set to maxUnavailable=0 / maxSurge=1")
			depQ := getDeployment(name)
			Expect(depQ.Spec.Strategy.Type).To(Equal(appsv1.RollingUpdateDeploymentStrategyType))
			Expect(depQ.Spec.Strategy.RollingUpdate.MaxUnavailable.IntValue()).To(Equal(0))
			Expect(depQ.Spec.Strategy.RollingUpdate.MaxSurge.IntValue()).To(Equal(1))

			By("verifying no migration Job exists yet for the new image")
			_, hasJobNp1 := migrationJobExists(getLK(name))
			Expect(hasJobNp1).To(BeFalse(), "the new-image migrate Job must not be created during Quiescing")

			By("advancing to Migrating once the read-only rollout completes")
			simulateRolloutComplete(name)
			Eventually(func(g Gomega) {
				_, err := reconcileOnce(name)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(getLK(name).Status.UpgradePhase).To(Equal(lakekeeperv1alpha1.UpgradePhaseMigrating))
			}, timeout, interval).Should(Succeed())

			By("verifying the new-image migrate Job is created (without the maintenance flag) during Migrating")
			var job *batchv1.Job
			Eventually(func(g Gomega) {
				_, err := reconcileOnce(name)
				g.Expect(err).NotTo(HaveOccurred())
				var hasJob bool
				job, hasJob = migrationJobExists(getLK(name))
				g.Expect(hasJob).To(BeTrue())
			}, timeout, interval).Should(Succeed())
			Expect(toEnvMap(job.Spec.Template.Spec.Containers[0].Env)).
				NotTo(HaveKey("LAKEKEEPER__MAINTENANCE_MODE"))

			By("advancing to RollingOut on migration success")
			Expect(completeMigrationJob(ctx, getLK(name), timeout)).To(Succeed())
			Eventually(func(g Gomega) {
				_, err := reconcileOnce(name)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(getLK(name).Status.UpgradePhase).To(Equal(lakekeeperv1alpha1.UpgradePhaseRollingOut))
			}, timeout, interval).Should(Succeed())

			By("rolling out the new image with the flag removed")
			Eventually(func(g Gomega) {
				_, err := reconcileOnce(name)
				g.Expect(err).NotTo(HaveOccurred())
				dep := getDeployment(name)
				g.Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal(imageNp1))
				g.Expect(envOf(dep)).NotTo(HaveKey("LAKEKEEPER__MAINTENANCE_MODE"))
			}, timeout, interval).Should(Succeed())

			By("clearing the phase and reaching Ready once the new rollout completes")
			simulateRolloutComplete(name)
			Eventually(func(g Gomega) {
				_, err := reconcileOnce(name)
				g.Expect(err).NotTo(HaveOccurred())
				lk := getLK(name)
				g.Expect(lk.Status.UpgradePhase).To(BeEmpty())
				g.Expect(lk.Status.UpgradeTargetImage).To(BeEmpty())
				upg := meta.FindStatusCondition(lk.Status.Conditions, TypeUpgrading)
				g.Expect(upg).NotTo(BeNil())
				g.Expect(upg.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(upg.Reason).To(Equal("UpgradeComplete"))
				g.Expect(meta.IsStatusConditionTrue(lk.Status.Conditions, TypeReady)).To(BeTrue())
			}, timeout, interval).Should(Succeed())
		})

		It("does not quiesce on initial install (phase stays empty, no flag)", func() {
			name := "upgrade-install"
			lk := newLakekeeper(name, imageN, 1, "")
			driveToSteady(lk)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, lk) })

			Expect(getLK(name).Status.UpgradePhase).To(BeEmpty())
			Expect(envOf(getDeployment(name))).NotTo(HaveKey("LAKEKEEPER__MAINTENANCE_MODE"))
		})

		It("does not quiesce on a non-migration change (replicas only)", func() {
			name := "upgrade-replicas"
			lk := newLakekeeper(name, imageN, 1, "")
			driveToSteady(lk)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, lk) })

			By("scaling replicas without changing the image")
			toUpdate := getLK(name)
			toUpdate.Spec.Replicas = ptr.To(int32(3))
			Expect(k8sClient.Update(ctx, toUpdate)).To(Succeed())

			Consistently(func(g Gomega) {
				_, err := reconcileOnce(name)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(getLK(name).Status.UpgradePhase).To(BeEmpty())
				g.Expect(envOf(getDeployment(name))).NotTo(HaveKey("LAKEKEEPER__MAINTENANCE_MODE"))
			}, "2s", interval).Should(Succeed())
		})

		It("skips quiesce when scaled to zero, taking the normal migrate path", func() {
			name := "upgrade-zero"
			lk := newLakekeeper(name, imageN, 0, "")
			driveToSteady(lk)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, lk) })

			By("changing the image while scaled to zero")
			toUpdate := getLK(name)
			toUpdate.Spec.Image = imageNp1
			Expect(k8sClient.Update(ctx, toUpdate)).To(Succeed())

			Consistently(func(g Gomega) {
				_, err := reconcileOnce(name)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(getLK(name).Status.UpgradePhase).To(BeEmpty())
			}, "2s", interval).Should(Succeed())

			By("the new-image migrate Job is created via the normal path")
			_, ok := migrationJobExists(getLK(name))
			Expect(ok).To(BeTrue())
		})

		It("stays in Migrating with Degraded=True on permanent migration failure, then recovers on a spec.image re-edit", func() {
			name := "upgrade-migfail"
			lk := newLakekeeper(name, imageN, 1, "")
			driveToSteady(lk)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, lk) })

			By("triggering the upgrade and reaching Migrating")
			toUpdate := getLK(name)
			toUpdate.Spec.Image = imageNp1
			Expect(k8sClient.Update(ctx, toUpdate)).To(Succeed())
			reconcileUntilPhase(name, lakekeeperv1alpha1.UpgradePhaseQuiescing)
			Eventually(func(g Gomega) {
				_, err := reconcileOnce(name)
				g.Expect(err).NotTo(HaveOccurred())
				simulateRolloutComplete(name)
				g.Expect(getLK(name).Status.UpgradePhase).To(Equal(lakekeeperv1alpha1.UpgradePhaseMigrating))
			}, timeout, interval).Should(Succeed())

			By("failing the migration Job permanently")
			Eventually(func(g Gomega) {
				_, err := reconcileOnce(name)
				g.Expect(err).NotTo(HaveOccurred())
				_, ok := migrationJobExists(getLK(name))
				g.Expect(ok).To(BeTrue())
			}, timeout, interval).Should(Succeed())
			Expect(simulateMigrationJobFailure(ctx, getLK(name), migrationJobBackoffLimit, timeout)).To(Succeed())

			By("staying in Migrating, Degraded=True, with old image + read-only flag intact")
			Eventually(func(g Gomega) {
				_, err := reconcileOnce(name)
				g.Expect(err).NotTo(HaveOccurred())
				lk := getLK(name)
				g.Expect(lk.Status.UpgradePhase).To(Equal(lakekeeperv1alpha1.UpgradePhaseMigrating))
				g.Expect(meta.IsStatusConditionTrue(lk.Status.Conditions, TypeDegraded)).To(BeTrue())
			}, timeout, interval).Should(Succeed())
			depFail := getDeployment(name)
			Expect(depFail.Spec.Template.Spec.Containers[0].Image).To(Equal(imageN))
			Expect(envOf(depFail)).To(HaveKey("LAKEKEEPER__MAINTENANCE_MODE"))

			By("recovering by rolling spec.image back to N (re-edit resets the machine)")
			toRollback := getLK(name)
			toRollback.Spec.Image = imageN
			Expect(k8sClient.Update(ctx, toRollback)).To(Succeed())
			driveUpgradeToCompletion(name, imageN)
			Expect(envOf(getDeployment(name))).NotTo(HaveKey("LAKEKEEPER__MAINTENANCE_MODE"))
		})

		It("retargets on a mid-Quiescing re-edit (N→N+1→N+2) and converges to N+2", func() {
			name := "upgrade-reedit"
			lk := newLakekeeper(name, imageN, 1, "")
			driveToSteady(lk)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, lk) })
			imageNp2 := "ghcr.io/lakekeeper/catalog:v0.12.5"

			By("starting an upgrade to N+1 and reaching Quiescing")
			toNp1 := getLK(name)
			toNp1.Spec.Image = imageNp1
			Expect(k8sClient.Update(ctx, toNp1)).To(Succeed())
			reconcileUntilPhase(name, lakekeeperv1alpha1.UpgradePhaseQuiescing)
			Expect(getLK(name).Status.UpgradeTargetImage).To(Equal(imageNp1))

			By("re-editing to N+2 mid-Quiescing")
			toNp2 := getLK(name)
			toNp2.Spec.Image = imageNp2
			Expect(k8sClient.Update(ctx, toNp2)).To(Succeed())

			Eventually(func(g Gomega) {
				_, err := reconcileOnce(name)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(getLK(name).Status.UpgradeTargetImage).To(Equal(imageNp2))
			}, timeout, interval).Should(Succeed())

			By("converging to N+2")
			driveUpgradeToCompletion(name, imageNp2)
		})

		It("is resumable across a fresh reconciler without creating duplicate Jobs", func() {
			name := "upgrade-resume"
			lk := newLakekeeper(name, imageN, 1, "")
			driveToSteady(lk)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, lk) })

			By("entering Quiescing with the original reconciler")
			toUpdate := getLK(name)
			toUpdate.Spec.Image = imageNp1
			Expect(k8sClient.Update(ctx, toUpdate)).To(Succeed())
			reconcileUntilPhase(name, lakekeeperv1alpha1.UpgradePhaseQuiescing)

			By("swapping in a fresh reconciler instance and converging")
			reconciler = &LakekeeperReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				getBaseURL: func(_ *lakekeeperv1alpha1.Lakekeeper) string { return "http://127.0.0.1:1" },
			}
			driveUpgradeToCompletion(name, imageNp1)

			By("exactly two migrate Jobs exist (one per image), with no duplicates")
			Expect(countMigrationJobs(name)).To(Equal(2))
		})

		It("drift guard: re-enters at Migrating (not RollingOut) when the Deployment is deleted mid-Quiescing", func() {
			name := "upgrade-drift-gone"
			lk := newLakekeeper(name, imageN, 1, "")
			driveToSteady(lk)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, lk) })

			By("entering Quiescing on an image upgrade")
			toUpdate := getLK(name)
			toUpdate.Spec.Image = imageNp1
			Expect(k8sClient.Update(ctx, toUpdate)).To(Succeed())
			reconcileUntilPhase(name, lakekeeperv1alpha1.UpgradePhaseQuiescing)

			By("deleting the Deployment out-of-band while Quiescing")
			Expect(k8sClient.Delete(ctx, getDeployment(name))).To(Succeed())

			By("re-entering at Migrating, never skipping straight to RollingOut")
			reconcileUntilPhase(name, lakekeeperv1alpha1.UpgradePhaseMigrating)

			By("creating the new-image migrate Job while no Deployment has been recreated")
			Eventually(func(g Gomega) {
				_, err := reconcileOnce(name)
				g.Expect(err).NotTo(HaveOccurred())
				_, ok := migrationJobExists(getLK(name))
				g.Expect(ok).To(BeTrue())
			}, timeout, interval).Should(Succeed())
			// The migration must run before any new-image pods are recreated, so the
			// Deployment is still absent. The old buggy path (jump to RollingOut)
			// would have recreated it on the new image before migrating.
			depErr := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &appsv1.Deployment{})
			Expect(errors.IsNotFound(depErr)).To(BeTrue(),
				"the new-image Deployment must not be recreated until the migration completes")

			By("converging to N+1 once migration and rollout complete")
			Expect(completeMigrationJob(ctx, getLK(name), timeout)).To(Succeed())
			// Reconcile until RollingOut has recreated the Deployment, then let the
			// shared convergence helper (which needs the Deployment to exist) finish.
			Eventually(func(g Gomega) {
				_, err := reconcileOnce(name)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(k8sClient.Get(ctx,
					types.NamespacedName{Name: name, Namespace: namespace}, &appsv1.Deployment{})).To(Succeed())
			}, timeout, interval).Should(Succeed())
			driveUpgradeToCompletion(name, imageNp1)
		})

		It("drift guard: re-enters at Migrating when the Deployment is already on the target image", func() {
			name := "upgrade-drift-ontarget"
			lk := newLakekeeper(name, imageN, 1, "")
			driveToSteady(lk)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, lk) })

			By("entering Quiescing on an image upgrade")
			toUpdate := getLK(name)
			toUpdate.Spec.Image = imageNp1
			Expect(k8sClient.Update(ctx, toUpdate)).To(Succeed())
			reconcileUntilPhase(name, lakekeeperv1alpha1.UpgradePhaseQuiescing)

			By("patching the Deployment image to the target out-of-band")
			dep := getDeployment(name)
			dep.Spec.Template.Spec.Containers[0].Image = imageNp1
			Expect(k8sClient.Update(ctx, dep)).To(Succeed())

			By("re-entering at Migrating rather than skipping to RollingOut")
			reconcileUntilPhase(name, lakekeeperv1alpha1.UpgradePhaseMigrating)

			By("converging to N+1")
			driveUpgradeToCompletion(name, imageNp1)
		})

		// NOTE: the unknown-phase default branch in reconcileUpgrade cannot be reached
		// through the API — status.upgradePhase carries an Enum validation marker, so
		// the API server rejects any out-of-enum value. It is covered as a unit test
		// (fake client, no CRD validation) in lakekeeper_upgrade_unit_test.go.
	})

	Context("version compatibility warning", func() {
		var ts *httptest.Server

		AfterEach(func() {
			if ts != nil {
				ts.Close()
				ts = nil
			}
		})

		It("raises UpgradeWarning when the running server predates MAINTENANCE_MODE", func() {
			name := "upgrade-oldver"
			lk := newLakekeeper(name, imageN, 1, "")
			driveToSteady(lk)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, lk) })

			By("pointing the management API at a server reporting an old version")
			ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(infoResponse{
					Bootstrapped:      true,
					ServerID:          "srv-1",
					LakekeeperVersion: "0.12.2",
				})
			}))
			reconciler = &LakekeeperReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				getBaseURL: func(_ *lakekeeperv1alpha1.Lakekeeper) string { return ts.URL },
			}

			By("triggering an upgrade and entering Quiescing")
			toUpdate := getLK(name)
			toUpdate.Spec.Image = imageNp1
			Expect(k8sClient.Update(ctx, toUpdate)).To(Succeed())
			reconcileUntilPhase(name, lakekeeperv1alpha1.UpgradePhaseQuiescing)

			By("the UpgradeWarning condition is set True with MaintenanceModeUnsupported")
			warn := meta.FindStatusCondition(getLK(name).Status.Conditions, TypeUpgradeWarning)
			Expect(warn).NotTo(BeNil())
			Expect(warn.Status).To(Equal(metav1.ConditionTrue))
			Expect(warn.Reason).To(Equal("MaintenanceModeUnsupported"))
		})
	})

	Context("Simple strategy", func() {
		It("never quiesces even when the image changes", func() {
			name := "upgrade-simple"
			lk := newLakekeeper(name, imageN, 1, lakekeeperv1alpha1.UpgradeStrategySimple)
			driveToSteady(lk)
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, lk) })

			By("changing the image under the Simple strategy")
			toUpdate := getLK(name)
			toUpdate.Spec.Image = imageNp1
			Expect(k8sClient.Update(ctx, toUpdate)).To(Succeed())

			By("the phase never becomes non-empty and no maintenance flag is ever set")
			Consistently(func(g Gomega) {
				_, err := reconcileOnce(name)
				g.Expect(err).NotTo(HaveOccurred())
				if job, ok := migrationJobExists(getLK(name)); ok && job.Status.Succeeded == 0 && job.Status.Failed == 0 {
					_ = completeMigrationJob(ctx, getLK(name), timeout)
				}
				simulateRolloutComplete(name)
				g.Expect(getLK(name).Status.UpgradePhase).To(BeEmpty())
				g.Expect(envOf(getDeployment(name))).NotTo(HaveKey("LAKEKEEPER__MAINTENANCE_MODE"))
			}, "3s", interval).Should(Succeed())

			By("the normal migrate-then-deploy path converges to N+1")
			Eventually(func(g Gomega) {
				_, err := reconcileOnce(name)
				g.Expect(err).NotTo(HaveOccurred())
				if job, ok := migrationJobExists(getLK(name)); ok && job.Status.Succeeded == 0 && job.Status.Failed == 0 {
					_ = completeMigrationJob(ctx, getLK(name), timeout)
				}
				simulateRolloutComplete(name)
				g.Expect(getDeployment(name).Spec.Template.Spec.Containers[0].Image).To(Equal(imageNp1))
			}, timeout, interval).Should(Succeed())
		})
	})
})
