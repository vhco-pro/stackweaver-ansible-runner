// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package tofuexec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReplaceRemoteBackend covers AUD-080 on the executor, the one OpenTofu path left (#933): both
// TFC/TFE `cloud {}` blocks and `backend "remote"` blocks are stripped from every .tf file, so the
// run uses the local backend the executor manages. It moved here from the platform runner's queue
// path, which rewrote a remote backend to an explicit local one; dropping the block selects the
// same local backend.
func TestReplaceRemoteBackend(t *testing.T) {
	cases := []struct {
		name          string
		in            string
		wantContains  []string
		wantAbsent    []string
		wantUnchanged bool
	}{
		{
			name:         "cloud block removed",
			in:           "terraform {\n  cloud {\n    organization = \"acme\"\n    workspaces { name = \"prod\" }\n  }\n}\n",
			wantAbsent:   []string{"cloud {", "organization = \"acme\""},
			wantContains: []string{"terraform {"},
		},
		{
			name:         "remote backend removed",
			in:           "terraform {\n  backend \"remote\" {\n    organization = \"acme\"\n    workspaces { name = \"prod\" }\n  }\n}\n",
			wantAbsent:   []string{"backend \"remote\"", "organization = \"acme\""},
			wantContains: []string{"terraform {"},
		},
		{
			name:          "plain config untouched",
			in:            "resource \"null_resource\" \"a\" {}\n",
			wantUnchanged: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "main.tf")
			if err := os.WriteFile(path, []byte(tc.in), 0o600); err != nil {
				t.Fatal(err)
			}
			replaceRemoteBackend(dir)
			data, err := os.ReadFile(path) //nolint:gosec // G304: the test's own file
			if err != nil {
				t.Fatal(err)
			}
			got := string(data)
			if tc.wantUnchanged && got != tc.in {
				t.Fatalf("expected unchanged, got:\n%s", got)
			}
			for _, s := range tc.wantContains {
				if !strings.Contains(got, s) {
					t.Errorf("expected output to contain %q, got:\n%s", s, got)
				}
			}
			for _, s := range tc.wantAbsent {
				if strings.Contains(got, s) {
					t.Errorf("expected output to NOT contain %q, got:\n%s", s, got)
				}
			}
		})
	}
}
