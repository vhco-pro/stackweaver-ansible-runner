// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"slices"
	"testing"

	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// A restarted launcher re-attaches to its two running placements without claiming them again,
// fails the job whose placement died while it was down, and stops a placement whose job is over
// (#933, D3, AC16).
func TestReconcileAfterARestart(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	api.statuses["run-a"], api.statuses["job-b"], api.statuses["run-dead"], api.statuses["run-over"] = "applying", "running", "planning", "applied"
	d := newFakeDriver("container")
	d.reattach = true
	placement := func(job string, ended bool) driver.Placement {
		return driver.Placement{ID: "c-" + job, Ended: ended, Labels: map[string]string{driver.LabelJob: job, driver.LabelRunner: "runner-1", driver.LabelManaged: "true"}}
	}
	d.listed = []driver.Placement{placement("run-a", false), placement("job-b", false), placement("run-dead", true), placement("run-over", false)}
	l := New(testConfig(api), d)
	l.goos = "linux"
	runLauncher(t, l)

	eventually(t, "the re-attachment", func() bool { return l.running() == 2 })
	eventually(t, "the dead placement's job failed", func() bool { _, _, failed, _ := api.snapshot(); return failed["run-dead"] != "" })
	eventually(t, "the finished job's placement stopped", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return slices.Contains(d.killed, "run-over")
	})
	// Re-attaching claims nothing, so no new attempt starts. It does mint a token for each job the
	// launcher still holds, of the attempt that is running: the launcher reports a job's failure
	// with the job's token (the API's JobData guard refuses the runner token there), and the token
	// carries the timeout the supervision enforces.
	started, tokens, failed, _ := api.snapshot()
	slices.Sort(tokens)
	if len(started) != 0 || !slices.Equal(tokens, []string{"job-b", "run-a", "run-dead"}) {
		t.Errorf("re-attaching claimed %v and minted %v: want no claim and a token for each active job", started, tokens)
	}
	if failed["run-a"] != "" || failed["job-b"] != "" {
		t.Errorf("a re-attached job was failed: %v", failed)
	}
	d.end("run-a", exitCompleted)
	d.end("job-b", exitCompleted)
	eventually(t, "the re-attached jobs finished", func() bool { return l.running() == 0 })
}
