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
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kubebirdv1 "github.com/henryx/kubebird/api/v1"
)

// An s3 destination without a local one streams every backup straight into
// the restic repository, with no backup PVC at all: gbak writes the backup
// to its stdout, and "restic backup --stdin-from-command" stores it as a
// snapshot of a single file, under the path it would have on the backup
// volume. gbak runs from the Firebird image, which has no restic, and
// restic from its own, which has no gbak, while a pipe can't cross
// containers: so an initContainer copies the restic binary (static, so it
// runs anywhere) from the restic image into an emptyDir, and the Firebird
// image's container runs it from there (streamingResticBin).
//
// Confirmed against the 3.0.14, 4.0.3 and 5.0.4 images: "gbak -backup -se
// <host>/<port>:service_mgr <database> stdout" streams a valid backup from
// the live server, and so does a local "gbak -backup <file> stdout" with
// the server stopped, security database included; restored with gbak, a
// 220 MB database's data matched. A wrong password, a missing database or
// the server dying mid-stream make gbak exit non-zero, and restic then
// saves no snapshot: --stdin-from-command checks the command's exit status,
// which "gbak | restic backup --stdin" couldn't.
//
// Only gbak backups can be streamed this way (the CRD requires a local
// destination for mode.type physical), and the security database is still
// only backed up at deletion and by a major upgrade, as with a local
// destination.
const (
	// streamingResticBinVolumeName and streamingResticBinDir hold the
	// restic binary copied by the resticInstallContainerName
	// initContainer.
	streamingResticBinVolumeName = "restic-bin"
	streamingResticBinDir        = "/kubebird/bin"

	// resticInstallContainerName names the initContainer copying restic's
	// binary into streamingResticBinDir.
	resticInstallContainerName = "restic-install"

	// streamingGbakStdout is the backup file name gbak treats as its
	// standard output.
	streamingGbakStdout = "stdout"
)

// streamingResticBin is the restic binary the containers streaming backups
// run.
var streamingResticBin = path.Join(streamingResticBinDir, binRestic)

// s3RestoredMarker is the file finishS3Restore leaves on the primary volume
// once every database has been restored (or created), so the
// backup-restore initContainer doesn't restore from the repository again
// on every later start of the pod (see resticBootstrapScript). A major
// upgrade, deleting the Instance or losing the primary volume take it
// away along with the primary volume, so the next start restores again;
// so does adding a database (see reconcileS3Restore), which removes it.
var s3RestoredMarker = path.Join(primaryDataMountPath, ".kubebird-s3-restored")

// s3Streaming reports whether the instance streams its backups into an s3
// destination, i.e. has one and no local destination to stage them in.
func s3Streaming(instance *kubebirdv1.Instance) bool {
	return s3Destination(instance) != nil && backupVolumeSpec(instance) == nil
}

// hasBackupDestination reports whether the instance backs up anywhere: a
// local destination (backupVolumeSpec), an s3 one (s3Destination), or
// both. Deletion, major upgrades, scheduled backups and restores all only
// need one.
func hasBackupDestination(instance *kubebirdv1.Instance) bool {
	return backupVolumeSpec(instance) != nil || s3Destination(instance) != nil
}

// backupVolume returns the StatefulSet's backup volume: the backup PVC with
// a local destination, otherwise an emptyDir the backup-restore
// initContainer restores the "base" snapshots into (see
// resticBootstrapScript), from which security-database-init and
// restoreDatabaseIfBackedUp restore as from a local backup.
func backupVolume(instance *kubebirdv1.Instance) corev1.Volume {
	if backupVolumeSpec(instance) == nil {
		return corev1.Volume{Name: backupVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
	}
	return corev1.Volume{
		Name: backupVolumeName,
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: backupPVCName(instance)},
		},
	}
}

// addStreamingRestic sets podSpec up for container, one of its containers
// running the Firebird image, to stream backups with streamingResticBin:
// an initContainer copying restic's binary into the emptyDir both mount,
// plus the repository's environment and password. Unlike the best effort
// upload next to a local destination, nothing here is optional: without a
// local destination, a backup that can't be uploaded isn't kept anywhere.
func addStreamingRestic(podSpec *corev1.PodSpec, container *corev1.Container, instance *kubebirdv1.Instance, s3 *kubebirdv1.S3BackupSpec) {
	bin := corev1.VolumeMount{Name: streamingResticBinVolumeName, MountPath: streamingResticBinDir}
	podSpec.InitContainers = append(podSpec.InitContainers, corev1.Container{
		Name:  resticInstallContainerName,
		Image: resticImage(s3),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Command:      []string{"sh", "-c", fmt.Sprintf(`cp "$(command -v %s)" %q`, binRestic, streamingResticBin)},
		VolumeMounts: []corev1.VolumeMount{bin},
	})
	podSpec.Volumes = append(podSpec.Volumes,
		corev1.Volume{Name: streamingResticBinVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		resticPasswordVolume(instance, s3, false),
	)
	container.Env = append(container.Env, resticEnv(instance, s3, false)...)
	container.VolumeMounts = append(container.VolumeMounts,
		corev1.VolumeMount{Name: streamingResticBinVolumeName, MountPath: streamingResticBinDir, ReadOnly: true},
		corev1.VolumeMount{Name: resticPasswordVolumeName, MountPath: resticPasswordMountPath, ReadOnly: true},
	)
}

// resticRunTagPrefix prefixes the tag every snapshot of a streaming run
// shares, e.g. "run:20261128T000000Z-a1b2c3": the run's UTC start time, in
// the scheduledBackupTimestampDate format, and 6 random hex digits, since
// two runs of the same frequency can start within the same second (a Job
// created from the CronJob by hand next to a scheduled one) and would
// otherwise share it. Each snapshot holds a single database, so the tag is
// what lists, or restores, a run's databases together.
const resticRunTagPrefix = "run:"

// streamingRunScript renders the shell line setting $RUN to the run's
// identifier, for its snapshots' run tag (see resticRunTagPrefix and
// streamingBackupCommand).
func streamingRunScript() string {
	return fmt.Sprintf("RUN=$(date -u %s)-$(od -An -N3 -tx1 /dev/urandom | tr -d ' \\n')\n", scheduledBackupTimestampDate)
}

// streamingBackupCommand renders the restic call streaming gbakArgs' backup
// into a snapshot of the single file file, tagged tag, with the run tag
// (see resticRunTagPrefix, expecting $RUN to be set) and the Firebird
// version tag of version (see firebirdVersionTag).
func streamingBackupCommand(instance *kubebirdv1.Instance, s3 *kubebirdv1.S3BackupSpec, tag, version, file, gbakArgs string) string {
	return fmt.Sprintf("%s backup --retry-lock %s --host %q --tag %q --tag %q --tag %q --stdin-filename %q --stdin-from-command -- %s %s\n",
		resticCommandFor(streamingResticBin, s3), resticLockWait, instance.Name, tag, resticRunTagPrefix+"$RUN",
		firebirdVersionTag(instance, version), file, binGbak, gbakArgs)
}

// streamingScheduledBackupScript renders the script a frequency's Jobs run
// without a local destination, in place of scheduledBackupScript and
// resticUploadScript together: for every database in status.databases,
// the live server's gbak backup (through the Services API, like
// scheduledBackupScript's, but to the client's stdout) is streamed into a
// snapshot of "<frequency dir>/<database>.fbk", tagged with the frequency;
// then that frequency's snapshots older than its retention are forgotten,
// every database's separately (--group-by paths: grouping by tags too
// would make every run, with its own run tag, a group of its own, never
// forgotten). The file needs no
// timestamp: every snapshot is a version of it, and two runs at once just
// make two. Under set -e, a failed backup fails the Job before forgetting
// anything. Expects $SYSDBA_PASSWORD to be set.
func streamingScheduledBackupScript(instance *kubebirdv1.Instance, s3 *kubebirdv1.S3BackupSpec, freq backupFrequency) string {
	restic := resticCommandFor(streamingResticBin, s3)
	conn := backupServiceConnection(instance)
	dir := scheduledBackupDir(freq.name)
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString(streamingRunScript())
	b.WriteString(resticEnsureRepositoryScript(restic))
	for _, name := range instance.Status.Databases {
		gbakArgs := fmt.Sprintf("-backup -se %s %q %s %s %s %s \"$SYSDBA_PASSWORD\"",
			conn, path.Join(primaryDataMountPath, name), streamingGbakStdout, flagUser, sysdbaUsername, flagPassword)
		b.WriteString(streamingBackupCommand(instance, s3, freq.name, instance.Spec.Version, path.Join(dir, backupFileName(name)), gbakArgs))
	}
	fmt.Fprintf(&b, "%s forget --retry-lock %s --tag %q --group-by paths --keep-within %s --prune\n",
		restic, resticLockWait, freq.name, frequencyRetention(instance, freq).resticDuration())
	return b.String()
}

// streamingBaseBackupScript renders the script the delete-time (and major
// upgrade) backup Pod runs without a local destination, in place of
// databaseBackupScript and resticBaseUploadScript together: the same local
// gbak backups, of every database in status.databases and then the
// security database, with the server stopped, each streamed into a "base"
// snapshot of the file databaseBackupScript would have written. Not best
// effort, unlike resticBaseUploadScript: the primary volume is released
// right after, so these snapshots are the only copy left, and any failure
// fails the Pod, which backupDatabasesOffline then retries. version is as
// for databaseBackupScript.
func streamingBaseBackupScript(instance *kubebirdv1.Instance, s3 *kubebirdv1.S3BackupSpec, version string) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString(streamingRunScript())
	b.WriteString(resticEnsureRepositoryScript(resticCommandFor(streamingResticBin, s3)))
	for _, name := range instance.Status.Databases {
		gbakArgs := fmt.Sprintf("-backup %q %s", path.Join(primaryDataMountPath, name), streamingGbakStdout)
		b.WriteString(streamingBackupCommand(instance, s3, resticBaseTag, version, path.Join(instanceBackupDir(), backupFileName(name)), gbakArgs))
	}
	gbakArgs := fmt.Sprintf("-backup %q %s", path.Join(primaryDataMountPath, securityDatabaseFileNameForVersion(version)), streamingGbakStdout)
	b.WriteString(streamingBackupCommand(instance, s3, resticBaseTag, version, securityDatabaseBackupPathForVersion(version), gbakArgs))
	b.WriteString("echo \"Uploaded the backup to the S3 repository\"\n")
	return b.String()
}

// s3RestoredAnnotationKey marks the Firebird pod once finishS3Restore has
// run for it, so a steady-state reconcile doesn't exec into the pod to
// leave s3RestoredMarker again. A pod recreated by the StatefulSet
// controller comes back without it, so finishS3Restore runs once per pod.
const s3RestoredAnnotationKey = "kubebird.github.io/s3-restored"

// reconcileS3Restore runs, only without a local destination, before
// reconcileDatabases creates or restores any database. The backup-restore
// initContainer only restores the "base" snapshots into the emptyDir when
// the primary volume lacks s3RestoredMarker (see resticBootstrapScript).
//
// With no pending database, there's nothing left to restore (the security
// database was checked by security-database-init when the pod started), so
// it leaves the marker through finishS3Restore, once per pod: an Instance
// with no databases doesn't restore again on every start.
//
// With a pending database, it checks the marker: when it's missing, the
// pod's start restored the base snapshots and reconcileDatabases can
// restore from them. When it's there, a database was added after the
// first restore and the base snapshots are gone, while the Firebird
// container has no restic or S3 credentials to fetch them: so it removes
// the marker and deletes the pod, whose next start restores them again,
// then reports that it's waiting. That's a restart for every database
// added later, even one the repository has no backup of.
func (r *InstanceReconciler) reconcileS3Restore(ctx context.Context, instance *kubebirdv1.Instance, pending bool) error {
	if !s3Streaming(instance) {
		return nil
	}
	pod := &corev1.Pod{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: firebirdPodName(instance)}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return waiting("Waiting for the Firebird pod to start")
		}
		return fmt.Errorf("failed to get Firebird pod: %w", err)
	}
	if pod.DeletionTimestamp != nil || !podReady(pod) {
		return waiting("Waiting for the Firebird pod to be ready")
	}

	if !pending {
		if pod.Annotations[s3RestoredAnnotationKey] != "" {
			return nil
		}
		return r.finishS3Restore(ctx, instance, pod)
	}

	restored, err := r.databaseFileExists(ctx, instance.Namespace, pod.Name, s3RestoredMarker)
	if err != nil {
		return fmt.Errorf("failed to check for %s: %w", s3RestoredMarker, err)
	}
	if !restored {
		return nil
	}
	if err := r.execInPod(ctx, instance.Namespace, pod.Name, []string{"rm", "-f", s3RestoredMarker}); err != nil {
		return fmt.Errorf("failed to remove %s: %w", s3RestoredMarker, err)
	}
	if err := r.Delete(ctx, pod, client.Preconditions{UID: &pod.UID}); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("failed to restart the Firebird pod: %w", err)
	}
	logf.FromContext(ctx).Info("Restarted the Firebird pod to restore new databases from the S3 repository", "pod", pod.Name)
	return waiting("Restarting the Firebird pod to restore new databases from the S3 repository")
}

// podReady reports whether pod's Ready condition is true.
func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// finishS3RestoreOf runs finishS3Restore on the pod named podName.
func (r *InstanceReconciler) finishS3RestoreOf(ctx context.Context, instance *kubebirdv1.Instance, podName string) error {
	pod := &corev1.Pod{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: instance.Namespace, Name: podName}, pod); err != nil {
		return fmt.Errorf("failed to get Firebird pod: %w", err)
	}
	return r.finishS3Restore(ctx, instance, pod)
}

// finishS3Restore runs once every pending database has been restored (or
// created) without a local destination, or right away when none is
// pending: it deletes the "base" backups the backup-restore initContainer
// restored into the emptyDir, which no longer serve any purpose but take
// up the node's disk, leaves s3RestoredMarker on the primary volume, so a
// later start of the pod doesn't restore them again, and annotates the pod
// with s3RestoredAnnotationKey. Until then, a restarted pod restores them
// again, so a database whose restore failed is still restored rather than
// created empty.
func (r *InstanceReconciler) finishS3Restore(ctx context.Context, instance *kubebirdv1.Instance, pod *corev1.Pod) error {
	script := fmt.Sprintf("rm -rf %q && touch %q", instanceBackupDir(), s3RestoredMarker)
	if err := r.execInPod(ctx, instance.Namespace, pod.Name, []string{"sh", "-c", script}); err != nil {
		return fmt.Errorf("failed to clean up the backups restored from the S3 repository: %w", err)
	}
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[s3RestoredAnnotationKey] = "true"
	if err := r.Patch(ctx, pod, patch); err != nil {
		return fmt.Errorf("failed to annotate the Firebird pod as restored: %w", err)
	}
	logf.FromContext(ctx).Info("Finished restoring from the S3 repository", "pod", pod.Name)
	return nil
}
