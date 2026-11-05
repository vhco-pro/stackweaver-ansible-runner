// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// A job is placed with the limits and the keep-failed window of its own pool, which its token
// carries (#933, D4, AC24, AC25).
func TestClaimPlacesWithThePoolSettings(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	api.pools = map[string]*runnercontract.PoolSettings{"run-1": {KeepFailedForSeconds: 600, CPULimit: "1500m", MemoryLimit: "2Gi", LimitsRequired: true}}
	d := newFakeDriver("container")
	l := New(testConfig(api), d)
	l.goos = "linux"
	api.offer(runnercontract.PendingJob{JobID: "run-1", JobType: "tofu_run", RunType: "plan"})

	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	go func() { _ = l.Run(ctx) }()
	eventually(t, "the job placed", func() bool { return len(d.placements()) == 1 })

	req := d.placements()[0]
	if req.Resources.CPULimit != "1500m" || req.Resources.MemoryLimit != "2Gi" || !req.Resources.Required {
		t.Errorf("placed with resources %+v, want the pool's 1500m, 2Gi, required", req.Resources)
	}
	if req.KeepFailedFor != 10*time.Minute {
		t.Errorf("placed keeping a failure for %s, want the pool's 10m", req.KeepFailedFor)
	}
}
