// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// Invocation is what the in-process driver hands an executor function: the job and its token, in
// memory.
type Invocation struct {
	ServerURL string
	JobID     string
	Token     string
	RunType   string
	RunnerID  string
	// Placement is where the driver placed the job, for the executor to report (AC77).
	Placement *runnercontract.Placement
	// CacheDir is the launcher's copy of the job's organization's cache; empty for none. The
	// executor only reads it.
	CacheDir string
	// LimitsReport says which of the pool's limits were enforced, for the job's output; the
	// in-process driver enforces none (AC24).
	LimitsReport string
}

// ExecutorFunc runs one job to its end and returns the executor's exit code; it must stop when ctx
// ends.
type ExecutorFunc func(ctx context.Context, inv Invocation) int

// ErrImageRefused is returned by the in-process driver for a job whose resolved image is not the
// platform image (AC30): the job would run on the launcher host's own toolchain, not the image it
// asked for.
var ErrImageRefused = errors.New("the in-process driver runs the platform image only")

// InProcess runs the executor in the launcher's own process (D4): no isolation beyond the
// executor's allowlisted environment and the non-dumpable launcher, so it is for customer pools
// that choose it and dev slots, never a hosted launcher under hosted.strict.
type InProcess struct {
	// PlatformImage is the image this launcher's toolchain is: a job resolved to it, or to no image,
	// runs here; any other is refused.
	PlatformImage string
	// Executors maps an engine ("tofu", "ansible") to the executor that runs its jobs.
	Executors map[string]ExecutorFunc

	mu      sync.Mutex
	running map[string]*inprocessJob
}

type inprocessJob struct {
	placement Placement
	cancel    context.CancelFunc
	done      chan struct{}
	code      int
}

// Name is "inprocess".
func (d *InProcess) Name() string { return NameInProcess }

// Capabilities: nothing is isolated and a placement does not outlive the launcher.
func (d *InProcess) Capabilities() Capabilities { return Capabilities{} }

// Ping always succeeds: there is nothing to reach.
func (d *InProcess) Ping(context.Context) error { return nil }

// Place starts the executor for req's engine on a goroutine.
func (d *InProcess) Place(ctx context.Context, req Request) (Placement, error) {
	if req.Image != "" && req.Image != d.PlatformImage {
		return Placement{}, fmt.Errorf("%w: the job resolved the image %s, and this pool's launcher runs %s; use a container or Kubernetes pool for a custom run image", ErrImageRefused, req.Image, d.platformImageName())
	}
	run := d.Executors[req.Kind]
	if run == nil {
		return Placement{}, fmt.Errorf("this launcher has no %s executor", req.Kind)
	}
	if req.Resources.Required {
		return Placement{}, errors.New("the pool requires enforced limits, which the in-process driver cannot apply")
	}
	p := Placement{ID: "inprocess-" + req.JobID + "-" + strconv.Itoa(req.Attempt), Labels: maps.Clone(req.Labels)}
	// The job runs past the request that placed it: its own context, ended by Kill.
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	j := &inprocessJob{placement: p, cancel: cancel, done: make(chan struct{})}
	d.mu.Lock()
	if d.running == nil {
		d.running = map[string]*inprocessJob{}
	}
	d.running[p.ID] = j
	d.mu.Unlock()
	host, _ := os.Hostname()
	inv := Invocation{
		ServerURL: req.Server, JobID: req.JobID, Token: req.Token, RunType: req.RunType, RunnerID: req.RunnerID,
		Placement:    &runnercontract.Placement{PlacementID: p.ID, Node: host},
		LimitsReport: LimitsReport(req.Resources, nil),
	}
	if req.Cache != nil {
		inv.CacheDir = req.Cache.Dir
	}
	go func() {
		defer close(j.done)
		defer cancel()
		j.code = run(ctx, inv)
	}()
	return p, nil
}

func (d *InProcess) platformImageName() string {
	if d.PlatformImage == "" {
		return "the platform image"
	}
	return d.PlatformImage
}

func (d *InProcess) job(p Placement) (*inprocessJob, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	j := d.running[p.ID]
	if j == nil {
		return nil, ErrNotFound
	}
	return j, nil
}

// Wait blocks until the executor returns.
func (d *InProcess) Wait(ctx context.Context, p Placement) (Exit, error) {
	j, err := d.job(p)
	if err != nil {
		return Exit{}, err
	}
	select {
	case <-j.done:
		return Exit{Code: j.code}, nil
	case <-ctx.Done():
		return Exit{}, ctx.Err()
	}
}

// Kill cancels the executor's context, which stops its user process, and waits up to grace for it
// to report.
func (d *InProcess) Kill(_ context.Context, p Placement, grace time.Duration) error {
	j, err := d.job(p)
	if err != nil {
		return err
	}
	j.cancel()
	select {
	case <-j.done:
	case <-time.After(grace):
	}
	return nil
}

// Remove forgets an ended placement.
func (d *InProcess) Remove(_ context.Context, p Placement) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.running, p.ID)
	return nil
}

// List returns this process's running placements matching selector; none outlives the process.
func (d *InProcess) List(_ context.Context, selector map[string]string) ([]Placement, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []Placement
	for _, j := range d.running {
		if matches(j.placement.Labels, selector) {
			out = append(out, j.placement)
		}
	}
	return out, nil
}

// Logs is empty: an in-process executor logs to the launcher's own output.
func (d *InProcess) Logs(context.Context, Placement) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

// matches reports whether labels carry every selector entry.
func matches(labels, selector map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}
