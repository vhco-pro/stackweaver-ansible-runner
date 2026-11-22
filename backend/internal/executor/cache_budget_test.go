// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

func TestCacheBudget_ExhaustionIsJobScopedAndPreservesLocalHits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := NewClient("http://control.example.test", "job-token", "runner-1")
		var calls atomic.Int64
		client.http.Transport = controlRoundTrip(func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			return controlAnswer(req, http.StatusInternalServerError, "unavailable"), nil
		})
		cache := &Cache{Dir: t.TempDir(), Client: client}
		entry := ref(runnercontract.CacheKindTofu, []byte("local release"))
		destination := filepath.Join(t.TempDir(), "release")
		start := time.Now()
		if source, err := cache.Fetch(t.Context(), entry, destination); source != SourceNone || err == nil {
			t.Fatalf("first exhausted read = %q, %v", source, err)
		}
		before := calls.Load()
		if source, err := cache.Fetch(t.Context(), entry, destination); source != SourceNone || err != nil {
			t.Fatalf("later fallback = %q, %v", source, err)
		}
		if time.Since(start) != 2*time.Minute || calls.Load() != before {
			t.Fatalf("later remote entry repeated acquisition: %s, requests %d -> %d", time.Since(start), before, calls.Load())
		}
		local := filepath.Join(cache.Dir, string(entry.Kind), entry.SHA256)
		if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(local, []byte("local release"), 0o600); err != nil {
			t.Fatal(err)
		}
		if source, err := cache.Fetch(t.Context(), entry, destination); source != SourceLocal || err != nil {
			t.Fatalf("verified local hit after exhaustion = %q, %v", source, err)
		}
		fresh := &Cache{Client: client}
		if _, err := fresh.Fetch(t.Context(), entry, destination); err == nil {
			t.Fatal("fresh job reader skipped its remote acquisition")
		}
		if calls.Load() <= before || time.Since(start) != 4*time.Minute {
			t.Fatalf("fresh job has no fresh budget: %s, requests %d", time.Since(start), calls.Load())
		}
		if _, err := os.Stat(destination + ".partial"); !os.IsNotExist(err) {
			t.Errorf("partial cache download remains: %v", err)
		}
	})
}

func TestCacheBudget_ConcurrentFetchesAfterExhaustion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := NewClient("http://control.example.test", "job-token", "runner-1")
		var calls atomic.Int64
		client.http.Transport = controlRoundTrip(func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			return controlAnswer(req, http.StatusInternalServerError, "unavailable"), nil
		})
		cache := &Cache{Client: client}
		entry := ref(runnercontract.CacheKindProvider, []byte("provider"))
		dir := t.TempDir()
		var wg sync.WaitGroup
		for i := range 4 {
			wg.Go(func() { _, _ = cache.Fetch(t.Context(), entry, filepath.Join(dir, "first-"+strconv.Itoa(i))) })
		}
		wg.Wait()
		before := calls.Load()
		for i := range 4 {
			wg.Go(func() {
				source, err := cache.Fetch(t.Context(), entry, filepath.Join(dir, "later-"+strconv.Itoa(i)))
				if source != SourceNone || err != nil {
					t.Errorf("later concurrent fallback = %q, %v", source, err)
				}
			})
		}
		wg.Wait()
		if calls.Load() != before {
			t.Errorf("later concurrent entries reached the API: %d -> %d", before, calls.Load())
		}
	})
}
