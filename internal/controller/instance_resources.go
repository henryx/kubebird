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
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"path"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kubebirdv1 "github.com/henryx/kubebird/api/v1"
)

// generatedPasswordLength is the number of random bytes used to generate a
// SYSDBA password; base64-encoded this yields a 32-character string.
const generatedPasswordLength = 24

// Layout and naming conventions for resources managed on behalf of an
// Instance, following the firebirdsql/firebird image's documented
// environment variables and data volume layout.
const (
	firebirdPort            = 3050
	primaryDataMountPath    = "/var/lib/firebird/data"
	shadowDataMountPath     = "/var/lib/firebird/shadow"
	backupDataMountPath     = "/var/lib/firebird/backup"
	sysdbaSecretUsernameKey = "username"
	sysdbaSecretPasswordKey = "password"
	sysdbaUsername          = "SYSDBA"

	aliasesConfigMapKey  = "databases.conf"
	aliasesMountPath     = "/opt/firebird/databases.conf"
	aliasesMountFileName = "databases.conf"

	primaryVolumeName = "primary"
	shadowVolumeName  = "shadow"
	backupVolumeName  = "backup"
	aliasesVolumeName = "aliases"

	containerName                     = "firebird"
	securityDatabaseInitContainerName = "security-database-init"

	// instanceLabelKey labels every object Kubebird creates on behalf of
	// an Instance, so `kubectl get all,pvc,secrets,configmaps
	// -l kubebird.github.io/instance=<name>` finds all of them.
	instanceLabelKey = "kubebird.github.io/instance"

	// sysdbaPasswordAnnotationKey records a hash of the SYSDBA Secret's
	// current password on the StatefulSet's pod template (see
	// mutateStatefulSet), so that rotating the Secret changes the
	// template and lets the StatefulSet controller's own rolling update
	// restart the pod - the same path a brand-new pod already takes to
	// pick up FIREBIRD_ROOT_PASSWORD, rather than Kubebird pushing the
	// change to the live server itself.
	sysdbaPasswordAnnotationKey = "kubebird.github.io/sysdba-password-hash"
)

func labelsForInstance(name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "firebird",
		"app.kubernetes.io/instance":   name,
		"app.kubernetes.io/managed-by": "kubebird-controller",
		instanceLabelKey:               name,
	}
}

// sysdbaSecretRefName returns the Secret name backing
// spec.authentication.sysdba, defaulting to "<instance-name>-sysdba" when
// spec.authentication.sysdba.secretRef isn't specified.
func sysdbaSecretRefName(instance *kubebirdv1.Instance) string {
	if ref := instance.Spec.Authentication.Sysdba.SecretRef; ref != "" {
		return ref
	}
	return instance.Name + "-sysdba"
}

// aliasesConfigMapName returns the name of the ConfigMap registering
// database aliases for the instance.
func aliasesConfigMapName(instance *kubebirdv1.Instance) string {
	return instance.Name + "-aliases"
}

// primaryPVCName returns the name of the PVC backing the instance's
// primary data.
func primaryPVCName(instance *kubebirdv1.Instance) string {
	return instance.Name + "-" + primaryVolumeName
}

// shadowPVCName returns the name of the PVC backing the instance's
// shadow databases.
func shadowPVCName(instance *kubebirdv1.Instance) string {
	return instance.Name + "-" + shadowVolumeName
}

// backupPVCName returns the name of the PVC staging backups for the
// instance.
func backupPVCName(instance *kubebirdv1.Instance) string {
	return instance.Name + "-" + backupVolumeName
}

// reconcilePVCs ensures the PVCs backing instance.Spec.Storage exist,
// creating any that are missing.
func (r *InstanceReconciler) reconcilePVCs(ctx context.Context, instance *kubebirdv1.Instance) error {
	if err := r.reconcilePVC(ctx, instance, primaryPVCName(instance), instance.Spec.Storage.Primary); err != nil {
		return fmt.Errorf("failed to reconcile primary PVC: %w", err)
	}
	if instance.Spec.Storage.Backup != nil {
		if err := r.reconcilePVC(ctx, instance, backupPVCName(instance), *instance.Spec.Storage.Backup); err != nil {
			return fmt.Errorf("failed to reconcile backup PVC: %w", err)
		}
	}
	if instance.Spec.Storage.Shadow != nil {
		if err := r.reconcilePVC(ctx, instance, shadowPVCName(instance), *instance.Spec.Storage.Shadow); err != nil {
			return fmt.Errorf("failed to reconcile shadow PVC: %w", err)
		}
	}
	return nil
}

// reconcilePVC ensures a single PVC exists, creating it from vol when it
// doesn't. Like a StatefulSet's own volumeClaimTemplates, size and
// storage class are only applied at creation time: an existing PVC is
// left untouched. Unlike a StatefulSet's own PVCs, it isn't
// owner-referenced to the Instance, so it isn't garbage collected along
// with it — but only the backup PVC actually survives an Instance
// deletion this way: the primary and shadow PVCs are explicitly deleted
// by releasePrimaryAndShadowStorage regardless (see reconcileDeletion).
func (r *InstanceReconciler) reconcilePVC(ctx context.Context, instance *kubebirdv1.Instance, name string, vol kubebirdv1.StorageVolumeSpec) error {
	nsName := types.NamespacedName{Name: name, Namespace: instance.Namespace}
	if err := r.Get(ctx, nsName, &corev1.PersistentVolumeClaim{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get PVC %q: %w", name, err)
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    labelsForInstance(instance.Name),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: vol.Size},
			},
		},
	}
	if vol.Class != "" {
		pvc.Spec.StorageClassName = &vol.Class
	}

	if err := r.Create(ctx, pvc); err != nil {
		return fmt.Errorf("failed to create PVC %q: %w", name, err)
	}
	logf.FromContext(ctx).Info("Created PVC", "name", name)
	return nil
}

// deletePVC deletes the PVC named name, ignoring an already-missing one.
// Used by releasePrimaryAndShadowStorage to release the primary/shadow
// PVCs on every Instance deletion, regardless of whether storage.backup
// holds a final backup of their databases.
func (r *InstanceReconciler) deletePVC(ctx context.Context, instance *kubebirdv1.Instance, name string) error {
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: instance.Namespace}}
	if err := r.Delete(ctx, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to delete PVC %q: %w", name, err)
	}
	logf.FromContext(ctx).Info("Deleted PVC", "name", name)
	return nil
}

// reconcileSysdbaSecret ensures the Secret backing spec.authentication.sysdba
// exists. When spec.authentication.sysdba.secretRef is set, that Secret must
// already exist (it's the user's responsibility to have created it) and
// reconciliation fails if it doesn't; when unset, the default
// "<instance-name>-sysdba" Secret is created with a freshly generated random
// password if missing. An existing Secret, whether created by a previous
// reconcile or supplied by the user ahead of time, is left untouched. The
// Secret is deliberately never owner-referenced to the Instance, so it
// survives deleting the Instance rather than being garbage collected along
// with it.
func (r *InstanceReconciler) reconcileSysdbaSecret(ctx context.Context, instance *kubebirdv1.Instance) error {
	secretRef := sysdbaSecretRefName(instance)
	nsName := types.NamespacedName{Name: secretRef, Namespace: instance.Namespace}

	if err := r.Get(ctx, nsName, &corev1.Secret{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get SYSDBA Secret %q: %w", secretRef, err)
	}

	if instance.Spec.Authentication.Sysdba.SecretRef != "" {
		return fmt.Errorf("SYSDBA Secret %q specified in spec.authentication.sysdba.secretRef not found", secretRef)
	}

	password, err := generateRandomPassword()
	if err != nil {
		return fmt.Errorf("failed to generate SYSDBA password: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretRef,
			Namespace: instance.Namespace,
			Labels:    labelsForInstance(instance.Name),
		},
		StringData: map[string]string{
			sysdbaSecretUsernameKey: sysdbaUsername,
			sysdbaSecretPasswordKey: password,
		},
	}

	if err := r.Create(ctx, secret); err != nil {
		return fmt.Errorf("failed to create SYSDBA Secret %q: %w", secretRef, err)
	}
	logf.FromContext(ctx).Info("Created SYSDBA Secret with a generated password", "name", secretRef)
	return nil
}

// generateRandomPassword returns a cryptographically random, URL-safe
// base64-encoded string suitable for use as the SYSDBA password. The
// result never starts with '-': tools invoked with it as a bare
// "-password <value>" CLI argument (e.g. isql, in both Kubebird itself
// and its e2e tests) would otherwise misparse a leading '-' as a flag of
// its own rather than the password value.
func generateRandomPassword() (string, error) {
	for {
		buf := make([]byte, generatedPasswordLength)
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		password := base64.RawURLEncoding.EncodeToString(buf)
		if password[0] != '-' {
			return password, nil
		}
	}
}

// mutateAliasesConfigMap populates a ConfigMap with a Firebird
// databases.conf file registering one alias per database in
// instance.Spec.Databases, so clients can connect using the alias
// instead of the in-pod filesystem path. Uses db.Alias if set, otherwise
// falls back to the database's own name. It also registers a security.db
// alias for the instance's security database, since mounting this file
// over the image's own databases.conf (see aliasesMountPath) would
// otherwise drop the image's default one; RemoteAccess is disabled on it
// so it's only reachable through the embedded/local connection Kubebird
// itself uses at boot to apply FIREBIRD_ROOT_PASSWORD. The alias points
// at the security database's actual location on the primary PVC
// (securityDatabasePath) rather than the image's built-in $(dir_secDb)
// macro, matching the FIREBIRD_CONF_SecurityDatabase override set on the
// firebird container in mutateStatefulSet — $(dir_secDb) is a fixed macro
// tied to the image's install root and doesn't follow that override.
func (r *InstanceReconciler) mutateAliasesConfigMap(cm *corev1.ConfigMap, instance *kubebirdv1.Instance) error {
	cm.Labels = labelsForInstance(instance.Name)

	var b strings.Builder
	for _, db := range instance.Spec.Databases {
		alias := db.Name
		if db.Alias != nil && *db.Alias != "" {
			alias = *db.Alias
		}
		fmt.Fprintf(&b, "%s = %s\n", alias, path.Join(primaryDataMountPath, db.Name))
	}
	fmt.Fprintf(&b, "security.db = %s\n{\n\tRemoteAccess = false\n\tDefaultDbCachePages = 50\n}\n",
		securityDatabasePath(instance))
	cm.Data = map[string]string{aliasesConfigMapKey: b.String()}

	return controllerutil.SetControllerReference(instance, cm, r.Scheme)
}

// mutateService applies the desired spec to a Service exposing the
// instance. Only mutable fields are touched so that immutable ones
// (e.g. clusterIP) assigned by the API server are preserved on update.
func (r *InstanceReconciler) mutateService(svc *corev1.Service, instance *kubebirdv1.Instance) error {
	svcType := instance.Spec.Service.Type
	if svcType == "" {
		svcType = corev1.ServiceTypeClusterIP
	}
	port := instance.Spec.Service.Port
	if port == 0 {
		port = firebirdPort
	}

	svc.Labels = labelsForInstance(instance.Name)
	svc.Spec.Type = svcType
	svc.Spec.Selector = labelsForInstance(instance.Name)
	svc.Spec.Ports = []corev1.ServicePort{
		{
			Name:       containerName,
			Protocol:   corev1.ProtocolTCP,
			Port:       port,
			TargetPort: intstr.FromInt32(firebirdPort),
		},
	}

	return controllerutil.SetControllerReference(instance, svc, r.Scheme)
}

// mutateStatefulSet applies the desired spec to the StatefulSet running
// the Firebird server. The selector is immutable after creation, so it's
// only set the first time.
func (r *InstanceReconciler) mutateStatefulSet(ctx context.Context, sts *appsv1.StatefulSet, instance *kubebirdv1.Instance) error {
	labels := labelsForInstance(instance.Name)
	replicas := int32(1)

	if sts.CreationTimestamp.IsZero() {
		sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
	}

	password, err := r.sysdbaPassword(ctx, instance)
	if err != nil {
		return err
	}

	sts.Labels = labels
	sts.Spec.Replicas = &replicas
	sts.Spec.ServiceName = instance.Name
	sts.Spec.Template.Labels = labels
	// Forces a rolling recreate of the pod whenever the SYSDBA Secret's
	// password changes: FIREBIRD_ROOT_PASSWORD (below) is only resolved
	// and applied by the image's entrypoint at container start, so
	// nothing short of a restart picks up a rotated password. Kubebird
	// doesn't need to detect or push the change itself - it just has to
	// make sure the template actually changes, which this annotation
	// guarantees regardless of what else did or didn't change this
	// reconcile.
	sts.Spec.Template.Annotations = map[string]string{sysdbaPasswordAnnotationKey: sha256Hex(password)}
	sts.Spec.Template.Spec.Containers = []corev1.Container{
		{
			Name:  containerName,
			Image: fmt.Sprintf("%s:%s", instance.Spec.Image, instance.Spec.Version),
			// firebirdsql/firebird's entrypoint needs root's full DAC override (e.g. to
			// remove /opt/firebird/SYSDBA.password, and to write into /tmp/firebird,
			// both owned by the image's own "firebird" user) whenever FIREBIRD_ROOT_PASSWORD
			// is set, which Kubebird always does. So RunAsNonRoot is left unset and
			// Capabilities aren't dropped; this satisfies the "baseline" Pod Security
			// Standard, not "restricted".
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr.To(false),
				SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Ports: []corev1.ContainerPort{
				{Name: containerName, ContainerPort: firebirdPort},
			},
			Env: []corev1.EnvVar{
				{
					Name: "FIREBIRD_ROOT_PASSWORD",
					ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: sysdbaSecretRefName(instance)},
							Key:                  sysdbaSecretPasswordKey,
						},
					},
				},
				// Points the engine's own security database at its
				// persistent location on the primary PVC (see
				// securityDatabasePath) instead of the image's default
				// under /opt/firebird, which the security-database-init
				// initContainer below has already populated by the time
				// this container starts.
				{Name: "FIREBIRD_CONF_SecurityDatabase", Value: securityDatabasePath(instance)},
			},
			VolumeMounts: volumeMounts(instance),
		},
	}
	initVolumeMounts := []corev1.VolumeMount{{Name: primaryVolumeName, MountPath: primaryDataMountPath}}
	if instance.Spec.Storage.Backup != nil {
		// Lets securityDatabaseInitScript restore a security database
		// backup left behind by an earlier Instance's backupDatabases
		// under the same name, instead of always falling back to the
		// image's stock default.
		initVolumeMounts = append(initVolumeMounts, corev1.VolumeMount{Name: backupVolumeName, MountPath: backupDataMountPath})
	}
	sts.Spec.Template.Spec.InitContainers = []corev1.Container{
		{
			Name:  securityDatabaseInitContainerName,
			Image: fmt.Sprintf("%s:%s", instance.Spec.Image, instance.Spec.Version),
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr.To(false),
				SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Command:      []string{"sh", "-c", securityDatabaseInitScript(instance)},
			VolumeMounts: initVolumeMounts,
		},
	}
	sts.Spec.Template.Spec.Volumes = []corev1.Volume{
		{
			Name: primaryVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: primaryPVCName(instance)},
			},
		},
		{
			Name: aliasesVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: aliasesConfigMapName(instance)},
				},
			},
		},
	}
	if instance.Spec.Storage.Backup != nil {
		sts.Spec.Template.Spec.Volumes = append(sts.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: backupVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: backupPVCName(instance)},
			},
		})
	}
	if instance.Spec.Storage.Shadow != nil {
		sts.Spec.Template.Spec.Volumes = append(sts.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: shadowVolumeName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: shadowPVCName(instance)},
			},
		})
	}

	return controllerutil.SetControllerReference(instance, sts, r.Scheme)
}

func volumeMounts(instance *kubebirdv1.Instance) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		{Name: primaryVolumeName, MountPath: primaryDataMountPath},
		{
			Name:      aliasesVolumeName,
			MountPath: aliasesMountPath,
			SubPath:   aliasesMountFileName,
			ReadOnly:  true,
		},
	}
	if instance.Spec.Storage.Backup != nil {
		mounts = append(mounts, corev1.VolumeMount{Name: backupVolumeName, MountPath: backupDataMountPath})
	}
	if instance.Spec.Storage.Shadow != nil {
		mounts = append(mounts, corev1.VolumeMount{Name: shadowVolumeName, MountPath: shadowDataMountPath})
	}
	return mounts
}
