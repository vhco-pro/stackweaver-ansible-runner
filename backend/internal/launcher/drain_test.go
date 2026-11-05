// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// Told to stop (SIGTERM), a launcher claims nothing more, lets its in-flight job finish and then
// deregisters (#933, D3, AC18).
func TestDrainFinishesInFlightWorkAndDeregisters(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	d := newFakeDriver("container")
	l := New(testConfig(api), d)
	l.goos = "linux"
	api.offer(runnercontract.PendingJob{JobID: "run-1", JobType: "tofu_run", RunType: "plan"})

	ctx, stop := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { result <- l.Run(ctx) }()
	eventually(t, "the first job placed", func() bool { return len(d.placements()) == 1 })

	stop() // SIGTERM
	api.offer(runnercontract.PendingJob{JobID: "run-2", JobType: "tofu_run", RunType: "plan"})
	time.Sleep(100 * time.Millisecond)
	if _, _, _, deregistered := api.snapshot(); deregistered {
		t.Fatal("the launcher deregistered while a job was still running")
	}
	d.end("run-1", exitCompleted)

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the launcher did not finish draining")
	}
	started, _, failed, deregistered := api.snapshot()
	if !deregistered {
		t.Error("the drained launcher did not deregister")
	}
	if slices.Contains(started, "run-2") {
		t.Error("a draining launcher claimed new work")
	}
	if msg, ok := failed["run-1"]; ok {
		t.Errorf("a job that completed was reported failed: %q", msg)
	}
}

// An executor whose completion did not reach the API exits with the unreported code (2, as a Go
// panic does): the launcher reports the job, so it does not stay running for ever.
func TestSupervisionReportsAnExecutorThatCouldNotReport(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	d := newFakeDriver("kubernetes")
	l := New(testConfig(api), d)
	l.goos = "linux"
	api.offer(runnercontract.PendingJob{JobID: "run-unreported", JobType: "tofu_run"})

	runLauncher(t, l)
	eventually(t, "the job placed", func() bool { return len(d.placements()) == 1 })
	d.end("run-unreported", 2)
	eventually(t, "the job reported", func() bool { _, _, failed, _ := api.snapshot(); return failed["run-unreported"] != "" })
}

// A placement whose executor ends without reporting (a crash, an OOM kill) is reported failed by
// the launcher; a canceled job's placement is stopped.
func TestSupervisionReportsACrashAndStopsACancel(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	d := newFakeDriver("container")
	l := New(testConfig(api), d)
	l.goos = "linux"
	api.offer(runnercontract.PendingJob{JobID: "run-crash", JobType: "tofu_run"}, runnercontract.PendingJob{JobID: "job-cancel", JobType: "ansible_job"})

	runLauncher(t, l)
	eventually(t, "both jobs placed", func() bool { return len(d.placements()) == 2 })

	d.end("run-crash", 137)
	eventually(t, "the crash reported", func() bool { _, _, failed, _ := api.snapshot(); return failed["run-crash"] != "" })

	api.mu.Lock()
	api.statuses["job-cancel"] = "canceled"
	api.mu.Unlock()
	eventually(t, "the canceled job stopped", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return slices.Contains(d.killed, "job-cancel")
	})
	if _, _, failed, _ := api.snapshot(); failed["job-cancel"] != "" {
		t.Errorf("a canceled job whose executor stopped was reported failed: %q", failed["job-cancel"])
	}
	// A cancel is not a failure: the placement is removed as canceled, so keep_failed_jobs_for does
	// not keep it and the container or pod is gone within the cancel's ten seconds (AC12). A crash is
	// removed as a failure, which a keep window may keep (AC25).
	eventually(t, "the canceled job's placement removed", func() bool { _, ok := d.removal("job-cancel"); return ok })
	if p, _ := d.removal("job-cancel"); !p.Canceled {
		t.Errorf("the canceled job's placement was removed as a failure: %+v", p)
	}
	if p, ok := d.removal("run-crash"); !ok || p.Canceled {
		t.Errorf("the crashed job's placement = %+v (removed %t), want it removed as a failure", p, ok)
	}
	for _, p := range d.placements() {
		if p.Token == "" || p.Kind == "" {
			t.Errorf("placement %+v lacks its token or engine", p)
		}
	}
}

// A job deleted while it runs (with its job template, or by a playbook force delete) answers 404: its
// placement is stopped and removed as a cancel is, nobody is waiting for the play any more, and no
// failure is reported for a job that no longer exists.
func TestSupervisionStopsAPlacementWhoseJobWasDeleted(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	d := newFakeDriver("container")
	l := New(testConfig(api), d)
	l.goos = "linux"
	api.offer(runnercontract.PendingJob{JobID: "job-deleted", JobType: "ansible_job"})

	runLauncher(t, l)
	eventually(t, "the job placed", func() bool { return len(d.placements()) == 1 })
	api.mu.Lock()
	api.statuses["job-deleted"] = "deleted"
	api.mu.Unlock()
	eventually(t, "the deleted job stopped", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return slices.Contains(d.killed, "job-deleted")
	})
	eventually(t, "its placement removed", func() bool { _, ok := d.removal("job-deleted"); return ok })
	if p, _ := d.removal("job-deleted"); !p.Canceled {
		t.Errorf("the deleted job's placement was removed as a failure: %+v", p)
	}
	if _, _, failed, _ := api.snapshot(); failed["job-deleted"] != "" {
		t.Errorf("a deleted job was reported failed: %q", failed["job-deleted"])
	}
}

// An executor stops a canceled job itself within one of its status polls, often before the
// launcher's own poll sees the cancel: it exits with the stopped code, which it uses for a cancel
// alone. That placement is removed as canceled too, never kept as a failure (AC12). A placement the
// launcher stopped at the job's timeout ends with the same code and is a failure (AC25).
func TestSupervisionRemovesAPlacementTheExecutorStoppedAsCanceled(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	d := newFakeDriver("container")
	l := New(testConfig(api), d)
	l.goos = "linux"
	api.offer(runnercontract.PendingJob{JobID: "run-self-stopped", JobType: "tofu_run"})

	runLauncher(t, l)
	eventually(t, "the job placed", func() bool { return len(d.placements()) == 1 })
	d.end("run-self-stopped", exitStopped)
	eventually(t, "the placement removed", func() bool { _, ok := d.removal("run-self-stopped"); return ok })
	if p, _ := d.removal("run-self-stopped"); !p.Canceled {
		t.Errorf("a placement its executor stopped for a cancel was removed as a failure: %+v", p)
	}
}

// A drain requested through the API (the heartbeat answers Drain) stops claiming, lets the
// in-flight job finish and deregisters, like SIGTERM does (#933, D11, AC18).
func TestRemoteDrainFinishesInFlightWorkAndDeregisters(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	d := newFakeDriver("container")
	l := New(testConfig(api), d)
	l.goos = "linux"
	api.offer(runnercontract.PendingJob{JobID: "run-1", JobType: "tofu_run", RunType: "plan"})

	result := make(chan error, 1)
	go func() { result <- l.Run(t.Context()) }()
	eventually(t, "the first job placed", func() bool { return len(d.placements()) == 1 })

	api.mu.Lock()
	api.drain = true
	api.mu.Unlock()
	api.offer(runnercontract.PendingJob{JobID: "run-2", JobType: "tofu_run", RunType: "plan"})
	time.Sleep(200 * time.Millisecond)
	if _, _, _, deregistered := api.snapshot(); deregistered {
		t.Fatal("the launcher deregistered while a job was still running")
	}
	d.end("run-1", exitCompleted)

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the launcher did not finish draining")
	}
	started, _, _, deregistered := api.snapshot()
	if !deregistered {
		t.Error("the drained launcher did not deregister")
	}
	if slices.Contains(started, "run-2") {
		t.Error("a draining launcher claimed new work")
	}
}
