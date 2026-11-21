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

package controller

import (
	"context"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kubebirdv1 "github.com/henryx/kubebird/api/v1"
)

const (
	testUserName       = "exampleUser"
	testReportUserName = "Report_Reader"
	testUserSecretRef  = "my-secret"
	testOwnedDatabase  = "a.fdb"
	testUsersInstance  = "test"
)

func usersTestInstance(auth kubebirdv1.AuthenticationSpec) *kubebirdv1.Instance {
	return &kubebirdv1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: testUsersInstance, Namespace: resourceNamespace},
		Spec:       kubebirdv1.InstanceSpec{Image: testImage, Version: testVersion, Authentication: auth},
	}
}

func usersTestSecret(name, password string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: resourceNamespace},
		Data:       map[string][]byte{sysdbaSecretPasswordKey: []byte(password)},
	}
}

func TestUserSecretName(t *testing.T) {
	instance := usersTestInstance(kubebirdv1.AuthenticationSpec{
		testReportUserName: {},
		"custom":           {SecretRef: testUserSecretRef},
	})
	cases := map[string]string{
		kubebirdv1.SysdbaUserName: "test-user-sysdba", // no sysdba key at all: the default
		testReportUserName:        "test-user-report-reader",
		"custom":                  testUserSecretRef,
	}
	for user, want := range cases {
		if got := userSecretName(instance, user); got != want {
			t.Errorf("userSecretName(%q) = %q, want %q", user, got, want)
		}
	}

	if got, want := userSecretNames(instance), []string{"test-user-sysdba", "test-user-report-reader", testUserSecretRef}; !slices.Equal(got, want) {
		t.Errorf("userSecretNames() = %v, want %v", got, want)
	}
}

func TestRegularUsers(t *testing.T) {
	instance := usersTestInstance(kubebirdv1.AuthenticationSpec{
		kubebirdv1.SysdbaUserName: {SecretRef: "s"},
		"zeta":                    {},
		"alpha":                   {},
	})
	if got, want := regularUsers(instance), []string{"alpha", "zeta"}; !slices.Equal(got, want) {
		t.Errorf("regularUsers() = %v, want %v", got, want)
	}
	if got := regularUsers(usersTestInstance(nil)); len(got) != 0 {
		t.Errorf("regularUsers() with no authentication = %v, want none", got)
	}
}

func TestDisplayedUserExists(t *testing.T) {
	// Actual fbsvcmgr -action_display_user output of the 3.0.14, 4.0.3 and
	// 5.0.4 images for an existing user; a missing one prints nothing.
	const out = "Login                        Full name                                 uid  gid adm\n" +
		"EXAMPLEUSER                                                              0    0  no\n"
	if !displayedUserExists(out, testUserName) {
		t.Errorf("displayedUserExists(%q, exampleUser) = false, want true", out)
	}
	if displayedUserExists(out, "example") {
		t.Errorf("displayedUserExists(%q, example) = true, want false: only whole names match", out)
	}
	if displayedUserExists("", testUserName) {
		t.Error("displayedUserExists on empty output = true, want false")
	}
}

func TestDatabaseOwnerCredentials(t *testing.T) {
	instance := usersTestInstance(kubebirdv1.AuthenticationSpec{testUserName: {}})
	r := &InstanceReconciler{Client: versionTestClient(t, usersTestSecret("test-user-exampleuser", "user-pw"))}
	ctx := context.Background()

	for _, owner := range []string{"", kubebirdv1.SysdbaUserName, sysdbaUsername} {
		user, password, err := r.databaseOwnerCredentials(ctx, instance, kubebirdv1.DatabaseSpec{Name: testOwnedDatabase, Owner: owner}, "sysdba-pw")
		if err != nil || user != sysdbaUsername || password != "sysdba-pw" {
			t.Errorf("owner %q: got (%q, %q, %v), want (%q, sysdba-pw, nil)", owner, user, password, err, sysdbaUsername)
		}
	}

	user, password, err := r.databaseOwnerCredentials(ctx, instance, kubebirdv1.DatabaseSpec{Name: testOwnedDatabase, Owner: testUserName}, "sysdba-pw")
	if err != nil || user != testUserName || password != "user-pw" {
		t.Errorf("owner exampleUser: got (%q, %q, %v), want (exampleUser, user-pw, nil)", user, password, err)
	}

	if _, _, err := r.databaseOwnerCredentials(ctx, instance, kubebirdv1.DatabaseSpec{Name: testOwnedDatabase, Owner: "nobody"}, "sysdba-pw"); err == nil {
		t.Error("owner not in spec.authentication: want an error")
	}
}

// TestReconcileUsersUpToDate checks that users whose Secrets haven't
// changed since their passwords were applied aren't touched: no exec into
// the pod (the reconciler has no ClientSet, so one would panic) and no
// status write.
func TestReconcileUsersUpToDate(t *testing.T) {
	ctx := context.Background()
	c := versionTestClient(t, usersTestSecret("test-user-exampleuser", "user-pw"))
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: "test-user-exampleuser", Namespace: resourceNamespace}, secret); err != nil {
		t.Fatal(err)
	}

	instance := usersTestInstance(kubebirdv1.AuthenticationSpec{testUserName: {}})
	instance.Status.Users = []kubebirdv1.UserStatus{{Name: testUserName, SecretVersion: secret.ResourceVersion}}
	r := &InstanceReconciler{Client: c}
	if err := r.reconcileUsers(ctx, instance); err != nil {
		t.Fatalf("reconcileUsers() failed: %v", err)
	}

	if err := r.reconcileUsers(ctx, usersTestInstance(nil)); err != nil {
		t.Fatalf("reconcileUsers() with no users at all failed: %v", err)
	}
}

// TestReconcileSysdbaSecretFromLegacy checks that an Instance created by
// Kubebird 0.4.0 or earlier, whose generated SYSDBA Secret is still named
// "<instance>-sysdba", gets its new "<instance>-user-sysdba" Secret with
// the same password instead of a generated one, leaving the legacy Secret
// alone, while a regular user's Secret is never copied from it.
func TestReconcileSysdbaSecretFromLegacy(t *testing.T) {
	ctx := context.Background()
	c := versionTestClient(t, usersTestSecret("test-sysdba", "legacy-pw"))
	r := &InstanceReconciler{Client: c}
	instance := usersTestInstance(kubebirdv1.AuthenticationSpec{testUserName: {}})

	if err := r.reconcileSysdbaSecret(ctx, instance); err != nil {
		t.Fatalf("reconcileSysdbaSecret() failed: %v", err)
	}
	if err := r.reconcileRegularUserSecrets(ctx, instance); err != nil {
		t.Fatalf("reconcileRegularUserSecrets() failed: %v", err)
	}

	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: "test-user-sysdba", Namespace: resourceNamespace}, secret); err != nil {
		t.Fatalf("new SYSDBA Secret not created: %v", err)
	}
	if got := string(secret.Data[sysdbaSecretPasswordKey]); got != "legacy-pw" {
		t.Errorf("new SYSDBA Secret password = %q, want the legacy one", got)
	}
	if got := string(secret.Data[sysdbaSecretUsernameKey]); got != sysdbaUsername {
		t.Errorf("new SYSDBA Secret username = %q, want %q", got, sysdbaUsername)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "test-sysdba", Namespace: resourceNamespace}, &corev1.Secret{}); err != nil {
		t.Errorf("legacy SYSDBA Secret must be left alone: %v", err)
	}

	user := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: "test-user-exampleuser", Namespace: resourceNamespace}, user); err != nil {
		t.Fatalf("regular user Secret not created: %v", err)
	}
	if got := user.StringData[sysdbaSecretPasswordKey] + string(user.Data[sysdbaSecretPasswordKey]); got == "" || got == "legacy-pw" {
		t.Errorf("regular user Secret password = %q, want a generated one", got)
	}
}
