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
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kubebirdv1 "github.com/henryx/kubebird/api/v1"
)

const (
	testS3Bucket      = "kubebird"
	testS3Credentials = "s3-credentials"
)

// s3TestInstance returns an Instance with a local backup volume and the
// given s3 destination (none when nil).
func s3TestInstance(s3 *kubebirdv1.S3BackupSpec) *kubebirdv1.Instance {
	destinations := []kubebirdv1.BackupDestinationSpec{
		{Local: &kubebirdv1.LocalBackupSpec{Storage: kubebirdv1.StorageVolumeSpec{Size: apiresource.MustParse("1Gi")}}},
	}
	if s3 != nil {
		destinations = append(destinations, kubebirdv1.BackupDestinationSpec{S3: s3})
	}
	return &kubebirdv1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: resourceNamespace},
		Spec: kubebirdv1.InstanceSpec{
			Image:     testImage,
			Version:   testVersion,
			Databases: []kubebirdv1.DatabaseSpec{{Name: "a.fdb"}},
			Backup: kubebirdv1.BackupSpec{
				Enabled:      true,
				Destinations: destinations,
				Retention:    kubebirdv1.RetentionSpec{Hour: "2h"},
			},
		},
		Status: kubebirdv1.InstanceStatus{Databases: []string{"a.fdb"}},
	}
}

func s3TestReconciler(t *testing.T, objs ...client.Object) *InstanceReconciler {
	t.Helper()
	scheme := newTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &InstanceReconciler{Client: c, Scheme: scheme}
}

func TestS3Destination(t *testing.T) {
	s3 := &kubebirdv1.S3BackupSpec{Bucket: testS3Bucket}
	if got := s3Destination(s3TestInstance(s3)); got != s3 {
		t.Errorf("s3Destination() = %v, want the s3 entry", got)
	}
	if got := s3Destination(s3TestInstance(nil)); got != nil {
		t.Errorf("s3Destination() without an s3 entry = %v, want nil", got)
	}

	disabled := s3TestInstance(s3)
	disabled.Spec.Backup.Enabled = false
	if got := s3Destination(disabled); got != nil {
		t.Errorf("s3Destination() with backup disabled = %v, want nil", got)
	}

	noLocal := s3TestInstance(s3)
	noLocal.Spec.Backup.Destinations = noLocal.Spec.Backup.Destinations[1:]
	if got := s3Destination(noLocal); got != nil {
		t.Errorf("s3Destination() without a local entry = %v, want nil", got)
	}
}

func TestResticRepository(t *testing.T) {
	cases := []struct {
		s3   kubebirdv1.S3BackupSpec
		want string
	}{
		{kubebirdv1.S3BackupSpec{Bucket: "b"}, "s3:s3.amazonaws.com/b/default/test"},
		{kubebirdv1.S3BackupSpec{Bucket: "b", Endpoint: "http://silo:9000/", Prefix: "/x/y/"}, "s3:http://silo:9000/b/x/y"},
		{kubebirdv1.S3BackupSpec{Bucket: "b", Endpoint: "minio.example.com:9000"}, "s3:minio.example.com:9000/b/default/test"},
	}
	for _, c := range cases {
		if got := resticRepository(s3TestInstance(&c.s3), &c.s3); got != c.want {
			t.Errorf("resticRepository(%+v) = %q, want %q", c.s3, got, c.want)
		}
	}
}

func TestResticCommand(t *testing.T) {
	if got, want := resticCommand(&kubebirdv1.S3BackupSpec{}), "restic --password-file /etc/kubebird/restic/password"; got != want {
		t.Errorf("resticCommand() = %q, want %q", got, want)
	}
	got := resticCommand(&kubebirdv1.S3BackupSpec{ForcePathStyle: true, Region: "eu-west-1", InsecureSkipTLSVerify: true})
	for _, want := range []string{"-o s3.bucket-lookup=path", "-o s3.region=eu-west-1", "--insecure-tls"} {
		if !strings.Contains(got, want) {
			t.Errorf("resticCommand() = %q, missing %q", got, want)
		}
	}
}

func TestResticDuration(t *testing.T) {
	cases := []struct {
		d    retentionDuration
		want string
	}{
		{retentionDuration{amount: 12, unit: 'h'}, "12h"},
		{retentionDuration{amount: 3, unit: 'd'}, "3d"},
		{retentionDuration{amount: 4, unit: 'w'}, "28d"}, // restic has no weeks
		{retentionDuration{amount: 6, unit: 'm'}, "6m"},
		{retentionDuration{amount: 2, unit: 'y'}, "2y"},
	}
	for _, c := range cases {
		if got := c.d.resticDuration(); got != c.want {
			t.Errorf("resticDuration(%+v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestResticEnv(t *testing.T) {
	s3 := &kubebirdv1.S3BackupSpec{Bucket: testS3Bucket}
	instance := s3TestInstance(s3)
	if env := resticEnv(instance, s3, false); len(env) != 1 || env[0].Name != "RESTIC_REPOSITORY" {
		t.Errorf("resticEnv() without credentials = %v, want only RESTIC_REPOSITORY (ambient credentials)", env)
	}

	s3.Credentials.SecretRef = testS3Credentials
	for _, optional := range []bool{false, true} {
		env := resticEnv(instance, s3, optional)
		keys := map[string]bool{}
		for _, e := range env[1:] {
			ref := e.ValueFrom.SecretKeyRef
			if ref.Name != testS3Credentials || ref.Key != e.Name || *ref.Optional != optional {
				t.Errorf("env %s = %+v, want key %s of %s, optional %v", e.Name, ref, e.Name, testS3Credentials, optional)
			}
			keys[e.Name] = true
		}
		if !keys[s3AccessKeyIDKey] || !keys[s3SecretAccessKeyKey] {
			t.Errorf("resticEnv() = %v, want both AWS keys", env)
		}
	}
}

// TestMutateStatefulSetS3 checks the backup-restore initContainer runs
// before security-database-init, with the backup volume writable and the
// password mounted, and that nothing changes without an s3 destination.
func TestMutateStatefulSetS3(t *testing.T) {
	s3 := &kubebirdv1.S3BackupSpec{Bucket: testS3Bucket}
	instance := s3TestInstance(s3)
	sysdba := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: sysdbaSecretRefName(instance), Namespace: instance.Namespace},
		Data:       map[string][]byte{sysdbaSecretPasswordKey: []byte("secret")},
	}
	r := s3TestReconciler(t, sysdba)

	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: instance.Name, Namespace: instance.Namespace}}
	if err := r.mutateStatefulSet(context.Background(), sts, instance); err != nil {
		t.Fatal(err)
	}
	inits := sts.Spec.Template.Spec.InitContainers
	if len(inits) != 2 || inits[0].Name != backupRestoreInitContainerName || inits[1].Name != securityDatabaseInitContainerName {
		t.Fatalf("initContainers = %v, want backup-restore then security-database-init", containerNames(inits))
	}
	restore := inits[0]
	if restore.Image != defaultResticImage {
		t.Errorf("backup-restore image = %q, want %q", restore.Image, defaultResticImage)
	}
	if !hasMount(restore, backupVolumeName, backupDataMountPath, false) || !hasMount(restore, resticPasswordVolumeName, resticPasswordMountPath, true) {
		t.Errorf("backup-restore mounts = %+v", restore.VolumeMounts)
	}
	if !strings.Contains(restore.Command[2], "restore latest --tag \"base\"") {
		t.Errorf("backup-restore script doesn't restore the base snapshot:\n%s", restore.Command[2])
	}
	vol := resticVolume(sts.Spec.Template.Spec.Volumes)
	if vol == nil || vol.Secret.SecretName != "test-restic" || *vol.Secret.Optional {
		t.Errorf("restic password volume = %+v, want the required test-restic Secret", vol)
	}

	plain := s3TestInstance(nil)
	sts = &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: plain.Name, Namespace: plain.Namespace}}
	if err := r.mutateStatefulSet(context.Background(), sts, plain); err != nil {
		t.Fatal(err)
	}
	if n := containerNames(sts.Spec.Template.Spec.InitContainers); len(n) != 1 {
		t.Errorf("initContainers without s3 = %v, want only security-database-init", n)
	}
	if resticVolume(sts.Spec.Template.Spec.Volumes) != nil {
		t.Error("restic password volume present without an s3 destination")
	}
}

// TestMutateScheduledBackupCronJobS3 checks a frequency's Job runs the
// backup as an initContainer and the upload after it, with the same pod
// affinity and the backup volume mounted read-only.
func TestMutateScheduledBackupCronJobS3(t *testing.T) {
	s3 := &kubebirdv1.S3BackupSpec{Bucket: testS3Bucket, Credentials: kubebirdv1.S3CredentialsSpec{SecretRef: testS3Credentials}}
	instance := s3TestInstance(s3)
	r := s3TestReconciler(t)
	hour := backupFrequencies[0]

	cronJob := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Namespace: instance.Namespace}}
	if err := r.mutateScheduledBackupCronJob(cronJob, instance, hour); err != nil {
		t.Fatal(err)
	}
	pod := cronJob.Spec.JobTemplate.Spec.Template.Spec
	if n := containerNames(pod.InitContainers); len(n) != 1 || n[0] != containerName {
		t.Fatalf("initContainers = %v, want the firebird backup", n)
	}
	if !strings.Contains(pod.InitContainers[0].Command[2], "-action_backup") {
		t.Errorf("backup initContainer doesn't take the backup:\n%s", pod.InitContainers[0].Command[2])
	}
	if n := containerNames(pod.Containers); len(n) != 1 || n[0] != uploadContainerName {
		t.Fatalf("containers = %v, want upload", n)
	}
	upload := pod.Containers[0]
	if !hasMount(upload, backupVolumeName, backupDataMountPath, true) {
		t.Errorf("upload mounts = %+v, want the backup volume read-only", upload.VolumeMounts)
	}
	script := upload.Command[2]
	for _, want := range []string{
		`backup --retry-lock 1h --host "test" --tag "hour" --exclude ".claim-*" "/var/lib/firebird/backup/hour"`,
		`forget --retry-lock 1h --tag "hour" --group-by tags --keep-within 2h --prune`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("upload script missing %q:\n%s", want, script)
		}
	}
	if pod.Affinity == nil || pod.Affinity.PodAffinity == nil {
		t.Error("the Job lost its pod affinity to the Firebird pod")
	}
	if vol := resticVolume(pod.Volumes); vol == nil || *vol.Secret.Optional {
		t.Errorf("restic password volume = %+v, want a required one", vol)
	}

	plain := s3TestInstance(nil)
	cronJob = &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Namespace: plain.Namespace}}
	if err := r.mutateScheduledBackupCronJob(cronJob, plain, hour); err != nil {
		t.Fatal(err)
	}
	pod = cronJob.Spec.JobTemplate.Spec.Template.Spec
	if len(pod.InitContainers) != 0 || len(pod.Containers) != 1 || pod.Containers[0].Name != containerName {
		t.Errorf("without s3, Job containers = %v/%v, want just firebird", containerNames(pod.InitContainers), containerNames(pod.Containers))
	}
}

// TestCreateDatabaseBackupPodS3 checks the delete-time backup Pod runs gbak
// as an initContainer, then the best effort upload, its Secrets optional.
func TestCreateDatabaseBackupPodS3(t *testing.T) {
	s3 := &kubebirdv1.S3BackupSpec{Bucket: testS3Bucket, Credentials: kubebirdv1.S3CredentialsSpec{SecretRef: testS3Credentials}}
	instance := s3TestInstance(s3)
	r := s3TestReconciler(t, instance)

	if err := r.createDatabaseBackupPod(context.Background(), instance, testVersion); err == nil {
		t.Fatal("createDatabaseBackupPod() = nil, want a waiting error")
	}
	pod := &corev1.Pod{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: databaseBackupPodName(instance), Namespace: instance.Namespace}, pod); err != nil {
		t.Fatal(err)
	}
	if n := containerNames(pod.Spec.InitContainers); len(n) != 1 || n[0] != containerName {
		t.Fatalf("initContainers = %v, want the gbak backup", n)
	}
	if n := containerNames(pod.Spec.Containers); len(n) != 1 || n[0] != uploadContainerName {
		t.Fatalf("containers = %v, want upload", n)
	}
	upload := pod.Spec.Containers[0]
	if !strings.Contains(upload.Command[2], "exit 0") || !strings.Contains(upload.Command[2], `--tag "base"`) {
		t.Errorf("upload script isn't a best effort upload of base:\n%s", upload.Command[2])
	}
	for _, e := range upload.Env[1:] {
		if !*e.ValueFrom.SecretKeyRef.Optional {
			t.Errorf("env %s isn't optional", e.Name)
		}
	}
	if vol := resticVolume(pod.Spec.Volumes); vol == nil || !*vol.Secret.Optional {
		t.Errorf("restic password volume = %+v, want an optional one", vol)
	}
}

// TestPodFailureReasonInitContainer checks a failed gbak initContainer is
// reported, not just a failed container.
func TestPodFailureReasonInitContainer(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "gbak failed"}},
		}},
	}}
	if got := podFailureReason(pod); got != "gbak failed" {
		t.Errorf("podFailureReason() = %q, want the initContainer's message", got)
	}
}

func TestReconcileS3Secrets(t *testing.T) {
	ctx := context.Background()

	t.Run("generates the repository password once", func(t *testing.T) {
		instance := s3TestInstance(&kubebirdv1.S3BackupSpec{Bucket: testS3Bucket})
		r := s3TestReconciler(t)
		if err := r.reconcileS3Secrets(ctx, instance); err != nil {
			t.Fatal(err)
		}
		secret := &corev1.Secret{}
		key := types.NamespacedName{Name: "test-restic", Namespace: instance.Namespace}
		if err := r.Get(ctx, key, secret); err != nil {
			t.Fatal(err)
		}
		password := secret.StringData[resticPasswordKey]
		if len(password) < 20 {
			t.Errorf("generated password = %q, want a random one", password)
		}
		if len(secret.OwnerReferences) != 0 {
			t.Error("the repository Secret is owner-referenced, so deleting the Instance would delete it")
		}
		if err := r.reconcileS3Secrets(ctx, instance); err != nil {
			t.Fatal(err)
		}
		if err := r.Get(ctx, key, secret); err != nil {
			t.Fatal(err)
		}
		if secret.StringData[resticPasswordKey] != password {
			t.Error("a second reconcile changed the repository password")
		}
	})

	t.Run("requires referenced Secrets to exist", func(t *testing.T) {
		instance := s3TestInstance(&kubebirdv1.S3BackupSpec{
			Bucket:      testS3Bucket,
			Credentials: kubebirdv1.S3CredentialsSpec{SecretRef: testS3Credentials},
			Repository:  kubebirdv1.ResticRepositorySpec{SecretRef: "my-restic"},
		})
		r := s3TestReconciler(t)
		if err := r.reconcileS3Secrets(ctx, instance); err == nil || !strings.Contains(err.Error(), testS3Credentials) {
			t.Errorf("reconcileS3Secrets() = %v, want the missing credentials Secret reported", err)
		}

		credentials := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testS3Credentials, Namespace: instance.Namespace}}
		r = s3TestReconciler(t, credentials)
		if err := r.reconcileS3Secrets(ctx, instance); err == nil || !strings.Contains(err.Error(), "my-restic") {
			t.Errorf("reconcileS3Secrets() = %v, want the missing repository Secret reported", err)
		}
		if got := s3SecretNames(instance); len(got) != 2 || got[0] != "my-restic" || got[1] != testS3Credentials {
			t.Errorf("s3SecretNames() = %v", got)
		}
	})

	t.Run("does nothing without an s3 destination", func(t *testing.T) {
		r := s3TestReconciler(t)
		if err := r.reconcileS3Secrets(ctx, s3TestInstance(nil)); err != nil {
			t.Fatal(err)
		}
		if got := s3SecretNames(s3TestInstance(nil)); got != nil {
			t.Errorf("s3SecretNames() = %v, want none", got)
		}
	})
}

func containerNames(containers []corev1.Container) []string {
	names := make([]string, len(containers))
	for i, c := range containers {
		names[i] = c.Name
	}
	return names
}

func hasMount(c corev1.Container, volume, mountPath string, readOnly bool) bool {
	for _, m := range c.VolumeMounts {
		if m.Name == volume && m.MountPath == mountPath && m.ReadOnly == readOnly {
			return true
		}
	}
	return false
}

// resticVolume returns the restic password volume among volumes, or nil.
func resticVolume(volumes []corev1.Volume) *corev1.Volume {
	for i := range volumes {
		if volumes[i].Name == resticPasswordVolumeName {
			return &volumes[i]
		}
	}
	return nil
}
