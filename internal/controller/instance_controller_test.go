/*
Copyright 2026 Enrico Bianchi.

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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kubebirdv1 "github.com/henryx/kubebird/api/v1"
)

var _ = Describe("Instance Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-resource"
			resourceNamespace = "default"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		instance := &kubebirdv1.Instance{}
		secretName := types.NamespacedName{Name: "test-sysdba", Namespace: resourceNamespace}
		aliasesConfigMapNameNN := types.NamespacedName{Name: resourceName + "-aliases", Namespace: resourceNamespace}
		var controllerReconciler *InstanceReconciler

		BeforeEach(func() {
			controllerReconciler = &InstanceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			By("creating the custom resource for the Kind Instance")
			err := k8sClient.Get(ctx, typeNamespacedName, instance)
			if err != nil && errors.IsNotFound(err) {
				resource := &kubebirdv1.Instance{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: resourceNamespace,
					},
					Spec: kubebirdv1.InstanceSpec{
						Image:   "firebirdsql/firebird",
						Version: "3.0.14",
						Databases: []kubebirdv1.DatabaseSpec{
							{Name: "instance.fdb"},
						},
						Storage: kubebirdv1.StorageSpec{
							Primary: kubebirdv1.StorageVolumeSpec{
								Size: apiresource.MustParse("3Gi"),
							},
						},
						Authentication: kubebirdv1.AuthenticationSpec{
							Sysdba: kubebirdv1.SysdbaAuthSpec{
								SecretRef: "test-sysdba",
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &kubebirdv1.Instance{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance Instance")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			By("Letting the controller process the deletion and remove its finalizer")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, typeNamespacedName, resource)).To(HaveOccurred())

			By("Cleanup the SYSDBA Secret")
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, secretName, secret)).To(Succeed())
			Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
		})
		It("should reconcile the Service, ConfigMap and StatefulSet for the instance", func() {
			By("Reconciling the created resource")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("auto-creating the SYSDBA Secret with a generated password")
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, secretName, secret)).To(Succeed())
			Expect(secret.Data).To(HaveKeyWithValue("username", []byte("SYSDBA")))
			Expect(secret.Data).To(HaveKey("password"))
			Expect(secret.Data["password"]).NotTo(BeEmpty())
			Expect(secret.Labels).To(HaveKeyWithValue("kubebird.github.io/instance", resourceName))

			By("registering a database alias in the aliases ConfigMap")
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, aliasesConfigMapNameNN, cm)).To(Succeed())
			Expect(cm.Data["databases.conf"]).To(ContainSubstring("instance.fdb = /var/lib/firebird/data/instance.fdb"))

			By("creating the Service exposing the instance")
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, svc)).To(Succeed())
			Expect(svc.Spec.Ports).To(HaveLen(1))
			Expect(svc.Spec.Ports[0].Port).To(Equal(int32(3050)))
			Expect(svc.Labels).To(HaveKeyWithValue("kubebird.github.io/instance", resourceName))

			By("creating the StatefulSet running Firebird")
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, sts)).To(Succeed())
			Expect(sts.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(sts.Spec.Template.Spec.Containers[0].Image).To(Equal("firebirdsql/firebird:3.0.14"))
			Expect(sts.Labels).To(HaveKeyWithValue("kubebird.github.io/instance", resourceName))

			By("marking the instance as not yet Available")
			updated := &kubebirdv1.Instance{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			cond := apimeta.FindStatusCondition(updated.Status.Conditions, conditionTypeAvailable)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))

			By("reporting the Provisioning phase, since the StatefulSet pod isn't ready")
			Expect(updated.Status.Phase).To(Equal("Provisioning"))

			By("not yet provisioning any database, since the StatefulSet pod isn't ready")
			Expect(updated.Status.Databases).To(BeEmpty())

			By("not reporting any reconcile error")
			Expect(updated.Status.Error).To(BeEmpty())

			By("mirroring the Available condition's message into status.message")
			Expect(updated.Status.Message).To(Equal("Waiting for the Firebird StatefulSet to become ready"))

			By("adding the finalizer so deletion can be observed")
			Expect(updated.Finalizers).To(ContainElement(finalizerName))
		})
	})
})
