// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

func hexDigest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// A claimed job's cache descriptor is prepared in the launcher's node-local copy, under its runner
// token, and the organization's directory is handed to the driver to mount read-only (#933, D7,
// decision 19, AC33).
func TestClaimPreparesTheNodeLocalCache(t *testing.T) {
	const org = "11111111-1111-1111-1111-111111111111"
	body := []byte("tofu 1.10.6 zip")
	sum := hexDigest(body)
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	api.caches = map[string]*runnercontract.CacheDescriptor{
		"run-1": {OrganizationID: org, Entries: []runnercontract.CacheEntryRef{{CacheEntry: runnercontract.CacheEntry{Kind: runnercontract.CacheKindTofu, SHA256: sum}}}},
		"run-2": {OrganizationID: org, Entries: []runnercontract.CacheEntryRef{{CacheEntry: runnercontract.CacheEntry{Kind: runnercontract.CacheKindTofu, SHA256: sum}}}},
	}
	api.cacheObjects = map[string][]byte{org + "/tofu/" + sum: body}
	d := newFakeDriver("container")
	cfg := testConfig(api)
	cfg.CacheDir = t.TempDir()
	cfg.CacheVolume = "stackweaver-launcher-cache"
	l := New(cfg, d)
	l.goos = "linux"
	api.offer(runnercontract.PendingJob{JobID: "run-1", JobType: "tofu_run", RunType: "plan"}, runnercontract.PendingJob{JobID: "run-2", JobType: "tofu_run", RunType: "plan"})

	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	go func() { _ = l.Run(ctx) }()
	eventually(t, "both jobs placed", func() bool { return len(d.placements()) == 2 })

	for _, req := range d.placements() {
		c := req.Cache
		if c == nil || c.Dir != filepath.Join(cfg.CacheDir, org) || c.Volume != cfg.CacheVolume || c.Subpath != org {
			t.Fatalf("%s placed with cache %+v", req.JobID, c)
		}
	}
	got, err := os.ReadFile(filepath.Join(cfg.CacheDir, org, "tofu", sum)) //nolint:gosec // G304: the test's directory
	if err != nil || string(got) != string(body) {
		t.Errorf("the entry was not placed: %v", err)
	}
	api.mu.Lock()
	fetches := len(api.cacheFetches)
	api.mu.Unlock()
	if fetches != 1 {
		t.Errorf("two jobs on one node fetched the entry %d times, want once", fetches)
	}
}

// Without a cache directory, or for a job with no descriptor, nothing is mounted.
func TestClaimWithoutACacheMountsNothing(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	api.caches = map[string]*runnercontract.CacheDescriptor{"run-1": {OrganizationID: "11111111-1111-1111-1111-111111111111"}}
	d := newFakeDriver("container")
	l := New(testConfig(api), d)
	l.goos = "linux"
	api.offer(runnercontract.PendingJob{JobID: "run-1", JobType: "tofu_run", RunType: "plan"})
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	go func() { _ = l.Run(ctx) }()
	eventually(t, "the job placed", func() bool { return len(d.placements()) == 1 })
	if c := d.placements()[0].Cache; c != nil {
		t.Errorf("placed with cache %+v and no cache directory configured", c)
	}
}
