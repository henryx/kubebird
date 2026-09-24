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
	"strconv"
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

// scheduledBackupTimestampDate is the "date -u" format of the UTC
// timestamp every scheduled backup file name carries
// ("<database>-<timestamp>.nbk[.gz]"). scheduledBackupScript renders both
// the run's own timestamp and its retention cutoff with it: being fixed
// width and most-significant-first, two such timestamps compare
// correctly as plain strings, so pruning needs no date parsing beyond
// GNU date's own "<n> <unit> ago".
const scheduledBackupTimestampDate = "+%Y%m%dT%H%M%SZ"

// scheduledBackupTimestampSedPattern extracts the timestamp from a
// scheduled backup file name (compressed or not), matching
// scheduledBackupTimestampDate's output; a file not following that naming
// yields nothing, so pruning never touches it.
const scheduledBackupTimestampSedPattern = `s/.*-\([0-9]\{8\}T[0-9]\{6\}Z\)\.nbk\(\.gz\)\{0,1\}$/\1/p`

// firebirdNodeTopologyKey schedules each scheduled backup Job's pod onto
// the same node as the instance's own Firebird pod (see
// mutateScheduledBackupCronJob), since the backup PVC it mounts is
// ReadWriteOnce.
const firebirdNodeTopologyKey = "kubernetes.io/hostname"

// backupFrequency describes one spec.backup.retention field: how to read
// its configured retention duration and the standard cron schedule
// (interpreted in UTC) a CronJob for it runs on.
type backupFrequency struct {
	name      string
	schedule  string
	retention func(kubebirdv1.RetentionSpec) string
}

// retentionDuration is a parsed spec.backup.retention value: amount units
// of h (hours), d (days), w (weeks), m (months) or y (years).
type retentionDuration struct {
	amount int
	unit   byte
}

// parseRetention parses a spec.backup.retention value of the form
// "<n><unit>" (see RetentionSpec). An empty value parses to the zero
// retentionDuration, i.e. disabled. The CRD's own validation pattern
// already rejects anything else, so an error here only guards against a
// value that bypassed it.
func parseRetention(value string) (retentionDuration, error) {
	if value == "" {
		return retentionDuration{}, nil
	}
	unit := value[len(value)-1]
	if !strings.ContainsRune("hdwmy", rune(unit)) {
		return retentionDuration{}, fmt.Errorf("invalid retention %q: unit must be one of h, d, w, m, y", value)
	}
	amount, err := strconv.Atoi(value[:len(value)-1])
	if err != nil || amount < 0 {
		return retentionDuration{}, fmt.Errorf("invalid retention %q: expected <n><unit>, e.g. 3d", value)
	}
	return retentionDuration{amount: amount, unit: unit}, nil
}

// enabled reports whether the retention turns its frequency on at all.
func (d retentionDuration) enabled() bool {
	return d.amount > 0
}

// dateAgo renders the retention as a GNU "date -d" relative expression,
// e.g. "3 days ago" — months and years follow the calendar rather than a
// fixed length, the same way date itself computes them.
func (d retentionDuration) dateAgo() string {
	units := map[byte]string{'h': "hours", 'd': "days", 'w': "weeks", 'm': "months", 'y': "years"}
	return fmt.Sprintf("%d %s ago", d.amount, units[d.unit])
}

// frequencyRetention returns freq's parsed retention for instance,
// treating an unparsable value as disabled.
func frequencyRetention(instance *kubebirdv1.Instance, freq backupFrequency) retentionDuration {
	d, err := parseRetention(freq.retention(instance.Spec.Backup.Retention))
	if err != nil {
		return retentionDuration{}
	}
	return d
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
		retention: func(r kubebirdv1.RetentionSpec) string { return r.Hour },
	},
	{
		name:      backupFrequencyDay,
		schedule:  "0 0 * * *",
		retention: func(r kubebirdv1.RetentionSpec) string { return r.Day },
	},
	{
		name:      backupFrequencyWeek,
		schedule:  "0 0 * * 0",
		retention: func(r kubebirdv1.RetentionSpec) string { return r.Week },
	},
	{
		name:      backupFrequencyMonth,
		schedule:  "0 0 1 * *",
		retention: func(r kubebirdv1.RetentionSpec) string { return r.Month },
	},
	{
		name:      backupFrequencyYear,
		schedule:  "0 0 1 1 *",
		retention: func(r kubebirdv1.RetentionSpec) string { return r.Year },
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
// field is enabled, regardless of whether a backup volume actually
// exists to make that effective — used only to decide whether
// compressScheduledBackups is worth polling for at all.
func anyRetentionEnabled(instance *kubebirdv1.Instance) bool {
	for _, freq := range backupFrequencies {
		if frequencyRetention(instance, freq).enabled() {
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
// spec.backup.retention frequency with a non-zero duration, and that none
// exists for a frequency left disabled (or when no local backup volume is
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
		name := scheduledBackupCronJobName(instance, freq.name)

		if !hasVolume || !frequencyRetention(instance, freq).enabled() {
			cronJob := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: instance.Namespace}}
			if err := r.Delete(ctx, cronJob); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("failed to delete backup CronJob %q: %w", name, err)
			}
			continue
		}

		if err := r.reconcileScheduledBackupCronJob(ctx, instance, freq); err != nil {
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
func (r *InstanceReconciler) reconcileScheduledBackupCronJob(ctx context.Context, instance *kubebirdv1.Instance, freq backupFrequency) error {
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
		return r.mutateScheduledBackupCronJob(cronJob, instance, freq)
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
// each run names its files after its own start time rather than
// depending on the previous run's state. BackoffLimit 0 means a
// failed run isn't retried by the Job controller either, for the same
// reason: a failure this period should just wait for the next scheduled
// tick rather than the Job controller immediately re-running (and, with
// the default backoff limit of 6, re-failing) the same script.
func (r *InstanceReconciler) mutateScheduledBackupCronJob(cronJob *batchv1.CronJob, instance *kubebirdv1.Instance, freq backupFrequency) error {
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
						Affinity: &corev1.Affinity{
							PodAffinity: &corev1.PodAffinity{
								RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
									{
										LabelSelector: &metav1.LabelSelector{MatchLabels: firebirdPodSelector(instance.Name)},
										TopologyKey:   firebirdNodeTopologyKey,
									},
								},
							},
						},
						Volumes: []corev1.Volume{
							{
								Name: backupVolumeName,
								VolumeSource: corev1.VolumeSource{
									PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: backupPVCName(instance)},
								},
							},
						},
						Containers: []corev1.Container{
							{
								Name:  containerName,
								Image: backupImage(instance),
								SecurityContext: &corev1.SecurityContext{
									AllowPrivilegeEscalation: ptr.To(false),
									SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
								},
								Command:      []string{"sh", "-c", scheduledBackupScript(instance, freq)},
								VolumeMounts: []corev1.VolumeMount{{Name: backupVolumeName, MountPath: backupDataMountPath}},
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

// scheduledBackupScript renders the shell script each of a frequency's
// CronJob-spawned Jobs runs: a full (level 0) backup of every database in
// instance.Status.Databases via fbsvcmgr's -action_nbak against the
// instance's own live server (backupServiceConnection). Per the Services
// API's own conventions for nbak (confirmed against the official nbackup
// documentation's remote examples), both -dbname and -nbk_file are
// resolved by the server itself, not by the Job — so each backup lands
// directly in the backup volume as already mounted into the live firebird
// container, without the pod ever stopping (unlike backupDatabasesOffline's
// gbak-based delete-time backup, which can't run while a live server has
// the database open at all). Every file name is made unique by the run's
// own UTC start time ($TS), so -action_nbak never finds an
// already-existing file ("Error creating backup file: ... File exists").
//
// Once every backup has succeeded, the script prunes the frequency's own
// backup directory (see pruneScheduledBackupsScript) through the Job's
// own mount of the backup PVC. Since it runs under set -e, a failed
// backup exits before pruning, so a run that couldn't take a new backup
// never deletes the older ones either.
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
func scheduledBackupScript(instance *kubebirdv1.Instance, freq backupFrequency) string {
	conn := backupServiceConnection(instance)
	dir := scheduledBackupDir(freq.name)
	retention := frequencyRetention(instance, freq)

	var b strings.Builder
	b.WriteString("set -e\n")
	fmt.Fprintf(&b, "TS=$(date -u %s)\n", scheduledBackupTimestampDate)

	for _, name := range instance.Status.Databases {
		serverDBPath := path.Join(primaryDataMountPath, name)
		dst := fmt.Sprintf("%s/%s-$TS.nbk", dir, strings.TrimSuffix(name, ".fdb"))
		fmt.Fprintf(&b, "%s %s %s %s %s \"$SYSDBA_PASSWORD\" -action_nbak -nbk_level 0 -dbname %q -nbk_file %q\n",
			binFbsvcmgr, conn, flagUser, sysdbaUsername, flagPassword, serverDBPath, dst)
	}

	b.WriteString(pruneScheduledBackupsScript(dir, retention))
	return b.String()
}

// pruneScheduledBackupsScript renders the shell snippet that deletes every
// scheduled backup in dir whose file name timestamp is older than
// retention, counting back from the time it runs — factored out of
// scheduledBackupScript so it can be exercised directly against a real
// temporary directory in tests. The cutoff comes from GNU date's own
// "<n> <unit> ago" (see retentionDuration.dateAgo), rendered in the same
// scheduledBackupTimestampDate format as the file names, so "expr" can
// compare the two as plain strings. Files not following the
// "<database>-<timestamp>.nbk[.gz]" naming are never deleted.
func pruneScheduledBackupsScript(dir string, retention retentionDuration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CUTOFF=$(date -u -d %q %s)\n", retention.dateAgo(), scheduledBackupTimestampDate)
	fmt.Fprintf(&b, "for f in %q/*.nbk %q/*.nbk.gz; do\n", dir, dir)
	b.WriteString("  [ -e \"$f\" ] || continue\n")
	fmt.Fprintf(&b, "  FTS=$(basename \"$f\" | sed -n '%s')\n", scheduledBackupTimestampSedPattern)
	b.WriteString("  [ -n \"$FTS\" ] || continue\n")
	b.WriteString("  if expr \"$FTS\" \\< \"$CUTOFF\" >/dev/null; then\n")
	b.WriteString("    rm -f \"$f\"\n")
	b.WriteString("    echo \"Deleted expired backup $f\"\n")
	b.WriteString("  fi\n")
	b.WriteString("done\n")
	return b.String()
}

// compressScheduledBackups gzips every ".nbk" file found directly under
// each enabled spec.backup.retention frequency's own backup subdirectory
// (scheduledBackupDir) — fbsvcmgr has no compression option of its own
// (see scheduledBackupScript), and the file lands inside the live pod's
// own filesystem, so this execs gzip there directly, the same mechanism
// reconcileDatabases already uses for isql. Matching by suffix, rather
// than a specific expected file name, means it doesn't need to know
// which timestamp a given run used, and it's naturally idempotent: an
// already-compressed file is simply not matched again by "*.nbk".
// Pruning expired backups isn't done here but by the CronJob's own Job,
// right after it takes a new one (see pruneScheduledBackupsScript).
func (r *InstanceReconciler) compressScheduledBackups(ctx context.Context, instance *kubebirdv1.Instance) error {
	podName := firebirdPodName(instance)

	for _, freq := range backupFrequencies {
		if !frequencyRetention(instance, freq).enabled() {
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
