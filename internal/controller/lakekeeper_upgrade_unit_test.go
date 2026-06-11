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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	lakekeeperv1alpha1 "github.com/lakekeeper/lakekeeper-operator/api/v1alpha1"
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
