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
	"os/exec"
	"strconv"
	"strings"
	"testing"

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

// TestBackupFrequencySequenceExpr actually runs each frequency's shell
// arithmetic expression (the same string baked into the CronJob's Job
// script — see scheduledBackupScript) through a real "sh", for a range
// of retention counts, and checks the result always lands in
// 1..retention. This is the one piece of the scheduling logic that lives
// entirely in a rendered shell snippet rather than Go, so it's only
// meaningfully testable by actually executing it.
func TestBackupFrequencySequenceExpr(t *testing.T) {
	for _, freq := range backupFrequencies {
		for _, retention := range []int32{1, 2, 3, 5} {
			t.Run(freq.name+"/retention="+strconv.Itoa(int(retention)), func(t *testing.T) {
				script := "echo " + freq.sequenceExpr(retention)
				out, err := exec.Command("sh", "-c", script).Output()
				if err != nil {
					t.Fatalf("sh -c %q failed: %v", script, err)
				}
				got, err := strconv.Atoi(strings.TrimSpace(string(out)))
				if err != nil {
					t.Fatalf("output %q isn't an integer: %v", out, err)
				}
				if got < 1 || got > int(retention) {
					t.Errorf("sequenceExpr(%d) = %d, want a value in 1..%d", retention, got, retention)
				}
			})
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
	// retention drops to 0 (or its backup volume disappears).
	scheme := newTestScheme(t)
	instance := &kubebirdv1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: testScheduleInstanceName, Namespace: "default"},
		Spec: kubebirdv1.InstanceSpec{
			Backup: kubebirdv1.BackupSpec{
				Enabled: true,
				Destinations: []kubebirdv1.BackupDestinationSpec{
					{Local: &kubebirdv1.LocalBackupSpec{Storage: kubebirdv1.StorageVolumeSpec{Size: apiresource.MustParse("1Gi")}}},
				},
				// Every RetentionSpec field left at 0 (disabled).
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
		t.Errorf("hourly CronJob still exists after its retention dropped to 0 (err = %v), want NotFound", err)
	}
}

func TestScheduledBackupScript(t *testing.T) {
	instance := &kubebirdv1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: testScheduleInstanceName},
		Spec: kubebirdv1.InstanceSpec{
			Image:   "firebirdsql/firebird",
			Version: "3.0.14",
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

	script := scheduledBackupScript(instance, hour, 2)

	for _, want := range []string{
		"SEQ=",
		"fbsvcmgr",
		"test/3050:service_mgr",
		"-action_nbak",
		"-nbk_level 0",
		`-dbname "/var/lib/firebird/data/instance.fdb"`,
		`-nbk_file "/var/lib/firebird/backup/hour/instance-$SEQ.nbk"`,
		"$SYSDBA_PASSWORD",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("scheduledBackupScript output missing %q; got:\n%s", want, script)
		}
	}
	if strings.Contains(script, "gzip") {
		t.Errorf("scheduledBackupScript should not compress its own output; got:\n%s", script)
	}
	if strings.Contains(script, "security3.fdb") {
		t.Errorf("scheduledBackupScript should not attempt to back up the security database, since it can't be nbackup'd while the server has it open; got:\n%s", script)
	}
}
