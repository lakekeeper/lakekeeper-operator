/*
Copyright 2024.

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
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cachev1alpha1 "github.com/peter-mcclonski/lakekeeper-operator/api/v1alpha1"
)

// LakekeeperReconciler reconciles a Lakekeeper object
type LakekeeperReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=cache.lakekeeper.io,resources=lakekeepers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cache.lakekeeper.io,resources=lakekeepers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cache.lakekeeper.io,resources=lakekeepers/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Lakekeeper object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.17.3/pkg/reconcile
func (r *LakekeeperReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	lakekeeper := &cachev1alpha1.Lakekeeper{}
	err := r.Get(ctx, req.NamespacedName, lakekeeper)
	if err != nil {
		log.Error(err, "unable to fetch lakekeeper")
		return ctrl.Result{}, nil
	}

	found := &appsv1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Name: lakekeeper.Name, Namespace: lakekeeper.Namespace}, found)
	if err != nil && errors.IsNotFound(err) {
		dep, err := r.getLakekeeperDeployment(lakekeeper)
		if err != nil {
			log.Error(err, "unable to fetch deployment")
		}
		log.Info("creating deployment", "namespace", lakekeeper.Namespace, "name", lakekeeper.Name)
		err = r.Create(ctx, dep)
		if err != nil {
			log.Error(err, "unable to create deployment", "namespace", lakekeeper.Namespace, "name", lakekeeper.Name)
		}
	}
	return ctrl.Result{}, nil
}

func (r *LakekeeperReconciler) getLakekeeperDeployment(lakekeeper *cachev1alpha1.Lakekeeper) (*appsv1.Deployment, error) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lakekeeper.Name,
			Namespace: lakekeeper.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &lakekeeper.Spec.Catalog.Replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labelsForLakekeeper(),
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labelsForLakekeeper(),
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Image:           lakekeeper.Spec.Catalog.Image.Repository + ":" + lakekeeper.Spec.Catalog.Image.Tag,
							Name:            lakekeeper.Name,
							ImagePullPolicy: v1.PullIfNotPresent,
						},
					},
				},
			},
		},
	}
	if err := ctrl.SetControllerReference(lakekeeper, dep, r.Scheme); err != nil {
		return nil, err
	}
	return dep, nil
}

// TODO
func labelsForLakekeeper() map[string]string {
	return map[string]string{"app.kubernetes.io/name": "lakekeeper-operator",
		"app.kubernetes.io/version":    "0.4.3",
		"app.kubernetes.io/managed-by": "LakekeeperController",
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *LakekeeperReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cachev1alpha1.Lakekeeper{}).
		Complete(r)
}
