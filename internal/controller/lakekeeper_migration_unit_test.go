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
	stderrors "errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	lakekeeperv1alpha1 "github.com/lakekeeper/lakekeeper-operator/api/v1alpha1"
)

// Pure-helper unit specs for migration error classification. These use a fake client
// (no envtest API server, no Reconcile), so they belong in a unit Describe per the
// two-context rule. The default fake scheme already registers batchv1.Job, so no
// explicit scheme is needed.

var _ = Describe("needsMigration error classification (unit)", func() {
	// A minimal Postgres-backed spec; only Image + Database feed the Job hash.
	newSpecLK := func() *lakekeeperv1alpha1.Lakekeeper {
		return &lakekeeperv1alpha1.Lakekeeper{
			ObjectMeta: metav1.ObjectMeta{Name: "classify", Namespace: "default"},
			Spec: lakekeeperv1alpha1.LakekeeperSpec{
				Image: "lakekeeper:v1.0.0",
				Database: lakekeeperv1alpha1.DatabaseConfig{
					Type:     lakekeeperv1alpha1.DatabaseTypePostgres,
					Postgres: &lakekeeperv1alpha1.PostgresConfig{Host: "pg", Database: "lk"},
				},
			},
		}
	}

	It("wraps errMigrationFailed when the migration Job is terminally Failed", func() {
		lk := newSpecLK()
		hash := HashMigrationRelevantSpec(lk.Spec)
		failedJob := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-migrate-%s", lk.Name, hash[:8]),
				Namespace: lk.Namespace,
			},
			Status: batchv1.JobStatus{
				Failed: 3,
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
				},
			},
		}
		c := fake.NewClientBuilder().WithObjects(failedJob).Build()
		r := &LakekeeperReconciler{Client: c, Scheme: c.Scheme()}

		_, err := r.needsMigration(context.Background(), lk)
		Expect(err).To(HaveOccurred())
		Expect(stderrors.Is(err, errMigrationFailed)).To(BeTrue(),
			"a terminal Job failure must classify as a permanent migration failure")
	})

	It("does not wrap errMigrationFailed on a transient API error", func() {
		lk := newSpecLK()
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

		_, err := r.needsMigration(context.Background(), lk)
		Expect(err).To(HaveOccurred())
		Expect(stderrors.Is(err, errMigrationFailed)).To(BeFalse(),
			"a transient API error must not be treated as a permanent migration failure")
	})
})
