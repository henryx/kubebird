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
	securityDBInstanceName = "e2e-security-db"
	securityDBSecretName   = securityDBInstanceName + "-sysdba"
	securityDBDatabaseName = "securitydb.fdb"
	securityDBPrimaryPVC   = securityDBInstanceName + "-primary"
	securityDBBackupPVC    = securityDBInstanceName + "-backup"
	securityDBPodName      = securityDBInstanceName + "-0"
	securityDBVersion      = "3.0.14"
	securityDBPath         = "/var/lib/firebird/data/security3.fdb"
	securityDBAppDBPath    = "/var/lib/firebird/data/" + securityDBDatabaseName
	securityDBMarkerUser   = "MARKERUSER"

	// firebirdBinaryPath is the firebirdsql/firebird image's own server
	// binary, whose executable bit execOfflineOnSecurityDB toggles to get
	// a deterministic window with no live engine holding the security
	// database open.
	firebirdBinaryPath = "/opt/firebird/bin/firebird"
)

// execOfflineOnSecurityDB runs sql as a bare, OS-trusted embedded
// connection directly against the security database: neither a bare-path
// connection (conflicts with the live server's own already-open engine
// instance, "Database already opened with engine instance, incompatible
// with current") nor a loopback one (refused outright - "no permission
// for remote access" - by the security.db alias's RemoteAccess = false)
// can reach the security database while the server is up, so this briefly
// disables the firebird binary (blocking fbguard from respawning it) and
// kills the running process to get a deterministic offline window, runs
// sql through isql's embedded engine, then restores the binary and waits
// for a real connection to succeed again before returning. Kubebird itself
// no longer needs this trick for SYSDBA password rotation (see
// mutateStatefulSet in internal/controller/instance_resources.go, which
// restarts the pod instead), but directly manipulating the security
// database - as this test does to prove a marker user survives a
// delete/recreate - has no such shortcut available.
func execOfflineOnSecurityDB(podName, sql string) (string, error) {
	shellScript := fmt.Sprintf(`set -e
trap 'chmod +x %[1]s' EXIT
chmod -x %[1]s
pid=$(pidof firebird || true)
[ -n "$pid" ] && kill -9 "$pid"
isql %[2]s <<'SQLEOF'
%[3]s
SQLEOF
chmod +x %[1]s
trap - EXIT
for i in $(seq 1 300); do
	if isql %[2]s <<'PROBEEOF' >/dev/null 2>&1
SET BAIL ON;
SELECT 1 FROM RDB$DATABASE;
QUIT;
PROBEEOF
	then
		exit 0
	fi
	sleep 0.1
done
echo "firebird did not become reachable again" >&2
exit 1`, firebirdBinaryPath, securityDBPath, sql)
	cmd := exec.Command("kubectl", "exec", "-i", podName, "-n", namespace, "-c", firebirdContainer,
		"--", "sh", "-c", shellScript)
	return utils.Run(cmd)
}

// instanceSecurityDatabaseSpecs exercises the security-database-init init
// container's backup/restore path specifically for the security database's
// own content, as opposed to just the SYSDBA password: a Firebird user
// created directly in the security database (not the SYSDBA account synced
// from the Secret via FIREBIRD_ROOT_PASSWORD) survives deleting and
// recreating an Instance under the same name with a local backup volume
// configured — releasePrimaryAndShadowStorage always deletes
// the primary PVC on deletion now (see instance_controller.go), so
// there's no more "the PVC just happens to survive" path; this proves the
// gbak backup taken by backupDatabases and the gbak restore run by
// securityDatabaseInitScript actually carry over arbitrary security
// database content, not just the well-known SYSDBA row that the image's
// own entrypoint re-applies from the Secret on every start regardless (see
// instanceLifecycleSpecs, which only checks the SYSDBA password and the
// security database file's existence after a restore).
//
// It must be called from inside the "Manager" Ordered Describe in
// e2e_test.go, after instanceVersionUpgradeSpecs, and before that
// Describe's AfterAll tears down the manager.
func instanceSecurityDatabaseSpecs() {
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
  storage:
    primary:
      size: 1Gi
  backup:
    enabled: true
    type:
      - local:
          storage:
            size: 1Gi
`, securityDBInstanceName, namespace, securityDBVersion, securityDBDatabaseName)

	Context("Instance security database content survives a delete/recreate via a local backup volume", Ordered, func() {
		AfterAll(func() {
			By("deleting the e2e-security-db Instance, waiting for its finalizer-driven backup and PVC release to finish")
			cmd := exec.Command("kubectl", "delete", "instance", securityDBInstanceName,
				"-n", namespace, "--ignore-not-found", "--timeout=2m")
			_, _ = utils.Run(cmd)

			By("cleaning up the backup PVC, which is never released automatically")
			cmd = exec.Command("kubectl", "delete", "pvc", securityDBPrimaryPVC, securityDBBackupPVC,
				"-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should provision the Instance and create a marker user directly in the security database", func() {
			By("applying the Instance CR")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the database to be provisioned")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "instance", securityDBInstanceName, "-n", namespace,
					"-o", "jsonpath={.status.databases[*]}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.Fields(output)).To(ConsistOf(securityDBDatabaseName))
			}, 5*time.Minute, 2*time.Second).Should(Succeed())

			// Eventually: shortly after the pod becomes ready, the offline
			// dance's server restart can still be transiently unready,
			// even though status.databases already shows the instance
			// provisioned.
			By("creating a marker user directly in the security database")
			Eventually(func(g Gomega) {
				_, err := execOfflineOnSecurityDB(securityDBPodName, fmt.Sprintf(
					"SET BAIL ON;\nCREATE USER %s PASSWORD 'MarkerPass123';\nCOMMIT;\nQUIT;", securityDBMarkerUser))
				g.Expect(err).NotTo(HaveOccurred())
			}, 3*time.Minute, 2*time.Second).Should(Succeed())
		})

		It("should keep the marker user after deleting and recreating the Instance, restored from its backup", func() {
			By("deleting the Instance, which backs up the security database before releasing the primary PVC")
			cmd := exec.Command("kubectl", "delete", "instance", securityDBInstanceName, "-n", namespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "instance", securityDBInstanceName, "-n", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "Instance should have been deleted")
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("releasing the primary PVC, since it's no longer reused across a delete/recreate")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pvc", securityDBPrimaryPVC, "-n", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "primary PVC should have been released")
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("keeping the backup PVC itself, since it isn't owned by the Instance")
			cmd = exec.Command("kubectl", "get", "pvc", securityDBBackupPVC, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("having actually backed up the security database, marker user included, before releasing the primary PVC")
			verifyBackupFiles(securityDBBackupPVC, backupBaseDir+"/security3.fbk")

			By("re-applying the identical Instance CR")
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("registering the database again")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "instance", securityDBInstanceName, "-n", namespace,
					"-o", "jsonpath={.status.databases[*]}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.Fields(output)).To(ConsistOf(securityDBDatabaseName))
			}, 5*time.Minute, 2*time.Second).Should(Succeed())

			By("not reporting any reconcile error")
			cmd = exec.Command("kubectl", "get", "instance", securityDBInstanceName, "-n", namespace,
				"-o", "jsonpath={.status.error}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(BeEmpty())

			// The Secret is never owner-referenced, so it survives the
			// delete/recreate with the same password — the image's
			// entrypoint still re-applies FIREBIRD_ROOT_PASSWORD from it to
			// the restored security database on every container start, so
			// SYSDBA authenticating below proves that still works
			// regardless of the marker user also being present.
			password, err := getSecretField(securityDBSecretName, "password")
			Expect(err).NotTo(HaveOccurred())

			By("authenticating with the SYSDBA Secret's password against the running server")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", "-i", securityDBPodName, "-n", namespace, "-c", firebirdContainer,
					"--", "isql", "-user", "SYSDBA", "-password", password, securityDBAppDBPath)
				cmd.Stdin = strings.NewReader("QUIT;\n")
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}, 3*time.Minute, 5*time.Second).Should(Succeed())

			By("finding the marker user still present in the security database, restored via gbak from its backup")
			Eventually(func(g Gomega) {
				output, err := execOfflineOnSecurityDB(securityDBPodName, fmt.Sprintf(
					"SET BAIL ON;\nSET LIST ON;\nSELECT SEC$USER_NAME FROM SEC$USERS WHERE SEC$USER_NAME = '%s';\nQUIT;",
					securityDBMarkerUser))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring(securityDBMarkerUser))
			}, 3*time.Minute, 2*time.Second).Should(Succeed())
		})
	})
}
