// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"strings"
	"testing"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// A job whose level rule resolves a run image is placed in that image, pulled with the image's
// registry credential, which the launcher reads under its runner token and never hands the job
// (#933, D6, AC27).
func TestClaimPlacesTheRunImageWithItsPullCredential(t *testing.T) {
	ref := "registry.internal:5000/acme/tofu-aws@sha256:" + strings.Repeat("ab", 32)
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	api.images = map[string]*runnercontract.JobImage{
		"run-1": {ID: "img-1", Ref: ref, Level: "workspace", Kind: "tofu", PullCredential: true},
		"run-2": {ID: "img-2", Ref: "ghcr.io/acme/public:1", Level: "organization", Kind: "tofu"},
	}
	api.pullCreds = map[string]runnercontract.PullCredential{"img-1": {Registry: "registry.internal:5000", Username: "robot", Password: "pull-s3cret"}}
	d := newFakeDriver("container")
	l := New(testConfig(api), d)
	l.goos = "linux"
	api.offer(runnercontract.PendingJob{JobID: "run-1", JobType: "tofu_run", RunType: "plan"}, runnercontract.PendingJob{JobID: "run-2", JobType: "tofu_run", RunType: "plan"})
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	go func() { _ = l.Run(ctx) }()
	eventually(t, "both jobs placed", func() bool { return len(d.placements()) == 2 })

	for _, req := range d.placements() {
		switch req.JobID {
		case "run-1":
			if req.Image != ref || req.Digest != "" && !strings.HasSuffix(ref, req.Digest) || req.PullAuth == nil || req.PullAuth.Password != "pull-s3cret" || req.PullAuth.Registry != "registry.internal:5000" {
				t.Errorf("run-1 placed with image %q auth %+v", req.Image, req.PullAuth)
			}
			for _, kv := range req.Env {
				if strings.Contains(kv, "pull-s3cret") {
					t.Error("the registry credential reached the job's environment")
				}
			}
		case "run-2":
			if req.Image != "ghcr.io/acme/public:1" || req.PullAuth != nil {
				t.Errorf("run-2 placed with image %q auth %+v", req.Image, req.PullAuth)
			}
		}
	}
}

// A run image whose credential cannot be read fails the job with the reason rather than placing it
// unauthenticated.
func TestClaimFailsAJobWhosePullCredentialIsGone(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	api.images = map[string]*runnercontract.JobImage{"run-1": {ID: "img-gone", Ref: "registry.internal/x:1", Level: "workspace", PullCredential: true}}
	d := newFakeDriver("container")
	l := New(testConfig(api), d)
	l.goos = "linux"
	api.offer(runnercontract.PendingJob{JobID: "run-1", JobType: "tofu_run", RunType: "plan"})
	runLauncher(t, l)
	eventually(t, "the job failed", func() bool {
		_, _, failed, _ := api.snapshot()
		return strings.Contains(failed["run-1"], "pull credential")
	})
	if n := len(d.placements()); n != 0 {
		t.Errorf("%d placements, want none", n)
	}
}

// A job whose pool offers a pull Secret for its image's registry is placed with that Secret on a
// driver that places pods, and its launcher reads no credential; on the container driver it pulls
// with the credential as before (#933, D8, AC48).
func TestClaimUsesThePoolsPullSecretOnPods(t *testing.T) {
	for _, tc := range []struct {
		driver     string
		wantSecret string
		wantAuth   bool
	}{{"kubernetes", "registry-ghcr", false}, {"container", "", true}} {
		t.Run(tc.driver, func(t *testing.T) {
			api := newFakeAPI(t, runnercontract.CapabilityJobToken)
			api.images = map[string]*runnercontract.JobImage{
				"run-1": {ID: "img-1", Ref: "ghcr.io/acme/tofu:1", Level: "workspace", Kind: "tofu", PullCredential: true, PullSecret: "registry-ghcr"},
			}
			api.pullCreds = map[string]runnercontract.PullCredential{"img-1": {Registry: "ghcr.io", Username: "robot", Password: "pull-s3cret"}}
			d := newFakeDriver(tc.driver)
			l := New(testConfig(api), d)
			l.goos = "linux"
			api.offer(runnercontract.PendingJob{JobID: "run-1", JobType: "tofu_run", RunType: "plan"})
			runLauncher(t, l)
			eventually(t, "the job placed", func() bool { return len(d.placements()) == 1 })
			req := d.placements()[0]
			if req.PullSecret != tc.wantSecret || (req.PullAuth != nil) != tc.wantAuth {
				t.Errorf("placed with pull Secret %q and auth %+v; want %q and auth %v", req.PullSecret, req.PullAuth, tc.wantSecret, tc.wantAuth)
			}
		})
	}
}
