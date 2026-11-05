// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"slices"
	"strings"
	"testing"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// A job or a sync that runs past the timeout its token carries is stopped and reported failed with
// the timeout as the reason, whatever its kind (#933, D2, AC12).
func TestSupervisionStopsWorkPastItsTimeout(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	api.timeouts = map[string]int{"wi-slow": 1, "run-slow": 1}
	d := newFakeDriver("container")
	// The failure is reported before the kill: an executor stopped by the kill reports itself
	// canceled, and the API keeps the first completion it is given.
	d.onKill = func(job string) {
		if _, _, failed, _ := api.snapshot(); !strings.Contains(failed[job], "exceeded its timeout") {
			t.Errorf("%s was killed before its timeout was reported (reported %q)", job, failed[job])
		}
	}
	l := New(testConfig(api), d)
	l.goos = "linux"
	api.offer(
		runnercontract.PendingJob{JobID: "wi-slow", JobType: runnercontract.JobTypeWorkItem},
		runnercontract.PendingJob{JobID: "run-slow", JobType: "tofu_run", RunType: "plan"},
	)

	runLauncher(t, l)
	for _, job := range []string{"wi-slow", "run-slow"} {
		eventually(t, job+" stopped at its timeout", func() bool {
			d.mu.Lock()
			defer d.mu.Unlock()
			return slices.Contains(d.killed, job)
		})
		eventually(t, job+" reported failed with the timeout", func() bool {
			_, _, failed, _ := api.snapshot()
			return strings.Contains(failed[job], "exceeded its timeout")
		})
		// A timeout fails the job, so its placement is removed as a failure, which a pool's keep
		// window may keep (AC25), although its executor ends with the stopped code.
		eventually(t, job+" removed", func() bool { _, ok := d.removal(job); return ok })
		if p, _ := d.removal(job); p.Canceled {
			t.Errorf("%s, stopped at its timeout, was removed as canceled", job)
		}
	}
}
