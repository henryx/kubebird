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
	versionUpgradeInstanceName = "e2e-version-upgrade"
	versionUpgradeSecretName   = versionUpgradeInstanceName + "-sysdba"
	versionUpgradeDatabaseName = "versionupgrade.fdb"
	versionUpgradeDBPath       = "/var/lib/firebird/data/" + versionUpgradeDatabaseName
	versionUpgradePodName      = versionUpgradeInstanceName + "-0"
	versionUpgradeFromVersion  = "3.0.14"
	versionUpgradeToVersion    = "4.0.3"
)

// instanceVersionUpgradeSpecs exercises deleting and recreating an
// Instance under the same name with spec.version bumped to a new Firebird
// major version: the backup taken on delete (with the old version's gbak)
// must restore cleanly under the new version's gbak and server, carrying
// user data across the major-version upgrade, the recreated StatefulSet
// must run the new image tag, and SYSDBA must authenticate against the
// upgraded server with the freshly-generated password (the Secret is
// itself garbage collected and recreated on delete/recreate, so it isn't
// the same password as before).
//
// It must be called from inside the "Manager" Ordered Describe in
// e2e_test.go, after instanceTableRestoreSpecs, and before that Describe's
// AfterAll tears down the manager.
func instanceVersionUpgradeSpecs() {
	manifest := func(version string) string {
		return fmt.Sprintf(`
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
  storage:
    primary:
      size: 1Gi
    backup:
      size: 1Gi
`, versionUpgradeInstanceName, namespace, version, versionUpgradeDatabaseName)
	}

	Context("Instance version upgrade across delete/recreate", Ordered, func() {
		AfterAll(func() {
			By("deleting the e2e-version-upgrade Instance, if it still exists")
			cmd := exec.Command("kubectl", "delete", "instance", versionUpgradeInstanceName,
				"-n", namespace, "--ignore-not-found", "--wait=false")
			_, _ = utils.Run(cmd)
		})

		It(fmt.Sprintf("should provision database A on Firebird %s and create a marker table", versionUpgradeFromVersion), func() {
			By("applying the Instance CR with database A, version " + versionUpgradeFromVersion + ", and a backup volume")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest(versionUpgradeFromVersion))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for database A to be provisioned")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "instance", versionUpgradeInstanceName, "-n", namespace,
					"-o", "jsonpath={.status.databases[*]}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.Fields(output)).To(ConsistOf(versionUpgradeDatabaseName))
			}, 5*time.Minute, 2*time.Second).Should(Succeed())

			By("running the Firebird " + versionUpgradeFromVersion + " image in the StatefulSet pod")
			cmd = exec.Command("kubectl", "get", "pod", versionUpgradePodName, "-n", namespace,
				"-o", fmt.Sprintf(`jsonpath={.spec.containers[?(@.name=="%s")].image}`, firebirdContainer))
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("firebirdsql/firebird:" + versionUpgradeFromVersion))

			By("creating a marker table to prove data survives the upgrade")
			_, err = versionUpgradeRunIsql(versionUpgradeSecretName, "CREATE TABLE PRE_UPGRADE (ID INTEGER);\nCOMMIT;\nQUIT;\n")
			Expect(err).NotTo(HaveOccurred())
		})

		It(fmt.Sprintf("should restore database A under Firebird %s after delete/recreate with a bumped version", versionUpgradeToVersion), func() {
			By("deleting the Instance, which backs up database A using Firebird " + versionUpgradeFromVersion + "'s gbak")
			cmd := exec.Command("kubectl", "delete", "instance", versionUpgradeInstanceName, "-n", namespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "instance", versionUpgradeInstanceName, "-n", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "Instance should have been deleted")
			}, 3*time.Minute, 2*time.Second).Should(Succeed())

			By("re-applying the Instance CR with version bumped to " + versionUpgradeToVersion)
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest(versionUpgradeToVersion))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("restoring database A from its backup rather than creating it empty")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "instance", versionUpgradeInstanceName, "-n", namespace,
					"-o", "jsonpath={.status.databases[*]}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.Fields(output)).To(ConsistOf(versionUpgradeDatabaseName))
			}, 5*time.Minute, 2*time.Second).Should(Succeed())

			By("not reporting any reconcile error")
			cmd = exec.Command("kubectl", "get", "instance", versionUpgradeInstanceName, "-n", namespace,
				"-o", "jsonpath={.status.error}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(BeEmpty())

			By("running the new Firebird " + versionUpgradeToVersion + " image in the StatefulSet pod")
			cmd = exec.Command("kubectl", "get", "pod", versionUpgradePodName, "-n", namespace,
				"-o", fmt.Sprintf(`jsonpath={.spec.containers[?(@.name=="%s")].image}`, firebirdContainer))
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("firebirdsql/firebird:" + versionUpgradeToVersion))

			By("finding the marker table created before the upgrade")
			schema, err := versionUpgradeExtractSchema(versionUpgradeSecretName)
			Expect(err).NotTo(HaveOccurred())
			Expect(schema).To(ContainSubstring("PRE_UPGRADE"))

			By("authenticating as SYSDBA against the upgraded server with the freshly-generated password")
			_, err = versionUpgradeRunIsql(versionUpgradeSecretName, "QUIT;\n")
			Expect(err).NotTo(HaveOccurred())
		})
	})
}

// versionUpgradeRunIsql execs isql against the e2e-version-upgrade
// Instance's database, authenticating as SYSDBA with the password read
// from secretName, and feeds it script over stdin. Used to run DDL that
// changes the database rather than just read from it.
func versionUpgradeRunIsql(secretName, script string) (string, error) {
	password, err := getSecretField(secretName, "password")
	if err != nil {
		return "", err
	}
	cmd := exec.Command("kubectl", "exec", "-i", versionUpgradePodName, "-n", namespace, "-c", firebirdContainer,
		"--", "isql", "-user", "SYSDBA", "-password", password, versionUpgradeDBPath)
	cmd.Stdin = strings.NewReader(script)
	return utils.Run(cmd)
}

// versionUpgradeExtractSchema returns the full DDL of the
// e2e-version-upgrade Instance's database via isql -x, used to confirm the
// marker table survived the backup/restore across the version upgrade
// without depending on isql's exit code for a SQL-level error.
func versionUpgradeExtractSchema(secretName string) (string, error) {
	password, err := getSecretField(secretName, "password")
	if err != nil {
		return "", err
	}
	cmd := exec.Command("kubectl", "exec", versionUpgradePodName, "-n", namespace, "-c", firebirdContainer,
		"--", "isql", "-x", "-user", "SYSDBA", "-password", password, versionUpgradeDBPath)
	return utils.Run(cmd)
}
