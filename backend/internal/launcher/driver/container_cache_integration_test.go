// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// The launcher's node-local cache in a job container (#933, D7, decision 19, AC34): the
// organization's directory is mounted read-only at CacheMountPath, from a host directory or from
// the launcher's named volume at the organization's subpath, and a write from the job fails.
//
//	go test -tags integration ./internal/launcher/driver/ -run TestContainerCache

//go:build integration

package driver

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestContainerCacheMountedReadOnlyFromADirectory(t *testing.T) {
	sfx := uuid.NewString()[:8]
	d := newTestDriver(t, "sw-probe-cache-"+sfx+":1")
	buildProbeImage(t, d.engine(), d.PlatformImage, "1001:1001")

	root := t.TempDir()
	org := uuid.NewString()
	other := uuid.NewString()
	for _, p := range []string{filepath.Join(root, org, "tofu", "aaaa"), filepath.Join(root, other, "tofu", "bbbb")} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil { //nolint:gosec // G301: read by the job's user
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("entry"), 0o444); err != nil { //nolint:gosec // G306: read by the job's user
			t.Fatal(err)
		}
	}
	_ = os.Chmod(root, 0o755) //nolint:gosec // G302: the job's user reads it through the mount

	res, _, exit := runProbe(t, d, Request{
		JobID: "cache-" + sfx, Attempt: 1, Kind: "tofu", Labels: map[string]string{LabelJob: "cache-" + sfx},
		Cache: &CacheMount{Dir: filepath.Join(root, org), Subpath: org},
	})
	if exit.Code != 0 {
		t.Fatalf("exit = %+v", exit)
	}
	if res.CacheDir != CacheMountPath {
		t.Errorf("STACKWEAVER_CACHE_DIR = %q, want %s", res.CacheDir, CacheMountPath)
	}
	if !slices.Equal(res.CacheFiles, []string{"tofu/aaaa"}) {
		t.Errorf("the job sees %v, want only its organization's entry", res.CacheFiles)
	}
	if res.CacheWritable {
		t.Error("the job wrote into the cache mount")
	}
}

func TestContainerCacheMountedReadOnlyFromAVolumeSubpath(t *testing.T) {
	sfx := uuid.NewString()[:8]
	d := newTestDriver(t, "sw-probe-cachevol-"+sfx+":1")
	e := d.engine()
	buildProbeImage(t, e, d.PlatformImage, "1001:1001")
	tools := "sw-tools-cachevol-" + sfx + ":1"
	buildToolsImage(t, e, tools)

	vol := "sw-test-cache-" + sfx
	if err := e.createVolume(t.Context(), vol, withTestRun(map[string]string{LabelManaged: "true"})); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.removeVolume(context.Background(), vol) })
	org, other := uuid.NewString(), uuid.NewString()
	fill, err := e.create(t.Context(), "sw-test-fill-"+sfx, containerConfig{
		Image:      tools,
		Cmd:        []string{"sh", "-c", "mkdir -p /v/" + org + "/collection /v/" + other + "/collection && echo e > /v/" + org + "/collection/cccc && echo e > /v/" + other + "/collection/dddd && chmod -R a+rX /v"},
		Labels:     withTestRun(map[string]string{LabelManaged: "true"}),
		HostConfig: hostConfig{Mounts: []mountSpec{{Type: "volume", Source: vol, Target: "/v"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.remove(context.Background(), fill) })
	if err := e.start(t.Context(), fill); err != nil {
		t.Fatal(err)
	}
	if code, err := e.wait(t.Context(), fill); err != nil || code != 0 {
		t.Fatalf("filling the volume: %d %v", code, err)
	}
	time.Sleep(100 * time.Millisecond)

	res, _, exit := runProbe(t, d, Request{
		JobID: "cachevol-" + sfx, Attempt: 1, Kind: "ansible", Labels: map[string]string{LabelJob: "cachevol-" + sfx},
		Cache: &CacheMount{Volume: vol, Subpath: org},
	})
	if exit.Code != 0 {
		t.Fatalf("exit = %+v", exit)
	}
	if !slices.Equal(res.CacheFiles, []string{"collection/cccc"}) {
		t.Errorf("the job sees %v, want only its organization's subpath", res.CacheFiles)
	}
	if res.CacheWritable {
		t.Error("the job wrote into the cache volume")
	}
}
