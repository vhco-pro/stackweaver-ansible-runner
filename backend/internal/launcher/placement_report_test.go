// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"slices"
	"testing"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// The claim reports the driver, the image and the level it came from (#933, D4, AC77). Until run
// images resolve a job's own image, every job runs the launcher's image, the platform default.
func TestClaimReportsThePlatformImageLevel(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	d := newFakeDriver("container")
	cfg := testConfig(api)
	cfg.JobImages = map[string]string{"tofu": "stackweaver-runner:test", "ansible": "stackweaver-ansible:test"}
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

// Each job is placed in, and its claim names, its own engine's job image, started with the
// command the launcher sends for that engine (one-launcher spec, A3).
func TestClaimNamesTheEnginesJobImage(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	d := newFakeDriver("container")
	cfg := testConfig(api)
	cfg.JobImages = map[string]string{"tofu": "opentofu-job:1", "ansible": "ansible-job:1"}
	cfg.ExecutorCommand = executorCommands()
	l := New(cfg, d)
	l.goos = "linux"
	api.offer(runnercontract.PendingJob{JobID: "run-1", JobType: "tofu_run", RunType: "plan"})
	api.offer(runnercontract.PendingJob{JobID: "job-1", JobType: "ansible_job"})

	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	go func() { _ = l.Run(ctx) }()
	eventually(t, "both jobs placed", func() bool { return len(d.placements()) == 2 })

	want := map[string]string{"run-1": "opentofu-job:1", "job-1": "ansible-job:1"}
	for job, image := range want {
		if got := api.startPlacement(job); got == nil || got.Image != image {
			t.Errorf("%s start placement = %+v, want image %s", job, got, image)
		}
	}
	for _, req := range d.placements() {
		if req.Image != want[req.JobID] {
			t.Errorf("%s placed in %s, want %s", req.JobID, req.Image, want[req.JobID])
		}
		if !slices.Equal(req.Command, []string{ExecutorPath, "executor"}) {
			t.Errorf("%s placed with command %v, want the executor at the constant path", req.JobID, req.Command)
		}
	}
}
