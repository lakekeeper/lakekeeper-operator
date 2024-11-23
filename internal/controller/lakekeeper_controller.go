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
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"strconv"

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
//+kubebuilder:rbac:groups=apps,resources=secrets;deployments;services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=core,resources=secrets;services;pods,verbs=create;update;patch;delete;get;list;watch

func (r *LakekeeperReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	lakekeeper := &cachev1alpha1.Lakekeeper{}
	err := r.Get(ctx, req.NamespacedName, lakekeeper)
	if err != nil {
		logger.Error(err, "unable to fetch lakekeeper")
		return ctrl.Result{}, nil
	}

	// Create Lakekeeper deployment if not found
	found := &appsv1.Deployment{}
	err = r.Get(ctx, types.NamespacedName{Name: lakekeeper.Name, Namespace: lakekeeper.Namespace}, found)
	if err != nil && errors.IsNotFound(err) {
		dep, err := r.getLakekeeperDeployment(lakekeeper)
		if err != nil {
			logger.Error(err, "unable to fetch deployment")
		}
		logger.Info("creating deployment", "namespace", lakekeeper.Namespace, "name", lakekeeper.Name)
		err = r.Create(ctx, dep)
		if err != nil {
			logger.Error(err, "unable to create deployment", "namespace", lakekeeper.Namespace, "name", lakekeeper.Name)
		}
	}

	// Create Lakekeeper config secret if not found
	foundSecret := &v1.Secret{}
	err = r.Get(ctx, types.NamespacedName{
		Namespace: lakekeeper.Namespace,
		Name:      lakekeeper.Name,
	}, foundSecret)

	if err != nil && errors.IsNotFound(err) {
		sec, err := r.getLakekeeperConfigSecret(lakekeeper)
		if err != nil {
			logger.Error(err, "unable to fetch secret")
		}
		logger.Info("creating secret", "namespace", lakekeeper.Namespace, "name", lakekeeper.Name)
		err = r.Create(ctx, sec)
		if err != nil {
			logger.Error(err, "unable to create secret", "namespace", lakekeeper.Namespace, "name", lakekeeper.Name)
		}
	}

	// Create Lakekeeper service if not found
	foundService := &v1.Service{}
	err = r.Get(ctx, types.NamespacedName{Name: lakekeeper.Name, Namespace: lakekeeper.Namespace}, foundService)
	if err != nil && errors.IsNotFound(err) {
		svc, err := r.getLakekeeperService(lakekeeper)
		if err != nil {
			logger.Error(err, "unable to fetch service")
		}
		logger.Info("creating service", "namespace", lakekeeper.Namespace, "name", lakekeeper.Name)
		err = r.Create(ctx, svc)
		if err != nil {
			logger.Error(err, "unable to create service", "namespace", lakekeeper.Namespace, "name", lakekeeper.Name)
		}
	}
	return ctrl.Result{}, nil
}

func (r *LakekeeperReconciler) getLakekeeperService(lakekeeper *cachev1alpha1.Lakekeeper) (*v1.Service, error) {
	service := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lakekeeper.Name,
			Namespace: lakekeeper.Namespace,
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{
					Port: 8095,
					TargetPort: intstr.IntOrString{
						IntVal: 8080,
					},
				},
			},
			Type: v1.ServiceTypeLoadBalancer,
		},
	}
	if err := ctrl.SetControllerReference(lakekeeper, service, r.Scheme); err != nil {
		return nil, err
	}
	return service, nil
}

func (r *LakekeeperReconciler) getLakekeeperConfigSecret(lakekeeper *cachev1alpha1.Lakekeeper) (*v1.Secret, error) {
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lakekeeper.Name + "-config-envs",
			Namespace: lakekeeper.Namespace,
		},
		Data: map[string][]byte{
			"ICEBERG_REST__PG_HOST_R":   []byte("postgres-postgresql"),
			"ICEBERG_REST__PG_HOST_W":   []byte("postgres-postgresql"),
			"ICEBERG_REST__PG_PORT":     []byte(strconv.Itoa(int(lakekeeper.Spec.ExternalDatabase.Port))),
			"ICEBERG_REST__PG_USER":     []byte(lakekeeper.Spec.ExternalDatabase.User),
			"ICEBERG_REST__PG_PASSWORD": []byte(lakekeeper.Spec.ExternalDatabase.Password),
			"ICEBERG_REST__PG_DATABASE": []byte(lakekeeper.Spec.ExternalDatabase.Database),
		},
	}
	if err := ctrl.SetControllerReference(lakekeeper, secret, r.Scheme); err != nil {
		return nil, err
	}
	return secret, nil
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
							Args: []string{
								"serve",
							},
							EnvFrom: []v1.EnvFromSource{
								{
									SecretRef: &v1.SecretEnvSource{
										LocalObjectReference: v1.LocalObjectReference{
											Name: lakekeeper.Name + "-config-envs",
										},
									},
								},
							},
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
		Owns(&appsv1.Deployment{}).
		Owns(&v1.Service{}).
		Complete(r)
}
