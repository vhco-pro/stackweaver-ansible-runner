// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// A launcher told to stop while it is claiming a job finishes that claim: it takes the job token,
// places the job and drains it, rather than leaving a claimed job with no placement for the API to
// declare lost (one-launcher spec, A8: an upgrade lets running work finish).
func TestAStopDuringAClaimLetsThePlacementFinish(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	d := newFakeDriver("container")
	l := New(testConfig(api), d)
	l.goos = "linux"
	ctx, stop := context.WithCancel(context.Background())
	// The stop signal arrives the moment the job is claimed, before its token and placement.
	api.onStart = func(string) { stop() }
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	api.offer(runnercontract.PendingJob{JobID: "run-1", JobType: "tofu_run", RunType: "apply"})

	eventually(t, "the claimed job is placed", func() bool { return len(d.placements()) == 1 })
	if _, _, failed, _ := api.snapshot(); failed["run-1"] != "" {
		t.Fatalf("the claimed job failed: %s", failed["run-1"])
	}
	// The launcher drains the job it placed, then deregisters.
	select {
	case err := <-done:
		t.Fatalf("the launcher stopped before its job ended: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	d.end("run-1", 0)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the launcher did not finish draining")
	}
	if _, _, _, deregistered := api.snapshot(); !deregistered {
		t.Error("the launcher did not deregister after its drain")
	}
}
