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
	concurrentBackupInstanceName = "e2e-concurrent-backup"
	concurrentBackupPodName      = concurrentBackupInstanceName + "-0"
	concurrentBackupSecretName   = concurrentBackupInstanceName + "-sysdba"
	concurrentBackupDatabaseName = "concurrent.fdb"
	concurrentBackupDBPath       = "/var/lib/firebird/data/" + concurrentBackupDatabaseName
	// concurrentBackupDBConn reaches the database through the live server.
	// A bare path would make isql open the file with its own embedded
	// engine, which can't share it with the server: while the writer holds
	// it that way, the server (and so every backup Job) fails with
	// "Database already opened with engine instance, incompatible with
	// current".
	concurrentBackupDBConn = "localhost:" + concurrentBackupDBPath
	// concurrentBackupRoot mirrors backupDataMountPath in
	// internal/controller.
	concurrentBackupRoot = "/var/lib/firebird/backup"
	// concurrentBackupWriterPIDFile is where the writer process started
	// in the Firebird pod records its PID, so it can be stopped.
	concurrentBackupWriterPIDFile = "/tmp/kubebird-e2e-writer.pid"
	// concurrentBackupExtraHourlyRuns is how many additional Jobs are
	// created from the hourly CronJob on top of one per frequency — runs of
	// the same frequency that ConcurrencyPolicy Forbid can't keep apart
	// (it only applies between a CronJob's own scheduled runs), and that
	// would otherwise pick the same timestamped file name.
	concurrentBackupExtraHourlyRuns = 2
)

// concurrentBackupFrequencies are every spec.backup.retention frequency,
// matching backupFrequencies in internal/controller.
var concurrentBackupFrequencies = []string{"hour", "day", "week", "month", "year"}

// instanceScheduledBackupConcurrencySpecs exercises spec.backup.retention's
// scheduled backups running at the same moment — e.g. the hourly and
// daily CronJobs both firing at 00:00 UTC, or all five on January 1st.
// Each frequency has its own CronJob, and ConcurrencyPolicy Forbid only
// keeps a CronJob from overlapping with itself, so their fbsvcmgr
// -action_backup calls run against the same database at the same time.
// gbak backups, unlike the nbackup-based -action_nbak Kubebird 0.3.1 and
// earlier used, don't conflict with each other, so nothing serializes
// them.
//
// One Job is created from every frequency's CronJob plus extra ones from
// the hourly CronJob, all at once, while a process in the Firebird pod
// keeps committing rows to the database: every Job must succeed with its
// own backup file (same-frequency runs starting within the same second
// claim distinct timestamps), the writer must never fail, so the backups
// never lock the database, and every backup must restore.
//
// It must be called from inside the "Manager" Ordered Describe in
// e2e_test.go, after the CRDs are installed and the controller-manager is
// deployed, and before that Describe's AfterAll tears them down.
func instanceScheduledBackupConcurrencySpecs() {
	manifestTemplate := `
apiVersion: kubebird.github.io/v1
kind: Instance
metadata:
  name: %s
  namespace: %s
spec:
  image: firebirdsql/firebird
  version: ` + firebirdVersion + `
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
    retention:
      hour: "2h"
      day: "2d"
      week: "2w"
      month: "2m"
      year: "2y"
`

	podExec := func(script string) (string, error) {
		cmd := exec.Command("kubectl", "exec", concurrentBackupPodName, "-n", namespace, "-c", firebirdContainer,
			"--", "sh", "-c", script)
		return utils.Run(cmd)
	}

	runIsql := func(script string) (string, error) {
		password, err := getSecretField(concurrentBackupSecretName, "password")
		if err != nil {
			return "", err
		}
		cmd := exec.Command("kubectl", "exec", "-i", concurrentBackupPodName, "-n", namespace, "-c", firebirdContainer,
			"--", "isql", "-user", "SYSDBA", "-password", password, concurrentBackupDBConn)
		cmd.Stdin = strings.NewReader(script)
		return utils.Run(cmd)
	}

	listBackupFiles := func(frequency string) []string {
		output, err := podExec(fmt.Sprintf("ls -1 %s/%s", concurrentBackupRoot, frequency))
		Expect(err).NotTo(HaveOccurred())
		return strings.Fields(output)
	}

	// createJobFromCronJob creates a Job from cronJob's template, the same
	// way "kubectl create job --from=cronjob/..." does, but without the
	// owner reference that command adds: an owned Job counts towards the
	// CronJob's SuccessfulJobsHistoryLimit (1), so the CronJob controller
	// would delete all but the latest finished one before they could all
	// be checked.
	createJobFromCronJob := func(job, cronJob string) {
		cmd := exec.Command("kubectl", "create", "job", job, "--from=cronjob/"+cronJob,
			"-n", namespace, "--dry-run=client", "-o", "json")
		output, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		var manifest map[string]any
		Expect(json.Unmarshal([]byte(output), &manifest)).To(Succeed())
		delete(manifest["metadata"].(map[string]any), "ownerReferences")
		detached, err := json.Marshal(manifest)
		Expect(err).NotTo(HaveOccurred())

		cmd = exec.Command("kubectl", "create", "-f", "-")
		cmd.Stdin = strings.NewReader(string(detached))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
	}

	jobField := func(job, jsonpath string) string {
		cmd := exec.Command("kubectl", "get", "job", job, "-n", namespace, "-o", "jsonpath="+jsonpath)
		output, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		return output
	}

	var jobs []string

	Context("Instance scheduled backups running concurrently", Ordered, func() {
		AfterAll(func() {
			By("stopping the writer, if it's still running")
			_, _ = podExec(fmt.Sprintf("[ -f %[1]s ] && kill $(cat %[1]s); rm -f %[1]s", concurrentBackupWriterPIDFile))

			By("deleting the manually-created Jobs")
			for _, job := range jobs {
				cmd := exec.Command("kubectl", "delete", "job", job, "-n", namespace, "--ignore-not-found")
				_, _ = utils.Run(cmd)
			}

			By("deleting the e2e-concurrent-backup Instance, if it still exists")
			cmd := exec.Command("kubectl", "delete", "instance", concurrentBackupInstanceName,
				"-n", namespace, "--ignore-not-found", "--timeout=2m")
			_, _ = utils.Run(cmd)
		})

		It("should create a backup CronJob for every frequency", func() {
			By("applying an Instance with every spec.backup.retention frequency enabled")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(manifestTemplate,
				concurrentBackupInstanceName, namespace, concurrentBackupDatabaseName))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the database to be provisioned")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "instance", concurrentBackupInstanceName, "-n", namespace,
					"-o", "jsonpath={.status.databases[*]}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.Fields(output)).To(ConsistOf(concurrentBackupDatabaseName))
			}, 5*time.Minute, 2*time.Second).Should(Succeed())

			By("waiting for all five backup CronJobs")
			Eventually(func(g Gomega) {
				for _, freq := range concurrentBackupFrequencies {
					cmd := exec.Command("kubectl", "get", "cronjob", concurrentBackupInstanceName+"-backup-"+freq,
						"-n", namespace)
					_, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred(), "missing %s backup CronJob", freq)
				}
			}, 3*time.Minute, 2*time.Second).Should(Succeed())

			By("writing a marker row, to check the backups' content later on")
			_, err = runIsql("CREATE TABLE MARKER (ID INTEGER);\nCREATE TABLE WRITES (ID INTEGER);\nCOMMIT;\n" +
				"INSERT INTO MARKER VALUES (42);\nCOMMIT;\nQUIT;\n")
			Expect(err).NotTo(HaveOccurred())
		})

		It("should run backups started at the same moment side by side without locking the database", func() {
			By("committing a row every half second from a writer in the Firebird pod")
			password, err := getSecretField(concurrentBackupSecretName, "password")
			Expect(err).NotTo(HaveOccurred())
			_, err = podExec(fmt.Sprintf(`echo $$ > %[1]s
exec </dev/null >/dev/null 2>&1
nohup sh -c 'i=0; while :; do i=$((i+1)); echo "INSERT INTO WRITES VALUES ($i); COMMIT;" | isql -q -user SYSDBA -password %[2]q %[3]q || exit 1; sleep 0.5; done' &
echo $! > %[1]s`, concurrentBackupWriterPIDFile, password, concurrentBackupDBConn))
			Expect(err).NotTo(HaveOccurred())

			countWrites := func() int {
				output, err := runIsql("SELECT 'WRITES=' || COUNT(*) FROM WRITES;\nQUIT;\n")
				Expect(err).NotTo(HaveOccurred())
				var n int
				_, err = fmt.Sscanf(output[strings.Index(output, "WRITES=")+len("WRITES="):], "%d", &n)
				Expect(err).NotTo(HaveOccurred(), "parsing %q", output)
				return n
			}
			Eventually(countWrites, time.Minute, time.Second).Should(BeNumerically(">", 0), "the writer never committed")

			By("creating a Job from every frequency's CronJob, plus extra ones from the hourly CronJob, all at once")
			for _, freq := range concurrentBackupFrequencies {
				jobs = append(jobs, fmt.Sprintf("%s-concurrent-%s", concurrentBackupInstanceName, freq))
			}
			for i := 1; i <= concurrentBackupExtraHourlyRuns; i++ {
				jobs = append(jobs, fmt.Sprintf("%s-concurrent-hour-%d", concurrentBackupInstanceName, i))
			}
			for _, job := range jobs {
				// Job names are "<instance>-concurrent-<frequency>[-<n>]".
				freq := strings.SplitN(strings.TrimPrefix(job, concurrentBackupInstanceName+"-concurrent-"), "-", 2)[0]
				createJobFromCronJob(job, concurrentBackupInstanceName+"-backup-"+freq)
			}

			By("waiting for every Job to succeed")
			Eventually(func(g Gomega) {
				for _, job := range jobs {
					cmd := exec.Command("kubectl", "get", "job", job, "-n", namespace,
						"-o", "jsonpath={.status.succeeded}/{.status.failed}")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					if strings.HasSuffix(output, "/1") {
						logs, _ := utils.Run(exec.Command("kubectl", "logs", "job/"+job, "-n", namespace))
						StopTrying(fmt.Sprintf("Job %s failed:\n%s", job, logs)).Now()
					}
					g.Expect(output).To(HavePrefix("1/"), "Job %s hasn't succeeded yet", job)
				}
			}, 5*time.Minute, 2*time.Second).Should(Succeed())
			for _, job := range jobs {
				Expect(jobField(job, "{.status.failed}")).To(BeEmpty(), "Job %s failed", job)
			}

			By("having kept the writer committing throughout")
			before := countWrites()
			Eventually(countWrites, 30*time.Second, time.Second).Should(BeNumerically(">", before),
				"the writer stopped committing")
			_, err = podExec(fmt.Sprintf("kill -0 $(cat %s)", concurrentBackupWriterPIDFile))
			Expect(err).NotTo(HaveOccurred(), "the writer exited, so one of its commits failed")

			// A frequency's real schedule can tick while this runs (the
			// hourly one at every :00), adding its own backup to the
			// directory, so only a lower bound is exact here — a run that
			// collided with another's file name would already have failed
			// its Job above.
			By("writing one distinct, compressed backup per run into each frequency's directory")
			for _, freq := range concurrentBackupFrequencies {
				want := 1
				if freq == "hour" {
					want += concurrentBackupExtraHourlyRuns
				}
				files := listBackupFiles(freq)
				Expect(len(files)).To(BeNumerically(">=", want), "%s backups: %v", freq, files)
				for _, f := range files {
					Expect(f).To(MatchRegexp(`^concurrent-[0-9]{8}T[0-9]{6}Z\.fbk\.gz$`), "%s backup", freq)
				}
			}

			By("never putting the database into physical backup mode")
			output, err := runIsql("SELECT 'STATE=' || MON$BACKUP_STATE FROM MON$DATABASE;\nQUIT;\n")
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("STATE=0"))

			By("not reporting any reconcile error")
			cmd := exec.Command("kubectl", "get", "instance", concurrentBackupInstanceName, "-n", namespace,
				"-o", "jsonpath={.status.error}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(BeEmpty())
		})

		It("should have taken backups that restore to the database's content", func() {
			By("restoring every frequency's backup with gbak into a scratch database")
			password, err := getSecretField(concurrentBackupSecretName, "password")
			Expect(err).NotTo(HaveOccurred())
			for _, freq := range concurrentBackupFrequencies {
				for _, f := range listBackupFiles(freq) {
					output, err := podExec(fmt.Sprintf(`set -e
rm -f /tmp/restore.fbk /tmp/restore.fdb
gunzip -c %[1]q > /tmp/restore.fbk
gbak -create /tmp/restore.fbk /tmp/restore.fdb
echo "SELECT 'MARKER=' || ID FROM MARKER;" | isql -q -user SYSDBA -password %[2]q /tmp/restore.fdb
rm -f /tmp/restore.fbk /tmp/restore.fdb`, concurrentBackupRoot+"/"+freq+"/"+f, password))
					Expect(err).NotTo(HaveOccurred(), "restoring %s/%s", freq, f)
					Expect(output).To(ContainSubstring("MARKER=42"), "restoring %s/%s", freq, f)
				}
			}
		})
	})
}
