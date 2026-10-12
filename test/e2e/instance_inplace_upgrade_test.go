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
	inPlaceUpgradeInstanceName = "e2e-inplace-upgrade"
	inPlaceUpgradePodName      = inPlaceUpgradeInstanceName + "-0"
	inPlaceUpgradeSecretName   = inPlaceUpgradeInstanceName + "-sysdba"
	inPlaceUpgradeDatabaseName = "inplace.fdb"
	inPlaceUpgradeDBPath       = "/var/lib/firebird/data/" + inPlaceUpgradeDatabaseName
	// inPlaceUpgradeUser is a regular Firebird user, created on 3.0, whose
	// password must survive every upgrade (migrated per the Firebird 4.0
	// migration guide from 3.0, restored with the security database from
	// 4.0 on).
	inPlaceUpgradeUser     = "APPUSER"
	inPlaceUpgradePassword = "apppass"
)

// instanceInPlaceUpgradeSpecs exercises changing spec.version on a live
// Instance, without deleting it: an upgrade to a new major version (3.0.14
// to 4.0.3, then 4.0.3 to 5.0.4) backs up the databases and the security
// database with the old version, releases the storage and restores onto
// the new version, keeping both a marker table and a regular user's login
// (migrated per the Firebird 4.0 migration guide when coming from 3.0); a
// move within the same major version (4.0.3 to 4.0.5 and back) just
// restarts the pod on the new image; a downgrade to an older major version
// is refused, leaving the running server alone.
//
// It must be called from inside the "Manager" Ordered Describe in
// e2e_test.go, after the CRDs are installed and the controller-manager is
// deployed, and before that Describe's AfterAll tears them down.
func instanceInPlaceUpgradeSpecs() {
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
    - name: %q
  storage:
    primary:
      size: 1Gi
  backup:
    enabled: true
    backupOnDelete: false
    destinations:
      - local:
          storage:
            size: 1Gi
`, inPlaceUpgradeInstanceName, namespace, version, inPlaceUpgradeDatabaseName)
	}

	apply := func(version string) {
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(manifest(version))
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
	}

	instanceField := func(jsonpath string) string {
		cmd := exec.Command("kubectl", "get", "instance", inPlaceUpgradeInstanceName, "-n", namespace,
			"-o", "jsonpath="+jsonpath)
		output, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())
		return output
	}

	podField := func(jsonpath string) (string, error) {
		cmd := exec.Command("kubectl", "get", "pod", inPlaceUpgradePodName, "-n", namespace, "-o", "jsonpath="+jsonpath)
		return utils.Run(cmd)
	}

	// runIsql runs script with isql inside the Firebird pod against the
	// database, over localhost so the server authenticates the user.
	runIsql := func(user, password, script string) (string, error) {
		cmd := exec.Command("kubectl", "exec", "-i", inPlaceUpgradePodName, "-n", namespace, "-c", firebirdContainer,
			"--", "isql", "-q", "-user", user, "-password", password, "localhost:"+inPlaceUpgradeDBPath)
		cmd.Stdin = strings.NewReader(script)
		return utils.Run(cmd)
	}

	runAsSysdba := func(script string) (string, error) {
		password, err := getSecretField(inPlaceUpgradeSecretName, "password")
		if err != nil {
			return "", err
		}
		return runIsql("SYSDBA", password, script)
	}

	// waitForVersion waits for the Instance to be Ready on version, with
	// its database (back) in place and no upgrade left in progress.
	waitForVersion := func(version string) {
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "instance", inPlaceUpgradeInstanceName, "-n", namespace,
				"-o", "jsonpath={.status.phase}|{.status.upgrade}|{.status.databases[*]}|{.status.error}")
			output, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(output).To(Equal("Ready||" + inPlaceUpgradeDatabaseName + "|"))
			image, err := podField(fmt.Sprintf(`{.spec.containers[?(@.name=="%s")].image}`, firebirdContainer))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(image).To(Equal("firebirdsql/firebird:" + version))
			ready, err := podField(`{.status.conditions[?(@.type=="Ready")].status}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(ready).To(Equal("True"))
		}, 10*time.Minute, 3*time.Second).Should(Succeed())
	}

	// expectDataAndUsers checks the server runs version, the marker table
	// holds its row, and both SYSDBA (with the Secret's password) and the
	// regular user (with its own) log in.
	expectDataAndUsers := func(version string) {
		output, err := runAsSysdba("SET HEADING OFF;\nSELECT rdb$get_context('SYSTEM', 'ENGINE_VERSION') FROM rdb$database;\nSELECT id FROM marker;\n")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.Fields(output)).To(Equal([]string{version, "42"}))

		output, err = runIsql(inPlaceUpgradeUser, inPlaceUpgradePassword, "SET HEADING OFF;\nSELECT CURRENT_USER FROM rdb$database;\n")
		Expect(err).NotTo(HaveOccurred(), "the regular user must still log in with its own password")
		Expect(strings.TrimSpace(output)).To(Equal(inPlaceUpgradeUser))
	}

	Context("Instance in-place spec.version changes", Ordered, func() {
		AfterAll(func() {
			By("deleting the e2e-inplace-upgrade Instance, if it still exists")
			cmd := exec.Command("kubectl", "delete", "instance", inPlaceUpgradeInstanceName,
				"-n", namespace, "--ignore-not-found", "--timeout=3m")
			_, _ = utils.Run(cmd)
		})

		It("should provision Firebird 3.0.14 with a marker table and a regular user", func() {
			apply("3.0.14")
			waitForVersion("3.0.14")

			_, err := runAsSysdba(fmt.Sprintf("CREATE TABLE marker (id INTEGER);\nINSERT INTO marker VALUES (42);\n"+
				"CREATE USER %s PASSWORD '%s';\nCOMMIT;\n", inPlaceUpgradeUser, inPlaceUpgradePassword))
			Expect(err).NotTo(HaveOccurred())
			expectDataAndUsers("3.0.14")
		})

		It("should upgrade in place from Firebird 3 to 4, restoring the data and migrating the users", func() {
			apply("4.0.3")

			By("going through the Upgrading phase, recorded in status.upgrade")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "instance", inPlaceUpgradeInstanceName, "-n", namespace,
					"-o", "jsonpath={.status.phase}|{.status.upgrade.fromVersion}|{.status.upgrade.toVersion}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Upgrading|3.0.14|4.0.3"))
			}, 2*time.Minute, time.Second).Should(Succeed())

			waitForVersion("4.0.3")
			expectDataAndUsers("4.0.3")
		})

		It("should just restart the pod for another release of the same major version, either way", func() {
			for _, version := range []string{"4.0.5", "4.0.3"} {
				By("moving to " + version)
				startTime, err := podField("{.status.startTime}")
				Expect(err).NotTo(HaveOccurred())
				apply(version)
				Eventually(func(g Gomega) {
					current, err := podField("{.status.startTime}")
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(current).NotTo(Equal(startTime), "the pod should have been recreated")
				}, 3*time.Minute, 2*time.Second).Should(Succeed())
				Expect(instanceField("{.status.upgrade}")).To(BeEmpty(), "no backup and restore within a major version")
				waitForVersion(version)
				expectDataAndUsers(version)
			}
		})

		It("should refuse to downgrade to Firebird 3, leaving the running server alone", func() {
			apply("3.0.14")
			Eventually(func(g Gomega) {
				g.Expect(instanceField("{.status.error}")).To(ContainSubstring("downgrading Firebird from 4.0.3 to 3.0.14 isn't supported"))
			}, 2*time.Minute, 2*time.Second).Should(Succeed())
			image, err := podField(fmt.Sprintf(`{.spec.containers[?(@.name=="%s")].image}`, firebirdContainer))
			Expect(err).NotTo(HaveOccurred())
			Expect(image).To(Equal("firebirdsql/firebird:4.0.3"))
			expectDataAndUsers("4.0.3")

			By("clearing the error once spec.version is set back")
			apply("4.0.3")
			waitForVersion("4.0.3")
		})

		It("should upgrade in place from Firebird 4 to 5, restoring the data and the security database", func() {
			apply("5.0.4")
			waitForVersion("5.0.4")
			expectDataAndUsers("5.0.4")
		})
	})
}
