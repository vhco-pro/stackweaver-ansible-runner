// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"os/exec"
	"strings"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// Kind is what one executor invocation runs.
type Kind string

// The kinds of job an executor runs.
const (
	KindSync    Kind = "sync"
	KindTofu    Kind = "tofu"
	KindAnsible Kind = "ansible"
)

// KindOf works out what an invocation runs from the job it names (one-launcher spec, A1): a work
// item is a sync, a run type (set only on an OpenTofu run's offer) is an OpenTofu run, and anything
// else is an Ansible job. It reads nothing else, so the executor's answer is the same whichever
// path or name it was started under.
func KindOf(jobID, runType string) Kind {
	switch {
	case strings.HasPrefix(jobID, runnercontract.WorkItemPrefix):
		return KindSync
	case runType != "":
		return KindTofu
	default:
		return KindAnsible
	}
}

// PreflightStream is the output stream a job's preflight lines are written to: a run's plan or
// apply stream, an Ansible job's stderr, and for a sync the stream its job image's engine uses,
// stderr where ansible is on PATH (the Ansible job image) and stdout otherwise.
func PreflightStream(jobID, runType string) string {
	switch KindOf(jobID, runType) {
	case KindSync:
		if _, err := exec.LookPath("ansible"); err == nil {
			return "stderr"
		}
		return "stdout"
	case KindTofu:
		if strings.HasPrefix(runType, "apply") {
			return "apply"
		}
		return "plan"
	case KindAnsible:
		return "stderr"
	}
	return "stderr"
}
