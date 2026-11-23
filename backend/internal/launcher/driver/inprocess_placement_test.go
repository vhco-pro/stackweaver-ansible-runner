// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInProcessRetainsPlacementUntilExecutorExits(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	canceled := make(chan struct{})
	d := &InProcess{Executors: map[string]ExecutorFunc{"tofu": func(ctx context.Context, _ Invocation) int {
		<-ctx.Done()
		close(canceled)
		<-release
		return 0
	}}}
	p, err := d.Place(t.Context(), Request{JobID: "run-confirm-stop", Attempt: 1, Kind: "tofu"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Kill(t.Context(), p, time.Millisecond); !errors.Is(err, ErrStopUnconfirmed) {
		t.Errorf("stopping a running executor = %v, want ErrStopUnconfirmed", err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel the executor")
	}
	if err := d.Remove(t.Context(), p); err == nil {
		t.Fatal("remove forgot the running placement")
	}
	unblock()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := d.Wait(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := d.Kill(ctx, p, 0); err != nil {
		t.Fatalf("stopping an already finished job: %v", err)
	}
	if err := d.Remove(ctx, p); err != nil {
		t.Fatal(err)
	}
}

// The in-process driver hands its executor the placement it made, the id and the host, for the
// executor to report as the job's placement record (#933, D4, AC77).
func TestInProcessHandsTheExecutorItsPlacement(t *testing.T) {
	got := make(chan Invocation, 1)
	d := &InProcess{Executors: map[string]ExecutorFunc{"tofu": func(_ context.Context, inv Invocation) int { got <- inv; return 0 }}}
	p, err := d.Place(t.Context(), Request{
		JobID: "run-1", Attempt: 2, Kind: "tofu",
		CacheRemoteExhausted: true,
		Env:                  []string{"STACKWEAVER_CACHE_REMOTE_EXHAUSTED=false"},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case inv := <-got:
		host, _ := os.Hostname()
		if inv.Placement == nil || inv.Placement.PlacementID != p.ID || inv.Placement.Node != host {
			t.Errorf("invocation placement = %+v, want id %s on %s", inv.Placement, p.ID, host)
		}
		if !inv.CacheRemoteExhausted {
			t.Error("the executor did not receive the typed cache result")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the executor never ran")
	}
}

// The in-process driver enforces no limit, and says so in the job's output: a pool that asks for
// limits it does not require gets a report naming them as not enforced (#933, D4, AC24).
func TestInProcessReportsItsLimitsAsNotEnforced(t *testing.T) {
	got := make(chan Invocation, 1)
	d := &InProcess{Executors: map[string]ExecutorFunc{"tofu": func(_ context.Context, inv Invocation) int { got <- inv; return 0 }}}
	if _, err := d.Place(t.Context(), Request{JobID: "run-1", Attempt: 1, Kind: "tofu", Resources: Resources{MemoryLimit: "512Mi"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case inv := <-got:
		if inv.LimitsReport != LimitsReport(Resources{MemoryLimit: "512Mi"}, nil) || inv.LimitsReport == "" {
			t.Errorf("limits report = %q", inv.LimitsReport)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the executor never ran")
	}
}

// A pool with `limits: required` is refused by a driver that cannot enforce limits: the in-process
// driver places nothing and says why (#933, D4, AC24).
func TestInProcessRefusesRequiredLimits(t *testing.T) {
	ran := false
	d := &InProcess{Executors: map[string]ExecutorFunc{"tofu": func(context.Context, Invocation) int { ran = true; return 0 }}}
	_, err := d.Place(t.Context(), Request{JobID: "run-1", Attempt: 1, Kind: "tofu", Resources: Resources{CPULimit: "1", Required: true}})
	if err == nil || !strings.Contains(err.Error(), "requires enforced limits") {
		t.Fatalf("Place = %v, want the required limits refused", err)
	}
	if ran {
		t.Error("the executor ran for a refused placement")
	}
}
