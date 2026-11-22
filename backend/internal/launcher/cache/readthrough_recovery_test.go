// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package cache

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

type recoverySource func(context.Context, string) (io.ReadCloser, error)

func (source recoverySource) Fetch(ctx context.Context, _ string, _ runnercontract.CacheKind, sum string) (io.ReadCloser, error) {
	return source(ctx, sum)
}

func cacheProvider(payload string) runnercontract.CacheEntry {
	return runnercontract.CacheEntry{Kind: runnercontract.CacheKindProvider, SHA256: digest([]byte(payload))}
}

func TestReadThroughExhaustionSuppressesLaterRemoteEntriesForOnlyOneJob(t *testing.T) {
	root := t.TempDir()
	stalled := cacheProvider("stalled")
	later := cacheProvider("later")
	const localBody = "already verified"
	local := cacheProvider(localBody)
	localDir := filepath.Join(root, orgA, string(local.Kind))
	if err := os.MkdirAll(localDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, local.SHA256), []byte(localBody), 0o600); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		calls := map[string]int{}
		cache := &ReadThrough{Dir: root, Source: recoverySource(func(ctx context.Context, sum string) (io.ReadCloser, error) {
			calls[sum]++
			switch sum {
			case stalled.SHA256:
				<-ctx.Done()
				return nil, ctx.Err()
			case later.SHA256:
				return io.NopCloser(strings.NewReader("later")), nil
			default:
				return nil, ErrNotCached
			}
		})}
		start := time.Now()
		if _, err := cache.Prepare(t.Context(), descriptor(orgA, stalled, later, local)); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != 2*time.Minute || calls[stalled.SHA256] != 1 || calls[later.SHA256] != 0 || calls[local.SHA256] != 0 {
			t.Errorf("exhausted job must keep the one acquisition bound and skip later remote entries: elapsed=%s stalled=%d later=%d local=%d", elapsed, calls[stalled.SHA256], calls[later.SHA256], calls[local.SHA256])
		}
		if !fileHashes(filepath.Join(localDir, local.SHA256), local.SHA256) {
			t.Error("the later verified local entry was lost")
		}
		before := calls[later.SHA256]
		if _, err := cache.Prepare(t.Context(), descriptor(orgA, later)); err != nil {
			t.Fatal(err)
		}
		if calls[later.SHA256] != before+1 || !fileHashes(filepath.Join(localDir, later.SHA256), later.SHA256) {
			t.Error("the next job did not acquire its cache entry independently")
		}
	})
}

func TestReadThroughBusyWarmupDoesNotBlockAnotherJob(t *testing.T) {
	root := t.TempDir()
	entry := cacheProvider("delayed provider")
	fetching := make(chan struct{})
	release := make(chan struct{})
	stopWaiting := sync.OnceFunc(func() { close(release) })
	var calls atomic.Int32
	cache := &ReadThrough{Dir: root, Source: recoverySource(func(ctx context.Context, _ string) (io.ReadCloser, error) {
		if calls.Add(1) != 1 {
			return nil, ErrNotCached
		}
		close(fetching)
		select {
		case <-release:
			return io.NopCloser(strings.NewReader("delayed provider")), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})}
	first := make(chan error, 1)
	go func() {
		_, err := cache.Prepare(t.Context(), descriptor(orgA, entry))
		first <- err
	}()
	t.Cleanup(func() {
		stopWaiting()
		if err := <-first; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("first job did not finish: %v", err)
		}
	})
	select {
	case <-fetching:
	case <-time.After(5 * time.Second):
		t.Fatal("first job did not begin remote cache warm-up")
	}
	type result struct {
		directory string
		err       error
	}
	second := make(chan result, 1)
	go func() {
		dir, err := cache.Prepare(t.Context(), descriptor(orgA, entry))
		second <- result{directory: dir, err: err}
	}()
	select {
	case got := <-second:
		if got.err != nil || got.directory != filepath.Join(root, orgA) {
			t.Fatalf("second job did not receive its organization mount: %+v", got)
		}
		if info, err := os.Stat(got.directory); err != nil || !info.IsDir() {
			t.Fatalf("second job's mount is not available: %v", err)
		}
	case <-time.After(2 * time.Second):
		// The old implementation waits for the lock. Release its source and join both
		// goroutines before returning, including when that negative control fails.
		t.Error("second job waited behind the first job's remote cache warm-up")
		stopWaiting()
		<-second
	}
	if calls.Load() != 1 {
		t.Errorf("busy warm-up triggered %d remote fetches, want one", calls.Load())
	}
}

func TestReadThroughHealthyEntriesEachGetAnAcquisitionWindow(t *testing.T) {
	root := t.TempDir()
	contents := map[string]string{}
	entries := make([]runnercontract.CacheEntry, 0, 2)
	for _, body := range []string{"first provider", "second provider"} {
		entry := cacheProvider(body)
		entries = append(entries, entry)
		contents[entry.SHA256] = body
	}
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		cache := &ReadThrough{Dir: root, Source: recoverySource(func(ctx context.Context, sum string) (io.ReadCloser, error) {
			calls++
			select {
			case <-time.After(70 * time.Second):
				return io.NopCloser(strings.NewReader(contents[sum])), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})}
		start := time.Now()
		prepared, err := cache.PrepareJob(t.Context(), descriptor(orgA, entries...))
		if err != nil || prepared.RemoteExhausted || calls != 2 || time.Since(start) != 140*time.Second {
			t.Fatalf("two healthy entries did not get independent acquisition windows: elapsed=%s calls=%d result=%+v error=%v", time.Since(start), calls, prepared, err)
		}
		for _, entry := range entries {
			if !fileHashes(filepath.Join(prepared.Dir, string(entry.Kind), entry.SHA256), entry.SHA256) {
				t.Errorf("verified entry %s was not placed", entry.SHA256)
			}
		}
	})
}
