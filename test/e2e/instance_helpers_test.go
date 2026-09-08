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

	. "github.com/onsi/gomega"

	"github.com/henryx/kubebird/test/utils"
)

const firebirdContainer = "firebird"

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
