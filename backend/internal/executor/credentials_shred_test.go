// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Every materialised credential is 0600 in one 0700 directory, and Shred overwrites it before
// removing it (#933, D2, AC25): a process that still holds the file open reads zeros.
func TestSecretDirShredsWhatItHolds(t *testing.T) {
	sd, err := NewSecretDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("placeholder-vault-password")
	p, err := sd.Write("vault_pass_0", secret)
	if err != nil {
		t.Fatal(err)
	}
	// A helper that writes into the directory itself, as the cloud identity helpers do.
	if err := os.WriteFile(filepath.Join(sd.Dir(), "aws-web-identity-token"), secret, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{p, sd.Dir()} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o600)
		if info.IsDir() {
			want = 0o700 | os.ModeDir
		}
		if info.Mode() != want {
			t.Errorf("%s has mode %v, want %v", name, info.Mode(), want)
		}
	}

	held, err := os.Open(p) //nolint:gosec // G304: the test's own file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()

	if err := sd.Shred(); err != nil {
		t.Fatalf("Shred: %v", err)
	}
	if _, err := os.Stat(sd.Dir()); !os.IsNotExist(err) {
		t.Errorf("the secret directory survived Shred (stat error %v)", err)
	}
	left, _ := io.ReadAll(held)
	if bytes.Contains(left, secret) || len(left) != len(secret) {
		t.Errorf("the shredded file still reads %q, want %d zero bytes", left, len(secret))
	}
	if err := sd.Shred(); err != nil {
		t.Errorf("a second Shred failed: %v", err)
	}
}

// Shred deferred by the executor runs on a panic too.
func TestSecretDirShredsOnPanic(t *testing.T) {
	var dir string
	func() {
		defer func() { _ = recover() }()
		sd, err := NewSecretDir(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = sd.Shred() }()
		dir = sd.Dir()
		if _, err := sd.Write("ssh_key", []byte("placeholder-key")); err != nil {
			t.Fatal(err)
		}
		panic("the executor panicked")
	}()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the secret directory survived a panic (stat error %v)", err)
	}
}
