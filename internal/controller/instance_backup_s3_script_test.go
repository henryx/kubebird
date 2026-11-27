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
	"path"
	"path/filepath"
	"strings"
	"testing"

	kubebirdv1 "github.com/henryx/kubebird/api/v1"
)

// resticStub is a stand-in restic: it appends each call's subcommand and
// arguments (without the global --password-file/-o/--insecure-tls ones) to
// $RESTIC_LOG, one line per call, and behaves as told by the environment:
//
//   - init: STUB_INIT is "new" (succeeds), "exists" (fails with restic's
//     "already initialized" message) or anything else (fails otherwise);
//   - snapshots: prints STUB_SNAPSHOTS, failing when STUB_SNAPSHOTS_FAIL is
//     set;
//   - restore "<id>:<path>" --target <dir>: writes "<dir>/restored.fbk",
//     holding the snapshot ID;
//   - dump <id> <path>: prints "<id> <path>", failing when STUB_DUMP_FAIL
//     is set;
//   - backup and forget: fail when STUB_BACKUP_FAIL is set; with
//     --stdin-from-command, backup runs the command after "--" and fails
//     when it does, as restic does.
const resticStub = `
while [ $# -gt 0 ]; do
  case "$1" in
    --password-file|-o) shift 2 ;;
    --insecure-tls) shift ;;
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
    mkdir -p "$4" && echo "${2%%:*}" > "$4/restored.fbk" ;;
  dump)
    [ -z "$STUB_DUMP_FAIL" ] || exit 1
    echo "$2 $3" ;;
  backup|forget)
    [ -z "$STUB_BACKUP_FAIL" ] || exit 1
    while [ $# -gt 0 ] && [ "$1" != "--" ]; do shift; done
    if [ $# -gt 0 ]; then
      shift
      "$@" > /dev/null || { echo "Fatal: command failed" >&2; exit 1; }
    fi ;;
esac
`

// gbakStub is a stand-in gbak for the streaming scripts: it appends its
// arguments to $GBAK_LOG, prints a fake backup and fails when
// STUB_GBAK_FAIL is set.
const gbakStub = `
echo "$*" >> "$GBAK_LOG"
[ -z "$STUB_GBAK_FAIL" ] || exit 1
echo backup
`

// resticScriptRun runs a rendered restic script under sh with the stub
// restic (and gbak) on PATH, and in place of streamingResticBin, and the
// backup and primary volumes moved into temporary directories, returning
// the calls the stub logged, the script's output and whether it
// succeeded.
type resticScriptRun struct {
	t       *testing.T
	bin     string
	volume  string
	primary string
	gbakLog string
}

func newResticScriptRun(t *testing.T) *resticScriptRun {
	t.Helper()
	bin := t.TempDir()
	writeStub(t, bin, binRestic, resticStub)
	writeStub(t, bin, binGbak, gbakStub)
	return &resticScriptRun{t: t, bin: bin, volume: t.TempDir(), primary: t.TempDir(), gbakLog: filepath.Join(t.TempDir(), "gbak.log")}
}

// path returns where a path under the backup or primary volume, or the
// streaming restic binary's directory, lands in the test.
func (r *resticScriptRun) path(p string) string {
	return strings.NewReplacer(
		backupDataMountPath, r.volume,
		primaryDataMountPath, r.primary,
		streamingResticBinDir, r.bin,
	).Replace(p)
}

func (r *resticScriptRun) run(script string, env ...string) (calls []string, out string, ok bool) {
	r.t.Helper()
	log := filepath.Join(r.t.TempDir(), "restic.log")
	cmd := exec.Command("sh", "-c", r.path(script))
	cmd.Env = append(os.Environ(), append([]string{
		"PATH=" + r.bin + ":" + os.Getenv("PATH"),
		"RESTIC_LOG=" + log,
		"GBAK_LOG=" + r.gbakLog,
	}, env...)...)
	output, err := cmd.CombinedOutput()
	data, readErr := os.ReadFile(log)
	if readErr != nil && !os.IsNotExist(readErr) {
		r.t.Fatal(readErr)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n"), string(output), err == nil
}

const (
	stubInitDenied = "STUB_INIT=denied"
	stubInitExists = "STUB_INIT=exists"
	resticInitCall = "init"
	resticDumpCall = "dump"
)

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
		calls, out, ok := newResticScriptRun(t).run(script, stubInitDenied)
		if ok || !strings.Contains(out, "Access Denied") {
			t.Fatalf("script succeeded or hid restic's error:\n%s", out)
		}
		if got := subcommands(calls); len(got) != 1 {
			t.Errorf("calls = %v, want only init", calls)
		}
	})

	t.Run("doesn't forget anything after a failed backup", func(t *testing.T) {
		calls, _, ok := newResticScriptRun(t).run(script, stubInitExists, "STUB_BACKUP_FAIL=1")
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
	calls, out, ok := r.run(script, stubInitExists)
	if !ok || !strings.Contains(out, "Uploaded the backup") {
		t.Fatalf("script failed:\n%s", out)
	}
	if want := "backup --retry-lock 1h --host test --tag base " + r.path(instanceBackupDir()); len(calls) != 2 || calls[1] != want {
		t.Errorf("calls = %v, want init then %q", calls, want)
	}

	// Best effort: a failure is only reported.
	for name, env := range map[string]string{"repository": stubInitDenied, "backup": "STUB_BACKUP_FAIL=1"} {
		_, out, ok := newResticScriptRun(t).run(script, stubInitExists, env)
		if !ok || !strings.Contains(out, "Warning: failed to upload") {
			t.Errorf("failed %s: script failed or didn't warn:\n%s", name, out)
		}
	}
}

// snapshotGroup renders one group of "restic snapshots --group-by paths
// --latest 1 --json", in restic 0.19.1's own shape: the group key's paths
// come before the snapshot's own fields, and its "id" next to "short_id".
func snapshotGroup(time, id, p string) string {
	return fmt.Sprintf(`{"group_key":{"hostname":"","paths":[%[3]q],"tags":null},"snapshots":[{"time":%[1]q,"parent":"ffff","tree":"eeee","paths":[%[3]q],"hostname":"test","username":"root","tags":["base"],"program_version":"restic 0.19.1","summary":{"backup_start":%[1]q,"backup_end":%[1]q,"files_new":1},"id":"%[2]s0000","short_id":%[2]q}]}`,
		time, id, p)
}

func TestResticBootstrapScript(t *testing.T) {
	s3 := &kubebirdv1.S3BackupSpec{Bucket: testS3Bucket}
	local := s3TestInstance(s3)
	streaming := s3TestInstance(s3)
	streaming.Spec.Backup.Destinations = streaming.Spec.Backup.Destinations[1:]
	base := instanceBackupDir()

	// The latest snapshot of three paths, out of order: a file, then the
	// whole base directory (which holds that file too), then another file.
	snapshots := func(r *resticScriptRun) string {
		return "[" + strings.Join([]string{
			snapshotGroup("2026-11-27T10:00:02.5Z", "cccc", r.path(path.Join(base, "security3.fbk"))),
			snapshotGroup("2026-11-27T10:00:00Z", "aaaa", r.path(path.Join(base, "a.fbk"))),
			snapshotGroup("2026-11-27T10:00:01Z", "bbbb", r.path(base)),
			snapshotGroup("2026-11-27T10:00:03Z", "dddd", r.path(path.Join(base, "x"))+"/../../escaped.fbk"),
			snapshotGroup("2026-11-27T10:00:04Z", "eeee", "/etc/elsewhere.fbk"),
		}, ",") + "]"
	}

	t.Run("restores every path's latest base snapshot, oldest first", func(t *testing.T) {
		for name, instance := range map[string]*kubebirdv1.Instance{"local": local, "streaming": streaming} {
			r := newResticScriptRun(t)
			calls, out, ok := r.run(resticBootstrapScript(instance, s3), stubInitExists, "STUB_SNAPSHOTS="+snapshots(r))
			if !ok {
				t.Fatalf("%s: script failed:\n%s", name, out)
			}
			b := r.path(base)
			want := []string{
				resticInitCall,
				"snapshots --tag base --group-by paths --latest 1 --json",
				"dump aaaa " + b + "/a.fbk",
				"restore bbbb:" + b + " --target " + b,
				"dump cccc " + b + "/security3.fbk",
			}
			if _, err := os.Stat(filepath.Join(r.volume, "escaped.fbk")); err == nil {
				t.Errorf("%s: restored a snapshot escaping the base directory", name)
			}
			if strings.Join(calls, "\n") != strings.Join(want, "\n") {
				t.Fatalf("%s: calls =\n%s\nwant\n%s", name, strings.Join(calls, "\n"), strings.Join(want, "\n"))
			}
			for file, content := range map[string]string{
				"a.fbk":         "aaaa " + b + "/a.fbk",
				"restored.fbk":  "bbbb",
				"security3.fbk": "cccc " + b + "/security3.fbk",
			} {
				got, err := os.ReadFile(filepath.Join(b, file))
				if err != nil || strings.TrimSpace(string(got)) != content {
					t.Errorf("%s: %s = %q (%v), want %q", name, file, got, err, content)
				}
			}
		}
	})

	t.Run("restores nothing from a repository without base snapshots", func(t *testing.T) {
		calls, out, ok := newResticScriptRun(t).run(resticBootstrapScript(local, s3), "STUB_INIT=new", "STUB_SNAPSHOTS=[]")
		if !ok || !strings.Contains(out, "holds no backup to restore") {
			t.Fatalf("script failed:\n%s", out)
		}
		if got := subcommands(calls); strings.Join(got, ",") != "init,snapshots" {
			t.Errorf("calls = %v, want init, snapshots", calls)
		}
	})

	t.Run("leaves a backup volume already holding backups alone", func(t *testing.T) {
		r := newResticScriptRun(t)
		b := r.path(base)
		if err := os.MkdirAll(b, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(b, "local.fbk"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		calls, out, ok := r.run(resticBootstrapScript(local, s3), stubInitExists, "STUB_SNAPSHOTS="+snapshots(r))
		if !ok || !strings.Contains(out, "already holds backups") {
			t.Fatalf("script failed:\n%s", out)
		}
		if got := subcommands(calls); len(got) != 0 {
			t.Errorf("calls = %v, want none", calls)
		}
	})

	t.Run("streaming: leaves an already restored primary volume alone", func(t *testing.T) {
		r := newResticScriptRun(t)
		if err := os.WriteFile(r.path(s3RestoredMarker), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		calls, out, ok := r.run(resticBootstrapScript(streaming, s3), stubInitExists, "STUB_SNAPSHOTS="+snapshots(r))
		if !ok || !strings.Contains(out, "already restored") {
			t.Fatalf("script failed:\n%s", out)
		}
		if got := subcommands(calls); len(got) != 0 {
			t.Errorf("calls = %v, want none", calls)
		}
	})

	t.Run("fails rather than starting fresh when the repository can't be read", func(t *testing.T) {
		for name, env := range map[string][]string{
			"init":         {stubInitDenied},
			"snapshots":    {stubInitExists, "STUB_SNAPSHOTS_FAIL=1"},
			resticDumpCall: {stubInitExists, "STUB_DUMP_FAIL=1"},
		} {
			r := newResticScriptRun(t)
			calls, out, ok := r.run(resticBootstrapScript(streaming, s3), append(env, "STUB_SNAPSHOTS="+snapshots(r))...)
			if ok {
				t.Errorf("failed %s: script succeeded:\n%s", name, out)
			}
			if name == resticDumpCall {
				if _, err := os.Stat(filepath.Join(r.path(base), "a.fbk")); err == nil {
					t.Error("failed dump: left a.fbk behind")
				}
				continue
			}
			for _, c := range subcommands(calls) {
				if c == "restore" || c == resticDumpCall {
					t.Errorf("failed %s: restored anyway", name)
				}
			}
		}
	})
}
