// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"time"

	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// reconcile adopts the placements this launcher left before a restart (D3, AC16), read from the
// placements' labels, which carry the same job, attempt and runner the stored placement record
// does: a running placement whose job the API still has active is supervised again; one that died
// meanwhile fails its job; anything else is stopped and removed.
func (l *Launcher) reconcile(ctx context.Context) {
	if !l.driver.Capabilities().SupportsReattach {
		return
	}
	mine, err := l.driver.List(ctx, map[string]string{driver.LabelManaged: "true", driver.LabelRunner: l.client.RunnerID()})
	if err != nil {
		l.client.logf("Listing this launcher's placements: %v", err)
		return
	}
	for _, p := range mine {
		jobID := p.Labels[driver.LabelJob]
		status, err := l.client.Status(ctx, jobID)
		active := err == nil && isActive(status)
		// A job this launcher still holds gets a fresh token for its supervision: the launcher
		// reports the job's failure with it, and its timeout comes with it.
		var tok runnercontract.JobToken
		if active {
			if tok, err = l.client.JobToken(ctx, jobID); err != nil {
				l.client.logf("Job %s: no job token for its supervision: %v", jobID, err)
			}
		}
		if active && !p.Ended {
			l.client.logf("Re-attaching to job %s (%s)", jobID, p.ID)
			l.supervise(ctx, jobID, p, time.Duration(tok.TimeoutSeconds)*time.Second, tok.Token)
			continue
		}
		if active {
			// The placement died while the launcher was down, and the job never reported.
			if ferr := l.client.FailWith(ctx, jobID, tok.Token, "the job's placement ended while its launcher was down"); ferr != nil {
				l.client.logf("Reporting job %s failed: %v", jobID, ferr)
			}
		}
		l.client.logf("Removing the placement of job %s (status %q)", jobID, status)
		_ = l.driver.Kill(ctx, p, l.cfg.killGrace())
		_ = l.driver.Remove(ctx, p)
	}
}

// isActive reports whether a job status is one an executor may still be working on.
func isActive(status string) bool {
	switch status {
	case "running", "planning", "applying", "pending", "plan_queued", "apply_queued":
		return true
	}
	return false
}
