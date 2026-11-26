//go:build e2e
// +build e2e

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

package e2e

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/henryx/kubebird/test/silo"
	"github.com/henryx/kubebird/test/utils"
)

const (
	s3InstanceName     = "e2e-s3-backup"
	s3PodName          = s3InstanceName + "-0"
	s3DatabaseName     = "s3.fdb"
	s3UserKey          = "s3User"
	s3UserSecret       = s3InstanceName + "-user-s3user"
	s3SysdbaSecret     = s3InstanceName + "-user-sysdba"
	s3ResticSecret     = s3InstanceName + "-restic"
	s3CredentialSecret = s3InstanceName + "-s3-credentials"
	s3PrimaryPVC       = s3InstanceName + "-primary"
	s3BackupPVC        = s3InstanceName + "-backup"
	s3CronJob          = s3InstanceName + "-backup-hour"
	s3Bucket           = "kubebird-e2e"

	// s3ResticImage must match the operator's default restic image
	// (defaultResticImage in internal/controller).
	s3ResticImage = "restic/restic:0.19.1"
)

// kindNetwork returns the Docker network the Kind cluster's nodes run on,
// which Silo joins so the cluster's pods can reach it by IP: "kind",
// unless KIND_EXPERIMENTAL_DOCKER_NETWORK picks another one.
func kindNetwork() string {
	if n := os.Getenv("KIND_EXPERIMENTAL_DOCKER_NETWORK"); n != "" {
		return n
	}
	return "kind"
}

// s3Login connects to the database through the instance's own server as
// user, returning isql's output.
func s3Login(user, password, sql string) (string, error) {
	cmd := exec.Command("kubectl", "exec", "-i", s3PodName, "-n", namespace, "-c", firebirdContainer,
		"--", "isql", "-q", "-user", user, "-password", password, "localhost:/var/lib/firebird/data/"+s3DatabaseName)
	cmd.Stdin = strings.NewReader("SET BAIL ON;\nSET LIST ON;\n" + sql + "\nQUIT;\n")
	return utils.Run(cmd)
}

// waitForS3Instance waits until the Instance has its database and user,
// without a reconcile error.
func waitForS3Instance() {
	Eventually(func(g Gomega) {
		for path, want := range map[string]string{
			"{.status.databases[*]}":  s3DatabaseName,
			"{.status.users[*].name}": s3UserKey,
			"{.status.phase}":         "Ready",
			"{.status.error}":         "",
		} {
			cmd := exec.Command("kubectl", "get", "instance", s3InstanceName, "-n", namespace, "-o", "jsonpath="+path)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal(want), path)
		}
	}, 6*time.Minute, 2*time.Second).Should(Succeed())
}

// instanceS3BackupSpecs exercises the s3 backup destination against Silo,
// an S3-compatible object store started through testcontainers on the
// Kind cluster's own Docker network: a scheduled backup Job uploads its
// backup into the restic repository, deleting the Instance uploads the
// delete-time backup, and, after the backup PVC is deleted too, a
// recreated Instance restores its database and users from the repository
// alone.
//
// It must be called from inside the "Manager" Ordered Describe in
// e2e_test.go, before that Describe's AfterAll tears down the manager.
func instanceS3BackupSpecs() {
	Context("Instance s3 backup destination", Ordered, func() {
		var siloContainer *testcontainers.DockerContainer
		var endpoint string
		var manifest string

		// restic runs restic subcommand args against the Instance's
		// repository from a container on the Kind network, with the
		// repository password from the Instance's generated Secret, and
		// returns its output.
		restic := func(args ...string) (string, error) {
			password, err := getSecretField(s3ResticSecret, "password")
			if err != nil {
				return "", err
			}
			ctx := context.Background()
			c, err := testcontainers.Run(ctx, s3ResticImage,
				testcontainers.WithCmd(append([]string{"--password-file", "/password", "-o", "s3.bucket-lookup=path"}, args...)...),
				testcontainers.WithEnv(map[string]string{
					"RESTIC_REPOSITORY":     fmt.Sprintf("s3:%s/%s/%s/%s", endpoint, s3Bucket, namespace, s3InstanceName),
					"AWS_ACCESS_KEY_ID":     silo.AccessKey,
					"AWS_SECRET_ACCESS_KEY": silo.SecretKey,
				}),
				testcontainers.WithFiles(testcontainers.ContainerFile{
					Reader: strings.NewReader(password), ContainerFilePath: "/password", FileMode: 0o400,
				}),
				network.WithNetworkName(nil, kindNetwork()),
				testcontainers.WithWaitStrategy(wait.ForExit().WithExitTimeout(3*time.Minute)),
			)
			defer func() { _ = testcontainers.TerminateContainer(c) }()
			if err != nil {
				return "", err
			}
			state, err := c.State(ctx)
			if err != nil {
				return "", err
			}
			logs, err := c.Logs(ctx)
			if err != nil {
				return "", err
			}
			defer func() { _ = logs.Close() }()
			out, err := io.ReadAll(logs)
			if err != nil {
				return "", err
			}
			if state.ExitCode != 0 {
				return string(out), fmt.Errorf("restic %v exited with %d: %s", args, state.ExitCode, out)
			}
			return string(out), nil
		}

		applyManifest := func() {
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		}

		BeforeAll(func() {
			By("starting Silo on the Kind network")
			var err error
			siloContainer, err = silo.Run(context.Background(), network.WithNetworkName(nil, kindNetwork()))
			Expect(err).NotTo(HaveOccurred())
			inspect, err := siloContainer.Inspect(context.Background())
			Expect(err).NotTo(HaveOccurred())
			settings, ok := inspect.NetworkSettings.Networks[kindNetwork()]
			Expect(ok).To(BeTrue(), "Silo isn't on the %s network", kindNetwork())
			endpoint = fmt.Sprintf("http://%s:%d", settings.IPAddress, silo.Port)

			By("creating the S3 credentials Secret")
			cmd := exec.Command("kubectl", "create", "secret", "generic", s3CredentialSecret, "-n", namespace,
				"--from-literal=AWS_ACCESS_KEY_ID="+silo.AccessKey,
				"--from-literal=AWS_SECRET_ACCESS_KEY="+silo.SecretKey)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			manifest = fmt.Sprintf(`
apiVersion: kubebird.github.io/v1
kind: Instance
metadata:
  name: %s
  namespace: %s
spec:
  image: firebirdsql/firebird
  version: %s
  databases:
    - name: "%s"
      owner: %s
  storage:
    primary:
      size: 1Gi
  authentication:
    %s: {}
  backup:
    enabled: true
    destinations:
      - local:
          storage:
            size: 1Gi
      - s3:
          endpoint: %s
          bucket: %s
          forcePathStyle: true
          credentials:
            secretRef: %s
    retention:
      hour: "2h"
`, s3InstanceName, namespace, firebirdVersion, s3DatabaseName, s3UserKey, s3UserKey, endpoint, s3Bucket, s3CredentialSecret)
		})

		AfterAll(func() {
			By("deleting the e2e-s3-backup Instance, its PVCs and Secrets")
			cmd := exec.Command("kubectl", "delete", "instance", s3InstanceName, "-n", namespace,
				"--ignore-not-found", "--timeout=3m")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pvc", s3PrimaryPVC, s3BackupPVC, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "secret", s3SysdbaSecret, s3UserSecret, s3ResticSecret,
				s3CredentialSecret, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			if siloContainer != nil {
				_ = testcontainers.TerminateContainer(siloContainer)
			}
		})

		It("should upload a scheduled backup into the restic repository", func() {
			By("applying the Instance CR")
			applyManifest()
			waitForS3Instance()

			By("generating the restic repository password Secret")
			password, err := getSecretField(s3ResticSecret, "password")
			Expect(err).NotTo(HaveOccurred())
			Expect(password).NotTo(BeEmpty())

			By("running the hourly CronJob's backup and upload")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "cronjob", s3CronJob, "-n", namespace,
					"-o", "jsonpath={.spec.jobTemplate.spec.template.spec.containers[*].name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("upload"))
			}, 3*time.Minute, 2*time.Second).Should(Succeed())
			const manualJob = s3CronJob + "-manual"
			cmd := exec.Command("kubectl", "create", "job", manualJob, "--from=cronjob/"+s3CronJob, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				cmd := exec.Command("kubectl", "delete", "job", manualJob, "-n", namespace, "--ignore-not-found")
				_, _ = utils.Run(cmd)
			}()
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "job", manualJob, "-n", namespace, "-o", "jsonpath={.status.succeeded}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}, 5*time.Minute, 2*time.Second).Should(Succeed())

			By("finding the backup in the repository's latest hourly snapshot")
			output, err := restic("ls", "latest", "--tag", "hour")
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(MatchRegexp(`/var/lib/firebird/backup/hour/s3-[0-9]{8}T[0-9]{6}Z\.fbk`))
		})

		It("should upload the delete-time backup", func() {
			password, err := getSecretField(s3UserSecret, "password")
			Expect(err).NotTo(HaveOccurred())

			By("creating a marker table as the regular user, the database's owner")
			_, err = s3Login(s3UserKey, password,
				"CREATE TABLE MARKER (ID INTEGER);\nCOMMIT;\nINSERT INTO MARKER VALUES (42);\nCOMMIT;")
			Expect(err).NotTo(HaveOccurred())

			By("deleting the Instance")
			cmd := exec.Command("kubectl", "delete", "instance", s3InstanceName, "-n", namespace, "--timeout=5m")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("finding the database and the security database in the repository's latest base snapshot")
			output, err := restic("ls", "latest", "--tag", "base")
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("/var/lib/firebird/backup/base/s3.fbk"))
			Expect(output).To(ContainSubstring("/var/lib/firebird/backup/base/" + securityDatabaseFile(".fbk")))
		})

		It("should restore the database and the users from the repository when the backup PVC is gone", func() {
			By("deleting the backup PVC too")
			cmd := exec.Command("kubectl", "delete", "pvc", s3BackupPVC, "-n", namespace, "--timeout=2m")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pvc", s3PrimaryPVC, s3BackupPVC, "-n", namespace,
					"-o", "name", "--ignore-not-found")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(BeEmpty())
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("re-applying the identical Instance CR")
			applyManifest()
			waitForS3Instance()

			By("having restored the backup volume from the repository")
			cmd = exec.Command("kubectl", "logs", s3PodName, "-n", namespace, "-c", "backup-restore")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("Restoring the backups from the S3 repository"))

			By("finding the marker row as the regular user, with its retained Secret's password")
			password, err := getSecretField(s3UserSecret, "password")
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				output, err := s3Login(s3UserKey, password, "SELECT ID FROM MARKER;")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("42"))
			}, 2*time.Minute, 2*time.Second).Should(Succeed())
		})
	})
}
