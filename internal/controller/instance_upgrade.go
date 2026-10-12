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
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kubebirdv1 "github.com/henryx/kubebird/api/v1"
)

// firebirdMajorVersion returns the major version of a Firebird version
// string (an image tag), e.g. 4 for "4.0.3" or "5.0.4-bookworm", and
// whether it could tell: a tag such as "latest" has none.
func firebirdMajorVersion(version string) (int, bool) {
	major, _, _ := strings.Cut(version, ".")
	major, _, _ = strings.Cut(major, "-")
	n, err := strconv.Atoi(major)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// statefulSetFirebirdVersion returns the Firebird version the StatefulSet
// currently runs, i.e. its firebird container's image tag (mutateStatefulSet
// sets it to "<spec.image>:<spec.version>"), or "" if there's none to tell.
func statefulSetFirebirdVersion(sts *appsv1.StatefulSet) string {
	for _, c := range sts.Spec.Template.Spec.Containers {
		if c.Name != containerName {
			continue
		}
		i := strings.LastIndex(c.Image, ":")
		if i < 0 || strings.Contains(c.Image[i:], "/") {
			return ""
		}
		return c.Image[i+1:]
	}
	return ""
}

// reconcileMajorUpgrade handles a change of spec.version from the version
// the StatefulSet runs (statefulSetFirebirdVersion) to another Firebird
// major version, reporting whether it took over this reconcile, in which
// case reconcileInstance stops there. Within the same major version (e.g.
// 4.0.3 to 4.0.5, or back) it does nothing: mutateStatefulSet then just
// moves the StatefulSet to the new image, which restarts the pod on it.
// The same goes when either version has no major version to tell (e.g.
// "latest").
//
// Moving to an older major version is refused: the databases' on-disk
// structure, and their backups, are only readable by the same or a newer
// major version. Moving to a newer one needs a local backup volume
// (backupVolumeSpec) and is recorded in status.upgrade, then follows the
// same steps as deleting and recreating the Instance, without deleting it:
//
//  1. BackingUp: backupDatabases stops the pod and backs up every database
//     in status.databases, plus the security database, with the old
//     version's own image and gbak (the new version's engine can't open
//     the old on-disk structure), into the backup volume's base
//     subdirectory. Like the deletion flow, every step is driven by the
//     cluster's state, waiting (see waitingError) until it's done.
//  2. Releasing: status.databases is cleared, then the primary and shadow
//     PVCs are deleted and waited for.
//  3. Restoring: reconcileInstance carries on as for a new Instance on
//     spec.version: it recreates the PVCs and moves the StatefulSet to the
//     new version, whose security-database-init restores the security
//     database from its backup (migrating its users when upgrading from
//     Firebird 3, see securityDatabaseInitScript), and reconcileDatabases
//     restores every database from its backup. finishMajorUpgrade clears
//     status.upgrade once it's all Ready.
//
// Each stage is recorded in status.upgrade.stage before the next one
// starts, which is what keeps a later reconcile from backing up again once
// the old storage may be gone, or from releasing the fresh storage being
// restored onto. As a second guard, nothing is released once the
// StatefulSet runs the new version. spec.version must stay at
// status.upgrade.toVersion until the upgrade finishes.
func (r *InstanceReconciler) reconcileMajorUpgrade(ctx context.Context, instance *kubebirdv1.Instance) (bool, error) {
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}, sts); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return true, fmt.Errorf("failed to get StatefulSet: %w", err)
	}

	// Every status write decodes the server's response back into
	// instance, replacing instance.Status.Upgrade, so it's read afresh
	// after each one rather than kept in a variable.
	if instance.Status.Upgrade == nil {
		started, err := r.startMajorUpgrade(ctx, instance, sts)
		if !started || err != nil {
			return err != nil, err
		}
	}
	upgrade := instance.Status.Upgrade

	if instance.Spec.Version != upgrade.ToVersion {
		return true, fmt.Errorf("spec.version changed to %s while upgrading Firebird from %s to %s; set it back to %s to let the upgrade finish",
			instance.Spec.Version, upgrade.FromVersion, upgrade.ToVersion, upgrade.ToVersion)
	}

	if upgrade.Stage == kubebirdv1.UpgradeStageBackingUp {
		if err := r.backupDatabases(ctx, instance, upgrade.FromVersion); err != nil {
			return true, err
		}
		logf.FromContext(ctx).Info("Backed up the databases for the Firebird upgrade",
			"from", upgrade.FromVersion, "to", upgrade.ToVersion)
		instance.Status.Databases = nil
		instance.Status.DatabaseCount = 0
		if err := r.setUpgradeStage(ctx, instance, kubebirdv1.UpgradeStageReleasing); err != nil {
			return true, err
		}
		upgrade = instance.Status.Upgrade
	}

	if upgrade.Stage == kubebirdv1.UpgradeStageReleasing {
		if statefulSetFirebirdVersion(sts) != upgrade.ToVersion {
			if done, err := r.releaseStorageForUpgrade(ctx, instance); !done || err != nil {
				return true, err
			}
		}
		logf.FromContext(ctx).Info("Starting the upgraded Firebird version on fresh storage",
			"from", upgrade.FromVersion, "to", upgrade.ToVersion)
		if err := r.setUpgradeStage(ctx, instance, kubebirdv1.UpgradeStageRestoring); err != nil {
			return true, err
		}
	}

	// Restoring: the rest of reconcileInstance does it.
	return false, nil
}

// releaseStorageForUpgrade deletes the primary and shadow PVCs (see
// releasePrimaryAndShadowStorage), reporting whether they're gone yet:
// reconcilePVCs would otherwise find one still terminating and leave it
// for the upgraded version's pod to start on.
func (r *InstanceReconciler) releaseStorageForUpgrade(ctx context.Context, instance *kubebirdv1.Instance) (bool, error) {
	if err := r.releasePrimaryAndShadowStorage(ctx, instance); err != nil {
		return false, err
	}
	names := []string{primaryPVCName(instance)}
	if instance.Spec.Storage.Shadow != nil {
		names = append(names, shadowPVCName(instance))
	}
	for _, name := range names {
		err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, &corev1.PersistentVolumeClaim{})
		if err == nil {
			return false, waiting("waiting for PVC %q to be released before restoring onto Firebird %s",
				name, instance.Status.Upgrade.ToVersion)
		}
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("failed to get PVC %q: %w", name, err)
		}
	}
	return true, nil
}

// setUpgradeStage records stage in status.upgrade, along with any other
// status change the caller made.
func (r *InstanceReconciler) setUpgradeStage(ctx context.Context, instance *kubebirdv1.Instance, stage kubebirdv1.UpgradeStage) error {
	instance.Status.Upgrade.Stage = stage
	if err := r.Status().Update(ctx, instance); err != nil {
		return fmt.Errorf("failed to record the Firebird upgrade's progress: %w", err)
	}
	return nil
}

// startMajorUpgrade records a new status.upgrade when spec.version moves
// sts to a newer Firebird major version, reporting whether it did, or
// fails when it can't (see reconcileMajorUpgrade).
func (r *InstanceReconciler) startMajorUpgrade(ctx context.Context, instance *kubebirdv1.Instance, sts *appsv1.StatefulSet) (bool, error) {
	running := statefulSetFirebirdVersion(sts)
	from, okFrom := firebirdMajorVersion(running)
	to, okTo := firebirdMajorVersion(instance.Spec.Version)
	if !okFrom || !okTo || from == to {
		return false, nil
	}
	if to < from {
		return false, fmt.Errorf("downgrading Firebird from %s to %s isn't supported: only a newer major version can be upgraded to; set spec.version back to a %d.x release",
			running, instance.Spec.Version, from)
	}
	if backupVolumeSpec(instance) == nil {
		return false, fmt.Errorf("upgrading Firebird from %s to %s needs a local backup volume (spec.backup.enabled with a local entry in spec.backup.destinations) to back up the databases into",
			running, instance.Spec.Version)
	}

	logf.FromContext(ctx).Info("Upgrading Firebird", "from", running, "to", instance.Spec.Version)
	instance.Status.Upgrade = &kubebirdv1.UpgradeStatus{
		FromVersion: running,
		ToVersion:   instance.Spec.Version,
		Stage:       kubebirdv1.UpgradeStageBackingUp,
	}
	instance.Status.Phase = phaseUpgrading
	if err := r.Status().Update(ctx, instance); err != nil {
		return false, fmt.Errorf("failed to record the Firebird upgrade: %w", err)
	}
	return true, nil
}

// finishMajorUpgrade clears status.upgrade, if any, once reconcileInstance
// finds the Instance Ready on the new version, with every database
// restored.
func (r *InstanceReconciler) finishMajorUpgrade(ctx context.Context, instance *kubebirdv1.Instance) error {
	upgrade := instance.Status.Upgrade
	if upgrade == nil {
		return nil
	}
	logf.FromContext(ctx).Info("Upgraded Firebird", "from", upgrade.FromVersion, "to", upgrade.ToVersion)
	instance.Status.Upgrade = nil
	if err := r.Status().Update(ctx, instance); err != nil {
		return fmt.Errorf("failed to record the Firebird upgrade's completion: %w", err)
	}
	return nil
}
