// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A job whose resolved image is not the platform image is refused by the in-process driver with a
// message that names both images and the way out (#933, D4, AC30).
func TestInProcessRefusesANonPlatformImage(t *testing.T) {
	ran := false
	d := &InProcess{JobImages: map[string]string{"tofu": "ghcr.io/vhco-pro/stackweaver-runner:1.4.0"}, Executors: map[string]ExecutorFunc{
		"tofu": func(context.Context, Invocation) int { ran = true; return 0 },
	}}
	_, err := d.Place(t.Context(), Request{JobID: "run-1", Attempt: 1, Kind: "tofu", Image: "registry.example.test/acme/tofu-custom:2"})
	if !errors.Is(err, ErrImageRefused) {
		t.Fatalf("Place = %v, want ErrImageRefused", err)
	}
	for _, want := range []string{"registry.example.test/acme/tofu-custom:2", "stackweaver-runner:1.4.0", "container or Kubernetes pool"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
	if ran {
		t.Error("the executor ran for a refused image")
	}
}

// The platform image, or no image, runs the engine's executor in this process with the token in
// memory; Kill stops it and Wait reports its exit code.
func TestInProcessRunsAndStopsTheExecutor(t *testing.T) {
	got := make(chan Invocation, 1)
	d := &InProcess{JobImages: sameJobImage("platform:1"), Executors: map[string]ExecutorFunc{
		"ansible": func(ctx context.Context, inv Invocation) int {
			got <- inv
			<-ctx.Done()
			return 3
		},
	}}
	p, err := d.Place(t.Context(), Request{JobID: "job-1", Attempt: 2, Kind: "ansible", Server: "https://api", Token: "swj-1", RunnerID: "r1", Labels: map[string]string{LabelJob: "job-1"}})
	if err != nil {
		t.Fatal(err)
	}
	inv := <-got
	if inv.Token != "swj-1" || inv.JobID != "job-1" || inv.ServerURL != "https://api" {
		t.Errorf("invocation = %+v", inv)
	}
	if listed, _ := d.List(t.Context(), map[string]string{LabelJob: "job-1"}); len(listed) != 1 {
		t.Errorf("List found %d placements, want the running one", len(listed))
	}
	if err := d.Kill(t.Context(), p, time.Second); err != nil {
		t.Fatal(err)
	}
	exit, err := d.Wait(t.Context(), p)
	if err != nil || exit.Code != 3 {
		t.Errorf("Wait = %+v, %v; want the stopped exit code 3", exit, err)
	}
	_ = d.Remove(t.Context(), p)
	if listed, _ := d.List(t.Context(), nil); len(listed) != 0 {
		t.Errorf("a removed placement is still listed")
	}
}

// The refusal is per engine (one-launcher spec, A3): a job runs here only in its own engine's job
// image or with no image, so another engine's job image is refused like any custom image.
func TestInProcessRefusesPerEngine(t *testing.T) {
	ran := map[string]int{}
	run := func(engine string) ExecutorFunc {
		return func(context.Context, Invocation) int { ran[engine]++; return 0 }
	}
	d := &InProcess{
		JobImages: map[string]string{"tofu": "ghcr.io/vhco-pro/stackweaver-opentofu-runner:latest", "ansible": "ghcr.io/vhco-pro/stackweaver-ansible-runner:latest"},
		Executors: map[string]ExecutorFunc{"tofu": run("tofu"), "ansible": run("ansible")},
	}
	cases := []struct {
		kind, image string
		refused     bool
	}{
		{"tofu", "", false},
		{"tofu", "ghcr.io/vhco-pro/stackweaver-opentofu-runner:latest", false},
		{"tofu", "ghcr.io/vhco-pro/stackweaver-ansible-runner:latest", true},
		{"ansible", "", false},
		{"ansible", "ghcr.io/vhco-pro/stackweaver-ansible-runner:latest", false},
		{"ansible", "ghcr.io/vhco-pro/stackweaver-opentofu-runner:latest", true},
	}
	for i, tc := range cases {
		p, err := d.Place(t.Context(), Request{JobID: "job-" + tc.kind, Attempt: i + 1, Kind: tc.kind, Image: tc.image})
		if tc.refused {
			if !errors.Is(err, ErrImageRefused) {
				t.Errorf("%s job in %q: Place = %v, want ErrImageRefused", tc.kind, tc.image, err)
			} else if !strings.Contains(err.Error(), d.JobImages[tc.kind]) {
				t.Errorf("refusal %q does not name the %s job image", err, tc.kind)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s job in %q: Place = %v, want it run", tc.kind, tc.image, err)
			continue
		}
		if _, err := d.Wait(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	if ran["tofu"] != 2 || ran["ansible"] != 2 {
		t.Errorf("executors ran %v, want each engine's twice", ran)
	}
}
