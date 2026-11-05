// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package cache

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// AC76: without the transparency log a tofu release is verified against the release key's GPG
// signature of its SHA256SUMS, offline, and recorded "offline"; cosign is not run, a release signed
// by another key or a zip that does not match is still refused, and the mode is refused under
// hosted.strict.
func TestTofuVerifier_OfflineVerifiesTheReleaseKeySignature(t *testing.T) {
	key := newKey(t, "opentofu release")
	fpr := fmt.Sprintf("%X", key.PrimaryKey.Fingerprint)
	zip := []byte("air-gapped tofu zip")
	srv := releasesServer(t, "1.9.1", zip, zip, key)
	cosign, argsFile := fakeCosign(t, true)
	v := &TofuVerifier{ReleasesURL: srv.URL, Cosign: cosign, VerifyTransparencyLog: false, ReleaseKey: armoredPublic(t, key), ReleaseKeyFingerprint: fpr}
	a, err := v.Verify(t.Context(), tofuEntry("1.9.1"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a.Verification != VerifiedOffline || a.SHA256 != digest(zip) {
		t.Errorf("artifact = %+v, want an offline verification", a)
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Error("cosign ran in offline mode")
	}

	forged := releasesServer(t, "1.9.1", zip, zip, newKey(t, "attacker"))
	v.ReleasesURL = forged.URL
	if _, err := v.Verify(t.Context(), tofuEntry("1.9.1"), t.TempDir()); !errors.Is(err, ErrPoisoned) {
		t.Errorf("a SHA256SUMS signed by another key: %v, want ErrPoisoned", err)
	}
	tampered := releasesServer(t, "1.9.1", zip, []byte("tampered while offline"), key)
	v.ReleasesURL = tampered.URL
	if _, err := v.Verify(t.Context(), tofuEntry("1.9.1"), t.TempDir()); !errors.Is(err, ErrPoisoned) {
		t.Errorf("offline mode and a zip that does not match: %v, want ErrPoisoned", err)
	}

	// The key must carry the pinned fingerprint.
	v.ReleasesURL = srv.URL
	v.ReleaseKeyFingerprint = strings.Repeat("0", 40)
	if _, err := v.Verify(t.Context(), tofuEntry("1.9.1"), t.TempDir()); err == nil {
		t.Error("a release key without the pinned fingerprint was trusted")
	}
}

func TestTofuVerifier_BundledKeyCarriesThePinnedFingerprint(t *testing.T) {
	ring, err := openpgp.ReadArmoredKeyRing(strings.NewReader(tofuReleaseKey))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range ring {
		if fmt.Sprintf("%X", k.PrimaryKey.Fingerprint) == TofuReleaseKeyFingerprint {
			return
		}
	}
	t.Fatalf("the bundled release key does not carry %s", TofuReleaseKeyFingerprint)
}

func TestPopulatorConfig_OfflineRefusedUnderStrict(t *testing.T) {
	if err := (PopulatorConfig{Strict: true, VerifyTransparencyLog: false}).Check(); err == nil {
		t.Error("hosted.strict accepted cache.verify_transparency_log=false")
	}
	for _, ok := range []PopulatorConfig{{Strict: true, VerifyTransparencyLog: true}, {Strict: false, VerifyTransparencyLog: false}} {
		if err := ok.Check(); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
}
