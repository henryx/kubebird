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

// instanceBackupOrphanSpecs exercises what happens to storage.backup's
// per-database .fbk files when an Instance is deleted with two databases,
// then recreated under the same name with a different database set: the
// database dropped from spec.databases (a.fdb) is never restored and its
// backup file is left behind untouched, the database kept in both
// generations (b.fdb) is restored from its backup, and the database newly
// added in the second generation (c.fdb) is created fresh with no backup
// file of its own.
//
// It must be called from inside the "Manager" Ordered Describe in
// e2e_test.go, after instancePVCReuseSpecs, and before that Describe's
// AfterAll tears down the manager.
func instanceBackupOrphanSpecs() {
	const (
		orphanInstanceName  = "e2e-backup-orphan"
		orphanBackupPVCName = orphanInstanceName + "-backup"
	)

	manifest := func(databases ...string) string {
		var dbEntries strings.Builder
		for _, db := range databases {
			fmt.Fprintf(&dbEntries, "    - name: %q\n", db)
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
%s  storage:
    primary:
      size: 1Gi
    backup:
      size: 1Gi
`, orphanInstanceName, namespace, dbEntries.String())
	}

	Context("Instance backup orphaning across a database set change", Ordered, func() {
		AfterAll(func() {
			By("deleting the e2e-backup-orphan Instance, if it still exists")
			cmd := exec.Command("kubectl", "delete", "instance", orphanInstanceName,
				"-n", namespace, "--ignore-not-found", "--wait=false")
			_, _ = utils.Run(cmd)
		})

		It("should back up both first-generation databases when the Instance is deleted", func() {
			By("applying an Instance with databases a.fdb and b.fdb")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest("a.fdb", "b.fdb"))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for both databases to be provisioned")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "instance", orphanInstanceName, "-n", namespace,
					"-o", "jsonpath={.status.databases[*]}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.Fields(output)).To(ConsistOf("a.fdb", "b.fdb"))
			}, 5*time.Minute, 2*time.Second).Should(Succeed())

			By("deleting the Instance")
			cmd = exec.Command("kubectl", "delete", "instance", orphanInstanceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "instance", orphanInstanceName, "-n", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "Instance should have been deleted")
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("backing up both a.fdb and b.fdb into storage.backup before releasing the primary PVC")
			verifyBackupFiles(orphanBackupPVCName, orphanInstanceName+"/a.fbk", orphanInstanceName+"/b.fbk")
		})

		It("should restore only b.fdb, create c.fdb fresh, and leave a.fbk untouched when recreated with a different database set", func() {
			By("re-applying the Instance CR with b.fdb kept, a.fdb dropped, and c.fdb newly added")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest("b.fdb", "c.fdb"))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("registering exactly b.fdb and c.fdb, never a.fdb")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "instance", orphanInstanceName, "-n", namespace,
					"-o", "jsonpath={.status.databases[*]}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.Fields(output)).To(ConsistOf("b.fdb", "c.fdb"))
			}, 5*time.Minute, 2*time.Second).Should(Succeed())

			By("not reporting any reconcile error")
			cmd = exec.Command("kubectl", "get", "instance", orphanInstanceName, "-n", namespace,
				"-o", "jsonpath={.status.error}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(BeEmpty())

			By("warning that a.fdb's backup wasn't restored, since a.fdb is no longer in spec.databases")
			cmd = exec.Command("kubectl", "get", "instance", orphanInstanceName, "-n", namespace,
				"-o", "jsonpath={.status.warning}")
			warning, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(warning).To(ContainSubstring("a.fdb"))
			Expect(warning).NotTo(ContainSubstring("b.fdb"), "b.fdb was restored, so it shouldn't be reported as orphaned")
			Expect(warning).NotTo(ContainSubstring("c.fdb"), "c.fdb was never backed up, so it shouldn't be reported as orphaned")

			By("surfacing that same warning through status.message")
			cmd = exec.Command("kubectl", "get", "instance", orphanInstanceName, "-n", namespace,
				"-o", "jsonpath={.status.message}")
			message, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(message).To(Equal(warning))

			podName := orphanInstanceName + "-0"

			By("restoring b.fdb from its backup rather than creating it empty")
			cmd = exec.Command("kubectl", "exec", podName, "-n", namespace, "-c", firebirdContainer,
				"--", "test", "-f", "/var/lib/firebird/data/b.fdb")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("creating c.fdb fresh, since it was never backed up")
			cmd = exec.Command("kubectl", "exec", podName, "-n", namespace, "-c", firebirdContainer,
				"--", "test", "-f", "/var/lib/firebird/data/c.fdb")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("never restoring a.fdb, since it's no longer in spec.databases")
			cmd = exec.Command("kubectl", "exec", podName, "-n", namespace, "-c", firebirdContainer,
				"--", "test", "-f", "/var/lib/firebird/data/a.fdb")
			_, err = utils.Run(cmd)
			Expect(err).To(HaveOccurred())

			By("leaving a.fbk in storage.backup untouched, orphaned rather than cleaned up")
			verifyBackupFiles(orphanBackupPVCName, orphanInstanceName+"/a.fbk")

			By("never writing a backup for c.fdb, since no deletion has backed it up yet")
			verifyBackupFileMissing(orphanBackupPVCName, orphanInstanceName+"/c.fbk")

			// Delete and wait for it to actually finish here, rather than
			// leaving it to AfterAll's fire-and-forget cleanup: this is the
			// last Context in the suite, so nothing else runs afterward to
			// give backupAndReleaseStorage time to back up b.fdb/c.fdb and
			// remove the finalizer before the manager is undeployed.
			By("deleting the Instance and waiting for its backup-then-finalizer-removal to finish")
			cmd = exec.Command("kubectl", "delete", "instance", orphanInstanceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "instance", orphanInstanceName, "-n", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "Instance should have been deleted")
			}, 3*time.Minute, 2*time.Second).Should(Succeed())
		})
	})
}
