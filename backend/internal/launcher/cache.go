// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/michielvha/stackweaver/backend/internal/launcher/cache"
	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// FetchCacheEntry reads an entry of an organization's cache under the runner token, for the
// node-local read-through copy (#933, D7, decision 19). An entry the organization does not hold is
// cache.ErrNotCached.
func (c *Client) FetchCacheEntry(ctx context.Context, org string, kind runnercontract.CacheKind, sha256 string) (io.ReadCloser, error) {
	u := c.serverURL + "/api/v2/runner/cache/" + url.PathEscape(string(kind)) + "/" + url.PathEscape(sha256) + "?organization=" + url.QueryEscape(org)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token())
	resp, err := c.http.Do(req) //nolint:gosec // G704: the server URL is the operator-configured endpoint
	if err != nil {
		return nil, fmt.Errorf("fetching the %s cache entry %s: %w", kind, sha256, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, cache.ErrNotCached
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		return nil, &StatusError{Code: resp.StatusCode, Body: string(data)}
	}
	return resp.Body, nil
}

// clientSource adapts the client to the read-through's source.
type clientSource struct{ c *Client }

func (s clientSource) Fetch(ctx context.Context, org string, kind runnercontract.CacheKind, sum string) (io.ReadCloser, error) {
	return s.c.FetchCacheEntry(ctx, org, kind, sum)
}

// runsInAContainer reports whether the launcher itself runs in a container, where its own paths are
// not the engine host's.
func runsInAContainer() bool {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	return false
}

// prepareCache readies a claimed job's entries in the node-local copy and returns the mount the
// driver places it with, or nil when there is nothing to mount.
func (l *Launcher) prepareCache(ctx context.Context, jobID string, d *runnercontract.CacheDescriptor) *driver.CacheMount {
	if l.readThrough == nil || d == nil || len(d.Entries) == 0 {
		return nil
	}
	// A job pod cannot mount a node directory under the restricted Pod Security profile, so on
	// Kubernetes the job reads its entries through the API.
	if l.driver.Name() == driver.NameKubernetes {
		return nil
	}
	// A containerised launcher's directory is not on the engine host: the job reads the cache only
	// through the launcher's named volume, or not from the node at all.
	if l.driver.Name() == driver.NameContainer && l.cfg.CacheVolume == "" && l.inContainer {
		return nil
	}
	dir, err := l.readThrough.Prepare(ctx, d)
	if err != nil {
		l.client.logf("Preparing the cache of job %s failed, the job reads it through the API: %v", jobID, err)
		return nil
	}
	return &driver.CacheMount{Dir: dir, Volume: l.cfg.CacheVolume, Subpath: d.OrganizationID}
}
