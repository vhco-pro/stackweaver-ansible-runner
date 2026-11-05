// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// The verifiers against the real sources (#933, D7, AC35): a published OpenTofu release with
// cosign and with the release key, a provider from registry.opentofu.org and a collection from
// Galaxy. Skips when the sources are unreachable. Run with:
//
//	go test -tags integration ./internal/launcher/cache/ -run TestRealSource

//go:build integration

package cache

import (
	"context"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

func reachable(t *testing.T, url string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("%s is unreachable: %v", url, err)
	}
	_ = resp.Body.Close()
}

func TestRealSource_TofuRelease(t *testing.T) {
	reachable(t, "https://github.com")
	e := runnercontract.CacheEntry{Kind: runnercontract.CacheKindTofu, Name: "tofu", Version: "1.10.6", Platform: "linux_amd64"}
	t.Run("release key, offline", func(t *testing.T) {
		a, err := (&TofuVerifier{VerifyTransparencyLog: false}).Verify(t.Context(), e, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if a.Verification != VerifiedOffline || a.Size < 1<<20 {
			t.Errorf("artifact = %+v", a)
		}
	})
	t.Run("cosign with the transparency log", func(t *testing.T) {
		cosign, err := exec.LookPath("cosign")
		if err != nil {
			t.Skip("cosign is not installed")
		}
		a, err := (&TofuVerifier{VerifyTransparencyLog: true, Cosign: cosign}).Verify(t.Context(), e, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if a.Verification != Verified {
			t.Errorf("artifact = %+v", a)
		}
	})
}

func TestRealSource_Provider(t *testing.T) {
	reachable(t, "https://registry.opentofu.org/.well-known/terraform.json")
	// The published digest of hashicorp/null 3.2.4 for linux_amd64, which `tofu providers lock`
	// writes into the lock file as its zh: hash.
	const published = "3d106c7e32a929e2843f732625a582e562ff09120021e510a51a6f5d01175b8d"
	v := &ProviderVerifier{}
	e := runnercontract.CacheEntry{Kind: runnercontract.CacheKindProvider, Name: "registry.opentofu.org/hashicorp/null", Version: "3.2.4", Platform: "linux_amd64", Hashes: []string{"zh:" + published}}
	a, err := v.Verify(t.Context(), e, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a.SHA256 != published {
		t.Errorf("artifact = %+v", a)
	}
	e.Hashes = []string{"zh:" + strings.Repeat("0", 64)}
	if _, err := v.Verify(t.Context(), e, t.TempDir()); !errors.Is(err, ErrPoisoned) {
		t.Errorf("a provider the lock file does not pin: %v, want ErrPoisoned", err)
	}
}

func TestRealSource_GalaxyCollection(t *testing.T) {
	reachable(t, DefaultGalaxyServer)
	a, err := (&CollectionVerifier{}).Verify(t.Context(), runnercontract.CacheEntry{Kind: runnercontract.CacheKindCollection, Name: "ansible.posix", Version: "2.2.2"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a.SHA256 != "00a58c5d804c9adc99c3c3dc1b9f2246f4bb5f7337941440e0956f0e31c3b82b" {
		t.Errorf("ansible.posix 2.2.2 = %s", a.SHA256)
	}
}
