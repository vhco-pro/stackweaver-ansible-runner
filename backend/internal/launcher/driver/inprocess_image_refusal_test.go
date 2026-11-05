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
	d := &InProcess{PlatformImage: "ghcr.io/vhco-pro/stackweaver-runner:1.4.0", Executors: map[string]ExecutorFunc{
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
	d := &InProcess{PlatformImage: "platform:1", Executors: map[string]ExecutorFunc{
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
