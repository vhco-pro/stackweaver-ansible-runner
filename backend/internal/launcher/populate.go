// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/michielvha/stackweaver/backend/internal/launcher/cache"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// errStrictOffline is hosted.strict refusing a launcher that would promote tofu releases without
// the transparency log (AC76).
var errStrictOffline = errors.New("cache.verify_transparency_log=false is refused under hosted.strict")

// promotion is one promotion and the mirror block its sources are fetched from.
type promotion struct {
	p      runnercontract.CachePromotion
	mirror *runnercontract.Mirror
}

// UploadCacheEntry stores a verified artifact in the promotion's organization's cache, under the
// promotion, with the runner token (PUT /runner/cache/:kind/:sha256).
func (c *Client) UploadCacheEntry(ctx context.Context, p runnercontract.CachePromotion, a *cache.Artifact) error {
	f, err := os.Open(a.Path) //nolint:gosec // G304: the populator's own scratch directory
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	version := p.Entry.Version
	if a.Version != "" {
		version = a.Version
	}
	q := url.Values{
		"organization": {p.OrganizationID}, "promotion": {p.ID}, "name": {p.Entry.Name}, "version": {version},
		"platform": {p.Entry.Platform}, "source": {a.SourceURL}, "verification": {a.Verification},
	}
	u := c.serverURL + "/api/v2/runner/cache/" + url.PathEscape(string(p.Entry.Kind)) + "/" + url.PathEscape(a.SHA256) + "?" + q.Encode()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, f)
	if err != nil {
		return err
	}
	req.ContentLength = a.Size
	req.Header.Set("Authorization", "Bearer "+c.token())
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req) //nolint:gosec // G704: the server URL is the operator-configured endpoint
	if err != nil {
		return fmt.Errorf("uploading the %s cache entry %s: %w", p.Entry.Kind, a.SHA256, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &StatusError{Code: resp.StatusCode, Body: string(data)}
	}
	return nil
}

// DropCachePromotion settles a promotion the populator could not verify.
func (c *Client) DropCachePromotion(ctx context.Context, p runnercontract.CachePromotion, reason string, poisoned bool) error {
	body, err := json.Marshal(struct {
		Reason   string `json:"reason"`
		Poisoned bool   `json:"poisoned"`
	}{reason, poisoned})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.serverURL+"/api/v2/runner/cache/promotions/"+url.PathEscape(p.ID)+"/drop", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token())
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req) //nolint:gosec // G704: the server URL is the operator-configured endpoint
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &StatusError{Code: resp.StatusCode, Body: string(data)}
	}
	return nil
}

// defaultVerifiers are the populator's verifiers for the mirror block's sources, with the
// launcher's verification settings (D7).
func (l *Launcher) defaultVerifiers(m *runnercontract.Mirror) map[runnercontract.CacheKind]cache.Verifier {
	if m == nil {
		m = &runnercontract.Mirror{}
	}
	client := mirrorHTTPClient(m)
	servers := make([]cache.GalaxyServer, 0, len(m.GalaxyServers))
	for _, s := range m.GalaxyServers {
		servers = append(servers, cache.GalaxyServer{URL: s.URL, Token: s.Token})
	}
	return map[runnercontract.CacheKind]cache.Verifier{
		runnercontract.CacheKindTofu:       &cache.TofuVerifier{ReleasesURL: m.TofuReleasesURL, Cosign: l.cfg.Cosign, VerifyTransparencyLog: l.cfg.VerifyTransparencyLog, HTTP: client},
		runnercontract.CacheKindProvider:   &cache.ProviderVerifier{NetworkMirror: m.ProviderNetworkMirror, HTTP: client},
		runnercontract.CacheKindCollection: &cache.CollectionVerifier{Servers: servers, HTTP: client},
		runnercontract.CacheKindRole:       &cache.RoleVerifier{Servers: servers, HTTP: client},
		runnercontract.CacheKindGit:        &cache.GitVerifier{},
		runnercontract.CacheKindPip:        &cache.PipVerifier{IndexURL: m.PipIndexURL, TrustHost: m.PipTrustedHost, HTTP: client},
	}
}

// mirrorHTTPClient trusts the mirror block's extra roots beside the system's, so an internal
// mirror's certificate verifies.
func mirrorHTTPClient(m *runnercontract.Mirror) *http.Client {
	c := &http.Client{Timeout: 5 * time.Minute}
	if m.CACertificates == "" {
		return c
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if pool.AppendCertsFromPEM([]byte(m.CACertificates)) {
		t := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // the standard library's transport
		t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		c.Transport = t
	}
	return c
}

// queuePromotions hands a heartbeat's promotions to the populator, each once.
func (l *Launcher) queuePromotions(ps []runnercontract.CachePromotion, m *runnercontract.Mirror) {
	for _, p := range ps {
		l.mu.Lock()
		_, busy := l.promoting[p.ID]
		if !busy {
			l.promoting[p.ID] = struct{}{}
		}
		l.mu.Unlock()
		if busy {
			continue
		}
		select {
		case l.promotions <- promotion{p: p, mirror: m}:
		default:
			// The queue is full: the promotion stays pending and comes back with a later heartbeat.
			l.mu.Lock()
			delete(l.promoting, p.ID)
			l.mu.Unlock()
		}
	}
}

// populate runs the populator over queued promotions, one at a time, until ctx ends.
func (l *Launcher) populate(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case w := <-l.promotions:
			pop := &cache.Populator{Verifiers: l.verifiers(w.mirror), Store: l.client, Logf: l.client.logf}
			if err := pop.Promote(ctx, w.p); err != nil && ctx.Err() == nil {
				l.client.logf("Promoting the %s cache entry %s %s failed: %v", w.p.Entry.Kind, w.p.Entry.Name, w.p.Entry.Version, err)
			}
			l.mu.Lock()
			delete(l.promoting, w.p.ID)
			l.mu.Unlock()
		}
	}
}

// PullCredential reads a run image's registry credential under the runner token (GET
// /runner/images/:id/pull-credential); the launcher pulls with it and never hands it to the job.
func (c *Client) PullCredential(ctx context.Context, imageID string) (runnercontract.PullCredential, error) {
	var out runnercontract.PullCredential
	err := c.call(ctx, http.MethodGet, "/api/v2/runner/images/"+url.PathEscape(imageID)+"/pull-credential", c.token(), nil, &out, 30*time.Second)
	return out, err
}
