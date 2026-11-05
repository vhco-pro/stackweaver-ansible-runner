// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package cache

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// The identity OpenTofu signs its releases' SHA256SUMS with: the release workflow of the version's
// major.minor branch, issued by GitHub Actions (opentofu.org/docs/intro/install/standalone/).
const (
	tofuSigningIssuer   = "https://token.actions.githubusercontent.com"
	tofuSigningIdentity = "https://github.com/opentofu/opentofu/.github/workflows/release.yml@refs/heads/v"
	defaultTofuReleases = "https://github.com/opentofu/opentofu/releases/download"
	// TofuReleaseKeyFingerprint is OpenTofu's release signing key, which also signs every release's
	// SHA256SUMS as tofu_<version>_SHA256SUMS.gpgsig (the fingerprint get.opentofu.org's installer
	// pins).
	TofuReleaseKeyFingerprint = "E3E6E43D84CB852EADB0051D0C0AF313E5FD9F80" // gitleaks:allow (a public key fingerprint)
)

// tofuReleaseKey is OpenTofu's public release key as https://get.opentofu.org/opentofu.asc serves
// it, bundled so the offline rule needs nothing from the network but the mirror.
//
//go:embed opentofu-release-key.asc
var tofuReleaseKey string

var (
	tofuVersion  = regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.[0-9]+(-[A-Za-z0-9.]+)?$`)
	tofuPlatform = regexp.MustCompile(`^[a-z0-9]+_[a-z0-9]+$`)
)

// TofuVerifier promotes an OpenTofu release zip only after its digest matches the release's
// SHA256SUMS and that file's signature verifies (D7, AC35, AC76).
//
// With the transparency log check on (cache.verify_transparency_log, the default and the only
// setting under hosted.strict) the signature is verified with cosign verify-blob against the release
// workflow's identity, Rekor inclusion included. Keyless signing certificates live for ten minutes,
// so without the log cosign cannot tell the signature was made while the certificate was valid; the
// offline rule therefore verifies the release's GPG signature (SHA256SUMS.gpgsig) against OpenTofu's
// release key, bundled and pinned by fingerprint, and records the entry "offline".
type TofuVerifier struct {
	// ReleasesURL is the mirror block's release base; empty means the upstream.
	ReleasesURL string
	// Cosign is the cosign binary; empty means "cosign" on PATH.
	Cosign                string
	VerifyTransparencyLog bool
	// ReleaseKey replaces the bundled release key (armored), for a rotated key; it must still carry
	// TofuReleaseKeyFingerprint unless ReleaseKeyFingerprint names the new one.
	ReleaseKey            string
	ReleaseKeyFingerprint string
	HTTP                  *http.Client
}

// Verify implements Verifier.
func (v *TofuVerifier) Verify(ctx context.Context, e runnercontract.CacheEntry, dir string) (*Artifact, error) {
	m := tofuVersion.FindStringSubmatch(e.Version)
	if m == nil {
		return nil, fmt.Errorf("%q is not an OpenTofu version", e.Version)
	}
	if !tofuPlatform.MatchString(e.Platform) {
		return nil, fmt.Errorf("%q is not an OpenTofu platform", e.Platform)
	}
	client := v.HTTP
	if client == nil {
		client = httpClient()
	}
	base := strings.TrimRight(v.ReleasesURL, "/")
	if base == "" {
		base = defaultTofuReleases
	}
	release := fmt.Sprintf("%s/v%s/", base, e.Version)
	sums := filepath.Join(dir, "SHA256SUMS")
	sigFiles := []string{"SHA256SUMS.sig", "SHA256SUMS.pem"}
	if !v.VerifyTransparencyLog {
		sigFiles = []string{"SHA256SUMS.gpgsig"}
	}
	for _, name := range append([]string{"SHA256SUMS"}, sigFiles...) {
		if _, _, err := fetchTo(ctx, client, release+"tofu_"+e.Version+"_"+name, filepath.Join(dir, name), nil); err != nil {
			return nil, err
		}
	}
	verification := Verified
	if v.VerifyTransparencyLog {
		if err := v.cosign(ctx, sums, m[1]+"."+m[2]); err != nil {
			if errors.Is(err, exec.ErrNotFound) {
				return nil, fmt.Errorf("cosign is not installed on this launcher: %w", err)
			}
			return nil, fmt.Errorf("%w: the signature of tofu %s's SHA256SUMS does not verify: %v", ErrPoisoned, e.Version, err)
		}
	} else {
		if err := v.gpg(sums); err != nil {
			return nil, fmt.Errorf("%w: the release key's signature of tofu %s's SHA256SUMS does not verify: %v", ErrPoisoned, e.Version, err)
		}
		verification = VerifiedOffline
	}

	zipName := fmt.Sprintf("tofu_%s_%s.zip", e.Version, e.Platform)
	want, err := sumFor(sums, zipName)
	if err != nil {
		return nil, err
	}
	zip := filepath.Join(dir, zipName)
	got, size, err := fetchTo(ctx, client, release+zipName, zip, nil)
	if err != nil {
		return nil, err
	}
	if got != want {
		return nil, fmt.Errorf("%w: %s hashes to %s, its signed SHA256SUMS says %s", ErrPoisoned, zipName, got, want)
	}
	return &Artifact{Path: zip, SHA256: got, Size: size, SourceURL: release + zipName, Verification: verification}, nil
}

// cosign runs cosign verify-blob over the SHA256SUMS file against the release workflow's identity.
func (v *TofuVerifier) cosign(ctx context.Context, sums, majorMinor string) error {
	bin := v.Cosign
	if bin == "" {
		bin = "cosign"
	}
	cmd := exec.CommandContext(ctx, bin, "verify-blob", //nolint:gosec // G204: the launcher's own cosign over fixed arguments
		"--certificate-identity", tofuSigningIdentity+majorMinor,
		"--certificate-oidc-issuer", tofuSigningIssuer,
		"--signature", sums+".sig",
		"--certificate", sums+".pem",
		sums)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

// gpg verifies the SHA256SUMS file's detached signature against the pinned release key.
func (v *TofuVerifier) gpg(sums string) error {
	armored, fpr := tofuReleaseKey, TofuReleaseKeyFingerprint
	if v.ReleaseKey != "" {
		armored = v.ReleaseKey
	}
	if v.ReleaseKeyFingerprint != "" {
		fpr = strings.ToUpper(strings.ReplaceAll(v.ReleaseKeyFingerprint, " ", ""))
	}
	ring, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armored))
	if err != nil {
		return fmt.Errorf("reading the release key: %w", err)
	}
	var pinned openpgp.EntityList
	for _, k := range ring {
		if fmt.Sprintf("%X", k.PrimaryKey.Fingerprint) == fpr {
			pinned = append(pinned, k)
		}
	}
	if len(pinned) == 0 {
		return fmt.Errorf("the release key does not carry the pinned fingerprint %s", fpr)
	}
	data, err := os.ReadFile(sums) //nolint:gosec // G304: the verifier's scratch directory
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(filepath.Join(filepath.Dir(sums), "SHA256SUMS.gpgsig")) //nolint:gosec // G304: the verifier's scratch directory
	if err != nil {
		return err
	}
	_, err = openpgp.CheckDetachedSignature(pinned, bytes.NewReader(data), bytes.NewReader(sig), nil)
	return err
}

// sumFor reads name's digest from a SHA256SUMS file.
func sumFor(sumsPath, name string) (string, error) {
	data, err := readSmall(sumsPath)
	if err != nil {
		return "", err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("%s is not listed in its SHA256SUMS", name)
}
