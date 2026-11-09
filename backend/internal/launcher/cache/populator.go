// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// Verification modes an upload records (core/models CacheVerified and CacheVerifiedOffline).
const (
	Verified        = "verified"
	VerifiedOffline = "offline"
)

// ErrPoisoned marks a verification that found the source's artifact differs from what the job
// reported or from the source's own published digest: an attempted poisoning, dropped with an event
// on the job and a pool-level audit record.
var ErrPoisoned = errors.New("the artifact does not match")

// Artifact is what a verifier fetched from the source and proved: a local file, its digest and how
// it was verified.
type Artifact struct {
	Path         string
	SHA256       string
	Size         int64
	SourceURL    string
	Verification string
	// Version replaces the entry's version when the verifier pinned it further (a role's tag
	// resolved to its commit).
	Version string
}

// Verifier fetches one kind of entry from its source, by name and version, and proves it with the
// verification of D7's table. dir is a private scratch directory the verifier may fill.
type Verifier interface {
	Verify(ctx context.Context, e runnercontract.CacheEntry, dir string) (*Artifact, error)
}

// Uploader stores a verified artifact in the organization's cache (PUT /runner/cache/:kind/:sha256).
type Uploader interface {
	UploadCacheEntry(ctx context.Context, p runnercontract.CachePromotion, a *Artifact) error
	DropCachePromotion(ctx context.Context, p runnercontract.CachePromotion, reason string, poisoned bool) error
}

// Populator promotes the entries finished jobs reported (#933, D7). It runs in the launcher, in the
// platform image, with no user code: for each entry it fetches the artifact again from its source,
// verifies it, confirms it is what the job reported, and only then uploads it. Nothing a job reported
// is trusted.
type Populator struct {
	Verifiers map[runnercontract.CacheKind]Verifier
	Store     Uploader
	// TmpDir is where verifiers work; empty means the system's.
	TmpDir string
	Logf   func(format string, args ...any)
}

// PopulatorConfig is the deployment's verification setting and the strictness it is checked
// against (AC76).
type PopulatorConfig struct {
	// VerifyTransparencyLog is cache.verify_transparency_log: false records tofu releases verified
	// without the log's inclusion proof, as "offline".
	VerifyTransparencyLog bool
	// Strict is hosted.strict, under which the log check cannot be switched off.
	Strict bool
}

// Check refuses a configuration hosted.strict forbids.
func (c PopulatorConfig) Check() error {
	if c.Strict && !c.VerifyTransparencyLog {
		return errors.New("cache.verify_transparency_log=false is refused under strict hosted isolation (STACKWEAVER_HOSTED_STRICT): hosted caches are verified against the transparency log")
	}
	return nil
}

func (p *Populator) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
	}
}

// Promote verifies and uploads one reported entry, or drops it with its reason.
func (p *Populator) Promote(ctx context.Context, pr runnercontract.CachePromotion) error {
	e := pr.Entry
	v := p.Verifiers[e.Kind]
	if v == nil {
		return p.Store.DropCachePromotion(ctx, pr, fmt.Sprintf("no verifier for %s entries on this launcher", e.Kind), false)
	}
	dir, err := os.MkdirTemp(p.TmpDir, "populate-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	a, err := v.Verify(ctx, e, dir)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		poisoned := errors.Is(err, ErrPoisoned)
		p.logf("Cache entry %s %s %s not promoted: %v", e.Kind, e.Name, e.Version, err)
		return p.Store.DropCachePromotion(ctx, pr, err.Error(), poisoned)
	}
	if e.SHA256 != "" && a.SHA256 != e.SHA256 {
		reason := fmt.Sprintf("the source's %s %s %s hashes to %s, the job reported %s: an attempted poisoning", e.Kind, e.Name, e.Version, a.SHA256, e.SHA256)
		p.logf("%s", reason)
		return p.Store.DropCachePromotion(ctx, pr, reason, true)
	}
	return p.Store.UploadCacheEntry(ctx, pr, a)
}

// httpClient is what verifiers fetch with: bounded, so a hung source cannot stall the populator.
func httpClient() *http.Client { return &http.Client{Timeout: 5 * time.Minute} }

// maxArtifactBytes bounds one fetched artifact.
const maxArtifactBytes int64 = 1 << 30

// fetchTo downloads url into path and returns its sha256 and size.
func fetchTo(ctx context.Context, c *http.Client, url, path string, headers map[string]string) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req) //nolint:gosec // G107: the mirror block's or the upstream source's URL
	if err != nil {
		return "", 0, fmt.Errorf("fetching %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("fetching %s: %s", url, resp.Status)
	}
	f, err := os.Create(path) //nolint:gosec // G304: the verifier's scratch directory
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxArtifactBytes+1))
	if err := errors.Join(copyErr, f.Close()); err != nil {
		return "", 0, fmt.Errorf("fetching %s: %w", url, err)
	}
	if n > maxArtifactBytes {
		return "", 0, fmt.Errorf("%s is larger than %d bytes", url, maxArtifactBytes)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// fetchBytes downloads a small document.
func fetchBytes(ctx context.Context, c *http.Client, url string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req) //nolint:gosec // G107: the mirror block's or the upstream source's URL
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}
