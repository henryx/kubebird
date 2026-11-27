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
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

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
	s3ResticPod        = s3InstanceName + "-restic-check"

	// s3ResticImage must match the operator's default restic image
	// (defaultResticImage in internal/controller).
	s3ResticImage = "restic/restic:0.19.1"
)

// s3Restic runs restic with args against the e2e-s3-backup Instance's
// repository (see s3ResticFor).
func s3Restic(args ...string) (string, error) {
	return s3ResticFor(s3InstanceName, args...)
}

// s3ResticFor runs restic with args against instance's repository, from a
// throwaway Pod mounting the Instance's repository password Secret and its
// "<instance>-s3-credentials" Secret the same way Kubebird's own restic
// containers do, and returns its output once it exits.
func s3ResticFor(instance string, args ...string) (string, error) {
	credentials := instance + "-s3-credentials"
	overrides, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"restartPolicy": "Never",
			"containers": []map[string]any{{
				"name":  "restic",
				"image": s3ResticImage,
				"args": append([]string{"--password-file", "/etc/restic/password", "-o", "s3.bucket-lookup=path"},
					args...),
				"env": []map[string]any{
					{"name": "RESTIC_REPOSITORY",
						"value": fmt.Sprintf("s3:%s/%s/%s/%s", siloEndpoint, s3Bucket, namespace, instance)},
					{"name": "AWS_ACCESS_KEY_ID", "valueFrom": map[string]any{
						"secretKeyRef": map[string]any{"name": credentials, "key": "AWS_ACCESS_KEY_ID"}}},
					{"name": "AWS_SECRET_ACCESS_KEY", "valueFrom": map[string]any{
						"secretKeyRef": map[string]any{"name": credentials, "key": "AWS_SECRET_ACCESS_KEY"}}},
				},
				"volumeMounts": []map[string]any{{"name": "password", "mountPath": "/etc/restic", "readOnly": true}},
			}},
			"volumes": []map[string]any{{"name": "password", "secret": map[string]any{"secretName": instance + "-restic"}}},
		},
	})
	if err != nil {
		return "", err
	}
	cmd := exec.Command("kubectl", "run", s3ResticPod, "--restart=Never", "-n", namespace,
		"--image="+s3ResticImage, "--overrides", string(overrides))
	if _, err := utils.Run(cmd); err != nil {
		return "", err
	}
	defer func() {
		cmd := exec.Command("kubectl", "delete", "pod", s3ResticPod, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	}()

	var phase string
	deadline := time.Now().Add(3 * time.Minute)
	for phase != "Succeeded" && phase != "Failed" {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("restic %v didn't finish in time (phase %q)", args, phase)
		}
		time.Sleep(2 * time.Second)
		cmd := exec.Command("kubectl", "get", "pod", s3ResticPod, "-n", namespace, "-o", "jsonpath={.status.phase}")
		if phase, err = utils.Run(cmd); err != nil {
			return "", err
		}
	}
	cmd = exec.Command("kubectl", "logs", s3ResticPod, "-n", namespace)
	output, err := utils.Run(cmd)
	if err != nil {
		return "", err
	}
	if phase == "Failed" {
		return output, fmt.Errorf("restic %v failed: %s", args, output)
	}
	return output, nil
}

// s3Snapshots returns the time of every snapshot tagged tag in the
// e2e-s3-backup Instance's repository.
func s3Snapshots(tag string) ([]string, error) {
	snapshots, err := s3SnapshotsFor(s3InstanceName, tag)
	if err != nil {
		return nil, err
	}
	times := make([]string, len(snapshots))
	for i, s := range snapshots {
		times[i] = s.Time
	}
	return times, nil
}

// s3Snapshot is a snapshot, as "restic snapshots --json" lists it.
type s3Snapshot struct {
	Time  string   `json:"time"`
	Paths []string `json:"paths"`
	Tags  []string `json:"tags"`
}

// s3SnapshotsFor returns every snapshot tagged tag in instance's
// repository.
func s3SnapshotsFor(instance, tag string) ([]s3Snapshot, error) {
	output, err := s3ResticFor(instance, "snapshots", "--tag", tag, "--json")
	if err != nil {
		return nil, err
	}
	var snapshots []s3Snapshot
	if err := json.Unmarshal([]byte(output[strings.Index(output, "["):]), &snapshots); err != nil {
		return nil, fmt.Errorf("unexpected restic snapshots output %q: %w", output, err)
	}
	return snapshots, nil
}

// applyS3Manifest applies the Instance manifest.
func applyS3Manifest(manifest string) {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
}

// s3Login connects to the e2e-s3-backup Instance's database through its
// own server as user, returning isql's output.
func s3Login(user, password, sql string) (string, error) {
	return s3LoginTo(s3InstanceName, user, password, sql)
}

// s3LoginTo connects to instance's s3DatabaseName database through its own
// server as user, returning isql's output.
func s3LoginTo(instance, user, password, sql string) (string, error) {
	cmd := exec.Command("kubectl", "exec", "-i", instance+"-0", "-n", namespace, "-c", firebirdContainer,
		"--", "isql", "-q", "-user", user, "-password", password, "localhost:/var/lib/firebird/data/"+s3DatabaseName)
	cmd.Stdin = strings.NewReader("SET BAIL ON;\nSET LIST ON;\n" + sql + "\nQUIT;\n")
	return utils.Run(cmd)
}

// waitForS3Instance waits until the e2e-s3-backup Instance has its
// database and user, without a reconcile error.
func waitForS3Instance() {
	waitForS3InstanceNamed(s3InstanceName)
}

// waitForS3InstanceNamed waits until instance has its s3DatabaseName
// database and s3UserKey user, without a reconcile error.
func waitForS3InstanceNamed(instance string) {
	Eventually(func(g Gomega) {
		for path, want := range map[string]string{
			"{.status.databases[*]}":  s3DatabaseName,
			"{.status.users[*].name}": s3UserKey,
			"{.status.phase}":         "Ready",
			"{.status.error}":         "",
		} {
			cmd := exec.Command("kubectl", "get", "instance", instance, "-n", namespace, "-o", "jsonpath="+path)
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal(want), path)
		}
	}, 6*time.Minute, 2*time.Second).Should(Succeed())
}

// instanceS3BackupSpecs exercises the s3 backup destination against Silo,
// the S3-compatible object store the suite deploys into the Kind cluster
// (see setupSilo): a scheduled backup Job uploads its backup into the
// restic repository and forgets an expired snapshot, deleting the Instance
// uploads the delete-time backup, and, after the backup PVC is deleted too,
// a recreated Instance restores its database and users from the repository
// alone.
//
// It must be called from inside the "Manager" Ordered Describe in
// e2e_test.go, before that Describe's AfterAll tears down the manager.
func instanceS3BackupSpecs() {
	manifest := fmt.Sprintf(`
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
`, s3InstanceName, namespace, firebirdVersion, s3DatabaseName, s3UserKey, s3UserKey, siloEndpoint, s3Bucket,
		s3CredentialSecret)

	Context("Instance s3 backup destination", Ordered, func() {
		BeforeAll(func() {
			By("creating the S3 credentials Secret")
			cmd := exec.Command("kubectl", "create", "secret", "generic", s3CredentialSecret, "-n", namespace,
				"--from-literal=AWS_ACCESS_KEY_ID="+siloAccessKey,
				"--from-literal=AWS_SECRET_ACCESS_KEY="+siloSecretKey)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
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
		})

		It("should upload a scheduled backup into the restic repository", func() {
			By("applying the Instance CR")
			applyS3Manifest(manifest)
			waitForS3Instance()

			By("generating the restic repository password Secret")
			password, err := getSecretField(s3ResticSecret, "password")
			Expect(err).NotTo(HaveOccurred())
			Expect(password).NotTo(BeEmpty())

			By("planting an hourly snapshot older than the 2h retention")
			_, err = s3Restic("backup", "--host", s3InstanceName, "--tag", "hour",
				"--time", "2000-01-01 00:00:00", "/etc/hostname")
			Expect(err).NotTo(HaveOccurred())

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

			By("forgetting the expired snapshot, keeping only the new one")
			snapshots, err := s3Snapshots("hour")
			Expect(err).NotTo(HaveOccurred())
			Expect(snapshots).To(HaveLen(1))
			Expect(snapshots[0]).NotTo(HavePrefix("2000-"))

			By("tagging the snapshot with the Firebird version")
			tagged, err := s3SnapshotsFor(s3InstanceName, "hour")
			Expect(err).NotTo(HaveOccurred())
			Expect(tagged[0].Tags).To(ConsistOf("hour", "firebird:"+firebirdVersion))

			By("finding the backup in the repository's latest hourly snapshot")
			output, err := s3Restic("ls", "latest", "--tag", "hour")
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
			output, err := s3Restic("ls", "latest", "--tag", "base")
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
			applyS3Manifest(manifest)
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

const (
	s3StreamingInstanceName = "e2e-s3-streaming"
	s3StreamingPodName      = s3StreamingInstanceName + "-0"
	s3StreamingCronJob      = s3StreamingInstanceName + "-backup-hour"
	s3StreamingCredentials  = s3StreamingInstanceName + "-s3-credentials"

	// s3RestoredMarker must match the operator's own (s3RestoredMarker in
	// internal/controller).
	s3RestoredMarker = "/var/lib/firebird/data/.kubebird-s3-restored"
)

// s3StreamingLogs returns the logs of container in the streaming
// Instance's pod.
func s3StreamingLogs(container string) string {
	cmd := exec.Command("kubectl", "logs", s3StreamingPodName, "-n", namespace, "-c", container)
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
	return output
}

// s3StreamingFileExists reports whether file exists in the streaming
// Instance's firebird container.
func s3StreamingFileExists(file string) bool {
	cmd := exec.Command("kubectl", "exec", s3StreamingPodName, "-n", namespace, "-c", firebirdContainer,
		"--", "test", "-e", file)
	_, err := utils.Run(cmd)
	return err == nil
}

// instanceS3StreamingBackupSpecs exercises an s3 destination without a
// local one, against the same Silo as instanceS3BackupSpecs: no backup PVC
// is ever created, a scheduled backup Job streams one snapshot per
// database into the restic repository and forgets an expired one,
// deleting the Instance streams the delete-time backup, and a recreated
// Instance restores its database and users from the repository, then
// doesn't restore again when its pod restarts.
//
// It must be called from inside the "Manager" Ordered Describe in
// e2e_test.go, before that Describe's AfterAll tears down the manager.
func instanceS3StreamingBackupSpecs() {
	manifest := fmt.Sprintf(`
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
      - s3:
          endpoint: %s
          bucket: %s
          forcePathStyle: true
          credentials:
            secretRef: %s
    retention:
      hour: "2h"
`, s3StreamingInstanceName, namespace, firebirdVersion, s3DatabaseName, s3UserKey, s3UserKey, siloEndpoint, s3Bucket,
		s3StreamingCredentials)
	userSecret := s3StreamingInstanceName + "-user-s3user"
	hourBackup := "/var/lib/firebird/backup/hour/" + strings.TrimSuffix(s3DatabaseName, ".fdb") + ".fbk"

	Context("Instance s3 destination without a local one", Ordered, func() {
		BeforeAll(func() {
			By("creating the S3 credentials Secret")
			cmd := exec.Command("kubectl", "create", "secret", "generic", s3StreamingCredentials, "-n", namespace,
				"--from-literal=AWS_ACCESS_KEY_ID="+siloAccessKey,
				"--from-literal=AWS_SECRET_ACCESS_KEY="+siloSecretKey)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			By("deleting the e2e-s3-streaming Instance, its PVC and Secrets")
			cmd := exec.Command("kubectl", "delete", "instance", s3StreamingInstanceName, "-n", namespace,
				"--ignore-not-found", "--timeout=3m")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pvc", s3StreamingInstanceName+"-primary", "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "secret", s3StreamingInstanceName+"-user-sysdba", userSecret,
				s3StreamingInstanceName+"-restic", s3StreamingCredentials, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should stream a scheduled backup into the restic repository, without a backup PVC", func() {
			By("applying the Instance CR")
			applyS3Manifest(manifest)
			waitForS3InstanceNamed(s3StreamingInstanceName)

			By("never creating a backup PVC")
			cmd := exec.Command("kubectl", "get", "pvc", s3StreamingInstanceName+"-backup", "-n", namespace,
				"-o", "name", "--ignore-not-found")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(BeEmpty())

			By("finding nothing to restore from the empty repository, then marking the primary volume restored")
			Expect(s3StreamingLogs("backup-restore")).To(ContainSubstring("holds no backup to restore"))
			Eventually(func() bool { return s3StreamingFileExists(s3RestoredMarker) }, time.Minute, 2*time.Second).Should(BeTrue())

			By("creating a marker table as the regular user, the database's owner")
			password, err := getSecretField(userSecret, "password")
			Expect(err).NotTo(HaveOccurred())
			_, err = s3LoginTo(s3StreamingInstanceName, s3UserKey, password,
				"CREATE TABLE MARKER (ID INTEGER);\nCOMMIT;\nINSERT INTO MARKER VALUES (42);\nCOMMIT;")
			Expect(err).NotTo(HaveOccurred())

			By("planting a snapshot of the database's hourly backup older than the 2h retention")
			_, err = s3ResticFor(s3StreamingInstanceName, "backup", "--host", s3StreamingInstanceName, "--tag", "hour",
				"--time", "2000-01-01 00:00:00", "--stdin-filename", hourBackup, "--stdin-from-command", "--", "echo", "old")
			Expect(err).NotTo(HaveOccurred())

			By("running the hourly CronJob's streaming backup")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "cronjob", s3StreamingCronJob, "-n", namespace,
					"-o", "jsonpath={.spec.jobTemplate.spec.template.spec.initContainers[*].name}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("restic-install"))
			}, 3*time.Minute, 2*time.Second).Should(Succeed())
			const manualJob = s3StreamingCronJob + "-manual"
			cmd = exec.Command("kubectl", "create", "job", manualJob, "--from=cronjob/"+s3StreamingCronJob, "-n", namespace)
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

			By("forgetting the expired snapshot, keeping only the new one of the database's backup")
			snapshots, err := s3SnapshotsFor(s3StreamingInstanceName, "hour")
			Expect(err).NotTo(HaveOccurred())
			Expect(snapshots).To(HaveLen(1))
			Expect(snapshots[0].Time).NotTo(HavePrefix("2000-"))
			Expect(snapshots[0].Paths).To(Equal([]string{hourBackup}))
			Expect(snapshots[0].Tags).To(ContainElements("hour", "firebird:"+firebirdVersion))
			Expect(snapshots[0].Tags).To(ContainElement(MatchRegexp(`^run:[0-9]{8}T[0-9]{6}Z$`)))

			By("having streamed the gbak backup itself, not the planted one")
			output, err = s3ResticFor(s3StreamingInstanceName, "dump", "latest", "--tag", "hour", hourBackup)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(output)).To(BeNumerically(">", 500))
		})

		It("should stream the delete-time backup", func() {
			By("deleting the Instance")
			cmd := exec.Command("kubectl", "delete", "instance", s3StreamingInstanceName, "-n", namespace, "--timeout=5m")
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("finding a base snapshot of the database and one of the security database")
			snapshots, err := s3SnapshotsFor(s3StreamingInstanceName, "base")
			Expect(err).NotTo(HaveOccurred())
			var paths []string
			runs := map[string]bool{}
			for _, s := range snapshots {
				paths = append(paths, s.Paths...)
				Expect(s.Tags).To(ContainElements("base", "firebird:"+firebirdVersion))
				for _, tag := range s.Tags {
					if strings.HasPrefix(tag, "run:") {
						runs[tag] = true
					}
				}
			}
			Expect(runs).To(HaveLen(1), "every base snapshot of the deletion shares its run tag")
			Expect(paths).To(ConsistOf(
				"/var/lib/firebird/backup/base/"+strings.TrimSuffix(s3DatabaseName, ".fdb")+".fbk",
				"/var/lib/firebird/backup/base/"+securityDatabaseFile(".fbk"),
			))
		})

		It("should restore the database and the users from the repository, and only once", func() {
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pvc", s3StreamingInstanceName+"-primary", "-n", namespace,
					"-o", "name", "--ignore-not-found")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(BeEmpty())
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("re-applying the identical Instance CR")
			applyS3Manifest(manifest)
			waitForS3InstanceNamed(s3StreamingInstanceName)
			Expect(s3StreamingLogs("backup-restore")).To(ContainSubstring("Restoring the backups from the S3 repository"))

			By("finding the marker row as the regular user, with its retained Secret's password")
			password, err := getSecretField(userSecret, "password")
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				output, err := s3LoginTo(s3StreamingInstanceName, s3UserKey, password, "SELECT ID FROM MARKER;")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("42"))
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("deleting the restored backups and marking the primary volume restored")
			Eventually(func(g Gomega) {
				g.Expect(s3StreamingFileExists(s3RestoredMarker)).To(BeTrue())
				g.Expect(s3StreamingFileExists("/var/lib/firebird/backup/base")).To(BeFalse())
			}, time.Minute, 2*time.Second).Should(Succeed())

			By("not restoring again when the pod restarts")
			cmd := exec.Command("kubectl", "delete", "pod", s3StreamingPodName, "-n", namespace, "--timeout=2m")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", s3StreamingPodName, "-n", namespace,
					"-o", "jsonpath={.status.containerStatuses[0].ready}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("true"))
			}, 3*time.Minute, 2*time.Second).Should(Succeed())
			Expect(s3StreamingLogs("backup-restore")).To(ContainSubstring("already restored"))
			output, err := s3LoginTo(s3StreamingInstanceName, s3UserKey, password, "SELECT ID FROM MARKER;")
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("42"))
		})
	})
}
