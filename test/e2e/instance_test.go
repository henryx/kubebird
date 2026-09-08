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
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/henryx/kubebird/test/utils"
)

const (
	instanceName           = "e2e-instance"
	instanceSecretName     = instanceName + "-sysdba"
	instanceAliasesCMName  = instanceName + "-aliases"
	instancePrimaryPVCName = instanceName + "-primary"
	instanceBackupPVCName  = instanceName + "-backup"
	instanceShadowPVCName  = instanceName + "-shadow"
	instanceAliasName      = "enforced"
	firebirdContainer      = "firebird"

	tableRestoreInstanceName = "e2e-table-restore"
	tableRestoreSecretName   = tableRestoreInstanceName + "-sysdba"
	tableRestoreDatabaseName = "tablerestore.fdb"
	tableRestoreDBPath       = "/var/lib/firebird/data/" + tableRestoreDatabaseName
	tableRestorePodName      = tableRestoreInstanceName + "-0"
)

// instanceLifecycleSpecs exercises the full Instance lifecycle against a
// real cluster: CRD validation, deployment of the Secret, aliases
// ConfigMap, Service and StatefulSet, real database provisioning inside
// the Firebird container, adding a database to an already-deployed
// Instance, live SYSDBA password rotation, and cleanup on deletion.
//
// It must be called from inside the "Manager" Ordered Describe in
// e2e_test.go, after the CRDs are installed and the controller-manager is
// deployed, and before that Describe's AfterAll tears them down.
func instanceLifecycleSpecs() {
	Context("Instance", Ordered, func() {
		AfterAll(func() {
			By("deleting the e2e Instance, if it still exists")
			cmd := exec.Command("kubectl", "delete", "instance", instanceName,
				"-n", namespace, "--ignore-not-found", "--wait=false")
			_, _ = utils.Run(cmd)
		})

		It("should reject an Instance that violates the CRD's cross-field validation", func() {
			By("applying an Instance with a shadowed database but no storage.shadow")
			manifest := fmt.Sprintf(`
apiVersion: kubebird.github.io/v1
kind: Instance
metadata:
  name: %s-invalid
  namespace: %s
spec:
  image: firebirdsql/firebird
  version: 3.0.14
  databases:
    - name: "invalid.fdb"
      shadow: true
  storage:
    primary:
      size: 1Gi
`, instanceName, namespace)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			output, err := utils.Run(cmd)
			Expect(err).To(HaveOccurred(), "the API server should have rejected the Instance")
			Expect(output).To(ContainSubstring("storage.shadow is required"))
		})

		It("should deploy the Secret, ConfigMap, Service, StatefulSet and provision the databases", func() {
			By("applying the Instance CR with a plain and a shadowed database")
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
    - name: "instance.fdb"
    - name: "shadowed.fdb"
      alias: "%s"
      shadow: true
  storage:
    primary:
      size: 1Gi
    backup:
      size: 1Gi
    shadow:
      size: 1Gi
`, instanceName, namespace, instanceAliasName)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("auto-creating the SYSDBA Secret with a generated password")
			Eventually(func(g Gomega) {
				username, err := getSecretField(instanceSecretName, "username")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(username).To(Equal("SYSDBA"))

				password, err := getSecretField(instanceSecretName, "password")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(password).NotTo(BeEmpty())
			}).Should(Succeed())

			By("registering both database aliases in the aliases ConfigMap")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "configmap", instanceAliasesCMName, "-n", namespace,
					"-o", `jsonpath={.data.databases\.conf}`)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("instance.fdb = /var/lib/firebird/data/instance.fdb"))
				g.Expect(output).To(ContainSubstring(instanceAliasName + " = /var/lib/firebird/data/shadowed.fdb"))
			}).Should(Succeed())

			By("creating PVCs named <instance>-primary, <instance>-backup and <instance>-shadow")
			cmd = exec.Command("kubectl", "get", "pvc", instancePrimaryPVCName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			cmd = exec.Command("kubectl", "get", "pvc", instanceBackupPVCName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			cmd = exec.Command("kubectl", "get", "pvc", instanceShadowPVCName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("exposing the instance on a ClusterIP Service")
			cmd = exec.Command("kubectl", "get", "service", instanceName, "-n", namespace,
				"-o", "jsonpath={.spec.ports[0].port}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(Equal("3050"))

			By("waiting for the StatefulSet's pod to become ready")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "statefulset", instanceName, "-n", namespace,
					"-o", "jsonpath={.status.readyReplicas}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("1"))
			}, 5*time.Minute, 2*time.Second).Should(Succeed())

			By("marking the Instance as Available")
			Eventually(func(g Gomega) {
				status, err := getInstanceCondition("Available")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(status).To(Equal("True"))
			}, 3*time.Minute, 2*time.Second).Should(Succeed())

			By("provisioning both databases and recording them in status.databases")
			Eventually(func(g Gomega) {
				names, err := getInstanceDatabases()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(names).To(ConsistOf("instance.fdb", "shadowed.fdb"))

				count, err := getInstanceDatabaseCount()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(count).To(Equal("2"))
			}, 3*time.Minute, 2*time.Second).Should(Succeed())

			By("not reporting any reconcile error")
			cmd = exec.Command("kubectl", "get", "instance", instanceName, "-n", namespace,
				"-o", "jsonpath={.status.error}")
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(BeEmpty())

			By("actually creating the primary database file inside the pod")
			cmd = exec.Command("kubectl", "exec", instancePod(), "-n", namespace, "-c", firebirdContainer,
				"--", "test", "-f", "/var/lib/firebird/data/instance.fdb")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("actually creating the shadow file for the shadowed database")
			cmd = exec.Command("kubectl", "exec", instancePod(), "-n", namespace, "-c", firebirdContainer,
				"--", "test", "-f", "/var/lib/firebird/shadow/shadowed.fdb")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("mounting the backup PVC into the pod")
			cmd = exec.Command("kubectl", "exec", instancePod(), "-n", namespace, "-c", firebirdContainer,
				"--", "test", "-d", "/var/lib/firebird/backup")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should provision a newly added database without disturbing existing ones", func() {
			By("recording the pod's current start time")
			startTimeBefore, err := getPodStartTime()
			Expect(err).NotTo(HaveOccurred())

			By("adding a third database to spec.databases")
			cmd := exec.Command("kubectl", "patch", "instance", instanceName, "-n", namespace,
				"--type=json", "-p", `[{"op":"add","path":"/spec/databases/-","value":{"name":"added.fdb"}}]`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("provisioning only the new database")
			Eventually(func(g Gomega) {
				names, err := getInstanceDatabases()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(names).To(ConsistOf("instance.fdb", "shadowed.fdb", "added.fdb"))

				count, err := getInstanceDatabaseCount()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(count).To(Equal("3"))
			}, 3*time.Minute, 2*time.Second).Should(Succeed())

			By("not restarting the pod to do so")
			startTimeAfter, err := getPodStartTime()
			Expect(err).NotTo(HaveOccurred())
			Expect(startTimeAfter).To(Equal(startTimeBefore))

			By("keeping the previously created database file in place")
			cmd = exec.Command("kubectl", "exec", instancePod(), "-n", namespace, "-c", firebirdContainer,
				"--", "test", "-f", "/var/lib/firebird/data/instance.fdb")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should drop a removed database without disturbing others", func() {
			By("recording the pod's current start time")
			startTimeBefore, err := getPodStartTime()
			Expect(err).NotTo(HaveOccurred())

			By("removing the database added in the previous spec from spec.databases")
			cmd := exec.Command("kubectl", "patch", "instance", instanceName, "-n", namespace,
				"--type=json", "-p", `[{"op":"remove","path":"/spec/databases/2"}]`)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("dropping only the removed database from status.databases")
			Eventually(func(g Gomega) {
				names, err := getInstanceDatabases()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(names).To(ConsistOf("instance.fdb", "shadowed.fdb"))

				count, err := getInstanceDatabaseCount()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(count).To(Equal("2"))
			}, 3*time.Minute, 2*time.Second).Should(Succeed())

			By("removing its alias from the aliases ConfigMap")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "configmap", instanceAliasesCMName, "-n", namespace,
					"-o", `jsonpath={.data.databases\.conf}`)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).NotTo(ContainSubstring("added.fdb"))
			}).Should(Succeed())

			By("not restarting the pod to do so")
			startTimeAfter, err := getPodStartTime()
			Expect(err).NotTo(HaveOccurred())
			Expect(startTimeAfter).To(Equal(startTimeBefore))

			By("actually deleting the dropped database's file inside the pod")
			cmd = exec.Command("kubectl", "exec", instancePod(), "-n", namespace, "-c", firebirdContainer,
				"--", "test", "-f", "/var/lib/firebird/data/added.fdb")
			_, err = utils.Run(cmd)
			Expect(err).To(HaveOccurred(), "added.fdb should have been dropped")

			By("keeping the other database files in place")
			cmd = exec.Command("kubectl", "exec", instancePod(), "-n", namespace, "-c", firebirdContainer,
				"--", "test", "-f", "/var/lib/firebird/data/instance.fdb")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should push a rotated SYSDBA password to the live server", func() {
			const newPassword = "e2e-rotated-Pa55word!"

			By("overwriting the SYSDBA Secret's password")
			cmd := exec.Command("kubectl", "patch", "secret", instanceSecretName, "-n", namespace,
				"--type=merge", "-p", fmt.Sprintf(`{"stringData":{"password":%q}}`, newPassword))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("authenticating against the running server with the new password")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "exec", "-i", instancePod(), "-n", namespace, "-c", firebirdContainer,
					"--", "isql", "-user", "SYSDBA", "-password", newPassword,
					"/var/lib/firebird/data/instance.fdb")
				cmd.Stdin = strings.NewReader("QUIT;\n")
				_, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("should garbage collect the Secret, ConfigMap, Service and StatefulSet on deletion", func() {
			By("deleting the Instance")
			cmd := exec.Command("kubectl", "delete", "instance", instanceName, "-n", namespace)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("garbage collecting every object it owned")
			owned := [][2]string{
				{"secret", instanceSecretName},
				{"configmap", instanceAliasesCMName},
				{"service", instanceName},
				{"statefulset", instanceName},
			}
			for _, kindAndName := range owned {
				kind, name := kindAndName[0], kindAndName[1]
				Eventually(func(g Gomega) {
					cmd := exec.Command("kubectl", "get", kind, name, "-n", namespace)
					_, err := utils.Run(cmd)
					g.Expect(err).To(HaveOccurred(), fmt.Sprintf("%s/%s should have been garbage collected", kind, name))
				}, 2*time.Minute, 2*time.Second).Should(Succeed())
			}

			By("releasing the primary and shadow PVCs, since storage.backup was configured")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pvc", instancePrimaryPVCName, "-n", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "primary PVC should have been released")
			}, 2*time.Minute, 2*time.Second).Should(Succeed())
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pvc", instanceShadowPVCName, "-n", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "shadow PVC should have been released")
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("keeping the backup PVC itself, since it isn't owned by the Instance")
			cmd = exec.Command("kubectl", "get", "pvc", instanceBackupPVCName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("actually backing up every provisioned database into the backup volume before releasing storage")
			verifyBackupFiles(instanceBackupPVCName, instanceName+"/instance.fbk", instanceName+"/shadowed.fbk")
		})

		It("should restore databases from their backups when the Instance is recreated", func() {
			By("re-applying the Instance CR under the same name, reusing the surviving backup PVC")
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
    - name: "instance.fdb"
    - name: "shadowed.fdb"
      alias: "%s"
      shadow: true
  storage:
    primary:
      size: 1Gi
    backup:
      size: 1Gi
    shadow:
      size: 1Gi
`, instanceName, namespace, instanceAliasName)
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("restoring both databases from their backups instead of creating them empty")
			Eventually(func(g Gomega) {
				names, err := getInstanceDatabases()
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(names).To(ConsistOf("instance.fdb", "shadowed.fdb"))
			}, 5*time.Minute, 2*time.Second).Should(Succeed())

			By("not reporting any reconcile error, since gbak restored rather than tried CREATE DATABASE")
			cmd = exec.Command("kubectl", "get", "instance", instanceName, "-n", namespace,
				"-o", "jsonpath={.status.error}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(BeEmpty())

			By("actually restoring the primary database file inside the pod")
			cmd = exec.Command("kubectl", "exec", instancePod(), "-n", namespace, "-c", firebirdContainer,
				"--", "test", "-f", "/var/lib/firebird/data/instance.fdb")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("recreating the shadow file for the restored shadowed database")
			cmd = exec.Command("kubectl", "exec", instancePod(), "-n", namespace, "-c", firebirdContainer,
				"--", "test", "-f", "/var/lib/firebird/shadow/shadowed.fdb")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})
	})
}

// instancePVCReuseSpecs exercises reusing a primary PVC left behind by an
// earlier Instance of the same name (storage.backup isn't set here, so the
// primary PVC survives deletion like any unowned PVC): the database file
// already on it must be registered into status.databases without
// re-running CREATE DATABASE against it.
//
// It must be called from inside the "Manager" Ordered Describe in
// e2e_test.go, after instanceLifecycleSpecs, and before that Describe's
// AfterAll tears down the manager.
func instancePVCReuseSpecs() {
	const (
		reuseInstanceName   = "e2e-reuse"
		reuseDatabaseName   = "reuse.fdb"
		reusePrimaryPVCName = reuseInstanceName + "-primary"
	)

	Context("Instance PVC reuse", Ordered, func() {
		AfterAll(func() {
			By("deleting the e2e-reuse Instance and its primary PVC, if they still exist")
			cmd := exec.Command("kubectl", "delete", "instance", reuseInstanceName,
				"-n", namespace, "--ignore-not-found", "--wait=false")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "pvc", reusePrimaryPVCName, "-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

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
`, reuseInstanceName, namespace, reuseDatabaseName)

		It("should provision the database on a first-generation Instance", func() {
			By("applying the Instance CR")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("waiting for the database to be provisioned")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "instance", reuseInstanceName, "-n", namespace,
					"-o", "jsonpath={.status.databases[*]}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.Fields(output)).To(ConsistOf(reuseDatabaseName))
			}, 5*time.Minute, 2*time.Second).Should(Succeed())
		})

		It("should reuse the surviving primary PVC without re-creating the database", func() {
			By("recording the primary PVC's UID before deletion")
			cmd := exec.Command("kubectl", "get", "pvc", reusePrimaryPVCName, "-n", namespace, "-o", "jsonpath={.metadata.uid}")
			pvcUIDBefore, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(pvcUIDBefore).NotTo(BeEmpty())

			By("deleting the first-generation Instance, leaving the unowned primary PVC behind")
			cmd = exec.Command("kubectl", "delete", "instance", reuseInstanceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "instance", reuseInstanceName, "-n", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "Instance should have been deleted")
			}, 2*time.Minute, 2*time.Second).Should(Succeed())
			cmd = exec.Command("kubectl", "get", "pvc", reusePrimaryPVCName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "the primary PVC should have survived deletion")

			By("re-applying the same Instance CR")
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("reusing the same primary PVC rather than creating a new one")
			cmd = exec.Command("kubectl", "get", "pvc", reusePrimaryPVCName, "-n", namespace, "-o", "jsonpath={.metadata.uid}")
			pvcUIDAfter, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(pvcUIDAfter).To(Equal(pvcUIDBefore))

			By("registering the already-present database without re-running CREATE DATABASE")
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "instance", reuseInstanceName, "-n", namespace,
					"-o", "jsonpath={.status.databases[*]}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.Fields(output)).To(ConsistOf(reuseDatabaseName))
			}, 5*time.Minute, 2*time.Second).Should(Succeed())

			By("not reporting any reconcile error, since CREATE DATABASE was never attempted against the existing file")
			cmd = exec.Command("kubectl", "get", "instance", reuseInstanceName, "-n", namespace,
				"-o", "jsonpath={.status.error}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(output).To(BeEmpty())
		})
	})
}

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

// instancePod returns the name of the e2e Instance's single StatefulSet pod.
func instancePod() string {
	return instanceName + "-0"
}

// getSecretField returns the decoded value of a key in a Secret's data.
func getSecretField(secretName, key string) (string, error) {
	cmd := exec.Command("kubectl", "get", "secret", secretName, "-n", namespace,
		"-o", fmt.Sprintf("jsonpath={.data.%s}", key))
	output, err := utils.Run(cmd)
	if err != nil {
		return "", err
	}
	decoded, err := base64.StdEncoding.DecodeString(output)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

// getInstanceCondition returns the status of one condition on the e2e Instance.
func getInstanceCondition(conditionType string) (string, error) {
	cmd := exec.Command("kubectl", "get", "instance", instanceName, "-n", namespace,
		"-o", fmt.Sprintf(`jsonpath={.status.conditions[?(@.type=="%s")].status}`, conditionType))
	return utils.Run(cmd)
}

// getInstanceDatabases returns the database names recorded in the e2e
// Instance's status.databases.
func getInstanceDatabases() ([]string, error) {
	cmd := exec.Command("kubectl", "get", "instance", instanceName, "-n", namespace,
		"-o", "jsonpath={.status.databases[*]}")
	output, err := utils.Run(cmd)
	if err != nil {
		return nil, err
	}
	return strings.Fields(output), nil
}

// getInstanceDatabaseCount returns the e2e Instance's status.databaseCount.
func getInstanceDatabaseCount() (string, error) {
	cmd := exec.Command("kubectl", "get", "instance", instanceName, "-n", namespace,
		"-o", "jsonpath={.status.databaseCount}")
	return utils.Run(cmd)
}

// getPodStartTime returns the e2e Instance pod's start time, used to prove
// a reconcile didn't require restarting the pod.
func getPodStartTime() (string, error) {
	cmd := exec.Command("kubectl", "get", "pod", instancePod(), "-n", namespace,
		"-o", "jsonpath={.status.startTime}")
	return utils.Run(cmd)
}

// verifyBackupFiles spins up a throwaway Pod mounting pvcName to confirm
// each of files actually exists on it, then removes the pod. Used to prove
// backupAndReleaseStorage's gbak backup actually ran, rather than just
// checking the backup PVC survived.
func verifyBackupFiles(pvcName string, files ...string) {
	checks := make([]string, len(files))
	for i, f := range files {
		checks[i] = fmt.Sprintf("test -f /backup/%s", f)
	}
	runVerifyBackupPod(pvcName, strings.Join(checks, " && "))
}

// verifyBackupFileMissing is the mirror of verifyBackupFiles: it confirms
// file does NOT exist on pvcName. Used to prove a database dropped from
// spec.databases before an Instance was ever deleted again has no backup
// file written for it.
func verifyBackupFileMissing(pvcName, file string) {
	runVerifyBackupPod(pvcName, fmt.Sprintf("! test -f /backup/%s", file))
}

// runVerifyBackupPod spins up a throwaway Pod mounting pvcName at /backup,
// runs script inside it, and removes the pod once it succeeds.
func runVerifyBackupPod(pvcName, script string) {
	cmd := exec.Command("kubectl", "run", "verify-backup", "--restart=Never",
		"--namespace", namespace,
		"--image=busybox",
		"--overrides",
		fmt.Sprintf(`{
			"spec": {
				"containers": [{
					"name": "verify",
					"image": "busybox",
					"command": ["sh", "-c", "%s"],
					"volumeMounts": [{"name": "backup", "mountPath": "/backup"}]
				}],
				"volumes": [{"name": "backup", "persistentVolumeClaim": {"claimName": "%s"}}]
			}
		}`, script, pvcName))
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "failed to create verify-backup pod")
	defer func() {
		cmd := exec.Command("kubectl", "delete", "pod", "verify-backup", "-n", namespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	}()

	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "pod", "verify-backup", "-n", namespace,
			"-o", "jsonpath={.status.phase}")
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(Equal("Succeeded"))
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
}
