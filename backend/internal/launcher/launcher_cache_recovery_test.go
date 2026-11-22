// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
	"github.com/michielvha/stackweaver/core/runnercontract"
	"github.com/michielvha/stackweaver/core/security/runenv"
)

type cacheRecoveryPlacement struct {
	*fakeDriver
	placed func()
}

func (placement *cacheRecoveryPlacement) Place(ctx context.Context, req driver.Request) (driver.Placement, error) {
	placed, err := placement.fakeDriver.Place(ctx, req)
	placement.placed()
	return placed, err
}

func TestLauncherPrefetchExhaustionReachesPlacedExecutor(t *testing.T) {
	const key = "STACKWEAVER_CACHE_REMOTE_EXHAUSTED"
	const org = "11111111-1111-4111-8111-111111111111"
	for _, next := range []struct {
		name, inherited, want string
		prefetch              bool
	}{
		{"exhausted", "false", "true", true},
		{"no prefetch", "true", "false", false},
	} {
		t.Run(next.name, func(t *testing.T) {
			t.Setenv(key, next.inherited)
			t.Setenv(runenv.PassthroughVar, key)
			root := t.TempDir()
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				d := &cacheRecoveryPlacement{fakeDriver: newFakeDriver(driver.NameContainer), placed: cancel}
				l := New(Config{ServerURL: "http://control.example.test", CacheDir: root, CacheVolume: "cache"}, d)
				offer := recoveryOffers()[0]
				source := &recoveryAPI{boundRequests: true}
				cacheCalls := 0
				l.client.http.Transport = claimRecoveryRoundTrip(func(req *http.Request) (*http.Response, error) {
					switch {
					case strings.HasSuffix(req.URL.Path, "/token"):
						token := runnercontract.JobToken{
							Token: "job-token", JobID: offer.JobID, Attempt: offer.Attempt,
							ContractVersion: runnercontract.Version, TimeoutSeconds: 600,
						}
						if next.prefetch {
							token.Cache = &runnercontract.CacheDescriptor{OrganizationID: org, Entries: []runnercontract.CacheEntryRef{{CacheEntry: runnercontract.CacheEntry{
								Kind: runnercontract.CacheKindCollection, SHA256: hexDigest([]byte("missing collection")),
							}}}}
						}
						return recoveryResponse(req, http.StatusOK, token)
					case strings.HasPrefix(req.URL.Path, "/api/v2/runner/cache/"):
						cacheCalls++
						return recoveryResponse(req, http.StatusInternalServerError, map[string]string{"error": "cache unavailable"})
					default:
						return source.roundTrip(req)
					}
				})
				start := time.Now()
				l.acquire(ctx, offer, &supervised{jobID: offer.JobID, expected: runnercontract.JobTokenRequest{Attempt: offer.Attempt}})
				placed := d.placements()
				if len(placed) != 1 {
					t.Fatalf("wanted one executor placement, got %d", len(placed))
				}
				var values []string
				for _, pair := range placed[0].Env {
					if value, ok := strings.CutPrefix(pair, key+"="); ok {
						values = append(values, value)
					}
				}
				if len(values) != 1 || values[0] != next.want {
					t.Errorf("executor got cache exhaustion %q, want the sole launcher-owned value %q", values, next.want)
				}
				if next.prefetch && (cacheCalls < 2 || time.Since(start) != 2*time.Minute) {
					t.Errorf("placement did not observe exhaustion: calls=%d elapsed=%s", cacheCalls, time.Since(start))
				}
				if !next.prefetch && cacheCalls != 0 {
					t.Errorf("a job without prefetch made %d cache calls", cacheCalls)
				}
			})
		})
	}
}
