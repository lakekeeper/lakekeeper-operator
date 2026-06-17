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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	lakekeeperv1alpha1 "github.com/lakekeeper/lakekeeper-operator/api/v1alpha1"
)

// Shared literals for the upgrade unit specs (kept as constants to satisfy goconst).
const (
	unitNamespace     = "default"
	unitImageNp1      = "ghcr.io/lakekeeper/catalog:v0.12.4"
	unitClosedBaseURL = "http://127.0.0.1:1"
)

// minimalLakekeeper returns a spec sufficient for the env-building helpers.
func minimalLakekeeper() *lakekeeperv1alpha1.Lakekeeper {
	return &lakekeeperv1alpha1.Lakekeeper{
		Spec: lakekeeperv1alpha1.LakekeeperSpec{
			Image: "ghcr.io/lakekeeper/catalog:v0.12.3",
			Database: lakekeeperv1alpha1.DatabaseConfig{
				Type: lakekeeperv1alpha1.DatabaseTypePostgres,
				Postgres: &lakekeeperv1alpha1.PostgresConfig{
					Host:     "postgres",
					Database: "lakekeeper",
					User:     ptr.To("lakekeeper"),
				},
			},
			Authorization: lakekeeperv1alpha1.AuthorizationConfig{
				Backend: lakekeeperv1alpha1.AuthzBackendAllowAll,
			},
		},
	}
}

// Pure-helper unit specs for the read-only-gated upgrade. No k8sClient, no Reconcile.

var _ = Describe("deploymentRolloutComplete (unit)", func() {
	// rolledOut builds a Deployment whose .Status reflects a fully-settled rollout
	// of `desired` replicas; individual cases then perturb one field.
	rolledOut := func(desired int32) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Generation: 7},
			Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(desired)},
			Status: appsv1.DeploymentStatus{
				ObservedGeneration:  7,
				Replicas:            desired,
				UpdatedReplicas:     desired,
				AvailableReplicas:   desired,
				ReadyReplicas:       desired,
				UnavailableReplicas: 0,
			},
		}
	}

	It("returns true when the rollout has fully settled", func() {
		Expect(deploymentRolloutComplete(rolledOut(2))).To(BeTrue())
	})

	It("treats a nil spec.replicas as desired=1", func() {
		dep := rolledOut(1)
		dep.Spec.Replicas = nil
		Expect(deploymentRolloutComplete(dep)).To(BeTrue())
	})

	It("returns false when the controller has not observed the latest generation", func() {
		dep := rolledOut(2)
		dep.Status.ObservedGeneration = 6
		Expect(deploymentRolloutComplete(dep)).To(BeFalse())
	})

	It("returns false when not all replicas are updated to the new template", func() {
		dep := rolledOut(2)
		dep.Status.UpdatedReplicas = 1
		Expect(deploymentRolloutComplete(dep)).To(BeFalse())
	})

	It("returns false when surplus (old) replicas are still present", func() {
		dep := rolledOut(2)
		dep.Status.Replicas = 3
		Expect(deploymentRolloutComplete(dep)).To(BeFalse())
	})

	It("returns false when some replicas are unavailable", func() {
		dep := rolledOut(2)
		dep.Status.UnavailableReplicas = 1
		dep.Status.AvailableReplicas = 1
		Expect(deploymentRolloutComplete(dep)).To(BeFalse())
	})

	It("returns false when not all replicas are ready", func() {
		dep := rolledOut(2)
		dep.Status.ReadyReplicas = 1
		Expect(deploymentRolloutComplete(dep)).To(BeFalse())
	})
})

var _ = Describe("deploymentSettledOn (unit)", func() {
	// settled builds a 2-replica Deployment whose .Status reflects a fully-rolled-out
	// instance running `image` in the lakekeeper container.
	const desired = int32(2)
	settled := func(image string) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Generation: 3},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To(desired),
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "lakekeeper", Image: image}},
					},
				},
			},
			Status: appsv1.DeploymentStatus{
				ObservedGeneration:  3,
				Replicas:            desired,
				UpdatedReplicas:     desired,
				AvailableReplicas:   desired,
				ReadyReplicas:       desired,
				UnavailableReplicas: 0,
			},
		}
	}

	It("is true when the rollout has settled and the lakekeeper container runs the wanted image", func() {
		Expect(deploymentSettledOn(settled("img:N+1"), "img:N+1")).To(BeTrue())
	})

	It("is false when the rollout has settled but on a different image (the drift guard)", func() {
		// A fully-settled rollout on the OLD image must not be treated as converged
		// to the target — otherwise an externally-reverted Deployment would let the
		// upgrade complete with the wrong binary serving the migrated schema.
		Expect(deploymentSettledOn(settled("img:N"), "img:N+1")).To(BeFalse())
	})

	It("is false when the image matches but the rollout has not settled", func() {
		dep := settled("img:N+1")
		dep.Status.ReadyReplicas = 1
		Expect(deploymentSettledOn(dep, "img:N+1")).To(BeFalse())
	})

	It("is false when no lakekeeper container is present", func() {
		dep := settled("img:N+1")
		dep.Spec.Template.Spec.Containers = nil
		Expect(deploymentSettledOn(dep, "img:N+1")).To(BeFalse())
	})
})

var _ = Describe("buildDeploymentEnvVars (unit)", func() {
	var r *LakekeeperReconciler
	var lk *lakekeeperv1alpha1.Lakekeeper

	BeforeEach(func() {
		r = &LakekeeperReconciler{}
		lk = minimalLakekeeper()
	})

	const maintenanceVar = "LAKEKEEPER__MAINTENANCE_MODE"

	It("does not append the maintenance flag when maintenanceMode is false", func() {
		env := toEnvMap(r.buildDeploymentEnvVars(lk, false))
		_, present := env[maintenanceVar]
		Expect(present).To(BeFalse())
	})

	It("appends exactly one read-only maintenance flag when maintenanceMode is true", func() {
		raw := r.buildDeploymentEnvVars(lk, true)

		count := 0
		for _, e := range raw {
			if e.Name == maintenanceVar {
				count++
			}
		}
		Expect(count).To(Equal(1), "exactly one maintenance flag must be appended")
		Expect(toEnvMap(raw)[maintenanceVar].Value).To(Equal("read-only"))
	})

	It("preserves the base environment unchanged and unduplicated", func() {
		base := r.buildEnvVars(lk)
		withFlag := r.buildDeploymentEnvVars(lk, true)
		withoutFlag := r.buildDeploymentEnvVars(lk, false)

		// false path equals the base env exactly.
		Expect(withoutFlag).To(Equal(base))
		// true path is the base env plus exactly the one extra entry.
		Expect(withFlag).To(HaveLen(len(base) + 1))
		// A representative base var survives in both and is not duplicated.
		Expect(toEnvMap(withFlag)).To(HaveKey("LAKEKEEPER__AUTHZ_BACKEND"))
	})
})

var _ = DescribeTable("quiesceRequired (unit)",
	func(needsMigration, deploymentExists bool, liveImage, specImage string,
		replicas int32, strategy lakekeeperv1alpha1.UpgradeStrategy, expected bool) {
		Expect(quiesceRequired(needsMigration, deploymentExists, liveImage, specImage, replicas, strategy)).
			To(Equal(expected))
	},
	Entry("all conditions hold → quiesce",
		true, true, "img:N", "img:N+1", int32(2), lakekeeperv1alpha1.UpgradeStrategyReadOnlyMigration, true),
	Entry("no migration needed → no quiesce",
		false, true, "img:N", "img:N+1", int32(2), lakekeeperv1alpha1.UpgradeStrategyReadOnlyMigration, false),
	Entry("deployment does not exist yet (initial install) → no quiesce",
		true, false, "", "img:N+1", int32(2), lakekeeperv1alpha1.UpgradeStrategyReadOnlyMigration, false),
	Entry("live image already equals spec image → no quiesce",
		true, true, "img:N+1", "img:N+1", int32(2), lakekeeperv1alpha1.UpgradeStrategyReadOnlyMigration, false),
	Entry("scaled to zero → no quiesce",
		true, true, "img:N", "img:N+1", int32(0), lakekeeperv1alpha1.UpgradeStrategyReadOnlyMigration, false),
	Entry("Simple strategy → no quiesce",
		true, true, "img:N", "img:N+1", int32(2), lakekeeperv1alpha1.UpgradeStrategySimple, false),
)

var _ = DescribeTable("versionOlderThan (unit)",
	func(v, ref string, wantOlder, wantOK bool) {
		older, ok := versionOlderThan(v, ref)
		Expect(ok).To(Equal(wantOK))
		if wantOK {
			Expect(older).To(Equal(wantOlder))
		}
	},
	Entry("patch older", "0.12.2", "0.12.3", true, true),
	Entry("exact match is not older", "0.12.3", "0.12.3", false, true),
	Entry("patch newer", "0.12.4", "0.12.3", false, true),
	Entry("minor older", "0.11.9", "0.12.3", true, true),
	Entry("major newer", "1.0.0", "0.12.3", false, true),
	Entry("leading v is tolerated", "v0.12.2", "0.12.3", true, true),
	Entry("missing patch defaults to 0", "0.12", "0.12.3", true, true),
	Entry("pre-release suffix is stripped (treated as the release)", "0.12.3-rc1", "0.12.3", false, true),
	Entry("build metadata is stripped", "0.12.4+build7", "0.12.3", false, true),
	Entry("unparseable version reports ok=false", "not-a-version", "0.12.3", false, false),
	Entry("empty version reports ok=false", "", "0.12.3", false, false),
	Entry("four-part version reports ok=false", "1.2.3.4", "0.12.3", false, false),
)

var _ = Describe("migration hash invariance under maintenance flag (unit)", func() {
	var r *LakekeeperReconciler
	var lk *lakekeeperv1alpha1.Lakekeeper

	BeforeEach(func() {
		r = &LakekeeperReconciler{}
		lk = minimalLakekeeper()
	})

	It("does not change the migration hash when deployment env is built in either mode", func() {
		before := HashMigrationRelevantSpec(lk.Spec)
		_ = r.buildDeploymentEnvVars(lk, true)
		_ = r.buildDeploymentEnvVars(lk, false)
		Expect(HashMigrationRelevantSpec(lk.Spec)).To(Equal(before),
			"the maintenance flag is env-only and must never perturb the migration Job name")
	})

	It("never injects the maintenance flag into the migrate-Job env (buildEnvVars)", func() {
		Expect(toEnvMap(r.buildEnvVars(lk))).NotTo(HaveKey("LAKEKEEPER__MAINTENANCE_MODE"),
			"the migrate Job must be free to write the schema, so it must not run read-only")
	})
})

// Entry-path error handling, exercised with a fake client (no envtest store) so a
// transient read failure can be injected deterministically.
var _ = Describe("reconcileUpgrade entry-path error handling (unit)", func() {
	It("owns the cycle (done=true) when the live Deployment read fails while evaluating an upgrade", func() {
		lk := minimalLakekeeper()
		lk.Name = "entry-deperr"

		// needsMigration succeeds (Job is NotFound → migration needed), so the entry
		// path advances to liveDeploymentImage, whose Get is then forced to fail. A
		// transient read error must surface as done=true so controller-runtime requeues
		// with backoff rather than the error being silently dropped.
		c := fake.NewClientBuilder().
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey,
					obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*appsv1.Deployment); ok {
						return apierrors.NewServiceUnavailable("simulated transient API error")
					}
					return cl.Get(ctx, key, obj, opts...)
				},
			}).Build()
		r := &LakekeeperReconciler{Client: c, Scheme: c.Scheme()}

		_, done, err := r.reconcileUpgrade(context.Background(), lk)
		Expect(err).To(HaveOccurred())
		Expect(done).To(BeTrue(),
			"a transient read error during upgrade evaluation must own the reconcile cycle so the error triggers a requeue")
	})

	It("owns the cycle (done=true) when the migration-status check fails transiently", func() {
		lk := minimalLakekeeper()
		lk.Name = "entry-migerr"

		// A transient error reading the migration Job (not a permanent migration
		// failure) must surface as done=true so controller-runtime requeues.
		c := fake.NewClientBuilder().
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey,
					obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*batchv1.Job); ok {
						return apierrors.NewServiceUnavailable("simulated transient API error")
					}
					return cl.Get(ctx, key, obj, opts...)
				},
			}).Build()
		r := &LakekeeperReconciler{Client: c, Scheme: c.Scheme()}

		_, done, err := r.reconcileUpgrade(context.Background(), lk)
		Expect(err).To(HaveOccurred())
		Expect(done).To(BeTrue(),
			"a transient migration-status error must own the cycle so the error triggers a requeue")
	})

	It("clears stale upgrade bookkeeping and reports NotUpgrading when no upgrade applies", func() {
		lk := minimalLakekeeper()
		lk.Name = "entry-stale"
		lk.Namespace = unitNamespace
		// Phase already empty, but a target image and an Upgrading=True condition linger
		// (e.g. just after an upgrade settled). The target matches spec.Image, so the
		// mid-flight re-edit guard does not fire — the cleanup branch must.
		lk.Status.UpgradeTargetImage = lk.Spec.Image
		(&LakekeeperReconciler{}).setCondition(lk, TypeUpgrading, metav1.ConditionTrue, "Quiescing", "stale")

		// A succeeded migrate Job for this spec makes needsMigration return false, so
		// quiesceRequired is false and the cleanup branch runs.
		hash := HashMigrationRelevantSpec(lk.Spec)
		succeededJob := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      lk.Name + "-migrate-" + hash[:8],
				Namespace: lk.Namespace,
			},
			Status: batchv1.JobStatus{Succeeded: 1},
		}
		c := fake.NewClientBuilder().WithObjects(succeededJob).Build()
		r := &LakekeeperReconciler{Client: c, Scheme: c.Scheme()}

		_, done, err := r.reconcileUpgrade(context.Background(), lk)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeFalse(), "no upgrade applies, so the normal path must run this cycle")
		Expect(lk.Status.UpgradeTargetImage).To(BeEmpty(), "stale target image must be cleared")
		upg := meta.FindStatusCondition(lk.Status.Conditions, TypeUpgrading)
		Expect(upg).NotTo(BeNil())
		Expect(upg.Status).To(Equal(metav1.ConditionFalse))
		Expect(upg.Reason).To(Equal("NotUpgrading"))
	})
})

// syncBootstrapStatus defers the mutating bootstrap POST while an upgrade holds the
// server read-only. Exercised by calling syncBootstrapStatus directly: in the full
// Reconcile flow this guard is defensive (the normal tail that calls it runs only
// once reconcileUpgrade has cleared UpgradePhase), so it is pinned here as a unit
// contract — the same approach used for the unknown-phase reset above.
var _ = Describe("syncBootstrapStatus during an upgrade (unit)", func() {
	It("defers the auto-bootstrap POST and reports PausedDuringUpgrade", func() {
		var postSeen bool
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Method == http.MethodPost {
				postSeen = true
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(infoResponse{Bootstrapped: false, ServerID: "srv"})
		}))
		defer ts.Close()

		lk := minimalLakekeeper()
		lk.Name = "bootstrap-paused"
		lk.Spec.Bootstrap = &lakekeeperv1alpha1.BootstrapConfig{Enabled: ptr.To(true)}
		lk.Status.UpgradePhase = lakekeeperv1alpha1.UpgradePhaseQuiescing

		r := &LakekeeperReconciler{
			getBaseURL: func(_ *lakekeeperv1alpha1.Lakekeeper) string { return ts.URL },
		}
		Expect(r.syncBootstrapStatus(context.Background(), lk)).To(Succeed())

		cond := meta.FindStatusCondition(lk.Status.Conditions, TypeBootstrapped)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("PausedDuringUpgrade"))
		Expect(postSeen).To(BeFalse(),
			"the operator must not POST bootstrap against a read-only server during an upgrade")
	})
})

// The unknown-phase default branch in reconcileUpgrade is unreachable through the
// API (status.upgradePhase carries an Enum validation marker), so it is exercised
// here with a nil-client reconciler — the branch makes no client calls.
var _ = Describe("reconcileUpgrade unknown-phase reset (unit)", func() {
	It("clears an out-of-enum phase and hands control back to the normal path", func() {
		r := &LakekeeperReconciler{}
		lk := minimalLakekeeper()
		lk.Status.UpgradePhase = lakekeeperv1alpha1.UpgradePhase("Frobnicating")
		lk.Status.UpgradeTargetImage = lk.Spec.Image // matches spec, so the re-edit guard does not fire first

		result, done, err := r.reconcileUpgrade(context.Background(), lk)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeFalse(), "an unknown phase must defer to the normal reconcile path")
		Expect(result).To(Equal(ctrl.Result{}))
		Expect(lk.Status.UpgradePhase).To(BeEmpty())
		Expect(lk.Status.UpgradeTargetImage).To(BeEmpty())
	})
})

// upgradeUnitClient builds a fake client with the Lakekeeper status subresource
// enabled. The default scheme (scheme.Scheme) is populated with the Lakekeeper
// types by the suite's BeforeSuite, so no explicit scheme registration is needed —
// these remain pure unit specs (fake client, no envtest API server, no Reconcile).
func upgradeUnitClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithObjects(objs...).
		WithStatusSubresource(&lakekeeperv1alpha1.Lakekeeper{}).
		Build()
}

var _ = Describe("reconcileQuiescing drift guards (unit)", func() {
	It("advances to Migrating when the live Deployment has no lakekeeper container (empty image)", func() {
		// liveDeploymentImage reports ("", true, nil) for a Deployment that exists but
		// has no "lakekeeper" container. The quiesce drift check must treat an empty
		// live image as drift and advance to Migrating rather than rendering an empty
		// image and stalling forever.
		lk := minimalLakekeeper()
		lk.Name = "quiesce-noimage"
		lk.Namespace = unitNamespace
		lk.Spec.Image = unitImageNp1
		lk.Status.UpgradePhase = lakekeeperv1alpha1.UpgradePhaseQuiescing
		lk.Status.UpgradeTargetImage = lk.Spec.Image

		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: lk.Name, Namespace: lk.Namespace},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "not-lakekeeper", Image: "x"}},
					},
				},
			},
		}
		c := upgradeUnitClient(lk, dep)
		r := &LakekeeperReconciler{Client: c, Scheme: c.Scheme()}

		_, done, err := r.reconcileQuiescing(context.Background(), lk)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeTrue())
		Expect(lk.Status.UpgradePhase).To(Equal(lakekeeperv1alpha1.UpgradePhaseMigrating),
			"an empty live image must be treated as drift and advance to Migrating, not render an empty image")
	})
})

var _ = Describe("advanceTo (unit)", func() {
	It("re-asserts Migrated=True and the Job name when transitioning out of Migrating to RollingOut", func() {
		// reconcileMigration writes Migrated=True best-effort; if that write is dropped,
		// advanceTo must re-assert it in its single status write so the condition cannot
		// flap on the Migrating→RollingOut transition.
		lk := minimalLakekeeper()
		lk.Name = "advance-mig"
		lk.Namespace = unitNamespace
		lk.Status.UpgradePhase = lakekeeperv1alpha1.UpgradePhaseMigrating
		// Migrated condition and MigrationJob deliberately absent (dropped best-effort write).
		c := upgradeUnitClient(lk)
		r := &LakekeeperReconciler{Client: c, Scheme: c.Scheme()}

		_, done, err := r.advanceTo(context.Background(), lk, lakekeeperv1alpha1.UpgradePhaseRollingOut, "rolling out")
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeTrue())
		Expect(lk.Status.UpgradePhase).To(Equal(lakekeeperv1alpha1.UpgradePhaseRollingOut))
		Expect(meta.IsStatusConditionTrue(lk.Status.Conditions, TypeMigrated)).To(BeTrue(),
			"leaving Migrating must re-assert Migrated=True so a dropped best-effort write cannot leave it flapping")
		expectedJob := fmt.Sprintf("%s-migrate-%s", lk.Name, HashMigrationRelevantSpec(lk.Spec)[:8])
		Expect(lk.Status.MigrationJob).To(Equal(expectedJob),
			"leaving Migrating must ensure the MigrationJob name is recorded")
	})

	It("does not pre-assert Migrated when entering Migrating from Quiescing", func() {
		lk := minimalLakekeeper()
		lk.Name = "advance-quiesce"
		lk.Namespace = unitNamespace
		lk.Status.UpgradePhase = lakekeeperv1alpha1.UpgradePhaseQuiescing
		c := upgradeUnitClient(lk)
		r := &LakekeeperReconciler{Client: c, Scheme: c.Scheme()}

		_, _, err := r.advanceTo(context.Background(), lk, lakekeeperv1alpha1.UpgradePhaseMigrating, "migrating")
		Expect(err).NotTo(HaveOccurred())
		Expect(meta.IsStatusConditionTrue(lk.Status.Conditions, TypeMigrated)).To(BeFalse(),
			"entering Migrating must not pre-assert Migrated=True before the migration has run")
	})
})

var _ = Describe("enterUpgradeIfNeeded concurrent migration guard (unit)", func() {
	It("defers entering Quiescing while a previous image's migration Job is still in-flight", func() {
		lk := minimalLakekeeper()
		lk.Name = "concurrent-mig"
		lk.Namespace = unitNamespace
		lk.Spec.Image = unitImageNp1 // new target N+1

		// A live Deployment on the old image N (so quiesceRequired holds: it exists on a
		// different image with replicas > 0 under the default ReadOnlyMigration strategy).
		oldImage := "ghcr.io/lakekeeper/catalog:v0.12.3"
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: lk.Name, Namespace: lk.Namespace},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "lakekeeper", Image: oldImage}},
					},
				},
			},
		}

		// An in-flight migration Job for the OLD image (different spec hash, still active).
		oldSpec := lk.Spec
		oldSpec.Image = oldImage
		oldHash := HashMigrationRelevantSpec(oldSpec)
		oldJob := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-migrate-%s", lk.Name, oldHash[:8]),
				Namespace: lk.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/component": "migration",
					"app.kubernetes.io/instance":  lk.Name,
				},
			},
			Status: batchv1.JobStatus{Active: 1},
		}

		c := upgradeUnitClient(lk, dep, oldJob)
		r := &LakekeeperReconciler{
			Client: c,
			Scheme: c.Scheme(),
			// A closed port keeps the version probe fast in case the guard does not fire.
			getBaseURL: func(_ *lakekeeperv1alpha1.Lakekeeper) string { return unitClosedBaseURL },
		}

		result, done, err := r.enterUpgradeIfNeeded(context.Background(), lk)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeTrue(), "the guard must own the reconcile cycle and requeue")
		Expect(result.RequeueAfter).To(Equal(upgradeRequeueAfter))
		Expect(lk.Status.UpgradePhase).To(BeEmpty(),
			"must not enter Quiescing while a previous image's migration Job is still in-flight")
	})

	It("enters Quiescing once the previous migration Jobs have all completed", func() {
		lk := minimalLakekeeper()
		lk.Name = "concurrent-done"
		lk.Namespace = unitNamespace
		lk.Spec.Image = unitImageNp1

		oldImage := "ghcr.io/lakekeeper/catalog:v0.12.3"
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: lk.Name, Namespace: lk.Namespace},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "lakekeeper", Image: oldImage}},
					},
				},
			},
		}
		oldSpec := lk.Spec
		oldSpec.Image = oldImage
		oldHash := HashMigrationRelevantSpec(oldSpec)
		oldJob := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-migrate-%s", lk.Name, oldHash[:8]),
				Namespace: lk.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/component": "migration",
					"app.kubernetes.io/instance":  lk.Name,
				},
			},
			Status: batchv1.JobStatus{Succeeded: 1}, // completed
		}

		c := upgradeUnitClient(lk, dep, oldJob)
		r := &LakekeeperReconciler{
			Client:     c,
			Scheme:     c.Scheme(),
			getBaseURL: func(_ *lakekeeperv1alpha1.Lakekeeper) string { return unitClosedBaseURL },
		}

		_, done, err := r.enterUpgradeIfNeeded(context.Background(), lk)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeTrue())
		Expect(lk.Status.UpgradePhase).To(Equal(lakekeeperv1alpha1.UpgradePhaseQuiescing),
			"a completed previous migration must not block entering Quiescing")
	})
})

var _ = Describe("reconcileUpgrade entry-path permanent migration failure (unit)", func() {
	It("defers to the normal path (done=false) when the current spec's migration has permanently failed", func() {
		// A terminally-failed migration Job for the current spec makes needsMigration
		// return errMigrationFailed. The upgrade entry must not start a new upgrade on
		// top of it — the normal migration path surfaces the failure.
		lk := minimalLakekeeper()
		lk.Name = "entry-migfailed"
		lk.Namespace = unitNamespace
		hash := HashMigrationRelevantSpec(lk.Spec)
		failedJob := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-migrate-%s", lk.Name, hash[:8]),
				Namespace: lk.Namespace,
			},
			Status: batchv1.JobStatus{
				Failed: migrationJobBackoffLimit,
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
				},
			},
		}
		c := upgradeUnitClient(lk, failedJob)
		r := &LakekeeperReconciler{Client: c, Scheme: c.Scheme()}

		_, done, err := r.reconcileUpgrade(context.Background(), lk)
		Expect(err).NotTo(HaveOccurred())
		Expect(done).To(BeFalse(), "a permanent migration failure is surfaced by the normal path, not the upgrade entry")
		Expect(lk.Status.UpgradePhase).To(BeEmpty())
	})
})

var _ = Describe("rolloutComplete (unit)", func() {
	It("returns an error when the live Deployment cannot be fetched", func() {
		lk := minimalLakekeeper()
		lk.Name = "rollout-geterr"
		lk.Namespace = unitNamespace
		c := fake.NewClientBuilder().
			WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey,
					obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*appsv1.Deployment); ok {
						return apierrors.NewServiceUnavailable("simulated transient API error")
					}
					return cl.Get(ctx, key, obj, opts...)
				},
			}).Build()
		r := &LakekeeperReconciler{Client: c, Scheme: c.Scheme()}

		_, err := r.rolloutComplete(context.Background(), lk, "img:N+1")
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("reconcileVersionWarning Version fallback (unit)", func() {
	It("falls back to the deprecated Version field when LakekeeperVersion is empty", func() {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			// Old server: only the deprecated plain `version` field is populated.
			_ = json.NewEncoder(w).Encode(infoResponse{Bootstrapped: true, ServerID: "srv", Version: "0.12.2"})
		}))
		defer ts.Close()

		lk := minimalLakekeeper()
		lk.Name = "ver-fallback"
		r := &LakekeeperReconciler{getBaseURL: func(_ *lakekeeperv1alpha1.Lakekeeper) string { return ts.URL }}

		r.reconcileVersionWarning(context.Background(), lk)

		warn := meta.FindStatusCondition(lk.Status.Conditions, TypeUpgradeWarning)
		Expect(warn).NotTo(BeNil())
		Expect(warn.Status).To(Equal(metav1.ConditionTrue))
		Expect(warn.Reason).To(Equal("MaintenanceModeUnsupported"))
	})
})

var _ = Describe("degradedMigrationMessage (unit)", func() {
	It("returns the plain permanent-failure message outside the Migrating phase", func() {
		lk := minimalLakekeeper()
		lk.Status.UpgradePhase = ""
		Expect(degradedMigrationMessage(lk)).To(Equal("Database migration failed permanently"))
	})

	It("returns read-only recovery guidance during the Migrating phase", func() {
		lk := minimalLakekeeper()
		lk.Status.UpgradePhase = lakekeeperv1alpha1.UpgradePhaseMigrating
		Expect(degradedMigrationMessage(lk)).To(ContainSubstring("read-only maintenance mode"))
	})
})
