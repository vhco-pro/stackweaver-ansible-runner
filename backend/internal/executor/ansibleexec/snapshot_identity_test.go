// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansibleexec

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

func TestExecute_VerifiesAssignedSnapshotAcrossAttempts(t *testing.T) {
	assigned := testSnapshot(t, map[string]string{"site.yml": "- hosts: all\n# alpha\n"})
	replacement := testSnapshot(t, map[string]string{"site.yml": "- hosts: all\n# bravo\n"})
	if len(replacement) != len(assigned) {
		t.Fatal("test requires distinct valid archives of the same size")
	}
	digest := sha256.Sum256(assigned)
	const validator = `"first-capture"`
	for _, test := range []struct {
		name    string
		partial bool
		changed bool
	}{
		{name: "same capture after interruption", partial: true},
		{name: "changed capture after interruption", partial: true, changed: true},
		{name: "changed capture on first delivery", changed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			stubs := newJobStubs(t)
			control := &fakeControlPlane{artifacts: map[string]any{
				"playbook_path": "site.yml",
				"playbook_source": map[string]any{
					"mode": "cached", "commit": "0123456789abcdef", "captured_at": "2026-11-19T12:00:00Z",
					"size_bytes": len(assigned), "sha256": hex.EncodeToString(digest[:]), "validator": validator,
				},
			}}
			control.onSnapshot = func() {
				switch {
				case test.partial && control.snapshotRequests == 1:
					control.snapshot = assigned[:len(assigned)/2]
				case test.changed:
					control.snapshot = replacement
				default:
					control.snapshot = assigned
				}
			}
			run := runJob(t, control, Options{})
			wantRequests := 1
			if test.partial {
				wantRequests = 2
			}
			if control.snapshotRequests != wantRequests {
				t.Errorf("snapshot requests = %d, want %d", control.snapshotRequests, wantRequests)
			}
			for _, sent := range control.snapshotValidators {
				if sent != validator {
					t.Errorf("request did not keep the original snapshot validator: %q", sent)
				}
			}
			if test.changed {
				if run.result.Status != "failed" || !strings.Contains(strings.ToLower(run.result.ErrorMessage), "start a new run") {
					t.Errorf("changed snapshot result = %s %q, want rerun reason", run.result.Status, run.result.ErrorMessage)
				}
				if stubs.read(t, "cwd") != "" {
					t.Error("Ansible started with replacement snapshot content")
				}
			} else if run.result.Status != "completed" || stubs.read(t, "cwd") == "" {
				t.Errorf("same snapshot result = %s %q, want completed play", run.result.Status, run.result.ErrorMessage)
			}
		})
	}
}

func TestExecute_RefusesUnverifiedSnapshot(t *testing.T) {
	stubs := newJobStubs(t)
	control := &fakeControlPlane{
		artifacts: map[string]any{
			"playbook_path": "site.yml",
			"playbook_source": map[string]any{
				"mode": "cached", "commit": "0123456789abcdef", "captured_at": "2026-11-19T12:00:00Z",
			},
		},
		snapshot:           testSnapshot(t, map[string]string{"site.yml": "- hosts: all\n"}),
		unverifiedSnapshot: true,
	}
	run := runJob(t, control, Options{})
	if run.result.Status != "failed" || !strings.Contains(run.result.ErrorMessage, "Start a new run") {
		t.Fatalf("unverified snapshot result = %s %q, want rerun reason", run.result.Status, run.result.ErrorMessage)
	}
	if control.snapshotRequests != 0 || stubs.read(t, "cwd") != "" {
		t.Fatal("the executor used an unverified snapshot")
	}
	if !slices.Contains(strings.Split(control.artifactsCapabilities, ","), runnercontract.CapabilityVerifiedPlaybookSnapshot) {
		t.Errorf("executor did not negotiate snapshot verification: %s", control.artifactsCapabilities)
	}
}
