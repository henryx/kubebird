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
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kubebirdv1 "github.com/henryx/kubebird/api/v1"
)

// securityDatabaseDir is where the firebirdsql/firebird image itself is
// installed, including its baked-in default security database
// (securityN.fdb, N being the major version) — used only as the seed
// source for a brand-new instance (see securityDatabaseImageDefaultPath);
// the live security database is kept on the primary PVC instead (see
// securityDatabasePath).
const securityDatabaseDir = "/opt/firebird"

// isql/gbak binaries and flags shared across the exec'd commands in this
// file.
const (
	binIsql = "isql"
	binGbak = "gbak"

	flagUser     = "-user"
	flagPassword = "-password"
	flagVerify   = "-verify"
)

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

	podName := firebirdPodName(instance)
	sysdbaCommand := []string{binIsql, flagUser, sysdbaUsername, flagPassword, password}

	// Only worth checking when something in spec.databases is actually
	// being created/restored: that's the event (an Instance recreated, or
	// a database dropped after spec.backup left its backup behind) this warning
	// exists to surface, and skipping it otherwise keeps a steady-state
	// reconcile (pending and removed both empty, returned above) from
	// paying for an extra exec into the pod.
	warning := ""
	if len(pending) > 0 && backupVolumeSpec(instance) != nil {
		orphaned, err := r.orphanedBackups(ctx, instance, podName)
		if err != nil {
			return fmt.Errorf("failed to check for orphaned backups: %w", err)
		}
		if len(orphaned) > 0 {
			warning = fmt.Sprintf(
				"the backup volume has a backup for %s, but it's no longer in spec.databases, so it wasn't restored",
				strings.Join(orphaned, ", "))
		}
	}

	for _, name := range removed {
		dropCommand := append(append([]string{}, sysdbaCommand...), path.Join(primaryDataMountPath, name))
		if err := r.execInPod(ctx, instance.Namespace, podName, dropCommand, databaseDropScript); err != nil {
			return fmt.Errorf("failed to drop database %q: %w", name, err)
		}
		logf.FromContext(ctx).Info("Dropped database", "database", name)
	}

	for _, db := range pending {
		// A previous reconcile may have already run CREATE DATABASE
		// against the primary PVC and then failed before recording it in
		// status.databases (e.g. a crash between the exec and the
		// Status().Update below). CREATE DATABASE would fail (or, via the
		// Go driver, silently overwrite it) against an existing file, so
		// check first and just register it if it's already there instead
		// of trying to recreate it.
		exists, err := r.databaseFileExists(ctx, instance.Namespace, podName, path.Join(primaryDataMountPath, db.Name))
		if err != nil {
			return fmt.Errorf("failed to check whether database %q already exists: %w", db.Name, err)
		}
		if exists {
			logf.FromContext(ctx).Info("Database file already exists on the primary PVC, registering it without recreating", "database", db.Name)
			continue
		}

		if backupVolumeSpec(instance) != nil {
			restored, err := r.restoreDatabaseIfBackedUp(ctx, instance, podName, password, db)
			if err != nil {
				return fmt.Errorf("failed to restore database %q from backup: %w", db.Name, err)
			}
			if restored {
				logf.FromContext(ctx).Info("Restored database from its backup", "database", db.Name)
				continue
			}
		}

		if err := r.execInPod(ctx, instance.Namespace, podName, sysdbaCommand, databaseCreateScript(db)); err != nil {
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
	instance.Status.Warning = warning

	return r.Status().Update(ctx, instance)
}

// securityDatabasePath returns the in-container path of the security
// database matching the instance's Firebird major version, on the primary
// PVC, e.g. "/var/lib/firebird/data/security3.fdb" for version "3.0.14" —
// wired up as the engine's live SecurityDatabase via the
// FIREBIRD_CONF_SecurityDatabase env var (mutateStatefulSet) and the
// security.db alias (mutateAliasesConfigMap), and populated there by the
// security-database-init initContainer before the firebird container ever
// starts (see securityDatabaseInitScript).
func securityDatabasePath(instance *kubebirdv1.Instance) string {
	return path.Join(primaryDataMountPath, securityDatabaseFileName(instance))
}

// securityDatabaseImageDefaultPath returns the path of the security
// database baked into the firebirdsql/firebird image itself, e.g.
// "/opt/firebird/security3.fdb" for version "3.0.14" — used only as the
// security-database-init initContainer's seed source when the primary PVC
// doesn't already have one (see securityDatabaseInitScript).
func securityDatabaseImageDefaultPath(instance *kubebirdv1.Instance) string {
	return path.Join(securityDatabaseDir, securityDatabaseFileName(instance))
}

// securityDatabaseFileName returns the security database's file name for
// the instance's Firebird major version, e.g. "security3.fdb" for version
// "3.0.14".
func securityDatabaseFileName(instance *kubebirdv1.Instance) string {
	return securityDatabaseFileNameForVersion(instance.Spec.Version)
}

// securityDatabaseFileNameForVersion returns the security database's file
// name for the given Firebird version, e.g. "security3.fdb" for "3.0.14" —
// what securityDatabaseFileName returns for spec.version, but for another
// version too, e.g. the one still running while a major version upgrade
// backs it up (see reconcileMajorUpgrade).
func securityDatabaseFileNameForVersion(version string) string {
	major, _, _ := strings.Cut(version, ".")
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

// securityDatabaseBackupPathForVersion returns where backupDatabasesOffline
// leaves the security database backup of the given Firebird version
// inside the backup volume's base subdirectory (see instanceBackupDir),
// e.g. "/var/lib/firebird/backup/base/security3.fbk" for "3.0.14" — a gbak
// archive, just like every other database's backup (see backupFileName),
// restored by securityDatabaseInitScript with its own local gbak restore.
func securityDatabaseBackupPathForVersion(version string) string {
	return path.Join(instanceBackupDir(), backupFileName(securityDatabaseFileNameForVersion(version)))
}

// securityDatabaseBackupPattern matches the file name of a security
// database backup of any Firebird major version (e.g. "security3.fbk"),
// as left in the backup volume's base subdirectory by backupDatabasesOffline.
// A backup of an older major version stays there after an upgrade (see
// securityDatabaseInitScript), so none of them is ever a spec.databases
// entry's (see orphanedBackups).
var securityDatabaseBackupPattern = regexp.MustCompile(`^security[0-9]+\.fbk$`)

// securityMigrationScript is the isql script, from the Firebird 4.0
// migration guide's "Migrating user list from Firebird 3.0"
// (https://ib-aid.com/download/docs/fb4migrationguide.html#_migrating_user_list),
// that copies every user but SYSDBA from a Firebird 3.0 security database,
// restored by the new version's gbak as a separate database at %s, into
// the new version's own security database isql is connected to: both the
// Srp users (PLG$SRP, password verifiers included, so passwords carry over)
// and the Legacy_UserManager ones (PLG$USERS). SYSDBA is left out because
// the image's entrypoint sets it from FIREBIRD_ROOT_PASSWORD on every
// start anyway. The guide prefers this over restoring the old security
// database as the new one, which would lose the new version's own changes
// to it. See securityDatabaseInitScript.
const securityMigrationScript = `set term ^;
EXECUTE BLOCK
AS
  DECLARE SRC_SEC_DB     VARCHAR(255) = '%s';
  DECLARE SRC_SEC_USER   VARCHAR(63) = 'SYSDBA';
  DECLARE PLG$USER_NAME  SEC$USER_NAME;
  DECLARE PLG$VERIFIER   VARCHAR(128) CHARACTER SET OCTETS;
  DECLARE PLG$SALT       VARCHAR(32) CHARACTER SET OCTETS;
  DECLARE PLG$COMMENT    BLOB SUB_TYPE TEXT CHARACTER SET UTF8;
  DECLARE PLG$FIRST      SEC$NAME_PART;
  DECLARE PLG$MIDDLE     SEC$NAME_PART;
  DECLARE PLG$LAST       SEC$NAME_PART;
  DECLARE PLG$ATTRIBUTES BLOB SUB_TYPE TEXT CHARACTER SET UTF8;
  DECLARE PLG$ACTIVE     BOOLEAN;
  DECLARE PLG$GROUP_NAME SEC$USER_NAME;
  DECLARE PLG$UID        PLG$ID;
  DECLARE PLG$GID        PLG$ID;
  DECLARE PLG$PASSWD     PLG$PASSWD;
BEGIN
  FOR EXECUTE STATEMENT Q'!
      SELECT PLG$USER_NAME, PLG$VERIFIER, PLG$SALT, PLG$COMMENT, PLG$FIRST,
             PLG$MIDDLE, PLG$LAST, PLG$ATTRIBUTES, PLG$ACTIVE
      FROM PLG$SRP
      WHERE PLG$USER_NAME <> 'SYSDBA'
!'
      ON EXTERNAL :SRC_SEC_DB
      AS USER :SRC_SEC_USER
      INTO :PLG$USER_NAME, :PLG$VERIFIER, :PLG$SALT, :PLG$COMMENT, :PLG$FIRST,
           :PLG$MIDDLE, :PLG$LAST, :PLG$ATTRIBUTES, :PLG$ACTIVE
  DO
    INSERT INTO PLG$SRP (PLG$USER_NAME, PLG$VERIFIER, PLG$SALT, PLG$COMMENT, PLG$FIRST,
                         PLG$MIDDLE, PLG$LAST, PLG$ATTRIBUTES, PLG$ACTIVE)
    VALUES (:PLG$USER_NAME, :PLG$VERIFIER, :PLG$SALT, :PLG$COMMENT, :PLG$FIRST,
            :PLG$MIDDLE, :PLG$LAST, :PLG$ATTRIBUTES, :PLG$ACTIVE);
  FOR EXECUTE STATEMENT Q'!
      SELECT PLG$USER_NAME, PLG$GROUP_NAME, PLG$UID, PLG$GID, PLG$PASSWD,
             PLG$COMMENT, PLG$FIRST_NAME, PLG$MIDDLE_NAME, PLG$LAST_NAME
      FROM PLG$USERS
      WHERE PLG$USER_NAME <> 'SYSDBA'
!'
      ON EXTERNAL :SRC_SEC_DB
      AS USER :SRC_SEC_USER
      INTO :PLG$USER_NAME, :PLG$GROUP_NAME, :PLG$UID, :PLG$GID, :PLG$PASSWD,
           :PLG$COMMENT, :PLG$FIRST, :PLG$MIDDLE, :PLG$LAST
  DO
    INSERT INTO PLG$USERS (PLG$USER_NAME, PLG$GROUP_NAME, PLG$UID, PLG$GID, PLG$PASSWD,
                           PLG$COMMENT, PLG$FIRST_NAME, PLG$MIDDLE_NAME, PLG$LAST_NAME)
    VALUES (:PLG$USER_NAME, :PLG$GROUP_NAME, :PLG$UID, :PLG$GID, :PLG$PASSWD,
            :PLG$COMMENT, :PLG$FIRST, :PLG$MIDDLE, :PLG$LAST);
END^
set term ;^
commit;
exit;
`

// securityDatabaseInitScript renders the shell script run by the
// security-database-init initContainer (see mutateStatefulSet) before the
// firebird container starts, if the security database isn't already
// present on the primary PVC — the normal case is a brand-new PVC, since
// releasePrimaryAndShadowStorage deletes the primary PVC on Instance
// deletion and on a major version upgrade (see reconcileMajorUpgrade), so
// a freshly created, recreated or upgraded Instance always needs seeding;
// the check only ever finds one already there across a plain pod restart.
// When a backup volume exists (backupVolumeSpec), it's seeded from the
// most recently written security database backup in the backup volume's
// base subdirectory (backupDatabasesOffline's "security<M>.fbk", see
// securityDatabaseBackupPathForVersion) of a Firebird major version M no
// newer than spec.version's own N — the most recent one, rather than
// security<N>.fbk first, since right after an upgrade from M the backup
// of M is the one just taken, while a security<N>.fbk can only be one left
// behind long before:
//
//  1. M is N (e.g. this Instance is recreating one deleted earlier under
//     the same name), or M is older than N but newer than 3 (an upgrade,
//     e.g. 4 to 5): restored with a local "gbak -create" (no host given,
//     so it runs against the image's own local engine directly, the only
//     option anyway since no server is listening yet), carrying over its
//     users and roles. This needs no -user/-password, since there's no
//     pre-existing security database yet for gbak to authenticate the
//     connection against — confirmed against the actual images, as is
//     restoring a 4.0 one with 5.0's gbak;
//  2. M is 3 and N newer (an upgrade from Firebird 3, in place or by
//     recreating the Instance): following the Firebird 4.0 migration
//     guide, the backup is restored as a separate database, the image's
//     own default security database is seeded as in 3., and
//     securityMigrationScript, run by isql in embedded mode (again, no
//     server yet, and -b so any failed statement fails the script),
//     copies the users from the former into the latter.
//     Built under a temporary name and only then renamed into place, so a
//     failure partway leaves nothing behind for the next pod start to
//     mistake for a finished security database. Confirmed against the
//     actual 3.0.14, 4.0.3 and 5.0.4 images that a user created on 3.0
//     logs in with its old password on 4.0 and 5.0 afterwards;
//  3. otherwise (no backup volume, or no such backup), the image's own
//     baked-in default (securityDatabaseImageDefaultPath) — a plain file
//     copy, since it's a raw database file rather than a gbak archive.
//
// The script depends only on spec (version and backup volume), never on
// status.upgrade, so finishing an upgrade doesn't change the pod template
// and restart the pod once more.
func securityDatabaseInitScript(instance *kubebirdv1.Instance) string {
	if backupVolumeSpec(instance) == nil {
		return fmt.Sprintf(`set -e
if [ ! -f %[1]q ]; then
	cp %[2]q %[1]q
fi
`, securityDatabasePath(instance), securityDatabaseImageDefaultPath(instance))
	}

	major, _, _ := strings.Cut(securityDatabaseFileName(instance), ".")
	major = strings.TrimPrefix(major, "security")
	dst := securityDatabasePath(instance)
	migrated := path.Join(primaryDataMountPath, ".security-migrated.fdb")
	source := path.Join(primaryDataMountPath, ".security-migration-source.fdb")
	return fmt.Sprintf(`set -e
if [ ! -f %[1]q ]; then
	SRC=
	SRC_MAJOR=
	for f in $(ls -t %[3]q/security*.fbk 2>/dev/null); do
		m=$(basename "$f" .fbk)
		m=${m#security}
		case "$m" in ''|*[!0-9]*) continue ;; esac
		if [ "$m" -le %[4]s ]; then
			SRC=$f
			SRC_MAJOR=$m
			break
		fi
	done
	if [ -z "$SRC" ]; then
		cp %[2]q %[1]q
	elif [ "$SRC_MAJOR" -eq 3 ] && [ %[4]s -gt 3 ]; then
		echo "Migrating the users of $SRC into Firebird %[4]s's security database"
		rm -f %[6]q %[7]q
		cp %[2]q %[6]q
		%[5]s -create "$SRC" %[7]q
		%[8]s -q -b %[9]s %[10]s %[6]q <<'SQLEOF'
%[11]sSQLEOF
		rm -f %[7]q
		mv %[6]q %[1]q
	else
		echo "Restoring the security database from $SRC"
		%[5]s -create -verify "$SRC" %[1]q
	fi
fi
`, dst, securityDatabaseImageDefaultPath(instance), instanceBackupDir(), major, binGbak,
		migrated, source, binIsql, flagUser, sysdbaUsername, fmt.Sprintf(securityMigrationScript, source))
}

// execInPod runs command inside the firebird container of podName,
// piping stdin in as its stdin. Used both for isql invocations (stdin
// carrying the script to run) and for gbak, which takes no stdin.
func (r *InstanceReconciler) execInPod(ctx context.Context, namespace, podName string, command []string, stdin string) error {
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
		Stdin:  strings.NewReader(stdin),
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return fmt.Errorf("exec failed: %w (stderr: %s)", err, stderr.String())
	}
	return nil
}

// execInPodOutput behaves like execInPod, but returns the command's
// stdout instead of discarding it — used by orphanedBackups to list the
// backup directory's contents.
func (r *InstanceReconciler) execInPodOutput(ctx context.Context, namespace, podName string, command []string) (string, error) {
	req := r.ClientSet.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(namespace).
		Name(podName).
		SubResource("exec")
	req.VersionedParams(&corev1.PodExecOptions{
		Container: containerName,
		Command:   command,
		Stdout:    true,
		Stderr:    true,
	}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(r.RestConfig, http.MethodPost, req.URL())
	if err != nil {
		return "", fmt.Errorf("failed to create exec executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return "", fmt.Errorf("exec failed: %w (stderr: %s)", err, stderr.String())
	}
	return stdout.String(), nil
}

// orphanedBackups lists the databases whose backup file sits in the
// instance's backup directory (see instanceBackupDir) without a matching
// entry in spec.databases — e.g. because the Instance was recreated with a
// different database list, or a database was dropped from spec.databases
// after spec.backup was enabled. Reports the database names those
// orphaned backups belong to (reversing backupFileName), sorted. The
// security database's own backups (securityDatabaseBackupPattern, of the
// current Firebird major version or, after an upgrade, an older one) are
// excluded even though they share the same .fbk directory and naming
// scheme: they have no corresponding spec.databases entry to match by
// design, so they would otherwise always show up as orphaned.
func (r *InstanceReconciler) orphanedBackups(ctx context.Context, instance *kubebirdv1.Instance, podName string) ([]string, error) {
	desired := make(map[string]bool, len(instance.Spec.Databases))
	for _, db := range instance.Spec.Databases {
		desired[db.Name] = true
	}

	dir := instanceBackupDir()
	// Redirects stderr to /dev/null and always exits 0: the directory
	// might not exist yet (e.g. no deletion has ever written a backup
	// into it), in which case there's simply nothing orphaned to report.
	output, err := r.execInPodOutput(ctx, instance.Namespace, podName,
		[]string{"sh", "-c", fmt.Sprintf("ls -1 %s 2>/dev/null || true", dir)})
	if err != nil {
		return nil, fmt.Errorf("failed to list backup directory %q: %w", dir, err)
	}

	var orphaned []string
	for name := range strings.FieldsSeq(output) {
		if !strings.HasSuffix(name, ".fbk") || securityDatabaseBackupPattern.MatchString(name) {
			continue
		}
		if dbName := strings.TrimSuffix(name, ".fbk") + ".fdb"; !desired[dbName] {
			orphaned = append(orphaned, dbName)
		}
	}
	slices.Sort(orphaned)
	return orphaned, nil
}

// databaseFileExists reports whether filePath already exists inside
// podName's firebird container, by exec-ing "test -f" and inspecting its
// exit code. Used both for a database's own file (see reconcileDatabases)
// and for its backup file (see restoreDatabaseIfBackedUp).
func (r *InstanceReconciler) databaseFileExists(ctx context.Context, namespace, podName, filePath string) (bool, error) {
	err := r.execInPod(ctx, namespace, podName, []string{"test", "-f", filePath}, "")
	if err == nil {
		return true, nil
	}
	var exitErr utilexec.CodeExitError
	if errors.As(err, &exitErr) {
		return false, nil
	}
	return false, err
}

// restoreDatabaseIfBackedUp restores db from its backup file in the
// backup volume's base subdirectory (instanceBackupDir), if one exists
// there — e.g. because an earlier Instance with this same name was
// deleted with a backup volume configured (see backupDatabases), and
// this Instance is recreating it. Reports whether a backup was found and
// restored.
func (r *InstanceReconciler) restoreDatabaseIfBackedUp(ctx context.Context, instance *kubebirdv1.Instance, podName, password string, db kubebirdv1.DatabaseSpec) (bool, error) {
	backupPath := path.Join(instanceBackupDir(), backupFileName(db.Name))
	exists, err := r.databaseFileExists(ctx, instance.Namespace, podName, backupPath)
	if err != nil {
		return false, fmt.Errorf("failed to check for a backup at %q: %w", backupPath, err)
	}
	if !exists {
		return false, nil
	}

	dbPath := path.Join(primaryDataMountPath, db.Name)
	restoreCommand := []string{binGbak, "-create", flagVerify, flagUser, sysdbaUsername, flagPassword, password, backupPath, dbPath}
	if err := r.execInPod(ctx, instance.Namespace, podName, restoreCommand, ""); err != nil {
		return false, fmt.Errorf("gbak restore failed: %w", err)
	}

	if db.Shadow {
		shadowPath := path.Join(shadowDataMountPath, db.Name)
		connectCommand := []string{binIsql, flagUser, sysdbaUsername, flagPassword, password, dbPath}
		script := fmt.Sprintf("CREATE SHADOW 1 '%s';\nQUIT;\n", shadowPath)
		if err := r.execInPod(ctx, instance.Namespace, podName, connectCommand, script); err != nil {
			return false, fmt.Errorf("failed to recreate shadow file after restore: %w", err)
		}
	}

	return true, nil
}

// databaseBackupPodName returns the name of the short-lived helper Pod
// backupDatabasesOffline creates to back up spec.databases and the
// security database once the StatefulSet's own pod has stopped.
func databaseBackupPodName(instance *kubebirdv1.Instance) string {
	return instance.Name + "-database-backup"
}

// backupImage returns the container image used by createDatabaseBackupPod,
// preferring spec.backup.image and falling back to
// "<spec.image>:<spec.version>" when it's unset.
func backupImage(instance *kubebirdv1.Instance) string {
	if instance.Spec.Backup.Image != "" {
		return instance.Spec.Backup.Image
	}
	return fmt.Sprintf("%s:%s", instance.Spec.Image, instance.Spec.Version)
}

// databaseBackupImage returns the image createDatabaseBackupPod runs to
// back up databases written by the given Firebird version: backupImage
// for spec.version itself, but "<spec.image>:<version>" for any other,
// ignoring spec.backup.image, since only that version's own engine is sure
// to open its databases — e.g. a 4.0 engine refuses a 3.0 database's
// on-disk structure, so a major version upgrade (see reconcileMajorUpgrade)
// must back up with the version it's upgrading from.
func databaseBackupImage(instance *kubebirdv1.Instance, version string) string {
	if version == instance.Spec.Version {
		return backupImage(instance)
	}
	return fmt.Sprintf("%s:%s", instance.Spec.Image, version)
}

// databaseBackupScript renders the shell script run by the helper Pod
// createDatabaseBackupPod creates: a local (no host) "gbak -backup" for
// every database in instance.Status.Databases, plus the security
// database itself, each writing into the backup volume's base subdirectory
// (see instanceBackupDir). No -user/-password is needed for any of it:
// run as root, a local gbak backup of an already-existing database
// (security database included) doesn't validate them against
// anything — confirmed against the actual image, not just documentation.
//
// version is the Firebird version that wrote the databases, which picks
// the security database's file name (see securityDatabaseFileNameForVersion).
func databaseBackupScript(instance *kubebirdv1.Instance, version string) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	fmt.Fprintf(&b, "mkdir -p %q\n", instanceBackupDir())
	for _, name := range instance.Status.Databases {
		src := path.Join(primaryDataMountPath, name)
		dst := path.Join(instanceBackupDir(), backupFileName(name))
		fmt.Fprintf(&b, "%s -backup -verify %q %q\n", binGbak, src, dst)
	}
	fmt.Fprintf(&b, "%s -backup -verify %q %q\n", binGbak,
		path.Join(primaryDataMountPath, securityDatabaseFileNameForVersion(version)), securityDatabaseBackupPathForVersion(version))
	return b.String()
}

// backupDatabasesOffline gbak-backs-up every database in
// instance.Status.Databases, plus the instance's security database, into
// the backup volume's base subdirectory (see instanceBackupDir), so their
// data survives even after the primary/shadow PVCs are removed (see
// releasePrimaryAndShadowStorage, always called on deletion). The
// security-database-init initContainer and restoreDatabaseIfBackedUp both
// restore these backups on a later recreate under the same name.
//
// Unlike a database that's still live, none of this can just exec gbak
// inside the running firebird container: gbak backing up the security
// database while the live server still has it open fails outright with
// "Database already opened with engine instance, incompatible with
// current" (confirmed against the actual image), and routing it through
// the services manager instead fares no better ("no permission for
// remote access to database") — the security database is apparently held
// exclusively by the engine and reachable only locally. So
// backupDatabases scales the StatefulSet to 0 replicas first,
// stopping the pod and releasing every database's live engine instance
// (not just the security database's), before calling this — which runs
// its own local gbak backups (no host given, so no live server is
// involved at all) from a short-lived helper Pod that mounts the same
// primary and backup PVCs instead of exec-ing into a running one. Doing
// every database this same way, rather than only the security database,
// means every backup in this run comes from the exact same quiesced,
// fully-stopped snapshot instead of a live one gbak's own transaction
// semantics merely made internally consistent.
//
// version is the Firebird version that wrote the databases (see
// createDatabaseBackupPod).
func (r *InstanceReconciler) backupDatabasesOffline(ctx context.Context, instance *kubebirdv1.Instance, version string) error {
	podName := databaseBackupPodName(instance)
	pod := &corev1.Pod{}
	err := r.Get(ctx, types.NamespacedName{Name: podName, Namespace: instance.Namespace}, pod)
	switch {
	case apierrors.IsNotFound(err):
		return r.createDatabaseBackupPod(ctx, instance, version)
	case err != nil:
		return fmt.Errorf("failed to get database backup Pod: %w", err)
	}

	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		logf.FromContext(ctx).Info("Backed up databases and the security database", "databases", instance.Status.Databases)
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete database backup Pod: %w", err)
		}
		return nil
	case corev1.PodFailed:
		// Deletes the failed Pod so the next reconcile recreates it fresh
		// instead of getting permanently stuck re-observing the same
		// failure forever.
		reason := podFailureReason(pod)
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to delete failed database backup Pod: %w", err)
		}
		return fmt.Errorf("database backup Pod %q failed: %s", podName, reason)
	default:
		return waiting("waiting for database backup Pod %q to complete", podName)
	}
}

// createDatabaseBackupPod creates the helper Pod backupDatabasesOffline
// polls for, running databaseBackupScript against the primary PVC,
// writing into the backup PVC — both mounted the same way
// mutateStatefulSet mounts them onto the firebird container itself, so
// the in-container paths databaseBackupScript builds resolve the same
// way either way.
//
// The "pods" RBAC marker (instance_controller.go) grants list;watch too,
// even though nothing here calls List or sets up a Watch: the manager's
// default client is a cached/informer-backed client, and the first Get or
// Create against a GVK it hasn't seen before starts a List+Watch informer
// for that whole type to populate its local cache — confirmed the hard
// way, via "pods is forbidden" on List even with get;create;delete
// already granted.
//
// The Pod runs version's own image and gbak (see databaseBackupImage and
// databaseBackupScript): spec.version, except while a major version
// upgrade is still backing up the version it's upgrading from.
func (r *InstanceReconciler) createDatabaseBackupPod(ctx context.Context, instance *kubebirdv1.Instance, version string) error {
	mounts := []corev1.VolumeMount{
		{Name: primaryVolumeName, MountPath: primaryDataMountPath},
		{Name: backupVolumeName, MountPath: backupDataMountPath},
	}
	volumes := []corev1.Volume{
		{
			Name:         primaryVolumeName,
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: primaryPVCName(instance)}},
		},
		{
			Name:         backupVolumeName,
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: backupPVCName(instance)}},
		},
	}
	if instance.Spec.Storage.Shadow != nil {
		// gbak -backup on a shadow: true database also needs its shadow
		// file to be present, even though it doesn't back it up
		// separately — confirmed against the actual image: without this
		// mount, gbak fails with "No such file or directory" trying to
		// open it.
		mounts = append(mounts, corev1.VolumeMount{Name: shadowVolumeName, MountPath: shadowDataMountPath})
		volumes = append(volumes, corev1.Volume{
			Name:         shadowVolumeName,
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: shadowPVCName(instance)}},
		})
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      databaseBackupPodName(instance),
			Namespace: instance.Namespace,
			Labels:    withFirebirdVersionLabel(instance, labelsForInstance(instance.Name)),
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:  containerName,
					Image: databaseBackupImage(instance, version),
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: ptr.To(false),
						SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Command:      []string{"sh", "-c", databaseBackupScript(instance, version)},
					VolumeMounts: mounts,
				},
			},
			Volumes: volumes,
		},
	}
	if err := controllerutil.SetControllerReference(instance, pod, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference on database backup Pod: %w", err)
	}
	if err := r.Create(ctx, pod); err != nil {
		return fmt.Errorf("failed to create database backup Pod: %w", err)
	}
	return waiting("waiting for database backup Pod %q to start", pod.Name)
}

// podFailureReason returns a human-readable reason pod's single container
// failed, preferring its own terminated reason/message — where a command's
// actual failure (e.g. gbak's stderr, surfaced via Terminated.Message when
// the container runtime captures it) shows up — over the Pod-level
// status.Reason, which is typically only set for pod-level failures like
// eviction or scheduling, not a plain non-zero exit code.
func podFailureReason(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
			if t.Message != "" {
				return t.Message
			}
			return fmt.Sprintf("%s (exit code %d)", t.Reason, t.ExitCode)
		}
	}
	if pod.Status.Reason != "" {
		return pod.Status.Reason
	}
	return "unknown reason"
}

// backupBaseDirName is the fixed subdirectory of the backup volume that
// all of an instance's own backups live under, e.g.
// "/var/lib/firebird/backup/base" (see instanceBackupDir). It doesn't need
// to be named after the Instance itself: the backup volume is already a
// PVC dedicated to this Instance (named "<instance-name>-backup"), so a
// fixed name avoids stuttering the Instance's name into the path a second
// time.
const backupBaseDirName = "base"

// instanceBackupDir returns the backup volume's dedicated subdirectory for
// an instance's own backups, e.g. "/var/lib/firebird/backup/base" (see
// backupBaseDirName).
func instanceBackupDir() string {
	return path.Join(backupDataMountPath, backupBaseDirName)
}

// backupFileName returns the gbak backup file name for a database, e.g.
// "instance.fdb" -> "instance.fbk".
func backupFileName(dbName string) string {
	return strings.TrimSuffix(dbName, ".fdb") + ".fbk"
}
