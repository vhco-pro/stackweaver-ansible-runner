// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"testing"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// A launcher reports its deployment-level settings at registration, so every pool it serves shows
// them read-only (#933, D0, D15): hosted.strict, the cache verification mode and the node cache.
func TestRegisterRequestReportsLauncherSettings(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	cfg := testConfig(api)
	cfg.Strict, cfg.VerifyTransparencyLog, cfg.CacheDir, cfg.CacheLimit = true, true, t.TempDir(), 3<<30
	req := New(cfg, newFakeDriver("container")).registerRequest()
	if !req.HostedStrict || req.CacheVerification != runnercontract.CacheVerificationTransparencyLog || !req.LocalCache || req.LocalCacheSizeLimit != 3<<30 {
		t.Errorf("register request = %+v", req)
	}

	cfg.Strict, cfg.VerifyTransparencyLog, cfg.CacheDir = false, false, ""
	req = New(cfg, newFakeDriver("container")).registerRequest()
	if req.HostedStrict || req.CacheVerification != runnercontract.CacheVerificationOffline || req.LocalCache || req.LocalCacheSizeLimit != 0 {
		t.Errorf("register request = %+v, want no strictness, offline verification and no node cache", req)
	}
}
