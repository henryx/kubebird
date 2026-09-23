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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kubebirdv1 "github.com/henryx/kubebird/api/v1"
)

// binFbsvcmgr is the Firebird Services API client used by
// spec.backup.retention's scheduled backups, in place of gbak: its
// -action_nbak invokes the same physically consistent, guarded backup as
// the nbackup CLI (briefly locking the database rather than requiring
// exclusive access), but performed by the instance's own already-running
// server through the Services API — nbackup itself only operates on a
// database local to wherever it runs, so it can't be pointed at a
// database a separate live server process already has open the way
// -action_nbak can. See scheduledBackupScript and
// https://www.firebirdsql.org/file/documentation/html/en/firebirddocs/nbackup/firebird-nbackup.html
const binFbsvcmgr = "fbsvcmgr"

// scheduledBackupCompressionPollInterval is how often reconcileInstance
// checks every enabled spec.backup.retention frequency's own backup
// subdirectory for a ".nbk" file fbsvcmgr has finished writing but
// nothing has gzip-compressed yet. Nothing else would otherwise trigger
// this promptly: the backup itself now runs on a Kubernetes-managed
// CronJob schedule, entirely independent of Kubebird's own Reconcile
// loop — see reconcileScheduledBackups and compressScheduledBackups.
const scheduledBackupCompressionPollInterval = 5 * time.Minute

// Names of every spec.backup.retention field, matching backupFrequency's
// own "name" values and the RetentionSpec field they read from.
const (
	backupFrequencyHour  = "hour"
	backupFrequencyDay   = "day"
	backupFrequencyWeek  = "week"
	backupFrequencyMonth = "month"
	backupFrequencyYear  = "year"
)

// backupFrequency describes one spec.backup.retention field: how to read
// its configured count and the standard cron schedule (interpreted in
// UTC) a CronJob for it runs on. Its rotation sequence number is computed
// generically by scheduledBackupScript, from what's already in that
// frequency's own backup directory rather than anything specific to the
// frequency itself — see there.
type backupFrequency struct {
	name      string
	schedule  string
	retention func(kubebirdv1.RetentionSpec) int32
}

// backupFrequencies lists every spec.backup.retention field, matching
// the schedule documented on RetentionSpec: hourly on the hour, daily at
// 00:00 UTC, weekly at 00:00 UTC on Sunday, monthly at 00:00 UTC on the
// 1st, yearly at 00:00 UTC on January 1st. Kubernetes interprets a
// CronJob's schedule in UTC unless a CRON_TZ prefix or spec.timeZone
// overrides it, so no explicit timezone handling is needed for the
// schedule string itself.
var backupFrequencies = []backupFrequency{
	{
		name:      backupFrequencyHour,
		schedule:  "0 * * * *",
		retention: func(r kubebirdv1.RetentionSpec) int32 { return r.Hour },
	},
	{
		name:      backupFrequencyDay,
		schedule:  "0 0 * * *",
		retention: func(r kubebirdv1.RetentionSpec) int32 { return r.Day },
	},
	{
		name:      backupFrequencyWeek,
		schedule:  "0 0 * * 0",
		retention: func(r kubebirdv1.RetentionSpec) int32 { return r.Week },
	},
	{
		name:      backupFrequencyMonth,
		schedule:  "0 0 1 * *",
		retention: func(r kubebirdv1.RetentionSpec) int32 { return r.Month },
	},
	{
		name:      backupFrequencyYear,
		schedule:  "0 0 1 1 *",
		retention: func(r kubebirdv1.RetentionSpec) int32 { return r.Year },
	},
}

// firebirdPodName returns the name of the single Pod the instance's
// StatefulSet manages, e.g. "test-0" — the target of this file's own
// exec calls (preparing a frequency's backup directory, compressing its
// output), the same convention reconcileDatabases uses to exec isql into
// it.
func firebirdPodName(instance *kubebirdv1.Instance) string {
	return instance.Name + "-0"
}

// anyRetentionEnabled reports whether at least one spec.backup.retention
// field is non-zero, regardless of whether a backup volume actually
// exists to make that effective — used only to decide whether
// compressScheduledBackups is worth polling for at all.
func anyRetentionEnabled(instance *kubebirdv1.Instance) bool {
	for _, freq := range backupFrequencies {
		if freq.retention(instance.Spec.Backup.Retention) > 0 {
			return true
		}
	}
	return false
}

// scheduledBackupCronJobName returns the name of the CronJob that runs a
// single spec.backup.retention frequency's scheduled backups, e.g.
// "test-backup-hour".
func scheduledBackupCronJobName(instance *kubebirdv1.Instance, frequency string) string {
	return fmt.Sprintf("%s-backup-%s", instance.Name, frequency)
}

// reconcileScheduledBackups ensures a CronJob exists for every
// spec.backup.retention frequency with a non-zero count, and that none
// exists for a frequency left at 0 (or when no local backup volume is
// configured at all — retention has no effect without one, matching
// backupOnDelete) — reconciling both directions so turning a frequency
// back off removes its stale schedule instead of leaving it to keep
// firing. Unlike backupOnDelete, this is independent of deletion
// entirely: once created, each CronJob runs for as long as the Instance
// exists, on its own Kubernetes-managed schedule — nothing here polls or
// drives individual runs the way the deletion flow's backupDatabases
// does for its own helper Pod.
func (r *InstanceReconciler) reconcileScheduledBackups(ctx context.Context, instance *kubebirdv1.Instance) error {
	hasVolume := backupVolumeSpec(instance) != nil

	for _, freq := range backupFrequencies {
		retention := freq.retention(instance.Spec.Backup.Retention)
		name := scheduledBackupCronJobName(instance, freq.name)

		if !hasVolume || retention <= 0 {
			cronJob := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: instance.Namespace}}
			if err := r.Delete(ctx, cronJob); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("failed to delete backup CronJob %q: %w", name, err)
			}
			continue
		}

		if err := r.reconcileScheduledBackupCronJob(ctx, instance, freq, retention); err != nil {
			return fmt.Errorf("failed to reconcile %s backup CronJob: %w", freq.name, err)
		}
	}
	return nil
}

// reconcileScheduledBackupCronJob creates or updates the CronJob for a
// single due-to-be-enabled frequency. Before the very first time it's
// created for this Instance, it prepares that frequency's backup
// directory on the instance's own live pod (see scheduledBackupDir) —
// -action_nbak writes there directly and expects it to already exist
// (see scheduledBackupScript) — since retention can be turned on for an
// already-Ready Instance at any time, long after security-database-init
// last ran, so nothing else guarantees it's there yet.
func (r *InstanceReconciler) reconcileScheduledBackupCronJob(ctx context.Context, instance *kubebirdv1.Instance, freq backupFrequency, retention int32) error {
	name := scheduledBackupCronJobName(instance, freq.name)
	nsName := types.NamespacedName{Name: name, Namespace: instance.Namespace}

	exists := true
	if err := r.Get(ctx, nsName, &batchv1.CronJob{}); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to get backup CronJob %q: %w", name, err)
		}
		exists = false
	}

	if !exists {
		dir := scheduledBackupDir(freq.name)
		if err := r.execInPod(ctx, instance.Namespace, firebirdPodName(instance), []string{"mkdir", "-p", dir}, ""); err != nil {
			return fmt.Errorf("failed to create backup directory %q: %w", dir, err)
		}
	}

	cronJob := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: instance.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cronJob, func() error {
		return r.mutateScheduledBackupCronJob(cronJob, instance, freq, retention)
	}); err != nil {
		return fmt.Errorf("failed to reconcile backup CronJob %q: %w", name, err)
	}
	if !exists {
		logf.FromContext(ctx).Info("Created scheduled backup CronJob", "frequency", freq.name, "schedule", freq.schedule)
	}
	return nil
}

// mutateScheduledBackupCronJob applies the desired spec to a frequency's
// backup CronJob. ConcurrencyPolicy Forbid skips a run entirely rather
// than queueing or overlapping it with one still in flight — a skipped
// run just means that period's backup waits for the next one, since
// scheduledBackupScript computes each run's rotation slot from what's
// already in the backup directory rather than from the previous run's
// own state. BackoffLimit 0 means a
// failed run isn't retried by the Job controller either, for the same
// reason: a failure this period should just wait for the next scheduled
// tick rather than the Job controller immediately re-running (and, with
// the default backoff limit of 6, re-failing) the same script.
func (r *InstanceReconciler) mutateScheduledBackupCronJob(cronJob *batchv1.CronJob, instance *kubebirdv1.Instance, freq backupFrequency, retention int32) error {
	successfulHistory := int32(1)
	failedHistory := int32(3)
	backoffLimit := int32(0)

	cronJob.Labels = labelsForInstance(instance.Name)
	cronJob.Spec = batchv1.CronJobSpec{
		Schedule:                   freq.schedule,
		ConcurrencyPolicy:          batchv1.ForbidConcurrent,
		SuccessfulJobsHistoryLimit: &successfulHistory,
		FailedJobsHistoryLimit:     &failedHistory,
		JobTemplate: batchv1.JobTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labelsForInstance(instance.Name)},
			Spec: batchv1.JobSpec{
				BackoffLimit: &backoffLimit,
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labelsForInstance(instance.Name)},
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyNever,
						Containers: []corev1.Container{
							{
								Name:  containerName,
								Image: backupImage(instance),
								SecurityContext: &corev1.SecurityContext{
									AllowPrivilegeEscalation: ptr.To(false),
									SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
								},
								Command: []string{"sh", "-c", scheduledBackupScript(instance, freq, retention)},
								Env: []corev1.EnvVar{
									{
										Name: "SYSDBA_PASSWORD",
										ValueFrom: &corev1.EnvVarSource{
											SecretKeyRef: &corev1.SecretKeySelector{
												LocalObjectReference: corev1.LocalObjectReference{Name: sysdbaSecretRefName(instance)},
												Key:                  sysdbaSecretPasswordKey,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	return controllerutil.SetControllerReference(instance, cronJob, r.Scheme)
}

// backupServiceConnection returns the Services API connection string
// fbsvcmgr uses to reach the instance's own already-running server over
// the network, through its Service, e.g. "test/3050:service_mgr" —
// resolved by short name, since the backup CronJob's Job runs in the
// same namespace as the Service.
func backupServiceConnection(instance *kubebirdv1.Instance) string {
	port := instance.Spec.Service.Port
	if port == 0 {
		port = firebirdPort
	}
	return fmt.Sprintf("%s/%d:service_mgr", instance.Name, port)
}

// scheduledBackupDir returns the backup volume's subdirectory for a
// given spec.backup.retention frequency's scheduled backups, e.g.
// "/var/lib/firebird/backup/hour" — a sibling of instanceBackupDir's
// fixed "base" subdirectory used by the unrelated delete-time gbak
// backup, so the two never collide. Since this path is also where the
// live firebird container has the backup volume mounted (mutateStatefulSet,
// whenever backupVolumeSpec is non-nil), it's what -action_nbak's own
// -nbk_file argument resolves against too — see scheduledBackupScript.
func scheduledBackupDir(frequency string) string {
	return path.Join(backupDataMountPath, frequency)
}

// sequenceRotationScript renders the two shell lines that compute a
// scheduled backup run's rotation sequence number into $SEQ, purely from
// what's already in dir — factored out of scheduledBackupScript so it
// can be exercised directly against a real temporary directory in
// tests, without dir needing to be the actual (unwritable-in-tests)
// backup mount path. See scheduledBackupScript's own doc for the full
// explanation of the rotation itself.
//
// Deliberately keys off the most recently *modified* "-<n>.nbk"/
// "-<n>.nbk.gz" file ("ls -1t ... | head -1"), not the highest "<n>"
// present: once every slot 1..retention has been used at least once,
// the highest "<n>" that ever exists is permanently retention itself
// (slots are overwritten in place, never renamed or removed), which
// would otherwise wedge every future run's LAST at retention and so
// SEQ at a constant 1 forever instead of continuing to rotate.
//
// The trailing "|| true" on the LAST= pipeline matters under
// scheduledBackupScript's own "set -e": the final grep (extracting the
// digits) exits 1 when nothing matched - the very first run, with
// nothing in dir yet - and since a "VAR=$(...)" assignment's own exit
// status is that of the command substitution, "set -e" would otherwise
// abort the whole script right here, before it ever reaches fbsvcmgr,
// with no output at all (confirmed against a real cluster: the Job's
// pod exits 1 in under a second with an empty log).
func sequenceRotationScript(dir string, retention int32) string {
	var b strings.Builder
	fmt.Fprintf(&b, "LAST=$(ls -1t %q 2>/dev/null | grep -oE -- '-[0-9]+\\.nbk(\\.gz)?$' | head -1 | grep -oE '[0-9]+' || true)\n", dir)
	fmt.Fprintf(&b, "SEQ=$(( (${LAST:-0} %% %d) + 1 ))\n", retention)
	return b.String()
}

// scheduledBackupScript renders the shell script each of a frequency's
// CronJob-spawned Jobs runs: a full (level 0) backup of every database in
// instance.Status.Databases via fbsvcmgr's -action_nbak against the
// instance's own live server (backupServiceConnection). Per the Services
// API's own conventions for nbak (confirmed against the official nbackup
// documentation's remote examples), both -dbname and -nbk_file are
// resolved by the server itself, not by the Job — so each backup lands
// directly in the backup volume as already mounted into the live firebird
// container, without the Job needing to mount the primary/backup PVCs
// itself, or the pod ever stopping (unlike backupDatabasesOffline's
// gbak-based delete-time backup, which can't run while a live server has
// the database open at all). The Job's own container therefore needs
// nothing but network access to the instance's Service and the SYSDBA
// credentials.
//
// Deliberately excludes the security database: unlike spec.databases,
// confirmed against the actual image that it can't be nbackup'd while the
// server has it open, neither locally ("Database already opened with
// engine instance, incompatible with current") nor remotely through the
// Services API ("no permission for remote access to database") — the same
// dual restriction backupDatabasesOffline's gbak already works around by
// stopping the pod first (see "Backup-and-release on deletion" in
// CLAUDE.md), which this live, zero-downtime scheduled flow can't do. The
// security database therefore stays covered only by that delete-time
// backup; a set -e failure on every single scheduled run (as attempting it
// here would cause) would be worse than not attempting it at all.
//
// The rotation sequence number, into the shell variable SEQ, is computed
// once per run purely from what's already in the frequency's own backup
// directory, not anything Kubebird tracks itself (see
// sequenceRotationScript): it lists scheduledBackupDir(freq.name) by
// modification time and takes the numeric suffix of whichever
// "<database>-<n>.nbk"/"<database>-<n>.nbk.gz" file was written most
// recently (any database's, since a single run always shares one SEQ
// across every database - see the loop below) as LAST (0 if none exist
// yet, i.e. the very first run). SEQ is then LAST+1, wrapped back into
// 1..retention via "(LAST % retention) + 1" — so the very first run is
// 1, the second is 2, and so on, restarting at 1 once retention
// consecutive runs have happened, regardless of wall-clock time or
// whether every scheduled tick actually ran.
func scheduledBackupScript(instance *kubebirdv1.Instance, freq backupFrequency, retention int32) string {
	conn := backupServiceConnection(instance)
	dir := scheduledBackupDir(freq.name)

	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString(sequenceRotationScript(dir, retention))

	for _, name := range instance.Status.Databases {
		serverDBPath := path.Join(primaryDataMountPath, name)
		dst := fmt.Sprintf("%s/%s-$SEQ.nbk", dir, strings.TrimSuffix(name, ".fdb"))
		// -action_nbak -nbk_level 0 refuses to write into an
		// already-existing file ("Error creating backup file: ... File
		// exists"), so reusing a rotation slot - this same period's own
		// retry, or a manually triggered "kubectl create job
		// --from=cronjob" landing in the same slot as an earlier
		// successful run - must clear the old file first to actually
		// overwrite it, matching this rotation's own "just overwrites
		// that file" design.
		fmt.Fprintf(&b, "rm -f %q\n", dst)
		fmt.Fprintf(&b, "%s %s %s %s %s \"$SYSDBA_PASSWORD\" -action_nbak -nbk_level 0 -dbname %q -nbk_file %q\n",
			binFbsvcmgr, conn, flagUser, sysdbaUsername, flagPassword, serverDBPath, dst)
	}

	return b.String()
}

// compressScheduledBackups gzips every ".nbk" file found directly under
// each enabled spec.backup.retention frequency's own backup subdirectory
// (scheduledBackupDir) — fbsvcmgr has no compression option of its own
// (see scheduledBackupScript), and the file lands inside the live pod's
// own filesystem, so this execs gzip there directly, the same mechanism
// reconcileDatabases already uses for isql, rather than from the
// (otherwise network-only, unmounted) backup CronJob's own Job. Matching
// by suffix, rather than a specific expected file name, means it doesn't
// need to know which rotation sequence a given run used — that's
// computed independently by the script itself, from what's already in
// the directory (see scheduledBackupScript) — and it's naturally
// idempotent: an already-compressed file is simply not matched again by
// "*.nbk".
func (r *InstanceReconciler) compressScheduledBackups(ctx context.Context, instance *kubebirdv1.Instance) error {
	podName := firebirdPodName(instance)

	for _, freq := range backupFrequencies {
		if freq.retention(instance.Spec.Backup.Retention) <= 0 {
			continue
		}

		dir := scheduledBackupDir(freq.name)
		// Redirects stderr to /dev/null and always exits 0: the
		// directory might not exist yet (e.g. a CronJob was just
		// created and hasn't had its first run yet), in which case
		// there's simply nothing to compress.
		output, err := r.execInPodOutput(ctx, instance.Namespace, podName,
			[]string{"sh", "-c", fmt.Sprintf("ls -1 %s 2>/dev/null || true", dir)})
		if err != nil {
			return fmt.Errorf("failed to list backup directory %q: %w", dir, err)
		}

		for name := range strings.FieldsSeq(output) {
			if !strings.HasSuffix(name, ".nbk") {
				continue
			}
			file := path.Join(dir, name)
			if err := r.execInPod(ctx, instance.Namespace, podName, []string{"gzip", "-f", file}, ""); err != nil {
				return fmt.Errorf("failed to compress backup %q: %w", file, err)
			}
			logf.FromContext(ctx).Info("Compressed scheduled backup", "frequency", freq.name, "file", file)
		}
	}
	return nil
}
