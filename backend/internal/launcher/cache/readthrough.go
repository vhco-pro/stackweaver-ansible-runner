// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// Package cache is the launcher's side of the per-organization cache (#933, D7): the node-local
// read-through copy jobs read through a read-only mount, and the populator that promotes what a job
// reported only once it verified it against the source.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// ErrNotCached is a source's answer for an entry the organization's cache does not hold.
var ErrNotCached = errors.New("not in the organization's cache")

// Source is where the read-through fetches an entry it lacks: the API's GET /runner/cache route,
// under the launcher's runner token.
type Source interface {
	Fetch(ctx context.Context, org string, kind runnercontract.CacheKind, sha256 string) (io.ReadCloser, error)
}

// retryableSource lets a source classify its own availability failures. Other sources keep
// their existing final-error behavior; the read-through owns the acquisition deadline.
type retryableSource interface {
	Retryable(error) bool
}

// ReadThrough is the launcher-owned node-local copy of the caches of the organizations whose jobs
// the launcher runs (decision 19), laid out <Dir>/<organization>/<kind>/<sha256>. Only the launcher
// writes it: it fetches what a claimed job's descriptor names and the node lacks, verifies the
// sha256 before placing the file, and mounts the organization's directory read-only into the job.
type ReadThrough struct {
	Dir    string
	Source Source
	// Limit bounds the whole directory in bytes (cache.local_size_limit); zero means no bound.
	// Over it, the least recently used entries go first.
	Limit int64
	Logf  func(format string, args ...any)

	mu sync.Mutex
}

func (r *ReadThrough) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// Prepare makes every entry of d present and verified under its organization's directory and
// returns that directory, the source of the job's read-only mount. An entry the API does not hold,
// or whose body does not hash to its key, is left out: the executor fetches what it lacks through
// its blob route, and verifies everything it reads either way.
func (r *ReadThrough) Prepare(ctx context.Context, d *runnercontract.CacheDescriptor) (string, error) {
	prepared, err := r.PrepareJob(ctx, d)
	return prepared.Dir, err
}

// Preparation is the cache mount and the optional remote-acquisition state for one job.
type Preparation struct {
	Dir             string
	RemoteExhausted bool
}

// PrepareJob warms the organization cache without delaying a claim behind another
// job's prefetch. The mount remains usable during warm-up because entries are published
// by verified atomic rename and readers verify the local bytes.
func (r *ReadThrough) PrepareJob(ctx context.Context, d *runnercontract.CacheDescriptor) (Preparation, error) {
	if err := ctx.Err(); err != nil {
		return Preparation{}, err
	}
	if _, err := uuid.Parse(d.OrganizationID); err != nil {
		return Preparation{}, fmt.Errorf("the cache descriptor names organization %q: %w", d.OrganizationID, err)
	}
	prepared := Preparation{Dir: filepath.Join(r.Dir, d.OrganizationID)}
	if err := os.MkdirAll(prepared.Dir, 0o755); err != nil { //nolint:gosec // G301: the job reads it through a read-only mount
		return Preparation{}, fmt.Errorf("creating the organization's cache directory: %w", err)
	}
	if !r.mu.TryLock() {
		return prepared, ctx.Err()
	}
	defer r.mu.Unlock()
	keep := map[string]bool{}
	for _, e := range d.Entries {
		if !e.Kind.Valid() || !validDigest(e.SHA256) {
			continue
		}
		path := filepath.Join(prepared.Dir, string(e.Kind), e.SHA256)
		keep[path] = true
		if fileHashes(path, e.SHA256) {
			now := time.Now()
			_ = os.Chtimes(path, now, now)
			continue
		}
		if prepared.RemoteExhausted {
			continue
		}
		// An absent or altered copy is never used. Fetch it while this job
		// still has remote acquisition available.
		_ = os.Remove(path)
		exhausted, err := r.fetch(ctx, d.OrganizationID, e.CacheEntry, path)
		prepared.RemoteExhausted = exhausted
		if err != nil {
			if ctx.Err() != nil {
				return Preparation{}, ctx.Err()
			}
			if !errors.Is(err, ErrNotCached) {
				r.logf("Cache entry %s %s of organization %s not placed: %v", e.Kind, e.SHA256, d.OrganizationID, err)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return Preparation{}, err
	}
	r.evict(keep)
	return prepared, nil
}

// fetch places a verified entry and reports whether its acquisition window was exhausted.
func (r *ReadThrough) fetch(ctx context.Context, org string, e runnercontract.CacheEntry, path string) (bool, error) {
	deadline := time.Now().Add(2 * time.Minute)
	delay := 100 * time.Millisecond
	var last error
	for {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if !time.Now().Before(deadline) {
			return true, fmt.Errorf("acquiring cache entry %s: %w", e.SHA256, errors.Join(context.DeadlineExceeded, last))
		}
		retry, err := r.fetchAttempt(ctx, org, e, path, deadline)
		if err == nil || !retry {
			return false, err
		}
		last = err
		remaining := time.Until(deadline)
		if remaining <= 0 {
			continue
		}
		timer := time.NewTimer(min(delay, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
		}
		timer.Stop()
		delay = min(delay*2, 5*time.Second)
	}
}

func (r *ReadThrough) canRetry(err error) bool {
	if errors.Is(err, ErrNotCached) {
		return false
	}
	source, ok := r.Source.(retryableSource)
	if ok {
		return source.Retryable(err)
	}
	return errors.Is(err, context.DeadlineExceeded)
}

func (r *ReadThrough) fetchAttempt(ctx context.Context, org string, e runnercontract.CacheEntry, path string, deadline time.Time) (bool, error) {
	attempt, cancel := context.WithCancel(ctx)
	timedOut := make(chan struct{})
	timer := time.AfterFunc(time.Until(deadline), func() { cancel(); close(timedOut) })
	body, err := r.Source.Fetch(attempt, org, e.Kind, e.SHA256)
	if !timer.Stop() {
		<-timedOut
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	} else if attempt.Err() != nil {
		err = errors.Join(context.DeadlineExceeded, err)
	}
	if err != nil {
		cancel()
		if body != nil {
			if closeErr := body.Close(); closeErr != nil {
				r.logf("Closing an unavailable cache response: %v", closeErr)
			}
		}
		return r.canRetry(err), err
	}
	defer func() {
		cancel()
		if err := body.Close(); err != nil {
			r.logf("Closing cache entry %s: %v", e.SHA256, err)
		}
	}()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // G301: read through a read-only mount
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".incoming-*")
	if err != nil {
		return false, err
	}
	defer func() {
		if err := os.Remove(tmp.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			r.logf("Removing an incoming cache entry: %v", err)
		}
	}()
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(tmp, h), body)
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil {
		var fileErr *os.PathError
		retry := closeErr == nil && !errors.As(copyErr, &fileErr) && r.canRetry(copyErr)
		return retry, errors.Join(copyErr, closeErr)
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
		return false, fmt.Errorf("the %s entry %s does not hash to its key; not placed", e.Kind, e.SHA256)
	}
	// Read-only on the node too: the job's mount is read-only, and nothing but this step writes.
	if err := os.Chmod(tmp.Name(), 0o444); err != nil { //nolint:gosec // G302: read by job users through the read-only mount
		return false, err
	}
	return false, os.Rename(tmp.Name(), path)
}

// evict removes the least recently used entries until the directory is under Limit, never one in
// keep, which the job being placed is about to read.
func (r *ReadThrough) evict(keep map[string]bool) {
	if r.Limit <= 0 {
		return
	}
	type file struct {
		path string
		size int64
		used time.Time
	}
	var files []file
	var total int64
	_ = filepath.WalkDir(r.Dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable corner is skipped, not fatal
		}
		info, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // vanished meanwhile
		}
		files = append(files, file{path, info.Size(), info.ModTime()})
		total += info.Size()
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].used.Before(files[j].used) })
	for _, f := range files {
		if total <= r.Limit {
			return
		}
		if keep[f.path] {
			continue
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
}

// fileHashes reports whether the file at path exists and hashes to sum.
func fileHashes(path, sum string) bool {
	f, err := os.Open(path) //nolint:gosec // G304: a path under the launcher's own cache directory
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == sum
}

// validDigest reports whether s is a lowercase hex sha256.
func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
