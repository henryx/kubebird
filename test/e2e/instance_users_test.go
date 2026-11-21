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
	usersInstanceName  = "e2e-users"
	usersPodName       = usersInstanceName + "-0"
	usersSysdbaSecret  = usersInstanceName + "-user-sysdba"
	usersOwnerKey      = "exampleUser"
	usersOwnerSecret   = usersInstanceName + "-user-exampleuser"
	usersTempKey       = "temp_user"
	usersTempSecret    = usersInstanceName + "-user-temp-user"
	usersOwnedDatabase = "owned.fdb"
	usersPlainDatabase = "plain.fdb"
	usersBackupPVC     = usersInstanceName + "-backup"
	usersPrimaryPVC    = usersInstanceName + "-primary"
)

// usersLogin connects to database through the instance's own server (a
// localhost connection, so the password is actually checked, unlike an
// embedded one) as user, returning isql's output.
func usersLogin(user, password, database, sql string) (string, error) {
	cmd := exec.Command("kubectl", "exec", "-i", usersPodName, "-n", namespace, "-c", firebirdContainer,
		"--", "isql", "-q", "-user", user, "-password", password, "localhost:/var/lib/firebird/data/"+database)
	cmd.Stdin = strings.NewReader("SET BAIL ON;\nSET LIST ON;\n" + sql + "\nQUIT;\n")
	return utils.Run(cmd)
}

// usersDatabaseOwner returns database's owner, as SYSDBA sees it.
func usersDatabaseOwner(g Gomega, database string) string {
	password, err := getSecretField(usersSysdbaSecret, "password")
	g.Expect(err).NotTo(HaveOccurred())
	output, err := usersLogin("SYSDBA", password, database,
		"SELECT TRIM(RDB$OWNER_NAME) AS OWNER_NAME FROM RDB$RELATIONS WHERE RDB$RELATION_NAME = 'RDB$DATABASE';")
	g.Expect(err).NotTo(HaveOccurred())
	return output
}

// usersPodStartTime returns the Firebird pod's start time, which changes
// whenever the pod is recreated.
func usersPodStartTime() string {
	cmd := exec.Command("kubectl", "get", "pod", usersPodName, "-n", namespace, "-o", "jsonpath={.status.startTime}")
	output, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
	return output
}

// waitForUsersDatabases waits until both databases are provisioned and
// both regular users applied.
func waitForUsersDatabases(users ...string) {
	Eventually(func(g Gomega) {
		cmd := exec.Command("kubectl", "get", "instance", usersInstanceName, "-n", namespace,
			"-o", "jsonpath={.status.databases[*]}")
		output, err := utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.Fields(output)).To(ConsistOf(usersOwnedDatabase, usersPlainDatabase))

		cmd = exec.Command("kubectl", "get", "instance", usersInstanceName, "-n", namespace,
			"-o", "jsonpath={.status.users[*].name}")
		output, err = utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.Fields(output)).To(ConsistOf(users))

		cmd = exec.Command("kubectl", "get", "instance", usersInstanceName, "-n", namespace,
			"-o", "jsonpath={.status.error}")
		output, err = utils.Run(cmd)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(output).To(BeEmpty())
	}, 5*time.Minute, 2*time.Second).Should(Succeed())
}

// instanceUserSpecs exercises regular users declared in
// spec.authentication and databases[].owner: Kubebird generates each
// user's Secret, creates the user in the security database, creates (and,
// after a delete/recreate, restores) an owned database as that user,
// applies a rotated password without restarting the pod, and deletes a
// user once it's removed from spec.authentication.
//
// It must be called from inside the "Manager" Ordered Describe in
// e2e_test.go, before that Describe's AfterAll tears down the manager.
func instanceUserSpecs() {
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
      owner: %s
    - name: "%s"
  storage:
    primary:
      size: 1Gi
  authentication:
    %s: {}
    %s: {}
  backup:
    enabled: true
    destinations:
      - local:
          storage:
            size: 1Gi
`, usersInstanceName, namespace, firebirdVersion, usersOwnedDatabase, usersOwnerKey, usersPlainDatabase,
		usersOwnerKey, usersTempKey)

	Context("Instance regular users and database owners", Ordered, func() {
		AfterAll(func() {
			By("deleting the e2e-users Instance")
			cmd := exec.Command("kubectl", "delete", "instance", usersInstanceName,
				"-n", namespace, "--ignore-not-found", "--timeout=2m")
			_, _ = utils.Run(cmd)

			By("cleaning up the PVCs and Secrets, which are never released automatically")
			cmd = exec.Command("kubectl", "delete", "pvc", usersPrimaryPVC, usersBackupPVC,
				"-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
			cmd = exec.Command("kubectl", "delete", "secret", usersSysdbaSecret, usersOwnerSecret, usersTempSecret,
				"-n", namespace, "--ignore-not-found")
			_, _ = utils.Run(cmd)
		})

		It("should create the users and the owned database as its owner", func() {
			By("applying the Instance CR")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			waitForUsersDatabases(usersOwnerKey, usersTempKey)

			By("generating each user's Secret, its username key being the authentication key")
			for secret, user := range map[string]string{usersOwnerSecret: usersOwnerKey, usersTempSecret: usersTempKey} {
				username, err := getSecretField(secret, "username")
				Expect(err).NotTo(HaveOccurred())
				Expect(username).To(Equal(user))
			}

			By("logging in as each user with its Secret's password")
			for secret, user := range map[string]string{usersOwnerSecret: usersOwnerKey, usersTempSecret: usersTempKey} {
				password, err := getSecretField(secret, "password")
				Expect(err).NotTo(HaveOccurred())
				_, err = usersLogin(user, password, usersPlainDatabase, "SELECT 1 FROM RDB$DATABASE;")
				Expect(err).NotTo(HaveOccurred(), "user %s should log in", user)
			}

			By("owning the databases as declared")
			Eventually(func(g Gomega) {
				g.Expect(usersDatabaseOwner(g, usersOwnedDatabase)).To(ContainSubstring("EXAMPLEUSER"))
				g.Expect(usersDatabaseOwner(g, usersPlainDatabase)).To(ContainSubstring("SYSDBA"))
			}, time.Minute, 2*time.Second).Should(Succeed())
		})

		It("should apply a rotated password without restarting the pod", func() {
			startTime := usersPodStartTime()
			oldPassword, err := getSecretField(usersOwnerSecret, "password")
			Expect(err).NotTo(HaveOccurred())

			By("rotating the user's Secret")
			const newPassword = "Rotated-Pass-123"
			cmd := exec.Command("kubectl", "patch", "secret", usersOwnerSecret, "-n", namespace,
				"--type=merge", "-p", fmt.Sprintf(`{"stringData":{"password":%q}}`, newPassword))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("logging in with the new password, and no longer with the old one")
			Eventually(func(g Gomega) {
				_, err := usersLogin(usersOwnerKey, newPassword, usersOwnedDatabase, "SELECT 1 FROM RDB$DATABASE;")
				g.Expect(err).NotTo(HaveOccurred())
			}, 2*time.Minute, 2*time.Second).Should(Succeed())
			_, err = usersLogin(usersOwnerKey, oldPassword, usersOwnedDatabase, "SELECT 1 FROM RDB$DATABASE;")
			Expect(err).To(HaveOccurred(), "the old password must no longer work")

			By("keeping the same pod")
			Expect(usersPodStartTime()).To(Equal(startTime))
		})

		It("should restore the owned database as its owner after a delete/recreate", func() {
			password, err := getSecretField(usersOwnerSecret, "password")
			Expect(err).NotTo(HaveOccurred())

			By("creating a marker table as the owner")
			_, err = usersLogin(usersOwnerKey, password, usersOwnedDatabase,
				"CREATE TABLE MARKER (ID INTEGER);\nCOMMIT;\nINSERT INTO MARKER VALUES (42);\nCOMMIT;")
			Expect(err).NotTo(HaveOccurred())

			By("deleting the Instance, which backs up its databases")
			cmd := exec.Command("kubectl", "delete", "instance", usersInstanceName, "-n", namespace, "--timeout=3m")
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pvc", usersPrimaryPVC, "-n", namespace)
				_, err := utils.Run(cmd)
				g.Expect(err).To(HaveOccurred(), "primary PVC should have been released")
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("re-applying the identical Instance CR")
			cmd = exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(manifest)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			waitForUsersDatabases(usersOwnerKey, usersTempKey)

			By("logging in as the owner with the retained Secret's password and finding the marker row")
			Eventually(func(g Gomega) {
				output, err := usersLogin(usersOwnerKey, password, usersOwnedDatabase, "SELECT ID FROM MARKER;")
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("42"))
				g.Expect(usersDatabaseOwner(g, usersOwnedDatabase)).To(ContainSubstring("EXAMPLEUSER"))
			}, 2*time.Minute, 2*time.Second).Should(Succeed())
		})

		It("should delete a user removed from spec.authentication", func() {
			password, err := getSecretField(usersTempSecret, "password")
			Expect(err).NotTo(HaveOccurred())

			By("removing the user from spec.authentication")
			cmd := exec.Command("kubectl", "patch", "instance", usersInstanceName, "-n", namespace,
				"--type=json", "-p", fmt.Sprintf(`[{"op":"remove","path":"/spec/authentication/%s"}]`, usersTempKey))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			waitForUsersDatabases(usersOwnerKey)

			By("no longer letting the user log in")
			_, err = usersLogin(usersTempKey, password, usersPlainDatabase, "SELECT 1 FROM RDB$DATABASE;")
			Expect(err).To(HaveOccurred(), "a deleted user must no longer log in")

			By("keeping the user's Secret, like every other Secret Kubebird creates")
			cmd = exec.Command("kubectl", "get", "secret", usersTempSecret, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
		})
	})
}
