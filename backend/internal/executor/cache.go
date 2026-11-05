// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// EnvCacheDir names the launcher's read-only copy of the organization's cache inside the job, when
// the launcher mounted one (decision 19).
const EnvCacheDir = "STACKWEAVER_CACHE_DIR"

// maxCacheEntryBytes bounds one entry read through the blob route.
const maxCacheEntryBytes int64 = 1 << 30

// Where a cache entry came from.
const (
	SourceLocal = "local"
	SourceBlob  = "blob"
	SourceNone  = "none"
)

// Cache reads the entries of a job's cache descriptor (#933, D7). It prefers the launcher's
// read-only copy at Dir and falls back to the blob route, verifies the sha256 of every entry before
// handing it over, and has no write path: it never writes Dir, and the cache's write route takes a
// runner token the executor does not hold.
type Cache struct {
	// Dir is the launcher's copy of the organization's cache, mounted read-only; empty when none.
	Dir    string
	Client *Client
}

// Fetch copies entry e to dst, verified, and reports where it came from. An entry the job cannot
// get (absent from the cache, refused) is SourceNone with no error: the job installs it from its
// source, and the launcher's populator may promote it afterwards.
func (c *Cache) Fetch(ctx context.Context, e runnercontract.CacheEntryRef, dst string) (string, error) {
	if c.Dir != "" {
		local := filepath.Join(c.Dir, string(e.Kind), e.SHA256)
		if err := copyVerified(local, dst, e.SHA256); err == nil {
			return SourceLocal, nil
		}
	}
	if c.Client == nil || e.Path == "" {
		return SourceNone, nil
	}
	tmp := dst + ".partial"
	err := c.Client.Download(ctx, e.Path, tmp, maxCacheEntryBytes)
	if err != nil {
		_ = os.Remove(tmp)
		var se *StatusError
		if errors.As(err, &se) && (se.Code == http.StatusForbidden || se.Code == http.StatusNotFound) {
			return SourceNone, nil
		}
		return SourceNone, fmt.Errorf("fetching the %s cache entry %s: %w", e.Kind, e.SHA256, err)
	}
	if !hashes(tmp, e.SHA256) {
		_ = os.Remove(tmp)
		return SourceNone, fmt.Errorf("the %s cache entry %s served by the API does not hash to its key", e.Kind, e.SHA256)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return SourceNone, err
	}
	return SourceBlob, nil
}

// copyVerified copies src to dst only when src hashes to sum.
func copyVerified(src, dst, sum string) error {
	in, err := os.Open(src) //nolint:gosec // G304: the read-only cache mount
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp := dst + ".partial"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // G304: the job's own directory
	if err != nil {
		return err
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(out, h), in)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil || hex.EncodeToString(h.Sum(nil)) != sum {
		_ = os.Remove(tmp)
		return errors.New("the local copy does not verify")
	}
	return os.Rename(tmp, dst)
}

// hashes reports whether the file at path hashes to sum.
func hashes(path, sum string) bool {
	f, err := os.Open(path) //nolint:gosec // G304: the job's own directory
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
