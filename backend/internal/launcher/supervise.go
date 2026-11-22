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
	token    string
	expected runnercontract.JobTokenRequest
}

func (l *Launcher) watch(ctx context.Context, s *supervised, timeout time.Duration) {
	// Hard drain signals supervision, which stops the placement before releasing its waiter.
	waitCtx, stopWaiting := context.WithCancel(context.WithoutCancel(ctx))
	defer stopWaiting()
	placement := s.placement
	exited := make(chan struct {
		exit driver.Exit
		err  error
	}, 1)
	go func() {
		exit, err := l.driver.Wait(waitCtx, placement)
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
	// reported once, then left alone. It is read on its own, shorter interval, so a job of a few
	// seconds, which ends before the first status read, still reports the image it ran in.
	digester, digestPending := l.driver.(driver.ImageDigester)
	var digestTick <-chan time.Time
	if digestPending {
		t := time.NewTicker(min(digestPollInterval, l.cfg.statusInterval()))
		defer t.Stop()
		digestTick = t.C
	}
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
			l.deliverFailure(ctx, s, reason)
			timedOut = true
			_ = l.driver.Kill(context.WithoutCancel(ctx), s.placement, l.cfg.killGrace())
		case <-digestTick:
			if digestPending = !l.reportImageDigest(ctx, s, digester); !digestPending {
				digestTick = nil
			}
		case <-ticker.C:
			status, err := l.client.Status(ctx, s.jobID)
			var se *StatusError
			deleted = deleted || (errors.As(err, &se) && se.Code == http.StatusNotFound && se.ErrorCode == runnercontract.ErrorCodeJobDeleted)
			if deleted || (err == nil && status == "canceled") {
				l.client.logf("Job %s was canceled or deleted, stopping its placement", s.jobID)
				// Removed as canceled once it has stopped: never kept as a failure (AC12).
				s.placement.Canceled = true
				_ = l.driver.Kill(context.WithoutCancel(ctx), s.placement, l.cfg.killGrace())
			}
		case <-ctx.Done():
			_ = l.driver.Kill(context.WithoutCancel(ctx), s.placement, l.cfg.killGrace())
			_ = l.driver.Remove(context.WithoutCancel(ctx), s.placement)
			return
		}
	}
}

// digestPollInterval is how often a placement's image digest is read until it is reported.
const digestPollInterval = 500 * time.Millisecond

// reportImageDigest reports the placement's image digest once the driver knows it, and answers
// whether there is nothing more to do: reported, or refused by the API (an API from before
// placement-only reports answers 400), which is not retried.
func (l *Launcher) reportImageDigest(ctx context.Context, s *supervised, d driver.ImageDigester) bool {
	digest, err := d.ImageDigest(ctx, s.placement)
	if err != nil || digest == "" {
		return false
	}
	if err := l.client.ReportPlacement(ctx, s.jobID, s.token, runnercontract.Placement{ImageDigest: digest}); err != nil {
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
		reason = fmt.Sprintf("the job's process ended with exit code %d without reporting", exit.Code)
		if exit.Reason != "" {
			reason += " (" + exit.Reason + ")"
		}
	}
	if reason != "" {
		l.deliverFailure(ctx, s, reason)
	}
	if rerr := l.driver.Remove(bg, s.placement); rerr != nil {
		l.client.logf("Removing the placement of job %s: %v", s.jobID, rerr)
	}
}

// finished forgets a job and wakes the loop: capacity is free.
func (l *Launcher) finished(key string) {
	l.mu.Lock()
	delete(l.inflight, key)
	l.mu.Unlock()
	select {
	case l.done <- struct{}{}:
	default:
	}
}

// stopAll kills every in-flight placement: the drain grace is over.
func (l *Launcher) stopAll(_ context.Context) {
	if l.recoveryCancel != nil {
		l.recoveryCancel()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.inflight {
		l.client.logf("Drain grace over: stopping job %s", s.jobID)
		s.cancel()
	}
}

// deliverFailure retains capacity and the exact payload until delivery, terminal refusal or the
// worker's hard deadline. Each delivery window has a fresh budget after acquisition has ended.
func (l *Launcher) deliverFailure(ctx context.Context, s *supervised, reason string) {
	for ctx.Err() == nil {
		if err := l.client.FailWith(ctx, s.jobID, s.token, reason, s.expected); err == nil {
			return
		} else {
			l.client.logf("Reporting job %s failed: %v", s.jobID, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}
