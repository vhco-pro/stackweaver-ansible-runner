// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansibleexec

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/backend/internal/executor"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// recordingSink captures every job event the shared helpers emit, in order.
type recordingSink struct {
	events []Event
	err    error
}

func (s *recordingSink) Emit(_ context.Context, ev Event) error {
	s.events = append(s.events, ev)
	return s.err
}

func (s *recordingSink) names() []string {
	names := make([]string, 0, len(s.events))
	for _, ev := range s.events {
		names = append(names, ev.Event)
	}
	return names
}

// galaxyStub is a fake ansible-galaxy placed first on PATH. Every requirements install appends
// "<kind> <requirements file> <install dir>" to calls.log and the listing of the install dir as it
// was BEFORE the install to seen.<kind>, then drops a marker into the install dir the way a real
// install would. It also appends the ANSIBLE_CONFIG it was run with to config.<kind> and its
// ANSIBLE_GALAXY_SERVER_LIST to server_list.<kind>. An install from the organization's cache
// (collection tarballs, or the cached-roles.yml list) appends "<kind> <files> <install dir>" to
// cached.log instead and lays the content out as ansible-galaxy would. A file named "fail" in the
// stub dir makes the collection requirements step exit non-zero, "fail_cached" the cached step;
// "installs" lists "namespace name version [git]" lines the requirements step installs with a
// MANIFEST.json and, unless marked git, the source metadata ansible-galaxy writes for a collection
// from a Galaxy server, and "roles" lists "name version" lines it installs with their install info.
type galaxyStub struct {
	dir string
}

const galaxyStubScript = `#!/bin/sh
dir="$(dirname "$0")"
kind="$1"; shift; shift
req=""; dest=""; files=""
while [ $# -gt 0 ]; do
  case "$1" in
    -r) req="$2"; shift ;;
    -p) dest="$2"; shift ;;
    --*) ;;
    *) files="$files $1" ;;
  esac
  shift
done
if [ -n "$files" ] || [ "$(basename "$req")" = "cached-roles.yml" ]; then
  echo "$kind${files:- $req} $dest" >> "$dir/cached.log"
  if [ -f "$dir/fail_cached" ]; then echo "ERROR! cached install failed"; exit 1; fi
  for f in $files; do mkdir -p "$dest/cached_$(basename "$f" .tar.gz)"; done
  [ -n "$req" ] && mkdir -p "$dest/cached_roles"
  exit 0
fi
echo "$kind $req $dest" >> "$dir/calls.log"
ls "$dest" 2>/dev/null | tr '\n' ' ' >> "$dir/seen.$kind"
echo >> "$dir/seen.$kind"
echo "${ANSIBLE_CONFIG:-}" >> "$dir/config.$kind"
echo "${ANSIBLE_GALAXY_SERVER_LIST:-}" >> "$dir/server_list.$kind"
cp "$req" "$dir/req.$kind" 2>/dev/null
if [ -f "$dir/hang" ]; then
  exec sleep 30
fi
if [ "$kind" = "collection" ] && [ -f "$dir/fail" ]; then
  echo "ERROR! stub collection install failed"
  exit 1
fi
if [ "$kind" = "collection" ] && [ -f "$dir/installs" ]; then
  while read -r ns name version src; do
    mkdir -p "$dest/ansible_collections/$ns/$name"
    printf '{"collection_info":{"namespace":"%s","name":"%s","version":"%s"}}' "$ns" "$name" "$version" > "$dest/ansible_collections/$ns/$name/MANIFEST.json"
    if [ "$src" != "git" ]; then
      mkdir -p "$dest/ansible_collections/$ns.$name-$version.info"
      echo "server: https://galaxy.ansible.com/api/" > "$dest/ansible_collections/$ns.$name-$version.info/GALAXY.yml"
    fi
  done < "$dir/installs"
fi
if [ "$kind" = "role" ] && [ -f "$dir/roles" ]; then
  while read -r name version; do
    mkdir -p "$dest/$name/meta"
    printf 'install_date: today\nversion: %s\n' "$version" > "$dest/$name/meta/.galaxy_install_info"
  done < "$dir/roles"
fi
mkdir -p "$dest/stub_$kind"
echo "stub $kind install ok"
`

func newGalaxyStub(t *testing.T) *galaxyStub {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ansible-galaxy"), []byte(galaxyStubScript), 0o700); err != nil { //nolint:gosec // G306: the stub must be executable
		t.Fatalf("writing ansible-galaxy stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &galaxyStub{dir: dir}
}

func (s *galaxyStub) failCollections(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.dir, "fail"), nil, 0o600); err != nil {
		t.Fatalf("arming stub failure: %v", err)
	}
}

func (s *galaxyStub) lines(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.dir, name)) //nolint:gosec // G304: reads a file the stub wrote into the test's own temp dir
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading stub file %s: %v", name, err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func writeRequirements(t *testing.T, playbookDir string, rel ...string) {
	t.Helper()
	for _, r := range rel {
		path := filepath.Join(playbookDir, r)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte("---\ncollections: []\n"), 0o600); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
}

func TestInstallGalaxyRequirements_SearchOrder(t *testing.T) {
	tests := []struct {
		name    string
		present []string
		want    string
	}{
		{"root wins over collections and roles", []string{"requirements.yml", "collections/requirements.yml", "roles/requirements.yml"}, "requirements.yml"},
		{"collections wins over roles", []string{"collections/requirements.yml", "roles/requirements.yml"}, "collections/requirements.yml"},
		{"roles alone", []string{"roles/requirements.yml"}, "roles/requirements.yml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := newGalaxyStub(t)
			playbookDir := t.TempDir()
			writeRequirements(t, playbookDir, tt.present...)
			in := GalaxyInstall{JobDir: t.TempDir(), PlaybookDir: playbookDir}

			if _, _, err := InstallGalaxyRequirements(t.Context(), &recordingSink{}, in); err != nil {
				t.Fatalf("InstallGalaxyRequirements() error = %v", err)
			}

			calls := stub.lines(t, "calls.log")
			if len(calls) != 2 {
				t.Fatalf("ansible-galaxy invocations = %v, want one collection and one role install", calls)
			}
			wantReq := filepath.Join(playbookDir, tt.want)
			wantCalls := []string{
				"collection " + wantReq + " " + filepath.Join(in.JobDir, "galaxy", "collections"),
				"role " + wantReq + " " + filepath.Join(in.JobDir, "galaxy", "roles"),
			}
			for i, want := range wantCalls {
				if calls[i] != want {
					t.Errorf("invocation %d = %q, want %q", i, calls[i], want)
				}
			}
		})
	}
}

// A repository without a requirements file stages nothing, and the helper says so with
// empty directories. This test asserted the project's warm cache paths before: with one
// search path rule for both runners, nothing reads the warm cache for a job that declared
// no requirements, so the helper reports what it staged and nothing else. Empty is also
// how both runners tell that there is nothing to put on the search path.
func TestInstallGalaxyRequirements_NoFile(t *testing.T) {
	stub := newGalaxyStub(t)
	sink := &recordingSink{}
	in := GalaxyInstall{JobDir: t.TempDir(), PlaybookDir: t.TempDir()}

	collections, roles, err := InstallGalaxyRequirements(t.Context(), sink, in)
	if err != nil {
		t.Fatalf("InstallGalaxyRequirements() error = %v", err)
	}
	if collections != "" {
		t.Errorf("collections dir = %q, want empty: nothing was staged", collections)
	}
	if roles != "" {
		t.Errorf("roles dir = %q, want empty: nothing was staged", roles)
	}
	if len(sink.events) != 0 {
		t.Errorf("events = %v, want none without a requirements file", sink.names())
	}
	if calls := stub.lines(t, "calls.log"); calls != nil {
		t.Errorf("ansible-galaxy invoked %v, want no invocation", calls)
	}
}

func TestInstallGalaxyRequirements_Success(t *testing.T) {
	newGalaxyStub(t)
	sink := &recordingSink{}
	playbookDir := t.TempDir()
	writeRequirements(t, playbookDir, "requirements.yml")
	in := GalaxyInstall{JobDir: t.TempDir(), PlaybookDir: playbookDir}

	collections, roles, err := InstallGalaxyRequirements(t.Context(), sink, in)
	if err != nil {
		t.Fatalf("InstallGalaxyRequirements() error = %v", err)
	}
	if want := filepath.Join(in.JobDir, "galaxy", "collections"); collections != want {
		t.Errorf("collections dir = %q, want the job staging dir %q", collections, want)
	}
	if want := filepath.Join(in.JobDir, "galaxy", "roles"); roles != want {
		t.Errorf("roles dir = %q, want the job staging dir %q", roles, want)
	}

	// The event rows are the user-visible contract: both runner paths must emit exactly
	// these values, so they are asserted verbatim.
	want := []Event{
		{Event: "galaxy_install", Task: "Installing Galaxy Requirements", Stdout: "Installing collections/roles from requirements.yml\n"},
		{Event: "galaxy_install_complete", Task: "Galaxy Requirements Installed", Stdout: "stub collection install ok\n\nstub role install ok\n"},
	}
	if len(sink.events) != len(want) {
		t.Fatalf("events = %v, want %d events", sink.names(), len(want))
	}
	for i := range want {
		if sink.events[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, sink.events[i], want[i])
		}
	}

	// Nothing is published anywhere: a job never writes a cache another job reads (#933, D7). The
	// test asserted the project's warm cache before; that cache was writable by every job of the
	// project, which D7 replaces with the organization's verified, read-only cache.
	if entries, _ := os.ReadDir(playbookDir); len(entries) != 1 {
		t.Errorf("the install wrote into the playbook repository: %d entries", len(entries))
	}
}

func TestInstallGalaxyRequirements_CollectionInstallFails(t *testing.T) {
	stub := newGalaxyStub(t)
	stub.failCollections(t)
	sink := &recordingSink{}
	playbookDir := t.TempDir()
	writeRequirements(t, playbookDir, "requirements.yml")
	in := GalaxyInstall{JobDir: t.TempDir(), PlaybookDir: playbookDir, Cache: &GalaxyCache{}}

	_, _, err := InstallGalaxyRequirements(t.Context(), sink, in)
	if err == nil {
		t.Fatal("InstallGalaxyRequirements() error = nil, want the collection install failure")
	}
	if len(in.Cache.Manifest) != 0 {
		t.Errorf("a failed install reported %+v", in.Cache.Manifest)
	}

	if got, want := sink.names(), []string{"galaxy_install", "galaxy_install_failed"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	failed := sink.events[1]
	wantFailed := Event{
		Event:  "galaxy_install_failed",
		Task:   "Galaxy Installation Failed",
		Stderr: "ERROR! stub collection install failed\n",
		Failed: true,
	}
	if failed != wantFailed {
		t.Errorf("failure event = %+v, want %+v", failed, wantFailed)
	}

	if calls := stub.lines(t, "calls.log"); len(calls) != 1 {
		t.Errorf("ansible-galaxy invocations = %v, want the collection step only", calls)
	}
}

// Each job's install starts from an empty staging directory: no job seeds another, because a job
// writes no cache another job reads (#933, D7). This test asserted the opposite before, the second
// install seeded from the first one's publish into the project's warm cache; D7 replaced that
// writable cache with the organization's verified, read-only one, whose seeding the tests below
// assert.
func TestInstallGalaxyRequirements_SecondInstallStartsEmpty(t *testing.T) {
	stub := newGalaxyStub(t)
	playbookDir := t.TempDir()
	writeRequirements(t, playbookDir, "requirements.yml")

	for range 2 {
		in := GalaxyInstall{JobDir: t.TempDir(), PlaybookDir: playbookDir}
		if _, _, err := InstallGalaxyRequirements(t.Context(), &recordingSink{}, in); err != nil {
			t.Fatalf("InstallGalaxyRequirements() error = %v", err)
		}
	}
	var collectionCalls int
	for _, c := range stub.lines(t, "calls.log") {
		if strings.HasPrefix(c, "collection ") {
			collectionCalls++
		}
	}
	if collectionCalls != 2 {
		t.Fatalf("collection installs = %d, want 2", collectionCalls)
	}
	for _, seen := range stub.lines(t, "seen.collection") {
		if strings.TrimSpace(seen) != "" {
			t.Errorf("a collection install started with %q in staging, want an empty dir", seen)
		}
	}
}

func TestInstallGalaxyRequirements_SinkErrorDoesNotFailInstall(t *testing.T) {
	newGalaxyStub(t)
	sink := &recordingSink{err: errors.New("event store unavailable")}
	playbookDir := t.TempDir()
	writeRequirements(t, playbookDir, "requirements.yml")
	in := GalaxyInstall{JobDir: t.TempDir(), PlaybookDir: playbookDir}

	if _, _, err := InstallGalaxyRequirements(t.Context(), sink, in); err != nil {
		t.Fatalf("InstallGalaxyRequirements() error = %v, want an event failure to be logged and ignored", err)
	}
	if len(sink.events) != 2 {
		t.Errorf("events attempted = %v, want both install events", sink.names())
	}
}

// cacheReader serves entries from a blob route and counts what it served.
func cacheReader(t *testing.T, blobs map[string][]byte) (*executor.Cache, *int) {
	t.Helper()
	served := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := blobs[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		served++
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return &executor.Cache{Client: executor.NewClient(srv.URL, "job-token", "runner")}, &served
}

func cachedEntry(kind runnercontract.CacheKind, name, version string, body []byte) runnercontract.CacheEntryRef {
	sum := sha256.Sum256(body)
	hexSum := hex.EncodeToString(sum[:])
	return runnercontract.CacheEntryRef{
		CacheEntry: runnercontract.CacheEntry{Kind: kind, Name: name, Version: version, SHA256: hexSum},
		Path:       "/api/v2/runner/jobs/job-1/blob/cache/" + string(kind) + "/" + hexSum,
	}
}

// What the organization's verified cache holds is installed from it first, offline and without
// dependency resolution, and the job output says so before the requirements install the rest
// (#933, D7, AC33).
func TestInstallGalaxyRequirements_InstallsFromTheVerifiedCacheFirst(t *testing.T) {
	stub := newGalaxyStub(t)
	playbookDir := t.TempDir()
	writeFile(t, playbookDir, "requirements.yml", "collections:\n  - name: ansible.posix\n    version: 2.2.2\nroles:\n  - name: geerlingguy.pip\n    version: 3.0.3\n")
	posix := cachedEntry(runnercontract.CacheKindCollection, "ansible.posix", "2.2.2", []byte("posix tarball"))
	role := cachedEntry(runnercontract.CacheKindRole, "geerlingguy.pip", "3.0.3@0123456789abcdef0123456789abcdef01234567", []byte("pip role archive"))
	reader, served := cacheReader(t, map[string][]byte{posix.Path: []byte("posix tarball"), role.Path: []byte("pip role archive")})
	in := GalaxyInstall{JobDir: t.TempDir(), PlaybookDir: playbookDir, Cache: &GalaxyCache{Reader: reader, Entries: []runnercontract.CacheEntryRef{posix, role}}}
	sink := &recordingSink{}

	if _, _, err := InstallGalaxyRequirements(t.Context(), sink, in); err != nil {
		t.Fatal(err)
	}
	if *served != 2 {
		t.Errorf("served %d cache entries, want the collection and the role", *served)
	}
	cached := stub.lines(t, "cached.log")
	if len(cached) != 2 || !strings.HasPrefix(cached[0], "collection ") || !strings.Contains(cached[0], "ansible-posix-2.2.2.tar.gz") || !strings.HasPrefix(cached[1], "role ") {
		t.Errorf("cached installs = %q", cached)
	}
	if got := strings.Join(sink.names(), ","); got != "galaxy_cache_seeded,galaxy_install,galaxy_install_complete" {
		t.Fatalf("events = %s, want the cache install announced first", got)
	}
	want := Event{Event: "galaxy_cache_seeded", Task: "Galaxy cache reused", Stdout: "Installed 1 collection, 1 role from the organization's verified cache\n"}
	if sink.events[0] != want {
		t.Errorf("seeded event = %+v, want %+v", sink.events[0], want)
	}
}

// An entry that does not verify is never installed: the requirements install it from its source.
func TestInstallGalaxyRequirements_AnAlteredEntryIsLeftToItsSource(t *testing.T) {
	stub := newGalaxyStub(t)
	playbookDir := t.TempDir()
	writeRequirements(t, playbookDir, "requirements.yml")
	posix := cachedEntry(runnercontract.CacheKindCollection, "ansible.posix", "2.2.2", []byte("the verified tarball"))
	reader, _ := cacheReader(t, map[string][]byte{posix.Path: []byte("a substituted tarball")})
	sink := &recordingSink{}
	in := GalaxyInstall{JobDir: t.TempDir(), PlaybookDir: playbookDir, Cache: &GalaxyCache{Reader: reader, Entries: []runnercontract.CacheEntryRef{posix}}}
	if _, _, err := InstallGalaxyRequirements(t.Context(), sink, in); err != nil {
		t.Fatal(err)
	}
	if cached := stub.lines(t, "cached.log"); cached != nil {
		t.Errorf("an entry that does not verify was installed: %q", cached)
	}
	if slices.Contains(sink.names(), "galaxy_cache_seeded") {
		t.Errorf("events = %v, want no seeded event when nothing came from the cache", sink.names())
	}
}

// What the requirements installed beyond the cache is reported for the populator: Galaxy
// collections and roles by name and version, and a git source pinned to a full commit; what came
// from the cache is not reported again.
func TestInstallGalaxyRequirements_ReportsWhatItInstalledBeyondTheCache(t *testing.T) {
	stub := newGalaxyStub(t)
	writeFile(t, stub.dir, "installs", "ansible posix 2.2.2\ncommunity general 9.4.0\n")
	writeFile(t, stub.dir, "roles", "geerlingguy.docker 7.4.1\n")
	playbookDir := t.TempDir()
	const commit = "0123456789abcdef0123456789abcdef01234567"
	writeFile(t, playbookDir, "requirements.yml", "collections:\n  - name: ansible.posix\n    version: 2.2.2\n  - community.general\n  - name: https://github.com/example/c.git\n    type: git\n    version: "+commit+"\nroles:\n  - geerlingguy.docker\n")
	posix := cachedEntry(runnercontract.CacheKindCollection, "ansible.posix", "2.2.2", []byte("posix"))
	reader, _ := cacheReader(t, map[string][]byte{posix.Path: []byte("posix")})
	in := GalaxyInstall{JobDir: t.TempDir(), PlaybookDir: playbookDir, Cache: &GalaxyCache{Reader: reader, Entries: []runnercontract.CacheEntryRef{posix}}}
	if _, _, err := InstallGalaxyRequirements(t.Context(), &recordingSink{}, in); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range in.Cache.Manifest {
		got[string(e.Kind)+" "+e.Name+" "+e.Version] = true
	}
	want := map[string]bool{
		"collection community.general 9.4.0":             true,
		"role geerlingguy.docker 7.4.1":                  true,
		"git https://github.com/example/c.git " + commit: true,
	}
	if len(got) != len(want) {
		t.Errorf("manifest = %v, want %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Errorf("manifest lacks %q: %v", k, got)
		}
	}
}

// A git source pinned to a commit the cache holds installs from its verified archive: the
// requirements the install runs point it at the archive's directory.
func TestInstallGalaxyRequirements_CachedGitSourceInstallsFromItsArchive(t *testing.T) {
	stub := newGalaxyStub(t)
	playbookDir := t.TempDir()
	const commit = "0123456789abcdef0123456789abcdef01234567"
	writeFile(t, playbookDir, "requirements.yml", "collections:\n  - name: https://github.com/example/c.git\n    type: git\n    version: "+commit+"\n")
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "source/galaxy.yml", Mode: 0o644, Size: 8, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("name: c\n"))
	_ = tw.Close()
	_ = gz.Close()
	git := cachedEntry(runnercontract.CacheKindGit, "https://github.com/example/c.git", commit, archive.Bytes())
	reader, _ := cacheReader(t, map[string][]byte{git.Path: archive.Bytes()})
	in := GalaxyInstall{JobDir: t.TempDir(), PlaybookDir: playbookDir, Cache: &GalaxyCache{Reader: reader, Entries: []runnercontract.CacheEntryRef{git}}}
	if _, _, err := InstallGalaxyRequirements(t.Context(), &recordingSink{}, in); err != nil {
		t.Fatal(err)
	}
	req := strings.Join(stub.lines(t, "req.collection"), "\n")
	if !strings.Contains(req, "type: dir") || strings.Contains(req, "github.com") || !strings.Contains(req, filepath.Join(in.JobDir, "galaxy", "cached")) {
		t.Errorf("the requirements installed were %q, want the git source pointed at its cached archive", req)
	}
	if len(in.Cache.Manifest) != 0 {
		t.Errorf("a git source from the cache was reported: %+v", in.Cache.Manifest)
	}
}

// hang makes every ansible-galaxy invocation block, the way a git requirement on an
// unreachable host does.
func (s *galaxyStub) hang(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.dir, "hang"), nil, 0o600); err != nil {
		t.Fatalf("arming stub hang: %v", err)
	}
}

// shortGalaxyDeadline shrinks the install deadline for one test.
func shortGalaxyDeadline(t *testing.T, d time.Duration) {
	t.Helper()
	old := galaxyInstallTimeout
	galaxyInstallTimeout = d
	t.Cleanup(func() { galaxyInstallTimeout = old })
}

// A requirement that never finishes must not hold a runner slot for ever: the install
// is stopped at its deadline, reported as failed with the reason, and the job goes on.
func TestInstallGalaxyRequirements_HungInstallIsStoppedAtTheDeadline(t *testing.T) {
	stub := newGalaxyStub(t)
	stub.hang(t)
	shortGalaxyDeadline(t, 300*time.Millisecond)
	sink := &recordingSink{}
	playbookDir := t.TempDir()
	writeRequirements(t, playbookDir, "requirements.yml")
	in := GalaxyInstall{JobDir: t.TempDir(), PlaybookDir: playbookDir}

	start := time.Now()
	_, _, err := InstallGalaxyRequirements(t.Context(), sink, in)
	if err == nil {
		t.Fatal("InstallGalaxyRequirements() error = nil, want the deadline failure")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("the hung install held on for %s, want it stopped at the 300ms deadline", elapsed)
	}
	if got := strings.Join(sink.names(), ","); got != "galaxy_install,galaxy_install_failed" {
		t.Fatalf("events = %s, want galaxy_install,galaxy_install_failed", got)
	}
	if failed := sink.events[1]; !failed.Failed || !strings.Contains(failed.Stderr, "did not finish within") {
		t.Errorf("failure event = %+v, want it marked failed and naming the deadline", failed)
	}
}

func TestInstallGalaxyRequirements_CancellationStopsTheInstall(t *testing.T) {
	stub := newGalaxyStub(t)
	stub.hang(t)
	sink := &recordingSink{}
	playbookDir := t.TempDir()
	writeRequirements(t, playbookDir, "requirements.yml")
	in := GalaxyInstall{JobDir: t.TempDir(), PlaybookDir: playbookDir}

	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	_, _, err := InstallGalaxyRequirements(ctx, sink, in)
	if err == nil {
		t.Fatal("InstallGalaxyRequirements() error = nil, want the cancellation")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("the cancelled install held on for %s", elapsed)
	}
	if len(sink.events) != 2 || !strings.Contains(sink.events[1].Stderr, "cancelled") {
		t.Errorf("events = %+v, want a failure event saying the job was cancelled", sink.events)
	}
}

// The extra environment reaches both ansible-galaxy steps; without any, they inherit the
// process environment untouched, which is the platform runner's behaviour.
func TestInstallGalaxyRequirements_ExtraEnvReachesAnsibleGalaxy(t *testing.T) {
	tests := []struct {
		name string
		env  []string
		want string
	}{
		{"job ansible.cfg passed by the agent", []string{"ANSIBLE_CONFIG=/tmp/job/ansible.cfg"}, "/tmp/job/ansible.cfg"},
		{"no extra environment", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ANSIBLE_CONFIG", "")
			stub := newGalaxyStub(t)
			playbookDir := t.TempDir()
			writeRequirements(t, playbookDir, "requirements.yml")
			in := GalaxyInstall{JobDir: t.TempDir(), PlaybookDir: playbookDir, Env: tt.env}

			if _, _, err := InstallGalaxyRequirements(t.Context(), &recordingSink{}, in); err != nil {
				t.Fatalf("InstallGalaxyRequirements() error = %v", err)
			}
			for _, kind := range []string{"collection", "role"} {
				if got := stub.lines(t, "config."+kind); len(got) != 1 || got[0] != tt.want {
					t.Errorf("ansible-galaxy %s install saw ANSIBLE_CONFIG = %q, want %q", kind, got, tt.want)
				}
			}
		})
	}
}

// A cached install that fails is discarded, so the requirements install everything from their
// sources into an empty staging directory and the job output does not claim reuse. This test
// asserted the same of a half-copied warm cache before; the warm cache is gone (#933, D7), and a
// failed install from the verified cache is the case that remains.
func TestInstallGalaxyRequirements_FailedCachedInstallStartsFromAnEmptyStagingDir(t *testing.T) {
	stub := newGalaxyStub(t)
	writeFile(t, stub.dir, "fail_cached", "")
	playbookDir := t.TempDir()
	writeRequirements(t, playbookDir, "requirements.yml")
	posix := cachedEntry(runnercontract.CacheKindCollection, "ansible.posix", "2.2.2", []byte("posix"))
	reader, _ := cacheReader(t, map[string][]byte{posix.Path: []byte("posix")})
	sink := &recordingSink{}
	in := GalaxyInstall{JobDir: t.TempDir(), PlaybookDir: playbookDir, Cache: &GalaxyCache{Reader: reader, Entries: []runnercontract.CacheEntryRef{posix}}}
	if _, _, err := InstallGalaxyRequirements(t.Context(), sink, in); err != nil {
		t.Fatalf("InstallGalaxyRequirements() error = %v", err)
	}
	seen := stub.lines(t, "seen.collection")
	if len(seen) != 1 || strings.TrimSpace(seen[0]) != "" {
		t.Errorf("the install started with %q in staging, want an empty dir after the failed cached install", seen)
	}
	if slices.Contains(sink.names(), "galaxy_cache_seeded") {
		t.Errorf("events = %v, want no galaxy_cache_seeded after a discarded install", sink.names())
	}
}

// gitSourceArchive is a cached git source's archive: the repository at a commit under source/.
func gitSourceArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: "source/" + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return archive.Bytes()
}

// resolvesTo is a ref resolver that answers every ref of repo with commit and records what it was
// asked.
func resolvesTo(repo, commit string, asked *[]string) func(context.Context, string, string) (string, error) {
	return func(_ context.Context, r, ref string) (string, error) {
		*asked = append(*asked, r+" "+ref)
		if r != repo {
			return "", errors.New("unknown repository")
		}
		return commit, nil
	}
}

// A git source on a branch is keyed by the commit the branch resolves to when the job runs (#933,
// D7): the job reports the repository, without the subdirectory it installs from, at that commit,
// so the populator clones exactly what was installed.
func TestInstallGalaxyRequirements_ABranchSourceIsReportedAtItsResolvedCommit(t *testing.T) {
	newGalaxyStub(t)
	playbookDir := t.TempDir()
	const commit = "89abcdef0123456789abcdef0123456789abcdef"
	writeFile(t, playbookDir, "requirements.yml", "collections:\n  - name: https://github.com/example/c.git#/collections/c/\n    type: git\n    version: galaxy\n")
	var asked []string
	in := GalaxyInstall{JobDir: t.TempDir(), PlaybookDir: playbookDir, Cache: &GalaxyCache{}, ResolveRef: resolvesTo("https://github.com/example/c.git", commit, &asked)}
	if _, _, err := InstallGalaxyRequirements(t.Context(), &recordingSink{}, in); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(asked, []string{"https://github.com/example/c.git galaxy"}) {
		t.Errorf("resolved %q, want the branch of the repository", asked)
	}
	want := []runnercontract.CacheEntry{{Kind: runnercontract.CacheKindGit, Name: "https://github.com/example/c.git", Version: commit}}
	if !manifestIs(in.Cache.Manifest, want) {
		t.Errorf("manifest = %+v, want %+v", in.Cache.Manifest, want)
	}
}

// A git source on a branch whose current commit the cache holds installs from the verified archive,
// from the subdirectory the requirement names, and the job output says the cache supplied it.
func TestInstallGalaxyRequirements_ABranchSourceInstallsFromTheCachedCommit(t *testing.T) {
	stub := newGalaxyStub(t)
	playbookDir := t.TempDir()
	const commit = "89abcdef0123456789abcdef0123456789abcdef"
	writeFile(t, playbookDir, "requirements.yml", "collections:\n  - name: git+https://github.com/example/c.git#/collections/c/\n    type: git\n    version: galaxy\n")
	archive := gitSourceArchive(t, map[string]string{"collections/c/galaxy.yml": "name: c\n"})
	git := cachedEntry(runnercontract.CacheKindGit, "https://github.com/example/c.git", commit, archive)
	reader, _ := cacheReader(t, map[string][]byte{git.Path: archive})
	var asked []string
	sink := &recordingSink{}
	in := GalaxyInstall{
		JobDir: t.TempDir(), PlaybookDir: playbookDir,
		Cache:      &GalaxyCache{Reader: reader, Entries: []runnercontract.CacheEntryRef{git}},
		ResolveRef: resolvesTo("https://github.com/example/c.git", commit, &asked),
	}
	if _, _, err := InstallGalaxyRequirements(t.Context(), sink, in); err != nil {
		t.Fatal(err)
	}
	req := strings.Join(stub.lines(t, "req.collection"), "\n")
	wantDir := filepath.Join(in.JobDir, "galaxy", "cached", "git-"+git.SHA256, "source", "collections", "c")
	if !strings.Contains(req, "type: dir") || strings.Contains(req, "github.com") || !strings.Contains(req, wantDir) {
		t.Errorf("the requirements installed were %q, want the source pointed at %s", req, wantDir)
	}
	if len(in.Cache.Manifest) != 0 {
		t.Errorf("a git source from the cache was reported: %+v", in.Cache.Manifest)
	}
	if len(sink.events) == 0 || sink.events[0].Stdout != "Installed 1 git source from the organization's verified cache\n" {
		t.Errorf("events = %+v, want the cached git source announced first", sink.events)
	}
}

// A branch that moved since its commit was cached installs from its source, and the new commit is
// reported for the populator.
func TestInstallGalaxyRequirements_ABranchThatMovedInstallsFromItsSource(t *testing.T) {
	stub := newGalaxyStub(t)
	playbookDir := t.TempDir()
	const cachedCommit = "0123456789abcdef0123456789abcdef01234567"
	const current = "89abcdef0123456789abcdef0123456789abcdef"
	writeFile(t, playbookDir, "requirements.yml", "collections:\n  - name: https://github.com/example/c.git\n    type: git\n    version: main\n")
	archive := gitSourceArchive(t, map[string]string{"galaxy.yml": "name: c\n"})
	git := cachedEntry(runnercontract.CacheKindGit, "https://github.com/example/c.git", cachedCommit, archive)
	reader, _ := cacheReader(t, map[string][]byte{git.Path: archive})
	var asked []string
	in := GalaxyInstall{
		JobDir: t.TempDir(), PlaybookDir: playbookDir,
		Cache:      &GalaxyCache{Reader: reader, Entries: []runnercontract.CacheEntryRef{git}},
		ResolveRef: resolvesTo("https://github.com/example/c.git", current, &asked),
	}
	if _, _, err := InstallGalaxyRequirements(t.Context(), &recordingSink{}, in); err != nil {
		t.Fatal(err)
	}
	if req := strings.Join(stub.lines(t, "req.collection"), "\n"); !strings.Contains(req, "github.com/example/c.git") {
		t.Errorf("the requirements installed were %q, want the source as written", req)
	}
	want := []runnercontract.CacheEntry{{Kind: runnercontract.CacheKindGit, Name: "https://github.com/example/c.git", Version: current}}
	if !manifestIs(in.Cache.Manifest, want) {
		t.Errorf("manifest = %+v, want %+v", in.Cache.Manifest, want)
	}
}

// A ref that cannot be resolved leaves the source to ansible-galaxy and reports nothing for it.
func TestInstallGalaxyRequirements_AnUnresolvedRefIsNotCached(t *testing.T) {
	stub := newGalaxyStub(t)
	playbookDir := t.TempDir()
	writeFile(t, playbookDir, "requirements.yml", "collections:\n  - name: https://github.com/example/c.git\n    type: git\n    version: main\n")
	in := GalaxyInstall{
		JobDir: t.TempDir(), PlaybookDir: playbookDir, Cache: &GalaxyCache{},
		ResolveRef: func(context.Context, string, string) (string, error) { return "", errors.New("unreachable") },
	}
	if _, _, err := InstallGalaxyRequirements(t.Context(), &recordingSink{}, in); err != nil {
		t.Fatal(err)
	}
	if len(stub.lines(t, "calls.log")) == 0 {
		t.Error("the requirements were not installed")
	}
	if len(in.Cache.Manifest) != 0 {
		t.Errorf("an unresolved ref was reported: %+v", in.Cache.Manifest)
	}
}

// A requirement whose subdirectory would leave the archive is never pointed into it.
func TestInstallGalaxyRequirements_ASubdirectoryCannotLeaveTheArchive(t *testing.T) {
	stub := newGalaxyStub(t)
	playbookDir := t.TempDir()
	const commit = "89abcdef0123456789abcdef0123456789abcdef"
	writeFile(t, playbookDir, "requirements.yml", "collections:\n  - name: https://github.com/example/c.git#/../../../etc/\n    type: git\n    version: "+commit+"\n")
	archive := gitSourceArchive(t, map[string]string{"galaxy.yml": "name: c\n"})
	git := cachedEntry(runnercontract.CacheKindGit, "https://github.com/example/c.git", commit, archive)
	reader, _ := cacheReader(t, map[string][]byte{git.Path: archive})
	in := GalaxyInstall{JobDir: t.TempDir(), PlaybookDir: playbookDir, Cache: &GalaxyCache{Reader: reader, Entries: []runnercontract.CacheEntryRef{git}}}
	if _, _, err := InstallGalaxyRequirements(t.Context(), &recordingSink{}, in); err != nil {
		t.Fatal(err)
	}
	if req := strings.Join(stub.lines(t, "req.collection"), "\n"); strings.Contains(req, "type: dir") {
		t.Errorf("the requirements installed were %q, want the escaping subdirectory left to its source", req)
	}
}

// manifestIs compares a manifest by kind, name and version.
func manifestIs(got, want []runnercontract.CacheEntry) bool {
	key := func(e runnercontract.CacheEntry) string { return string(e.Kind) + " " + e.Name + " " + e.Version }
	return slices.EqualFunc(got, want, func(a, b runnercontract.CacheEntry) bool { return key(a) == key(b) })
}

// git ls-remote resolves a branch, an annotated tag to the commit it points at, and no version to
// the default branch.
func TestLsRemoteResolvesBranchesTagsAndTheDefaultBranch(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // G204: the test's own git commands
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "first")
	first := git("rev-parse", "HEAD")
	git("tag", "-a", "v1", "-m", "v1")
	git("checkout", "-q", "-b", "galaxy")
	git("commit", "-q", "--allow-empty", "-m", "second")
	second := git("rev-parse", "HEAD")
	git("checkout", "-q", "main")

	in := GalaxyInstall{}
	for ref, want := range map[string]string{"galaxy": second, "v1": first, "": first} {
		got, err := in.lsRemote(t.Context(), "file://"+repo, ref)
		if err != nil || got != want {
			t.Errorf("lsRemote(%q) = %q, %v, want %q", ref, got, err, want)
		}
	}
	if _, err := in.lsRemote(t.Context(), "file://"+repo, "missing"); err == nil {
		t.Error("a ref the repository lacks resolved")
	}
	if _, err := in.lsRemote(t.Context(), "--upload-pack=touch /tmp/x", ""); err == nil {
		t.Error("an option was taken for a repository")
	}
}

// A collection installed from a git source is not reported as a Galaxy collection: ansible-galaxy
// writes its source metadata (<fqcn>-<version>.info) only for a collection from a Galaxy server,
// and the git source itself is what the populator promotes.
func TestInstallGalaxyRequirements_ACollectionFromAGitSourceIsNotReportedAsAGalaxyCollection(t *testing.T) {
	stub := newGalaxyStub(t)
	writeFile(t, stub.dir, "installs", "stackweaver e2e 1.0.0 git\nansible posix 2.2.2\n")
	playbookDir := t.TempDir()
	const commit = "89abcdef0123456789abcdef0123456789abcdef"
	writeFile(t, playbookDir, "requirements.yml", "collections:\n  - name: https://github.com/example/c.git#/e2e/\n    type: git\n    version: galaxy\n  - ansible.posix\n")
	var asked []string
	in := GalaxyInstall{JobDir: t.TempDir(), PlaybookDir: playbookDir, Cache: &GalaxyCache{}, ResolveRef: resolvesTo("https://github.com/example/c.git", commit, &asked)}
	if _, _, err := InstallGalaxyRequirements(t.Context(), &recordingSink{}, in); err != nil {
		t.Fatal(err)
	}
	want := []runnercontract.CacheEntry{
		{Kind: runnercontract.CacheKindCollection, Name: "ansible.posix", Version: "2.2.2"},
		{Kind: runnercontract.CacheKindGit, Name: "https://github.com/example/c.git", Version: commit},
	}
	if !manifestIs(in.Cache.Manifest, want) {
		t.Errorf("manifest = %+v, want %+v", in.Cache.Manifest, want)
	}
}
