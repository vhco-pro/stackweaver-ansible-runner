// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// Executor exit codes the launcher reads from a placement (executor.ExitCode): the executor reports
// its own completion before it exits with one of these, so any other code means it ended without
// reporting, and the launcher reports the job failed.
const (
	exitCompleted = 0
	exitFailed    = 1
	exitStopped   = 3
)

// supervised is one placement the launcher watches.
type supervised struct {
	jobID     string
	placement driver.Placement
	cancel    context.CancelFunc
	// token is the job's token, which the launcher reports a failure of the job with.
	token string
}

// supervise watches a placement until it ends (D3): it stops it when the API reports the job
// canceled or the job's timeout passes, and reports the job failed when the executor ends without
// having reported (a crash, an out-of-memory kill, a placement that died).
func (l *Launcher) supervise(parent context.Context, jobID string, p driver.Placement, timeout time.Duration, token string) {
	// The supervision outlives the claim that started it; only its own end stops it.
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	s := &supervised{jobID: jobID, placement: p, cancel: cancel, token: token}
	l.mu.Lock()
	l.inflight[jobID] = s
	l.mu.Unlock()
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer cancel()
		defer l.finished(jobID)
		l.watch(ctx, s, timeout)
	}()
}

func (l *Launcher) watch(ctx context.Context, s *supervised, timeout time.Duration) {
	exited := make(chan struct {
		exit driver.Exit
		err  error
	}, 1)
	go func() {
		exit, err := l.driver.Wait(ctx, s.placement)
		exited <- struct {
			exit driver.Exit
			err  error
		}{exit, err}
	}()
	var deadline <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		deadline = t.C
	}
	ticker := time.NewTicker(l.cfg.statusInterval())
	defer ticker.Stop()
	timedOut, deleted := false, false
	// A driver whose kubelet pulls the image learns its digest once the placement runs: it is
	// reported once, then left alone.
	digester, digestPending := l.driver.(driver.ImageDigester)
	for {
		select {
		case r := <-exited:
			if timedOut || deleted {
				// Reported already, at the deadline; or a job that no longer exists to report on.
				r.exit.Code = exitStopped
			} else if r.err == nil && r.exit.Code == exitStopped {
				// The executor exits with the stopped code for a cancel alone (it saw the cancel on
				// its own status poll, often before this loop's): not a failure, so never kept.
				s.placement.Canceled = true
			}
			l.ended(ctx, s, r.exit, r.err)
			return
		case <-deadline:
			// The failure is reported before the kill: the executor the kill stops reports itself
			// canceled, and the API keeps the first completion it is given (AC12).
			reason := fmt.Sprintf("the job exceeded its timeout of %s and was stopped", timeout)
			l.client.logf("Job %s: %s", s.jobID, reason)
			if ferr := l.client.FailWith(context.WithoutCancel(ctx), s.jobID, s.token, reason); ferr != nil {
				l.client.logf("Reporting job %s failed: %v", s.jobID, ferr)
			}
			timedOut = true
			_ = l.driver.Kill(context.WithoutCancel(ctx), s.placement, l.cfg.killGrace())
		case <-ticker.C:
			if digestPending {
				digestPending = !l.reportImageDigest(ctx, s, digester)
			}
			status, err := l.client.Status(ctx, s.jobID)
			var se *StatusError
			deleted = errors.As(err, &se) && se.Code == http.StatusNotFound
			if deleted || (err == nil && status == "canceled") {
				l.client.logf("Job %s was canceled or deleted, stopping its placement", s.jobID)
				// Removed as canceled once it has stopped: never kept as a failure (AC12).
				s.placement.Canceled = true
				_ = l.driver.Kill(context.WithoutCancel(ctx), s.placement, l.cfg.killGrace())
			}
		case <-ctx.Done():
			return
		}
	}
}

// reportImageDigest reports the placement's image digest once the driver knows it, and answers
// whether there is nothing more to do: reported, or refused by the API (an API from before
// placement-only reports answers 400), which is not retried.
func (l *Launcher) reportImageDigest(ctx context.Context, s *supervised, d driver.ImageDigester) bool {
	digest, err := d.ImageDigest(ctx, s.placement)
	if err != nil || digest == "" {
		return false
	}
	if err := l.client.ReportPlacement(context.WithoutCancel(ctx), s.jobID, s.token, runnercontract.Placement{ImageDigest: digest}); err != nil {
		l.client.logf("Reporting the image digest of job %s: %v", s.jobID, err)
	}
	return true
}

// ended handles a placement that ended. An executor that exited with one of its own codes has
// already reported; anything else is reported here.
func (l *Launcher) ended(ctx context.Context, s *supervised, exit driver.Exit, err error) {
	bg := context.WithoutCancel(ctx)
	reason := ""
	switch {
	case err != nil:
		reason = fmt.Sprintf("the launcher lost the job's placement: %v", err)
	case exit.Code == exitCompleted || exit.Code == exitFailed || exit.Code == exitStopped:
	default:
		reason = fmt.Sprintf("the executor ended with exit code %d without reporting", exit.Code)
		if exit.Reason != "" {
			reason += " (" + exit.Reason + ")"
		}
	}
	if reason != "" {
		if ferr := l.client.FailWith(bg, s.jobID, s.token, reason); ferr != nil {
			l.client.logf("Reporting job %s failed: %v", s.jobID, ferr)
		}
	}
	if rerr := l.driver.Remove(bg, s.placement); rerr != nil {
		l.client.logf("Removing the placement of job %s: %v", s.jobID, rerr)
	}
}

// finished forgets a job and wakes the loop: capacity is free.
func (l *Launcher) finished(jobID string) {
	l.mu.Lock()
	delete(l.inflight, jobID)
	l.mu.Unlock()
	select {
	case l.done <- struct{}{}:
	default:
	}
}

// stopAll kills every in-flight placement: the drain grace is over.
func (l *Launcher) stopAll(ctx context.Context) {
	l.mu.Lock()
	jobs := make([]*supervised, 0, len(l.inflight))
	for _, s := range l.inflight {
		jobs = append(jobs, s)
	}
	l.mu.Unlock()
	for _, s := range jobs {
		l.client.logf("Drain grace over: stopping job %s", s.jobID)
		_ = l.driver.Kill(ctx, s.placement, l.cfg.killGrace())
	}
}
