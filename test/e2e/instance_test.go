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
	})
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
	script := strings.Join(checks, " && ")

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
