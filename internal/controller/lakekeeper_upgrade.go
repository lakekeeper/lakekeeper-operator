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
	"errors"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	lakekeeperv1alpha1 "github.com/lakekeeper/lakekeeper-operator/api/v1alpha1"
)

// reconcileUpgrade drives the read-only-gated upgrade state machine.
//
// It returns done=true when an upgrade is active and owns this reconcile cycle
// (the caller must return result/err immediately). It returns done=false when no
// upgrade applies — or when an upgrade has just completed — so the caller's normal
// migrate→deploy→status path runs this cycle.
//
// The state machine is level-based and resumable: each phase is persisted to status
// before its mutating step, so an operator restart re-enters the same phase.
func (r *LakekeeperReconciler) reconcileUpgrade(ctx context.Context, lk *lakekeeperv1alpha1.Lakekeeper) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)

	// Mid-upgrade re-edit guard: spec.image changed while an upgrade was in flight.
	// Reset the machine and re-evaluate from the entry phase against the new target.
	if lk.Status.UpgradeTargetImage != "" && lk.Status.UpgradeTargetImage != lk.Spec.Image {
		logger.Info("Upgrade target changed mid-flight, resetting upgrade state machine",
			"previousTarget", lk.Status.UpgradeTargetImage, "newTarget", lk.Spec.Image)
		lk.Status.UpgradePhase = ""
		lk.Status.UpgradeTargetImage = ""
	}

	switch lk.Status.UpgradePhase {
	case "":
		return r.enterUpgradeIfNeeded(ctx, lk)
	case lakekeeperv1alpha1.UpgradePhaseQuiescing:
		return r.reconcileQuiescing(ctx, lk)
	case lakekeeperv1alpha1.UpgradePhaseMigrating:
		return r.reconcileMigratingPhase(ctx, lk)
	case lakekeeperv1alpha1.UpgradePhaseRollingOut:
		return r.reconcileRollingOut(ctx, lk)
	default:
		logger.Info("Unknown upgrade phase, resetting", "phase", lk.Status.UpgradePhase)
		lk.Status.UpgradePhase = ""
		lk.Status.UpgradeTargetImage = ""
		return ctrl.Result{}, false, nil
	}
}

// enterUpgradeIfNeeded evaluates whether a read-only-gated upgrade should start.
// When the quiesce predicate holds it transitions to Quiescing; otherwise it
// returns done=false so the normal reconcile path handles this change.
func (r *LakekeeperReconciler) enterUpgradeIfNeeded(ctx context.Context, lk *lakekeeperv1alpha1.Lakekeeper) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)

	needsMig, err := r.needsMigration(ctx, lk)
	if err != nil {
		if errors.Is(err, errMigrationFailed) {
			// A permanent migration failure is surfaced by the normal migration
			// path; don't start an upgrade on top of it.
			return ctrl.Result{}, false, nil
		}
		// Transient error while checking migration status (e.g. API server
		// unavailable). Requeue with backoff rather than silently proceeding.
		return ctrl.Result{}, true, err
	}

	liveImage, exists, err := r.liveDeploymentImage(ctx, lk)
	if err != nil {
		return ctrl.Result{}, false, err
	}

	if !quiesceRequired(needsMig, exists, liveImage, lk.Spec.Image, r.desiredReplicas(lk), r.upgradeStrategy(lk)) {
		// No read-only-gated upgrade applies. Clear any stale upgrade bookkeeping
		// (e.g. after a mid-upgrade revert) so the normal path's status update is clean.
		if lk.Status.UpgradeTargetImage != "" {
			lk.Status.UpgradeTargetImage = ""
		}
		if meta.IsStatusConditionTrue(lk.Status.Conditions, TypeUpgrading) {
			r.setCondition(lk, TypeUpgrading, metav1.ConditionFalse, "NotUpgrading", "No upgrade in progress")
		}
		return ctrl.Result{}, false, nil
	}

	// Best-effort: warn if the running server predates MAINTENANCE_MODE support.
	r.reconcileVersionWarning(ctx, lk)

	logger.Info("Starting read-only-gated upgrade", "from", liveImage, "to", lk.Spec.Image)
	lk.Status.UpgradePhase = lakekeeperv1alpha1.UpgradePhaseQuiescing
	lk.Status.UpgradeTargetImage = lk.Spec.Image
	r.setCondition(lk, TypeUpgrading, metav1.ConditionTrue, string(lakekeeperv1alpha1.UpgradePhaseQuiescing),
		"Restarting pods in read-only maintenance mode before migration")
	r.setCondition(lk, TypeReady, metav1.ConditionFalse, "Upgrading", "Upgrade in progress: quiescing")
	if err := r.Status().Update(ctx, lk); err != nil {
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{Requeue: true}, true, nil
}

// reconcileQuiescing renders the existing (old) image with read-only maintenance
// mode enabled and waits for that rollout to complete before migrating.
func (r *LakekeeperReconciler) reconcileQuiescing(ctx context.Context, lk *lakekeeperv1alpha1.Lakekeeper) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)

	liveImage, exists, err := r.liveDeploymentImage(ctx, lk)
	if err != nil {
		return ctrl.Result{}, true, err
	}

	// Drift guard: the Deployment was deleted or externally patched to the target
	// image while we were Quiescing (the normal path never reaches Quiescing with
	// liveImage == spec.Image, since reconcileQuiescing only ever renders the old
	// image). Re-enter at Migrating — not RollingOut — so the migration still runs
	// against the new image before any new-image pods can serve writes. Jumping
	// straight to RollingOut would promote the image first and run the migration
	// only afterwards via the normal tail, defeating the read-only gate.
	if !exists || liveImage == lk.Spec.Image {
		return r.advanceTo(ctx, lk, lakekeeperv1alpha1.UpgradePhaseMigrating,
			"Deployment already on the target image; running migration before rollout")
	}

	if err := r.reconcileDeployment(ctx, lk, deploymentIntent{image: liveImage, maintenanceMode: true}); err != nil {
		return ctrl.Result{}, true, err
	}
	if err := r.reconcileServices(ctx, lk); err != nil {
		return ctrl.Result{}, true, err
	}

	complete, err := r.rolloutComplete(ctx, lk)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if !complete {
		logger.V(1).Info("Waiting for read-only quiesce rollout to complete")
		return ctrl.Result{RequeueAfter: upgradeRequeueAfter}, true, nil
	}

	return r.advanceTo(ctx, lk, lakekeeperv1alpha1.UpgradePhaseMigrating,
		"Running the database migration against the new image")
}

// reconcileMigratingPhase runs the database migration (new image) via the shared
// reconcileMigration path. On success it advances to RollingOut; while in progress
// or on permanent failure it stays in Migrating (the pods remain safely read-only).
func (r *LakekeeperReconciler) reconcileMigratingPhase(ctx context.Context, lk *lakekeeperv1alpha1.Lakekeeper) (ctrl.Result, bool, error) {
	result, done, err := r.reconcileMigration(ctx, lk)
	if err != nil {
		return result, true, err
	}
	if done {
		// reconcileMigration returns done=true either while the Job is in progress
		// (RequeueAfter set) or on permanent failure (no requeue). On permanent
		// failure, add read-only-recovery context to the Degraded condition.
		if result.RequeueAfter == 0 {
			r.setCondition(lk, TypeDegraded, metav1.ConditionTrue, "MigrationFailed",
				"Database migration failed; the cluster remains in read-only maintenance mode. "+
					"Fix the underlying cause or re-apply spec.image to retry.")
			if statusErr := r.Status().Update(ctx, lk); statusErr != nil {
				return ctrl.Result{}, true, statusErr
			}
		}
		return result, true, nil
	}

	// Migration complete — roll forward to the new image with the flag removed.
	return r.advanceTo(ctx, lk, lakekeeperv1alpha1.UpgradePhaseRollingOut,
		"Rolling out the new image with maintenance mode disabled")
}

// reconcileRollingOut renders the new image with maintenance mode disabled and,
// once the rollout completes, clears the upgrade state so the normal reconcile tail
// finishes this cycle.
func (r *LakekeeperReconciler) reconcileRollingOut(ctx context.Context, lk *lakekeeperv1alpha1.Lakekeeper) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)

	if err := r.reconcileDeployment(ctx, lk, deploymentIntent{image: lk.Spec.Image, maintenanceMode: false}); err != nil {
		return ctrl.Result{}, true, err
	}
	if err := r.reconcileServices(ctx, lk); err != nil {
		return ctrl.Result{}, true, err
	}

	complete, err := r.rolloutComplete(ctx, lk)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if !complete {
		logger.V(1).Info("Waiting for new-image rollout to complete")
		return ctrl.Result{RequeueAfter: upgradeRequeueAfter}, true, nil
	}

	logger.Info("Read-only-gated upgrade complete", "image", lk.Spec.Image)
	lk.Status.UpgradePhase = ""
	lk.Status.UpgradeTargetImage = ""
	r.setCondition(lk, TypeUpgrading, metav1.ConditionFalse, "UpgradeComplete",
		fmt.Sprintf("Upgrade to %s completed", lk.Spec.Image))
	// Clear any version warning raised for this upgrade.
	r.setCondition(lk, TypeUpgradeWarning, metav1.ConditionFalse, "UpgradeComplete", "No upgrade in progress")
	if err := r.Status().Update(ctx, lk); err != nil {
		return ctrl.Result{}, true, err
	}

	// done=false: let the normal tail (migration no-op, deployment no-op, services,
	// updateStatus, bootstrap poll) run this cycle to settle Ready=True.
	return ctrl.Result{}, false, nil
}

// advanceTo persists the next upgrade phase (keeping Ready=False) and requeues.
func (r *LakekeeperReconciler) advanceTo(
	ctx context.Context, lk *lakekeeperv1alpha1.Lakekeeper, phase lakekeeperv1alpha1.UpgradePhase, msg string,
) (ctrl.Result, bool, error) {
	lk.Status.UpgradePhase = phase
	r.setCondition(lk, TypeUpgrading, metav1.ConditionTrue, string(phase), msg)
	r.setCondition(lk, TypeReady, metav1.ConditionFalse, "Upgrading", "Upgrade in progress: "+string(phase))
	if err := r.Status().Update(ctx, lk); err != nil {
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{Requeue: true}, true, nil
}

// rolloutComplete fetches the live Deployment and reports whether its rollout has
// fully settled. A fresh Get is required because CreateOrUpdate returns an object
// with stale .Status.
func (r *LakekeeperReconciler) rolloutComplete(ctx context.Context, lk *lakekeeperv1alpha1.Lakekeeper) (bool, error) {
	dep := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{Name: lk.Name, Namespace: lk.Namespace}, dep); err != nil {
		return false, fmt.Errorf("failed to get deployment for rollout check: %w", err)
	}
	return deploymentRolloutComplete(dep), nil
}

// liveDeploymentImage reads the lakekeeper container image from the live Deployment.
// Returns ("", false, nil) when the Deployment does not exist yet.
func (r *LakekeeperReconciler) liveDeploymentImage(ctx context.Context, lk *lakekeeperv1alpha1.Lakekeeper) (string, bool, error) {
	dep := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: lk.Name, Namespace: lk.Namespace}, dep)
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("failed to get deployment: %w", err)
	}
	for i := range dep.Spec.Template.Spec.Containers {
		if dep.Spec.Template.Spec.Containers[i].Name == "lakekeeper" {
			return dep.Spec.Template.Spec.Containers[i].Image, true, nil
		}
	}
	// Deployment exists but has no recognizable lakekeeper container.
	return "", true, nil
}

// desiredReplicas returns the spec replica count, defaulting to 1 when unset.
func (r *LakekeeperReconciler) desiredReplicas(lk *lakekeeperv1alpha1.Lakekeeper) int32 {
	if lk.Spec.Replicas != nil {
		return *lk.Spec.Replicas
	}
	return 1
}

// upgradeStrategy returns the configured upgrade strategy, defaulting to
// ReadOnlyMigration when unset.
func (r *LakekeeperReconciler) upgradeStrategy(lk *lakekeeperv1alpha1.Lakekeeper) lakekeeperv1alpha1.UpgradeStrategy {
	if lk.Spec.Upgrade != nil && lk.Spec.Upgrade.Strategy != "" {
		return lk.Spec.Upgrade.Strategy
	}
	return lakekeeperv1alpha1.UpgradeStrategyReadOnlyMigration
}

// baseURL returns the management-API base URL for lk, honoring the test override.
func (r *LakekeeperReconciler) baseURL(lk *lakekeeperv1alpha1.Lakekeeper) string {
	if r.getBaseURL != nil {
		return r.getBaseURL(lk)
	}
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", lk.Name, lk.Namespace, r.getListenPort(lk))
}

// reconcileVersionWarning probes the running server version and, when it predates
// MAINTENANCE_MODE support, raises an informational UpgradeWarning condition. It is
// best-effort: it never blocks the upgrade and stays silent if the probe fails or
// the version is unparseable.
func (r *LakekeeperReconciler) reconcileVersionWarning(ctx context.Context, lk *lakekeeperv1alpha1.Lakekeeper) {
	logger := log.FromContext(ctx)

	if r.upgradeStrategy(lk) != lakekeeperv1alpha1.UpgradeStrategyReadOnlyMigration {
		return
	}

	info, err := r.fetchInfo(ctx, r.baseURL(lk))
	if err != nil {
		logger.V(1).Info("Could not probe server version for upgrade compatibility", "error", err)
		return
	}

	version := info.LakekeeperVersion
	if version == "" {
		version = info.Version
	}

	older, ok := versionOlderThan(version, minMaintenanceModeVersion)
	if !ok {
		logger.V(1).Info("Could not parse server version; skipping compatibility warning", "version", version)
		return
	}

	if older {
		r.setCondition(lk, TypeUpgradeWarning, metav1.ConditionTrue, "MaintenanceModeUnsupported",
			fmt.Sprintf("Running Lakekeeper version %s predates %s, which introduced read-only "+
				"MAINTENANCE_MODE; read-only upgrade gating may be ineffective. Consider "+
				"strategy: Simple or upgrade incrementally.", version, minMaintenanceModeVersion))
		return
	}
	r.setCondition(lk, TypeUpgradeWarning, metav1.ConditionFalse, "MaintenanceModeSupported",
		fmt.Sprintf("Running Lakekeeper version %s supports read-only maintenance mode", version))
}

// buildDeploymentEnvVars returns the environment for the serving Deployment.
// It wraps buildEnvVars and, when maintenanceMode is true, appends
// LAKEKEEPER__MAINTENANCE_MODE=read-only.
//
// IMPORTANT: the maintenance flag is injected here — not in buildEnvVars or
// buildServerEnvVars — because createMigrationJob shares buildEnvVars and the
// migrate Job must never receive the flag (it must be free to write the schema).
// HashMigrationRelevantSpec is spec-only, so this flag never perturbs the Job name.
func (r *LakekeeperReconciler) buildDeploymentEnvVars(lk *lakekeeperv1alpha1.Lakekeeper, maintenanceMode bool) []corev1.EnvVar {
	envVars := r.buildEnvVars(lk)
	if maintenanceMode {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__MAINTENANCE_MODE",
			Value: "read-only",
		})
	}
	return envVars
}

// quiesceRequired reports whether a read-only-gated quiesce step should run before
// migrating. It is true only when a migration is needed for a Deployment that
// already exists on a different image, replicas are scaled above zero, and the
// ReadOnlyMigration strategy is selected.
func quiesceRequired(
	needsMigration, deploymentExists bool, liveImage, specImage string,
	desiredReplicas int32, strategy lakekeeperv1alpha1.UpgradeStrategy,
) bool {
	return needsMigration &&
		deploymentExists &&
		liveImage != specImage &&
		desiredReplicas > 0 &&
		strategy == lakekeeperv1alpha1.UpgradeStrategyReadOnlyMigration
}

// versionOlderThan reports whether semver string v is strictly older than ref.
// The second return value is false when either version cannot be parsed, in which
// case the comparison result must not be trusted. A leading 'v' and any
// pre-release/build suffix (after '-' or '+') are ignored.
func versionOlderThan(v, ref string) (older bool, ok bool) {
	a, okA := parseVersion(v)
	b, okB := parseVersion(ref)
	if !okA || !okB {
		return false, false
	}
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] < b[i], true
		}
	}
	return false, true
}

// parseVersion parses "MAJOR.MINOR.PATCH" (with optional leading 'v' and trailing
// pre-release/build metadata) into a [3]int. Missing minor/patch default to 0.
func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return out, false
	}
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// deploymentRolloutComplete reports whether a Deployment's rollout has fully
// settled — the level-based equivalent of `kubectl rollout status`. The caller
// must pass a freshly-fetched Deployment (CreateOrUpdate returns stale .Status).
func deploymentRolloutComplete(dep *appsv1.Deployment) bool {
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	st := dep.Status
	return dep.Generation == st.ObservedGeneration &&
		st.UpdatedReplicas == desired &&
		st.Replicas == desired &&
		st.AvailableReplicas == desired &&
		st.ReadyReplicas == desired &&
		st.UnavailableReplicas == 0
}
