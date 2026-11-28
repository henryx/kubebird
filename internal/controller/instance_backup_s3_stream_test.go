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
	"os"
	"path"
	"regexp"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kubebirdv1 "github.com/henryx/kubebird/api/v1"
)

// Versions the S3 tests tag backups with.
const (
	testFirebird4Version = "4.0.3"
	testLatestVersion    = "latest"
)

// The databases of s3StreamingTestInstance.
const (
	streamingTestDatabaseA = "a.fdb"
	streamingTestDatabaseB = "b.fdb"
)

// s3StreamingTestInstance returns an Instance with the given s3 destination
// and no local one, with streamingTestDatabaseA and streamingTestDatabaseB
// in status.databases.
func s3StreamingTestInstance(s3 *kubebirdv1.S3BackupSpec) *kubebirdv1.Instance {
	instance := s3TestInstance(s3)
	instance.Spec.Backup.Destinations = instance.Spec.Backup.Destinations[1:]
	instance.Spec.Databases = []kubebirdv1.DatabaseSpec{{Name: streamingTestDatabaseA}, {Name: streamingTestDatabaseB}}
	instance.Status.Databases = []string{streamingTestDatabaseA, streamingTestDatabaseB}
	return instance
}

func TestS3Streaming(t *testing.T) {
	s3 := &kubebirdv1.S3BackupSpec{Bucket: testS3Bucket}
	disabled := s3StreamingTestInstance(s3)
	disabled.Spec.Backup.Enabled = false
	cases := []struct {
		name                 string
		instance             *kubebirdv1.Instance
		streaming, hasBackup bool
	}{
		{"local only", s3TestInstance(nil), false, true},
		{"local and s3", s3TestInstance(s3), false, true},
		{"s3 only", s3StreamingTestInstance(s3), true, true},
		{"s3 only, disabled", disabled, false, false},
		{"no destination", &kubebirdv1.Instance{Spec: kubebirdv1.InstanceSpec{Backup: kubebirdv1.BackupSpec{Enabled: true}}}, false, false},
	}

	for _, c := range cases {
		if got := s3Streaming(c.instance); got != c.streaming {
			t.Errorf("%s: s3Streaming() = %v, want %v", c.name, got, c.streaming)
		}
		if got := hasBackupDestination(c.instance); got != c.hasBackup {
			t.Errorf("%s: hasBackupDestination() = %v, want %v", c.name, got, c.hasBackup)
		}
	}
}

// runTagPattern matches a streaming run's run tag.
var runTagPattern = regexp.MustCompile(`run:[0-9]{8}T[0-9]{6}Z-[0-9a-f]{6}`)

// sameRun checks every backup call carries the same run tag, and returns
// the calls with it replaced by "run:RUN".
func sameRun(t *testing.T, calls []string) []string {
	t.Helper()
	run := ""
	normalized := make([]string, len(calls))
	for i, c := range calls {
		if strings.HasPrefix(c, "backup ") {
			tags := runTagPattern.FindAllString(c, -1)
			if len(tags) != 1 || (run != "" && tags[0] != run) {
				t.Fatalf("backup call %q has run tags %v, want the run's single one (%q)", c, tags, run)
			}
			run = tags[0]
		}
		normalized[i] = runTagPattern.ReplaceAllString(c, "run:RUN")
	}
	return normalized
}

func TestStreamingScheduledBackupScript(t *testing.T) {
	s3 := &kubebirdv1.S3BackupSpec{Bucket: testS3Bucket, ForcePathStyle: true}
	instance := s3StreamingTestInstance(s3)
	// The version tag is the one the server reported, not the image tag.
	instance.Spec.Version = testLatestVersion
	instance.Status.FirebirdVersion = testFirebird5Version
	script := streamingScheduledBackupScript(instance, s3, backupFrequencies[0])

	t.Run("streams every database, then forgets the frequency's expired snapshots", func(t *testing.T) {
		r := newResticScriptRun(t)
		calls, out, ok := r.run(script, stubInitExists, "SYSDBA_PASSWORD=s3cr3t")
		if !ok {
			t.Fatalf("script failed:\n%s", out)
		}
		calls = sameRun(t, calls)
		want := make([]string, 0, 4)
		want = append(want, resticInitCall)
		for _, db := range []string{"a", "b"} {
			want = append(want, "backup --retry-lock 1h --host test --tag hour --tag run:RUN --tag firebird:5.0.4 --stdin-filename "+
				r.path(path.Join(scheduledBackupDir(backupFrequencyHour), db+".fbk"))+
				" --stdin-from-command -- gbak -backup -se test/3050:service_mgr "+
				r.path(path.Join(primaryDataMountPath, db+".fdb"))+" stdout -user SYSDBA -password s3cr3t")
		}
		want = append(want, "forget --retry-lock 1h --tag hour --group-by paths --keep-within 2h --prune")
		if strings.Join(calls, "\n") != strings.Join(want, "\n") {
			t.Fatalf("calls =\n%s\nwant\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
		}
		gbak, err := os.ReadFile(r.gbakLog)
		if err != nil || strings.Count(string(gbak), "-backup -se") != 2 {
			t.Errorf("gbak calls = %q (%v), want two backups through the Services API", gbak, err)
		}
	})

	t.Run("fails without forgetting anything when gbak fails", func(t *testing.T) {
		calls, out, ok := newResticScriptRun(t).run(script, stubInitExists, "STUB_GBAK_FAIL=1")
		if ok {
			t.Fatalf("script succeeded after a failed gbak:\n%s", out)
		}
		if got := subcommands(calls); strings.Join(got, ",") != "init,backup" {
			t.Errorf("calls = %v, want init, backup", calls)
		}
	})

	t.Run("only forgets when there's no database", func(t *testing.T) {
		empty := s3StreamingTestInstance(s3)
		empty.Status.Databases = nil
		calls, out, ok := newResticScriptRun(t).run(streamingScheduledBackupScript(empty, s3, backupFrequencies[0]), "STUB_INIT=new")
		if !ok {
			t.Fatalf("script failed:\n%s", out)
		}
		if got := subcommands(calls); strings.Join(got, ",") != "init,forget" {
			t.Errorf("calls = %v, want init, forget", calls)
		}
	})
}

func TestStreamingBaseBackupScript(t *testing.T) {
	s3 := &kubebirdv1.S3BackupSpec{Bucket: testS3Bucket}
	instance := s3StreamingTestInstance(s3)
	// A major upgrade's backup, still with the version upgraded from.
	instance.Spec.Version = testFirebird4Version
	instance.Status.FirebirdVersion = testVersion
	script := streamingBaseBackupScript(instance, s3, testVersion)

	r := newResticScriptRun(t)
	calls, out, ok := r.run(script, stubInitExists)
	if !ok || !strings.Contains(out, "Uploaded the backup") {
		t.Fatalf("script failed:\n%s", out)
	}
	calls = sameRun(t, calls)
	backup := func(file, db string) string {
		return "backup --retry-lock 1h --host test --tag base --tag run:RUN --tag firebird:3.0.14 --stdin-filename " + r.path(path.Join(instanceBackupDir(), file)) +
			" --stdin-from-command -- gbak -backup " + r.path(path.Join(primaryDataMountPath, db)) + " stdout"
	}
	want := []string{
		resticInitCall,
		backup(backupFileName(streamingTestDatabaseA), streamingTestDatabaseA),
		backup(backupFileName(streamingTestDatabaseB), streamingTestDatabaseB),
		backup("security3.fbk", "security3.fdb"),
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls =\n%s\nwant\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}

	// Not best effort: the snapshots are the only copy left.
	for name, env := range map[string]string{"repository": stubInitDenied, "upload": "STUB_BACKUP_FAIL=1", "gbak": "STUB_GBAK_FAIL=1"} {
		if _, out, ok := newResticScriptRun(t).run(script, stubInitExists, env); ok {
			t.Errorf("failed %s: script succeeded:\n%s", name, out)
		}
	}
}

// TestMutateStatefulSetS3Streaming checks the StatefulSet restores into an
// emptyDir backup volume, never the backup PVC, with backup-restore able
// to see the primary volume's s3RestoredMarker, and that
// security-database-init restores from it.
func TestMutateStatefulSetS3Streaming(t *testing.T) {
	s3 := &kubebirdv1.S3BackupSpec{Bucket: testS3Bucket}
	instance := s3StreamingTestInstance(s3)
	sysdba := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: sysdbaSecretRefName(instance), Namespace: instance.Namespace},
		Data:       map[string][]byte{sysdbaSecretPasswordKey: []byte("secret")},
	}
	r := s3TestReconciler(t, sysdba)

	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: instance.Name, Namespace: instance.Namespace}}
	if err := r.mutateStatefulSet(context.Background(), sts, instance); err != nil {
		t.Fatal(err)
	}
	spec := sts.Spec.Template.Spec
	var backup *corev1.Volume
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == backupVolumeName {
			backup = &spec.Volumes[i]
		}
	}
	if backup == nil || backup.EmptyDir == nil || backup.PersistentVolumeClaim != nil {
		t.Fatalf("backup volume = %+v, want an emptyDir", backup)
	}
	inits := spec.InitContainers
	if len(inits) != 2 || inits[0].Name != backupRestoreInitContainerName || inits[1].Name != securityDatabaseInitContainerName {
		t.Fatalf("initContainers = %v, want backup-restore then security-database-init", containerNames(inits))
	}
	restore := inits[0]
	if !hasMount(restore, backupVolumeName, backupDataMountPath, false) || !hasMount(restore, primaryVolumeName, primaryDataMountPath, true) {
		t.Errorf("backup-restore mounts = %+v, want the backup volume and the primary one read-only", restore.VolumeMounts)
	}
	if !strings.Contains(restore.Command[2], s3RestoredMarker) {
		t.Errorf("backup-restore script doesn't check %s:\n%s", s3RestoredMarker, restore.Command[2])
	}
	if !hasMount(inits[1], backupVolumeName, backupDataMountPath, false) || !strings.Contains(inits[1].Command[2], "security*.fbk") {
		t.Errorf("security-database-init doesn't restore from the backup volume: %+v\n%s", inits[1].VolumeMounts, inits[1].Command[2])
	}
	if !hasMount(spec.Containers[0], backupVolumeName, backupDataMountPath, false) {
		t.Errorf("firebird mounts = %+v, want the backup volume", spec.Containers[0].VolumeMounts)
	}
}

// TestMutateScheduledBackupCronJobS3Streaming checks a frequency's Job
// streams from the Firebird image with restic copied in, mounting no
// backup PVC and free to run on any node.
func TestMutateScheduledBackupCronJobS3Streaming(t *testing.T) {
	s3 := &kubebirdv1.S3BackupSpec{Bucket: testS3Bucket, Credentials: kubebirdv1.S3CredentialsSpec{SecretRef: testS3Credentials}}
	instance := s3StreamingTestInstance(s3)
	r := s3TestReconciler(t)

	cronJob := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Namespace: instance.Namespace}}
	if err := r.mutateScheduledBackupCronJob(cronJob, instance, backupFrequencies[0]); err != nil {
		t.Fatal(err)
	}
	pod := cronJob.Spec.JobTemplate.Spec.Template.Spec
	if pod.Affinity != nil {
		t.Errorf("affinity = %+v, want none", pod.Affinity)
	}
	checkStreamingPod(t, pod, false)
	backup := pod.Containers[0]
	if backup.Image != backupImage(instance) {
		t.Errorf("backup image = %q, want %q", backup.Image, backupImage(instance))
	}
	if !strings.Contains(backup.Command[2], `--stdin-from-command -- gbak -backup -se test/3050:service_mgr "/var/lib/firebird/data/a.fdb" stdout`) {
		t.Errorf("backup script doesn't stream a.fdb:\n%s", backup.Command[2])
	}
	envs := map[string]bool{}
	for _, e := range backup.Env {
		envs[e.Name] = true
	}
	for _, name := range []string{"SYSDBA_PASSWORD", "RESTIC_REPOSITORY", s3AccessKeyIDKey, s3SecretAccessKeyKey} {
		if !envs[name] {
			t.Errorf("backup env misses %s", name)
		}
	}
}

// TestCreateDatabaseBackupPodS3Streaming checks the delete-time backup Pod
// streams from the Firebird image with restic copied in, mounting no
// backup PVC, its Secrets required.
func TestCreateDatabaseBackupPodS3Streaming(t *testing.T) {
	s3 := &kubebirdv1.S3BackupSpec{Bucket: testS3Bucket, Credentials: kubebirdv1.S3CredentialsSpec{SecretRef: testS3Credentials}}
	instance := s3StreamingTestInstance(s3)
	r := s3TestReconciler(t, instance)

	if err := r.createDatabaseBackupPod(context.Background(), instance, "3.0.14"); err == nil {
		t.Fatal("createDatabaseBackupPod() = nil, want a waiting error")
	}
	pod := &corev1.Pod{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: databaseBackupPodName(instance), Namespace: instance.Namespace}, pod); err != nil {
		t.Fatal(err)
	}
	checkStreamingPod(t, pod.Spec, true)
	backup := pod.Spec.Containers[0]
	if !strings.Contains(backup.Command[2], `--stdin-from-command -- gbak -backup "/var/lib/firebird/data/security3.fdb" stdout`) {
		t.Errorf("backup script doesn't stream the security database:\n%s", backup.Command[2])
	}
	if strings.Contains(backup.Command[2], "exit 0") {
		t.Errorf("backup script is best effort:\n%s", backup.Command[2])
	}
	for _, e := range backup.Env[1:] {
		if *e.ValueFrom.SecretKeyRef.Optional {
			t.Errorf("env %s is optional", e.Name)
		}
	}
}

// checkStreamingPod checks pod runs a single firebird container streaming
// with restic copied in by the restic-install initContainer, without the
// backup volume. primary is whether it mounts the primary volume.
func checkStreamingPod(t *testing.T, pod corev1.PodSpec, primary bool) {
	t.Helper()
	if n := containerNames(pod.InitContainers); len(n) != 1 || n[0] != resticInstallContainerName {
		t.Fatalf("initContainers = %v, want restic-install", n)
	}
	if n := containerNames(pod.Containers); len(n) != 1 || n[0] != containerName {
		t.Fatalf("containers = %v, want firebird", n)
	}
	install, backup := pod.InitContainers[0], pod.Containers[0]
	if install.Image != defaultResticImage || !strings.Contains(install.Command[2], streamingResticBin) ||
		!hasMount(install, streamingResticBinVolumeName, streamingResticBinDir, false) {
		t.Errorf("restic-install = %+v", install)
	}
	if !hasMount(backup, streamingResticBinVolumeName, streamingResticBinDir, true) ||
		!hasMount(backup, resticPasswordVolumeName, resticPasswordMountPath, true) {
		t.Errorf("backup mounts = %+v, want restic's binary and password", backup.VolumeMounts)
	}
	if hasMount(backup, primaryVolumeName, primaryDataMountPath, false) != primary {
		t.Errorf("backup mounts = %+v, want the primary volume: %v", backup.VolumeMounts, primary)
	}
	for _, v := range pod.Volumes {
		if v.Name == backupVolumeName || (v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "test-backup") {
			t.Errorf("volume %s: the backup volume is mounted", v.Name)
		}
	}
	if vol := resticVolume(pod.Volumes); vol == nil || *vol.Secret.Optional {
		t.Errorf("restic password volume = %+v, want a required one", vol)
	}
}
