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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/henryx/kubebird/test/utils"
)

// Silo (https://silo.pgsty.com) is an S3-compatible object store forked
// from MinIO. The suite deploys a single, throwaway instance of it into the
// Kind cluster (see setupSilo) for the specs of the s3 backup destination.
const (
	siloNamespace = "silo"
	siloImage     = "pgsty/silo:RELEASE.2026-11-12T00-00-00Z"
	siloPort      = 9000

	// siloAccessKey and siloSecretKey are Silo's root credentials, which
	// the specs use as S3 credentials.
	siloAccessKey = "kubebird"
	siloSecretKey = "kubebird-secret"
)

// siloEndpoint is Silo's S3 endpoint, as reached from the cluster's pods.
var siloEndpoint = fmt.Sprintf("http://silo.%s.svc.cluster.local:%d", siloNamespace, siloPort)

// siloManifest deploys Silo, storing its data in an emptyDir, behind a
// "silo" Service.
var siloManifest = fmt.Sprintf(`
apiVersion: v1
kind: Namespace
metadata:
  name: %[1]s
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: silo
  namespace: %[1]s
spec:
  replicas: 1
  selector:
    matchLabels:
      app: silo
  template:
    metadata:
      labels:
        app: silo
    spec:
      containers:
        - name: silo
          image: %[2]s
          args: ["server", "/data"]
          env:
            - name: MINIO_ROOT_USER
              value: %[4]s
            - name: MINIO_ROOT_PASSWORD
              value: %[5]s
          ports:
            - containerPort: %[3]d
          readinessProbe:
            httpGet:
              path: /minio/health/live
              port: %[3]d
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: silo
  namespace: %[1]s
spec:
  selector:
    app: silo
  ports:
    - port: %[3]d
      targetPort: %[3]d
`, siloNamespace, siloImage, siloPort, siloAccessKey, siloSecretKey)

// setupSilo deploys Silo into the Kind cluster and waits for it to be
// ready.
func setupSilo() {
	By("deploying Silo, an S3-compatible object store, into the Kind cluster")
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(siloManifest)
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to deploy Silo")

	cmd = exec.Command("kubectl", "rollout", "status", "deployment/silo", "-n", siloNamespace, "--timeout=5m")
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Silo didn't become ready")
}

// teardownSilo removes Silo, and with it every bucket the specs wrote.
func teardownSilo() {
	By("removing Silo")
	cmd := exec.Command("kubectl", "delete", "ns", siloNamespace, "--ignore-not-found", "--timeout=2m")
	_, _ = utils.Run(cmd)
}
