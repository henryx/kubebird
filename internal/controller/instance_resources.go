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
	sysdbaSecretUsernameKey = "username"
	sysdbaSecretPasswordKey = "password"
	sysdbaUsername          = "SYSDBA"

	aliasesConfigMapKey  = "databases.conf"
	aliasesMountPath     = "/opt/firebird/databases.conf"
	aliasesMountFileName = "databases.conf"

	primaryVolumeName = "primary"
	shadowVolumeName  = "shadow"
	aliasesVolumeName = "aliases"

	containerName = "firebird"

	// instanceLabelKey labels every object Kubebird creates on behalf of
	// an Instance, so `kubectl get all,pvc,secrets,configmaps
	// -l kubebird.github.io/instance=<name>` finds all of them.
	instanceLabelKey = "kubebird.github.io/instance"
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

// reconcileSysdbaSecret ensures the Secret backing spec.authentication.sysdba
// exists, creating it with a freshly generated random password when it does
// not. An existing Secret, whether created by a previous reconcile or
// supplied by the user ahead of time, is left untouched.
func (r *InstanceReconciler) reconcileSysdbaSecret(ctx context.Context, instance *kubebirdv1.Instance) error {
	secretRef := sysdbaSecretRefName(instance)
	nsName := types.NamespacedName{Name: secretRef, Namespace: instance.Namespace}

	if err := r.Get(ctx, nsName, &corev1.Secret{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get SYSDBA Secret %q: %w", secretRef, err)
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
	if err := controllerutil.SetControllerReference(instance, secret, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference on SYSDBA Secret: %w", err)
	}

	if err := r.Create(ctx, secret); err != nil {
		return fmt.Errorf("failed to create SYSDBA Secret %q: %w", secretRef, err)
	}
	logf.FromContext(ctx).Info("Created SYSDBA Secret with a generated password", "name", secretRef)
	return nil
}

// generateRandomPassword returns a cryptographically random, URL-safe
// base64-encoded string suitable for use as the SYSDBA password.
func generateRandomPassword() (string, error) {
	buf := make([]byte, generatedPasswordLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// mutateAliasesConfigMap populates a ConfigMap with a Firebird
// databases.conf file registering one alias per database in
// instance.Spec.Databases, so clients can connect using the alias
// instead of the in-pod filesystem path. Uses db.Alias if set, otherwise
// falls back to the database's own name.
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
// the Firebird server. Fields that are immutable after creation
// (selector, volumeClaimTemplates) are only set the first time.
func (r *InstanceReconciler) mutateStatefulSet(sts *appsv1.StatefulSet, instance *kubebirdv1.Instance) error {
	labels := labelsForInstance(instance.Name)
	replicas := int32(1)

	if sts.CreationTimestamp.IsZero() {
		sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		sts.Spec.VolumeClaimTemplates = volumeClaimTemplates(instance)
	}

	sts.Labels = labels
	sts.Spec.Replicas = &replicas
	sts.Spec.ServiceName = instance.Name
	sts.Spec.Template.Labels = labels
	sts.Spec.Template.Spec.Containers = []corev1.Container{
		{
			Name:  containerName,
			Image: fmt.Sprintf("%s:%s", instance.Spec.Image, instance.Spec.Version),
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
			},
			VolumeMounts: volumeMounts(instance),
		},
	}
	sts.Spec.Template.Spec.Volumes = []corev1.Volume{
		{
			Name: aliasesVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: aliasesConfigMapName(instance)},
				},
			},
		},
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
	if instance.Spec.Storage.Shadow != nil {
		mounts = append(mounts, corev1.VolumeMount{Name: shadowVolumeName, MountPath: shadowDataMountPath})
	}
	return mounts
}

func volumeClaimTemplates(instance *kubebirdv1.Instance) []corev1.PersistentVolumeClaim {
	templates := []corev1.PersistentVolumeClaim{
		volumeClaimTemplate(primaryVolumeName, instance.Spec.Storage.Primary, labelsForInstance(instance.Name)),
	}
	if instance.Spec.Storage.Shadow != nil {
		templates = append(templates, volumeClaimTemplate(shadowVolumeName, *instance.Spec.Storage.Shadow, labelsForInstance(instance.Name)))
	}
	return templates
}

func volumeClaimTemplate(name string, vol kubebirdv1.StorageVolumeSpec, labels map[string]string) corev1.PersistentVolumeClaim {
	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
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
	return pvc
}
