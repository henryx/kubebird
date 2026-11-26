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
	"slices"
	"strings"
	"testing"
)

// TestSecretCommand runs secretCommand's rendered command under sh, with a
// stub program on PATH that prints each argument on its own line followed
// by its own stdin, the same way the exec into the pod runs it.
func TestSecretCommand(t *testing.T) {
	bin := t.TempDir()
	stub := "#!/bin/sh\nfor a in \"$@\"; do printf '[%s]\\n' \"$a\"; done\necho ---\ncat\n"
	if err := os.WriteFile(filepath.Join(bin, "stub"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	sysdba := `p4ss w'ord"$HOME` + "`id`\\"
	user := "-sec_password"
	plain := "it's a $PATH"
	script := "CREATE DATABASE 'x';\nQUIT;\n"

	command, secrets, err := secretCommand("stub", "-user", "SYSDBA", "-password", secret(sysdba), plain, user, secret(""), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range command {
		if strings.Contains(arg, "p4ss") {
			t.Fatalf("command %q carries a secret", command)
		}
	}

	cmd := exec.Command(command[0], command[1:]...)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	cmd.Stdin = strings.NewReader(secrets + script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("command failed: %v\n%s", err, out)
	}

	want := "[-user]\n[SYSDBA]\n[-password]\n[" + sysdba + "]\n[" + plain + "]\n[" + user + "]\n[]\n[]\n---\n" + script
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}

func TestSecretCommandWithoutSecrets(t *testing.T) {
	command, secrets, err := secretCommand("test", "-f", "/a b")
	if err != nil {
		t.Fatal(err)
	}
	if secrets != "" {
		t.Fatalf("got stdin prefix %q, want none", secrets)
	}
	if err := exec.Command(command[0], command[1:]...).Run(); err == nil {
		t.Fatal("test -f on a missing file succeeded")
	}
}

func TestSecretCommandRejectsLineBreaks(t *testing.T) {
	for _, password := range []string{"a\nb", "a\r"} {
		if _, _, err := secretCommand("isql", secret(password)); err == nil {
			t.Fatalf("password %q accepted", password)
		}
	}
	if _, _, err := secretCommand("isql", 42); err == nil {
		t.Fatal("non-string argument accepted")
	}
}

func TestServiceManagerCommandKeepsPasswordSecret(t *testing.T) {
	args := serviceManagerCommand("pw", actionAddUser, flagSecPassword, secret("userpw"))
	if !slices.Contains(args, any(secret("pw"))) || !slices.Contains(args, any(secret("userpw"))) {
		t.Fatalf("passwords not passed as secrets: %v", args)
	}
	if slices.Contains(args, any("pw")) {
		t.Fatalf("SYSDBA password passed as a plain argument: %v", args)
	}
}
