// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// One container launcher serves both engines (one-launcher spec, A2, A3, AC2): with no engine or
// image setting it declares tofu and ansible, and it places an OpenTofu run in the OpenTofu job
// image and an Ansible job and every sync in the Ansible job image, as each placement record and
// each job container show. A probe stamped with its engine stands in for each job image's executor.
//
// Gated behind `integration`; skips unless an engine answers on $STACKWEAVER_TEST_ENGINE_SOCKET
// (default /var/run/docker.sock). Run with:
//
//	go test -tags integration ./internal/launcher/ -run TestCombinedLauncher

//go:build integration

package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// engineSocket is the container engine's socket, or a skip.
func engineSocket(t *testing.T) string {
	t.Helper()
	sock := firstOf(os.Getenv("STACKWEAVER_TEST_ENGINE_SOCKET"), "/var/run/docker.sock")
	if err := (&driver.Container{Socket: sock}).Ping(t.Context()); err != nil {
		t.Skipf("no container engine on %s: %v", sock, err)
	}
	return sock
}

// buildStampedJobImage builds an image holding the driver's static probe at /probe, stamped with
// stamp so a job container tells which job image it runs, as user 1001.
func buildStampedJobImage(t *testing.T, tag, stamp string) {
	t.Helper()
	dir := t.TempDir()
	build := exec.CommandContext(t.Context(), "go", "build", "-ldflags", "-X main.stamp="+stamp, "-o", filepath.Join(dir, "probe"), "./driver/testdata/probe") //nolint:gosec // G204: the test builds its own fixture
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the probe: %v: %s", err, out)
	}
	dockerfile := "FROM scratch\nLABEL " + driver.LabelManaged + "=true\nCOPY probe /probe\nUSER 1001:1001\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(t.Context(), "docker", "build", "-q", "-t", tag, dir).CombinedOutput(); err != nil { //nolint:gosec // G204: the test's own image
		t.Fatalf("building %s: %v: %s", tag, err, out)
	}
	t.Cleanup(func() { _ = exec.CommandContext(context.Background(), "docker", "rmi", "-f", tag).Run() }) //nolint:gosec // G204: the test's own image
}

// recordingDriver is the container driver, recording the image each job's container runs while it
// is placed: the launcher removes a container once its job ends.
type recordingDriver struct {
	*driver.Container
	mu     sync.Mutex
	images map[string]string // job id -> the container's image
}

func (d *recordingDriver) Place(ctx context.Context, req driver.Request) (driver.Placement, error) {
	p, err := d.Container.Place(ctx, req)
	if err != nil {
		return p, err
	}
	out, ierr := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.Config.Image}}", p.ID).Output() //nolint:gosec // G204: a container this test's launcher placed
	d.mu.Lock()
	defer d.mu.Unlock()
	if ierr == nil {
		d.images[req.JobID] = strings.TrimSpace(string(out))
	}
	return p, nil
}

func (d *recordingDriver) image(job string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.images[job]
}

func TestCombinedLauncher_ServesBothEnginesInTheirJobImages(t *testing.T) {
	sock := engineSocket(t)

	// No engine and no image setting: a container launcher serves both engines.
	env := map[string]string{EnvServer: "https://api.test", EnvToken: "swa-agent", EnvPoolID: "pool-1", EnvDriver: driver.NameContainer}
	plain, err := ConfigFromEnv(bothExecutors(), envOf(env))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plain.Engines, []string{EngineTofu, EngineAnsible}) {
		t.Fatalf("engines = %v, want both", plain.Engines)
	}

	sfx := uuid.NewString()[:8]
	tofuImage, ansibleImage := "sw-combined-tofu-"+sfx+":1", "sw-combined-ansible-"+sfx+":1"
	buildStampedJobImage(t, tofuImage, "tofu")
	buildStampedJobImage(t, ansibleImage, "ansible")

	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	cfg := testConfig(api)
	cfg.Engines = plain.Engines
	cfg.JobImages = map[string]string{EngineTofu: tofuImage, EngineAnsible: ansibleImage}
	// The probe stands in for the executor at its own path in these job images.
	cfg.ExecutorCommand = map[string][]string{EngineTofu: {"/probe", "executor"}, EngineAnsible: {"/probe", "executor"}}
	cfg.JobImagePull = driver.PullMissing
	cfg.StatusInterval = 200 * time.Millisecond
	d := &recordingDriver{Container: &driver.Container{Socket: sock, JobImages: cfg.JobImages, JobImagePull: driver.PullMissing, ExecutorPath: "/probe"}, images: map[string]string{}}
	l := New(cfg, d)
	l.goos = "linux"
	if got := l.registerRequest().Engines; !slices.Equal(got, []string{EngineTofu, EngineAnsible}) {
		t.Fatalf("the registration declares %v, want both engines (stored as combined)", got)
	}

	want := map[string]string{
		"run-" + sfx:                  tofuImage,
		"job-" + sfx:                  ansibleImage,
		"wi-snapshot-" + sfx:          ansibleImage,
		"wi-playbook-snapshot-" + sfx: ansibleImage,
		"wi-inventory-sync-" + sfx:    ansibleImage,
	}
	api.offer(
		runnercontract.PendingJob{JobID: "run-" + sfx, JobType: "tofu_run", RunType: "plan"},
		runnercontract.PendingJob{JobID: "job-" + sfx, JobType: "ansible_job"},
		runnercontract.PendingJob{JobID: "wi-snapshot-" + sfx, JobType: runnercontract.JobTypeWorkItem},
		runnercontract.PendingJob{JobID: "wi-playbook-snapshot-" + sfx, JobType: runnercontract.JobTypeWorkItem},
		runnercontract.PendingJob{JobID: "wi-inventory-sync-" + sfx, JobType: runnercontract.JobTypeWorkItem},
	)
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	done := make(chan struct{})
	go func() { defer close(done); _ = l.Run(ctx) }()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		finished := 0
		for job := range want {
			if d.image(job) != "" && !l.isInflight(job) {
				finished++
			}
		}
		if finished == len(want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not every job was placed and ended within three minutes: images %v", d.images)
		}
		time.Sleep(200 * time.Millisecond)
	}
	stop()
	<-done

	_, _, failed, _ := api.snapshot()
	for job, image := range want {
		if got := api.startPlacement(job); got == nil || got.Image != image || got.Driver != driver.NameContainer {
			t.Errorf("%s placement record = %+v, want the container driver and %s", job, got, image)
		}
		if got := d.image(job); got != image {
			t.Errorf("%s ran in a container of %s, want %s", job, got, image)
		}
		if msg, ok := failed[job]; ok && msg != "" {
			t.Errorf("%s was reported failed: %s", job, msg)
		}
	}
	raw, _ := json.Marshal(failed)
	if bytes.Contains(raw, []byte("placed")) {
		t.Errorf("a job could not be placed: %s", raw)
	}
}

// isInflight reports whether job is still supervised.
func (l *Launcher) isInflight(job string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.inflight[job]
	return ok
}
