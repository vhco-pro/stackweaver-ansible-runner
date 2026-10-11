// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// recordingSink captures every job event the shared helpers emit, in order.
type recordingSink struct {
	events []jobEvent
	err    error
}

func (s *recordingSink) Emit(_ context.Context, ev jobEvent) error {
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

// galaxyStub is a fake ansible-galaxy placed first on PATH. Every invocation appends
// "<kind> <requirements file> <install dir>" to calls.log and the listing of the install
// dir as it was BEFORE the install to seen.<kind>, then drops a marker into the install
// dir the way a real install would. It also appends the ANSIBLE_CONFIG it was run with
// to config.<kind>. A file named "fail" in the stub dir makes the collection step exit
// non-zero.
type galaxyStub struct {
	dir string
}

const galaxyStubScript = `#!/bin/sh
dir="$(dirname "$0")"
kind="$1"
req="$4"
dest="$6"
echo "$kind $req $dest" >> "$dir/calls.log"
ls "$dest" 2>/dev/null | tr '\n' ' ' >> "$dir/seen.$kind"
echo >> "$dir/seen.$kind"
echo "${ANSIBLE_CONFIG:-}" >> "$dir/config.$kind"
if [ -f "$dir/hang" ]; then
  exec sleep 30
fi
if [ "$kind" = "collection" ] && [ -f "$dir/fail" ]; then
  echo "ERROR! stub collection install failed"
  exit 1
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
			in := galaxyInstallInput{cacheBase: filepath.Join(t.TempDir(), "project"), jobDir: t.TempDir(), playbookDir: playbookDir}

			if _, _, err := installGalaxyRequirements(t.Context(), &recordingSink{}, in); err != nil {
				t.Fatalf("installGalaxyRequirements() error = %v", err)
			}

			calls := stub.lines(t, "calls.log")
			if len(calls) != 2 {
				t.Fatalf("ansible-galaxy invocations = %v, want one collection and one role install", calls)
			}
			wantReq := filepath.Join(playbookDir, tt.want)
			wantCalls := []string{
				"collection " + wantReq + " " + filepath.Join(in.jobDir, "galaxy", "collections"),
				"role " + wantReq + " " + filepath.Join(in.jobDir, "galaxy", "roles"),
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
	cacheBase := filepath.Join(t.TempDir(), "project")
	in := galaxyInstallInput{cacheBase: cacheBase, jobDir: t.TempDir(), playbookDir: t.TempDir()}

	collections, roles, err := installGalaxyRequirements(t.Context(), sink, in)
	if err != nil {
		t.Fatalf("installGalaxyRequirements() error = %v", err)
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
	cacheBase := filepath.Join(t.TempDir(), "galaxy-cache", "project")
	in := galaxyInstallInput{cacheBase: cacheBase, jobDir: t.TempDir(), playbookDir: playbookDir}

	collections, roles, err := installGalaxyRequirements(t.Context(), sink, in)
	if err != nil {
		t.Fatalf("installGalaxyRequirements() error = %v", err)
	}
	if want := filepath.Join(in.jobDir, "galaxy", "collections"); collections != want {
		t.Errorf("collections dir = %q, want the job staging dir %q", collections, want)
	}
	if want := filepath.Join(in.jobDir, "galaxy", "roles"); roles != want {
		t.Errorf("roles dir = %q, want the job staging dir %q", roles, want)
	}

	// The event rows are the user-visible contract: both runner paths must emit exactly
	// these values, so they are asserted verbatim.
	want := []jobEvent{
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

	for _, published := range []string{
		filepath.Join(cacheBase, "collections", "stub_collection"),
		filepath.Join(cacheBase, "roles", "stub_role"),
	} {
		if _, statErr := os.Stat(published); statErr != nil {
			t.Errorf("published cache entry %s missing: %v", published, statErr)
		}
	}
}

func TestInstallGalaxyRequirements_CollectionInstallFails(t *testing.T) {
	stub := newGalaxyStub(t)
	stub.failCollections(t)
	sink := &recordingSink{}
	playbookDir := t.TempDir()
	writeRequirements(t, playbookDir, "requirements.yml")
	cacheBase := filepath.Join(t.TempDir(), "galaxy-cache", "project")
	in := galaxyInstallInput{cacheBase: cacheBase, jobDir: t.TempDir(), playbookDir: playbookDir}

	_, _, err := installGalaxyRequirements(t.Context(), sink, in)
	if err == nil {
		t.Fatal("installGalaxyRequirements() error = nil, want the collection install failure")
	}

	if got, want := sink.names(), []string{"galaxy_install", "galaxy_install_failed"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	failed := sink.events[1]
	wantFailed := jobEvent{
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
	if _, statErr := os.Stat(cacheBase); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("cache base %s exists after a failed install (stat err = %v), want nothing published", cacheBase, statErr)
	}
}

func TestInstallGalaxyRequirements_SecondInstallStartsFromSeededStaging(t *testing.T) {
	stub := newGalaxyStub(t)
	playbookDir := t.TempDir()
	writeRequirements(t, playbookDir, "requirements.yml")
	cacheBase := filepath.Join(t.TempDir(), "galaxy-cache", "project")

	for range 2 {
		in := galaxyInstallInput{cacheBase: cacheBase, jobDir: t.TempDir(), playbookDir: playbookDir}
		if _, _, err := installGalaxyRequirements(t.Context(), &recordingSink{}, in); err != nil {
			t.Fatalf("installGalaxyRequirements() error = %v", err)
		}
	}

	// seen.collection holds what the collections install dir contained when each install
	// began: empty for the cold run, the first run's published content for the warm one.
	seen := stub.lines(t, "seen.collection")
	if len(seen) != 2 {
		t.Fatalf("collection installs seen = %q, want 2", seen)
	}
	if strings.TrimSpace(seen[0]) != "" {
		t.Errorf("first install started with %q in staging, want an empty dir", seen[0])
	}
	if strings.TrimSpace(seen[1]) != "stub_collection" {
		t.Errorf("second install started with %q in staging, want it seeded with stub_collection", seen[1])
	}
}

func TestInstallGalaxyRequirements_SinkErrorDoesNotFailInstall(t *testing.T) {
	newGalaxyStub(t)
	sink := &recordingSink{err: errors.New("event store unavailable")}
	playbookDir := t.TempDir()
	writeRequirements(t, playbookDir, "requirements.yml")
	in := galaxyInstallInput{cacheBase: filepath.Join(t.TempDir(), "project"), jobDir: t.TempDir(), playbookDir: playbookDir}

	if _, _, err := installGalaxyRequirements(t.Context(), sink, in); err != nil {
		t.Fatalf("installGalaxyRequirements() error = %v, want an event failure to be logged and ignored", err)
	}
	if len(sink.events) != 2 {
		t.Errorf("events attempted = %v, want both install events", sink.names())
	}
}

// Cache reuse has to be visible in the job output: ansible-galaxy reinstalls a git-sourced
// collection on every run, so its transcript cannot show that the staging directory came
// from the project's warm cache. The helper says so itself, before the install starts.
func TestInstallGalaxyRequirements_SeededStagingIsAnnounced(t *testing.T) {
	newGalaxyStub(t)
	playbookDir := t.TempDir()
	writeRequirements(t, playbookDir, "requirements.yml")
	cacheBase := filepath.Join(t.TempDir(), "galaxy-cache", "project")
	install := func() *recordingSink {
		t.Helper()
		sink := &recordingSink{}
		in := galaxyInstallInput{cacheBase: cacheBase, jobDir: t.TempDir(), playbookDir: playbookDir}
		if _, _, err := installGalaxyRequirements(t.Context(), sink, in); err != nil {
			t.Fatalf("installGalaxyRequirements() error = %v", err)
		}
		return sink
	}

	// Cold cache: nothing to seed from, so nothing is announced.
	if got := strings.Join(install().names(), ","); got != "galaxy_install,galaxy_install_complete" {
		t.Fatalf("cold run events = %s, want galaxy_install,galaxy_install_complete", got)
	}

	// The first run published collections and roles; the second is seeded from both.
	warm := install()
	if got := strings.Join(warm.names(), ","); got != "galaxy_cache_seeded,galaxy_install,galaxy_install_complete" {
		t.Fatalf("warm run events = %s, want galaxy_cache_seeded first", got)
	}
	want := jobEvent{
		Event:  "galaxy_cache_seeded",
		Task:   "Galaxy cache reused",
		Stdout: "Seeded the Galaxy staging directory from the project's warm cache (collections, roles)\n",
	}
	if warm.events[0] != want {
		t.Errorf("seeded event = %+v, want %+v", warm.events[0], want)
	}

	// With only one kind in the cache, only that kind is named.
	if err := os.RemoveAll(filepath.Join(cacheBase, "roles")); err != nil {
		t.Fatalf("removing the roles cache: %v", err)
	}
	collectionsOnly := install()
	if len(collectionsOnly.events) == 0 || collectionsOnly.events[0].Stdout != "Seeded the Galaxy staging directory from the project's warm cache (collections)\n" {
		t.Errorf("collections-only run events = %+v, want a seeded event naming collections alone", collectionsOnly.events)
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
	in := galaxyInstallInput{cacheBase: filepath.Join(t.TempDir(), "project"), jobDir: t.TempDir(), playbookDir: playbookDir}

	start := time.Now()
	_, _, err := installGalaxyRequirements(t.Context(), sink, in)
	if err == nil {
		t.Fatal("installGalaxyRequirements() error = nil, want the deadline failure")
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
	in := galaxyInstallInput{cacheBase: filepath.Join(t.TempDir(), "project"), jobDir: t.TempDir(), playbookDir: playbookDir}

	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	_, _, err := installGalaxyRequirements(ctx, sink, in)
	if err == nil {
		t.Fatal("installGalaxyRequirements() error = nil, want the cancellation")
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
			in := galaxyInstallInput{cacheBase: filepath.Join(t.TempDir(), "project"), jobDir: t.TempDir(), playbookDir: playbookDir, env: tt.env}

			if _, _, err := installGalaxyRequirements(t.Context(), &recordingSink{}, in); err != nil {
				t.Fatalf("installGalaxyRequirements() error = %v", err)
			}
			for _, kind := range []string{"collection", "role"} {
				if got := stub.lines(t, "config."+kind); len(got) != 1 || got[0] != tt.want {
					t.Errorf("ansible-galaxy %s install saw ANSIBLE_CONFIG = %q, want %q", kind, got, tt.want)
				}
			}
		})
	}
}

func TestPruneGalaxyCache_EvictsOnlyIdleProjectCaches(t *testing.T) {
	// Project caches are named after the project's UUID (the names here used to be
	// arbitrary; they are UUIDs now because the janitor only touches what the installer
	// creates).
	base := t.TempDir()
	idle := filepath.Join(base, "7c1d5b0a-2f64-4c1e-9d3a-5e8f7a6b4c21")
	active := filepath.Join(base, "0b2f6c1e-8a51-4a37-9a0e-6f1d3c2b4a59")
	// Not the janitor's: an operator's own directory, or a leftover of another tool,
	// that happens to live under the cache base and has not been touched in a long time.
	foreign := filepath.Join(base, "site-collections")
	for _, dir := range []string{idle, active, foreign} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, dir := range []string{idle, foreign} {
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatalf("aging %s: %v", dir, err)
		}
	}

	pruneGalaxyCache(base, 24*time.Hour)

	if _, err := os.Stat(idle); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("idle cache still present (stat err = %v), want it evicted", err)
	}
	if _, err := os.Stat(active); err != nil {
		t.Errorf("active cache evicted: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("a stale directory that is not a project cache was deleted: %v", err)
	}
}

// A seed that fails half way leaves a partial tree in the staging directory. Installing
// on top of it and publishing the result would poison the project's cache, so the
// staging directory must start empty instead.
func TestInstallGalaxyRequirements_FailedSeedStartsFromAnEmptyStagingDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable files, so the seed cannot be made to fail")
	}
	stub := newGalaxyStub(t)
	playbookDir := t.TempDir()
	writeRequirements(t, playbookDir, "requirements.yml")
	cacheBase := filepath.Join(t.TempDir(), "galaxy-cache", "project")

	// A warm cache whose second entry cannot be read: the copy gets "a_copied" across
	// and then fails on "b_unreadable".
	cached := filepath.Join(cacheBase, "collections")
	writeFile(t, cached, "a_copied/MANIFEST.json", "{}")
	writeFile(t, cached, "b_unreadable", "secret")
	if err := os.Chmod(filepath.Join(cached, "b_unreadable"), 0o000); err != nil {
		t.Fatalf("making the cache entry unreadable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(cached, "b_unreadable"), 0o600) })

	in := galaxyInstallInput{cacheBase: cacheBase, jobDir: t.TempDir(), playbookDir: playbookDir}
	sink := &recordingSink{}
	if _, _, err := installGalaxyRequirements(t.Context(), sink, in); err != nil {
		t.Fatalf("installGalaxyRequirements() error = %v", err)
	}

	seen := stub.lines(t, "seen.collection")
	if len(seen) != 1 || strings.TrimSpace(seen[0]) != "" {
		t.Errorf("the install started with %q in staging, want an empty dir after the failed seed", seen)
	}
	// A seed that was discarded reused nothing, so the job output must not claim it did.
	if slices.Contains(sink.names(), "galaxy_cache_seeded") {
		t.Errorf("events = %v, want no galaxy_cache_seeded after a discarded seed", sink.names())
	}
}

// The first sweep must be over before the caller goes on to run jobs, so it can never
// remove a cache directory a job is in the middle of seeding from.
func TestStartGalaxyCacheJanitor_FirstSweepIsSynchronous(t *testing.T) {
	t.Setenv("GALAXY_CACHE_TTL_DAYS", "1")
	base := t.TempDir()
	idle := filepath.Join(base, "7c1d5b0a-2f64-4c1e-9d3a-5e8f7a6b4c21")
	if err := os.MkdirAll(idle, 0o750); err != nil {
		t.Fatalf("creating %s: %v", idle, err)
	}
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(idle, old, old); err != nil {
		t.Fatalf("aging %s: %v", idle, err)
	}

	startGalaxyCacheJanitor(t.Context(), base)

	if _, err := os.Stat(idle); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("idle cache still present when startGalaxyCacheJanitor returned (stat err = %v)", err)
	}
}
