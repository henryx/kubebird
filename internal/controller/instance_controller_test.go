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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kubebirdv1 "github.com/henryx/kubebird/api/v1"
)

const resourceNamespace = "default"

const testDatabaseName = "instance.fdb"

const (
	testImage   = "firebirdsql/firebird"
	testVersion = "3.0.14"
)

var _ = Describe("Instance Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		instance := &kubebirdv1.Instance{}
		secretName := types.NamespacedName{Name: resourceName + "-sysdba", Namespace: resourceNamespace}
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
						Image:   testImage,
						Version: testVersion,
						Databases: []kubebirdv1.DatabaseSpec{
							{Name: testDatabaseName},
						},
						Storage: kubebirdv1.StorageSpec{
							Primary: kubebirdv1.StorageVolumeSpec{
								Size: apiresource.MustParse("3Gi"),
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
			Expect(secret.OwnerReferences).To(BeEmpty(), "the SYSDBA Secret must not be owned by the Instance, so it survives deletion")

			By("registering a database alias in the aliases ConfigMap")
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, aliasesConfigMapNameNN, cm)).To(Succeed())
			Expect(cm.Data["databases.conf"]).To(ContainSubstring("instance.fdb = /var/lib/firebird/data/instance.fdb"))

			By("also registering the security database alias, since it mounts over the image's own databases.conf")
			Expect(cm.Data["databases.conf"]).To(ContainSubstring("security.db = /var/lib/firebird/data/security3.fdb"))
			Expect(cm.Data["databases.conf"]).To(ContainSubstring("RemoteAccess = false"))

			By("creating the Service exposing the instance")
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, svc)).To(Succeed())
			Expect(svc.Spec.Ports).To(HaveLen(1))
			Expect(svc.Spec.Ports[0].Port).To(Equal(int32(3050)))
			Expect(svc.Labels).To(HaveKeyWithValue("kubebird.github.io/instance", resourceName))

			By("creating a PVC named <instance>-primary for the primary data")
			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + "-primary", Namespace: resourceNamespace}, pvc)).To(Succeed())
			Expect(pvc.Spec.Resources.Requests[corev1.ResourceStorage]).To(Equal(apiresource.MustParse("3Gi")))
			Expect(pvc.Labels).To(HaveKeyWithValue("kubebird.github.io/instance", resourceName))
			Expect(pvc.OwnerReferences).To(BeEmpty(), "the PVC must not be owned by the Instance, so it isn't garbage-collected along with it")

			By("creating the StatefulSet running Firebird")
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, sts)).To(Succeed())
			Expect(sts.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(sts.Spec.Template.Spec.Containers[0].Image).To(Equal(testImage + ":" + testVersion))
			Expect(sts.Labels).To(HaveKeyWithValue("kubebird.github.io/instance", resourceName))

			By("annotating the pod template with a hash of the SYSDBA Secret's current password")
			Expect(sts.Spec.Template.Annotations).To(HaveKeyWithValue(
				"kubebird.github.io/sysdba-password-hash", sha256Hex(string(secret.Data["password"]))))

			By("running the security-database-init initContainer before it, to seed the primary PVC's security database")
			Expect(sts.Spec.Template.Spec.InitContainers).To(HaveLen(1))
			Expect(sts.Spec.Template.Spec.InitContainers[0].Name).To(Equal("security-database-init"))
			Expect(sts.Spec.Template.Spec.InitContainers[0].Image).To(Equal(testImage + ":" + testVersion))

			By("mounting the primary PVC by name rather than via a volumeClaimTemplate")
			Expect(sts.Spec.VolumeClaimTemplates).To(BeEmpty())
			var primaryVolume *corev1.Volume
			for i := range sts.Spec.Template.Spec.Volumes {
				if sts.Spec.Template.Spec.Volumes[i].Name == "primary" {
					primaryVolume = &sts.Spec.Template.Spec.Volumes[i]
				}
			}
			Expect(primaryVolume).NotTo(BeNil())
			Expect(primaryVolume.PersistentVolumeClaim).NotTo(BeNil())
			Expect(primaryVolume.PersistentVolumeClaim.ClaimName).To(Equal(resourceName + "-primary"))

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
			Expect(updated.Status.DatabaseCount).To(Equal(int32(0)))

			By("not reporting any reconcile error")
			Expect(updated.Status.Error).To(BeEmpty())

			By("mirroring the Available condition's message into status.message")
			Expect(updated.Status.Message).To(Equal("Waiting for the Firebird StatefulSet to become ready"))

			By("adding the finalizer so deletion can be observed")
			Expect(updated.Finalizers).To(ContainElement(finalizerName))
		})

		It("changes the StatefulSet's pod template when the SYSDBA Secret's password is rotated", func() {
			By("reconciling once to create the StatefulSet with the initial password's hash")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, sts)).To(Succeed())
			hashBefore := sts.Spec.Template.Annotations["kubebird.github.io/sysdba-password-hash"]
			Expect(hashBefore).NotTo(BeEmpty())

			By("overwriting the Secret's password")
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, secretName, secret)).To(Succeed())
			secret.Data["password"] = []byte("a-different-password")
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())

			By("reconciling again")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, typeNamespacedName, sts)).To(Succeed())

			By("changing the pod template's password-hash annotation, so the StatefulSet controller restarts the pod")
			Expect(sts.Spec.Template.Annotations["kubebird.github.io/sysdba-password-hash"]).
				NotTo(Equal(hashBefore))
		})

		It("fails to reconcile when spec.authentication.sysdba.secretRef names a Secret that doesn't exist", func() {
			By("reconciling the shared test-resource Instance created in BeforeEach, so the outer AfterEach cleanup finds its Secret")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("creating an Instance referencing a SYSDBA Secret that was never created")
			missingTypeNamespacedName := types.NamespacedName{Name: "test-missing-secretref", Namespace: resourceNamespace}
			resource := &kubebirdv1.Instance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      missingTypeNamespacedName.Name,
					Namespace: resourceNamespace,
				},
				Spec: kubebirdv1.InstanceSpec{
					Image:   testImage,
					Version: testVersion,
					Databases: []kubebirdv1.DatabaseSpec{
						{Name: testDatabaseName},
					},
					Storage: kubebirdv1.StorageSpec{
						Primary: kubebirdv1.StorageVolumeSpec{Size: apiresource.MustParse("1Gi")},
					},
					Authentication: kubebirdv1.AuthenticationSpec{
						Sysdba: kubebirdv1.SysdbaAuthSpec{SecretRef: "does-not-exist-sysdba"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			By("reconciling and expecting an error, since the referenced Secret doesn't exist")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: missingTypeNamespacedName})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("does-not-exist-sysdba"))

			By("not auto-creating a Secret with that name")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "does-not-exist-sysdba", Namespace: resourceNamespace},
				&corev1.Secret{})).To(HaveOccurred())

			By("cleaning up the Instance")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: missingTypeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
		})

		It("uses a pre-existing Secret unmodified when spec.authentication.sysdba.secretRef points to it, and doesn't delete it when the Instance is deleted", func() {
			By("reconciling the shared test-resource Instance created in BeforeEach, so the outer AfterEach cleanup finds its Secret")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("pre-creating a SYSDBA Secret with a fixed password before the Instance exists")
			presetSecretName := types.NamespacedName{Name: "test-preexisting-sysdba", Namespace: resourceNamespace}
			preset := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: presetSecretName.Name, Namespace: resourceNamespace},
				StringData: map[string]string{"username": "SYSDBA", "password": "a-preset-password"},
			}
			Expect(k8sClient.Create(ctx, preset)).To(Succeed())

			By("creating an Instance referencing that Secret")
			presetTypeNamespacedName := types.NamespacedName{Name: "test-preexisting-secretref", Namespace: resourceNamespace}
			resource := &kubebirdv1.Instance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      presetTypeNamespacedName.Name,
					Namespace: resourceNamespace,
				},
				Spec: kubebirdv1.InstanceSpec{
					Image:   testImage,
					Version: testVersion,
					Databases: []kubebirdv1.DatabaseSpec{
						{Name: testDatabaseName},
					},
					Storage: kubebirdv1.StorageSpec{
						Primary: kubebirdv1.StorageVolumeSpec{Size: apiresource.MustParse("1Gi")},
					},
					Authentication: kubebirdv1.AuthenticationSpec{
						Sysdba: kubebirdv1.SysdbaAuthSpec{SecretRef: presetSecretName.Name},
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			By("reconciling without error, using the pre-existing Secret as-is")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: presetTypeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("leaving the pre-existing Secret's password untouched")
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, presetSecretName, secret)).To(Succeed())
			Expect(string(secret.Data["password"])).To(Equal("a-preset-password"))

			By("deleting the Instance")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: presetTypeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, presetTypeNamespacedName, &kubebirdv1.Instance{})).To(HaveOccurred())

			By("not deleting the Secret, since it's never owned by the Instance")
			Expect(k8sClient.Get(ctx, presetSecretName, secret)).To(Succeed())
			Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
		})
	})

	Context("When deleting an Instance with a local backup volume configured", func() {
		const (
			backupResourceName = "test-backup-resource"
			backupSecretName   = backupResourceName + "-sysdba"
		)

		ctx := context.Background()
		backupTypeNamespacedName := types.NamespacedName{Name: backupResourceName, Namespace: resourceNamespace}
		var controllerReconciler *InstanceReconciler

		BeforeEach(func() {
			controllerReconciler = &InstanceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			resource := &kubebirdv1.Instance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      backupResourceName,
					Namespace: resourceNamespace,
				},
				Spec: kubebirdv1.InstanceSpec{
					Image:   testImage,
					Version: testVersion,
					Databases: []kubebirdv1.DatabaseSpec{
						{Name: testDatabaseName},
					},
					Storage: kubebirdv1.StorageSpec{
						Primary: kubebirdv1.StorageVolumeSpec{Size: apiresource.MustParse("1Gi")},
					},
					Backup: kubebirdv1.BackupSpec{
						Enabled: true,
						Destinations: []kubebirdv1.BackupDestinationSpec{
							{Local: &kubebirdv1.LocalBackupSpec{Storage: kubebirdv1.StorageVolumeSpec{Size: apiresource.MustParse("1Gi")}}},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: backupTypeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
		})

		It("blocks deletion until the Firebird pod stops, then releases the primary PVC but keeps the backup PVC", func() {
			By("the primary and backup PVCs existing after the first reconcile")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: backupResourceName + "-primary", Namespace: resourceNamespace},
				&corev1.PersistentVolumeClaim{})).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: backupResourceName + "-backup", Namespace: resourceNamespace},
				&corev1.PersistentVolumeClaim{})).To(Succeed())

			By("deleting the Instance before any database was ever provisioned")
			resource := &kubebirdv1.Instance{}
			Expect(k8sClient.Get(ctx, backupTypeNamespacedName, resource)).To(Succeed())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			By("reconciling once, which blocks on stopping the StatefulSet's pod to back up its databases")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: backupTypeNamespacedName})
			Expect(err).To(HaveOccurred())
			Expect(k8sClient.Get(ctx, backupTypeNamespacedName, resource)).To(Succeed())
			Expect(resource.Finalizers).To(ContainElement(finalizerName), "finalizer should not be removed until the backup completes")

			By("removing the StatefulSet, simulating it already being cleaned up, so deletion proceeds without a real pod to exec into")
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, backupTypeNamespacedName, sts)).To(Succeed())
			Expect(k8sClient.Delete(ctx, sts)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: backupTypeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, backupTypeNamespacedName, resource)).To(HaveOccurred())

			By("requesting deletion of the primary PVC, after backing it up since a local backup volume was configured")
			primaryPVC := &corev1.PersistentVolumeClaim{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: backupResourceName + "-primary", Namespace: resourceNamespace}, primaryPVC)
			if err == nil {
				// The StorageObjectInUseProtection admission plugin adds a
				// finalizer that only the (unrunning, in envtest)
				// pvc-protection controller removes, so the object may
				// still exist with a DeletionTimestamp rather than being
				// fully gone.
				Expect(primaryPVC.DeletionTimestamp).NotTo(BeNil())
			} else {
				Expect(errors.IsNotFound(err)).To(BeTrue())
			}

			By("not requesting deletion of the backup PVC itself")
			backupPVC := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: backupResourceName + "-backup", Namespace: resourceNamespace}, backupPVC)).To(Succeed())
			Expect(backupPVC.DeletionTimestamp).To(BeNil())

			By("Cleanup the SYSDBA Secret")
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: backupSecretName, Namespace: resourceNamespace}, secret)).To(Succeed())
			Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
		})

		It("reports the specific operation blocking deletion while stopping the pod", func() {
			By("faking a provisioned database in status, since envtest never gets a real pod ready")
			resource := &kubebirdv1.Instance{}
			Expect(k8sClient.Get(ctx, backupTypeNamespacedName, resource)).To(Succeed())
			resource.Status.Databases = []string{testDatabaseName}
			resource.Status.DatabaseCount = 1
			Expect(k8sClient.Status().Update(ctx, resource)).To(Succeed())

			By("deleting the Instance")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			By("reconciling once, which blocks on stopping the StatefulSet's pod to back up its databases")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: backupTypeNamespacedName})
			Expect(err).To(HaveOccurred())

			By("reporting the Deleting phase and the specific operation blocking it")
			updated := &kubebirdv1.Instance{}
			Expect(k8sClient.Get(ctx, backupTypeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.Phase).To(Equal(phaseDeleting))
			Expect(updated.Status.Message).To(Equal("Stopping the Firebird pod to back up its databases"))
			Expect(updated.Finalizers).To(ContainElement(finalizerName), "finalizer should not be removed until the backup completes")

			By("removing the StatefulSet, simulating it already being cleaned up, so deletion proceeds without a real pod to exec into")
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, backupTypeNamespacedName, sts)).To(Succeed())
			Expect(k8sClient.Delete(ctx, sts)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: backupTypeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, backupTypeNamespacedName, &kubebirdv1.Instance{})).To(HaveOccurred())

			By("Cleanup the SYSDBA Secret")
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: backupSecretName, Namespace: resourceNamespace}, secret)).To(Succeed())
			Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
		})

		It("stops the pod, runs the database backup Pod, and releases storage once it succeeds", func() {
			By("faking a provisioned database in status, since envtest never gets a real pod ready")
			resource := &kubebirdv1.Instance{}
			Expect(k8sClient.Get(ctx, backupTypeNamespacedName, resource)).To(Succeed())
			resource.Status.Databases = []string{testDatabaseName}
			resource.Status.DatabaseCount = 1
			Expect(k8sClient.Status().Update(ctx, resource)).To(Succeed())

			By("deleting the Instance")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			By("reconciling once, which scales the StatefulSet to zero replicas")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: backupTypeNamespacedName})
			Expect(err).To(HaveOccurred())
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, backupTypeNamespacedName, sts)).To(Succeed())
			Expect(*sts.Spec.Replicas).To(Equal(int32(0)))

			By("faking the StatefulSet still reporting its old pod, since nothing else updates status.replicas in envtest")
			sts.Status.Replicas = 1
			Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())

			By("reconciling again, which blocks until that pod actually stops")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: backupTypeNamespacedName})
			Expect(err).To(HaveOccurred())
			updated := &kubebirdv1.Instance{}
			Expect(k8sClient.Get(ctx, backupTypeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.Message).To(Equal("Stopping the Firebird pod to back up its databases"))

			By("faking the StatefulSet reporting the pod has actually stopped")
			sts.Status.Replicas = 0
			Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())

			By("reconciling again, which creates the database backup Pod")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: backupTypeNamespacedName})
			Expect(err).To(HaveOccurred())
			Expect(k8sClient.Get(ctx, backupTypeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.Message).To(Equal("Backing up databases into the backup volume"))
			backupPodName := types.NamespacedName{Name: backupResourceName + "-database-backup", Namespace: resourceNamespace}
			backupPod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, backupPodName, backupPod)).To(Succeed())
			Expect(backupPod.Spec.Containers[0].Command).To(ContainElement(ContainSubstring("gbak")))
			Expect(backupPod.Spec.Containers[0].Command).To(ContainElement(ContainSubstring(testDatabaseName)))
			var mountsPrimary, mountsBackup bool
			for _, m := range backupPod.Spec.Containers[0].VolumeMounts {
				mountsPrimary = mountsPrimary || m.Name == "primary"
				mountsBackup = mountsBackup || m.Name == "backup"
			}
			Expect(mountsPrimary).To(BeTrue(), "the backup Pod must mount the primary PVC")
			Expect(mountsBackup).To(BeTrue(), "the backup Pod must mount the backup PVC")

			By("faking the backup Pod succeeding, since envtest never actually runs it")
			backupPod.Status.Phase = corev1.PodSucceeded
			Expect(k8sClient.Status().Update(ctx, backupPod)).To(Succeed())

			By("reconciling again, which deletes the backup Pod and releases storage")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: backupTypeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, backupPodName, &corev1.Pod{})).To(HaveOccurred())
			Expect(k8sClient.Get(ctx, backupTypeNamespacedName, &kubebirdv1.Instance{})).To(HaveOccurred())

			By("Cleanup the SYSDBA Secret")
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: backupSecretName, Namespace: resourceNamespace}, secret)).To(Succeed())
			Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
		})
	})

	Context("When deleting an Instance with a local backup volume configured but backupOnDelete disabled", func() {
		const (
			noBackupOnDeleteResourceName = "test-no-backup-on-delete-resource"
			noBackupOnDeleteSecretName   = noBackupOnDeleteResourceName + "-sysdba"
		)

		ctx := context.Background()
		noBackupOnDeleteNamespacedName := types.NamespacedName{Name: noBackupOnDeleteResourceName, Namespace: resourceNamespace}
		var controllerReconciler *InstanceReconciler

		BeforeEach(func() {
			controllerReconciler = &InstanceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			resource := &kubebirdv1.Instance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      noBackupOnDeleteResourceName,
					Namespace: resourceNamespace,
				},
				Spec: kubebirdv1.InstanceSpec{
					Image:   testImage,
					Version: testVersion,
					Databases: []kubebirdv1.DatabaseSpec{
						{Name: testDatabaseName},
					},
					Storage: kubebirdv1.StorageSpec{
						Primary: kubebirdv1.StorageVolumeSpec{Size: apiresource.MustParse("1Gi")},
					},
					Backup: kubebirdv1.BackupSpec{
						Enabled:        true,
						BackupOnDelete: ptr.To(false),
						Destinations: []kubebirdv1.BackupDestinationSpec{
							{Local: &kubebirdv1.LocalBackupSpec{Storage: kubebirdv1.StorageVolumeSpec{Size: apiresource.MustParse("1Gi")}}},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: noBackupOnDeleteNamespacedName})
			Expect(err).NotTo(HaveOccurred())
		})

		It("releases the primary PVC in a single reconcile, without stopping the pod to back anything up first", func() {
			By("the primary and backup PVCs existing after the first reconcile")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: noBackupOnDeleteResourceName + "-primary", Namespace: resourceNamespace},
				&corev1.PersistentVolumeClaim{})).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: noBackupOnDeleteResourceName + "-backup", Namespace: resourceNamespace},
				&corev1.PersistentVolumeClaim{})).To(Succeed())

			By("faking a provisioned database in status, since envtest never gets a real pod ready")
			resource := &kubebirdv1.Instance{}
			Expect(k8sClient.Get(ctx, noBackupOnDeleteNamespacedName, resource)).To(Succeed())
			resource.Status.Databases = []string{testDatabaseName}
			resource.Status.DatabaseCount = 1
			Expect(k8sClient.Status().Update(ctx, resource)).To(Succeed())

			By("deleting the Instance")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			By("reconciling once, which completes deletion immediately since backupOnDelete is false")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: noBackupOnDeleteNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, noBackupOnDeleteNamespacedName, resource)).To(HaveOccurred())

			By("never scaling the StatefulSet down to back anything up")
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, noBackupOnDeleteNamespacedName, sts)).To(Succeed())
			Expect(*sts.Spec.Replicas).To(Equal(int32(1)))

			By("never creating a database backup Pod")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: noBackupOnDeleteResourceName + "-database-backup", Namespace: resourceNamespace},
				&corev1.Pod{})).To(HaveOccurred())

			By("requesting deletion of the primary PVC despite backupOnDelete being false")
			primaryPVC := &corev1.PersistentVolumeClaim{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: noBackupOnDeleteResourceName + "-primary", Namespace: resourceNamespace}, primaryPVC)
			if err == nil {
				// The StorageObjectInUseProtection admission plugin adds a
				// finalizer that only the (unrunning, in envtest)
				// pvc-protection controller removes, so the object may
				// still exist with a DeletionTimestamp rather than being
				// fully gone.
				Expect(primaryPVC.DeletionTimestamp).NotTo(BeNil())
			} else {
				Expect(errors.IsNotFound(err)).To(BeTrue())
			}

			By("not requesting deletion of the backup PVC itself")
			backupPVC := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: noBackupOnDeleteResourceName + "-backup", Namespace: resourceNamespace}, backupPVC)).To(Succeed())
			Expect(backupPVC.DeletionTimestamp).To(BeNil())

			By("Cleanup the SYSDBA Secret")
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: noBackupOnDeleteSecretName, Namespace: resourceNamespace}, secret)).To(Succeed())
			Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
		})
	})

	Context("When deleting an Instance without backup enabled", func() {
		const (
			noBackupResourceName = "test-no-backup-resource"
			noBackupSecretName   = noBackupResourceName + "-sysdba"
		)

		ctx := context.Background()
		noBackupTypeNamespacedName := types.NamespacedName{Name: noBackupResourceName, Namespace: resourceNamespace}
		var controllerReconciler *InstanceReconciler

		BeforeEach(func() {
			controllerReconciler = &InstanceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			resource := &kubebirdv1.Instance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      noBackupResourceName,
					Namespace: resourceNamespace,
				},
				Spec: kubebirdv1.InstanceSpec{
					Image:   testImage,
					Version: testVersion,
					Databases: []kubebirdv1.DatabaseSpec{
						{Name: testDatabaseName},
					},
					Storage: kubebirdv1.StorageSpec{
						Primary: kubebirdv1.StorageVolumeSpec{Size: apiresource.MustParse("1Gi")},
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: noBackupTypeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
		})

		It("releases the primary PVC in a single reconcile, without stopping the pod to back anything up first", func() {
			By("the primary PVC existing after the first reconcile")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: noBackupResourceName + "-primary", Namespace: resourceNamespace},
				&corev1.PersistentVolumeClaim{})).To(Succeed())

			By("deleting the Instance before any database was ever provisioned")
			resource := &kubebirdv1.Instance{}
			Expect(k8sClient.Get(ctx, noBackupTypeNamespacedName, resource)).To(Succeed())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			By("reconciling once, which completes deletion immediately since there's no backup to run first")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: noBackupTypeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, noBackupTypeNamespacedName, resource)).To(HaveOccurred())

			By("requesting deletion of the primary PVC even though backup was never enabled")
			primaryPVC := &corev1.PersistentVolumeClaim{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: noBackupResourceName + "-primary", Namespace: resourceNamespace}, primaryPVC)
			if err == nil {
				// The StorageObjectInUseProtection admission plugin adds a
				// finalizer that only the (unrunning, in envtest)
				// pvc-protection controller removes, so the object may
				// still exist with a DeletionTimestamp rather than being
				// fully gone.
				Expect(primaryPVC.DeletionTimestamp).NotTo(BeNil())
			} else {
				Expect(errors.IsNotFound(err)).To(BeTrue())
			}

			By("Cleanup the SYSDBA Secret")
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: noBackupSecretName, Namespace: resourceNamespace}, secret)).To(Succeed())
			Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
		})
	})

	Context("When backup is enabled but spec.backup.destinations has no local entry", func() {
		const (
			noLocalResourceName = "test-backup-no-local-resource"
			noLocalSecretName   = noLocalResourceName + "-sysdba"
		)

		ctx := context.Background()
		noLocalTypeNamespacedName := types.NamespacedName{Name: noLocalResourceName, Namespace: resourceNamespace}
		var controllerReconciler *InstanceReconciler

		BeforeEach(func() {
			controllerReconciler = &InstanceReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			resource := &kubebirdv1.Instance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      noLocalResourceName,
					Namespace: resourceNamespace,
				},
				Spec: kubebirdv1.InstanceSpec{
					Image:   testImage,
					Version: testVersion,
					Databases: []kubebirdv1.DatabaseSpec{
						{Name: testDatabaseName},
					},
					Storage: kubebirdv1.StorageSpec{
						Primary: kubebirdv1.StorageVolumeSpec{Size: apiresource.MustParse("1Gi")},
					},
					Backup: kubebirdv1.BackupSpec{
						Enabled:      true,
						Destinations: []kubebirdv1.BackupDestinationSpec{{}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: noLocalTypeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
		})

		It("never creates a backup PVC, and deletes cleanly in a single reconcile with nothing to back up into", func() {
			By("enabled being true not by itself creating a backup PVC, since spec.backup.destinations has no local entry")
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: noLocalResourceName + "-backup", Namespace: resourceNamespace},
				&corev1.PersistentVolumeClaim{})).To(HaveOccurred())

			By("deleting the Instance before any database was ever provisioned")
			resource := &kubebirdv1.Instance{}
			Expect(k8sClient.Get(ctx, noLocalTypeNamespacedName, resource)).To(Succeed())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			By("reconciling once, which completes deletion immediately since there is no backup volume to back up into or release")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: noLocalTypeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, noLocalTypeNamespacedName, resource)).To(HaveOccurred())

			By("Cleanup the SYSDBA Secret")
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: noLocalSecretName, Namespace: resourceNamespace}, secret)).To(Succeed())
			Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
		})
	})

	Context("When spec.backup.retention has a frequency enabled", func() {
		const (
			retentionResourceName = "test-retention-resource"
			retentionSecretName   = retentionResourceName + "-sysdba"
		)

		ctx := context.Background()
		retentionTypeNamespacedName := types.NamespacedName{Name: retentionResourceName, Namespace: resourceNamespace}
		var controllerReconciler *InstanceReconciler

		BeforeEach(func() {
			clientSet, err := kubernetes.NewForConfig(cfg)
			Expect(err).NotTo(HaveOccurred())
			controllerReconciler = &InstanceReconciler{
				Client:     k8sClient,
				Scheme:     k8sClient.Scheme(),
				RestConfig: cfg,
				ClientSet:  clientSet,
			}

			resource := &kubebirdv1.Instance{
				ObjectMeta: metav1.ObjectMeta{
					Name:      retentionResourceName,
					Namespace: resourceNamespace,
				},
				Spec: kubebirdv1.InstanceSpec{
					Image:   testImage,
					Version: testVersion,
					Databases: []kubebirdv1.DatabaseSpec{
						{Name: testDatabaseName},
					},
					Storage: kubebirdv1.StorageSpec{
						Primary: kubebirdv1.StorageVolumeSpec{Size: apiresource.MustParse("1Gi")},
					},
					Backup: kubebirdv1.BackupSpec{
						Enabled: true,
						Destinations: []kubebirdv1.BackupDestinationSpec{
							{Local: &kubebirdv1.LocalBackupSpec{Storage: kubebirdv1.StorageVolumeSpec{Size: apiresource.MustParse("1Gi")}}},
						},
						// Skips the delete-time backup/pod-stop dance
						// (covered by the earlier backup Contexts) so
						// this Context's AfterEach can clean up in a
						// single reconcile; orthogonal to what's under
						// test here.
						BackupOnDelete: ptr.To(false),
						Retention:      kubebirdv1.RetentionSpec{Hour: "2h"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: retentionTypeNamespacedName})
			Expect(err).NotTo(HaveOccurred())

			By("faking the StatefulSet pod as ready and the database as already provisioned, since envtest never runs a real pod")
			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, retentionTypeNamespacedName, sts)).To(Succeed())
			sts.Status.Replicas = 1
			sts.Status.ReadyReplicas = 1
			Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())

			Expect(k8sClient.Get(ctx, retentionTypeNamespacedName, resource)).To(Succeed())
			resource.Status.Databases = []string{testDatabaseName}
			resource.Status.DatabaseCount = 1
			Expect(k8sClient.Status().Update(ctx, resource)).To(Succeed())
		})

		AfterEach(func() {
			resource := &kubebirdv1.Instance{}
			Expect(k8sClient.Get(ctx, retentionTypeNamespacedName, resource)).To(Succeed())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			// envtest never runs real garbage collection, so the
			// StatefulSet's owner reference alone wouldn't remove it (or
			// the ReadyReplicas/Replicas this Context's BeforeEach fakes
			// on it) once the Instance is gone; deleting it explicitly
			// keeps that faked status from leaking into the next It's
			// fresh Instance of the same name. Likewise for the hourly
			// backup CronJob an It may have created.
			sts := &appsv1.StatefulSet{}
			if err := k8sClient.Get(ctx, retentionTypeNamespacedName, sts); err == nil {
				Expect(k8sClient.Delete(ctx, sts)).To(Succeed())
			}
			cronJobName := types.NamespacedName{Name: retentionResourceName + "-backup-hour", Namespace: resourceNamespace}
			if err := k8sClient.Get(ctx, cronJobName, &batchv1.CronJob{}); err == nil {
				Expect(k8sClient.Delete(ctx, &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{
					Name: cronJobName.Name, Namespace: cronJobName.Namespace}})).To(Succeed())
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: retentionTypeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, retentionTypeNamespacedName, &kubebirdv1.Instance{})).To(HaveOccurred())

			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: retentionSecretName, Namespace: resourceNamespace}, secret)).To(Succeed())
			Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
		})

		It("builds an hourly CronJob that backs up over the network via fbsvcmgr and prunes expired backups through the backup PVC", func() {
			// Calls mutateScheduledBackupCronJob directly rather than
			// going through Reconcile/reconcileScheduledBackups: the
			// latter first execs "mkdir -p" into the instance's own live
			// pod (see reconcileScheduledBackupCronJob) before creating a
			// frequency's CronJob for the first time, which envtest can't
			// satisfy since it never runs a real StatefulSet pod to exec
			// into. This still exercises the actual production code that
			// builds the CronJob, just not the exec-dependent step
			// around it — that round-trip needs a real cluster (e2e)
			// instead.
			resource := &kubebirdv1.Instance{}
			Expect(k8sClient.Get(ctx, retentionTypeNamespacedName, resource)).To(Succeed())

			var hour backupFrequency
			for _, freq := range backupFrequencies {
				if freq.name == backupFrequencyHour {
					hour = freq
				}
			}

			cronJob := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{
				Name:      retentionResourceName + "-backup-hour",
				Namespace: resourceNamespace,
			}}
			Expect(controllerReconciler.mutateScheduledBackupCronJob(cronJob, resource, hour)).To(Succeed())
			Expect(cronJob.OwnerReferences).NotTo(BeEmpty())
			Expect(cronJob.Spec.Schedule).To(Equal("0 * * * *"))
			Expect(cronJob.Spec.ConcurrencyPolicy).To(Equal(batchv1.ForbidConcurrent))

			command := cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Command
			Expect(command).To(ContainElement(ContainSubstring("fbsvcmgr")))
			Expect(command).To(ContainElement(ContainSubstring("-action_nbak")))
			Expect(command).To(ContainElement(ContainSubstring(retentionResourceName + "/3050:service_mgr")))
			Expect(command).To(ContainElement(ContainSubstring(testDatabaseName[:len(testDatabaseName)-len(".fdb")] + "-$TS.nbk")))
			Expect(command).NotTo(ContainElement(ContainSubstring("security3-$TS.nbk")),
				"the security database can't be nbackup'd while the server has it open, neither locally nor remotely, so it's deliberately excluded")
			Expect(command).NotTo(ContainElement(ContainSubstring("gzip")),
				"compression runs separately, exec'd into the live pod once a run's backup file appears")

			Expect(command).To(ContainElement(ContainSubstring(`CUTOFF=$(date -u -d "2 hours ago"`)),
				"the Job prunes backups older than spec.backup.retention.hour itself, right after taking a new one")

			podSpec := cronJob.Spec.JobTemplate.Spec.Template.Spec
			Expect(podSpec.Volumes).To(HaveLen(1))
			Expect(podSpec.Volumes[0].PersistentVolumeClaim).NotTo(BeNil())
			Expect(podSpec.Volumes[0].PersistentVolumeClaim.ClaimName).To(Equal(retentionResourceName+"-backup"),
				"the Job mounts only the backup PVC, to prune expired backups there")
			Expect(podSpec.Containers[0].VolumeMounts).To(ContainElement(
				corev1.VolumeMount{Name: backupVolumeName, MountPath: backupDataMountPath}))

			Expect(podSpec.Affinity).NotTo(BeNil())
			Expect(podSpec.Affinity.PodAffinity).NotTo(BeNil())
			terms := podSpec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution
			Expect(terms).To(HaveLen(1))
			Expect(terms[0].TopologyKey).To(Equal("kubernetes.io/hostname"))
			Expect(terms[0].LabelSelector.MatchLabels).To(Equal(firebirdPodSelector(retentionResourceName)),
				"the backup PVC is ReadWriteOnce, so the Job must land on the Firebird pod's own node")

			var hasPasswordEnv bool
			for _, e := range cronJob.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Env {
				hasPasswordEnv = hasPasswordEnv || e.Name == "SYSDBA_PASSWORD"
			}
			Expect(hasPasswordEnv).To(BeTrue())
		})

		It("fails clearly, without creating a CronJob, when the Firebird pod isn't reachable to prepare its backup directory", func() {
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: retentionTypeNamespacedName})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("failed to create backup directory"))

			cronJobName := types.NamespacedName{Name: retentionResourceName + "-backup-hour", Namespace: resourceNamespace}
			Expect(k8sClient.Get(ctx, cronJobName, &batchv1.CronJob{})).To(HaveOccurred(),
				"no CronJob should be created before its backup directory is prepared")
		})
	})
})
