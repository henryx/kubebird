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
// +kubebuilder:rbac:groups="",namespace=kubebird-system,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",namespace=kubebird-system,resources=pods/exec,verbs=create

// Reconcile drives the cluster state for an Instance towards the desired
// state: a Secret-backed StatefulSet running Firebird, a Service exposing
// it, and a ConfigMap of database aliases, then exec's isql inside the
// pod once it is ready to create the requested databases and keep the
// live SYSDBA password in sync with the Secret.
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

	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: instance.Name, Namespace: instance.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		return r.mutateStatefulSet(sts, instance)
	}); err != nil {
		return fmt.Errorf("failed to reconcile StatefulSet: %w", err)
	}

	if err := r.reconcileSysdbaPassword(ctx, instance, sts); err != nil {
		return err
	}

	if err := r.reconcileDatabases(ctx, instance, sts); err != nil {
		return err
	}

	if sts.Status.ReadyReplicas > 0 {
		return r.setAvailable(ctx, instance, metav1.ConditionTrue,
			"StatefulSetReady", "Firebird instance is ready")
	}
	return r.setAvailable(ctx, instance, metav1.ConditionFalse,
		"StatefulSetNotReady", "Waiting for the Firebird StatefulSet to become ready")
}

// reconcileDeletion logs an Instance's deletion, reports status.phase as
// Deleting, and removes the finalizer so the API server can complete the
// deletion; the objects Kubebird created are removed by Kubernetes'
// garbage collection of their owner references.
func (r *InstanceReconciler) reconcileDeletion(ctx context.Context, instance *kubebirdv1.Instance) error {
	if !controllerutil.ContainsFinalizer(instance, finalizerName) {
		return nil
	}

	logf.FromContext(ctx).Info("Deleting Instance", "name", instance.Name)

	if instance.Status.Phase != phaseDeleting {
		instance.Status.Phase = phaseDeleting
		if err := r.Status().Update(ctx, instance); err != nil {
			return fmt.Errorf("failed to report Deleting phase: %w", err)
		}
	}

	controllerutil.RemoveFinalizer(instance, finalizerName)
	if err := r.Update(ctx, instance); err != nil {
		return fmt.Errorf("failed to remove finalizer: %w", err)
	}
	return nil
}

// setError records the message from the most recent reconcile failure in
// status.error, clearing it once Reconcile succeeds again. It skips the
// status write when nothing changed.
func (r *InstanceReconciler) setError(ctx context.Context, instance *kubebirdv1.Instance, reconcileErr error) error {
	message := ""
	if reconcileErr != nil {
		message = reconcileErr.Error()
	}
	if instance.Status.Error == message {
		return nil
	}
	instance.Status.Error = message
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
