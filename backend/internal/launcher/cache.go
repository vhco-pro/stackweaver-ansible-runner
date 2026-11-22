// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/michielvha/stackweaver/backend/internal/launcher/cache"
	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
	"github.com/michielvha/stackweaver/backend/internal/launcher/readbody"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// FetchCacheEntry reads an entry of an organization's cache under the runner token, for the
// node-local read-through copy (#933, D7, decision 19). An entry the organization does not hold is
// cache.ErrNotCached.
func (c *Client) FetchCacheEntry(ctx context.Context, org string, kind runnercontract.CacheKind, sha256 string) (io.ReadCloser, error) {
	requestCtx, cancel := context.WithCancel(ctx)
	u := c.serverURL + "/api/v2/runner/cache/" + url.PathEscape(string(kind)) + "/" + url.PathEscape(sha256) + "?organization=" + url.QueryEscape(org)
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, u, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token())
	timedOut := make(chan struct{})
	timer := time.AfterFunc(20*time.Second, func() { cancel(); close(timedOut) })
	client := *c.http
	client.Timeout = 0
	resp, err := client.Do(req) //nolint:gosec // G704: the server URL is the operator-configured endpoint
	if err == nil && resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			err = cache.ErrNotCached
		} else {
			data, readErr := io.ReadAll(io.LimitReader(resp.Body, 512))
			code, _ := runnercontract.DecodeError(data)
			err = errors.Join(&StatusError{Code: resp.StatusCode, ErrorCode: code, Body: string(data)}, readErr)
		}
	}
	if !timer.Stop() {
		<-timedOut
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	} else if requestCtx.Err() != nil {
		err = errors.Join(context.DeadlineExceeded, err)
	}
	if err != nil {
		cancel()
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, fmt.Errorf("fetching the %s cache entry %s: %w", kind, sha256, err)
	}
	return readbody.New(requestCtx, resp.Body, cancel), nil
}

// clientSource adapts the client to the read-through's source.
type clientSource struct{ c *Client }

func (s clientSource) Fetch(ctx context.Context, org string, kind runnercontract.CacheKind, sum string) (io.ReadCloser, error) {
	return s.c.FetchCacheEntry(ctx, org, kind, sum)
}

// Retryable opts the HTTP source into the read-through's bounded availability retries.
// Cache authentication refusals keep their existing immediate fallback disposition.
func (s clientSource) Retryable(err error) bool {
	var status *StatusError
	if errors.As(err, &status) && status.Code == http.StatusUnauthorized {
		return false
	}
	return retryControl(err)
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

// prepareCache readies the node-local cache and returns its mount and the job's
// remote acquisition state. Paths that skip prefetch start with remote reads available.
func (l *Launcher) prepareCache(ctx context.Context, jobID string, d *runnercontract.CacheDescriptor) (*driver.CacheMount, bool) {
	if l.readThrough == nil || d == nil || len(d.Entries) == 0 {
		return nil, false
	}
	// A job pod cannot mount a node directory under the restricted Pod Security profile, so on
	// Kubernetes the job reads its entries through the API.
	if l.driver.Name() == driver.NameKubernetes {
		return nil, false
	}
	// A containerised launcher's directory is not on the engine host: the job reads the cache only
	// through the launcher's named volume, or not from the node at all.
	if l.driver.Name() == driver.NameContainer && l.cfg.CacheVolume == "" && l.inContainer {
		return nil, false
	}
	prepared, err := l.readThrough.PrepareJob(ctx, d)
	if err != nil {
		l.client.logf("Preparing the cache of job %s failed, the job reads it through the API: %v", jobID, err)
		return nil, false
	}
	return &driver.CacheMount{Dir: prepared.Dir, Volume: l.cfg.CacheVolume, Subpath: d.OrganizationID}, prepared.RemoteExhausted
}
