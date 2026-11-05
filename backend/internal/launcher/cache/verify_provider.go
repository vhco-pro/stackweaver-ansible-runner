// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// ProviderVerifier promotes a provider package only after it matches the configuration's
// dependency lock file and its source's proof (D7, AC35): from a registry, the GPG signature of the
// shasums document against the keys the registry lists and the zip against the shasum; from the
// mirror block's provider network mirror, the zip against the mirror's zh: hashes.
type ProviderVerifier struct {
	// NetworkMirror is the mirror block's provider network mirror; empty means each provider's own
	// registry.
	NetworkMirror string
	HTTP          *http.Client
	// Scheme is how registry hosts are reached; empty means https (tests serve plain http).
	Scheme string
}

type providerPackage struct {
	Filename            string `json:"filename"`
	DownloadURL         string `json:"download_url"`
	ShasumsURL          string `json:"shasums_url"`
	ShasumsSignatureURL string `json:"shasums_signature_url"`
	Shasum              string `json:"shasum"`
	SigningKeys         struct {
		GPGPublicKeys []struct {
			KeyID      string `json:"key_id"`
			ASCIIArmor string `json:"ascii_armor"`
		} `json:"gpg_public_keys"`
	} `json:"signing_keys"`
}

// Verify implements Verifier. e.Name is the provider's source address (host/namespace/type),
// e.Platform its os_arch, and e.Hashes the lock file's hashes for the version.
func (v *ProviderVerifier) Verify(ctx context.Context, e runnercontract.CacheEntry, dir string) (*Artifact, error) {
	parts := strings.Split(e.Name, "/")
	if len(parts) != 3 || slices.Contains(parts, "") || slices.Contains(parts, "..") {
		return nil, fmt.Errorf("%q is not a provider source address", e.Name)
	}
	osName, arch, ok := strings.Cut(e.Platform, "_")
	if !ok {
		return nil, fmt.Errorf("%q is not a provider platform", e.Platform)
	}
	locked := lockedZipHashes(e.Hashes)
	if len(locked) == 0 {
		return nil, fmt.Errorf("the job's lock file pins no zh: hash for %s %s; a provider is promoted only against its lock file", e.Name, e.Version)
	}
	client := v.HTTP
	if client == nil {
		client = httpClient()
	}
	var (
		zipURL string
		want   []string
		source string
	)
	if v.NetworkMirror != "" {
		u, w, err := v.fromMirror(ctx, client, parts, e.Version, e.Platform)
		if err != nil {
			return nil, err
		}
		zipURL, want, source = u, w, u
	} else {
		pkg, err := v.fromRegistry(ctx, client, parts, e.Version, osName, arch, dir)
		if err != nil {
			return nil, err
		}
		zipURL, want, source = pkg.DownloadURL, []string{strings.ToLower(pkg.Shasum)}, pkg.DownloadURL
	}
	zip := filepath.Join(dir, "provider.zip")
	got, size, err := fetchTo(ctx, client, zipURL, zip, nil)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(want, got) {
		return nil, fmt.Errorf("%w: %s %s %s hashes to %s, its source publishes %v", ErrPoisoned, e.Name, e.Version, e.Platform, got, want)
	}
	if !slices.Contains(locked, got) {
		return nil, fmt.Errorf("%w: %s %s %s hashes to %s, which the configuration's lock file does not pin", ErrPoisoned, e.Name, e.Version, e.Platform, got)
	}
	return &Artifact{Path: zip, SHA256: got, Size: size, SourceURL: source, Verification: Verified}, nil
}

func (v *ProviderVerifier) scheme() string {
	if v.Scheme != "" {
		return v.Scheme
	}
	return "https"
}

// fromRegistry finds the package through the registry protocol and verifies the shasums
// document's signature with the keys the registry lists, and that the package's shasum is the one
// that document signs.
func (v *ProviderVerifier) fromRegistry(ctx context.Context, c *http.Client, addr []string, version, osName, arch, dir string) (*providerPackage, error) {
	host := v.scheme() + "://" + addr[0]
	disco, err := fetchBytes(ctx, c, host+"/.well-known/terraform.json", nil)
	if err != nil {
		return nil, err
	}
	var services map[string]any
	if err := json.Unmarshal(disco, &services); err != nil {
		return nil, fmt.Errorf("reading %s's service discovery: %w", addr[0], err)
	}
	base, _ := services["providers.v1"].(string)
	if base == "" {
		return nil, fmt.Errorf("%s offers no provider registry", addr[0])
	}
	baseURL, err := url.Parse(host + "/")
	if err != nil {
		return nil, err
	}
	ref, err := baseURL.Parse(base)
	if err != nil {
		return nil, err
	}
	pkgURL := strings.TrimRight(ref.String(), "/") + "/" + url.PathEscape(addr[1]) + "/" + url.PathEscape(addr[2]) + "/" + url.PathEscape(version) + "/download/" + url.PathEscape(osName) + "/" + url.PathEscape(arch)
	raw, err := fetchBytes(ctx, c, pkgURL, nil)
	if err != nil {
		return nil, err
	}
	var pkg providerPackage
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return nil, fmt.Errorf("reading the package document of %s: %w", strings.Join(addr, "/"), err)
	}
	resolve := func(s string) (string, error) {
		u, err := ref.Parse(s)
		if err != nil {
			return "", err
		}
		return u.String(), nil
	}
	for _, p := range []*string{&pkg.DownloadURL, &pkg.ShasumsURL, &pkg.ShasumsSignatureURL} {
		if *p, err = resolve(*p); err != nil {
			return nil, err
		}
	}
	sums, err := fetchBytes(ctx, c, pkg.ShasumsURL, nil)
	if err != nil {
		return nil, err
	}
	sig, err := fetchBytes(ctx, c, pkg.ShasumsSignatureURL, nil)
	if err != nil {
		return nil, err
	}
	var ring openpgp.EntityList
	for _, k := range pkg.SigningKeys.GPGPublicKeys {
		keys, err := openpgp.ReadArmoredKeyRing(strings.NewReader(k.ASCIIArmor))
		if err == nil {
			ring = append(ring, keys...)
		}
	}
	if len(ring) == 0 {
		return nil, fmt.Errorf("the registry lists no usable signing key for %s", strings.Join(addr, "/"))
	}
	if _, err := openpgp.CheckDetachedSignature(ring, bytes.NewReader(sums), bytes.NewReader(sig), nil); err != nil {
		return nil, fmt.Errorf("%w: the shasums of %s %s are not signed by the keys its registry lists: %v", ErrPoisoned, strings.Join(addr, "/"), version, err)
	}
	sumsPath := filepath.Join(dir, "SHA256SUMS")
	if err := os.WriteFile(sumsPath, sums, 0o600); err != nil {
		return nil, err
	}
	signed, err := sumFor(sumsPath, pkg.Filename)
	if err != nil {
		return nil, err
	}
	if signed != strings.ToLower(pkg.Shasum) {
		return nil, fmt.Errorf("%w: the registry's shasum for %s is not the one its signed shasums list", ErrPoisoned, pkg.Filename)
	}
	return &pkg, nil
}

// fromMirror reads the package's URL and hashes from a provider network mirror.
func (v *ProviderVerifier) fromMirror(ctx context.Context, c *http.Client, addr []string, version, platform string) (string, []string, error) {
	base, err := url.Parse(strings.TrimRight(v.NetworkMirror, "/") + "/")
	if err != nil {
		return "", nil, err
	}
	doc, err := base.Parse(url.PathEscape(addr[0]) + "/" + url.PathEscape(addr[1]) + "/" + url.PathEscape(addr[2]) + "/" + url.PathEscape(version) + ".json")
	if err != nil {
		return "", nil, err
	}
	raw, err := fetchBytes(ctx, c, doc.String(), nil)
	if err != nil {
		return "", nil, err
	}
	var idx struct {
		Archives map[string]struct {
			URL    string   `json:"url"`
			Hashes []string `json:"hashes"`
		} `json:"archives"`
	}
	if err := json.Unmarshal(raw, &idx); err != nil {
		return "", nil, fmt.Errorf("reading the mirror's document of %s: %w", strings.Join(addr, "/"), err)
	}
	a, ok := idx.Archives[platform]
	if !ok {
		return "", nil, fmt.Errorf("the mirror has no %s package of %s %s", platform, strings.Join(addr, "/"), version)
	}
	zipURL, err := doc.Parse(a.URL)
	if err != nil {
		return "", nil, err
	}
	want := lockedZipHashes(a.Hashes)
	if len(want) == 0 {
		return "", nil, fmt.Errorf("the mirror publishes no zh: hash for %s %s %s", strings.Join(addr, "/"), version, platform)
	}
	return zipURL.String(), want, nil
}

// lockedZipHashes are the zip digests of a lock file's or mirror's hash list ("zh:<hex>").
func lockedZipHashes(hashes []string) []string {
	var out []string
	for _, h := range hashes {
		if hex, ok := strings.CutPrefix(h, "zh:"); ok && validDigest(strings.ToLower(hex)) {
			out = append(out, strings.ToLower(hex))
		}
	}
	return out
}
