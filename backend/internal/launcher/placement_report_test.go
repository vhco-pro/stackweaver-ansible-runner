// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"testing"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// The claim reports the driver, the image and the level it came from (#933, D4, AC77). Until run
// images resolve a job's own image, every job runs the launcher's image, the platform default.
func TestClaimReportsThePlatformImageLevel(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	d := newFakeDriver("container")
	cfg := testConfig(api)
	cfg.Image = "stackweaver-runner:test"
	l := New(cfg, d)
	l.goos = "linux"
	api.offer(runnercontract.PendingJob{JobID: "run-1", JobType: "tofu_run", RunType: "plan"})

	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	go func() { _ = l.Run(ctx) }()
	eventually(t, "the job placed", func() bool { return len(d.placements()) == 1 })

	got := api.startPlacement("run-1")
	if got == nil || got.Driver != "container" || got.Image != "stackweaver-runner:test" || got.ImageLevel != runnercontract.ImageLevelPlatform {
		t.Errorf("start placement = %+v, want the container driver, the launcher's image and level %q", got, runnercontract.ImageLevelPlatform)
	}
}
