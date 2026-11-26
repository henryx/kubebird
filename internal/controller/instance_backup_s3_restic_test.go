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
	"io"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	kubebirdv1 "github.com/henryx/kubebird/api/v1"
	"github.com/henryx/kubebird/test/silo"
)

const (
	testResticPassword = "repository-password"
	testSiloAlias      = "silo"
)

// resticRun runs script in a restic container (defaultResticImage) on nw,
// with the environment and the password file the Pods Kubebird creates
// give it, and returns its exit code and output. Each run starts from an
// empty container, so whatever must carry over between runs lives in the
// S3 repository, as between Kubebird's own Pods.
func resticRun(t *testing.T, nw *testcontainers.DockerNetwork, instance *kubebirdv1.Instance, s3 *kubebirdv1.S3BackupSpec, password, script string) (int, string) {
	t.Helper()
	ctx := context.Background()
	c, err := testcontainers.Run(ctx, defaultResticImage,
		testcontainers.WithEntrypoint("sh", "-c", script),
		testcontainers.WithEnv(map[string]string{
			"RESTIC_REPOSITORY":  resticRepository(instance, s3),
			s3AccessKeyIDKey:     silo.AccessKey,
			s3SecretAccessKeyKey: silo.SecretKey,
		}),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			Reader:            strings.NewReader(password),
			ContainerFilePath: resticPasswordFile,
			FileMode:          0o400,
		}),
		network.WithNetwork(nil, nw),
		testcontainers.WithWaitStrategy(wait.ForExit().WithExitTimeout(5*time.Minute)),
	)
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatalf("failed to run restic: %v", err)
	}
	state, err := c.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	logs, err := c.Logs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logs.Close() }()
	out, err := io.ReadAll(logs)
	if err != nil {
		t.Fatal(err)
	}
	return state.ExitCode, string(out)
}

// TestResticScriptsAgainstSilo runs the restic scripts Kubebird renders
// against a real S3 endpoint, Silo, through testcontainers: the
// scheduled upload and its forget, the best effort base upload, and the
// StatefulSet's bootstrap restore, in the order an Instance's lifetime
// runs them. Skipped when no container runtime is available.
func TestResticScriptsAgainstSilo(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()

	nw, err := network.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nw.Remove(context.Background()) })

	siloContainer, err := silo.Run(ctx, network.WithNetwork([]string{testSiloAlias}, nw))
	testcontainers.CleanupContainer(t, siloContainer)
	if err != nil {
		t.Fatal(err)
	}

	s3 := &kubebirdv1.S3BackupSpec{
		Endpoint:       fmt.Sprintf("http://%s:%d", testSiloAlias, silo.Port),
		Bucket:         testS3Bucket,
		ForcePathStyle: true,
		Credentials:    kubebirdv1.S3CredentialsSpec{SecretRef: testS3Credentials},
	}
	instance := s3TestInstance(s3)
	restic := resticCommand(s3)
	hourDir := scheduledBackupDir(backupFrequencyHour)
	baseDir := instanceBackupDir()
	twoHours, err := parseRetention("2h")
	if err != nil {
		t.Fatal(err)
	}
	run := func(password, script string) (int, string) {
		return resticRun(t, nw, instance, s3, password, script)
	}

	// Each step depends on the repository state the previous ones left.
	t.Run("bootstrap on a new repository restores nothing", func(t *testing.T) {
		code, out := run(testResticPassword, resticBootstrapScript(s3))
		if code != 0 || !strings.Contains(out, "holds no backup to restore") {
			t.Fatalf("exit code %d, output:\n%s", code, out)
		}
	})

	t.Run("scheduled upload snapshots the frequency and forgets expired snapshots", func(t *testing.T) {
		// An expired snapshot of the same frequency, as left by a run
		// years ago, and a claim file the upload must leave out.
		script := fmt.Sprintf(`set -e
mkdir -p %[1]q
echo old > %[1]q/a-20000101T000000Z.fbk
%[2]s backup --host test --tag hour --time "2000-01-01 00:00:00" %[1]q
rm %[1]q/a-20000101T000000Z.fbk
echo new > %[1]q/a-20990101T000000Z.fbk
touch %[1]q/.claim-20990101T000000Z
%[3]s
%[2]s snapshots --tag hour --json | grep -o '"id"' | wc -l | sed 's/^/snapshots: /'
%[2]s ls latest --tag hour
`, hourDir, restic, resticUploadScript(instance, s3, backupFrequencyHour, twoHours))
		code, out := run(testResticPassword, script)
		if code != 0 {
			t.Fatalf("exit code %d, output:\n%s", code, out)
		}
		for _, want := range []string{"snapshots: 1", hourDir + "/a-20990101T000000Z.fbk"} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, ".claim-") {
			t.Errorf("the timestamp claim was uploaded:\n%s", out)
		}
	})

	t.Run("scheduled upload fails the Job when the repository can't be opened", func(t *testing.T) {
		script := fmt.Sprintf("mkdir -p %q\n%s", hourDir, resticUploadScript(instance, s3, backupFrequencyHour, twoHours))
		if code, out := run("wrong-password", script); code == 0 {
			t.Fatalf("exit code 0 with the wrong password, output:\n%s", out)
		}
	})

	t.Run("base upload is best effort", func(t *testing.T) {
		script := fmt.Sprintf("mkdir -p %[1]q\necho users > %[1]q/security3.fbk\necho data > %[1]q/a.fbk\n%[2]s",
			baseDir, resticBaseUploadScript(instance, s3))
		if code, out := run("wrong-password", script); code != 0 || !strings.Contains(out, "Warning: failed to upload") {
			t.Fatalf("with the wrong password: exit code %d, output:\n%s", code, out)
		}
		if code, out := run(testResticPassword, script); code != 0 || !strings.Contains(out, "Uploaded the backup") {
			t.Fatalf("exit code %d, output:\n%s", code, out)
		}
	})

	t.Run("bootstrap restores the base snapshot into an empty backup volume", func(t *testing.T) {
		script := resticBootstrapScript(s3) + fmt.Sprintf("cat %[1]q/security3.fbk %[1]q/a.fbk\n", baseDir)
		code, out := run(testResticPassword, script)
		if code != 0 || !strings.Contains(out, "Restoring the backups") || !strings.Contains(out, "users\ndata") {
			t.Fatalf("exit code %d, output:\n%s", code, out)
		}
	})

	t.Run("bootstrap leaves a backup volume holding backups alone", func(t *testing.T) {
		script := fmt.Sprintf("mkdir -p %[1]q\necho local > %[1]q/b.fbk\n%[2]sls %[1]q\n", baseDir, resticBootstrapScript(s3))
		code, out := run(testResticPassword, script)
		if code != 0 || !strings.Contains(out, "already holds backups") || strings.Contains(out, "security3.fbk") {
			t.Fatalf("exit code %d, output:\n%s", code, out)
		}
	})

	t.Run("bootstrap fails rather than starting fresh when the repository can't be read", func(t *testing.T) {
		if code, out := run("wrong-password", resticBootstrapScript(s3)); code == 0 {
			t.Fatalf("exit code 0 with the wrong password, output:\n%s", out)
		}
	})
}
