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
	"os"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kubebirdv1 "github.com/henryx/kubebird/api/v1"
)

// TestDatabaseBackupScript runs the delete-time backup Pod's script under
// sh with a stub gbak: every database in status.databases and the
// security database are backed up, but a missing security database (the
// Instance deleted before its Firebird pod ever started) is skipped
// instead of failing the Pod on every retry.
func TestDatabaseBackupScript(t *testing.T) {
	instance := &kubebirdv1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: testScheduleInstanceName, Namespace: resourceNamespace},
		Spec:       kubebirdv1.InstanceSpec{Image: testImage, Version: testVersion},
		Status:     kubebirdv1.InstanceStatus{Databases: []string{streamingTestDatabaseA}},
	}

	r := newResticScriptRun(t)
	writeSecurityDatabase(t, r)
	if _, out, ok := r.run(databaseBackupScript(instance, testVersion)); !ok {
		t.Fatalf("script failed:\n%s", out)
	}
	want := []string{
		"-backup -verify " + r.path(primaryDataMountPath+"/"+streamingTestDatabaseA) + " " + r.path(instanceBackupDir()+"/"+backupFileName(streamingTestDatabaseA)),
		"-backup -verify " + r.path(primaryDataMountPath+"/security3.fdb") + " " + r.path(instanceBackupDir()+"/security3.fbk"),
	}
	if got := gbakCalls(t, r); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("gbak calls =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// A failed backup still fails the Pod.
	failed := newResticScriptRun(t)
	writeSecurityDatabase(t, failed)
	if _, out, ok := failed.run(databaseBackupScript(instance, testVersion), "STUB_GBAK_FAIL=1"); ok {
		t.Errorf("with gbak failing, script succeeded:\n%s", out)
	}

	instance.Status.Databases = nil
	r = newResticScriptRun(t)
	_, out, ok := r.run(databaseBackupScript(instance, testVersion))
	if !ok || !strings.Contains(out, "No security database") {
		t.Fatalf("without a security database, script failed or didn't say so:\n%s", out)
	}
	if got := gbakCalls(t, r); len(got) != 0 {
		t.Errorf("without a security database, gbak calls = %v, want none", got)
	}
}

// gbakCalls returns the arguments of every call r's stub gbak logged.
func gbakCalls(t *testing.T, r *resticScriptRun) []string {
	t.Helper()
	data, err := os.ReadFile(r.gbakLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}
