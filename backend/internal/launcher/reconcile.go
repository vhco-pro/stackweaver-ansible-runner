// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

func (l *Launcher) noteActiveClaims(claims []runnercontract.ActiveClaim) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.claimsAvailable = claims != nil
	if claims != nil {
		l.activeClaims = slices.Clone(claims)
	}
}

// reconcile reserves the complete startup join before opening admission. Each resource or
// unplaced claim then has an independent worker, so control retries leave heartbeats available.
func (l *Launcher) reconcile(ctx context.Context) {
	defer func() {
		if !l.startupReady.Load() && ctx.Err() != nil {
			l.mu.Lock()
			placements := slices.Clone(l.startupPlacements)
			l.mu.Unlock()
			for _, placement := range placements {
				l.removeRecovered(context.WithoutCancel(ctx), placement)
			}
		}
	}()
	for ctx.Err() == nil {
		var placements []driver.Placement
		err := l.client.control(ctx, "startup placement inventory", func(attempt context.Context) error {
			found, err := l.driver.List(attempt, map[string]string{driver.LabelManaged: "true", driver.LabelRunner: l.client.RunnerID()})
			if err != nil {
				return err
			}
			placements = found
			l.mu.Lock()
			l.startupPlacements = slices.Clone(found)
			l.mu.Unlock()
			return nil
		})
		if err != nil {
			if !l.waitRecovery(ctx) {
				return
			}
			continue
		}
		for ctx.Err() == nil {
			l.mu.Lock()
			available := l.claimsAvailable
			claims := slices.Clone(l.activeClaims)
			l.mu.Unlock()
			if available {
				l.reserveRecovered(ctx, placements, claims)
				l.startupReady.Store(true)
				return
			}
			if !l.waitRecovery(ctx) {
				return
			}
		}
	}
}

func (l *Launcher) waitRecovery(ctx context.Context) bool {
	timer := time.NewTimer(l.cfg.idleBeat())
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (l *Launcher) reserveRecovered(parent context.Context, placements []driver.Placement, claims []runnercontract.ActiveClaim) {
	byJob := make(map[string][]driver.Placement, len(placements))
	for _, placement := range placements {
		jobID := placement.Labels[driver.LabelJob]
		byJob[jobID] = append(byJob[jobID], placement)
		l.startRecovered(parent, "placement:"+placement.ID, jobID, placement, nil)
	}
	for _, claim := range claims {
		possiblePlacement := false
		for _, placement := range byJob[claim.JobID] {
			_, known := placementClaim(placement)
			// A legacy or inconsistent identity could belong to this claim. Keep its
			// reservation until both original identities are established.
			if !known || !validClaim(claim) || !placementKindMatches(placement, claim) || placementMatches(placement, claim) {
				possiblePlacement = true
				break
			}
		}
		if !possiblePlacement {
			l.startRecovered(parent, "claim:"+claim.JobID, claim.JobID, driver.Placement{}, &claim)
		}
	}
}

func (l *Launcher) startRecovered(parent context.Context, key, jobID string, placement driver.Placement, unplaced *runnercontract.ActiveClaim) {
	ctx, cancel := context.WithCancel(parent)
	s := &supervised{jobID: jobID, placement: placement, cancel: cancel}
	l.mu.Lock()
	if _, exists := l.inflight[key]; exists {
		l.mu.Unlock()
		cancel()
		return
	}
	l.inflight[key] = s
	l.wg.Add(1)
	l.mu.Unlock()
	go func() {
		defer l.wg.Done()
		defer cancel()
		defer l.finished(key)
		if unplaced != nil {
			if validClaim(*unplaced) {
				s.expected = claimRequest(*unplaced)
				l.deliverFailure(ctx, s, "the launcher restarted before the original claim had a placement")
			} else {
				<-ctx.Done()
			}
			return
		}
		l.recoverPlacement(ctx, s)
	}()
}

func validClaim(claim runnercontract.ActiveClaim) bool {
	return validOffer(runnercontract.PendingJob{JobID: claim.JobID, JobType: claim.JobType, Attempt: claim.Attempt, Phase: claim.Phase, RunType: claim.RunType})
}

func claimRequest(claim runnercontract.ActiveClaim) runnercontract.JobTokenRequest {
	return runnercontract.JobTokenRequest{Attempt: claim.Attempt, Phase: claim.Phase, RunType: claim.RunType}
}

// placementClaim uses the immutable labels recorded before user code starts. Plan-only is an
// existing placement alias for the ordinary plan operation.
func placementClaim(placement driver.Placement) (runnercontract.JobTokenRequest, bool) {
	attempt, err := strconv.Atoi(placement.Labels[driver.LabelAttempt])
	if err != nil || attempt <= 0 {
		return runnercontract.JobTokenRequest{}, false
	}
	runType := placement.Labels[driver.LabelRunType]
	kind := placement.Labels[driver.LabelKind]
	if kind != "ansible" && kind != "tofu" || runType != "" && kind != "tofu" {
		return runnercontract.JobTokenRequest{}, false
	}
	if runType == "plan-only" {
		runType = "plan"
	}
	request := runnercontract.JobTokenRequest{Attempt: attempt, RunType: runType}
	switch runType {
	case "plan", "plan-destroy":
		request.Phase = "plan"
	case "apply", "apply-destroy":
		request.Phase = "apply"
	case "":
	default:
		return request, false
	}
	return request, true
}

func placementMatches(placement driver.Placement, claim runnercontract.ActiveClaim) bool {
	expected, known := placementClaim(placement)
	if !known || !validClaim(claim) || expected != claimRequest(claim) {
		return false
	}
	return placementKindMatches(placement, claim)
}

func placementKindMatches(placement driver.Placement, claim runnercontract.ActiveClaim) bool {
	kind := placement.Labels[driver.LabelKind]
	switch claim.JobType {
	case "ansible_job":
		return kind == "ansible"
	case "tofu_run":
		return kind == "tofu"
	case runnercontract.JobTypeWorkItem:
		// Work items retain either supported original executor engine across config changes.
		return kind == "ansible" || kind == "tofu"
	}
	return false
}

func (l *Launcher) recoverPlacement(ctx context.Context, s *supervised) {
	watching := false
	defer func() {
		if !watching {
			l.removeRecovered(context.WithoutCancel(ctx), s.placement)
		}
	}()
	for ctx.Err() == nil {
		status, err := l.client.Status(ctx, s.jobID)
		var response *StatusError
		deleted := errors.As(err, &response) && response.Code == http.StatusNotFound && response.ErrorCode == runnercontract.ErrorCodeJobDeleted
		if deleted || err == nil && status == "canceled" {
			s.placement.Canceled = true
			return
		}
		if err != nil {
			if !l.waitRecovery(ctx) {
				return
			}
			continue
		}
		expected, known := placementClaim(s.placement)
		if !known || s.jobID == "" {
			if !l.waitRecovery(ctx) {
				return
			}
			continue
		}
		l.mu.Lock()
		claims := slices.Clone(l.activeClaims)
		available := l.claimsAvailable
		l.mu.Unlock()
		if !available {
			if !l.waitRecovery(ctx) {
				return
			}
			continue
		}
		matches, compatible := false, true
		for _, claim := range claims {
			if claim.JobID == s.jobID {
				compatible = validClaim(claim) && placementKindMatches(s.placement, claim)
				matches = compatible && claimRequest(claim) == expected
				break
			}
		}
		if !compatible {
			if !l.waitRecovery(ctx) {
				return
			}
			continue
		}
		s.expected = expected
		token, err := l.client.JobToken(ctx, s.jobID, expected)
		if terminalRefusal(err) {
			if errors.As(err, &response) && response.ErrorCode == runnercontract.ErrorCodeJobDeleted {
				s.placement.Canceled = true
			}
			return
		}
		if err != nil {
			if !l.waitRecovery(ctx) {
				return
			}
			continue
		}
		// The bound request validates the original claim under the API row lock.
		// A classified refusal can settle an absent or superseded inventory entry.
		// Retain capacity until the complete inventory confirms reattachment.
		if !matches {
			if !l.waitRecovery(ctx) {
				return
			}
			continue
		}
		s.token = token.Token
		if !token.ExpiresAt.IsZero() {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, token.ExpiresAt)
			defer cancel()
		}
		if s.placement.Ended {
			l.deliverFailure(ctx, s, "the job's placement ended while its launcher was down")
			return
		}
		watching = true
		l.client.logf("Re-attaching to job %s (%s)", s.jobID, s.placement.ID)
		l.watch(ctx, s, time.Duration(token.TimeoutSeconds)*time.Second)
		return
	}
}

func (l *Launcher) removeRecovered(ctx context.Context, placement driver.Placement) {
	_ = l.driver.Kill(ctx, placement, l.cfg.killGrace())
	_ = l.driver.Remove(ctx, placement)
}
