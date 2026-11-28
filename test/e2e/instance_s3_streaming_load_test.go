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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/henryx/kubebird/test/utils"
)

const (
	s3LoadInstanceName  = "e2e-s3-load"
	s3LoadPodName       = s3LoadInstanceName + "-0"
	s3LoadSysdbaSecret  = s3LoadInstanceName + "-user-sysdba"
	s3LoadCredentials   = s3LoadInstanceName + "-s3-credentials"
	s3LoadDatabaseName  = "load.fdb"
	s3LoadDBConn        = "localhost:/var/lib/firebird/data/" + s3LoadDatabaseName
	s3LoadWriterPIDFile = "/tmp/kubebird-e2e-s3-writer.pid"
	s3LoadVerifyPod     = s3LoadInstanceName + "-verify"
	// s3LoadBulkRows is how many rows of padding the database gets before
	// the backups start, so each one takes long enough for the writer's
	// commits to land while it's streaming.
	s3LoadBulkRows = 100000
	// s3LoadExtraHourlyRuns is how many Jobs are created from the hourly
	// CronJob on top of one per frequency: same-frequency runs starting
	// within the same second, which must still get distinct run tags.
	s3LoadExtraHourlyRuns = 2
)

// s3LoadSnapshot is a snapshot of the load test's repository.
type s3LoadSnapshot struct {
	ID    string   `json:"short_id"`
	Paths []string `json:"paths"`
	Tags  []string `json:"tags"`
}

// s3LoadCheckPattern matches the line the verify Pod prints for every
// snapshot it restored.
var s3LoadCheckPattern = regexp.MustCompile(`CHECK (\S+) MARKER=(\d+) BULK=(\d+) WRITES=(\d+) MAXW=(\d+)`)

// s3LoadVerify runs script in a throwaway Pod of the Firebird image with
// restic's binary copied in (as Kubebird's own streaming Jobs do), as
// $RESTIC, set up against the load test Instance's repository, and returns
// its output once it succeeds.
func s3LoadVerify(script string) string {
	const bin = "/kubebird-e2e/bin"
	restic := bin + "/restic --password-file /etc/restic/password -o s3.bucket-lookup=path"
	overrides, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"restartPolicy": "Never",
			"initContainers": []map[string]any{{
				"name":         "restic-install",
				"image":        s3ResticImage,
				"command":      []string{"sh", "-c", "cp \"$(command -v restic)\" " + bin + "/restic"},
				"volumeMounts": []map[string]any{{"name": "bin", "mountPath": bin}},
			}},
			"containers": []map[string]any{{
				"name":    "verify",
				"image":   "firebirdsql/firebird:" + firebirdVersion,
				"command": []string{"sh", "-c", "RESTIC=\"" + restic + "\"\n" + script},
				"env": []map[string]any{
					{"name": "RESTIC_REPOSITORY",
						"value": fmt.Sprintf("s3:%s/%s/%s/%s", siloEndpoint, s3Bucket, namespace, s3LoadInstanceName)},
					{"name": "AWS_ACCESS_KEY_ID", "valueFrom": map[string]any{
						"secretKeyRef": map[string]any{"name": s3LoadCredentials, "key": "AWS_ACCESS_KEY_ID"}}},
					{"name": "AWS_SECRET_ACCESS_KEY", "valueFrom": map[string]any{
						"secretKeyRef": map[string]any{"name": s3LoadCredentials, "key": "AWS_SECRET_ACCESS_KEY"}}},
				},
				"volumeMounts": []map[string]any{
					{"name": "bin", "mountPath": bin, "readOnly": true},
					{"name": "password", "mountPath": "/etc/restic", "readOnly": true},
				},
			}},
			"volumes": []map[string]any{
				{"name": "bin", "emptyDir": map[string]any{}},
				{"name": "password", "secret": map[string]any{"secretName": s3LoadInstanceName + "-restic"}},
			},
		},
	})
	Expect(err).NotTo(HaveOccurred())
	cmd := exec.Command("kubectl", "run", s3LoadVerifyPod, "--restart=Never", "-n", namespace,
		"--image=firebirdsql/firebird:"+firebirdVersion, "--overrides", string(overrides))
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
	defer func() {
		cmd := exec.Command("kubectl", "delete", "pod", s3LoadVerifyPod, "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	}()

	var phase string
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "pod", s3LoadVerifyPod, "-n", namespace, "-o", "jsonpath={.status.phase}")
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		phase = output
		g.Expect(phase).To(BeElementOf("Succeeded", "Failed"))
	}, 10*time.Minute, 2*time.Second).Should(Succeed())
	cmd = exec.Command("kubectl", "logs", s3LoadVerifyPod, "-n", namespace, "-c", "verify")
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
	Expect(phase).To(Equal("Succeeded"), "verify Pod failed:\n%s", output)
	return output
}

// instanceS3StreamingLoadSpecs exercises backups streamed to an s3
// destination without a local one while the database is being written
// to, with every frequency's backup started at the same moment (plus
// extra hourly ones, as a Job created by hand next to a scheduled run
// would be). While a writer in the Firebird pod commits a row every half
// second (exiting on its first failed commit), every Job must succeed
// without stopping the writer or putting the database into physical
// backup mode, every run must get its own run tag, and every snapshot must
// restore to a consistent database: the marker row, all the padding, and
// a gap-free prefix of the writer's rows (each gbak reads the database in
// a single snapshot transaction).
//
// It must be called from inside the "Manager" Ordered Describe in
// e2e_test.go, after instanceS3BackupSpecs (whose Silo bucket it uses) and
// before that Describe's AfterAll tears down the manager.
func instanceS3StreamingLoadSpecs() {
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
    - name: %q
  storage:
    primary:
      size: 2Gi
  backup:
    enabled: true
    backupOnDelete: false
    destinations:
      - s3:
          endpoint: %s
          bucket: %s
          forcePathStyle: true
          credentials:
            secretRef: %s
    retention:
      hour: "2h"
      day: "2d"
      week: "2w"
      month: "2m"
      year: "2y"
`, s3LoadInstanceName, namespace, firebirdVersion, s3LoadDatabaseName, siloEndpoint, s3Bucket, s3LoadCredentials)
	backupPath := func(frequency string) string {
		return "/var/lib/firebird/backup/" + frequency + "/" + strings.TrimSuffix(s3LoadDatabaseName, ".fdb") + ".fbk"
	}

	podExec := func(script string) (string, error) {
		cmd := exec.Command("kubectl", "exec", s3LoadPodName, "-n", namespace, "-c", firebirdContainer,
			"--", "sh", "-c", script)
		return utils.Run(cmd)
	}
	runIsql := func(script string) (string, error) {
		password, err := getSecretField(s3LoadSysdbaSecret, "password")
		if err != nil {
			return "", err
		}
		cmd := exec.Command("kubectl", "exec", "-i", s3LoadPodName, "-n", namespace, "-c", firebirdContainer,
			"--", "isql", "-q", "-user", "SYSDBA", "-password", password, s3LoadDBConn)
		cmd.Stdin = strings.NewReader(script)
		return utils.Run(cmd)
	}
	countWrites := func() int {
		output, err := runIsql("SELECT 'WRITES=' || COUNT(*) FROM WRITES;\nQUIT;\n")
		Expect(err).NotTo(HaveOccurred())
		var n int
		_, err = fmt.Sscanf(output[strings.Index(output, "WRITES=")+len("WRITES="):], "%d", &n)
		Expect(err).NotTo(HaveOccurred(), "parsing %q", output)
		return n
	}

	var jobs []string

	Context("Instance s3 streaming backups under write load", Ordered, func() {
		BeforeAll(func() {
			By("creating the S3 credentials Secret")
			cmd := exec.Command("kubectl", "create", "secret", "generic", s3LoadCredentials, "-n", namespace,
				"--from-literal=AWS_ACCESS_KEY_ID="+siloAccessKey,
				"--from-literal=AWS_SECRET_ACCESS_KEY="+siloSecretKey)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			By("stopping the writer, if it's still running")
			_, _ = podExec(fmt.Sprintf("[ -f %[1]s ] && kill $(cat %[1]s); rm -f %[1]s", s3LoadWriterPIDFile))

			By("deleting the Jobs, the e2e-s3-load Instance, its PVC and Secrets")
			for _, job := range jobs {
				cmd := exec.Command("kubectl", "delete", "job", job, "-n", namespace, "--ignore-not-found")
				_, _ = utils.Run(cmd)
			}
			cmd := exec.Command("kubectl", "delete", "instance", s3LoadInstanceName, "-n", namespace,
				"--ignore-not-found", "--timeout=3m")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pvc", s3LoadInstanceName+"-primary", "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "secret", s3LoadSysdbaSecret, s3LoadInstanceName+"-restic",
				s3LoadCredentials, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should provision a database with enough data to keep each backup busy", func() {
			By("applying an Instance streaming every frequency to S3")
			applyS3Manifest(manifest)
			Eventually(func(g Gomega) {
				for path, want := range map[string]string{
					"{.status.databases[*]}": s3LoadDatabaseName,
					"{.status.phase}":        "Ready",
					"{.status.error}":        "",
				} {
					cmd := exec.Command("kubectl", "get", "instance", s3LoadInstanceName, "-n", namespace, "-o", "jsonpath="+path)
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(output).To(Equal(want), path)
				}
			}, 6*time.Minute, 2*time.Second).Should(Succeed())

			By(fmt.Sprintf("writing a marker row and %d rows of padding", s3LoadBulkRows))
			_, err := runIsql(fmt.Sprintf(`CREATE TABLE MARKER (ID INTEGER);
CREATE TABLE WRITES (ID INTEGER);
CREATE TABLE BULK (ID INTEGER, PAD VARCHAR(400));
COMMIT;
INSERT INTO MARKER VALUES (42);
SET TERM ^;
EXECUTE BLOCK AS DECLARE I INTEGER = 0; BEGIN
  WHILE (I < %d) DO BEGIN
    INSERT INTO BULK VALUES (:I, LPAD('', 300, UUID_TO_CHAR(GEN_UUID())));
    I = I + 1;
  END
END^
SET TERM ;^
COMMIT;
QUIT;
`, s3LoadBulkRows))
			Expect(err).NotTo(HaveOccurred())

			By("waiting for all five backup CronJobs")
			Eventually(func(g Gomega) {
				for _, freq := range concurrentBackupFrequencies {
					cmd := exec.Command("kubectl", "get", "cronjob", s3LoadInstanceName+"-backup-"+freq, "-n", namespace)
					_, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred(), "missing %s backup CronJob", freq)
				}
			}, 3*time.Minute, 2*time.Second).Should(Succeed())
		})

		It("should stream backups started at the same moment without locking the database", func() {
			By("committing a row every half second from a writer in the Firebird pod")
			password, err := getSecretField(s3LoadSysdbaSecret, "password")
			Expect(err).NotTo(HaveOccurred())
			_, err = podExec(fmt.Sprintf(`exec </dev/null >/dev/null 2>&1
nohup sh -c 'i=0; while :; do i=$((i+1)); echo "INSERT INTO WRITES VALUES ($i); COMMIT;" | isql -q -user SYSDBA -password %[2]q %[3]q || exit 1; sleep 0.5; done' &
echo $! > %[1]s`, s3LoadWriterPIDFile, password, s3LoadDBConn))
			Expect(err).NotTo(HaveOccurred())
			Eventually(countWrites, time.Minute, time.Second).Should(BeNumerically(">", 0), "the writer never committed")

			By("creating a Job from every frequency's CronJob, plus extra ones from the hourly CronJob, all at once")
			for _, freq := range concurrentBackupFrequencies {
				jobs = append(jobs, fmt.Sprintf("%s-load-%s", s3LoadInstanceName, freq))
			}
			for i := 1; i <= s3LoadExtraHourlyRuns; i++ {
				jobs = append(jobs, fmt.Sprintf("%s-load-hour-%d", s3LoadInstanceName, i))
			}
			for _, job := range jobs {
				// Job names are "<instance>-load-<frequency>[-<n>]".
				freq := strings.SplitN(strings.TrimPrefix(job, s3LoadInstanceName+"-load-"), "-", 2)[0]
				createDetachedJob(job, s3LoadInstanceName+"-backup-"+freq)
			}

			By("waiting for every Job to succeed")
			Eventually(func(g Gomega) {
				for _, job := range jobs {
					cmd := exec.Command("kubectl", "get", "job", job, "-n", namespace,
						"-o", "jsonpath={.status.succeeded}/{.status.failed}")
					output, err := utils.Run(cmd)
					g.Expect(err).NotTo(HaveOccurred())
					if strings.HasSuffix(output, "/1") {
						logs, _ := utils.Run(exec.Command("kubectl", "logs", "job/"+job, "-n", namespace, "--all-containers"))
						StopTrying(fmt.Sprintf("Job %s failed:\n%s", job, logs)).Now()
					}
					g.Expect(output).To(HavePrefix("1/"), "Job %s hasn't succeeded yet", job)
				}
			}, 10*time.Minute, 2*time.Second).Should(Succeed())

			By("having kept the writer committing throughout")
			before := countWrites()
			Eventually(countWrites, 30*time.Second, time.Second).Should(BeNumerically(">", before),
				"the writer stopped committing")
			_, err = podExec(fmt.Sprintf("kill -0 $(cat %s)", s3LoadWriterPIDFile))
			Expect(err).NotTo(HaveOccurred(), "the writer exited, so one of its commits failed")

			By("never putting the database into physical backup mode")
			output, err := runIsql("SELECT 'STATE=' || MON$BACKUP_STATE FROM MON$DATABASE;\nQUIT;\n")
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(ContainSubstring("STATE=0"))

			// A frequency's real schedule can tick while this runs (the
			// hourly one at every :00), adding its own snapshot, so only a
			// lower bound is exact here.
			By("streaming one snapshot per run, each with its own run tag")
			output, err = s3ResticFor(s3LoadInstanceName, "snapshots", "--json")
			Expect(err).NotTo(HaveOccurred())
			var snapshots []s3LoadSnapshot
			Expect(json.Unmarshal([]byte(output[strings.Index(output, "["):]), &snapshots)).To(Succeed())
			for _, freq := range concurrentBackupFrequencies {
				want := 1
				if freq == "hour" {
					want += s3LoadExtraHourlyRuns
				}
				runs := map[string]bool{}
				n := 0
				for _, s := range snapshots {
					if !slices.Contains(s.Tags, freq) {
						continue
					}
					n++
					Expect(s.Paths).To(Equal([]string{backupPath(freq)}), "%s snapshot %s", freq, s.ID)
					Expect(s.Tags).To(ContainElement("firebird:"+firebirdVersion), "%s snapshot %s", freq, s.ID)
					for _, tag := range s.Tags {
						if strings.HasPrefix(tag, "run:") {
							runs[tag] = true
						}
					}
				}
				Expect(n).To(BeNumerically(">=", want), "%s snapshots", freq)
				Expect(runs).To(HaveLen(n), "every %s run has its own run tag", freq)
			}

			By("not reporting any reconcile error")
			cmd := exec.Command("kubectl", "get", "instance", s3LoadInstanceName, "-n", namespace,
				"-o", "jsonpath={.status.error}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(BeEmpty())
		})

		It("should have streamed backups that restore to a consistent database", func() {
			By("stopping the writer")
			_, err := podExec(fmt.Sprintf("kill $(cat %[1]s); rm -f %[1]s", s3LoadWriterPIDFile))
			Expect(err).NotTo(HaveOccurred())

			By("restoring every snapshot with gbak, straight from restic dump")
			output, err := s3ResticFor(s3LoadInstanceName, "snapshots", "--json")
			Expect(err).NotTo(HaveOccurred())
			var snapshots []s3LoadSnapshot
			Expect(json.Unmarshal([]byte(output[strings.Index(output, "["):]), &snapshots)).To(Succeed())
			var script strings.Builder
			script.WriteString("set -e\nexport PATH=/opt/firebird/bin:$PATH\n")
			for _, s := range snapshots {
				fmt.Fprintf(&script, `rm -f /tmp/r.fdb
$RESTIC dump %[1]s %[2]q | gbak -create stdin /tmp/r.fdb
echo "SELECT 'CHECK %[1]s MARKER=' || (SELECT ID FROM MARKER) || ' BULK=' || (SELECT COUNT(*) FROM BULK) || ' WRITES=' || (SELECT COUNT(*) FROM WRITES) || ' MAXW=' || (SELECT COALESCE(MAX(ID), 0) FROM WRITES) FROM RDB\$DATABASE;" | isql -q -user SYSDBA /tmp/r.fdb
`, s.ID, s.Paths[0])
			}
			checks := s3LoadCheckPattern.FindAllStringSubmatch(s3LoadVerify(script.String()), -1)
			Expect(checks).To(HaveLen(len(snapshots)), "one check per snapshot")
			for _, c := range checks {
				Expect(c[2]).To(Equal("42"), "snapshot %s's marker", c[1])
				Expect(c[3]).To(Equal(strconv.Itoa(s3LoadBulkRows)), "snapshot %s's padding", c[1])
				Expect(c[4]).NotTo(Equal("0"), "snapshot %s has none of the writer's rows", c[1])
				Expect(c[4]).To(Equal(c[5]), "snapshot %s's writer rows aren't a gap-free prefix", c[1])
			}
		})
	})
}
