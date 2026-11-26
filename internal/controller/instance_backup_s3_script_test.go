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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	kubebirdv1 "github.com/henryx/kubebird/api/v1"
)

// resticStub is a stand-in restic: it appends each call's subcommand and
// arguments (without the global --password-file/-o ones) to $RESTIC_LOG,
// one line per call, and behaves as told by the environment:
//
//   - init: STUB_INIT is "new" (succeeds), "exists" (fails with restic's
//     "already initialized" message) or anything else (fails otherwise);
//   - snapshots: prints STUB_SNAPSHOTS, failing when STUB_SNAPSHOTS_FAIL is
//     set;
//   - restore: creates $STUB_RESTORE_FILE;
//   - backup and forget: fail when STUB_BACKUP_FAIL is set.
const resticStub = `
while [ $# -gt 0 ]; do
  case "$1" in
    --password-file|-o) shift 2 ;;
    *) break ;;
  esac
done
echo "$*" >> "$RESTIC_LOG"
case "$1" in
  init)
    case "$STUB_INIT" in
      new) exit 0 ;;
      exists) echo "Fatal: create key in repository failed: repository master key and config already initialized" >&2; exit 1 ;;
      *) echo "Fatal: unable to open config file: Access Denied" >&2; exit 1 ;;
    esac ;;
  snapshots)
    [ -z "$STUB_SNAPSHOTS_FAIL" ] || exit 1
    echo "$STUB_SNAPSHOTS" ;;
  restore)
    mkdir -p "$(dirname "$STUB_RESTORE_FILE")" && : > "$STUB_RESTORE_FILE" ;;
  backup|forget)
    [ -z "$STUB_BACKUP_FAIL" ] || exit 1 ;;
esac
`

// resticScriptRun runs a rendered restic script under sh with the stub
// restic on PATH and the backup volume moved into a temporary directory,
// returning the calls the stub logged, the script's output and whether it
// succeeded.
type resticScriptRun struct {
	t      *testing.T
	bin    string
	volume string
}

func newResticScriptRun(t *testing.T) *resticScriptRun {
	t.Helper()
	bin := t.TempDir()
	writeStub(t, bin, binRestic, resticStub)
	return &resticScriptRun{t: t, bin: bin, volume: t.TempDir()}
}

// path returns where a path under the backup volume lands in the test.
func (r *resticScriptRun) path(p string) string {
	return strings.Replace(p, backupDataMountPath, r.volume, 1)
}

func (r *resticScriptRun) run(script string, env ...string) (calls []string, out string, ok bool) {
	r.t.Helper()
	log := filepath.Join(r.t.TempDir(), "restic.log")
	cmd := exec.Command("sh", "-c", strings.ReplaceAll(script, backupDataMountPath, r.volume))
	cmd.Env = append(os.Environ(), append([]string{
		"PATH=" + r.bin + ":" + os.Getenv("PATH"),
		"RESTIC_LOG=" + log,
		"STUB_RESTORE_FILE=" + r.path(filepath.Join(instanceBackupDir(), "restored.fbk")),
	}, env...)...)
	output, err := cmd.CombinedOutput()
	data, readErr := os.ReadFile(log)
	if readErr != nil && !os.IsNotExist(readErr) {
		r.t.Fatal(readErr)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n"), string(output), err == nil
}

// subcommands returns the restic subcommand of every call.
func subcommands(calls []string) []string {
	var names []string
	for _, c := range calls {
		if c != "" {
			names = append(names, strings.Fields(c)[0])
		}
	}
	return names
}

func TestResticUploadScript(t *testing.T) {
	s3 := &kubebirdv1.S3BackupSpec{Bucket: testS3Bucket, ForcePathStyle: true}
	instance := s3TestInstance(s3)
	script := resticUploadScript(instance, s3, backupFrequencyHour, retentionDuration{amount: 2, unit: 'w'})

	t.Run("snapshots the frequency, then forgets its expired snapshots", func(t *testing.T) {
		for _, init := range []string{"new", "exists"} {
			r := newResticScriptRun(t)
			calls, out, ok := r.run(script, "STUB_INIT="+init)
			if !ok {
				t.Fatalf("STUB_INIT=%s: script failed:\n%s", init, out)
			}
			if got := subcommands(calls); strings.Join(got, ",") != "init,backup,forget" {
				t.Fatalf("STUB_INIT=%s: calls = %v, want init, backup, forget", init, calls)
			}
			wantBackup := `backup --retry-lock 1h --host test --tag hour --exclude .claim-* ` + r.path(scheduledBackupDir(backupFrequencyHour))
			if calls[1] != wantBackup {
				t.Errorf("backup call = %q, want %q", calls[1], wantBackup)
			}
			if want := "forget --retry-lock 1h --tag hour --group-by tags --keep-within 14d --prune"; calls[2] != want {
				t.Errorf("forget call = %q, want %q", calls[2], want)
			}
		}
	})

	t.Run("fails without touching the repository when it can't be opened", func(t *testing.T) {
		calls, out, ok := newResticScriptRun(t).run(script, "STUB_INIT=denied")
		if ok || !strings.Contains(out, "Access Denied") {
			t.Fatalf("script succeeded or hid restic's error:\n%s", out)
		}
		if got := subcommands(calls); len(got) != 1 {
			t.Errorf("calls = %v, want only init", calls)
		}
	})

	t.Run("doesn't forget anything after a failed backup", func(t *testing.T) {
		calls, _, ok := newResticScriptRun(t).run(script, "STUB_INIT=exists", "STUB_BACKUP_FAIL=1")
		if ok {
			t.Fatal("script succeeded after a failed backup")
		}
		if got := subcommands(calls); strings.Join(got, ",") != "init,backup" {
			t.Errorf("calls = %v, want init, backup", calls)
		}
	})
}

func TestResticBaseUploadScript(t *testing.T) {
	s3 := &kubebirdv1.S3BackupSpec{Bucket: testS3Bucket}
	instance := s3TestInstance(s3)
	script := resticBaseUploadScript(instance, s3)

	r := newResticScriptRun(t)
	calls, out, ok := r.run(script, "STUB_INIT=exists")
	if !ok || !strings.Contains(out, "Uploaded the backup") {
		t.Fatalf("script failed:\n%s", out)
	}
	if want := "backup --retry-lock 1h --host test --tag base " + r.path(instanceBackupDir()); len(calls) != 2 || calls[1] != want {
		t.Errorf("calls = %v, want init then %q", calls, want)
	}

	// Best effort: a failure is only reported.
	for name, env := range map[string]string{"repository": "STUB_INIT=denied", "backup": "STUB_BACKUP_FAIL=1"} {
		_, out, ok := newResticScriptRun(t).run(script, "STUB_INIT=exists", env)
		if !ok || !strings.Contains(out, "Warning: failed to upload") {
			t.Errorf("failed %s: script failed or didn't warn:\n%s", name, out)
		}
	}
}

func TestResticBootstrapScript(t *testing.T) {
	s3 := &kubebirdv1.S3BackupSpec{Bucket: testS3Bucket}
	script := resticBootstrapScript(s3)
	const snapshot = `[{"time":"2026-11-27T00:00:00Z","id":"0123abcd","short_id":"0123abcd"}]`

	t.Run("restores the latest base snapshot into an empty backup volume", func(t *testing.T) {
		r := newResticScriptRun(t)
		calls, out, ok := r.run(script, "STUB_INIT=exists", "STUB_SNAPSHOTS="+snapshot)
		if !ok {
			t.Fatalf("script failed:\n%s", out)
		}
		if want := "restore latest --tag base --target /"; len(calls) != 3 || calls[2] != want {
			t.Fatalf("calls = %v, want init, snapshots, then %q", calls, want)
		}
		if calls[1] != "snapshots --tag base --latest 1 --json" {
			t.Errorf("snapshots call = %q", calls[1])
		}
		if _, err := os.Stat(r.path(filepath.Join(instanceBackupDir(), "restored.fbk"))); err != nil {
			t.Errorf("nothing restored: %v", err)
		}
	})

	t.Run("restores nothing from a repository without base snapshots", func(t *testing.T) {
		calls, out, ok := newResticScriptRun(t).run(script, "STUB_INIT=new", "STUB_SNAPSHOTS=[]")
		if !ok || !strings.Contains(out, "holds no backup to restore") {
			t.Fatalf("script failed:\n%s", out)
		}
		if got := subcommands(calls); strings.Join(got, ",") != "init,snapshots" {
			t.Errorf("calls = %v, want init, snapshots", calls)
		}
	})

	t.Run("leaves a backup volume already holding backups alone", func(t *testing.T) {
		r := newResticScriptRun(t)
		base := r.path(instanceBackupDir())
		if err := os.MkdirAll(base, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, "local.fbk"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		calls, out, ok := r.run(script, "STUB_INIT=exists", "STUB_SNAPSHOTS="+snapshot)
		if !ok || !strings.Contains(out, "already holds backups") {
			t.Fatalf("script failed:\n%s", out)
		}
		if got := subcommands(calls); len(got) != 0 {
			t.Errorf("calls = %v, want none", calls)
		}
	})

	t.Run("fails rather than starting fresh when the repository can't be read", func(t *testing.T) {
		for name, env := range map[string][]string{
			"init":      {"STUB_INIT=denied"},
			"snapshots": {"STUB_INIT=exists", "STUB_SNAPSHOTS_FAIL=1"},
		} {
			calls, out, ok := newResticScriptRun(t).run(script, env...)
			if ok {
				t.Errorf("failed %s: script succeeded:\n%s", name, out)
			}
			for _, c := range subcommands(calls) {
				if c == "restore" {
					t.Errorf("failed %s: restored anyway", name)
				}
			}
		}
	})
}
