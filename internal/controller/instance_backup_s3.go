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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kubebirdv1 "github.com/henryx/kubebird/api/v1"
)

// With a local destination too, the local backup volume stays the staging
// area for every backup: the Firebird server (scheduled backups) and gbak
// (delete-time and upgrade backups) only write to a filesystem, and the
// firebirdsql/firebird image ships no S3 client. An s3 destination then
// adds a restic container, running spec.backup.destinations[].s3.image,
// after each of them, that copies the directory just written into a
// restic repository in the bucket. Without a local destination, backups
// are streamed into the repository instead (see instance_backup_s3_stream.go).
// Either way, a restic initContainer in the StatefulSet restores the "base"
// snapshots into the backup volume before security-database-init (see
// resticBootstrapScript).
const (
	// defaultResticImage is the restic image used when the s3 destination
	// doesn't set one.
	defaultResticImage = "restic/restic:0.19.1"

	// binRestic is restic's binary in its image.
	binRestic = "restic"

	// resticPasswordVolumeName, resticPasswordMountPath and
	// resticPasswordFile mount the repository password Secret's "password"
	// key as a file, which every restic command reads with --password-file,
	// so the password never shows up in an environment variable or in a
	// command line.
	resticPasswordVolumeName = "restic-password"
	resticPasswordMountPath  = "/etc/kubebird/restic"
	resticPasswordKey        = "password"

	// s3AccessKeyIDKey and s3SecretAccessKeyKey are the keys of
	// spec.backup.destinations[].s3.credentials.secretRef, exposed under
	// the same names as environment variables, which restic reads.
	s3AccessKeyIDKey     = "AWS_ACCESS_KEY_ID"
	s3SecretAccessKeyKey = "AWS_SECRET_ACCESS_KEY"

	// uploadContainerName names the restic container that copies a
	// backup into the repository, in the scheduled backup Jobs and the
	// delete-time backup Pod.
	uploadContainerName = "upload"

	// backupRestoreInitContainerName names the StatefulSet's restic
	// initContainer that restores the base directory from the repository
	// (see resticBootstrapScript), before security-database-init.
	backupRestoreInitContainerName = "backup-restore"

	// resticBaseTag tags the snapshots of the base directory: the
	// delete-time and upgrade backups (see resticBaseUploadScript).
	// Scheduled backups' snapshots are tagged with their frequency instead.
	resticBaseTag = backupBaseDirName

	// resticLockWait is how long a restic command waits for another one's
	// exclusive repository lock (a forget --prune of another frequency)
	// instead of failing.
	resticLockWait = "1h"

	// resticBaseUploadTimeout bounds resticBaseUploadScript: restic retries
	// an unreachable endpoint, bad credentials or a missing bucket for
	// about 15 minutes per request, and the delete-time upload must not
	// keep the Instance's deletion waiting much longer than that.
	resticBaseUploadTimeout = 30 * time.Minute
)

// resticPasswordFile is where the repository password is mounted.
var resticPasswordFile = path.Join(resticPasswordMountPath, resticPasswordKey)

// s3Destination returns the instance's s3 backup destination, or nil when
// it has none or spec.backup.enabled is false. The CRD only allows a
// single s3 entry.
func s3Destination(instance *kubebirdv1.Instance) *kubebirdv1.S3BackupSpec {
	if !instance.Spec.Backup.Enabled {
		return nil
	}
	for _, d := range instance.Spec.Backup.Destinations {
		if d.S3 != nil {
			return d.S3
		}
	}
	return nil
}

// resticSecretName returns the name of the Secret holding the restic
// repository's password: spec.backup.destinations[].s3.repository.secretRef,
// or "<instance-name>-restic".
func resticSecretName(instance *kubebirdv1.Instance, s3 *kubebirdv1.S3BackupSpec) string {
	if s3.Repository.SecretRef != "" {
		return s3.Repository.SecretRef
	}
	return instance.Name + "-restic"
}

// s3SecretNames returns the Secrets the instance's s3 destination reads,
// so the Secret watch (see SetupWithManager) reconciles the Instance once
// one of them is created or changed.
func s3SecretNames(instance *kubebirdv1.Instance) []string {
	s3 := s3Destination(instance)
	if s3 == nil {
		return nil
	}
	names := []string{resticSecretName(instance, s3)}
	if s3.Credentials.SecretRef != "" {
		names = append(names, s3.Credentials.SecretRef)
	}
	return names
}

// resticImage returns the image of the restic containers.
func resticImage(s3 *kubebirdv1.S3BackupSpec) string {
	if s3.Image != "" {
		return s3.Image
	}
	return defaultResticImage
}

// resticRepository returns the restic repository URL of the s3
// destination, e.g. "s3:s3.amazonaws.com/bucket/namespace/name" or
// "s3:http://silo:9000/bucket/namespace/name"; restic uses HTTPS unless
// the endpoint says http://.
func resticRepository(instance *kubebirdv1.Instance, s3 *kubebirdv1.S3BackupSpec) string {
	endpoint := strings.TrimSuffix(s3.Endpoint, "/")
	if endpoint == "" {
		endpoint = "s3.amazonaws.com"
	}
	prefix := strings.Trim(s3.Prefix, "/")
	if prefix == "" {
		prefix = instance.Namespace + "/" + instance.Name
	}
	return fmt.Sprintf("s3:%s/%s/%s", endpoint, s3.Bucket, prefix)
}

// resticCommand returns the restic command line every script below runs,
// before its subcommand: the repository comes from RESTIC_REPOSITORY (see
// resticEnv), the password from the mounted Secret.
func resticCommand(s3 *kubebirdv1.S3BackupSpec) string {
	return resticCommandFor(binRestic, s3)
}

// resticCommandFor is resticCommand running the restic binary bin, e.g.
// the copy streamingResticBin of the containers streaming backups.
func resticCommandFor(bin string, s3 *kubebirdv1.S3BackupSpec) string {
	cmd := fmt.Sprintf("%s --password-file %s", bin, resticPasswordFile)
	if s3.ForcePathStyle {
		cmd += " -o s3.bucket-lookup=path"
	}
	if s3.Region != "" {
		cmd += " -o s3.region=" + s3.Region
	}
	if s3.InsecureSkipTLSVerify {
		cmd += " --insecure-tls"
	}
	return cmd
}

// resticEnv returns the restic containers' environment: the repository
// and, when credentials.secretRef is set, the S3 credentials from it.
// Without it, restic falls back to the pod's ambient credentials (IAM
// roles for service accounts and the like). optional makes the
// credentials Secret's keys optional, for the delete-time backup Pod,
// whose upload is best effort and so must never keep it from starting.
func resticEnv(instance *kubebirdv1.Instance, s3 *kubebirdv1.S3BackupSpec, optional bool) []corev1.EnvVar {
	env := []corev1.EnvVar{{Name: "RESTIC_REPOSITORY", Value: resticRepository(instance, s3)}}
	if s3.Credentials.SecretRef == "" {
		return env
	}
	for _, key := range []string{s3AccessKeyIDKey, s3SecretAccessKeyKey} {
		env = append(env, corev1.EnvVar{
			Name: key,
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: s3.Credentials.SecretRef},
					Key:                  key,
					Optional:             ptr.To(optional),
				},
			},
		})
	}
	return env
}

// resticPasswordVolume returns the volume mounting the repository password
// Secret (see resticPasswordFile). optional is as for resticEnv.
func resticPasswordVolume(instance *kubebirdv1.Instance, s3 *kubebirdv1.S3BackupSpec, optional bool) corev1.Volume {
	return corev1.Volume{
		Name: resticPasswordVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  resticSecretName(instance, s3),
				Items:       []corev1.KeyToPath{{Key: resticPasswordKey, Path: resticPasswordKey}},
				DefaultMode: ptr.To(int32(0o400)),
				Optional:    ptr.To(optional),
			},
		},
	}
}

// resticContainer returns a restic container running script, with the
// backup volume mounted at backupDataMountPath (read-only unless
// writable) and the repository password at resticPasswordMountPath.
func resticContainer(instance *kubebirdv1.Instance, s3 *kubebirdv1.S3BackupSpec, name, script string, writable, optional bool) corev1.Container {
	return corev1.Container{
		Name:  name,
		Image: resticImage(s3),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Command: []string{"sh", "-c", script},
		Env:     resticEnv(instance, s3, optional),
		VolumeMounts: []corev1.VolumeMount{
			{Name: backupVolumeName, MountPath: backupDataMountPath, ReadOnly: !writable},
			{Name: resticPasswordVolumeName, MountPath: resticPasswordMountPath, ReadOnly: true},
		},
	}
}

// resticEnsureRepositoryScript renders a shell snippet that initializes
// the repository unless it already is: restic init creates the bucket
// too when it's missing, and fails fast, with "already initialized", on
// an existing repository, while "restic cat config" retries a missing
// bucket for about 15 minutes. Two runs initializing at once are fine:
// the loser sees "already initialized" too. restic is the command line
// to run (see resticCommand).
func resticEnsureRepositoryScript(restic string) string {
	return fmt.Sprintf(`if ! OUT=$(%s init 2>&1); then
	case "$OUT" in
	*"already initialized"*|*"already exists"*) ;;
	*) echo "$OUT" >&2; exit 1 ;;
	esac
fi
`, restic)
}

// resticDuration renders the retention as a restic duration for forget's
// --keep-within, which knows y, m, d and h but not weeks.
func (d retentionDuration) resticDuration() string {
	if d.unit == 'w' {
		return fmt.Sprintf("%dd", d.amount*7)
	}
	return fmt.Sprintf("%d%c", d.amount, d.unit)
}

// resticUploadScript renders the scheduled backup Jobs' upload container
// script: a snapshot of the frequency's directory, tagged with the
// frequency (the directory's state after the backup container's own
// pruning, so the latest snapshot always mirrors it), then a forget of
// the frequency's snapshots older than its retention, relative to the
// latest one, pruning the data only they referenced. A file pruned locally
// therefore stays in the repository until the last snapshot holding it is
// forgotten, up to twice the retention: in exchange, a backup volume that
// was lost or emptied doesn't take the repository's copies with it on the
// next run. Any failure fails the Job, so it shows up like a failed
// backup. Each snapshot is also tagged with the Firebird version (see
// firebirdVersionTag). The frequency's snapshots are forgotten as a single
// group (--group-by ”), since the host name is the Instance's and the
// path the frequency's anyway, while grouping by tags would split them at
// every version change and never forget the older version's.
func resticUploadScript(instance *kubebirdv1.Instance, s3 *kubebirdv1.S3BackupSpec, frequency string, retention retentionDuration) string {
	restic := resticCommand(s3)
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString(resticEnsureRepositoryScript(restic))
	fmt.Fprintf(&b, "%s backup --retry-lock %s --host %q --tag %q --tag %q --exclude %q %q\n",
		restic, resticLockWait, instance.Name, frequency, firebirdVersionTag(instance, instance.Spec.Version),
		scheduledBackupClaimPrefix+"*", scheduledBackupDir(frequency))
	fmt.Fprintf(&b, "%s forget --retry-lock %s --tag %q --group-by '' --keep-within %s --prune\n",
		restic, resticLockWait, frequency, retention.resticDuration())
	return b.String()
}

// resticVersionTagPrefix prefixes the tag recording the Firebird version a
// snapshot's backups were taken with, e.g. "firebird:4.0.3".
const resticVersionTagPrefix = "firebird:"

// firebirdVersionTag returns the tag recording the Firebird version of a
// backup taken with version: spec.version, or, during a major upgrade's
// backup, the version upgraded from. For spec.version, it's the version
// the running server reported (status.firebirdVersion) once detected,
// since spec.version is only an image tag (e.g. "latest").
func firebirdVersionTag(instance *kubebirdv1.Instance, version string) string {
	if version == instance.Spec.Version && instance.Status.FirebirdVersion != "" {
		version = instance.Status.FirebirdVersion
	}
	return resticVersionTagPrefix + version
}

// resticBaseUploadScript renders the delete-time backup Pod's upload
// container script: a snapshot of the base directory, tagged
// resticBaseTag, never forgotten, so the security database backups of
// older Firebird versions stay in it too. Best effort: the backup itself
// already sits on the backup volume, which deleting the Instance always
// keeps, so a failed or timed out (resticBaseUploadTimeout) upload is only
// logged, and the Pod still succeeds rather than holding up the deletion
// or a major version upgrade. version is the Firebird version the backup
// was taken with (see firebirdVersionTag).
func resticBaseUploadScript(instance *kubebirdv1.Instance, s3 *kubebirdv1.S3BackupSpec, version string) string {
	upload := "set -e\n" + resticEnsureRepositoryScript(resticCommand(s3)) +
		fmt.Sprintf("%s backup --retry-lock %s --host %q --tag %q --tag %q %q\n",
			resticCommand(s3), resticLockWait, instance.Name, resticBaseTag, firebirdVersionTag(instance, version), instanceBackupDir())
	return fmt.Sprintf(`if timeout %d sh -c %s; then
	echo "Uploaded the backup to the S3 repository"
else
	echo "Warning: failed to upload the backup to the S3 repository; it is still on the backup volume" >&2
fi
exit 0
`, int(resticBaseUploadTimeout.Seconds()), shellQuote(upload))
}

// resticBootstrapScript renders the StatefulSet's backup-restore
// initContainer script, which restores the latest "base" snapshots into
// the backup volume's base directory, so security-database-init and
// restoreDatabaseIfBackedUp find the backups as if they had been there all
// along. With a local destination, it only runs when the base directory
// is empty or missing (a new Instance, or the backup volume was lost),
// and never touches one that already holds something, which is always at
// least as recent as the repository's copy of it. Without one (see
// s3Streaming), the backup volume is a fresh emptyDir on every start, so
// it runs whenever the primary volume hasn't been restored yet, i.e. holds
// no s3RestoredMarker (see finishS3Restore).
//
// The repository can hold two kinds of "base" snapshots: of the whole
// base directory (resticBaseUploadScript) and of a single file in it
// (streamingBaseBackupScript). The latest snapshot of every path is
// restored, oldest first, so a newer one wins wherever they overlap: a
// directory snapshot with restore "<id>:<base>", a file one with dump.
// Any other path, including one escaping base with "..", is skipped.
// Neither restores the parent directories' metadata, which "--target /"
// does, and which fails where it can't be set (e.g. SELinux labels).
// Snapshot paths are absolute, whatever Instance took them (see
// S3BackupSpec.Prefix). Failing here (e.g. the endpoint is unreachable)
// fails the pod's start, rather than starting fresh databases where a
// restore was due: the snapshot list is captured into a variable first,
// so a failed restic command isn't hidden by the pipeline parsing it.
func resticBootstrapScript(instance *kubebirdv1.Instance, s3 *kubebirdv1.S3BackupSpec) string {
	restic := resticCommand(s3)
	base := instanceBackupDir()
	var b strings.Builder
	b.WriteString("set -e\n")
	if s3Streaming(instance) {
		fmt.Fprintf(&b, `if [ -e %q ]; then
	echo "The primary volume was already restored; not restoring from the S3 repository"
	exit 0
fi
`, s3RestoredMarker)
	} else {
		fmt.Fprintf(&b, `if [ -n "$(ls -A %q 2>/dev/null)" ]; then
	echo "The backup volume already holds backups; not restoring from the S3 repository"
	exit 0
fi
`, base)
	}
	b.WriteString(resticEnsureRepositoryScript(restic))
	fmt.Fprintf(&b, `SNAPSHOTS=$(%[1]s snapshots --tag %[2]q --group-by paths --latest 1 --json)
LIST=$(printf '%%s\n' "$SNAPSHOTS" | grep -o '"\(time\|short_id\)":"[^"]*"\|"paths":\["[^"]*"' | awk -F'"' '$2 == "time" { t = $4 } $2 == "paths" { p = $4 } $2 == "short_id" { print t, $4, p }' | sort)
if [ -z "$LIST" ]; then
	echo "The S3 repository holds no backup to restore"
	exit 0
fi
echo "Restoring the backups from the S3 repository"
mkdir -p %[3]q
while read -r TIME ID P; do
	case "$P" in
	*/..|*/../*)
		echo "Skipping snapshot $ID of $P, outside the base directory" >&2
		;;
	%[3]q)
		%[1]s restore "$ID:$P" --target "$P"
		;;
	%[3]q/*)
		%[1]s dump "$ID" "$P" > "$P.tmp"
		mv "$P.tmp" "$P"
		;;
	esac
done <<EOF
$LIST
EOF
`, restic, resticBaseTag, base)
	return b.String()
}

// reconcileS3Secrets ensures the Secrets the s3 destination reads exist:
// the credentials Secret, when set, must already exist; the repository
// password Secret is created with a generated password when unset and
// missing (see reconcileResticSecret).
func (r *InstanceReconciler) reconcileS3Secrets(ctx context.Context, instance *kubebirdv1.Instance) error {
	s3 := s3Destination(instance)
	if s3 == nil {
		return nil
	}
	if name := s3.Credentials.SecretRef; name != "" {
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, &corev1.Secret{}); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("secret %q specified in spec.backup.destinations[].s3.credentials.secretRef not found", name)
			}
			return fmt.Errorf("failed to get S3 credentials Secret %q: %w", name, err)
		}
	}
	return r.reconcileResticSecret(ctx, instance, s3)
}

// reconcileResticSecret ensures the restic repository password Secret
// exists, the same way reconcileUserSecret does a user's: a secretRef must
// name an existing Secret, otherwise "<instance-name>-restic" is created
// with a generated password if missing. Never owner-referenced, so it
// survives deleting the Instance: a recreated Instance needs the same
// password to read the repository.
func (r *InstanceReconciler) reconcileResticSecret(ctx context.Context, instance *kubebirdv1.Instance, s3 *kubebirdv1.S3BackupSpec) error {
	name := resticSecretName(instance, s3)
	existing := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, existing); err == nil {
		if err := r.syncFirebirdVersionLabel(ctx, instance, existing); err != nil {
			return fmt.Errorf("failed to label restic repository Secret %q with the Firebird version: %w", name, err)
		}
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to get restic repository Secret %q: %w", name, err)
	}

	if s3.Repository.SecretRef != "" {
		return fmt.Errorf("secret %q specified in spec.backup.destinations[].s3.repository.secretRef not found", name)
	}

	password, err := generateRandomPassword()
	if err != nil {
		return fmt.Errorf("failed to generate the restic repository password: %w", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    withFirebirdVersionLabel(instance, labelsForInstance(instance.Name)),
		},
		StringData: map[string]string{resticPasswordKey: password},
	}
	if err := r.Create(ctx, secret); err != nil {
		return fmt.Errorf("failed to create restic repository Secret %q: %w", name, err)
	}
	logf.FromContext(ctx).Info("Created restic repository Secret with a generated password", "name", name)
	return nil
}
