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
