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
	"bufio"
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kubebirdv1 "github.com/henryx/kubebird/api/v1"
)

// fbsvcmgr's Services API user management actions, run by
// applyFirebirdUser/deleteFirebirdUser against the live server. Unlike SQL
// (CREATE USER and the like), they need no database to connect to, so they
// work on an Instance with no spec.databases too, and the security database
// itself stays reachable only locally (see mutateAliasesConfigMap).
const (
	serviceManagerConn = "localhost:service_mgr"

	actionDisplayUser = "-action_display_user"
	actionAddUser     = "-action_add_user"
	actionModifyUser  = "-action_modify_user"
	actionDeleteUser  = "-action_delete_user"
	flagSecUsername   = "-sec_username"
	flagSecPassword   = "-sec_password"
)

// regularUsers returns the authentication keys naming regular users, i.e.
// every key but sysdba, sorted so reconcileUsers applies them in a stable
// order.
func regularUsers(instance *kubebirdv1.Instance) []string {
	users := make([]string, 0, len(instance.Spec.Authentication))
	for user := range instance.Spec.Authentication {
		if user != kubebirdv1.SysdbaUserName {
			users = append(users, user)
		}
	}
	slices.Sort(users)
	return users
}

// userSecretName returns the name of the Secret holding the credentials of
// spec.authentication[user]: its secretRef when set, otherwise
// "<instance-name>-user-<user>", the user name lowercased and its
// underscores turned into dashes so it's a valid Secret name (e.g.
// "test-user-sysdba", or "test-user-report-reader" for "Report_Reader").
func userSecretName(instance *kubebirdv1.Instance, user string) string {
	if ref := instance.Spec.Authentication[user].SecretRef; ref != "" {
		return ref
	}
	return instance.Name + "-user-" + strings.ReplaceAll(strings.ToLower(user), "_", "-")
}

// legacySysdbaSecretName returns the name Kubebird 0.4.0 and earlier gave
// the generated SYSDBA Secret, "<instance-name>-sysdba". reconcileUserSecret
// copies it into the current default Secret (see userSecretName) when only
// the legacy one exists, so upgrading the operator keeps SYSDBA's password.
func legacySysdbaSecretName(instance *kubebirdv1.Instance) string {
	return instance.Name + "-sysdba"
}

// userSecretNames returns the names of every Secret the instance reads
// credentials from, SYSDBA's included, for the Secret watch's index (see
// SetupWithManager).
func userSecretNames(instance *kubebirdv1.Instance) []string {
	users := regularUsers(instance)
	names := make([]string, 0, len(users)+1)
	names = append(names, sysdbaSecretRefName(instance))
	for _, user := range users {
		names = append(names, userSecretName(instance, user))
	}
	return names
}

// reconcileRegularUserSecrets ensures the Secret of every regular user in
// spec.authentication exists (see reconcileUserSecret), a generated one's
// username key being the authentication key as written.
func (r *InstanceReconciler) reconcileRegularUserSecrets(ctx context.Context, instance *kubebirdv1.Instance) error {
	for _, user := range regularUsers(instance) {
		if err := r.reconcileUserSecret(ctx, instance, user, user); err != nil {
			return err
		}
	}
	return nil
}

// userSecret reads spec.authentication[user]'s Secret, returning its
// password and resourceVersion.
func (r *InstanceReconciler) userSecret(ctx context.Context, instance *kubebirdv1.Instance, user string) (password, version string, err error) {
	name := userSecretName(instance, user)
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", fmt.Errorf("secret %q of user %s not found", name, user)
		}
		return "", "", fmt.Errorf("failed to get Secret %q of user %s: %w", name, user, err)
	}
	password = string(secret.Data[sysdbaSecretPasswordKey])
	if password == "" {
		return "", "", fmt.Errorf("secret %q of user %s has no %q key", name, user, sysdbaSecretPasswordKey)
	}
	return password, secret.ResourceVersion, nil
}

// authenticationKey returns the spec.authentication key naming the same
// Firebird user as name, compared case-insensitively like Firebird itself
// does (the CRD rejects keys differing only in case), and whether there's
// one: a user whose key was only re-cased is still the same user.
func authenticationKey(instance *kubebirdv1.Instance, name string) (string, bool) {
	for user := range instance.Spec.Authentication {
		if strings.EqualFold(user, name) {
			return user, true
		}
	}
	return "", false
}

// databaseOwnerCredentials returns the user name and password db is
// created or restored with, and so owned by: SYSDBA when db.Owner is empty
// or "sysdba" (in any case), otherwise the regular user whose
// spec.authentication key it is (the CRD checks it's declared).
//
// Kubebird runs isql and gbak in the Firebird container without a host,
// so they open the database file with the embedded engine, which trusts
// the user name it's given: the database is owned by that user, who needs
// no CREATE DATABASE privilege for it. That privilege couldn't be granted
// anyway: it lives in the security database, which the running server
// keeps open (confirmed against the 3.0.14, 4.0.3 and 5.0.4 images).
func (r *InstanceReconciler) databaseOwnerCredentials(ctx context.Context, instance *kubebirdv1.Instance, db kubebirdv1.DatabaseSpec, sysdbaPassword string) (string, string, error) {
	if db.Owner == "" || strings.EqualFold(db.Owner, kubebirdv1.SysdbaUserName) {
		return sysdbaUsername, sysdbaPassword, nil
	}
	if _, ok := instance.Spec.Authentication[db.Owner]; !ok {
		return "", "", fmt.Errorf("owner %s of database %q isn't declared in spec.authentication", db.Owner, db.Name)
	}
	password, _, err := r.userSecret(ctx, instance, db.Owner)
	if err != nil {
		return "", "", err
	}
	return db.Owner, password, nil
}

// reconcileUsers makes the security database's regular users match
// spec.authentication, through the running server's Services API: a user
// not yet in status.users, or whose Secret changed since its password was
// last applied (its resourceVersion differs from the recorded one), is
// created or given the Secret's current password, and one in status.users
// but no longer in spec.authentication is deleted. Only users recorded in
// status.users are ever deleted, so users created by other means are left
// alone. A password changed by other means than the Secret isn't put back
// until the Secret itself changes.
//
// Unlike SYSDBA's (see mutateStatefulSet), a regular user's password is
// changed on the live server, without restarting the pod: SYSDBA's own
// credentials are all it takes.
func (r *InstanceReconciler) reconcileUsers(ctx context.Context, instance *kubebirdv1.Instance) error {
	users := regularUsers(instance)
	if len(users) == 0 && len(instance.Status.Users) == 0 {
		return nil
	}

	applied := make(map[string]string, len(instance.Status.Users))
	for _, u := range instance.Status.Users {
		applied[u.Name] = u.SecretVersion
	}

	type pendingUser struct{ name, password, version string }
	pending := make([]pendingUser, 0, len(users))
	statuses := make([]kubebirdv1.UserStatus, 0, len(users))
	for _, user := range users {
		password, version, err := r.userSecret(ctx, instance, user)
		if err != nil {
			return err
		}
		if v, ok := applied[user]; !ok || v != version {
			pending = append(pending, pendingUser{user, password, version})
		}
		statuses = append(statuses, kubebirdv1.UserStatus{Name: user, SecretVersion: version})
	}

	removed := make([]string, 0, len(instance.Status.Users))
	for _, u := range instance.Status.Users {
		if !slices.Contains(users, u.Name) {
			removed = append(removed, u.Name)
		}
	}

	if len(pending) == 0 && len(removed) == 0 {
		return nil
	}

	sysdbaPassword, err := r.sysdbaPassword(ctx, instance)
	if err != nil {
		return err
	}
	podName := firebirdPodName(instance)

	for _, u := range pending {
		if err := r.applyFirebirdUser(ctx, instance, podName, sysdbaPassword, u.name, u.password); err != nil {
			return err
		}
	}
	for _, user := range removed {
		if _, ok := authenticationKey(instance, user); ok {
			// Renamed only in letter case: still the same Firebird user,
			// just applied above under its new key.
			continue
		}
		if err := r.deleteFirebirdUser(ctx, instance, podName, sysdbaPassword, user); err != nil {
			return err
		}
	}

	instance.Status.Users = statuses
	return r.Status().Update(ctx, instance)
}

// serviceManagerCommand returns the fbsvcmgr command line running action
// (strings, or a secret for a password) against the instance's own server
// as SYSDBA, for execInPodWithSecrets.
func serviceManagerCommand(sysdbaPassword string, action ...any) []any {
	return append([]any{binFbsvcmgr, serviceManagerConn, flagUser, sysdbaUsername, flagPassword, secret(sysdbaPassword)}, action...)
}

// applyFirebirdUser creates user in the security database with password,
// or, when it's already there (e.g. restored from a backup with the
// security database), gives it password instead: -action_add_user fails on
// an existing user, so whether it exists is checked first.
func (r *InstanceReconciler) applyFirebirdUser(ctx context.Context, instance *kubebirdv1.Instance, podName, sysdbaPassword, user, password string) error {
	out, err := r.execInPodWithSecrets(ctx, instance.Namespace, podName, "",
		serviceManagerCommand(sysdbaPassword, actionDisplayUser, flagSecUsername, user)...)
	if err != nil {
		return fmt.Errorf("failed to look up Firebird user %s: %w", user, err)
	}

	action, verb := actionAddUser, "Created"
	if displayedUserExists(out, user) {
		action, verb = actionModifyUser, "Updated"
	}
	if _, err := r.execInPodWithSecrets(ctx, instance.Namespace, podName, "",
		serviceManagerCommand(sysdbaPassword, action, flagSecUsername, user, flagSecPassword, secret(password))...); err != nil {
		return fmt.Errorf("failed to apply Firebird user %s: %w", user, err)
	}
	logf.FromContext(ctx).Info(verb+" Firebird user from its Secret", "user", user)
	return nil
}

// deleteFirebirdUser deletes user from the security database, if it's
// still there.
func (r *InstanceReconciler) deleteFirebirdUser(ctx context.Context, instance *kubebirdv1.Instance, podName, sysdbaPassword, user string) error {
	out, err := r.execInPodWithSecrets(ctx, instance.Namespace, podName, "",
		serviceManagerCommand(sysdbaPassword, actionDisplayUser, flagSecUsername, user)...)
	if err != nil {
		return fmt.Errorf("failed to look up Firebird user %s: %w", user, err)
	}
	if !displayedUserExists(out, user) {
		return nil
	}
	if _, err := r.execInPodWithSecrets(ctx, instance.Namespace, podName, "",
		serviceManagerCommand(sysdbaPassword, actionDeleteUser, flagSecUsername, user)...); err != nil {
		return fmt.Errorf("failed to delete Firebird user %s: %w", user, err)
	}
	logf.FromContext(ctx).Info("Deleted Firebird user no longer in spec.authentication", "user", user)
	return nil
}

// displayedUserExists reports whether fbsvcmgr -action_display_user's
// output lists user. It prints a header and a row starting with the
// (uppercased) user name when the user exists, but nothing at all, still
// exiting 0, when it doesn't (confirmed against the 3.0.14, 4.0.3 and
// 5.0.4 images).
func displayedUserExists(out, user string) bool {
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		if fields := strings.Fields(scanner.Text()); len(fields) > 0 && strings.EqualFold(fields[0], user) {
			return true
		}
	}
	return false
}
