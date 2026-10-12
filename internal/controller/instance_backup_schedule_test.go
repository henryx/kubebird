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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kubebirdv1 "github.com/henryx/kubebird/api/v1"
)

// testScheduleInstanceName names the Instance built by this file's own
// tests that don't otherwise need a distinct name.
const testScheduleInstanceName = "test"

// testOtherDatabaseName names a second database for this file's tests
// that need more than one.
const testOtherDatabaseName = "other.fdb"

// newTestScheme returns a runtime.Scheme with both the built-in
// Kubernetes types (Secret, StatefulSet, CronJob, ...) and the Instance
// CRD registered, for building a fake client in these tests.
func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add client-go types to scheme: %v", err)
	}
	if err := kubebirdv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add kubebird types to scheme: %v", err)
	}
	return scheme
}

// TestParseRetention checks every accepted spec.backup.retention unit,
// the empty (disabled) value, a zero duration (also disabled), and that a
// malformed value is rejected.
func TestParseRetention(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    retentionDuration
		enabled bool
		wantErr bool
	}{
		{value: "", want: retentionDuration{}},
		{value: "0d", want: retentionDuration{amount: 0, unit: 'd'}},
		{value: "12h", want: retentionDuration{amount: 12, unit: 'h'}, enabled: true},
		{value: "3d", want: retentionDuration{amount: 3, unit: 'd'}, enabled: true},
		{value: "4w", want: retentionDuration{amount: 4, unit: 'w'}, enabled: true},
		{value: "6m", want: retentionDuration{amount: 6, unit: 'm'}, enabled: true},
		{value: "2y", want: retentionDuration{amount: 2, unit: 'y'}, enabled: true},
		{value: "3", wantErr: true},
		{value: "d", wantErr: true},
		{value: "3x", wantErr: true},
		{value: "-3d", wantErr: true},
	} {
		got, err := parseRetention(tc.value)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseRetention(%q) = %+v, want error", tc.value, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseRetention(%q) unexpected error: %v", tc.value, err)
			continue
		}
		if got != tc.want || got.enabled() != tc.enabled {
			t.Errorf("parseRetention(%q) = %+v (enabled %v), want %+v (enabled %v)", tc.value, got, got.enabled(), tc.want, tc.enabled)
		}
	}
}

// TestRetentionDateAgo checks every unit renders as the GNU "date -d"
// relative expression pruneScheduledBackupsScript's cutoff relies on.
func TestRetentionDateAgo(t *testing.T) {
	for value, want := range map[string]string{
		"5h": "5 hours ago",
		"3d": "3 days ago",
		"2w": "2 weeks ago",
		"6m": "6 months ago",
		"1y": "1 years ago",
	} {
		d, err := parseRetention(value)
		if err != nil {
			t.Fatalf("parseRetention(%q): %v", value, err)
		}
		if got := d.dateAgo(); got != want {
			t.Errorf("dateAgo(%q) = %q, want %q", value, got, want)
		}
	}
}

// TestPruneScheduledBackupsScript executes pruneScheduledBackupsScript's
// rendered snippet via a real "sh" (prefixed with "set -e", matching
// scheduledBackupScript's own usage) against a real temporary directory:
// with "3d", only backups whose file name timestamp is more than three
// days old are deleted, compressed or not, while recent ones and files not
// following the scheduled backup naming are left alone. ".nbk" backups
// left by Kubebird 0.3.1 and earlier expire the same way, and so do
// timestamp claims left behind by killed runs. Fixture timestamps are
// relative to the real clock, since the script's own cutoff is.
func TestPruneScheduledBackupsScript(t *testing.T) {
	retention, err := parseRetention("3d")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	now := time.Now().UTC()
	stamp := func(ago time.Duration) string { return now.Add(-ago).Format("20060102T150405Z") }

	expired := []string{
		"instance-" + stamp(4*24*time.Hour) + ".fbk.gz",
		"instance-" + stamp(3*24*time.Hour+time.Hour) + ".fbk",
		"instance-" + stamp(5*24*time.Hour) + ".nbk.gz", // Kubebird 0.3.1 and earlier
		"instance-" + stamp(5*24*time.Hour) + ".nbk",
	}
	expiredClaims := []string{scheduledBackupClaimPrefix + stamp(4*24*time.Hour)}
	kept := []string{
		"instance-" + stamp(3*24*time.Hour-time.Hour) + ".fbk.gz",
		"instance-" + stamp(time.Hour) + ".fbk",
		"instance-" + stamp(time.Hour) + ".nbk.gz",
		"instance-1.fbk.gz", // not timestamp-named: ignored
		"notes.txt",         // unrelated: ignored
	}
	keptClaims := []string{scheduledBackupClaimPrefix + stamp(time.Hour)}
	for _, name := range append(slices.Clone(expired), kept...) {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatalf("failed to create fixture file: %v", err)
		}
	}
	for _, name := range append(slices.Clone(expiredClaims), keptClaims...) {
		if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatalf("failed to create fixture claim: %v", err)
		}
	}

	script := "set -e\n" + pruneScheduledBackupsScript(dir, retention)
	if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("sh -c %q failed: %v\n%s", script, err, out)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Name())
	}
	slices.Sort(got)
	want := append(slices.Clone(kept), keptClaims...)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("remaining files = %v, want %v (expired: %v, %v)", got, want, expired, expiredClaims)
	}
}

// TestPruneScheduledBackupsScriptEmptyDir confirms pruning an empty
// directory succeeds under "set -e" — the very first run for a frequency,
// where every glob matches nothing and stays literal.
func TestPruneScheduledBackupsScriptEmptyDir(t *testing.T) {
	retention, err := parseRetention("1h")
	if err != nil {
		t.Fatal(err)
	}
	script := "set -e\n" + pruneScheduledBackupsScript(t.TempDir(), retention)
	if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("sh -c %q failed: %v\n%s", script, err, out)
	}
}

// TestScheduledBackupUniqueTimestampScript executes
// scheduledBackupUniqueTimestampScript's rendered snippet via a real "sh"
// against a real temporary directory already holding backups (compressed
// or not, of different databases) for the current second and the next
// two, plus another run's claim on the third, a mode.incremental pool
// started on the fourth and a level 1 backup in it taken on the fifth: the
// $TS it settles on must
// be none of them, so a run of the same frequency right after, or at the
// same time as, another one never reuses its file names. Its own claim
// must be gone once the script exits.
func TestScheduledBackupUniqueTimestampScript(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	taken := map[string]bool{}
	for i, name := range []string{"instance-%s.fbk", "other-%s.fbk.gz", "instance-%s.fbk.gz"} {
		ts := now.Add(time.Duration(i) * time.Second).Format("20060102T150405Z")
		taken[ts] = true
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf(name, ts)), nil, 0o600); err != nil {
			t.Fatalf("failed to create fixture file: %v", err)
		}
	}
	pool := now.Add(4 * time.Second).Format("20060102T150405Z")
	taken[pool] = true
	if err := os.Mkdir(filepath.Join(dir, scheduledBackupPoolPrefix+pool), 0o700); err != nil {
		t.Fatalf("failed to create fixture pool: %v", err)
	}
	level1 := now.Add(5 * time.Second).Format("20060102T150405Z")
	taken[level1] = true
	if err := os.WriteFile(filepath.Join(dir, scheduledBackupPoolPrefix+pool, "instance-level1-"+level1+".nbk.gz"), nil, 0o600); err != nil {
		t.Fatalf("failed to create fixture file: %v", err)
	}
	claimed := now.Add(3 * time.Second).Format("20060102T150405Z")
	taken[claimed] = true
	if err := os.Mkdir(filepath.Join(dir, scheduledBackupClaimPrefix+claimed), 0o700); err != nil {
		t.Fatalf("failed to create fixture claim: %v", err)
	}

	script := "set -e\n" + scheduledBackupUniqueTimestampScript(dir) + "echo \"$TS\"\n"
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("sh -c %q failed: %v\n%s", script, err, out)
	}
	ts := strings.TrimSpace(string(out))
	if _, err := time.Parse("20060102T150405Z", ts); err != nil {
		t.Fatalf("$TS = %q, want a scheduledBackupTimestampDate timestamp: %v", ts, err)
	}
	if taken[ts] {
		t.Errorf("$TS = %q, which a backup or claim in %s already carries", ts, dir)
	}
	if _, err := os.Stat(filepath.Join(dir, scheduledBackupClaimPrefix+ts)); !os.IsNotExist(err) {
		t.Errorf("claim on %q still there after the script exited (err = %v)", ts, err)
	}
}

// TestScheduledBackupRunsConcurrently starts several runs at once, each
// settling on a unique $TS (scheduledBackupUniqueTimestampScript) and
// creating its "backup" file with it: every run must succeed and end up
// with its own file, even though they all start within the same second —
// the case of a Job created by hand from a CronJob while that CronJob's
// scheduled run is in flight. No claim may be left behind.
func TestScheduledBackupRunsConcurrently(t *testing.T) {
	const runs = 3
	dir := t.TempDir()
	script := "set -e\n" + scheduledBackupUniqueTimestampScript(dir) +
		fmt.Sprintf("touch %q/instance-\"$TS\".fbk\n", dir)

	errs := make(chan error, runs)
	for range runs {
		go func() {
			out, err := exec.Command("sh", "-c", script).CombinedOutput()
			if err != nil {
				err = fmt.Errorf("%w: %s", err, out)
			}
			errs <- err
		}()
	}
	for range runs {
		if err := <-errs; err != nil {
			t.Errorf("run failed: %v", err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != runs {
		t.Errorf("got %d entries %v, want %d backup files (one per run) and no claims", len(entries), entries, runs)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), scheduledBackupClaimPrefix) {
			t.Errorf("claim %q left behind", e.Name())
		}
	}
}

// TestBackupFrequencySchedules checks every backupFrequency's cron
// schedule string against what RetentionSpec documents: hourly on the
// hour, daily at 00:00 UTC, weekly at 00:00 UTC on Sunday, monthly at
// 00:00 UTC on the 1st, yearly at 00:00 UTC on January 1st.
func TestBackupFrequencySchedules(t *testing.T) {
	want := map[string]string{
		backupFrequencyHour: "0 * * * *",
		"day":               "0 0 * * *",
		"week":              "0 0 * * 0",
		"month":             "0 0 1 * *",
		"year":              "0 0 1 1 *",
	}
	got := make(map[string]string, len(backupFrequencies))
	for _, freq := range backupFrequencies {
		got[freq.name] = freq.schedule
	}
	for name, schedule := range want {
		if got[name] != schedule {
			t.Errorf("backupFrequencies[%q].schedule = %q, want %q", name, got[name], schedule)
		}
	}
}

func TestReconcileScheduledBackupsRemovesADisabledFrequencysCronJob(t *testing.T) {
	// Creating a new frequency's CronJob for the first time execs
	// "mkdir -p" into the instance's live pod first (see
	// reconcileScheduledBackupCronJob), which needs a real cluster to
	// exercise — covered instead by instance_controller_test.go's own
	// envtest Context, which has a real (if pod-less) apiserver to exec
	// against. This sticks to the direction that needs no exec at all:
	// removing an already-existing CronJob once its frequency's
	// retention is disabled (or its backup volume disappears).
	scheme := newTestScheme(t)
	instance := &kubebirdv1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: testScheduleInstanceName, Namespace: "default"},
		Spec: kubebirdv1.InstanceSpec{
			Backup: kubebirdv1.BackupSpec{
				Enabled: true,
				Destinations: []kubebirdv1.BackupDestinationSpec{
					{Local: &kubebirdv1.LocalBackupSpec{Storage: kubebirdv1.StorageVolumeSpec{Size: apiresource.MustParse("1Gi")}}},
				},
				// Every RetentionSpec field left empty (disabled).
			},
		},
	}

	existing := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{
		Name:      scheduledBackupCronJobName(instance, backupFrequencyHour),
		Namespace: instance.Namespace,
	}}
	r := &InstanceReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build(),
		Scheme: scheme,
	}

	if err := r.reconcileScheduledBackups(t.Context(), instance); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err := r.Get(t.Context(), client.ObjectKeyFromObject(existing), &batchv1.CronJob{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("hourly CronJob still exists after its retention was disabled (err = %v), want NotFound", err)
	}
}

func TestScheduledBackupScript(t *testing.T) {
	instance := &kubebirdv1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: testScheduleInstanceName},
		Spec: kubebirdv1.InstanceSpec{
			Image:   testImage,
			Version: testVersion,
			Backup: kubebirdv1.BackupSpec{
				Retention: kubebirdv1.RetentionSpec{Hour: "3h"},
			},
		},
		Status: kubebirdv1.InstanceStatus{
			Databases: []string{"instance.fdb"},
		},
	}

	var hour backupFrequency
	for _, freq := range backupFrequencies {
		if freq.name == backupFrequencyHour {
			hour = freq
		}
	}

	script := scheduledBackupScript(instance, hour)

	for _, want := range []string{
		"TS=$(date -u +%Y%m%dT%H%M%SZ)",
		`if mkdir "/var/lib/firebird/backup/hour"/.claim-"$TS" 2>/dev/null; then`,
		`for f in "/var/lib/firebird/backup/hour"/*-"$TS".[fn]bk* "/var/lib/firebird/backup/hour"/pool-"$TS" "/var/lib/firebird/backup/hour"/pool-*/*-"$TS".nbk*; do`,
		`trap 'rmdir "/var/lib/firebird/backup/hour"/.claim-"$TS"' EXIT`,
		"fbsvcmgr",
		"test/3050:service_mgr",
		"-action_backup",
		`-dbname "/var/lib/firebird/data/instance.fdb"`,
		`-bkp_file "/var/lib/firebird/backup/hour/instance-$TS.fbk"`,
		"$SYSDBA_PASSWORD",
		`CUTOFF=$(date -u -d "3 hours ago" +%Y%m%dT%H%M%SZ)`,
		`for f in "/var/lib/firebird/backup/hour"/*.fbk "/var/lib/firebird/backup/hour"/*.fbk.gz "/var/lib/firebird/backup/hour"/*.nbk "/var/lib/firebird/backup/hour"/*.nbk.gz; do`,
		`rm -f "$f"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("scheduledBackupScript output missing %q; got:\n%s", want, script)
		}
	}
	if strings.Contains(script, "flock") || strings.Contains(script, "-action_nbak") {
		t.Errorf("scheduledBackupScript must back up with gbak (-action_backup), which needs no lock between concurrent runs; got:\n%s", script)
	}
	if claim := strings.Index(script, "trap "); claim < 0 || claim > strings.Index(script, "fbsvcmgr") {
		t.Errorf("scheduledBackupScript must claim its timestamp before backing up anything; got:\n%s", script)
	}
	if strings.Index(script, "CUTOFF=") < strings.LastIndex(script, "fbsvcmgr") {
		t.Errorf("scheduledBackupScript must prune only after every backup succeeded; got:\n%s", script)
	}
	if strings.Contains(script, "gzip") {
		t.Errorf("scheduledBackupScript should not compress its output when spec.backup.compress is false; got:\n%s", script)
	}
	if strings.Contains(script, "security3.fdb") {
		t.Errorf("scheduledBackupScript should not attempt to back up the security database, since the Services API refuses it while the server has it open; got:\n%s", script)
	}
}

func TestScheduledBackupScriptCompress(t *testing.T) {
	instance := &kubebirdv1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: testScheduleInstanceName},
		Spec: kubebirdv1.InstanceSpec{
			Image:   testImage,
			Version: testVersion,
			Backup: kubebirdv1.BackupSpec{
				Compress:  true,
				Retention: kubebirdv1.RetentionSpec{Hour: "3h"},
			},
		},
		Status: kubebirdv1.InstanceStatus{
			Databases: []string{"instance.fdb", "other.fdb"},
		},
	}

	var hour backupFrequency
	for _, freq := range backupFrequencies {
		if freq.name == backupFrequencyHour {
			hour = freq
		}
	}

	script := scheduledBackupScript(instance, hour)

	for _, db := range []string{"instance", "other"} {
		dst := fmt.Sprintf("/var/lib/firebird/backup/hour/%s-$TS.fbk", db)
		backup := strings.Index(script, fmt.Sprintf("-bkp_file %q", dst))
		compress := strings.Index(script, fmt.Sprintf("gzip -f %q", dst))
		if backup < 0 || compress < 0 {
			t.Fatalf("scheduledBackupScript output missing backup or gzip of %q; got:\n%s", dst, script)
		}
		if compress < backup {
			t.Errorf("scheduledBackupScript must gzip %q only after fbsvcmgr wrote it; got:\n%s", dst, script)
		}
	}
	if strings.Index(script, "CUTOFF=") < strings.LastIndex(script, "gzip") {
		t.Errorf("scheduledBackupScript must prune only after every backup was compressed; got:\n%s", script)
	}
}

// hourFrequency returns backupFrequencies' hourly entry.
func hourFrequency() backupFrequency {
	for _, freq := range backupFrequencies {
		if freq.name == backupFrequencyHour {
			return freq
		}
	}
	panic("no hourly backupFrequency")
}

func TestScheduledBackupScriptPhysical(t *testing.T) {
	instance := &kubebirdv1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: testScheduleInstanceName},
		Spec: kubebirdv1.InstanceSpec{
			Backup: kubebirdv1.BackupSpec{
				Compress:  true,
				Mode:      kubebirdv1.BackupModeSpec{Type: kubebirdv1.BackupModePhysical},
				Retention: kubebirdv1.RetentionSpec{Hour: "3h"},
			},
		},
		Status: kubebirdv1.InstanceStatus{Databases: []string{testDatabaseName}},
	}

	script := scheduledBackupScript(instance, hourFrequency())

	for _, want := range []string{
		`exec 9>"/var/lib/firebird/backup/.scheduled-backup.lock"`,
		"flock 9",
		`-action_nbak -nbk_level 0 -dbname "/var/lib/firebird/data/instance.fdb" -nbk_file "/var/lib/firebird/backup/hour/instance-$TS.nbk"`,
		`gzip -f "/var/lib/firebird/backup/hour/instance-$TS.nbk"`,
		`CUTOFF=$(date -u -d "3 hours ago" +%Y%m%dT%H%M%SZ)`,
		`rm -rf "$p"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("scheduledBackupScript output missing %q; got:\n%s", want, script)
		}
	}
	if strings.Contains(script, "-action_backup") || strings.Contains(script, "-nbk_level 1") {
		t.Errorf("a non-incremental physical backup must be a level 0 nbackup only; got:\n%s", script)
	}
	if lock, claim := strings.Index(script, "flock 9"), strings.Index(script, "trap "); lock > claim {
		t.Errorf("scheduledBackupScript must take the lock before claiming its timestamp; got:\n%s", script)
	}
}

// writeStub writes an executable shell script named name into dir.
func writeStub(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatalf("failed to write %s stub: %v", name, err)
	}
}

// TestIncrementalBackupScript runs incrementalBackupScript's rendered
// snippet under sh against a real temporary directory, with stub
// fbsvcmgr and isql commands standing in for the server: fbsvcmgr creates
// the -nbk_file it's given and, for a level 0 backup, records it as the
// database's latest level 0 in a per-database "history" file, which isql
// prints back the way the real RDB$BACKUP_HISTORY query does.
func TestIncrementalBackupScript(t *testing.T) {
	bin := t.TempDir()
	history := t.TempDir()
	writeStub(t, bin, "fbsvcmgr", `
level=; db=; file=
while [ $# -gt 0 ]; do
  case "$1" in
    -nbk_level) level=$2; shift ;;
    -dbname) db=$2; shift ;;
    -nbk_file) file=$2; shift ;;
  esac
  shift
done
if [ "$(basename "$db")" = "$FAIL_DB" ]; then exit 1; fi
: > "$file"
if [ "$level" = 0 ]; then echo "$file" > "$HISTORY_DIR/$(basename "$db")"; fi
`)
	writeStub(t, bin, "isql", `
for a; do conn=$a; done
cat >/dev/null
h="$HISTORY_DIR/$(basename "${conn#*:}")"
echo
if [ -e "$h" ]; then echo "   $(cat "$h")   "; fi
`)

	dir := t.TempDir()
	instance := &kubebirdv1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: testScheduleInstanceName},
		Spec: kubebirdv1.InstanceSpec{
			Backup: kubebirdv1.BackupSpec{
				Compress: true,
				Mode:     kubebirdv1.BackupModeSpec{Type: kubebirdv1.BackupModePhysical, Incremental: true},
			},
		},
		Status: kubebirdv1.InstanceStatus{Databases: []string{testDatabaseName}},
	}
	retention := retentionDuration{amount: 3, unit: 'h'}
	now := time.Now().UTC()
	const (
		instanceLevel0 = "instance-level0.nbk.gz"
		otherLevel0    = "other-level0.nbk.gz"
	)

	run := func(ts time.Time, failDB string) error {
		t.Helper()
		script := "set -e\nTS=" + ts.Format("20060102T150405Z") + "\n" + incrementalBackupScript(instance, dir, retention)
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "HISTORY_DIR="+history, "FAIL_DB="+failDB)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		return nil
	}
	pools := func() map[string][]string {
		t.Helper()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string][]string{}
		for _, e := range entries {
			files, err := os.ReadDir(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			got[e.Name()] = []string{}
			for _, f := range files {
				got[e.Name()] = append(got[e.Name()], f.Name())
			}
		}
		return got
	}
	ts := func(offset time.Duration) string { return now.Add(offset).Format("20060102T150405Z") }
	check := func(step string, want map[string][]string) {
		t.Helper()
		got := pools()
		if len(got) != len(want) {
			t.Fatalf("%s: got pools %v, want %v", step, got, want)
		}
		for pool, files := range want {
			if !slices.Equal(got[pool], files) {
				t.Fatalf("%s: got pools %v, want %v", step, got, want)
			}
		}
	}

	// The first run starts a pool with a level 0 backup.
	if err := run(now.Add(-2*time.Hour), ""); err != nil {
		t.Fatal(err)
	}
	pool1 := "pool-" + ts(-2*time.Hour)
	check("first run", map[string][]string{pool1: {instanceLevel0}})

	// Later runs within retention add level 1 backups to it, and a
	// database new to the pool gets its own level 0 there.
	instance.Status.Databases = []string{testDatabaseName, testOtherDatabaseName}
	if err := run(now.Add(-1*time.Hour), ""); err != nil {
		t.Fatal(err)
	}
	check("second run", map[string][]string{pool1: {
		instanceLevel0, "instance-level1-" + ts(-1*time.Hour) + ".nbk.gz", otherLevel0,
	}})

	// A level 0 taken outside the pool (e.g. the database was restored
	// from somewhere else) starts a new pool, and the old one is deleted.
	if err := os.WriteFile(filepath.Join(history, testOtherDatabaseName), []byte("/elsewhere.nbk\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(now.Add(-30*time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	pool2 := "pool-" + ts(-30*time.Minute)
	check("foreign level 0", map[string][]string{pool2: {instanceLevel0, otherLevel0}})

	// Once the pool's level 0 is older than retention, a failing run
	// leaves the old pool alone...
	if err := os.Rename(filepath.Join(dir, pool2), filepath.Join(dir, "pool-"+ts(-4*time.Hour))); err != nil {
		t.Fatal(err)
	}
	pool2 = "pool-" + ts(-4*time.Hour)
	for _, db := range []string{testDatabaseName, testOtherDatabaseName} {
		if err := os.WriteFile(filepath.Join(history, db), []byte(filepath.Join(dir, pool2, strings.TrimSuffix(db, ".fdb")+"-level0.nbk")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := run(now, testOtherDatabaseName); err == nil {
		t.Fatal("run with a failing backup succeeded")
	}
	pool3 := "pool-" + ts(0)
	check("failed new pool", map[string][]string{
		pool2: {instanceLevel0, otherLevel0},
		pool3: {instanceLevel0},
	})

	// ...and the next run completes the new pool, then deletes the old one.
	if err := run(now.Add(time.Second), ""); err != nil {
		t.Fatal(err)
	}
	check("completed new pool", map[string][]string{pool3: {
		instanceLevel0, "instance-level1-" + ts(time.Second) + ".nbk.gz", otherLevel0,
	}})
}

// TestPruneExpiredPoolsScript checks that pools left behind by
// mode.incremental expire under the retention of a non-incremental mode,
// while newer pools and directories not named like a pool are kept.
func TestPruneExpiredPoolsScript(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	expired := scheduledBackupPoolPrefix + now.Add(-5*time.Hour).Format("20060102T150405Z")
	kept := scheduledBackupPoolPrefix + now.Add(-1*time.Hour).Format("20060102T150405Z")
	for _, name := range []string{expired, kept, scheduledBackupPoolPrefix + "foreign"} {
		if err := os.MkdirAll(filepath.Join(dir, name, "sub"), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	script := "set -e\n" + pruneScheduledBackupsScript(dir, retentionDuration{amount: 3, unit: 'h'}) + pruneExpiredPoolsScript(dir)
	if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("sh -c %q failed: %v\n%s", script, err, out)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if want := []string{kept, scheduledBackupPoolPrefix + "foreign"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestIncrementalBackupScriptCleansHistory checks that every level 0
// backup, and only those, prunes the database's backup history on
// Firebird 4.0 and later, and that nothing does on Firebird 3.0 (which
// can't) or before the server's version is known.
func TestIncrementalBackupScriptCleansHistory(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"", false},
		{testVersion, false},
		{upgradedTestVersion, true},
		{"5.0.1", true},
	} {
		instance := &kubebirdv1.Instance{
			ObjectMeta: metav1.ObjectMeta{Name: testScheduleInstanceName},
			Spec: kubebirdv1.InstanceSpec{
				Backup: kubebirdv1.BackupSpec{
					Mode:      kubebirdv1.BackupModeSpec{Type: kubebirdv1.BackupModePhysical, Incremental: true},
					Retention: kubebirdv1.RetentionSpec{Hour: "3h"},
				},
			},
			Status: kubebirdv1.InstanceStatus{
				Databases:       []string{testDatabaseName, testOtherDatabaseName},
				FirebirdVersion: tc.version,
			},
		}

		script := scheduledBackupScript(instance, hourFrequency())

		for line := range strings.SplitSeq(script, "\n") {
			cleans := strings.Contains(line, nbackupCleanHistoryArgs)
			switch {
			case strings.Contains(line, "-nbk_level 0"):
				if cleans != tc.want {
					t.Errorf("version %q: level 0 backup cleans history = %v, want %v: %s", tc.version, cleans, tc.want, line)
				}
			case cleans:
				t.Errorf("version %q: only level 0 backups may clean history, got: %s", tc.version, line)
			}
		}

		instance.Spec.Backup.Mode.Incremental = false
		if script := scheduledBackupScript(instance, hourFrequency()); strings.Contains(script, "nbk_clean_history") {
			t.Errorf("version %q: non-incremental physical backups must leave the history alone; got:\n%s", tc.version, script)
		}
	}
}
