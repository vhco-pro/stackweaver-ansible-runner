// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// digestDriver is a driver whose image digest is known only once the placement runs, as the
// Kubernetes driver's kubelet pulls the image.
type digestDriver struct {
	*fakeDriver
	digest string
}

func (d *digestDriver) ImageDigest(context.Context, driver.Placement) (string, error) {
	return d.digest, nil
}

// The launcher reports the digest its driver learns after the claim, with the job's token, so a
// job on Kubernetes records the image digest it ran in (#933, D4, AC27, AC77).
func TestSupervisionReportsTheImageDigestTheDriverLearns(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	d := &digestDriver{fakeDriver: newFakeDriver("kubernetes"), digest: digest}
	l := New(testConfig(api), d)
	l.goos = "linux"
	api.offer(runnercontract.PendingJob{JobID: "run-digest", JobType: "tofu_run"})

	runLauncher(t, l)
	eventually(t, "the digest reported", func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		p := api.reported["run-digest"]
		return p != nil && p.ImageDigest == digest
	})
}

// A short job ends before the launcher's next status read; its digest is still reported while it
// runs, because the digest is polled on its own, faster interval, not with the job's status (the
// custom run image specs saw an empty digest for jobs of a few seconds).
func TestTheImageDigestOfAShortJobIsReportedWhileItRuns(t *testing.T) {
	const digest = "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	d := &digestDriver{fakeDriver: newFakeDriver("kubernetes"), digest: digest}
	cfg := testConfig(api)
	cfg.StatusInterval = time.Hour
	l := New(cfg, d)
	l.goos = "linux"
	api.offer(runnercontract.PendingJob{JobID: "run-short", JobType: "tofu_run"})

	runLauncher(t, l)
	eventually(t, "the short job's digest reported while it runs", func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		p := api.reported["run-short"]
		return p != nil && p.ImageDigest == digest
	})
	d.end("run-short", 0)
}
