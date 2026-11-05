// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"slices"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// A launcher sweeps the failed placements it kept for inspection once their keep_failed_jobs_for
// has passed: at start and then periodically, only its own (#933, D4, AC25).
func TestLauncherReapsItsKeptPlacements(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	d := newFakeDriver("container")
	cfg := testConfig(api)
	cfg.ReapInterval = 50 * time.Millisecond
	l := New(cfg, d)
	l.goos = "linux"
	runLauncher(t, l)

	eventually(t, "two sweeps", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return len(d.reaped) >= 2
	})
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, sel := range d.reaped {
		if sel[driver.LabelRunner] != "runner-1" || sel[driver.LabelManaged] != "true" {
			t.Errorf("swept with selector %v, want this launcher's managed placements", sel)
		}
	}
}

// A launcher stops and removes the placements a lost launcher of its pool left behind, once the
// API names that runner lost (offline, its work already settled), and never touches its own or
// those of a runner the API has not named (#933, D3, AC67).
func TestLauncherReapsTheOrphansOfALostPeer(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	api.reapRunners = []string{"runner-old"}
	d := newFakeDriver("kubernetes")
	d.listed = []driver.Placement{
		{ID: "pod-orphan", Labels: map[string]string{driver.LabelRunner: "runner-old", driver.LabelJob: "run-orphan"}},
		{ID: "pod-other", Labels: map[string]string{driver.LabelRunner: "runner-live", driver.LabelJob: "run-other"}},
		{ID: "pod-mine", Labels: map[string]string{driver.LabelRunner: "runner-1", driver.LabelJob: "run-mine"}},
	}
	cfg := testConfig(api)
	cfg.ReapInterval = 50 * time.Millisecond
	l := New(cfg, d)
	l.goos = "linux"
	runLauncher(t, l)

	eventually(t, "the orphan stopped", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return slices.Contains(d.killed, "run-orphan")
	})
	eventually(t, "the lost runner's placements swept", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return slices.ContainsFunc(d.reaped, func(sel map[string]string) bool { return sel[driver.LabelRunner] == "runner-old" })
	})
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, job := range []string{"run-other", "run-mine"} {
		if slices.Contains(d.killed, job) {
			t.Errorf("stopped %s, which no lost runner placed", job)
		}
	}
}

// A runner the API stops naming is no longer reaped: the list is replaced by every heartbeat, so a
// runner that came back online under the same identity keeps its placements (AC67).
func TestLauncherForgetsALostPeerTheAPINoLongerNames(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	api.reapRunners = []string{"runner-old"}
	d := newFakeDriver("kubernetes")
	cfg := testConfig(api)
	cfg.ReapInterval = time.Hour
	l := New(cfg, d)
	l.goos = "linux"
	runLauncher(t, l)
	eventually(t, "the lost runner named", func() bool { p := l.lostPeers.Load(); return p != nil && len(*p) == 1 })
	api.mu.Lock()
	api.reapRunners = nil
	api.mu.Unlock()
	eventually(t, "the name dropped", func() bool { p := l.lostPeers.Load(); return p != nil && len(*p) == 0 })
}
