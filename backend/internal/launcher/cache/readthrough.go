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
	if _, err := uuid.Parse(d.OrganizationID); err != nil {
		return "", fmt.Errorf("the cache descriptor names organization %q: %w", d.OrganizationID, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	orgDir := filepath.Join(r.Dir, d.OrganizationID)
	if err := os.MkdirAll(orgDir, 0o755); err != nil { //nolint:gosec // G301: the job reads it through a read-only mount
		return "", fmt.Errorf("creating the organization's cache directory: %w", err)
	}
	keep := map[string]bool{}
	for _, e := range d.Entries {
		if !e.Kind.Valid() || !validDigest(e.SHA256) {
			continue
		}
		path := filepath.Join(orgDir, string(e.Kind), e.SHA256)
		keep[path] = true
		if fileHashes(path, e.SHA256) {
			now := time.Now()
			_ = os.Chtimes(path, now, now)
			continue
		}
		// Absent, or altered on the node: never used, fetched again.
		_ = os.Remove(path)
		if err := r.fetch(ctx, d.OrganizationID, e.CacheEntry, path); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if !errors.Is(err, ErrNotCached) {
				r.logf("Cache entry %s %s of organization %s not placed: %v", e.Kind, e.SHA256, d.OrganizationID, err)
			}
		}
	}
	r.evict(keep)
	return orgDir, nil
}

// fetch places one entry at path, only once its body hashes to its key.
func (r *ReadThrough) fetch(ctx context.Context, org string, e runnercontract.CacheEntry, path string) error {
	body, err := r.Source.Fetch(ctx, org, e.Kind, e.SHA256)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // G301: read through a read-only mount
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".incoming-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(tmp, h), body)
	closeErr := tmp.Close()
	if copyErr != nil || closeErr != nil {
		return errors.Join(copyErr, closeErr)
	}
	if hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
		return fmt.Errorf("the %s entry %s does not hash to its key; not placed", e.Kind, e.SHA256)
	}
	// Read-only on the node too: the job's mount is read-only, and nothing but this step writes.
	if err := os.Chmod(tmp.Name(), 0o444); err != nil { //nolint:gosec // G302: read by job users through the read-only mount
		return err
	}
	return os.Rename(tmp.Name(), path)
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
