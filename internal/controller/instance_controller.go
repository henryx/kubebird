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
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kubebirdv1 "github.com/henryx/kubebird/api/v1"
)

// conditionTypeAvailable reports whether the Firebird instance is up and
// serving connections.
const conditionTypeAvailable = "Available"

// secretRefIndexField indexes Instances by the Secret their SYSDBA
// credentials are read from, so a Secret change can be mapped back to the
// Instances that depend on it.
const secretRefIndexField = ".spec.authentication.sysdba.secretRef"

// finalizerName lets Reconcile observe and log an Instance's deletion,
// and report status.phase=Deleting, before the owned resources are
// garbage collected.
const finalizerName = "kubebird.github.io/finalizer"

// phaseProvisioning is the value Reconcile reports in status.phase while
// the StatefulSet or its databases aren't ready yet.
const phaseProvisioning = "Provisioning"

// phaseReady is the value Reconcile reports in status.phase once the
// StatefulSet pod is ready and every database in spec.databases has been
// created.
const phaseReady = "Ready"

// phaseDeleting is the value Reconcile reports in status.phase while an
// Instance's owned resources are being garbage collected.
const phaseDeleting = "Deleting"

// InstanceReconciler reconciles a Instance object
type InstanceReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// RestConfig and ClientSet are used to exec isql inside the Firebird
	// pod to provision databases; see reconcileDatabases.
	RestConfig *rest.Config
	ClientSet  kubernetes.Interface
}

// +kubebuilder:rbac:groups=kubebird.github.io,namespace=kubebird-system,resources=instances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubebird.github.io,namespace=kubebird-system,resources=instances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kubebird.github.io,namespace=kubebird-system,resources=instances/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,namespace=kubebird-system,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",namespace=kubebird-system,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",namespace=kubebird-system,resources=secrets,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",namespace=kubebird-system,resources=persistentvolumeclaims,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",namespace=kubebird-system,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",namespace=kubebird-system,resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",namespace=kubebird-system,resources=pods/exec,verbs=create

// Reconcile drives the cluster state for an Instance towards the desired
// state: a Secret-backed StatefulSet running Firebird, a Service exposing
// it, and a ConfigMap of database aliases, then exec's isql inside the
// pod once it is ready to create the requested databases. The StatefulSet
// pod template carries an annotation hashing the Secret's current
// password (see mutateStatefulSet), so a password rotation naturally
// triggers a pod restart, picked up by the image's own entrypoint the
// same way a delete/recreate of the Instance already does.
func (r *InstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	instance := &kubebirdv1.Instance{}
	if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get Instance: %w", err)
	}

	if !instance.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.reconcileDeletion(ctx, instance)
	}

	if !controllerutil.ContainsFinalizer(instance, finalizerName) {
		controllerutil.AddFinalizer(instance, finalizerName)
		if err := r.Update(ctx, instance); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
	}

	reconcileErr := r.reconcileInstance(ctx, instance)
	if err := r.setError(ctx, instance, reconcileErr); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, reconcileErr
}

// reconcileInstance drives the cluster state for a non-deleted Instance
// towards its desired state.
func (r *InstanceReconciler) reconcileInstance(ctx context.Context, instance *kubebirdv1.Instance) error {
	if err := r.reconcileSysdbaSecret(ctx, instance); err != nil {
		return err
	}

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: aliasesConfigMapName(instance), Namespace: instance.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		return r.mutateAliasesConfigMap(cm, instance)
	}); err != nil {
		return fmt.Errorf("failed to reconcile aliases ConfigMap: %w", err)
	}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: instance.Name, Namespace: instance.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		return r.mutateService(svc, instance)
	}); err != nil {
		return fmt.Errorf("failed to reconcile Service: %w", err)
	}

	if err := r.reconcilePVCs(ctx, instance); err != nil {
		return err
	}

	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: instance.Name, Namespace: instance.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		return r.mutateStatefulSet(ctx, sts, instance)
	}); err != nil {
		return fmt.Errorf("failed to reconcile StatefulSet: %w", err)
	}

	if err := r.reconcileDatabases(ctx, instance, sts); err != nil {
		return err
	}

	if sts.Status.ReadyReplicas > 0 && len(instance.Status.Databases) >= len(instance.Spec.Databases) {
		if err := r.setPhase(ctx, instance, phaseReady); err != nil {
			return err
		}
		return r.setAvailable(ctx, instance, metav1.ConditionTrue,
			"StatefulSetReady", "Firebird instance is ready")
	}
	if err := r.setPhase(ctx, instance, phaseProvisioning); err != nil {
		return err
	}
	return r.setAvailable(ctx, instance, metav1.ConditionFalse,
		"StatefulSetNotReady", "Waiting for the Firebird StatefulSet to become ready")
}

// reconcileDeletion logs an Instance's deletion, reports status.phase as
// Deleting and status.message with the specific operation currently
// underway (see setDeletionMessage), backs up its databases when a backup
// volume exists (backupVolumeSpec, i.e. spec.backup.enabled is true and
// spec.backup.type has a "local" entry) — see backupDatabases — always
// releases the primary/shadow PVCs regardless of spec.backup (see
// releasePrimaryAndShadowStorage — without a backup volume that data is
// simply gone), and removes the finalizer so the API server can complete
// the deletion; the objects Kubebird created are removed by Kubernetes'
// garbage collection of their owner references. The backup volume itself,
// when it exists, is never touched here: backupDatabases always leaves it
// in place for a later Instance recreated under the same name to restore
// from.
func (r *InstanceReconciler) reconcileDeletion(ctx context.Context, instance *kubebirdv1.Instance) error {
	if !controllerutil.ContainsFinalizer(instance, finalizerName) {
		return nil
	}

	logf.FromContext(ctx).Info("Deleting Instance", "name", instance.Name)

	if err := r.setPhase(ctx, instance, phaseDeleting); err != nil {
		return fmt.Errorf("failed to report Deleting phase: %w", err)
	}
	if err := r.setDeletionMessage(ctx, instance, "Deleting Instance"); err != nil {
		return err
	}

	if backupVolumeSpec(instance) != nil {
		if err := r.backupDatabases(ctx, instance); err != nil {
			return err
		}
	}

	if err := r.releasePrimaryAndShadowStorage(ctx, instance); err != nil {
		return err
	}

	if err := r.setDeletionMessage(ctx, instance, "Removing finalizer"); err != nil {
		return err
	}
	controllerutil.RemoveFinalizer(instance, finalizerName)
	if err := r.Update(ctx, instance); err != nil {
		return fmt.Errorf("failed to remove finalizer: %w", err)
	}
	return nil
}

// setDeletionMessage records the operation reconcileDeletion is currently
// performing in status.message, so `kubectl get instances` reflects
// deletion progress (e.g. while backupDatabases waits on the pod)
// instead of showing a stale pre-deletion message. Skips the write when
// the message hasn't changed. Unlike setError, this never touches
// status.error: deletion isn't a reconcile failure.
func (r *InstanceReconciler) setDeletionMessage(ctx context.Context, instance *kubebirdv1.Instance, message string) error {
	if instance.Status.Message == message {
		return nil
	}
	instance.Status.Message = message
	if err := r.Status().Update(ctx, instance); err != nil {
		return fmt.Errorf("failed to report deletion progress: %w", err)
	}
	return nil
}

// backupDatabases runs a final backup of every database recorded in
// instance.Status.Databases, plus the instance's security database (see
// backupDatabasesOffline), into the backup volume. Only called when a
// backup volume exists (backupVolumeSpec) — releasePrimaryAndShadowStorage
// deletes the primary/shadow PVCs regardless, so without a backup volume
// that data is simply lost.
//
// Every one of those backups runs the same way: gbak can't back up a
// database that a live server still has open (confirmed against the
// actual image: "Database already opened with engine instance,
// incompatible with current" over a local connection, and
// remote/service-manager access is refused outright regardless, at least
// for the security database) — so this scales the StatefulSet to 0
// replicas first, stopping the pod and releasing every database's live
// engine instance, then runs local gbak backups from a short-lived helper
// Pod that mounts the same PVCs instead (see backupDatabasesOffline).
// Unlike instance.Status.Databases, the security database always exists
// by this point regardless of spec.databases — the security-database-init
// initContainer seeds one even for an Instance with none — so this always
// backs it up too, rather than only when instance.Status.Databases is
// non-empty.
//
// Each stage is driven by observable cluster state rather than a status
// field, so it's naturally idempotent across the repeated reconciles this
// needs (StatefulSet scale-down and pod termination both take real time):
// spec.Replicas still non-zero means the pod hasn't been asked to stop
// yet; spec.Replicas zero but status.Replicas still non-zero means it
// hasn't fully stopped yet; both zero means it's safe to back up.
func (r *InstanceReconciler) backupDatabases(ctx context.Context, instance *kubebirdv1.Instance) error {
	sts := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}, sts)
	switch {
	case apierrors.IsNotFound(err):
		logf.FromContext(ctx).Info("Skipping final backup: StatefulSet no longer exists", "name", instance.Name)
		return nil
	case err != nil:
		return fmt.Errorf("failed to get StatefulSet: %w", err)
	case sts.Spec.Replicas == nil || *sts.Spec.Replicas != 0:
		if err := r.setDeletionMessage(ctx, instance, "Stopping the Firebird pod to back up its databases"); err != nil {
			return err
		}
		zero := int32(0)
		sts.Spec.Replicas = &zero
		if err := r.Update(ctx, sts); err != nil {
			return fmt.Errorf("failed to stop the Firebird pod: %w", err)
		}
		return fmt.Errorf("stopping the Firebird pod before backing up its databases")
	case sts.Status.Replicas != 0:
		if err := r.setDeletionMessage(ctx, instance, "Stopping the Firebird pod to back up its databases"); err != nil {
			return err
		}
		return fmt.Errorf("waiting for the Firebird pod to stop before backing up its databases")
	default:
		if err := r.setDeletionMessage(ctx, instance, "Backing up databases into the backup volume"); err != nil {
			return err
		}
		if err := r.backupDatabasesOffline(ctx, instance); err != nil {
			return err
		}
	}
	return nil
}

// releasePrimaryAndShadowStorage deletes the primary and (if configured)
// shadow PVCs on every Instance deletion — regardless of spec.backup —
// but never the backup PVC itself: when a backup volume exists,
// backupDatabases has already preserved this data there, and it's always
// left in place for a later Instance recreated under the same name to
// restore from; without a backup volume, the data is simply gone.
func (r *InstanceReconciler) releasePrimaryAndShadowStorage(ctx context.Context, instance *kubebirdv1.Instance) error {
	if err := r.setDeletionMessage(ctx, instance, "Releasing primary and shadow storage"); err != nil {
		return err
	}
	if err := r.deletePVC(ctx, instance, primaryPVCName(instance)); err != nil {
		return err
	}
	if instance.Spec.Storage.Shadow != nil {
		if err := r.deletePVC(ctx, instance, shadowPVCName(instance)); err != nil {
			return err
		}
	}
	return nil
}

// setError records the message from the most recent reconcile failure in
// status.error, clearing it once Reconcile succeeds again, and mirrors a
// human-readable summary into status.message: the error itself when set,
// otherwise status.warning when reconcileDatabases has set one (see
// orphanedBackups), otherwise the Available condition's message. It skips
// the status write when nothing changed.
func (r *InstanceReconciler) setError(ctx context.Context, instance *kubebirdv1.Instance, reconcileErr error) error {
	message := ""
	if reconcileErr != nil {
		message = reconcileErr.Error()
	}

	summary := message
	if summary == "" {
		summary = instance.Status.Warning
	}
	if summary == "" {
		if cond := apimeta.FindStatusCondition(instance.Status.Conditions, conditionTypeAvailable); cond != nil {
			summary = cond.Message
		}
	}

	if instance.Status.Error == message && instance.Status.Message == summary {
		return nil
	}
	instance.Status.Error = message
	instance.Status.Message = summary
	return r.Status().Update(ctx, instance)
}

// setPhase records instance's high-level lifecycle phase, skipping the
// status write when nothing changed.
func (r *InstanceReconciler) setPhase(ctx context.Context, instance *kubebirdv1.Instance, phase string) error {
	if instance.Status.Phase == phase {
		return nil
	}
	instance.Status.Phase = phase
	return r.Status().Update(ctx, instance)
}

// setAvailable records the Available condition on instance, skipping the
// status write when nothing changed.
func (r *InstanceReconciler) setAvailable(ctx context.Context, instance *kubebirdv1.Instance, status metav1.ConditionStatus, reason, message string) error {
	changed := apimeta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
		Type:               conditionTypeAvailable,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: instance.Generation,
	})
	if !changed {
		return nil
	}
	return r.Status().Update(ctx, instance)
}

// findInstancesForSecret maps a Secret to the Instances that reference it
// as their SYSDBA credentials, so Reconcile runs as soon as the Secret
// appears or changes instead of waiting for a poll.
func (r *InstanceReconciler) findInstancesForSecret(ctx context.Context, secret client.Object) []reconcile.Request {
	var list kubebirdv1.InstanceList
	if err := r.List(ctx, &list,
		client.InNamespace(secret.GetNamespace()),
		client.MatchingFields{secretRefIndexField: secret.GetName()},
	); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list Instances for Secret", "secret", secret.GetName())
		return nil
	}

	requests := make([]reconcile.Request, 0, len(list.Items))
	for _, item := range list.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: item.Name, Namespace: item.Namespace},
		})
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *InstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &kubebirdv1.Instance{}, secretRefIndexField,
		func(obj client.Object) []string {
			instance, ok := obj.(*kubebirdv1.Instance)
			if !ok {
				return nil
			}
			return []string{sysdbaSecretRefName(instance)}
		}); err != nil {
		return fmt.Errorf("failed to index Instance by SYSDBA secretRef: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&kubebirdv1.Instance{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.findInstancesForSecret)).
		Named("instance").
		Complete(r)
}
