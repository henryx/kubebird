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
// following the scheduled backup naming are left alone. Fixture
// timestamps are relative to the real clock, since the script's own
// cutoff is.
func TestPruneScheduledBackupsScript(t *testing.T) {
	retention, err := parseRetention("3d")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	now := time.Now().UTC()
	stamp := func(ago time.Duration) string { return now.Add(-ago).Format("20060102T150405Z") }

	expired := []string{
		"instance-" + stamp(4*24*time.Hour) + ".nbk.gz",
		"instance-" + stamp(3*24*time.Hour+time.Hour) + ".nbk",
	}
	kept := []string{
		"instance-" + stamp(3*24*time.Hour-time.Hour) + ".nbk.gz",
		"instance-" + stamp(time.Hour) + ".nbk",
		"instance-1.nbk.gz", // not timestamp-named: ignored
		"notes.txt",         // unrelated: ignored
	}
	for _, name := range append(slices.Clone(expired), kept...) {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatalf("failed to create fixture file: %v", err)
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
	want := slices.Clone(kept)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("remaining files = %v, want %v (expired: %v)", got, want, expired)
	}
}

// TestPruneScheduledBackupsScriptEmptyDir confirms pruning an empty
// directory succeeds under "set -e" — the very first run for a frequency,
// where both globs match nothing and stay literal.
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
			Image:   "firebirdsql/firebird",
			Version: "3.0.14",
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
		"fbsvcmgr",
		"test/3050:service_mgr",
		"-action_nbak",
		"-nbk_level 0",
		`-dbname "/var/lib/firebird/data/instance.fdb"`,
		`-nbk_file "/var/lib/firebird/backup/hour/instance-$TS.nbk"`,
		"$SYSDBA_PASSWORD",
		`CUTOFF=$(date -u -d "3 hours ago" +%Y%m%dT%H%M%SZ)`,
		`for f in "/var/lib/firebird/backup/hour"/*.nbk "/var/lib/firebird/backup/hour"/*.nbk.gz; do`,
		`rm -f "$f"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("scheduledBackupScript output missing %q; got:\n%s", want, script)
		}
	}
	if strings.Index(script, "CUTOFF=") < strings.LastIndex(script, "fbsvcmgr") {
		t.Errorf("scheduledBackupScript must prune only after every backup succeeded; got:\n%s", script)
	}
	if strings.Contains(script, "gzip") {
		t.Errorf("scheduledBackupScript should not compress its output when spec.backup.compress is false; got:\n%s", script)
	}
	if strings.Contains(script, "security3.fdb") {
		t.Errorf("scheduledBackupScript should not attempt to back up the security database, since it can't be nbackup'd while the server has it open; got:\n%s", script)
	}
}

func TestScheduledBackupScriptCompress(t *testing.T) {
	instance := &kubebirdv1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: testScheduleInstanceName},
		Spec: kubebirdv1.InstanceSpec{
			Image:   "firebirdsql/firebird",
			Version: "3.0.14",
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
		dst := fmt.Sprintf("/var/lib/firebird/backup/hour/%s-$TS.nbk", db)
		backup := strings.Index(script, fmt.Sprintf("-nbk_file %q", dst))
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
