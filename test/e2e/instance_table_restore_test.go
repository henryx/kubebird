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
	tableRestoreInstanceName = "e2e-table-restore"
	tableRestoreSecretName   = tableRestoreInstanceName + "-sysdba"
	tableRestoreDatabaseName = "tablerestore.fdb"
	tableRestoreDBPath       = "/var/lib/firebird/data/" + tableRestoreDatabaseName
	tableRestorePodName      = tableRestoreInstanceName + "-0"
)

// instanceTableRestoreSpecs exercises that user data survives repeated
// backup-then-restore cycles, not just the database file's existence: a
// table created after the Instance is first provisioned must still be
// there after the Instance is deleted (which backs it up via gbak) and
// recreated under the same name (which restores it), and a second table
// created after that first restore must join the first once the Instance
// is deleted and recreated a second time.
//
// It must be called from inside the "Manager" Ordered Describe in
// e2e_test.go, after instanceBackupOrphanSpecs, and before that Describe's
// AfterAll tears down the manager.
func instanceTableRestoreSpecs() {
	manifest := fmt.Sprintf(`
apiVersion: kubebird.github.io/v1
kind: Instance
metadata:
  name: %s
  namespace: %s
spec:
  image: firebirdsql/firebird
  version: 3.0.14
  databases:
    - name: "%s"
  storage:
    primary:
      size: 1Gi
    backup:
      size: 1Gi
`, tableRestoreInstanceName, namespace, tableRestoreDatabaseName)

	Context("Instance table restore across repeated delete/recreate", Ordered, func() {
		AfterAll(func() {
			By("deleting the e2e-table-restore Instance, if it still exists")
			cmd := exec.Command("kubectl", "delete", "instance", tableRestoreInstanceName,
				"-n", namespace, "--ignore-not-found", "--wait=false")
			_, _ = utils.Run(cmd)
		})

		It("should provision database A and create a test table in it", func() {
			By("applying the Instance CR with database A and a backup volume")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for database A to be provisioned")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "instance", tableRestoreInstanceName, "-n", namespace,
					"-o", "jsonpath={.status.databases[*]}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.Fields(output)).To(ConsistOf(tableRestoreDatabaseName))
			}, 5*time.Minute, 2*time.Second).Should(Succeed())

			By("creating a test table in database A")
			_, err = tableRestoreRunIsql(tableRestoreSecretName, "CREATE TABLE TABLE_ONE (ID INTEGER);\nCOMMIT;\nQUIT;\n")
			Expect(err).NotTo(HaveOccurred())

			By("finding the test table in the freshly-created database")
			schema, err := tableRestoreExtractSchema(tableRestoreSecretName)
			Expect(err).NotTo(HaveOccurred())
			Expect(schema).To(ContainSubstring("TABLE_ONE"))
		})

		It("should still find the first test table after the Instance is deleted and recreated", func() {
			By("deleting the Instance, which backs up database A before releasing its storage")
			cmd := exec.Command("kubectl", "delete", "instance", tableRestoreInstanceName, "-n", namespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "instance", tableRestoreInstanceName, "-n", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "Instance should have been deleted")
			}, 3*time.Minute, 2*time.Second).Should(Succeed())

			By("re-applying the identical Instance CR")
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("restoring database A from its backup rather than creating it empty")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "instance", tableRestoreInstanceName, "-n", namespace,
					"-o", "jsonpath={.status.databases[*]}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.Fields(output)).To(ConsistOf(tableRestoreDatabaseName))
			}, 5*time.Minute, 2*time.Second).Should(Succeed())

			By("not reporting any reconcile error")
			cmd = exec.Command("kubectl", "get", "instance", tableRestoreInstanceName, "-n", namespace,
				"-o", "jsonpath={.status.error}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(BeEmpty())

			By("finding the first test table still present in the restored database")
			schema, err := tableRestoreExtractSchema(tableRestoreSecretName)
			Expect(err).NotTo(HaveOccurred())
			Expect(schema).To(ContainSubstring("TABLE_ONE"))

			By("creating a second test table in the restored database")
			_, err = tableRestoreRunIsql(tableRestoreSecretName, "CREATE TABLE TABLE_TWO (ID INTEGER);\nCOMMIT;\nQUIT;\n")
			Expect(err).NotTo(HaveOccurred())
		})

		It("should find both test tables after a second delete and recreate", func() {
			By("deleting the Instance again, backing up both tables via database A's backup")
			cmd := exec.Command("kubectl", "delete", "instance", tableRestoreInstanceName, "-n", namespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "instance", tableRestoreInstanceName, "-n", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "Instance should have been deleted")
			}, 3*time.Minute, 2*time.Second).Should(Succeed())

			By("re-applying the identical Instance CR once more")
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("restoring database A from its updated backup")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "instance", tableRestoreInstanceName, "-n", namespace,
					"-o", "jsonpath={.status.databases[*]}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.Fields(output)).To(ConsistOf(tableRestoreDatabaseName))
			}, 5*time.Minute, 2*time.Second).Should(Succeed())

			By("finding both test tables present in the restored database")
			schema, err := tableRestoreExtractSchema(tableRestoreSecretName)
			Expect(err).NotTo(HaveOccurred())
			Expect(schema).To(ContainSubstring("TABLE_ONE"))
			Expect(schema).To(ContainSubstring("TABLE_TWO"))
		})
	})
}

// tableRestoreRunIsql execs isql against the e2e-table-restore Instance's
// database, authenticating as SYSDBA with the password read from
// secretName, and feeds it script over stdin. Used to run DDL that changes
// the database rather than just read from it.
func tableRestoreRunIsql(secretName, script string) (string, error) {
	password, err := getSecretField(secretName, "password")
	if err != nil {
		return "", err
	}
	cmd := exec.Command("kubectl", "exec", "-i", tableRestorePodName, "-n", namespace, "-c", firebirdContainer,
		"--", "isql", "-user", "SYSDBA", "-password", password, tableRestoreDBPath)
	cmd.Stdin = strings.NewReader(script)
	return utils.Run(cmd)
}

// tableRestoreExtractSchema returns the full DDL of the e2e-table-restore
// Instance's database via isql -x, used to confirm a table survived a
// backup/restore cycle without depending on isql's exit code for a
// SQL-level error (e.g. querying a table that no longer exists).
func tableRestoreExtractSchema(secretName string) (string, error) {
	password, err := getSecretField(secretName, "password")
	if err != nil {
		return "", err
	}
	cmd := exec.Command("kubectl", "exec", tableRestorePodName, "-n", namespace, "-c", firebirdContainer,
		"--", "isql", "-x", "-user", "SYSDBA", "-password", password, tableRestoreDBPath)
	return utils.Run(cmd)
}
