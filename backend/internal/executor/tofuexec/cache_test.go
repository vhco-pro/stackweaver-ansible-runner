// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package tofuexec

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/michielvha/stackweaver/backend/internal/executor"
	"github.com/michielvha/stackweaver/core/runnercontract"
	"github.com/michielvha/stackweaver/core/tofu"
)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func platform() string { return runtime.GOOS + "_" + runtime.GOARCH }

// tofuZip is a release zip holding the stand-in engine.
func tofuZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.CreateHeader(&zip.FileHeader{Name: "tofu", Method: zip.Deflate})
	_, _ = w.Write([]byte(stubTofu))
	_ = zw.Close()
	return buf.Bytes()
}

// configWithLock is a configuration whose lock file pins the given providers.
func configWithLock(t *testing.T, lock string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"main.tf": `resource "null_resource" "a" {}` + "\n", ".terraform.lock.hcl": lock} {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// cacheHarness is the tofu harness with a blob route for cache entries.
func cacheHarness(t *testing.T, runID string, blobs map[string][]byte) (*tofuHarness, *int) {
	t.Helper()
	h := newTofuHarness(t, runID)
	served := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/runner/jobs/"+runID+"/blob/cache/", func(w http.ResponseWriter, r *http.Request) {
		b, ok := blobs[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		served++
		_, _ = w.Write(b)
	})
	mux.Handle("/", h.api)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h.client = executor.NewClient(srv.URL, "placeholder-job-token", "runner-1")
	return h, &served
}

func entry(runID string, kind runnercontract.CacheKind, name, version string, body []byte) runnercontract.CacheEntryRef {
	return runnercontract.CacheEntryRef{
		CacheEntry: runnercontract.CacheEntry{Kind: kind, Name: name, Version: version, Platform: platform(), SHA256: sum(body)},
		Path:       "/api/v2/runner/jobs/" + runID + "/blob/cache/" + string(kind) + "/" + sum(body),
	}
}

// The run's OpenTofu release comes from the organization's cache, verified, and nothing is
// downloaded from the release source (#933, D7, AC33).
func TestExecute_InstallsTheReleaseFromTheCache(t *testing.T) {
	const version = "0.0.1-cachetest"
	release := tofuZip(t)
	e := entry("run-cache-tofu", runnercontract.CacheKindTofu, "tofu", version, release)
	h, served := cacheHarness(t, "run-cache-tofu", map[string][]byte{e.Path: release})
	h.api.artifacts.TofuVersion = version
	h.api.artifacts.Cache = &runnercontract.CacheDescriptor{OrganizationID: "org", Entries: []runnercontract.CacheEntryRef{e}}
	// A release source that answers nothing proves no download happened.
	h.api.artifacts.Mirror = &runnercontract.Mirror{TofuReleasesURL: "http://127.0.0.1:1"}

	if res := h.execute(t, "plan-only"); res.Status != "completed" {
		t.Fatalf("plan-only = %+v", res)
	}
	if *served != 1 {
		t.Errorf("the blob route served %d entries, want the release once", *served)
	}
	if h.api.completed.CacheManifest != nil && len(h.api.completed.CacheManifest.Entries) > 0 {
		t.Errorf("a release taken from the cache was reported: %+v", h.api.completed.CacheManifest)
	}
}

// Locked providers the descriptor names are placed in a packed filesystem mirror the CLI
// configuration routes only those providers to; every other provider installs as before.
func TestExecute_PlacesCachedProvidersInAFilesystemMirror(t *testing.T) {
	nullZip := []byte("terraform-provider-null zip")
	lock := fmt.Sprintf(`provider "registry.opentofu.org/hashicorp/null" {
  version = "3.2.4"
  hashes  = ["zh:%s"]
}

provider "registry.opentofu.org/hashicorp/random" {
  version = "3.6.3"
  hashes  = ["h1:abc=", "zh:%s"]
}
`, sum(nullZip), strings.Repeat("b", 64))
	e := entry("run-cache-prov", runnercontract.CacheKindProvider, "registry.opentofu.org/hashicorp/null", "3.2.4", nullZip)
	h, _ := cacheHarness(t, "run-cache-prov", map[string][]byte{e.Path: nullZip})
	h.api.artifacts.ConfigTarball = configWithLock(t, lock)
	h.api.artifacts.Cache = &runnercontract.CacheDescriptor{OrganizationID: "org", Entries: []runnercontract.CacheEntryRef{e}}

	if res := h.execute(t, "plan-only"); res.Status != "completed" {
		t.Fatalf("plan-only = %+v", res)
	}
	rc := h.read(t, "tofurc")
	if !strings.Contains(rc, "filesystem_mirror") || !strings.Contains(rc, `"registry.opentofu.org/hashicorp/null"`) || !strings.Contains(rc, "direct") {
		t.Errorf("CLI configuration = %q, want a filesystem mirror for null and direct for the rest", rc)
	}
	osName, arch, _ := strings.Cut(platform(), "_")
	want := "./registry.opentofu.org/hashicorp/null/terraform-provider-null_3.2.4_" + osName + "_" + arch + ".zip"
	if got := strings.TrimSpace(h.read(t, "fsmirror")); got != want {
		t.Errorf("filesystem mirror holds %q, want %q", got, want)
	}

	// random was installed from its source: reported with the lock file's hashes for the
	// populator, which fetches and verifies it itself. null came from the cache: not reported.
	m := h.api.completed.CacheManifest
	if m == nil || len(m.Entries) != 1 {
		t.Fatalf("manifest = %+v, want random alone", m)
	}
	got := m.Entries[0]
	if got.Kind != runnercontract.CacheKindProvider || got.Name != "registry.opentofu.org/hashicorp/random" || got.Version != "3.6.3" || got.Platform != platform() || !slices.Contains(got.Hashes, "zh:"+strings.Repeat("b", 64)) {
		t.Errorf("manifest entry = %+v", got)
	}
}

// A release downloaded because the cache lacked it is reported with the digest its signed
// SHA256SUMS lists, for the populator to verify and promote.
func TestExecute_ReportsADownloadedRelease(t *testing.T) {
	const version = "0.0.2-cachetest"
	release := tofuZip(t)
	osName, arch, _ := strings.Cut(platform(), "_")
	zipName := "tofu_" + version + "_" + osName + "_" + arch + ".zip"
	rel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "_SHA256SUMS"):
			_, _ = fmt.Fprintf(w, "%s  %s\n", sum(release), zipName)
		case strings.HasSuffix(r.URL.Path, zipName):
			_, _ = w.Write(release)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(rel.Close)
	h, _ := cacheHarness(t, "run-cache-dl", nil)
	h.api.artifacts.TofuVersion = version
	h.api.artifacts.Mirror = &runnercontract.Mirror{TofuReleasesURL: rel.URL}
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".tofu-versions"), 0o750); err != nil { //nolint:gosec // G703: the test's own HOME
		t.Fatal(err)
	}

	if res := h.execute(t, "plan-only"); res.Status != "completed" {
		t.Fatalf("plan-only = %+v", res)
	}
	m := h.api.completed.CacheManifest
	if m == nil || len(m.Entries) != 1 {
		t.Fatalf("manifest = %+v, want the downloaded release", m)
	}
	if e := m.Entries[0]; e.Kind != runnercontract.CacheKindTofu || e.Version != version || e.Platform != platform() || e.SHA256 != sum(release) {
		t.Errorf("manifest entry = %+v", e)
	}
}

// A failed run reports nothing: only a job that succeeded offers what it installed.
func TestExecute_FailedRunReportsNothing(t *testing.T) {
	h, _ := cacheHarness(t, "run-cache-fail", nil)
	h.api.artifacts.ConfigTarball = configWithLock(t, `provider "registry.opentofu.org/hashicorp/random" {
  version = "3.6.3"
  hashes  = ["zh:`+strings.Repeat("c", 64)+`"]
}
`)
	res := h.execute(t, "apply")
	if res.Status != "failed" {
		t.Fatalf("apply without a plan = %+v, want failed", res)
	}
	if m := h.api.completed.CacheManifest; m != nil {
		t.Errorf("a failed run reported %+v", m)
	}
}

// The run's log says what the organization's cache supplied, so a user (and the cache-reuse E2E)
// can see a second run fetched nothing (#933, D7, AC33).
func TestExecute_LogsWhatTheCacheSupplied(t *testing.T) {
	const version = "0.0.3-cachetest"
	release := tofuZip(t)
	nullZip := []byte("terraform-provider-null zip, logged")
	lock := fmt.Sprintf("provider \"registry.opentofu.org/hashicorp/null\" {\n  version = \"3.2.4\"\n  hashes  = [\"zh:%s\"]\n}\n", sum(nullZip))
	tofuEntry := entry("run-cache-log", runnercontract.CacheKindTofu, "tofu", version, release)
	provEntry := entry("run-cache-log", runnercontract.CacheKindProvider, "registry.opentofu.org/hashicorp/null", "3.2.4", nullZip)
	h, _ := cacheHarness(t, "run-cache-log", map[string][]byte{tofuEntry.Path: release, provEntry.Path: nullZip})
	h.api.artifacts.TofuVersion = version
	h.api.artifacts.ConfigTarball = configWithLock(t, lock)
	h.api.artifacts.Mirror = &runnercontract.Mirror{TofuReleasesURL: "http://127.0.0.1:1"}
	h.api.artifacts.Cache = &runnercontract.CacheDescriptor{OrganizationID: "org", Entries: []runnercontract.CacheEntryRef{tofuEntry, provEntry}}

	if res := h.execute(t, "plan-only"); res.Status != "completed" {
		t.Fatalf("plan-only = %+v", res)
	}
	var log strings.Builder
	for _, o := range h.api.outputs {
		log.WriteString(o.Output)
	}
	want := "From the organization's cache: OpenTofu " + version + ", provider registry.opentofu.org/hashicorp/null 3.2.4"
	if !strings.Contains(log.String(), want) {
		t.Errorf("run log = %q, want a line %q", log.String(), want)
	}

	// From the launcher's node-local copy, the line says no entry crossed the API.
	local := t.TempDir()
	for kind, body := range map[runnercontract.CacheKind][]byte{runnercontract.CacheKindTofu: release, runnercontract.CacheKindProvider: nullZip} {
		_ = os.MkdirAll(filepath.Join(local, string(kind)), 0o750)
		if err := os.WriteFile(filepath.Join(local, string(kind), sum(body)), body, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	h3, served := cacheHarness(t, "run-cache-local", map[string][]byte{})
	h3.api.artifacts.TofuVersion = "0.0.4-cachetest"
	tofuLocal := tofuEntry
	tofuLocal.Version = "0.0.4-cachetest"
	h3.api.artifacts.ConfigTarball = configWithLock(t, lock)
	h3.api.artifacts.Mirror = &runnercontract.Mirror{TofuReleasesURL: "http://127.0.0.1:1"}
	h3.api.artifacts.Cache = &runnercontract.CacheDescriptor{OrganizationID: "org", Entries: []runnercontract.CacheEntryRef{tofuLocal, provEntry}}
	h3.passStubEnv(t)
	if res := Execute(t.Context(), h3.client, Job{ID: h3.api.artifacts.JobID, RunType: "plan-only"}, Options{Console: io.Discard, CacheDir: local}); res.Status != "completed" {
		t.Fatalf("plan-only from the node-local copy = %+v", res)
	}
	var localLog strings.Builder
	for _, o := range h3.api.outputs {
		localLog.WriteString(o.Output)
	}
	if !strings.Contains(localLog.String(), "From the organization's cache (node-local copy): OpenTofu 0.0.4-cachetest") || *served != 0 {
		t.Errorf("run log = %q with %d blob fetches, want the node-local line and none", localLog.String(), *served)
	}

	// A run the cache supplied nothing says nothing about it.
	h2 := newTofuHarness(t, "run-cache-none")
	if res := h2.execute(t, "plan-only"); res.Status != "completed" {
		t.Fatalf("plan-only = %+v", res)
	}
	for _, o := range h2.api.outputs {
		if strings.Contains(o.Output, "organization's cache") {
			t.Errorf("a run without cache entries logged %q", o.Output)
		}
	}
}

// The apply phase installs the providers the plan phase selected: it restores the dependency lock
// file the saved plan recorded before init, so a provider promoted into the cache between the two
// phases cannot change the lock file under the plan ("Inconsistent dependency lock file").
func TestExecute_ApplyRestoresThePlansLockFile(t *testing.T) {
	const planLock = "provider \"registry.opentofu.org/hashicorp/null\" {\n  version = \"3.2.4\"\n  hashes  = [\"h1:plan=\", \"zh:plan\"]\n}\n"
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{"tfplan": "plan body", ".terraform.lock.hcl": planLock} {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(body))
	}
	_ = zw.Close()

	h := newTofuHarness(t, "run-plan-lock")
	h.api.plan = buf.Bytes()
	h.api.artifacts.Plan = &runnercontract.PlanDescriptor{Path: "/api/v2/runner/jobs/run-plan-lock/plan", SizeBytes: int64(buf.Len())}
	h.api.artifacts.ConfigTarball = configWithLock(t, "provider \"registry.opentofu.org/hashicorp/null\" {\n  version = \"3.2.4\"\n  hashes  = [\"zh:config\"]\n}\n")
	if res := h.execute(t, "apply"); res.Status != "completed" {
		t.Fatalf("apply = %+v", res)
	}
	if got := h.read(t, "init_lock"); got != planLock {
		t.Errorf("the lock file init saw = %q, want the saved plan's", got)
	}

	// A saved plan that is no zip (or holds no lock file) leaves the configuration's lock file.
	h2 := newTofuHarness(t, "run-plan-nolock")
	h2.api.plan = []byte("saved-plan-for-run-plan-nolock\n")
	h2.api.artifacts.Plan = &runnercontract.PlanDescriptor{Path: "/api/v2/runner/jobs/run-plan-nolock/plan", SizeBytes: int64(len(h2.api.plan))}
	h2.api.artifacts.ConfigTarball = configWithLock(t, "config lock\n")
	if res := h2.execute(t, "apply"); res.Status != "completed" {
		t.Fatalf("apply = %+v", res)
	}
	if got := h2.read(t, "init_lock"); got != "config lock\n" {
		t.Errorf("the lock file init saw = %q, want the configuration's", got)
	}
}

// A releases mirror behind the install's own CA: the executor's download of OpenTofu trusts the
// mirror block's CA bundle, as the engine and the job's tools do (#933, D2, AC76).
func TestExecute_DownloadsTheReleaseFromAMirrorBehindItsCA(t *testing.T) {
	const version = "0.0.5-cachetest"
	release := tofuZip(t)
	osName, arch, _ := strings.Cut(platform(), "_")
	zipName := "tofu_" + version + "_" + osName + "_" + arch + ".zip"
	rel := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "_SHA256SUMS"):
			_, _ = fmt.Fprintf(w, "%s  %s\n", sum(release), zipName)
		case strings.HasSuffix(r.URL.Path, zipName):
			_, _ = w.Write(release)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(rel.Close)
	t.Cleanup(func() { _ = tofu.SetExtraRootCAs(nil) })
	h, _ := cacheHarness(t, "run-cache-ca", nil)
	h.api.artifacts.TofuVersion = version
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rel.Certificate().Raw})
	h.api.artifacts.Mirror = &runnercontract.Mirror{TofuReleasesURL: rel.URL, CACertificates: string(ca)}
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".tofu-versions"), 0o750); err != nil { //nolint:gosec // G703: the test's own HOME
		t.Fatal(err)
	}
	if res := h.execute(t, "plan-only"); res.Status != "completed" {
		t.Fatalf("plan-only through a mirror behind its CA = %+v", res)
	}
}
