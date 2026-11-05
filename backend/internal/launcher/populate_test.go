// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michielvha/stackweaver/backend/internal/launcher/cache"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// stubVerifier stands in for a source: it "fetches" body, or fails with err.
type stubVerifier struct {
	body []byte
	err  error
	seen *[]runnercontract.CacheEntry
}

func (v stubVerifier) Verify(_ context.Context, e runnercontract.CacheEntry, dir string) (*cache.Artifact, error) {
	*v.seen = append(*v.seen, e)
	if v.err != nil {
		return nil, v.err
	}
	path := filepath.Join(dir, "artifact")
	if err := os.WriteFile(path, v.body, 0o600); err != nil {
		return nil, err
	}
	return &cache.Artifact{Path: path, SHA256: hexDigest(v.body), Size: int64(len(v.body)), SourceURL: "https://galaxy.example/x.tar.gz", Verification: cache.Verified}, nil
}

// The promotions a heartbeat hands the launcher are verified by its populator and uploaded under
// the promotion, or dropped with the reason and whether it was a poisoning (#933, D7, AC35).
func TestHeartbeatPromotionsArePopulated(t *testing.T) {
	const org = "11111111-1111-1111-1111-111111111111"
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	api.promotions = []runnercontract.CachePromotion{
		{ID: "p-good", OrganizationID: org, JobID: "job-1", Entry: runnercontract.CacheEntry{Kind: runnercontract.CacheKindCollection, Name: "community.general", Version: "9.4.0"}},
		{ID: "p-bad", OrganizationID: org, JobID: "job-1", Entry: runnercontract.CacheEntry{Kind: runnercontract.CacheKindRole, Name: "evil.role", Version: "1.0.0"}},
	}
	l := New(testConfig(api), newFakeDriver("container"))
	l.goos = "linux"
	var seen []runnercontract.CacheEntry
	l.verifiers = func(*runnercontract.Mirror) map[runnercontract.CacheKind]cache.Verifier {
		return map[runnercontract.CacheKind]cache.Verifier{
			runnercontract.CacheKindCollection: stubVerifier{body: []byte("verified tarball"), seen: &seen},
			runnercontract.CacheKindRole:       stubVerifier{err: fmt.Errorf("%w: the archive differs from the tag", cache.ErrPoisoned), seen: &seen},
		}
	}
	runLauncher(t, l)

	eventually(t, "both promotions settled", func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		return len(api.uploads) == 1 && len(api.drops) == 1
	})
	api.mu.Lock()
	upload, drop := api.uploads[0], api.drops["p-bad"]
	api.mu.Unlock()
	path, body, _ := strings.Cut(upload, " ")
	if body != "verified tarball" || !strings.HasPrefix(path, "/cache/collection/"+hexDigest([]byte("verified tarball"))+"?") {
		t.Errorf("upload = %q", upload)
	}
	u, _ := url.Parse(path)
	q := u.Query()
	if q.Get("promotion") != "p-good" || q.Get("organization") != org || q.Get("name") != "community.general" || q.Get("version") != "9.4.0" || q.Get("verification") != cache.Verified {
		t.Errorf("upload query = %v", q)
	}
	if drop["poisoned"] != true || !strings.Contains(fmt.Sprint(drop["reason"]), "differs from the tag") {
		t.Errorf("drop = %v, want a poisoning with its reason", drop)
	}
}

// hosted.strict refuses a launcher configured to promote without the transparency log (AC76).
func TestStrictRefusesTheOfflineVerification(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	cfg := testConfig(api)
	cfg.Hosted, cfg.Strict, cfg.PoolID = true, true, ""
	cfg.VerifyTransparencyLog = false
	l := New(cfg, newFakeDriver("container"))
	l.goos = "linux"
	err := l.Run(t.Context())
	if err == nil || !errors.Is(err, errStrictOffline) {
		t.Errorf("Run = %v, want the offline verification refused under hosted.strict", err)
	}
}
