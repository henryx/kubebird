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
	incrementalBackupDBPath = "/var/lib/firebird/data/incremental.fdb"
	// incrementalBackupDir mirrors scheduledBackupDir(backupFrequencyHour)
	// in internal/controller/instance_backup_schedule.go.
	incrementalBackupDir = "/var/lib/firebird/backup/hour"
	// incrementalBackupExpiredPool names a pool far older than the 2h
	// retention window, as renamed by the test to expire the current one.
	incrementalBackupExpiredPool = "pool-20000101T000000Z"
)

// instanceScheduledBackupIncrementalSpecs exercises spec.backup.mode's
// physical, incremental scheduled backups end to end: the first manually
// triggered run of the hourly CronJob starts a pool with a level 0
// nbackup, the second adds a level 1 nbackup to the same pool, and the
// two restore together (nbackup -R) to the database as of the second
// run; once the pool's level 0 is older than the retention window, the
// next run starts a new pool with a new level 0 and deletes the old one,
// pruning the database's backup history down to that new level 0. It runs
// the given Firebird version, which must be 4.0 or later, since 3.0 can't
// prune the backup history at all (see canCleanBackupHistory in
// internal/controller/instance_backup_schedule.go), in an Instance named
// after its major version ("e2e-incremental-backup-<major>"), so it can be
// called once per version. As in instanceScheduledBackupSpecs, it never waits on the CronJob's real
// schedule, creating Jobs "--from=cronjob" instead.
//
// It must be called from inside the "Manager" Ordered Describe in
// e2e_test.go, after the CRDs are installed and the controller-manager is
// deployed, and before that Describe's AfterAll tears them down.
func instanceScheduledBackupIncrementalSpecs(version string) {
	major, _, _ := strings.Cut(version, ".")
	incrementalBackupInstanceName := "e2e-incremental-backup-" + major
	incrementalBackupPodName := incrementalBackupInstanceName + "-0"
	incrementalBackupSecretName := incrementalBackupInstanceName + "-sysdba"
	incrementalBackupCronJob := incrementalBackupInstanceName + "-backup-hour"

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
    - name: incremental.fdb
  storage:
    primary:
      size: 1Gi
  backup:
    enabled: true
    backupOnDelete: false
    mode:
      type: physical
      incremental: true
    destinations:
      - local:
          storage:
            size: 1Gi
    retention:
      hour: "2h"
`, incrementalBackupInstanceName, namespace, version)

	podExec := func(script string) (string, error) {
		cmd := exec.Command("kubectl", "exec", incrementalBackupPodName, "-n", namespace, "-c", firebirdContainer,
			"--", "sh", "-c", script)
		return utils.Run(cmd)
	}

	runIsql := func(dbPath, script string) (string, error) {
		password, err := getSecretField(incrementalBackupSecretName, "password")
		if err != nil {
			return "", err
		}
		cmd := exec.Command("kubectl", "exec", "-i", incrementalBackupPodName, "-n", namespace, "-c", firebirdContainer,
			"--", "isql", "-q", "-user", "SYSDBA", "-password", password, dbPath)
		cmd.Stdin = strings.NewReader(script)
		return utils.Run(cmd)
	}

	// listPools returns every pool directory in the hourly backup
	// directory, mapped to the files it holds.
	listPools := func() map[string][]string {
		output, err := podExec(fmt.Sprintf(
			`cd %s && for p in pool-*; do [ -d "$p" ] || continue; echo "$p:$(ls -1 "$p" | tr '\n' ' ')"; done`,
			incrementalBackupDir))
		Expect(err).NotTo(HaveOccurred())
		pools := map[string][]string{}
		for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
			if name, files, ok := strings.Cut(line, ":"); ok {
				pools[name] = strings.Fields(files)
			}
		}
		return pools
	}

	backupHistory := func() []string {
		output, err := runIsql(incrementalBackupDBPath,
			"SET HEADING OFF;\nSELECT RDB$BACKUP_LEVEL || ' ' || RDB$FILE_NAME FROM RDB$BACKUP_HISTORY ORDER BY RDB$BACKUP_ID;\n")
		Expect(err).NotTo(HaveOccurred())
		var rows []string
		for _, line := range strings.Split(output, "\n") {
			if line = strings.Join(strings.Fields(line), " "); line != "" {
				rows = append(rows, line)
			}
		}
		return rows
	}

	runBackupJob := func(jobName string) {
		cmd := exec.Command("kubectl", "create", "job", jobName, "--from=cronjob/"+incrementalBackupCronJob, "-n", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			cmd := exec.Command("kubectl", "delete", "job", jobName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "job", jobName, "-n", namespace, "-o", "jsonpath={.status.succeeded}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("1"))
		}, 3*time.Minute, 2*time.Second).Should(Succeed())
	}

	Context("Instance physical incremental scheduled backups (spec.backup.mode) on Firebird "+version, Ordered, func() {
		var firstPool string

		AfterAll(func() {
			By("deleting the " + incrementalBackupInstanceName + " Instance, if it still exists")
			cmd := exec.Command("kubectl", "delete", "instance", incrementalBackupInstanceName,
				"-n", namespace, "--ignore-not-found", "--timeout=2m")
			_, _ = utils.Run(cmd)
		})

		It("should deploy and create the hourly backup CronJob", func() {
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "cronjob", incrementalBackupCronJob, "-n", namespace,
					"-o", "jsonpath={.spec.schedule}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("0 * * * *"))
			}, 5*time.Minute, 2*time.Second).Should(Succeed())

			By("re-rendering the CronJob to prune the backup history once the server's version is detected")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "cronjob", incrementalBackupCronJob, "-n", namespace,
					"-o", "jsonpath={.spec.jobTemplate.spec.template.spec.containers[0].command[2]}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("-nbk_clean_history -nbk_keep_rows 1"))
			}, 3*time.Minute, 2*time.Second).Should(Succeed())

			By("creating a marker table holding a first row")
			_, err = runIsql(incrementalBackupDBPath, "CREATE TABLE marker (id INTEGER);\nCOMMIT;\nINSERT INTO marker VALUES (1);\nCOMMIT;\n")
			Expect(err).NotTo(HaveOccurred())
		})

		It("should start a pool with a level 0 backup, then add a level 1 backup to it", func() {
			By("running the first backup Job")
			runBackupJob(incrementalBackupCronJob + "-manual-1")
			pools := listPools()
			Expect(pools).To(HaveLen(1))
			for name, files := range pools {
				firstPool = name
				Expect(files).To(ConsistOf("incremental-level0.nbk"))
			}

			By("adding a second row, then running the second backup Job")
			_, err := runIsql(incrementalBackupDBPath, "INSERT INTO marker VALUES (2);\nCOMMIT;\n")
			Expect(err).NotTo(HaveOccurred())
			runBackupJob(incrementalBackupCronJob + "-manual-2")
			pools = listPools()
			Expect(pools).To(HaveKey(firstPool))
			Expect(pools).To(HaveLen(1))
			Expect(pools[firstPool]).To(HaveLen(2))
			Expect(pools[firstPool]).To(ContainElement("incremental-level0.nbk"))
			Expect(pools[firstPool]).To(ContainElement(HavePrefix("incremental-level1-")))
			Expect(backupHistory()).To(HaveLen(2), "a level 1 backup must not prune the backup history")

			By("never putting the database in physical backup mode for longer than a run")
			output, err := runIsql(incrementalBackupDBPath, "SET HEADING OFF;\nSELECT MON$BACKUP_STATE FROM MON$DATABASE;\n")
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(output)).To(Equal("0"))

			By("restoring the level 0 and level 1 backups together to the database as of the second run")
			var level1 string
			for _, f := range pools[firstPool] {
				if strings.HasPrefix(f, "incremental-level1-") {
					level1 = f
				}
			}
			pool := incrementalBackupDir + "/" + firstPool
			_, err = podExec(fmt.Sprintf("rm -f /tmp/restored.fdb && nbackup -R /tmp/restored.fdb %s/incremental-level0.nbk %s/%s",
				pool, pool, level1))
			Expect(err).NotTo(HaveOccurred())
			output, err = runIsql("/tmp/restored.fdb", "SET HEADING OFF;\nSELECT COUNT(*) FROM marker;\n")
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(output)).To(Equal("2"))
			_, _ = podExec("rm -f /tmp/restored.fdb")
		})

		It("should start a new pool and delete the old one once its level 0 is older than the retention", func() {
			By("making the current pool look older than the 2h retention window")
			_, err := podExec(fmt.Sprintf("mv %[1]s/%[2]s %[1]s/%[3]s", incrementalBackupDir, firstPool, incrementalBackupExpiredPool))
			Expect(err).NotTo(HaveOccurred())

			By("running a third backup Job")
			runBackupJob(incrementalBackupCronJob + "-manual-3")
			pools := listPools()
			Expect(pools).NotTo(HaveKey(incrementalBackupExpiredPool))
			Expect(pools).To(HaveLen(1))
			var newPool string
			for name, files := range pools {
				newPool = name
				Expect(files).To(ConsistOf("incremental-level0.nbk"))
			}

			By("pruning the backup history down to the new pool's level 0")
			Expect(backupHistory()).To(Equal([]string{
				"0 " + incrementalBackupDir + "/" + newPool + "/incremental-level0.nbk",
			}))

			By("not reporting any reconcile error")
			cmd := exec.Command("kubectl", "get", "instance", incrementalBackupInstanceName, "-n", namespace,
				"-o", "jsonpath={.status.error}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(BeEmpty())
		})
	})
}
