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
	"k8s.io/utils/ptr"
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
// away along with the primary volume, so the next start restores again.
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

// streamingBackupCommand renders the restic call streaming gbakArgs' backup
// into a snapshot of the single file file, tagged tag.
func streamingBackupCommand(instance *kubebirdv1.Instance, s3 *kubebirdv1.S3BackupSpec, tag, file, gbakArgs string) string {
	return fmt.Sprintf("%s backup --retry-lock %s --host %q --tag %q --stdin-filename %q --stdin-from-command -- %s %s\n",
		resticCommandFor(streamingResticBin, s3), resticLockWait, instance.Name, tag, file, binGbak, gbakArgs)
}

// streamingScheduledBackupScript renders the script a frequency's Jobs run
// without a local destination, in place of scheduledBackupScript and
// resticUploadScript together: for every database in status.databases,
// the live server's gbak backup (through the Services API, like
// scheduledBackupScript's, but to the client's stdout) is streamed into a
// snapshot of "<frequency dir>/<database>.fbk", tagged with the frequency;
// then that frequency's snapshots older than its retention are forgotten,
// every database's separately (--group-by paths,tags). The file needs no
// timestamp: every snapshot is a version of it, and two runs at once just
// make two. Under set -e, a failed backup fails the Job before forgetting
// anything. Expects $SYSDBA_PASSWORD to be set.
func streamingScheduledBackupScript(instance *kubebirdv1.Instance, s3 *kubebirdv1.S3BackupSpec, freq backupFrequency) string {
	restic := resticCommandFor(streamingResticBin, s3)
	conn := backupServiceConnection(instance)
	dir := scheduledBackupDir(freq.name)
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString(resticEnsureRepositoryScript(restic))
	for _, name := range instance.Status.Databases {
		gbakArgs := fmt.Sprintf("-backup -se %s %q %s %s %s %s \"$SYSDBA_PASSWORD\"",
			conn, path.Join(primaryDataMountPath, name), streamingGbakStdout, flagUser, sysdbaUsername, flagPassword)
		b.WriteString(streamingBackupCommand(instance, s3, freq.name, path.Join(dir, backupFileName(name)), gbakArgs))
	}
	fmt.Fprintf(&b, "%s forget --retry-lock %s --tag %q --group-by paths,tags --keep-within %s --prune\n",
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
	b.WriteString(resticEnsureRepositoryScript(resticCommandFor(streamingResticBin, s3)))
	for _, name := range instance.Status.Databases {
		gbakArgs := fmt.Sprintf("-backup %q %s", path.Join(primaryDataMountPath, name), streamingGbakStdout)
		b.WriteString(streamingBackupCommand(instance, s3, resticBaseTag, path.Join(instanceBackupDir(), backupFileName(name)), gbakArgs))
	}
	gbakArgs := fmt.Sprintf("-backup %q %s", path.Join(primaryDataMountPath, securityDatabaseFileNameForVersion(version)), streamingGbakStdout)
	b.WriteString(streamingBackupCommand(instance, s3, resticBaseTag, securityDatabaseBackupPathForVersion(version), gbakArgs))
	b.WriteString("echo \"Uploaded the backup to the S3 repository\"\n")
	return b.String()
}

// finishS3Restore runs once reconcileDatabases has restored (or created)
// every pending database without a local destination: it deletes the
// "base" backups the backup-restore initContainer restored into the
// emptyDir, which no longer serve any purpose but take up the node's
// disk, and leaves s3RestoredMarker on the primary volume, so a later
// start of the pod doesn't restore them again. Until then, a restarted
// pod restores them again, so a database whose restore failed is still
// restored rather than created empty. A database added to spec.databases
// later on is then created empty, even if the repository has a backup of
// it.
func (r *InstanceReconciler) finishS3Restore(ctx context.Context, instance *kubebirdv1.Instance, podName string) error {
	script := fmt.Sprintf("rm -rf %q && touch %q", instanceBackupDir(), s3RestoredMarker)
	if err := r.execInPod(ctx, instance.Namespace, podName, []string{"sh", "-c", script}, ""); err != nil {
		return fmt.Errorf("failed to clean up the backups restored from the S3 repository: %w", err)
	}
	logf.FromContext(ctx).Info("Finished restoring from the S3 repository")
	return nil
}
