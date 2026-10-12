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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiresource "k8s.io/apimachinery/pkg/api/resource"

	kubebirdv1 "github.com/henryx/kubebird/api/v1"
)

// Test fixtures shared by this file's tests.
const (
	testFirebird5Version = "5.0.4"
	security3Backup      = "security3.fbk"
	security4Backup      = "security4.fbk"
	security5Backup      = "security5.fbk"
	restoredSecurity4    = "restored " + security4Backup
)

func TestFirebirdMajorVersion(t *testing.T) {
	for version, want := range map[string]int{
		testVersion:         3,
		upgradedTestVersion: 4,
		"5.0.4-bookworm":    5,
		"5":                 5,
		"latest":            0,
		"":                  0,
	} {
		got, ok := firebirdMajorVersion(version)
		if ok != (want != 0) || got != want {
			t.Errorf("firebirdMajorVersion(%q) = %d, %v; want %d, %v", version, got, ok, want, want != 0)
		}
	}
}

func TestStatefulSetFirebirdVersion(t *testing.T) {
	for image, want := range map[string]string{
		"firebirdsql/firebird:4.0.3":                upgradedTestVersion,
		"registry.example.com:5000/firebird:5.0.4":  testFirebird5Version,
		"registry.example.com:5000/firebird":        "",
		testImage:                                   "",
		"firebirdsql/firebird:5.0.4-bookworm":       "5.0.4-bookworm",
		"localhost:5000/firebirdsql/firebird:3.0.1": "3.0.1",
	} {
		sts := &appsv1.StatefulSet{}
		sts.Spec.Template.Spec.Containers = []corev1.Container{
			{Name: securityDatabaseInitContainerName, Image: "other:1"},
			{Name: containerName, Image: image},
		}
		if got := statefulSetFirebirdVersion(sts); got != want {
			t.Errorf("statefulSetFirebirdVersion(image %q) = %q, want %q", image, got, want)
		}
	}
}

func TestSecurityDatabaseBackupPattern(t *testing.T) {
	for name, want := range map[string]bool{
		security3Backup:  true,
		security5Backup:  true,
		"security.fbk":   false,
		"securityx.fbk":  false,
		"instance.fbk":   false,
		"security3.fdb":  false,
		"mysecurity.fbk": false,
	} {
		if got := securityDatabaseBackupPattern.MatchString(name); got != want {
			t.Errorf("securityDatabaseBackupPattern.MatchString(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestSecurityDatabaseInitScript runs securityDatabaseInitScript's
// rendered script under sh, its absolute paths moved into a temporary
// directory, against backups of different Firebird major versions in the
// backup volume's base subdirectory, with stub gbak and isql commands
// recording what they're asked to do: it must restore the most recent
// backup of a major version no newer than its own, migrate the users of a
// Firebird 3 one into a newer version's default security database per the
// migration guide, and otherwise seed the default.
func TestSecurityDatabaseInitScript(t *testing.T) {
	bin := t.TempDir()
	writeStub(t, bin, "gbak", `
# gbak -create [-verify] SRC DST
for a; do SRC=$DST; DST=$a; done
echo "restored $(basename "$SRC")" > "$DST"
`)
	writeStub(t, bin, "isql", `
# isql -q -b -user SYSDBA DB, the migration script on stdin
for a; do DB=$a; done
grep -q "ON EXTERNAL" || exit 1
echo "migrated users" >> "$DB"
`)

	type backup struct {
		name string
		age  time.Duration
	}
	for _, tc := range []struct {
		name    string
		version string
		backups []backup
		want    string
	}{
		{"no backup seeds the default", upgradedTestVersion, nil, "default security4"},
		{"same major version is restored", upgradedTestVersion, []backup{{security4Backup, time.Hour}}, restoredSecurity4},
		{"upgrade from 3 migrates the users", upgradedTestVersion, []backup{{security3Backup, time.Hour}}, "default security4\nmigrated users"},
		{"upgrade from 3 to 5 migrates the users", testFirebird5Version, []backup{{security3Backup, time.Hour}}, "default security5\nmigrated users"},
		{"upgrade from 4 to 5 is restored", testFirebird5Version, []backup{{security4Backup, time.Hour}}, restoredSecurity4},
		{
			"the most recent backup wins over the version's own older one", upgradedTestVersion,
			[]backup{{security4Backup, 2 * time.Hour}, {security3Backup, time.Hour}}, "default security4\nmigrated users",
		},
		{
			"a newer major version's backup is skipped", upgradedTestVersion,
			[]backup{{security4Backup, 2 * time.Hour}, {security5Backup, time.Hour}}, restoredSecurity4,
		},
		{"only a newer major version's backup seeds the default", upgradedTestVersion, []backup{{security5Backup, time.Hour}}, "default security4"},
		{"3 restores its own backup", testVersion, []backup{{security3Backup, time.Hour}}, "restored security3.fbk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			major, _ := firebirdMajorVersion(tc.version)
			for _, dir := range []string{"data", "backup/base", "opt"} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			defaultDB := filepath.Join(root, "opt", securityDatabaseFileNameForVersion(tc.version))
			if err := os.WriteFile(defaultDB, []byte("default security"+strings.TrimPrefix(strings.TrimSuffix(filepath.Base(defaultDB), ".fdb"), "security")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			for _, b := range tc.backups {
				p := filepath.Join(root, "backup", "base", b.name)
				if err := os.WriteFile(p, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				mtime := time.Now().Add(-b.age)
				if err := os.Chtimes(p, mtime, mtime); err != nil {
					t.Fatal(err)
				}
			}

			instance := &kubebirdv1.Instance{Spec: kubebirdv1.InstanceSpec{
				Version: tc.version,
				Backup: kubebirdv1.BackupSpec{
					Enabled: true,
					Destinations: []kubebirdv1.BackupDestinationSpec{
						{Local: &kubebirdv1.LocalBackupSpec{Storage: kubebirdv1.StorageVolumeSpec{Size: apiresource.MustParse("1Gi")}}},
					},
				},
			}}
			script := strings.NewReplacer(
				primaryDataMountPath, filepath.Join(root, "data"),
				backupDataMountPath, filepath.Join(root, "backup"),
				securityDatabaseDir, filepath.Join(root, "opt"),
			).Replace(securityDatabaseInitScript(instance))
			cmd := exec.Command("sh", "-c", script)
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("init script failed: %v\n%s", err, out)
			}

			got, err := os.ReadFile(filepath.Join(root, "data", securityDatabaseFileNameForVersion(tc.version)))
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(got)) != tc.want {
				t.Errorf("security%d.fdb = %q, want %q", major, strings.TrimSpace(string(got)), tc.want)
			}
			entries, err := os.ReadDir(filepath.Join(root, "data"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Errorf("data directory holds %v, want only the security database (no migration leftovers)", entries)
			}

			// A second run, as on a plain pod restart, leaves it alone.
			cmd = exec.Command("sh", "-c", script)
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("second init script run failed: %v\n%s", err, out)
			}
			if again, _ := os.ReadFile(filepath.Join(root, "data", securityDatabaseFileNameForVersion(tc.version))); string(again) != string(got) {
				t.Errorf("second run changed the security database to %q", again)
			}
		})
	}
}

// TestSecurityDatabaseInitScriptFailedMigration checks that a migration
// failing partway leaves no security database behind, so the next pod
// start tries again instead of starting with a user-less one.
func TestSecurityDatabaseInitScriptFailedMigration(t *testing.T) {
	bin := t.TempDir()
	writeStub(t, bin, "gbak", `for a; do DST=$a; done; : > "$DST"`)
	writeStub(t, bin, "isql", `exit 1`)

	root := t.TempDir()
	for _, dir := range []string{"data", "backup/base", "opt"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"opt/security4.fdb", "backup/base/security3.fbk"} {
		if err := os.WriteFile(filepath.Join(root, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	instance := &kubebirdv1.Instance{Spec: kubebirdv1.InstanceSpec{
		Version: upgradedTestVersion,
		Backup: kubebirdv1.BackupSpec{
			Enabled: true,
			Destinations: []kubebirdv1.BackupDestinationSpec{
				{Local: &kubebirdv1.LocalBackupSpec{Storage: kubebirdv1.StorageVolumeSpec{Size: apiresource.MustParse("1Gi")}}},
			},
		},
	}}
	script := strings.NewReplacer(
		primaryDataMountPath, filepath.Join(root, "data"),
		backupDataMountPath, filepath.Join(root, "backup"),
		securityDatabaseDir, filepath.Join(root, "opt"),
	).Replace(securityDatabaseInitScript(instance))
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	if err := cmd.Run(); err == nil {
		t.Fatal("init script succeeded although the migration failed")
	}
	if _, err := os.Stat(filepath.Join(root, "data", "security4.fdb")); !os.IsNotExist(err) {
		t.Errorf("security4.fdb exists after a failed migration (err = %v)", err)
	}
}
