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
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/henryx/kubebird/test/utils"
)

const (
	scheduledBackupInstanceName = "e2e-scheduled-backup"
	scheduledBackupPodName      = scheduledBackupInstanceName + "-0"
	scheduledBackupCronJob      = scheduledBackupInstanceName + "-backup-hour"
	scheduledBackupDatabaseName = "scheduled.fdb"
	// scheduledBackupDir mirrors scheduledBackupDir(backupFrequencyHour)
	// in internal/controller/instance_backup_schedule.go.
	scheduledBackupDir = "/var/lib/firebird/backup/hour"
)

// instanceScheduledBackupSpecs exercises spec.backup.retention's
// CronJob-driven scheduled backups end to end: the hourly CronJob gets
// created (and its backup directory pre-created on the live pod) once the
// Instance is ready, a manually-triggered run of that CronJob backs up the
// database (but deliberately not the security database — see
// scheduledBackupScript in internal/controller/instance_backup_schedule.go
// for why) via fbsvcmgr's remote nbackup action and gzips the result
// (spec.backup.compress), a second run lands alongside the first (each
// file is named after its run's own UTC timestamp) while a backup older
// than the retention window is pruned, and unsetting
// spec.backup.retention.hour removes the CronJob again.
//
// The CronJob Kubebird creates only fires on its own real schedule ("0 * *
// * *" for hourly, the fastest of the five), far too slow for an e2e run —
// so rather than waiting on it, this creates a Job "--from=cronjob" to run
// it immediately, the same way an operator would force an out-of-band
// backup.
//
// It must be called from inside the "Manager" Ordered Describe in
// e2e_test.go, after the CRDs are installed and the controller-manager is
// deployed, and before that Describe's AfterAll tears them down.
func instanceScheduledBackupSpecs() {
	manifest := func(hourRetention string) string {
		retention := ""
		if hourRetention != "" {
			retention = fmt.Sprintf("    retention:\n      hour: %q\n", hourRetention)
		}
		return fmt.Sprintf(`
apiVersion: kubebird.github.io/v1
kind: Instance
metadata:
  name: %s
  namespace: %s
spec:
  image: firebirdsql/firebird
  version: 3.0.14
  databases:
    - name: %q
  storage:
    primary:
      size: 1Gi
  backup:
    enabled: true
    compress: true
    destinations:
      - local:
          storage:
            size: 1Gi
%s`, scheduledBackupInstanceName, namespace, scheduledBackupDatabaseName, retention)
	}

	listBackupFiles := func() []string {
		cmd := exec.Command("kubectl", "exec", scheduledBackupPodName, "-n", namespace, "-c", firebirdContainer,
			"--", "sh", "-c", fmt.Sprintf("ls -1 %s", scheduledBackupDir))
		output, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		return strings.Fields(output)
	}

	Context("Instance scheduled backups (spec.backup.retention)", Ordered, func() {
		AfterAll(func() {
			By("deleting the e2e-scheduled-backup Instance, if it still exists")
			cmd := exec.Command("kubectl", "delete", "instance", scheduledBackupInstanceName,
				"-n", namespace, "--ignore-not-found", "--timeout=2m")
			_, _ = utils.Run(cmd)
		})

		It("should deploy with hourly retention enabled and create the hourly backup CronJob", func() {
			By("applying an Instance with a local backup volume and spec.backup.retention.hour set")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest("2h"))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the database to be provisioned")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "instance", scheduledBackupInstanceName, "-n", namespace,
					"-o", "jsonpath={.status.databases[*]}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.Fields(output)).To(ConsistOf(scheduledBackupDatabaseName))
			}, 5*time.Minute, 2*time.Second).Should(Succeed())

			By("creating the hourly backup CronJob once the pod is ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "cronjob", scheduledBackupCronJob, "-n", namespace,
					"-o", "jsonpath={.spec.schedule}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("0 * * * *"))
			}, 3*time.Minute, 2*time.Second).Should(Succeed())

			By("skipping a run rather than overlapping one still in flight")
			cmd = exec.Command("kubectl", "get", "cronjob", scheduledBackupCronJob, "-n", namespace,
				"-o", "jsonpath={.spec.concurrencyPolicy}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("Forbid"))

			By("having pre-created the hourly backup directory on the instance's own pod")
			cmd = exec.Command("kubectl", "exec", scheduledBackupPodName, "-n", namespace, "-c", firebirdContainer,
				"--", "test", "-d", scheduledBackupDir)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should back up the database, but not the security database, when the CronJob is triggered manually", func() {
			By("manually running a Job from the hourly CronJob instead of waiting for its real schedule")
			manualJobName := scheduledBackupCronJob + "-manual"
			cmd := exec.Command("kubectl", "create", "job", manualJobName,
				"--from=cronjob/"+scheduledBackupCronJob, "-n", namespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				cmd := exec.Command("kubectl", "delete", "job", manualJobName, "-n", namespace, "--ignore-not-found")
				_, _ = utils.Run(cmd)
			}()

			By("waiting for the manually-triggered Job to succeed")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "job", manualJobName, "-n", namespace,
					"-o", "jsonpath={.status.succeeded}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}, 3*time.Minute, 2*time.Second).Should(Succeed())

			By("writing one gzip-compressed backup file for the database via fbsvcmgr's remote nbackup")
			files := listBackupFiles()
			Expect(files).To(HaveLen(1), "expected exactly one backup file, for scheduled.fdb")
			Expect(files[0]).To(HavePrefix("scheduled-"))
			Expect(files[0]).To(HaveSuffix(".nbk.gz"), "expected the Job itself to gzip the backup, since spec.backup.compress is true")

			By("never attempting the security database, since it can't be nbackup'd while the server has it open")
			for _, f := range files {
				Expect(f).NotTo(HavePrefix("security"))
			}

			By("not reporting any reconcile error")
			cmd = exec.Command("kubectl", "get", "instance", scheduledBackupInstanceName, "-n", namespace,
				"-o", "jsonpath={.status.error}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(BeEmpty())
		})

		It("should keep a second run alongside the first and prune backups older than the retention window", func() {
			const expiredBackup = "scheduled-20000101T000000Z.nbk.gz"

			By("planting a backup file far older than the 2h retention window")
			cmd := exec.Command("kubectl", "exec", scheduledBackupPodName, "-n", namespace, "-c", firebirdContainer,
				"--", "touch", scheduledBackupDir+"/"+expiredBackup)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("manually running a second Job from the hourly CronJob")
			manualJobName := scheduledBackupCronJob + "-manual-2"
			cmd = exec.Command("kubectl", "create", "job", manualJobName,
				"--from=cronjob/"+scheduledBackupCronJob, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			defer func() {
				cmd := exec.Command("kubectl", "delete", "job", manualJobName, "-n", namespace, "--ignore-not-found")
				_, _ = utils.Run(cmd)
			}()

			By("waiting for the second Job to succeed instead of colliding with the first run's file, pruning as it goes")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "job", manualJobName, "-n", namespace,
					"-o", "jsonpath={.status.succeeded}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}, 3*time.Minute, 2*time.Second).Should(Succeed())

			By("keeping both recent backups, compressed, while the Job itself deleted the expired one")
			Eventually(func(g Gomega) {
				files := listBackupFiles()
				g.Expect(files).To(HaveLen(2))
				g.Expect(files).NotTo(ContainElement(expiredBackup))
				for _, f := range files {
					g.Expect(f).To(HavePrefix("scheduled-"))
					g.Expect(f).To(HaveSuffix(".nbk.gz"))
				}
			}, 2*time.Minute, 2*time.Second).Should(Succeed())
		})

		It("should delete the hourly CronJob once retention is disabled", func() {
			By("re-applying the Instance with spec.backup.retention.hour back at its default (unset)")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest(""))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("removing the hourly backup CronJob")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "cronjob", scheduledBackupCronJob, "-n", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "the hourly backup CronJob should have been deleted")
			}, 2*time.Minute, 2*time.Second).Should(Succeed())
		})
	})
}
