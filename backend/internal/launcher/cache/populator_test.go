// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// fakeCosign writes a cosign stand-in that records its arguments and succeeds unless fail is set.
func fakeCosign(t *testing.T, fail bool) (bin, argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	code := 0
	if fail {
		code = 1
	}
	bin = filepath.Join(dir, "cosign")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\n[ %d -eq 0 ] || echo 'error: invalid signature' >&2\nexit %d\n", argsFile, code, code)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // G306: an executable test fixture
		t.Fatal(err)
	}
	return bin, argsFile
}

// releasesServer serves one OpenTofu release: SHA256SUMS listing listed, the cosign signature
// files, the GPG signature by signer (when set), and the zip body served.
func releasesServer(t *testing.T, version string, listed, served []byte, signer *openpgp.Entity) *httptest.Server {
	t.Helper()
	zipName := "tofu_" + version + "_linux_amd64.zip"
	sums := []byte(digest(listed) + "  " + zipName + "\n" + digest([]byte("other")) + "  tofu_" + version + "_darwin_arm64.zip\n")
	files := map[string][]byte{
		"/v" + version + "/tofu_" + version + "_SHA256SUMS":     sums,
		"/v" + version + "/tofu_" + version + "_SHA256SUMS.sig": []byte("signature"),
		"/v" + version + "/tofu_" + version + "_SHA256SUMS.pem": []byte("certificate"),
		"/v" + version + "/" + zipName:                          served,
	}
	if signer != nil {
		var sig bytes.Buffer
		if err := openpgp.DetachSign(&sig, signer, bytes.NewReader(sums), nil); err != nil {
			t.Fatal(err)
		}
		files["/v"+version+"/tofu_"+version+"_SHA256SUMS.gpgsig"] = sig.Bytes()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func tofuEntry(version string) runnercontract.CacheEntry {
	return runnercontract.CacheEntry{Kind: runnercontract.CacheKindTofu, Name: "tofu", Version: version, Platform: "linux_amd64"}
}

func TestTofuVerifier_PromotesASignedMatchingRelease(t *testing.T) {
	zip := []byte("tofu 1.10.6 zip")
	srv := releasesServer(t, "1.10.6", zip, zip, nil)
	cosign, argsFile := fakeCosign(t, false)
	v := &TofuVerifier{ReleasesURL: srv.URL, Cosign: cosign, VerifyTransparencyLog: true}
	a, err := v.Verify(t.Context(), tofuEntry("1.10.6"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a.SHA256 != digest(zip) || a.Verification != Verified {
		t.Errorf("artifact = %+v", a)
	}
	args, _ := os.ReadFile(argsFile) //nolint:gosec // G304: the fixture's own file
	for _, want := range []string{
		"verify-blob",
		"https://github.com/opentofu/opentofu/.github/workflows/release.yml@refs/heads/v1.10",
		"https://token.actions.githubusercontent.com",
	} {
		if !strings.Contains(string(args), want+"\n") {
			t.Errorf("cosign was not run with %q:\n%s", want, args)
		}
	}
	if strings.Contains(string(args), "insecure-ignore-tlog") {
		t.Error("the transparency log check was skipped with verification on")
	}
}

func TestTofuVerifier_DropsATamperedZipOrABadSignature(t *testing.T) {
	cosign, _ := fakeCosign(t, false)
	srv := releasesServer(t, "1.10.6", []byte("the published zip"), []byte("a substituted zip"), nil)
	v := &TofuVerifier{ReleasesURL: srv.URL, Cosign: cosign, VerifyTransparencyLog: true}
	if _, err := v.Verify(t.Context(), tofuEntry("1.10.6"), t.TempDir()); !errors.Is(err, ErrPoisoned) {
		t.Errorf("a zip that does not match SHA256SUMS: %v, want ErrPoisoned", err)
	}

	bad, _ := fakeCosign(t, true)
	good := releasesServer(t, "1.10.6", []byte("zip"), []byte("zip"), nil)
	v = &TofuVerifier{ReleasesURL: good.URL, Cosign: bad, VerifyTransparencyLog: true}
	if _, err := v.Verify(t.Context(), tofuEntry("1.10.6"), t.TempDir()); !errors.Is(err, ErrPoisoned) {
		t.Errorf("a SHA256SUMS whose signature does not verify: %v, want ErrPoisoned", err)
	}
	if _, err := v.Verify(t.Context(), tofuEntry("../1.10.6"), t.TempDir()); err == nil {
		t.Error("a version that is not one was fetched")
	}
}

// providerRegistry serves the registry protocol for one provider package, signed by signer, whose
// zip body is served.
type providerRegistry struct {
	srv    *httptest.Server
	listed []byte
}

func armoredPublic(t *testing.T, e *openpgp.Entity) string {
	t.Helper()
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Serialize(w); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	return buf.String()
}

func newProviderRegistry(t *testing.T, listedKey, signer *openpgp.Entity, listed, served []byte) *providerRegistry {
	t.Helper()
	const file = "terraform-provider-null_3.2.4_linux_amd64.zip"
	sums := []byte(digest(listed) + "  " + file + "\n")
	var sig bytes.Buffer
	if err := openpgp.DetachSign(&sig, signer, bytes.NewReader(sums), nil); err != nil {
		t.Fatal(err)
	}
	p := &providerRegistry{listed: listed}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/terraform.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"providers.v1":"/v1/providers/"}`))
	})
	mux.HandleFunc("/v1/providers/hashicorp/null/3.2.4/download/linux/amd64", func(w http.ResponseWriter, _ *http.Request) {
		doc := map[string]any{
			"filename": file, "download_url": "/dl/" + file, "shasums_url": "/dl/SHA256SUMS", "shasums_signature_url": "/dl/SHA256SUMS.sig",
			"shasum":       digest(listed),
			"signing_keys": map[string]any{"gpg_public_keys": []map[string]string{{"key_id": "k", "ascii_armor": armoredPublic(t, listedKey)}}},
		}
		_ = json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("/dl/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(sums) })
	mux.HandleFunc("/dl/SHA256SUMS.sig", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(sig.Bytes()) })
	mux.HandleFunc("/dl/"+file, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(served) })
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func newKey(t *testing.T, name string) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity(name, "", name+"@example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func providerEntry(host string, hashes ...string) runnercontract.CacheEntry {
	return runnercontract.CacheEntry{Kind: runnercontract.CacheKindProvider, Name: host + "/hashicorp/null", Version: "3.2.4", Platform: "linux_amd64", Hashes: hashes}
}

func TestProviderVerifier_PromotesOnlySignedLockedPackages(t *testing.T) {
	key := newKey(t, "publisher")
	zip := []byte("null provider zip")
	reg := newProviderRegistry(t, key, key, zip, zip)
	host := strings.TrimPrefix(reg.srv.URL, "http://")
	v := &ProviderVerifier{Scheme: "http"}

	a, err := v.Verify(t.Context(), providerEntry(host, "h1:irrelevant=", "zh:"+digest(zip)), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a.SHA256 != digest(zip) {
		t.Errorf("artifact sha = %s", a.SHA256)
	}

	if _, err := v.Verify(t.Context(), providerEntry(host, "zh:"+digest([]byte("another version"))), t.TempDir()); !errors.Is(err, ErrPoisoned) {
		t.Errorf("a package the lock file does not pin: %v, want ErrPoisoned", err)
	}
	if _, err := v.Verify(t.Context(), providerEntry(host), t.TempDir()); err == nil {
		t.Error("a package with no lock file hash was promoted")
	}

	tampered := newProviderRegistry(t, key, key, zip, []byte("a substituted zip"))
	if _, err := v.Verify(t.Context(), providerEntry(strings.TrimPrefix(tampered.srv.URL, "http://"), "zh:"+digest(zip)), t.TempDir()); !errors.Is(err, ErrPoisoned) {
		t.Errorf("a zip that does not match the shasum: %v, want ErrPoisoned", err)
	}
	forged := newProviderRegistry(t, key, newKey(t, "attacker"), zip, zip)
	if _, err := v.Verify(t.Context(), providerEntry(strings.TrimPrefix(forged.srv.URL, "http://"), "zh:"+digest(zip)), t.TempDir()); !errors.Is(err, ErrPoisoned) {
		t.Errorf("shasums signed by a key the registry does not list: %v, want ErrPoisoned", err)
	}
}

func TestProviderVerifier_NetworkMirror(t *testing.T) {
	zip := []byte("mirrored null provider")
	served := zip
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/registry.opentofu.org/hashicorp/null/3.2.4.json":
			_, _ = fmt.Fprintf(w, `{"archives":{"linux_amd64":{"url":"null_3.2.4_linux_amd64.zip","hashes":["zh:%s"]}}}`, digest(zip))
		case "/registry.opentofu.org/hashicorp/null/null_3.2.4_linux_amd64.zip":
			_, _ = w.Write(served)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	v := &ProviderVerifier{NetworkMirror: srv.URL}
	if _, err := v.Verify(t.Context(), providerEntry("registry.opentofu.org", "zh:"+digest(zip)), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	served = []byte("tampered in the mirror")
	if _, err := v.Verify(t.Context(), providerEntry("registry.opentofu.org", "zh:"+digest(zip)), t.TempDir()); !errors.Is(err, ErrPoisoned) {
		t.Errorf("a mirrored zip that does not match: %v, want ErrPoisoned", err)
	}
}

func galaxyServer(t *testing.T, published, served []byte) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/collections/ansible/posix/versions/2.2.2/":
			_, _ = fmt.Fprintf(w, `{"download_url":"%s/dl/ansible-posix-2.2.2.tar.gz","artifact":{"filename":"ansible-posix-2.2.2.tar.gz","sha256":"%s"}}`, srv.URL, digest(published))
		case "/dl/ansible-posix-2.2.2.tar.gz":
			_, _ = w.Write(served)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCollectionVerifier_ChecksArtifactSHA256(t *testing.T) {
	tarball := []byte("ansible.posix 2.2.2")
	e := runnercontract.CacheEntry{Kind: runnercontract.CacheKindCollection, Name: "ansible.posix", Version: "2.2.2"}
	good := galaxyServer(t, tarball, tarball)
	a, err := (&CollectionVerifier{Servers: []GalaxyServer{{URL: good.URL + "/api/"}}}).Verify(t.Context(), e, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a.SHA256 != digest(tarball) {
		t.Errorf("artifact = %+v", a)
	}
	bad := galaxyServer(t, tarball, []byte("a tampered tarball"))
	if _, err := (&CollectionVerifier{Servers: []GalaxyServer{{URL: bad.URL + "/api/"}}}).Verify(t.Context(), e, t.TempDir()); !errors.Is(err, ErrPoisoned) {
		t.Errorf("a tarball that does not match artifact.sha256: %v, want ErrPoisoned", err)
	}
}

type fakeGit struct {
	commit  string
	archive []byte
}

func (g fakeGit) TagCommit(context.Context, string, string) (string, error) { return g.commit, nil }

func (g fakeGit) Archive(_ context.Context, _, commit, dst string) error {
	if commit != g.commit {
		return fmt.Errorf("no commit %s", commit)
	}
	return os.WriteFile(dst, g.archive, 0o600)
}

func TestRoleVerifier_KeysTheArchiveByTheTagsCommit(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	archive := []byte("role archive at the commit")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/roles/":
			_, _ = w.Write([]byte(`{"results":[{"github_user":"geerlingguy","github_repo":"ansible-role-docker"}]}`))
		case "/archive/geerlingguy/ansible-role-docker/" + commit + ".tar.gz":
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	v := &RoleVerifier{
		Servers:    []GalaxyServer{{URL: srv.URL + "/api/"}},
		Git:        fakeGit{commit: commit},
		ArchiveURL: func(u, r, c string) string { return srv.URL + "/archive/" + u + "/" + r + "/" + c + ".tar.gz" },
	}
	a, err := v.Verify(t.Context(), runnercontract.CacheEntry{Kind: runnercontract.CacheKindRole, Name: "geerlingguy.docker", Version: "7.4.1"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a.SHA256 != digest(archive) || a.Version != "7.4.1@"+commit {
		t.Errorf("artifact = %+v", a)
	}
}

func TestGitVerifier_OnlyAnExactCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // G204: the test's own git fixture
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	run("config", "uploadpack.allowAnySHA1InWant", "true")
	_ = os.WriteFile(filepath.Join(repo, "galaxy.yml"), []byte("name: c\n"), 0o600)
	run("add", ".")
	run("commit", "-q", "-m", "one")
	commit := run("rev-parse", "HEAD")

	v := &GitVerifier{}
	e := runnercontract.CacheEntry{Kind: runnercontract.CacheKindGit, Name: "file://" + repo, Version: commit}
	a, err := v.Verify(t.Context(), e, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a.SHA256 == "" || !strings.HasSuffix(a.SourceURL, "@"+commit) {
		t.Errorf("artifact = %+v", a)
	}
	again, err := v.Verify(t.Context(), e, t.TempDir())
	if err != nil || again.SHA256 != a.SHA256 {
		t.Errorf("the same commit archived to %s then %s (%v): the key must be reproducible", a.SHA256, again.SHA256, err)
	}
	for _, version := range []string{"main", "HEAD", commit[:12], strings.Repeat("0", 40)} {
		e.Version = version
		if _, err := v.Verify(t.Context(), e, t.TempDir()); err == nil {
			t.Errorf("a git source at %q was promoted", version)
		}
	}
}

// fakePip writes a pip stand-in that downloads body, recording that it ran with --require-hashes.
func fakePip(t *testing.T, body []byte) (bin, ran string) {
	t.Helper()
	dir := t.TempDir()
	ran = filepath.Join(dir, "ran")
	src := filepath.Join(dir, "pkg.whl")
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(dir, "pip")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %q
out=""; req=""
while [ $# -gt 0 ]; do case "$1" in -d) out="$2"; shift;; -r) req="$2"; shift;; esac; shift; done
sum=$(sha256sum %q | cut -d' ' -f1)
grep -q "sha256:$sum" "$req" || { echo "THESE PACKAGES DO NOT MATCH THE HASHES" >&2; exit 1; }
mkdir -p "$out" && cp %q "$out/requests-2.32.3-py3-none-any.whl"
`, ran, src, src)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // G306: an executable test fixture
		t.Fatal(err)
	}
	return bin, ran
}

func TestPipVerifier_RequireHashes(t *testing.T) {
	wheel := []byte("requests wheel")
	pip, ran := fakePip(t, wheel)
	v := &PipVerifier{Pip: pip}
	e := runnercontract.CacheEntry{Kind: runnercontract.CacheKindPip, Name: "requests", Version: "2.32.3", Hashes: []string{"sha256:" + digest(wheel)}}
	a, err := v.Verify(t.Context(), e, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if a.SHA256 != digest(wheel) {
		t.Errorf("artifact = %+v", a)
	}
	args, _ := os.ReadFile(ran) //nolint:gosec // G304: the fixture's own file
	if !strings.Contains(string(args), "--require-hashes\n") {
		t.Errorf("pip ran without --require-hashes:\n%s", args)
	}

	e.Hashes = []string{"sha256:" + digest([]byte("a different wheel"))}
	if _, err := v.Verify(t.Context(), e, t.TempDir()); !errors.Is(err, ErrPoisoned) {
		t.Errorf("a wheel the pins do not match: %v, want ErrPoisoned", err)
	}

	_ = os.Remove(ran)
	e.Hashes = nil
	if _, err := v.Verify(t.Context(), e, t.TempDir()); err == nil {
		t.Error("an unhashed requirement was promoted")
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("pip ran for an unhashed requirement")
	}
}

type fakeVerifier struct {
	a   *Artifact
	err error
}

func (f fakeVerifier) Verify(context.Context, runnercontract.CacheEntry, string) (*Artifact, error) {
	return f.a, f.err
}

type recordingStore struct {
	uploaded []runnercontract.CachePromotion
	dropped  []string
	poisoned []bool
}

func (s *recordingStore) UploadCacheEntry(_ context.Context, p runnercontract.CachePromotion, _ *Artifact) error {
	s.uploaded = append(s.uploaded, p)
	return nil
}

func (s *recordingStore) DropCachePromotion(_ context.Context, _ runnercontract.CachePromotion, reason string, poisoned bool) error {
	s.dropped = append(s.dropped, reason)
	s.poisoned = append(s.poisoned, poisoned)
	return nil
}

func TestPopulator_PromotesOnlyWhatVerifiesAndMatchesTheReport(t *testing.T) {
	sum := digest([]byte("x"))
	pr := func(reported string) runnercontract.CachePromotion {
		return runnercontract.CachePromotion{ID: "p", Entry: runnercontract.CacheEntry{Kind: runnercontract.CacheKindTofu, Version: "1.10.6", SHA256: reported}}
	}
	cases := []struct {
		name     string
		verifier Verifier
		reported string
		uploaded bool
		poisoned bool
	}{
		{"verified and as reported", fakeVerifier{a: &Artifact{SHA256: sum}}, sum, true, false},
		{"verified, nothing reported", fakeVerifier{a: &Artifact{SHA256: sum}}, "", true, false},
		{"the source differs from the report", fakeVerifier{a: &Artifact{SHA256: sum}}, digest([]byte("y")), false, true},
		{"a tampered source", fakeVerifier{err: fmt.Errorf("%w: bad", ErrPoisoned)}, sum, false, true},
		{"a source that could not be reached", fakeVerifier{err: errors.New("connection refused")}, sum, false, false},
		{"no verifier for the kind", nil, sum, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &recordingStore{}
			p := &Populator{Store: store, Verifiers: map[runnercontract.CacheKind]Verifier{}}
			if tc.verifier != nil {
				p.Verifiers[runnercontract.CacheKindTofu] = tc.verifier
			}
			if err := p.Promote(t.Context(), pr(tc.reported)); err != nil {
				t.Fatal(err)
			}
			if got := len(store.uploaded) == 1; got != tc.uploaded {
				t.Errorf("uploaded = %v, want %v", got, tc.uploaded)
			}
			if !tc.uploaded && (len(store.dropped) != 1 || store.poisoned[0] != tc.poisoned) {
				t.Errorf("dropped %v poisoned %v, want one drop with poisoned=%v", store.dropped, store.poisoned, tc.poisoned)
			}
		})
	}
}
