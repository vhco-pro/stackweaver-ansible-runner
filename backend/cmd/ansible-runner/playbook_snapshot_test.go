// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFile creates parent dirs and writes a file under root.
func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveSnapshotPaths_LeanRolesCase(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "site.yml", "- hosts: all\n  roles:\n    - web\n")
	writeFile(t, repo, "roles/web/tasks/main.yml", "- debug: msg=web\n")
	writeFile(t, repo, "roles/web/meta/main.yml", "dependencies:\n  - common\n")
	writeFile(t, repo, "roles/common/tasks/main.yml", "- debug: msg=common\n")
	writeFile(t, repo, "roles/db/tasks/main.yml", "- debug: msg=db\n") // unused role
	writeFile(t, repo, "group_vars/all.yml", "k: v\n")
	writeFile(t, repo, "files/x.txt", "x\n")
	writeFile(t, repo, "docs/README.md", "unrelated\n")

	paths, whole := resolvePlaybookSnapshotPaths(repo, "site.yml")
	if whole {
		t.Fatalf("expected lean capture, got whole-repo fallback")
	}
	got := map[string]bool{}
	for _, p := range paths {
		got[p] = true
	}
	for _, want := range []string{"site.yml", "roles/web", "roles/common", "group_vars", "files"} {
		if !got[want] {
			t.Errorf("expected %q in snapshot paths, got %v", want, paths)
		}
	}
	// Unused role and unrelated docs must NOT be captured.
	if got["roles/db"] {
		t.Errorf("unused role roles/db should not be captured: %v", paths)
	}
	if got["docs"] {
		t.Errorf("unrelated docs/ should not be captured: %v", paths)
	}
}

func TestResolveSnapshotPaths_WholeRepoFallback(t *testing.T) {
	cases := map[string]string{
		"loose include_tasks": "- hosts: all\n  tasks:\n    - include_tasks: extra.yml\n",
		"vars_files":          "- hosts: all\n  vars_files:\n    - secrets.yml\n  roles:\n    - web\n",
		"dynamic role name":   "- hosts: all\n  roles:\n    - \"{{ role_var }}\"\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			repo := t.TempDir()
			writeFile(t, repo, "site.yml", body)
			writeFile(t, repo, "roles/web/tasks/main.yml", "- debug: msg=web\n")
			_, whole := resolvePlaybookSnapshotPaths(repo, "site.yml")
			if !whole {
				t.Errorf("expected whole-repo fallback for %q", name)
			}
		})
	}
}

func TestBuildPlaybookSnapshot_RoundTrip(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "site.yml", "- hosts: all\n  roles:\n    - web\n")
	writeFile(t, repo, "roles/web/tasks/main.yml", "- debug: msg=web\n")
	writeFile(t, repo, "group_vars/all.yml", "k: v\n")
	writeFile(t, repo, "docs/README.md", "unrelated\n")
	writeFile(t, repo, ".git/config", "[core]\n") // must be excluded

	data, whole, err := buildPlaybookSnapshot(repo, "site.yml")
	if err != nil {
		t.Fatalf("buildPlaybookSnapshot: %v", err)
	}
	if whole {
		t.Fatalf("expected lean snapshot")
	}

	out := t.TempDir()
	if err := extractTarGz(data, out); err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}
	mustExist := []string{"site.yml", "roles/web/tasks/main.yml", "group_vars/all.yml"}
	for _, rel := range mustExist {
		if _, err := os.Stat(filepath.Join(out, filepath.FromSlash(rel))); err != nil {
			t.Errorf("expected %q in extracted snapshot: %v", rel, err)
		}
	}
	mustNotExist := []string{"docs/README.md", ".git/config"}
	for _, rel := range mustNotExist {
		if _, err := os.Stat(filepath.Join(out, filepath.FromSlash(rel))); err == nil {
			t.Errorf("did not expect %q in extracted snapshot", rel)
		}
	}
}
