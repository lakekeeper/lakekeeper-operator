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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	lakekeeperv1alpha1 "github.com/lakekeeper/lakekeeper-operator/api/v1alpha1"
)

const (
	// Condition types
	TypeReady         = "Ready"
	TypeDegraded      = "Degraded"
	TypeMigrated      = "Migrated"
	TypeBootstrapped  = "Bootstrapped"
	TypeConfigWarning = "ConfigWarning"

	// Finalizer name
	finalizerName = "lakekeeper.k8s.lakekeeper.io/finalizer"

	// Migration Job configuration
	migrationJobBackoffLimit = int32(3)         // Number of retries before Job is marked as failed
	migrationJobTTLSeconds   = int32(600)       // TTL for completed Jobs (10 minutes)
	migrationJobRequeueAfter = 10 * time.Second // How long to wait before checking Job status again

	// Bootstrap configuration
	bootstrapHTTPTimeout  = 3 * time.Second  // Timeout for bootstrap HTTP calls (short to avoid blocking reconcile loop)
	bootstrapPollInterval = 15 * time.Second // How often to poll /management/v1/info until bootstrap is confirmed
)

// LakekeeperReconciler reconciles a Lakekeeper object
type LakekeeperReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// getBaseURL overrides the management-API base URL; intended for unit tests only.
	// When nil the production URL (http://<name>.<namespace>.svc.cluster.local:<port>) is used.
	getBaseURL func(*lakekeeperv1alpha1.Lakekeeper) string
}

// +kubebuilder:rbac:groups=lakekeeper.k8s.lakekeeper.io,resources=lakekeepers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=lakekeeper.k8s.lakekeeper.io,resources=lakekeepers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=lakekeeper.k8s.lakekeeper.io,resources=lakekeepers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get

// Reconcile implements the main reconciliation loop for Lakekeeper instances.
//
// Retry Strategy:
//   - Transient errors (e.g., API server unavailable, network issues) trigger automatic
//     retry with exponential backoff (controller-runtime default behavior).
//   - When errors are returned from this function, controller-runtime automatically
//     requeues the request with increasing backoff delays (up to a maximum).
//   - The controller watches Deployments, Services, and Jobs (owned resources), so
//     changes to these resources automatically trigger reconciliation.
//     NOTE: Referenced Secrets are NOT watched. If a Secret changes or is deleted,
//     reconciliation is only triggered by the normal requeue cycle, not immediately.
//     See Future Enhancements in docs/architecture.md for planned Secret watching.
//   - Permanent configuration errors (e.g., missing Secret references) are logged and
//     set the Degraded status condition, then return an error to trigger retry.
//     The retry will succeed once the referenced resource is created.
//
// Reconciliation Flow:
//  1. Fetch the Lakekeeper resource from the Kubernetes API
//  2. Handle deletion via finalizer if DeletionTimestamp is set
//  3. Add finalizer if not present (ensures clean deletion)
//  4. Validate all referenced Secrets exist
//  5. Run database migration Job (wait for completion)
//  6. Create or update Deployment (only after successful migration)
//  7. Create or update Services (main API + metrics)
//  8. Update status based on Deployment state
//
// For more details on controller-runtime reconciliation patterns, see:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/reconcile
func (r *LakekeeperReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the Lakekeeper instance
	lk := &lakekeeperv1alpha1.Lakekeeper{}
	if err := r.Get(ctx, req.NamespacedName, lk); err != nil {
		if apierrors.IsNotFound(err) {
			// Resource deleted, nothing to do
			logger.Info("Lakekeeper resource not found, assuming deleted")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get Lakekeeper resource")
		return ctrl.Result{}, err
	}

	// Handle deletion with finalizers
	if !lk.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, lk)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(lk, finalizerName) {
		logger.Info("Adding finalizer to Lakekeeper resource")
		controllerutil.AddFinalizer(lk, finalizerName)
		if err := r.Update(ctx, lk); err != nil {
			logger.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Validate referenced secrets exist
	if done, err := r.reconcileSecretValidation(ctx, lk); done {
		return ctrl.Result{}, err
	}

	// Check for unimplemented configuration fields (informational only, never blocks reconciliation)
	r.reconcileConfigWarnings(ctx, lk)

	// Run migration Job (blocks deployment until complete)
	if result, done, err := r.reconcileMigration(ctx, lk); done {
		return result, err
	}

	// Reconcile the Deployment (only after successful migration)
	if err := r.reconcileDeployment(ctx, lk); err != nil {
		logger.Error(err, "Failed to reconcile Deployment")
		r.setCondition(lk, TypeDegraded, metav1.ConditionTrue, "DeploymentReconciliationFailed", err.Error())
		r.setCondition(lk, TypeReady, metav1.ConditionFalse, "DeploymentReconciliationFailed", "Failed to reconcile Deployment")
		if statusErr := r.Status().Update(ctx, lk); statusErr != nil {
			logger.Error(statusErr, "Failed to update status after deployment reconciliation failure")
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	// Reconcile the Services
	if err := r.reconcileServices(ctx, lk); err != nil {
		logger.Error(err, "Failed to reconcile Services")
		r.setCondition(lk, TypeDegraded, metav1.ConditionTrue, "ServiceReconciliationFailed", err.Error())
		r.setCondition(lk, TypeReady, metav1.ConditionFalse, "ServiceReconciliationFailed", "Failed to reconcile Services")
		if statusErr := r.Status().Update(ctx, lk); statusErr != nil {
			logger.Error(statusErr, "Failed to update status after service reconciliation failure")
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	// Update status based on Deployment state
	if err := r.updateStatus(ctx, lk); err != nil {
		logger.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}

	logger.Info("Successfully reconciled Lakekeeper resource")

	// Always poll until bootstrap is confirmed — regardless of who performs it.
	// Once BootstrappedAt is set, polling stops permanently.
	// When replicas==0 there are no pods to bootstrap against; skip the poll.
	if lk.Status.BootstrappedAt == nil && (lk.Spec.Replicas == nil || *lk.Spec.Replicas != 0) {
		return ctrl.Result{RequeueAfter: r.getBootstrapCheckInterval(lk)}, nil
	}

	return ctrl.Result{}, nil
}

// reconcileSecretValidation validates that all referenced secrets exist and are accessible.
// Returns done=true if validation failed (caller should return early), done=false to proceed.
func (r *LakekeeperReconciler) reconcileSecretValidation(ctx context.Context, lk *lakekeeperv1alpha1.Lakekeeper) (bool, error) {
	logger := log.FromContext(ctx)

	if err := r.validateSecrets(ctx, lk); err != nil {
		logger.Error(err, "Secret validation failed")
		r.setCondition(lk, TypeDegraded, metav1.ConditionTrue, "SecretValidationFailed", err.Error())
		r.setCondition(lk, TypeReady, metav1.ConditionFalse, "SecretValidationFailed", "Required secrets are missing or inaccessible")
		if statusErr := r.Status().Update(ctx, lk); statusErr != nil {
			logger.Error(statusErr, "Failed to update status after secret validation failure")
			return true, statusErr
		}
		return true, err
	}

	return false, nil
}

// reconcileMigration runs database migration Jobs and tracks their status.
// Returns done=true when the caller should return early (migration pending, failed, or error).
// Returns done=false when migration is complete and reconciliation should continue.
func (r *LakekeeperReconciler) reconcileMigration(ctx context.Context, lk *lakekeeperv1alpha1.Lakekeeper) (ctrl.Result, bool, error) {
	logger := log.FromContext(ctx)

	// Check if migration Job needed
	jobNeeded, err := r.needsMigration(ctx, lk)
	if err != nil {
		// Migration failed permanently — conditions capture the failure.
		// Do not requeue automatically; user must change spec to recover.
		logger.Error(err, "Migration failed permanently")
		r.setCondition(lk, TypeMigrated, metav1.ConditionFalse, "MigrationFailed", err.Error())
		r.setCondition(lk, TypeDegraded, metav1.ConditionTrue, "MigrationFailed", "Database migration failed permanently")
		r.setCondition(lk, TypeReady, metav1.ConditionFalse, "MigrationFailed", "Database migration failed")
		if statusErr := r.Status().Update(ctx, lk); statusErr != nil {
			logger.Error(statusErr, "Failed to update status after permanent migration failure")
			return ctrl.Result{}, true, statusErr
		}
		return ctrl.Result{}, true, nil
	}

	if jobNeeded {
		// Create migration Job
		job, err := r.createMigrationJob(ctx, lk)
		if err != nil {
			logger.Error(err, "Failed to create migration Job")
			r.setCondition(lk, TypeMigrated, metav1.ConditionFalse, "MigrationJobCreationFailed", fmt.Sprintf("Failed to create migration Job: %v", err))
			r.setCondition(lk, TypeReady, metav1.ConditionFalse, "MigrationJobCreationFailed", "Migration Job creation failed")
			if statusErr := r.Status().Update(ctx, lk); statusErr != nil {
				logger.Error(statusErr, "Failed to update status after migration Job creation failure")
			}
			return ctrl.Result{}, true, err
		}

		// Wait for Job completion (requeue)
		if !isJobComplete(job) {
			logger.Info("Migration Job in progress", "jobName", job.Name)
			r.setCondition(lk, TypeMigrated, metav1.ConditionFalse, "MigrationInProgress", fmt.Sprintf("Migration Job %s is running", job.Name))
			r.setCondition(lk, TypeReady, metav1.ConditionFalse, "MigrationInProgress", "Waiting for migration to complete")
			lk.Status.MigrationJob = job.Name
			if err := r.Status().Update(ctx, lk); err != nil {
				logger.Error(err, "Failed to update status during migration")
			}
			return ctrl.Result{RequeueAfter: migrationJobRequeueAfter}, true, nil
		}

		// Check Job succeeded
		if !isJobSuccessful(job) {
			logger.Info("Migration Job permanently failed, halting requeue until spec changes", "jobName", job.Name)
			r.setCondition(lk, TypeMigrated, metav1.ConditionFalse, "MigrationFailed", fmt.Sprintf("Migration Job %s failed", job.Name))
			r.setCondition(lk, TypeReady, metav1.ConditionFalse, "MigrationFailed", "Database migration failed")
			r.setCondition(lk, TypeDegraded, metav1.ConditionTrue, "MigrationFailed", "Migration Job failed after maximum retries")
			lk.Status.MigrationJob = job.Name
			if err := r.Status().Update(ctx, lk); err != nil {
				logger.Error(err, "Failed to update status after migration failure")
			}
			return ctrl.Result{}, true, nil
		}

		// Mark migration complete
		logger.Info("Migration Job succeeded", "jobName", job.Name)
		r.setCondition(lk, TypeMigrated, metav1.ConditionTrue, "MigrationSucceeded", fmt.Sprintf("Migration Job %s completed successfully", job.Name))
		lk.Status.MigrationJob = job.Name
		if err := r.Status().Update(ctx, lk); err != nil {
			logger.Error(err, "Failed to update status after successful migration")
		}
	} else {
		// Migration not needed or already complete - check status
		job, err := r.getMigrationJob(ctx, lk)
		if err == nil && job != nil {
			// Job exists - check if it's still running
			if !isJobComplete(job) {
				logger.Info("Migration Job still running", "jobName", job.Name)
				r.setCondition(lk, TypeMigrated, metav1.ConditionFalse, "MigrationInProgress", fmt.Sprintf("Migration Job %s is running", job.Name))
				r.setCondition(lk, TypeReady, metav1.ConditionFalse, "MigrationInProgress", "Waiting for migration to complete")
				lk.Status.MigrationJob = job.Name
				if err := r.Status().Update(ctx, lk); err != nil {
					logger.Error(err, "Failed to update status during migration")
				}
				return ctrl.Result{RequeueAfter: migrationJobRequeueAfter}, true, nil
			}

			// Job complete - ensure it succeeded
			if isJobSuccessful(job) {
				// Ensure Migrated condition is set
				if !meta.IsStatusConditionTrue(lk.Status.Conditions, TypeMigrated) {
					logger.V(1).Info("Setting Migrated condition for existing successful Job", "jobName", job.Name)
					r.setCondition(lk, TypeMigrated, metav1.ConditionTrue, "MigrationSucceeded", fmt.Sprintf("Migration Job %s completed successfully", job.Name))
					lk.Status.MigrationJob = job.Name
					if err := r.Status().Update(ctx, lk); err != nil {
						logger.Error(err, "Failed to update status with migration condition")
					}
				}
			}
		}
	}

	return ctrl.Result{}, false, nil
}

// reconcileDelete handles the deletion of a Lakekeeper resource
func (r *LakekeeperReconciler) reconcileDelete(ctx context.Context, lk *lakekeeperv1alpha1.Lakekeeper) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if controllerutil.ContainsFinalizer(lk, finalizerName) {
		logger.Info("Cleaning up Lakekeeper resources")

		// Note: Kubernetes will automatically delete owned resources (Deployment, Services)
		// due to owner references. No additional cleanup needed for now.
		// When we add Lakekeeper API integration in future, we would clean up
		// external resources here (warehouses, projects, etc.)

		// Remove finalizer
		controllerutil.RemoveFinalizer(lk, finalizerName)
		if err := r.Update(ctx, lk); err != nil {
			logger.Error(err, "Failed to remove finalizer")
			return ctrl.Result{}, err
		}
		logger.Info("Finalizer removed, resource will be deleted")
	}

	return ctrl.Result{}, nil
}

// validateSecrets checks that all referenced secrets exist
func (r *LakekeeperReconciler) validateSecrets(ctx context.Context, lk *lakekeeperv1alpha1.Lakekeeper) error {
	logger := log.FromContext(ctx)

	secretRefs := r.collectSecretReferences(lk)

	for _, ref := range secretRefs {
		secret := &corev1.Secret{}
		secretKey := client.ObjectKey{
			Namespace: lk.Namespace,
			Name:      ref.Name,
		}
		if err := r.Get(ctx, secretKey, secret); err != nil {
			if apierrors.IsNotFound(err) {
				logger.Error(err, "Referenced secret not found", "secretName", ref.Name, "key", ref.Key)
				return fmt.Errorf("secret %s not found in namespace %s", ref.Name, lk.Namespace)
			}
			logger.Error(err, "Failed to get secret", "secretName", ref.Name)
			return fmt.Errorf("failed to get secret %s: %w", ref.Name, err)
		}

		// Verify the key exists in the secret
		if _, ok := secret.Data[ref.Key]; !ok {
			logger.Error(nil, "Secret key not found", "secretName", ref.Name, "key", ref.Key)
			return fmt.Errorf("key %s not found in secret %s", ref.Key, ref.Name)
		}

		logger.V(1).Info("Secret validated", "secretName", ref.Name, "key", ref.Key)
	}

	return nil
}

// collectSecretReferences gathers all SecretKeySelector references from the spec
func (r *LakekeeperReconciler) collectSecretReferences(lk *lakekeeperv1alpha1.Lakekeeper) []corev1.SecretKeySelector {
	var refs []corev1.SecretKeySelector

	// Database secrets
	if lk.Spec.Database.Type == lakekeeperv1alpha1.DatabaseTypePostgres && lk.Spec.Database.Postgres != nil {
		pg := lk.Spec.Database.Postgres
		refs = append(refs, pg.PasswordSecretRef, pg.EncryptionKeySecretRef)

		if pg.UserSecretRef != nil {
			refs = append(refs, *pg.UserSecretRef)
		}
		if pg.SSLRootCertSecretRef != nil {
			refs = append(refs, *pg.SSLRootCertSecretRef)
		}
	}

	// Secret store secrets
	if lk.Spec.SecretStore != nil && lk.Spec.SecretStore.Type == lakekeeperv1alpha1.SecretStoreTypeVaultKV2 {
		if lk.Spec.SecretStore.VaultKV2 != nil {
			vault := lk.Spec.SecretStore.VaultKV2
			refs = append(refs, vault.PasswordSecretRef)
			if vault.UserSecretRef != nil {
				refs = append(refs, *vault.UserSecretRef)
			}
		}
	}

	// Authorization secrets
	if lk.Spec.Authorization.Backend == lakekeeperv1alpha1.AuthzBackendOpenFGA {
		if lk.Spec.Authorization.OpenFGA != nil && lk.Spec.Authorization.OpenFGA.ClientSecretRef != nil {
			refs = append(refs, *lk.Spec.Authorization.OpenFGA.ClientSecretRef)
		}
	}

	// SSL secrets
	if lk.Spec.SSL != nil && lk.Spec.SSL.CertFileSecretRef != nil {
		refs = append(refs, *lk.Spec.SSL.CertFileSecretRef)
	}

	return refs
}

// reconcileDeployment creates or updates the Lakekeeper Deployment
func (r *LakekeeperReconciler) reconcileDeployment(ctx context.Context, lk *lakekeeperv1alpha1.Lakekeeper) error {
	logger := log.FromContext(ctx)

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lk.Name,
			Namespace: lk.Namespace,
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		// Set labels
		labels := r.getLabels(lk)
		deployment.Labels = labels

		// Set owner reference for garbage collection
		if err := controllerutil.SetControllerReference(lk, deployment, r.Scheme); err != nil {
			return fmt.Errorf("failed to set controller reference: %w", err)
		}

		// Set spec
		replicas := ptr.To(int32(1))
		if lk.Spec.Replicas != nil {
			replicas = lk.Spec.Replicas
		}

		deployment.Spec = appsv1.DeploymentSpec{
			Replicas: replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "lakekeeper",
							Image: lk.Spec.Image,
							Args:  []string{"serve"},
							Env:   r.buildEnvVars(lk),
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: r.getListenPort(lk),
									Protocol:      corev1.ProtocolTCP,
								},
								{
									Name:          "metrics",
									ContainerPort: r.getMetricsPort(lk),
									Protocol:      corev1.ProtocolTCP,
								},
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/health",
										Port: intstr.FromString("http"),
									},
								},
								InitialDelaySeconds: 30,
								PeriodSeconds:       10,
								TimeoutSeconds:      5,
								FailureThreshold:    3,
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/health",
										Port: intstr.FromString("http"),
									},
								},
								InitialDelaySeconds: 10,
								PeriodSeconds:       5,
								TimeoutSeconds:      3,
								FailureThreshold:    3,
							},
						},
					},
				},
			},
		}

		// Apply resource requirements if specified
		if lk.Spec.Resources != nil {
			deployment.Spec.Template.Spec.Containers[0].Resources = *lk.Spec.Resources
		}

		// Apply SSL volume mounts and volumes
		sslVolumeMounts := r.buildSSLVolumeMounts(lk)
		if len(sslVolumeMounts) > 0 {
			deployment.Spec.Template.Spec.Containers[0].VolumeMounts = sslVolumeMounts
		}
		sslVolumes := r.buildSSLVolumes(lk)
		if len(sslVolumes) > 0 {
			deployment.Spec.Template.Spec.Volumes = sslVolumes
		}

		return nil
	})

	if err != nil {
		logger.Error(err, "Failed to reconcile Deployment")
		return fmt.Errorf("failed to reconcile deployment: %w", err)
	}

	logger.Info("Deployment reconciled", "operation", op)
	return nil
}

// reconcileServices creates or updates the Lakekeeper Services
func (r *LakekeeperReconciler) reconcileServices(ctx context.Context, lk *lakekeeperv1alpha1.Lakekeeper) error {
	logger := log.FromContext(ctx)

	// Main service for catalog/management APIs
	mainSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lk.Name,
			Namespace: lk.Namespace,
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, mainSvc, func() error {
		labels := r.getLabels(lk)
		mainSvc.Labels = labels

		if err := controllerutil.SetControllerReference(lk, mainSvc, r.Scheme); err != nil {
			return fmt.Errorf("failed to set controller reference: %w", err)
		}

		mainSvc.Spec = corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       r.getListenPort(lk),
					TargetPort: intstr.FromString("http"),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		}

		return nil
	})

	if err != nil {
		logger.Error(err, "Failed to reconcile main Service")
		return fmt.Errorf("failed to reconcile main service: %w", err)
	}
	logger.Info("Main Service reconciled", "operation", op)

	// Metrics service
	metricsSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lk.Name + "-metrics",
			Namespace: lk.Namespace,
		},
	}

	op, err = controllerutil.CreateOrUpdate(ctx, r.Client, metricsSvc, func() error {
		labels := r.getLabels(lk)
		metricsSvc.Labels = labels

		if err := controllerutil.SetControllerReference(lk, metricsSvc, r.Scheme); err != nil {
			return fmt.Errorf("failed to set controller reference: %w", err)
		}

		metricsSvc.Spec = corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{
				{
					Name:       "metrics",
					Port:       r.getMetricsPort(lk),
					TargetPort: intstr.FromString("metrics"),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		}

		return nil
	})

	if err != nil {
		logger.Error(err, "Failed to reconcile metrics Service")
		return fmt.Errorf("failed to reconcile metrics service: %w", err)
	}
	logger.Info("Metrics Service reconciled", "operation", op)

	return nil
}

// updateStatus updates the Lakekeeper status based on the Deployment state
func (r *LakekeeperReconciler) updateStatus(ctx context.Context, lk *lakekeeperv1alpha1.Lakekeeper) error {
	logger := log.FromContext(ctx)

	// Fetch the Deployment
	deployment := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{Name: lk.Name, Namespace: lk.Namespace}, deployment); err != nil {
		logger.Error(err, "Failed to get Deployment for status update")
		return fmt.Errorf("failed to get deployment: %w", err)
	}

	// Update replica counts
	lk.Status.Replicas = &deployment.Status.Replicas
	lk.Status.ReadyReplicas = &deployment.Status.ReadyReplicas
	lk.Status.ObservedGeneration = &lk.Generation

	// Set conditions based on Deployment status
	desiredReplicas := int32(1)
	if lk.Spec.Replicas != nil {
		desiredReplicas = *lk.Spec.Replicas
	}

	// Check if at least one pod is ready
	if desiredReplicas == 0 {
		// Intentionally scaled to zero — not degraded, not waiting for pods
		r.setCondition(lk, TypeReady, metav1.ConditionTrue, "ScaledToZero", "Deployment intentionally scaled to zero")
		r.setCondition(lk, TypeDegraded, metav1.ConditionFalse, "Healthy", "Scaled to zero by user")
	} else if deployment.Status.ReadyReplicas > 0 {
		r.setCondition(lk, TypeReady, metav1.ConditionTrue, "MinimumReplicasAvailable",
			fmt.Sprintf("%d/%d pods ready", deployment.Status.ReadyReplicas, desiredReplicas))
		r.setCondition(lk, TypeDegraded, metav1.ConditionFalse, "Healthy", "All pods are running and ready")
	} else {
		r.setCondition(lk, TypeReady, metav1.ConditionFalse, "MinimumReplicasUnavailable",
			fmt.Sprintf("%d/%d pods ready", deployment.Status.ReadyReplicas, desiredReplicas))

		// Set Degraded when no pods are ready
		if deployment.Status.ReadyReplicas == 0 && deployment.Status.Replicas > 0 {
			r.setCondition(lk, TypeDegraded, metav1.ConditionTrue, "NoPodsReady",
				fmt.Sprintf("Deployment has %d replicas but 0 are ready", deployment.Status.Replicas))
		}
	}

	// Sync bootstrap status whenever pods are ready (detects manual bootstrap too)
	if deployment.Status.ReadyReplicas > 0 {
		if err := r.syncBootstrapStatus(ctx, lk); err != nil {
			logger.Error(err, "Failed to sync bootstrap status")
			r.setCondition(lk, TypeBootstrapped, metav1.ConditionFalse, "BootstrapFailed", err.Error())
		}
	}

	// Update status
	if err := r.Status().Update(ctx, lk); err != nil {
		logger.Error(err, "Failed to update Lakekeeper status")
		return fmt.Errorf("failed to update status: %w", err)
	}

	logger.V(1).Info("Status updated", "replicas", deployment.Status.Replicas, "readyReplicas", deployment.Status.ReadyReplicas)
	return nil
}

// infoResponse is the JSON response body from GET /management/v1/info.
type infoResponse struct {
	Bootstrapped bool   `json:"bootstrapped"`
	ServerID     string `json:"server-id"`
}

// getBootstrapCheckInterval returns the configured bootstrap poll interval,
// falling back to the default if unset or unparseable.
func (r *LakekeeperReconciler) getBootstrapCheckInterval(lk *lakekeeperv1alpha1.Lakekeeper) time.Duration {
	if lk.Spec.Bootstrap != nil && lk.Spec.Bootstrap.CheckInterval != "" {
		if d, err := time.ParseDuration(lk.Spec.Bootstrap.CheckInterval); err == nil && d > 0 {
			return d
		}
	}
	return bootstrapPollInterval
}

// syncBootstrapStatus polls GET /management/v1/info when pods are ready.
// It updates BootstrappedAt/ServerID and the Bootstrapped condition, and –
// if spec.bootstrap.enabled=true and not yet bootstrapped – performs
// POST /management/v1/bootstrap.
// Transient network errors set Bootstrapped=False/ServerNotReachable and
// return nil so the next natural reconcile retries.
func (r *LakekeeperReconciler) syncBootstrapStatus(ctx context.Context, lk *lakekeeperv1alpha1.Lakekeeper) error {
	logger := log.FromContext(ctx)

	// Already confirmed bootstrapped — no need to poll the server again.
	// The status fields and Bootstrapped condition are preserved as-is.
	if lk.Status.BootstrappedAt != nil {
		return nil
	}

	var baseURL string
	if r.getBaseURL != nil {
		baseURL = r.getBaseURL(lk)
	} else {
		port := r.getListenPort(lk)
		baseURL = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", lk.Name, lk.Namespace, port)
	}

	info, err := r.fetchInfo(ctx, baseURL)
	if err != nil {
		// Transient: server not yet reachable
		logger.V(1).Info("Lakekeeper not reachable for bootstrap status check, will retry", "error", err)
		r.setCondition(lk, TypeBootstrapped, metav1.ConditionFalse, "ServerNotReachable", "Lakekeeper is not yet reachable")
		return nil
	}

	if info.Bootstrapped {
		// Always record that the server is bootstrapped, regardless of spec.bootstrap.
		// This covers both operator-managed bootstrap and external bootstrap (e.g. allowall mode).
		now := metav1.Now()
		lk.Status.BootstrappedAt = &now
		lk.Status.ServerID = &info.ServerID
		logger.Info("Detected bootstrapped Lakekeeper server", "serverID", info.ServerID)
		r.setCondition(lk, TypeBootstrapped, metav1.ConditionTrue, "Bootstrapped", "Lakekeeper server is bootstrapped")
		return nil
	}

	// Not bootstrapped
	if lk.Spec.Bootstrap == nil || lk.Spec.Bootstrap.Enabled == nil || !*lk.Spec.Bootstrap.Enabled {
		// Auto-bootstrap disabled: report current state only
		r.setCondition(lk, TypeBootstrapped, metav1.ConditionFalse, "NotBootstrapped",
			"Lakekeeper server is not bootstrapped. Set spec.bootstrap.enabled=true to bootstrap automatically.")
		return nil
	}

	// Auto-bootstrap enabled: perform the POST
	httpClient := &http.Client{Timeout: bootstrapHTTPTimeout}
	body, err := json.Marshal(map[string]interface{}{
		"accept-terms-of-use": true,
		"is-operator":         true,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal bootstrap request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/management/v1/bootstrap", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create bootstrap request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		// Transient: service not yet reachable
		logger.V(1).Info("Lakekeeper bootstrap endpoint not reachable, will retry", "error", err)
		return nil
	}
	defer resp.Body.Close() //nolint:errcheck

	switch resp.StatusCode {
	case http.StatusNoContent:
		// Bootstrap succeeded: re-fetch info to get the server-id assigned after bootstrap.
		info, err = r.fetchInfo(ctx, baseURL)
		if err != nil {
			return fmt.Errorf("failed to fetch server info after bootstrap: %w", err)
		}
	case http.StatusConflict:
		// 409: another actor bootstrapped concurrently; re-fetch to confirm.
		logger.V(1).Info("Bootstrap POST returned 409 Conflict, another actor bootstrapped concurrently; re-fetching info")
		info, err = r.fetchInfo(ctx, baseURL)
		if err != nil {
			return fmt.Errorf("failed to fetch server info after 409 Conflict: %w", err)
		}
	default:
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("bootstrap POST returned unexpected status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	if !info.Bootstrapped {
		// Transient consistency lag — let the next requeue try again
		return nil
	}
	now := metav1.Now()
	lk.Status.BootstrappedAt = &now
	lk.Status.ServerID = &info.ServerID
	r.setCondition(lk, TypeBootstrapped, metav1.ConditionTrue, "Bootstrapped",
		"Lakekeeper server was bootstrapped by the operator")
	logger.Info("Lakekeeper server bootstrapped successfully", "serverID", info.ServerID)

	return nil
}

// fetchInfo calls GET /management/v1/info and returns the parsed response.
// Returns an error for any network or non-200 response.
func (r *LakekeeperReconciler) fetchInfo(ctx context.Context, baseURL string) (*infoResponse, error) {
	httpClient := &http.Client{Timeout: bootstrapHTTPTimeout}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/management/v1/info", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create info request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("info GET returned unexpected status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var info infoResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("failed to decode info response: %w", err)
	}
	return &info, nil
}

// getLabels returns the labels for Lakekeeper resources
func (r *LakekeeperReconciler) getLabels(lk *lakekeeperv1alpha1.Lakekeeper) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "lakekeeper",
		"app.kubernetes.io/instance":   lk.Name,
		"app.kubernetes.io/managed-by": "lakekeeper-operator",
	}
}

// getListenPort returns the configured listen port or default
func (r *LakekeeperReconciler) getListenPort(lk *lakekeeperv1alpha1.Lakekeeper) int32 {
	if lk.Spec.Server != nil && lk.Spec.Server.ListenPort != nil {
		return *lk.Spec.Server.ListenPort
	}
	return 8181 // Default Lakekeeper listen port
}

// getMetricsPort returns the configured metrics port or default
func (r *LakekeeperReconciler) getMetricsPort(lk *lakekeeperv1alpha1.Lakekeeper) int32 {
	if lk.Spec.Server != nil && lk.Spec.Server.MetricsPort != nil {
		return *lk.Spec.Server.MetricsPort
	}
	return 9000 // Default Lakekeeper metrics port
}

// setCondition sets or updates a status condition
func (r *LakekeeperReconciler) setCondition(lk *lakekeeperv1alpha1.Lakekeeper, conditionType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&lk.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: lk.Generation,
	})
}

// buildEnvVars maps Lakekeeper CRD fields to environment variables for the Lakekeeper container.
//
// Environment variable names are based on Lakekeeper's configuration schema.
// Compatible with Lakekeeper v0.12.x and later.
//
// For the complete list of supported environment variables, see:
// https://docs.lakekeeper.io/docs/nightly/configuration/
//
// IMPORTANT: When updating to a new Lakekeeper version, verify environment variable
// names haven't changed by reviewing Lakekeeper's release notes and configuration docs.
func (r *LakekeeperReconciler) buildEnvVars(lk *lakekeeperv1alpha1.Lakekeeper) []corev1.EnvVar {
	var envVars []corev1.EnvVar

	envVars = append(envVars, r.buildServerEnvVars(lk)...)
	envVars = append(envVars, r.buildDatabaseEnvVars(lk)...)
	envVars = append(envVars, r.buildSecretStoreEnvVars(lk)...)
	envVars = append(envVars, r.buildAuthorizationEnvVars(lk)...)
	envVars = append(envVars, r.buildAuthenticationEnvVars(lk)...)
	envVars = append(envVars, r.buildIdempotencyEnvVars(lk)...)
	envVars = append(envVars, r.buildTaskQueuesEnvVars(lk)...)
	envVars = append(envVars, r.buildRequestLimitsEnvVars(lk)...)
	envVars = append(envVars, r.buildSSLEnvVars(lk)...)

	return envVars
}

// buildServerEnvVars generates environment variables for server configuration.
// Includes base URI, listen port, metrics port, pagination, CORS, reserved namespaces,
// and storage system credentials.
func (r *LakekeeperReconciler) buildServerEnvVars(lk *lakekeeperv1alpha1.Lakekeeper) []corev1.EnvVar {
	var envVars []corev1.EnvVar

	if lk.Spec.Server == nil {
		return envVars
	}

	if lk.Spec.Server.BaseURI != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__BASE_URI",
			Value: *lk.Spec.Server.BaseURI,
		})
	}
	if lk.Spec.Server.ListenPort != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__LISTEN_PORT",
			Value: strconv.Itoa(int(*lk.Spec.Server.ListenPort)),
		})
	}
	if lk.Spec.Server.MetricsPort != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__METRICS__PORT",
			Value: strconv.Itoa(int(*lk.Spec.Server.MetricsPort)),
		})
	}
	if lk.Spec.Server.EnableDefaultProject != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__ENABLE_DEFAULT_PROJECT",
			Value: strconv.FormatBool(*lk.Spec.Server.EnableDefaultProject),
		})
	}

	// Pagination
	if lk.Spec.Server.Pagination != nil {
		if lk.Spec.Server.Pagination.DefaultSize != nil {
			envVars = append(envVars, corev1.EnvVar{
				Name:  "LAKEKEEPER__PAGE_SIZE_DEFAULT",
				Value: strconv.Itoa(int(*lk.Spec.Server.Pagination.DefaultSize)),
			})
		}
		if lk.Spec.Server.Pagination.MaxSize != nil {
			envVars = append(envVars, corev1.EnvVar{
				Name:  "LAKEKEEPER__PAGE_SIZE_MAX",
				Value: strconv.Itoa(int(*lk.Spec.Server.Pagination.MaxSize)),
			})
		}
	}

	// CORS
	if lk.Spec.Server.CORS != nil && len(lk.Spec.Server.CORS.AllowOrigins) > 0 {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__CORS_ALLOW_ORIGINS",
			Value: strings.Join(lk.Spec.Server.CORS.AllowOrigins, ","),
		})
	}

	// Reserved namespaces
	if len(lk.Spec.Server.ReservedNamespaces) > 0 {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__RESERVED_NAMESPACES",
			Value: strings.Join(lk.Spec.Server.ReservedNamespaces, ","),
		})
	}

	// Storage system credentials
	if lk.Spec.Server.StorageSystemCredentials != nil {
		creds := lk.Spec.Server.StorageSystemCredentials
		if creds.EnableAWS != nil {
			envVars = append(envVars, corev1.EnvVar{
				Name:  "LAKEKEEPER__STORAGE_CREDENTIAL_BACKEND__AWS__ENABLED",
				Value: strconv.FormatBool(*creds.EnableAWS),
			})
			if creds.AWS != nil {
				if creds.AWS.AssumeRoleRequireExternalID != nil {
					envVars = append(envVars, corev1.EnvVar{
						Name:  "LAKEKEEPER__STORAGE_CREDENTIAL_BACKEND__AWS__ASSUME_ROLE_REQUIRE_EXTERNAL_ID",
						Value: strconv.FormatBool(*creds.AWS.AssumeRoleRequireExternalID),
					})
				}
				if creds.AWS.AllowDirectSystemCredentials != nil {
					envVars = append(envVars, corev1.EnvVar{
						Name:  "LAKEKEEPER__STORAGE_CREDENTIAL_BACKEND__AWS__ALLOW_DIRECT_SYSTEM_CREDENTIALS",
						Value: strconv.FormatBool(*creds.AWS.AllowDirectSystemCredentials),
					})
				}
			}
		}
		if creds.EnableAzure != nil {
			envVars = append(envVars, corev1.EnvVar{
				Name:  "LAKEKEEPER__STORAGE_CREDENTIAL_BACKEND__AZURE__ENABLED",
				Value: strconv.FormatBool(*creds.EnableAzure),
			})
		}
		if creds.EnableGCP != nil {
			envVars = append(envVars, corev1.EnvVar{
				Name:  "LAKEKEEPER__STORAGE_CREDENTIAL_BACKEND__GCP__ENABLED",
				Value: strconv.FormatBool(*creds.EnableGCP),
			})
		}
	}

	return envVars
}

// buildDatabaseEnvVars generates environment variables for database configuration.
// Currently supports PostgreSQL with host, port, database, user, password, encryption key,
// read replicas, connection pooling, and SSL settings.
func (r *LakekeeperReconciler) buildDatabaseEnvVars(lk *lakekeeperv1alpha1.Lakekeeper) []corev1.EnvVar {
	var envVars []corev1.EnvVar

	if lk.Spec.Database.Type != lakekeeperv1alpha1.DatabaseTypePostgres || lk.Spec.Database.Postgres == nil {
		return envVars
	}

	pg := lk.Spec.Database.Postgres

	envVars = append(envVars, corev1.EnvVar{
		Name:  "LAKEKEEPER__PG_HOST_W",
		Value: pg.Host,
	})

	port := int32(5432)
	if pg.Port != nil {
		port = *pg.Port
	}
	envVars = append(envVars, corev1.EnvVar{
		Name:  "LAKEKEEPER__PG_PORT",
		Value: strconv.Itoa(int(port)),
	})

	envVars = append(envVars, corev1.EnvVar{
		Name:  "LAKEKEEPER__PG_DATABASE",
		Value: pg.Database,
	})

	// User - either from plain value or secret
	if pg.User != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__PG_USER",
			Value: *pg.User,
		})
	} else if pg.UserSecretRef != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name: "LAKEKEEPER__PG_USER",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: pg.UserSecretRef,
			},
		})
	}

	// Password from secret
	envVars = append(envVars, corev1.EnvVar{
		Name: "LAKEKEEPER__PG_PASSWORD",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &pg.PasswordSecretRef,
		},
	})

	// Encryption key from secret
	envVars = append(envVars, corev1.EnvVar{
		Name: "LAKEKEEPER__PG_ENCRYPTION_KEY",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &pg.EncryptionKeySecretRef,
		},
	})

	// Read replica configuration
	// Note: Only host has read/write variants. Port is shared.
	if pg.ReadHost != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__PG_HOST_R",
			Value: *pg.ReadHost,
		})
	}

	// Connection pool settings
	if pg.MaxConnections != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__PG_MAX_CONNECTIONS",
			Value: strconv.Itoa(int(*pg.MaxConnections)),
		})
	}
	if pg.MinIdleConnections != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__PG_MIN_IDLE_CONNECTIONS",
			Value: strconv.Itoa(int(*pg.MinIdleConnections)),
		})
	}
	if pg.ConnectionTimeout != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__PG_CONNECTION_TIMEOUT",
			Value: *pg.ConnectionTimeout,
		})
	}

	// SSL settings
	sslMode := "require"
	if pg.SSLMode != nil {
		sslMode = *pg.SSLMode
	}
	envVars = append(envVars, corev1.EnvVar{
		Name:  "LAKEKEEPER__PG_SSL_MODE",
		Value: sslMode,
	})

	if pg.SSLRootCertSecretRef != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__PG_SSL_ROOT_CERT",
			Value: fmt.Sprintf("/etc/lakekeeper/ssl/pg-root-cert/%s", pg.SSLRootCertSecretRef.Key),
		})
	}

	return envVars
}

// buildSecretStoreEnvVars generates environment variables for secret store configuration.
// Supports Vault KV2 backend with URL, user, password, and secret mount configuration.
func (r *LakekeeperReconciler) buildSecretStoreEnvVars(lk *lakekeeperv1alpha1.Lakekeeper) []corev1.EnvVar {
	var envVars []corev1.EnvVar

	if lk.Spec.SecretStore == nil {
		return envVars
	}

	envVars = append(envVars, corev1.EnvVar{
		Name:  "LAKEKEEPER__SECRET_BACKEND",
		Value: string(lk.Spec.SecretStore.Type),
	})

	if lk.Spec.SecretStore.Type == lakekeeperv1alpha1.SecretStoreTypeVaultKV2 && lk.Spec.SecretStore.VaultKV2 != nil {
		vault := lk.Spec.SecretStore.VaultKV2

		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__VAULT_URL",
			Value: vault.URL,
		})

		// User - either from plain value or secret
		if vault.User != nil {
			envVars = append(envVars, corev1.EnvVar{
				Name:  "LAKEKEEPER__VAULT_USER",
				Value: *vault.User,
			})
		} else if vault.UserSecretRef != nil {
			envVars = append(envVars, corev1.EnvVar{
				Name: "LAKEKEEPER__VAULT_USER",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: vault.UserSecretRef,
				},
			})
		}

		envVars = append(envVars, corev1.EnvVar{
			Name: "LAKEKEEPER__VAULT_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &vault.PasswordSecretRef,
			},
		})

		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__VAULT_SECRET_MOUNT",
			Value: vault.SecretMount,
		})
	}

	return envVars
}

// buildAuthorizationEnvVars generates environment variables for authorization configuration.
// Supports allowall and OpenFGA backends with endpoint, store name, authorization model version,
// and client credentials.
func (r *LakekeeperReconciler) buildAuthorizationEnvVars(lk *lakekeeperv1alpha1.Lakekeeper) []corev1.EnvVar {
	envVars := []corev1.EnvVar{
		{
			Name:  "LAKEKEEPER__AUTHZ_BACKEND",
			Value: string(lk.Spec.Authorization.Backend),
		},
	}

	if lk.Spec.Authorization.Backend == lakekeeperv1alpha1.AuthzBackendOpenFGA && lk.Spec.Authorization.OpenFGA != nil {
		fga := lk.Spec.Authorization.OpenFGA

		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__OPENFGA_ENDPOINT",
			Value: fga.Endpoint,
		})

		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__OPENFGA_STORE_NAME",
			Value: fga.StoreName,
		})

		if fga.AuthorizationModelVersion != nil {
			envVars = append(envVars, corev1.EnvVar{
				Name:  "LAKEKEEPER__OPENFGA_AUTHORIZATION_MODEL_VERSION",
				Value: *fga.AuthorizationModelVersion,
			})
		}

		if fga.ClientID != nil {
			envVars = append(envVars, corev1.EnvVar{
				Name:  "LAKEKEEPER__OPENFGA_CLIENT_ID",
				Value: *fga.ClientID,
			})
		}

		if fga.ClientSecretRef != nil {
			envVars = append(envVars, corev1.EnvVar{
				Name: "LAKEKEEPER__OPENFGA_CLIENT_SECRET",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: fga.ClientSecretRef,
				},
			})
		}
	}

	return envVars
}

// buildAuthenticationEnvVars generates environment variables for authentication configuration.
// Supports OpenID Connect and Kubernetes authentication with provider URI, audience, claims,
// scopes, and token handling options.
func (r *LakekeeperReconciler) buildAuthenticationEnvVars(lk *lakekeeperv1alpha1.Lakekeeper) []corev1.EnvVar {
	var envVars []corev1.EnvVar

	if lk.Spec.Authentication == nil {
		return envVars
	}

	// OpenID Connect configuration
	if lk.Spec.Authentication.OpenID != nil {
		oidc := lk.Spec.Authentication.OpenID

		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__OPENID_PROVIDER_URI",
			Value: oidc.ProviderURI,
		})

		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__OPENID_AUDIENCE",
			Value: oidc.Audience,
		})

		if oidc.SubjectClaim != nil {
			envVars = append(envVars, corev1.EnvVar{
				Name:  "LAKEKEEPER__OPENID_SUBJECT_CLAIM",
				Value: *oidc.SubjectClaim,
			})
		}

		if oidc.RolesClaim != nil {
			envVars = append(envVars, corev1.EnvVar{
				Name:  "LAKEKEEPER__OPENID_ROLES_CLAIM",
				Value: *oidc.RolesClaim,
			})
		}

		if len(oidc.Scopes) > 0 {
			envVars = append(envVars, corev1.EnvVar{
				Name:  "LAKEKEEPER__OPENID_SCOPES",
				Value: strings.Join(oidc.Scopes, ","),
			})
		}
	}

	// Kubernetes authentication configuration
	if lk.Spec.Authentication.Kubernetes != nil {
		k8s := lk.Spec.Authentication.Kubernetes

		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__K8S_AUTH_ENABLED",
			Value: strconv.FormatBool(k8s.Enabled),
		})

		if len(k8s.Audiences) > 0 {
			envVars = append(envVars, corev1.EnvVar{
				Name:  "LAKEKEEPER__K8S_AUTH_AUDIENCES",
				Value: strings.Join(k8s.Audiences, ","),
			})
		}

		if k8s.AcceptLegacyTokens != nil {
			envVars = append(envVars, corev1.EnvVar{
				Name:  "LAKEKEEPER__K8S_AUTH_ACCEPT_LEGACY_TOKENS",
				Value: strconv.FormatBool(*k8s.AcceptLegacyTokens),
			})
		}
	}

	return envVars
}

// buildIdempotencyEnvVars generates environment variables for idempotency configuration.
// Includes enabled flag, lifetime, grace period, and cleanup timeout settings.
func (r *LakekeeperReconciler) buildIdempotencyEnvVars(lk *lakekeeperv1alpha1.Lakekeeper) []corev1.EnvVar {
	var envVars []corev1.EnvVar

	if lk.Spec.Idempotency == nil {
		return envVars
	}

	if lk.Spec.Idempotency.Enabled != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__IDEMPOTENCY_ENABLED",
			Value: strconv.FormatBool(*lk.Spec.Idempotency.Enabled),
		})
	}
	if lk.Spec.Idempotency.Lifetime != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__IDEMPOTENCY_LIFETIME",
			Value: *lk.Spec.Idempotency.Lifetime,
		})
	}
	if lk.Spec.Idempotency.GracePeriod != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__IDEMPOTENCY_GRACE_PERIOD",
			Value: *lk.Spec.Idempotency.GracePeriod,
		})
	}
	if lk.Spec.Idempotency.CleanupTimeout != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__IDEMPOTENCY_CLEANUP_TIMEOUT",
			Value: *lk.Spec.Idempotency.CleanupTimeout,
		})
	}

	return envVars
}

// buildTaskQueuesEnvVars generates environment variables for task queue configuration.
// Includes poll interval and worker counts for various background task types.
func (r *LakekeeperReconciler) buildTaskQueuesEnvVars(lk *lakekeeperv1alpha1.Lakekeeper) []corev1.EnvVar {
	var envVars []corev1.EnvVar

	if lk.Spec.TaskQueues == nil {
		return envVars
	}

	if lk.Spec.TaskQueues.PollInterval != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__TASK_QUEUE_POLL_INTERVAL",
			Value: *lk.Spec.TaskQueues.PollInterval,
		})
	}
	if lk.Spec.TaskQueues.TabularExpirationWorkers != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__TASK_QUEUE_TABULAR_EXPIRATION_WORKERS",
			Value: strconv.Itoa(int(*lk.Spec.TaskQueues.TabularExpirationWorkers)),
		})
	}
	if lk.Spec.TaskQueues.TabularPurgeWorkers != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__TASK_QUEUE_TABULAR_PURGE_WORKERS",
			Value: strconv.Itoa(int(*lk.Spec.TaskQueues.TabularPurgeWorkers)),
		})
	}
	if lk.Spec.TaskQueues.ExpireSnapshotsWorkers != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__TASK_QUEUE_EXPIRE_SNAPSHOTS_WORKERS",
			Value: strconv.Itoa(int(*lk.Spec.TaskQueues.ExpireSnapshotsWorkers)),
		})
	}

	return envVars
}

// buildRequestLimitsEnvVars generates environment variables for request limits configuration.
// Includes maximum request body size and maximum request time limits.
func (r *LakekeeperReconciler) buildRequestLimitsEnvVars(lk *lakekeeperv1alpha1.Lakekeeper) []corev1.EnvVar {
	var envVars []corev1.EnvVar

	if lk.Spec.RequestLimits == nil {
		return envVars
	}

	if lk.Spec.RequestLimits.MaxRequestBodySize != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__MAX_REQUEST_BODY_SIZE",
			Value: *lk.Spec.RequestLimits.MaxRequestBodySize,
		})
	}
	if lk.Spec.RequestLimits.MaxRequestTime != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__MAX_REQUEST_TIME",
			Value: *lk.Spec.RequestLimits.MaxRequestTime,
		})
	}

	return envVars
}

// buildSSLEnvVars generates environment variables for SSL/TLS configuration.
// Certificate file paths reference volume mounts (see buildSSLVolumes/buildSSLVolumeMounts).
func (r *LakekeeperReconciler) buildSSLEnvVars(lk *lakekeeperv1alpha1.Lakekeeper) []corev1.EnvVar {
	var envVars []corev1.EnvVar

	if lk.Spec.SSL == nil {
		return envVars
	}

	if lk.Spec.SSL.CertFileSecretRef != nil {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "LAKEKEEPER__SSL_CERT_FILE",
			Value: fmt.Sprintf("/etc/lakekeeper/ssl/lakekeeper-ssl-cert/%s", lk.Spec.SSL.CertFileSecretRef.Key),
		})
	}

	return envVars
}

// buildSSLVolumes returns Volumes needed to mount SSL certificate secrets.
func (r *LakekeeperReconciler) buildSSLVolumes(lk *lakekeeperv1alpha1.Lakekeeper) []corev1.Volume {
	var volumes []corev1.Volume

	if lk.Spec.Database.Type == lakekeeperv1alpha1.DatabaseTypePostgres &&
		lk.Spec.Database.Postgres != nil &&
		lk.Spec.Database.Postgres.SSLRootCertSecretRef != nil {
		volumes = append(volumes, corev1.Volume{
			Name: "pg-ssl-root-cert",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: lk.Spec.Database.Postgres.SSLRootCertSecretRef.Name,
				},
			},
		})
	}

	if lk.Spec.SSL != nil && lk.Spec.SSL.CertFileSecretRef != nil {
		volumes = append(volumes, corev1.Volume{
			Name: "lakekeeper-ssl-cert",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: lk.Spec.SSL.CertFileSecretRef.Name,
				},
			},
		})
	}

	return volumes
}

// buildSSLVolumeMounts returns VolumeMounts for the SSL certificate volumes.
func (r *LakekeeperReconciler) buildSSLVolumeMounts(lk *lakekeeperv1alpha1.Lakekeeper) []corev1.VolumeMount {
	var mounts []corev1.VolumeMount

	if lk.Spec.Database.Type == lakekeeperv1alpha1.DatabaseTypePostgres &&
		lk.Spec.Database.Postgres != nil &&
		lk.Spec.Database.Postgres.SSLRootCertSecretRef != nil {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      "pg-ssl-root-cert",
			MountPath: "/etc/lakekeeper/ssl/pg-root-cert",
			ReadOnly:  true,
		})
	}

	if lk.Spec.SSL != nil && lk.Spec.SSL.CertFileSecretRef != nil {
		mounts = append(mounts, corev1.VolumeMount{
			Name:      "lakekeeper-ssl-cert",
			MountPath: "/etc/lakekeeper/ssl/lakekeeper-ssl-cert",
			ReadOnly:  true,
		})
	}

	return mounts
}

// reconcileConfigWarnings inspects the spec for fields that are recognised by the API
// but not yet implemented in the controller, and sets a ConfigWarning status condition
// to inform the user. This is purely informational — it never blocks reconciliation.
func (r *LakekeeperReconciler) reconcileConfigWarnings(_ context.Context, lk *lakekeeperv1alpha1.Lakekeeper) {
	var warnings []string

	// spec.ssl.certDirConfigMapRef — not yet implemented
	if lk.Spec.SSL != nil && lk.Spec.SSL.CertDirConfigMapRef != nil {
		warnings = append(warnings, "spec.ssl.certDirConfigMapRef")
	}

	// spec.server.cors.allowHeaders — not yet implemented
	if lk.Spec.Server != nil && lk.Spec.Server.CORS != nil && len(lk.Spec.Server.CORS.AllowHeaders) > 0 {
		warnings = append(warnings, "spec.server.cors.allowHeaders")
	}

	// spec.server.cors.allowMethods — not yet implemented
	if lk.Spec.Server != nil && lk.Spec.Server.CORS != nil && len(lk.Spec.Server.CORS.AllowMethods) > 0 {
		warnings = append(warnings, "spec.server.cors.allowMethods")
	}

	// spec.server.storageSystemCredentials.azure — sub-fields not yet implemented
	if lk.Spec.Server != nil && lk.Spec.Server.StorageSystemCredentials != nil {
		creds := lk.Spec.Server.StorageSystemCredentials
		if ptr.Deref(creds.EnableAzure, false) && creds.Azure != nil {
			if creds.Azure.AllowManagedIdentity != nil || creds.Azure.AllowWorkloadIdentity != nil {
				warnings = append(warnings, "spec.server.storageSystemCredentials.azure")
			}
		}
		// spec.server.storageSystemCredentials.gcp — sub-fields not yet implemented
		if ptr.Deref(creds.EnableGCP, false) && creds.GCP != nil {
			if creds.GCP.AllowWorkloadIdentity != nil || creds.GCP.AllowServiceAccountKey != nil {
				warnings = append(warnings, "spec.server.storageSystemCredentials.gcp")
			}
		}
	}

	if len(warnings) > 0 {
		msg := "The following fields are configured but not yet implemented: " + strings.Join(warnings, ", ")
		r.setCondition(lk, TypeConfigWarning, metav1.ConditionTrue, "UnimplementedFieldsConfigured", msg)
	} else {
		r.setCondition(lk, TypeConfigWarning, metav1.ConditionFalse, "NoUnimplementedFields", "All configured fields are implemented")
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *LakekeeperReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&lakekeeperv1alpha1.Lakekeeper{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&batchv1.Job{}).
		Named("lakekeeper").
		Complete(r)
}

// HashMigrationRelevantSpec computes a hash of spec fields that affect database migrations.
//
// Migration Triggering Logic:
//   - Image changes: New Lakekeeper version may contain new migrations
//   - Database connection changes: Different database requires fresh migration
//
// Fields INCLUDED in hash (trigger new migration):
//   - spec.Image: Version changes may add migrations
//   - spec.Database.Type: Different database type (postgres, etc.)
//   - For Postgres:
//   - Host, Port, Database, User: Different database instance = need migration
//   - ReadHost: Replica host is emitted as LAKEKEEPER__PG_HOST_R (included)
//
// Fields EXCLUDED from hash (don't trigger migration):
//   - spec.Replicas: Doesn't affect database schema
//   - spec.Resources: Doesn't affect database schema
//   - spec.Server.*: Server config doesn't affect database
//   - spec.Authorization.*: Authz config doesn't affect database
//   - Secret values: Passwords/keys in secrets don't trigger migration
//     (changing encryption key may require manual data re-encryption, but this
//     is not handled by automatic migrations)
//   - SSLMode, ConnectionTimeout, MaxConnections: Connection tuning, not schema
//
// Design Decision: We prefer under-triggering to over-triggering migrations.
// Schema migrations are expensive and risky. Only clearly migration-relevant
// changes should trigger them. Configuration changes that don't affect schema
// should not force a re-migration of the same Lakekeeper version.
//
// This function is exported for testing purposes.
func HashMigrationRelevantSpec(spec lakekeeperv1alpha1.LakekeeperSpec) string {
	// Build a deterministic string representation of migration-relevant fields
	// Only Image and Database configuration affect migrations
	// Note: CRD validation should ensure that if Database.Type == "postgres",
	// then Database.Postgres != nil. This defensive check handles edge cases
	// during CRD validation changes or manual API calls bypassing validation.
	var dbConfig string
	if spec.Database.Type == lakekeeperv1alpha1.DatabaseTypePostgres {
		if spec.Database.Postgres == nil {
			// This should never happen due to CRD validation with CEL rules,
			// but we handle it defensively. Fall back to type-only hash.
			// Consider this a configuration error - should be caught earlier.
			dbConfig = string(spec.Database.Type)
			// Note: Can't log here as we don't have context. Caller should validate.
		} else {
			pg := spec.Database.Postgres
			// Dereference pointers to get actual values, not memory addresses
			// nil Port hashes as 0; CRD defaulting is expected to set a default before this is called.
			var port int32
			var user, readHost string

			if pg.Port != nil {
				port = *pg.Port
			}
			if pg.User != nil {
				user = *pg.User
			}
			if pg.ReadHost != nil {
				readHost = *pg.ReadHost
			}
			dbConfig = fmt.Sprintf("postgres|host:%s|port:%d|db:%s|user:%s|readHost:%s",
				pg.Host,
				port,
				pg.Database,
				user,
				readHost,
			)
		}
	} else {
		dbConfig = string(spec.Database.Type)
	}

	data := fmt.Sprintf("image:%s|database:%s", spec.Image, dbConfig)
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])
}

// needsMigration checks if a migration Job is needed for the current spec.
// Returns true if:
//   - No migration Job exists for the current spec hash
//   - The existing migration Job failed too many times (>=3 failures)
//
// Returns false if:
//   - A migration Job exists and succeeded
//   - A migration Job is still running (caller should wait)
func (r *LakekeeperReconciler) needsMigration(ctx context.Context, lk *lakekeeperv1alpha1.Lakekeeper) (bool, error) {
	logger := log.FromContext(ctx)

	// Calculate hash of migration-relevant spec fields
	specHash := HashMigrationRelevantSpec(lk.Spec)
	expectedJobName := fmt.Sprintf("%s-migrate-%s", lk.Name, specHash[:8])

	// Check if Job exists
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      expectedJobName,
		Namespace: lk.Namespace,
	}, job)

	if apierrors.IsNotFound(err) {
		// Job was TTL-cleaned or manually deleted.
		// If this exact spec hash was already successfully migrated, skip re-migration.
		if lk.Status.MigrationJob == expectedJobName &&
			meta.IsStatusConditionTrue(lk.Status.Conditions, TypeMigrated) {
			logger.V(1).Info("Migration Job TTL-cleaned but spec already migrated, skipping re-migration",
				"jobName", expectedJobName)
			return false, nil
		}
		// No Job exists for this spec → need migration
		logger.Info("Migration Job not found, needs creation", "jobName", expectedJobName)
		return true, nil
	}
	if err != nil {
		logger.Error(err, "Failed to get migration Job", "jobName", expectedJobName)
		return false, fmt.Errorf("failed to get migration job: %w", err)
	}

	// Job exists - check if it succeeded
	if job.Status.Succeeded > 0 {
		// Migration already complete
		logger.Info("Migration Job already succeeded", "jobName", expectedJobName)
		return false, nil
	}

	if job.Status.Failed >= migrationJobBackoffLimit {
		// Job failed too many times with current spec - permanent failure
		logger.Error(nil, "Migration Job failed permanently", "jobName", expectedJobName)
		return false, fmt.Errorf("migration job %s failed after %d attempts", expectedJobName, migrationJobBackoffLimit)
	}

	// Job still running - wait
	logger.Info("Migration Job still running", "jobName", expectedJobName)
	return false, nil
}

// createMigrationJob creates a Kubernetes Job to run database migrations.
// The Job name includes a hash of migration-relevant spec fields to ensure
// idempotency - the same spec always creates the same Job name.
func (r *LakekeeperReconciler) createMigrationJob(ctx context.Context, lk *lakekeeperv1alpha1.Lakekeeper) (*batchv1.Job, error) {
	logger := log.FromContext(ctx)

	specHash := HashMigrationRelevantSpec(lk.Spec)
	jobName := fmt.Sprintf("%s-migrate-%s", lk.Name, specHash[:8])

	// Check if Job already exists
	existingJob := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      jobName,
		Namespace: lk.Namespace,
	}, existingJob)

	if err == nil {
		// Job already exists
		logger.Info("Migration Job already exists", "jobName", jobName)
		return existingJob, nil
	}

	if !apierrors.IsNotFound(err) {
		logger.Error(err, "Failed to check for existing migration Job", "jobName", jobName)
		return nil, fmt.Errorf("failed to check for existing migration job: %w", err)
	}

	// Build Job spec
	labels := map[string]string{
		"app.kubernetes.io/name":      "lakekeeper",
		"app.kubernetes.io/component": "migration",
		"app.kubernetes.io/instance":  lk.Name,
	}

	backoffLimit := migrationJobBackoffLimit
	ttl := migrationJobTTLSeconds

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: lk.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						{
							Name:         "migrate",
							Image:        lk.Spec.Image,
							Args:         []string{"migrate"},
							Env:          r.buildEnvVars(lk),
							VolumeMounts: r.buildSSLVolumeMounts(lk),
							Resources:    resourceRequirementsOrEmpty(lk.Spec.Resources),
						},
					},
					Volumes: r.buildSSLVolumes(lk),
				},
			},
		},
	}

	// Set owner reference for cleanup
	if err := ctrl.SetControllerReference(lk, job, r.Scheme); err != nil {
		logger.Error(err, "Failed to set controller reference on migration Job")
		return nil, fmt.Errorf("failed to set controller reference: %w", err)
	}

	// Create Job
	if err := r.Create(ctx, job); err != nil {
		logger.Error(err, "Failed to create migration Job", "jobName", jobName)
		return nil, fmt.Errorf("failed to create migration job: %w", err)
	}

	logger.Info("Created migration Job", "jobName", jobName)
	return job, nil
}

// getMigrationJob retrieves the current migration Job for the given Lakekeeper spec.
func (r *LakekeeperReconciler) getMigrationJob(ctx context.Context, lk *lakekeeperv1alpha1.Lakekeeper) (*batchv1.Job, error) {
	logger := log.FromContext(ctx)

	specHash := HashMigrationRelevantSpec(lk.Spec)
	jobName := fmt.Sprintf("%s-migrate-%s", lk.Name, specHash[:8])

	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      jobName,
		Namespace: lk.Namespace,
	}, job)

	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(1).Info("Migration Job not found", "jobName", jobName)
		} else {
			logger.Error(err, "Failed to get migration Job", "jobName", jobName)
		}
		return nil, err
	}

	return job, nil
}

// isJobComplete returns true if the Job has finished (either succeeded or failed permanently).
func isJobComplete(job *batchv1.Job) bool {
	return job.Status.Succeeded > 0 || job.Status.Failed >= migrationJobBackoffLimit
}

// isJobSuccessful returns true if the Job completed successfully.
func isJobSuccessful(job *batchv1.Job) bool {
	return job.Status.Succeeded > 0
}

// resourceRequirementsOrEmpty dereferences a *corev1.ResourceRequirements pointer,
// returning an empty ResourceRequirements when the pointer is nil.
func resourceRequirementsOrEmpty(r *corev1.ResourceRequirements) corev1.ResourceRequirements {
	if r != nil {
		return *r
	}
	return corev1.ResourceRequirements{}
}
