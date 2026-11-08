// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// The executor works out what it runs from its invocation alone (one-launcher spec, A1, AC6): a
// work item is a sync, a run type is an OpenTofu run, anything else an Ansible job. The binary's
// name says nothing about the engine, so the answer is the same from the constant path, the old
// per-engine paths a previous launcher runs, and the copy injected into a custom run image.
func TestKindOfComesFromTheInvocationNotTheBinaryName(t *testing.T) {
	starts := []string{
		"/usr/local/bin/stackweaver-launcher",
		"/usr/local/bin/runner",
		"/usr/local/bin/ansible-runner",
		"/stackweaver/stackweaver-executor",
	}
	cases := []struct {
		name    string
		jobID   string
		runType string
		want    Kind
	}{
		{"configuration snapshot", runnercontract.WorkItemPrefix + "snap-1", "", KindSync},
		{"inventory sync", runnercontract.WorkItemPrefix + "inv-1", "", KindSync},
		{"plan", "run-abc", "plan", KindTofu},
		{"apply", "run-abc", "apply", KindTofu},
		{"plan-destroy", "run-abc", "plan-destroy", KindTofu},
		{"apply-destroy", "run-abc", "apply-destroy", KindTofu},
		{"ansible job", "job-42", "", KindAnsible},
	}
	saved := os.Args[0]
	t.Cleanup(func() { os.Args[0] = saved })
	for _, start := range starts {
		os.Args[0] = start
		for _, tc := range cases {
			if got := KindOf(tc.jobID, tc.runType); got != tc.want {
				t.Errorf("started as %s: KindOf(%q, %q) = %q, want %q", start, tc.jobID, tc.runType, got, tc.want)
			}
		}
	}
}

// A sync's preflight goes where the job image's engine writes it: stderr in the Ansible job image,
// stdout in the OpenTofu one, told apart by ansible on PATH (A1, AC6). Runs and jobs keep their own
// streams.
func TestPreflightStreamFollowsTheJobImage(t *testing.T) {
	withAnsible := t.TempDir()
	stub := filepath.Join(withAnsible, "ansible")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // G306: a test stub must be executable
		t.Fatal(err)
	}
	without := t.TempDir()
	cases := []struct {
		name    string
		path    string
		jobID   string
		runType string
		want    string
	}{
		{"sync in the Ansible job image", withAnsible, runnercontract.WorkItemPrefix + "s", "", "stderr"},
		{"sync in the OpenTofu job image", without, runnercontract.WorkItemPrefix + "s", "", "stdout"},
		{"plan", withAnsible, "run-1", "plan", "plan"},
		{"plan-destroy", without, "run-1", "plan-destroy", "plan"},
		{"apply", without, "run-1", "apply", "apply"},
		{"apply-destroy", withAnsible, "run-1", "apply-destroy", "apply"},
		{"ansible job", without, "job-1", "", "stderr"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", tc.path)
			if got := PreflightStream(tc.jobID, tc.runType); got != tc.want {
				t.Errorf("PreflightStream(%q, %q) with PATH=%s = %q, want %q", tc.jobID, tc.runType, tc.path, got, tc.want)
			}
		})
	}
}
