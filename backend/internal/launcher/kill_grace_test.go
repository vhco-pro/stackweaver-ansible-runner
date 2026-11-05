// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
	"github.com/michielvha/stackweaver/core/runnercontract"
	k8s "github.com/michielvha/stackweaver/core/runnerdriver/kubernetes"
)

// graceDriver records the grace of every kill and lets the test decide when the killed executor
// exits, as a real one does once OpenTofu has stopped on its interrupt and saved its state.
type graceDriver struct {
	*fakeDriver
	mu     sync.Mutex
	graces map[string]time.Duration
	killed chan string
}

func (d *graceDriver) Kill(_ context.Context, p driver.Placement, grace time.Duration) error {
	job := p.Labels[driver.LabelJob]
	d.mu.Lock()
	d.graces[job] = grace
	d.mu.Unlock()
	select {
	case d.killed <- job:
	default:
	}
	return nil
}

func (d *graceDriver) grace(job string) (time.Duration, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	g, ok := d.graces[job]
	return g, ok
}

// A cancel stops the executor with the full kill grace, so an interrupted apply has the time
// OpenTofu needs to save the state of what it already changed (D2, TFE's cancel). The grace is an
// upper bound, not a wait: the placement is removed as canceled the moment its executor exits, so a
// job that stops on its interrupt is gone within the cancel's ten seconds whatever the grace is
// (#933, D3, AC12).
func TestCancelKeepsTheGraceButRemovesOnExit(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	d := &graceDriver{fakeDriver: newFakeDriver("kubernetes"), graces: map[string]time.Duration{}, killed: make(chan string, 1)}
	cfg := testConfig(api)
	cfg.KillGrace = time.Hour
	l := New(cfg, d)
	l.goos = "linux"
	api.offer(runnercontract.PendingJob{JobID: "run-cancel", JobType: "tofu_run", RunType: "apply"})

	runLauncher(t, l)
	eventually(t, "the job placed", func() bool { return len(d.placements()) == 1 })
	api.mu.Lock()
	api.statuses["run-cancel"] = "canceled"
	api.mu.Unlock()

	select {
	case <-d.killed:
	case <-time.After(3 * time.Second):
		t.Fatal("the canceled job was not stopped")
	}
	if g, _ := d.grace("run-cancel"); g != time.Hour {
		t.Errorf("a cancel killed with grace %s, want the configured %s: a shorter one cuts OpenTofu's state save", g, time.Hour)
	}
	if _, removed := d.removal("run-cancel"); removed {
		t.Fatal("the placement was removed before its executor exited")
	}

	// The executor stops on its interrupt and exits with the stopped code.
	exited := time.Now()
	d.end("run-cancel", exitStopped)
	eventually(t, "the canceled placement removed", func() bool { _, ok := d.removal("run-cancel"); return ok })
	if waited := time.Since(exited); waited > 2*time.Second {
		t.Errorf("the placement was removed %s after its executor exited, want at once", waited)
	}
	if p, _ := d.removal("run-cancel"); !p.Canceled {
		t.Errorf("the placement was removed as a failure, which a keep window may keep: %+v", p)
	}
}

// The launcher's default kill grace is the job pod's terminationGracePeriodSeconds, so the kubelet
// never sends SIGKILL to an executor the launcher still grants time to report (#933, D3, D4).
func TestDefaultKillGraceIsThePodsGrace(t *testing.T) {
	if got := (Config{}).killGrace(); got != 60*time.Second {
		t.Errorf("default kill grace %s, want 60s", got)
	}
	pod := k8s.RenderPod(k8s.PodRequest{JobID: "run-x", Namespace: "jobs", PlatformImage: "platform:1", ExecutorPath: "/stackweaver"}, k8s.Template{})
	if pod.Spec.TerminationGracePeriodSeconds == nil {
		t.Fatal("the job pod sets no terminationGracePeriodSeconds")
	}
	if pg := time.Duration(*pod.Spec.TerminationGracePeriodSeconds) * time.Second; pg != (Config{}).killGrace() {
		t.Errorf("pod grace %s, launcher kill grace %s: they must agree", pg, (Config{}).killGrace())
	}
}
