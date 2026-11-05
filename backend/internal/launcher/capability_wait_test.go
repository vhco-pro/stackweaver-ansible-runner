// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// A launcher against an API that does not issue job tokens places nothing: it waits for the API
// rather than hand the executor its runner token (#933, D2, AC72).
func TestLauncherWaitsForTheJobTokenCapability(t *testing.T) {
	api := newFakeAPI(t) // an older API: no capabilities declared
	d := newFakeDriver("container")
	l := New(testConfig(api), d)
	l.goos = "linux"
	api.offer(runnercontract.PendingJob{JobID: "run-1", JobType: "tofu_run"})

	ctx, stop := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer stop()
	_ = l.Run(ctx)

	started, tokens, _, _ := api.snapshot()
	if len(started) != 0 || len(tokens) != 0 || len(d.placements()) != 0 {
		t.Errorf("claimed %v, tokens %v, placed %d: want nothing without the job-token capability", started, tokens, len(d.placements()))
	}
	api.mu.Lock()
	beats := api.heartbeats
	api.mu.Unlock()
	if beats == 0 {
		t.Error("the waiting launcher stopped heartbeating, so it would be marked offline")
	}
}

// Under hosted.strict a launcher refuses the in-process driver and the host-identity network
// before it registers.
func TestStrictRefusesInProcessAndHostIdentity(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	cfg := testConfig(api)
	cfg.Hosted, cfg.Strict, cfg.PoolID = true, true, ""

	l := New(cfg, &driver.InProcess{})
	l.goos = "linux"
	if err := l.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "in-process") {
		t.Errorf("Run = %v, want the in-process driver refused under hosted.strict", err)
	}
	cfg.NetworkMode = NetworkHostIdentity
	l = New(cfg, newFakeDriver("container"))
	l.goos = "linux"
	if err := l.Run(t.Context()); err == nil || !strings.Contains(err.Error(), "host-identity") {
		t.Errorf("Run = %v, want host-identity refused under hosted.strict", err)
	}
	if _, _, _, _ = api.snapshot(); api.heartbeats != 0 {
		t.Error("a refused launcher registered and heartbeated")
	}
}

// The launcher refuses a non-Linux host before it does anything.
func TestRunRefusesANonLinuxHost(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	l := New(testConfig(api), newFakeDriver("container"))
	l.goos = "windows"
	if err := l.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "decision 4") {
		t.Errorf("Run on windows = %v, want the Linux-only refusal", err)
	}
}
