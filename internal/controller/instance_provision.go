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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"path"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kubebirdv1 "github.com/henryx/kubebird/api/v1"
)

// securityDatabaseDir is where the firebirdsql/firebird image keeps the
// server's security database (securityN.fdb, N being the major version).
const securityDatabaseDir = "/usr/local/firebird"

// reconcileDatabases exec's isql inside the Firebird pod, once it is
// ready, to run CREATE DATABASE/CREATE SHADOW for each entry in
// instance.Spec.Databases not yet recorded in instance.Status.Databases,
// and DROP DATABASE for each entry in instance.Status.Databases no longer
// present in instance.Spec.Databases.
//
// Provisioning happens this way, rather than via a mounted init script,
// because the nakagami/firebirdsql Go driver's own database-creation path
// hardcodes page_size=4096 and always sets isc_dpb_overwrite, which would
// silently ignore spec.databases[].pageSize and risk clobbering an
// existing database file; isql over exec preserves full fidelity to the
// CRD fields.
func (r *InstanceReconciler) reconcileDatabases(ctx context.Context, instance *kubebirdv1.Instance, sts *appsv1.StatefulSet) error {
	if sts.Status.ReadyReplicas == 0 {
		return nil
	}

	desired := make(map[string]bool, len(instance.Spec.Databases))
	for _, db := range instance.Spec.Databases {
		desired[db.Name] = true
	}

	created := make(map[string]bool, len(instance.Status.Databases))
	for _, name := range instance.Status.Databases {
		created[name] = true
	}

	pending := make([]kubebirdv1.DatabaseSpec, 0, len(instance.Spec.Databases))
	for _, db := range instance.Spec.Databases {
		if !created[db.Name] {
			pending = append(pending, db)
		}
	}

	removed := make([]string, 0, len(instance.Status.Databases))
	for _, name := range instance.Status.Databases {
		if !desired[name] {
			removed = append(removed, name)
		}
	}

	if len(pending) == 0 && len(removed) == 0 {
		return nil
	}

	password, err := r.sysdbaPassword(ctx, instance)
	if err != nil {
		return err
	}

	podName := instance.Name + "-0"
	sysdbaCommand := []string{"isql", "-user", sysdbaUsername, "-password", password}

	for _, name := range removed {
		dropCommand := append(append([]string{}, sysdbaCommand...), path.Join(primaryDataMountPath, name))
		if err := r.execIsql(ctx, instance.Namespace, podName, dropCommand, databaseDropScript); err != nil {
			return fmt.Errorf("failed to drop database %q: %w", name, err)
		}
		logf.FromContext(ctx).Info("Dropped database", "database", name)
	}

	for _, db := range pending {
		if err := r.execIsql(ctx, instance.Namespace, podName, sysdbaCommand, databaseCreateScript(db)); err != nil {
			return fmt.Errorf("failed to create database %q: %w", db.Name, err)
		}
		logf.FromContext(ctx).Info("Created database", "database", db.Name)
	}

	removedSet := make(map[string]bool, len(removed))
	for _, name := range removed {
		removedSet[name] = true
	}
	databases := make([]string, 0, len(instance.Status.Databases)+len(pending))
	for _, name := range instance.Status.Databases {
		if !removedSet[name] {
			databases = append(databases, name)
		}
	}
	for _, db := range pending {
		databases = append(databases, db.Name)
	}
	instance.Status.Databases = databases
	instance.Status.DatabaseCount = int32(len(databases))

	return r.Status().Update(ctx, instance)
}

// reconcileSysdbaPassword pushes the SYSDBA password in the referenced
// Secret to the live server once it drifts from the password Kubebird
// last applied.
//
// Firebird has no way to change a user's password without either the
// current password or an OS-trusted local connection, and once the user
// overwrites the Secret the previous password is gone from Kubernetes
// state. So rather than caching it, the change is made by exec-ing isql
// directly against the security database file (an embedded connection,
// not the usual host:port TCP one) so that root inside the container -
// the same account that owns the security database - is trusted without
// a password, matching how gsec/isql are used locally to recover a lost
// SYSDBA password.
func (r *InstanceReconciler) reconcileSysdbaPassword(ctx context.Context, instance *kubebirdv1.Instance, sts *appsv1.StatefulSet) error {
	if sts.Status.ReadyReplicas == 0 {
		return nil
	}

	password, err := r.sysdbaPassword(ctx, instance)
	if err != nil {
		return err
	}
	hash := sha256Hex(password)
	if hash == instance.Status.SysdbaPasswordHash {
		return nil
	}

	if instance.Status.SysdbaPasswordHash != "" {
		podName := instance.Name + "-0"
		// No -user/-password: a direct (embedded) connection to the
		// security database file, rather than the usual host:port one,
		// is trusted based on the OS user running isql instead of
		// requiring a password - the same mechanism admins rely on to
		// recover a lost SYSDBA password locally.
		embeddedCommand := []string{"isql", securityDatabasePath(instance)}
		script := fmt.Sprintf("ALTER USER %s PASSWORD '%s';\nQUIT;\n", sysdbaUsername, password)
		if err := r.execIsql(ctx, instance.Namespace, podName, embeddedCommand, script); err != nil {
			return fmt.Errorf("failed to rotate SYSDBA password: %w", err)
		}
		logf.FromContext(ctx).Info("Rotated SYSDBA password on the live server")
	}

	instance.Status.SysdbaPasswordHash = hash
	return r.Status().Update(ctx, instance)
}

// securityDatabasePath returns the in-container path of the security
// database matching the instance's Firebird major version, e.g.
// "/usr/local/firebird/security3.fdb" for version "3.0.14".
func securityDatabasePath(instance *kubebirdv1.Instance) string {
	return path.Join(securityDatabaseDir, securityDatabaseFileName(instance))
}

// securityDatabaseFileName returns the security database's file name for
// the instance's Firebird major version, e.g. "security3.fdb" for version
// "3.0.14".
func securityDatabaseFileName(instance *kubebirdv1.Instance) string {
	major, _, _ := strings.Cut(instance.Spec.Version, ".")
	if major == "" {
		major = "4"
	}
	return fmt.Sprintf("security%s.fdb", major)
}

// sha256Hex returns the hex-encoded SHA-256 digest of s.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// sysdbaPassword reads the current SYSDBA password from the Secret
// referenced by instance.Spec.Authentication.Sysdba.SecretRef.
func (r *InstanceReconciler) sysdbaPassword(ctx context.Context, instance *kubebirdv1.Instance) (string, error) {
	secretRef := sysdbaSecretRefName(instance)
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: secretRef, Namespace: instance.Namespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("SYSDBA Secret %q not found", secretRef)
		}
		return "", fmt.Errorf("failed to get SYSDBA Secret %q: %w", secretRef, err)
	}
	return string(secret.Data[sysdbaSecretPasswordKey]), nil
}

// databaseDropScript is the isql script that drops the database isql is
// connected to (see reconcileDatabases, which connects by passing the
// database's path as isql's positional argument). Firebird removes any
// attached shadow files along with it, so no separate cleanup is needed
// for databases created with shadow: true.
const databaseDropScript = "DROP DATABASE;\nQUIT;\n"

// databaseCreateScript renders the isql script that creates a single
// database, applying its page size, charset and collation, and adding a
// shadow file when the database requests one.
func databaseCreateScript(db kubebirdv1.DatabaseSpec) string {
	var b strings.Builder

	dbPath := path.Join(primaryDataMountPath, db.Name)
	fmt.Fprintf(&b, "CREATE DATABASE '%s' PAGE_SIZE %d DEFAULT CHARACTER SET %s COLLATION %s;\n",
		dbPath, db.PageSize, db.Charset, db.Collation)

	if db.Shadow {
		shadowPath := path.Join(shadowDataMountPath, db.Name)
		fmt.Fprintf(&b, "CREATE SHADOW 1 '%s';\n", shadowPath)
	}
	b.WriteString("QUIT;\n")

	return b.String()
}

// execIsql runs command (an isql invocation) inside the firebird
// container of podName, piping script in as its stdin.
func (r *InstanceReconciler) execIsql(ctx context.Context, namespace, podName string, command []string, script string) error {
	req := r.ClientSet.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(namespace).
		Name(podName).
		SubResource("exec")
	req.VersionedParams(&corev1.PodExecOptions{
		Container: containerName,
		Command:   command,
		Stdin:     true,
		Stdout:    true,
		Stderr:    true,
	}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(r.RestConfig, http.MethodPost, req.URL())
	if err != nil {
		return fmt.Errorf("failed to create exec executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  strings.NewReader(script),
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return fmt.Errorf("isql exec failed: %w (stderr: %s)", err, stderr.String())
	}
	return nil
}
