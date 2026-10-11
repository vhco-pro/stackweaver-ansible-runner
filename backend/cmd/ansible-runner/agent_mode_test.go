// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	testRunnerToken = "swr-runner-token" //nolint:gosec // G101: fixture value, not a credential
	testJobID       = "5d1c9f6e-3b7a-4c58-9e21-7a4f0b6d2c13"
)

// newTestAgent returns an agent whose control plane is the given handler. It carries a
// registration key and a runner token so a test can tell which one a call used.
func newTestAgent(t *testing.T, controlPlane http.Handler) *AgentRunner {
	t.Helper()
	srv := httptest.NewServer(controlPlane)
	t.Cleanup(srv.Close)
	return &AgentRunner{
		config:      AgentConfig{ServerURL: srv.URL, Token: "swo-registration-key", PollInterval: time.Second},
		runnerID:    "runner-1",
		runnerToken: testRunnerToken,
		client:      srv.Client(),
		shutdown:    make(chan struct{}),
	}
}

func TestAPIEventSink_PostsEventWithRunnerToken(t *testing.T) {
	type seen struct {
		method, path, auth, contentType string
		body                            map[string]any
	}
	var got []seen
	agent := newTestAgent(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		got = append(got, seen{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), body})
		w.WriteHeader(http.StatusOK)
	}))

	sink := apiEventSink{agent: agent, jobID: testJobID}
	ev := jobEvent{Event: "galaxy_install_failed", Task: "Galaxy Installation Failed", Stdout: "out\n", Stderr: "ERROR! boom\n", Failed: true}
	if err := sink.Emit(t.Context(), ev); err != nil {
		t.Fatalf("Emit() error = %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("control plane saw %d requests, want 1", len(got))
	}
	req := got[0]
	if req.method != http.MethodPost || req.path != "/api/v2/runner/jobs/"+testJobID+"/events" {
		t.Errorf("request = %s %s, want POST /api/v2/runner/jobs/%s/events", req.method, req.path, testJobID)
	}
	if want := "Bearer " + testRunnerToken; req.auth != want {
		t.Errorf("Authorization = %q, want the runner token %q", req.auth, want)
	}
	if req.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", req.contentType)
	}
	wantBody := map[string]any{
		"event":  "galaxy_install_failed",
		"task":   "Galaxy Installation Failed",
		"stdout": "out\n",
		"stderr": "ERROR! boom\n",
		"failed": true,
	}
	if len(req.body) != len(wantBody) {
		t.Errorf("body = %v, want exactly the keys of %v", req.body, wantBody)
	}
	for k, want := range wantBody {
		if req.body[k] != want {
			t.Errorf("body[%q] = %v, want %v", k, req.body[k], want)
		}
	}
}

// The events endpoint caps a request at 1 MiB. Output larger than that must still arrive,
// shortened from the front: the end of ansible-galaxy's output is where the reason is.
func TestAPIEventSink_OversizedOutputKeepsItsTail(t *testing.T) {
	var (
		size int
		body map[string]any
	)
	agent := newTestAgent(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		size = len(raw)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusOK)
	}))

	const reason = "ERROR! the actual reason is on the last line\n"
	big := strings.Repeat("an early line of progress output é\n", 100_000) + reason // about 3.5 MB
	sink := apiEventSink{agent: agent, jobID: testJobID}
	if err := sink.Emit(t.Context(), jobEvent{Event: "galaxy_install_failed", Stdout: big, Stderr: big, Failed: true}); err != nil {
		t.Fatalf("Emit() error = %v", err)
	}

	if size == 0 || size > 1<<20 {
		t.Fatalf("request body is %d bytes, want it to fit the endpoint's 1 MiB cap", size)
	}
	for _, field := range []string{"stdout", "stderr"} {
		got, _ := body[field].(string)
		if !strings.HasSuffix(got, reason) {
			t.Errorf("%s does not end with the tail of the output (ends %q)", field, got[max(0, len(got)-60):])
		}
		if !strings.Contains(got, "of earlier output omitted") {
			t.Errorf("%s carries no marker saying output was omitted (starts %q)", field, got[:min(len(got), 80)])
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s was cut in the middle of a character", field)
		}
	}

	// Output that fits is sent untouched.
	small := "short output\n"
	if err := sink.Emit(t.Context(), jobEvent{Event: "galaxy_install_complete", Stdout: small}); err != nil {
		t.Fatalf("Emit() error = %v", err)
	}
	if got, _ := body["stdout"].(string); got != small {
		t.Errorf("small stdout = %q, want it unchanged", got)
	}
}

func TestAPIEventSink_NonSuccessResponseIsLoggedNotReturned(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			agent := newTestAgent(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "nope", status)
			}))
			sink := apiEventSink{agent: agent, jobID: testJobID}
			if err := sink.Emit(t.Context(), jobEvent{Event: "playbook_source", Stdout: "banner\n"}); err != nil {
				t.Errorf("Emit() error = %v, want a rejected event to be logged and the job to carry on", err)
			}
		})
	}
}

func TestAPIEventSink_UnreachableControlPlaneReturnsError(t *testing.T) {
	agent := newTestAgent(t, http.NotFoundHandler())
	agent.config.ServerURL = "http://127.0.0.1:1" // nothing listens here
	sink := apiEventSink{agent: agent, jobID: testJobID}
	if err := sink.Emit(t.Context(), jobEvent{Event: "playbook_source"}); err == nil {
		t.Error("Emit() error = nil, want the transport failure so the caller can log it")
	}
}

// fakeControlPlane is the slice of the runner control-plane API a job run touches. It
// serves a job's artifacts and playbook snapshot and records the job events posted back.
type fakeControlPlane struct {
	mu sync.Mutex

	artifacts      map[string]any // served as the job's artifacts document
	snapshot       []byte
	snapshotStatus int    // 0 means 200
	jobStatus      string // what the status poll reports; "" means running

	events                []jobEvent
	artifactsCapabilities string // X-Stackweaver-Runner-Capabilities of the last artifacts request
	snapshotRequests      int
	snapshotAuth          string
}

func (cp *fakeControlPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/artifacts"):
		cp.artifactsCapabilities = r.Header.Get("X-Stackweaver-Runner-Capabilities")
		raw, err := json.Marshal(cp.artifacts)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(raw)
	case strings.HasSuffix(r.URL.Path, "/playbook-snapshot"):
		cp.snapshotRequests++
		cp.snapshotAuth = r.Header.Get("Authorization")
		if cp.snapshotStatus != 0 {
			http.Error(w, `{"errors":[{"status":"404","title":"Not Found","detail":"the playbook has no snapshot yet"}]}`, cp.snapshotStatus)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(cp.snapshot)
	case strings.HasSuffix(r.URL.Path, "/events"):
		var body struct {
			Event, Task, Stdout, Stderr string
			Failed                      bool
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		cp.events = append(cp.events, jobEvent{Event: body.Event, Task: body.Task, Stdout: body.Stdout, Stderr: body.Stderr, Failed: body.Failed})
	case strings.HasSuffix(r.URL.Path, "/status"):
		status := cp.jobStatus
		if status == "" {
			status = "running"
		}
		_, _ = w.Write([]byte(`{"status":"` + status + `"}`))
	default: // output lines and anything else the run reports
		w.WriteHeader(http.StatusOK)
	}
}

func (cp *fakeControlPlane) recorded() []jobEvent {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return append([]jobEvent(nil), cp.events...)
}

// jobStubs puts a fake ansible-playbook and git first on PATH. ansible-playbook records
// its working directory, the playbook it was handed (path and content) and its ANSIBLE_*
// environment; git logs its arguments and "clones" by creating a site.yml in the target.
type jobStubs struct {
	dir string
}

const ansiblePlaybookStub = `#!/bin/sh
rec="$(dirname "$0")"
for a in "$@"; do playbook="$a"; done
pwd > "$rec/cwd"
echo "$playbook" > "$rec/playbook_path"
cat "$playbook" > "$rec/playbook_content"
env | grep '^ANSIBLE_' | sort > "$rec/env"
echo "stub play ok"
`

const gitStub = `#!/bin/sh
rec="$(dirname "$0")"
echo "$@" >> "$rec/git.log"
for a in "$@"; do target="$a"; done
mkdir -p "$target"
echo "from clone" > "$target/site.yml"
`

func newJobStubs(t *testing.T) *jobStubs {
	t.Helper()
	dir := t.TempDir()
	for name, script := range map[string]string{"ansible-playbook": ansiblePlaybookStub, "git": gitStub} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil { //nolint:gosec // G306: the stub must be executable
			t.Fatalf("writing %s stub: %v", name, err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &jobStubs{dir: dir}
}

// read returns a file the stubs recorded, or "" when the stub never ran.
func (s *jobStubs) read(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.dir, name)) //nolint:gosec // G304: reads a file the stub wrote into the test's own temp dir
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatalf("reading stub record %s: %v", name, err)
	}
	return strings.TrimRight(string(data), "\n")
}

// testSnapshot builds a playbook snapshot the way the sync worker does.
func testSnapshot(t *testing.T, files map[string]string) []byte {
	t.Helper()
	repo := t.TempDir()
	for rel, content := range files {
		writeFile(t, repo, rel, content)
	}
	data, _, err := buildPlaybookSnapshot(repo, "site.yml")
	if err != nil {
		t.Fatalf("building snapshot: %v", err)
	}
	return data
}

// runJob downloads the job's artifacts from the control plane and runs the playbook,
// which is what executeJob does between claiming a job and reporting its result.
func runJob(t *testing.T, cp *fakeControlPlane) (jobID, status, errMsg string) {
	t.Helper()
	agent := newTestAgent(t, cp)
	jobID = uuid.NewString()
	artifacts, err := agent.downloadJobArtifacts(jobID)
	if err != nil {
		t.Fatalf("downloading artifacts: %v", err)
	}
	status, errMsg = agent.runAnsiblePlaybook(jobID, artifacts)
	return jobID, status, errMsg
}

// The API hands the playbook-source contract only to a runner that announces it can
// download snapshots, so this agent must say so on every artifacts request.
func TestDownloadJobArtifacts_AnnouncesSnapshotCapability(t *testing.T) {
	cp := &fakeControlPlane{artifacts: map[string]any{"playbook_path": "site.yml"}}
	agent := newTestAgent(t, cp)

	if _, err := agent.downloadJobArtifacts(testJobID); err != nil {
		t.Fatalf("downloadJobArtifacts() error = %v", err)
	}

	announced := strings.Split(cp.artifactsCapabilities, ",")
	for i := range announced {
		announced[i] = strings.TrimSpace(announced[i])
	}
	if !slices.Contains(announced, "playbook-snapshot") {
		t.Errorf("X-Stackweaver-Runner-Capabilities = %q, want it to list playbook-snapshot", cp.artifactsCapabilities)
	}
}

func TestRunAnsiblePlaybook_CachedSnapshot(t *testing.T) {
	stubs := newJobStubs(t)
	capturedAt := time.Date(2026, 10, 11, 8, 15, 0, 0, time.UTC)
	const commit = "0123456789abcdef0123456789abcdef01234567"
	cp := &fakeControlPlane{
		// No vcs object: for a cached playbook with a snapshot the API sends none.
		artifacts: map[string]any{
			"job_id":          "ignored",
			"playbook_path":   "site.yml",
			"project_id":      "7c1d5b0a-2f64-4c1e-9d3a-5e8f7a6b4c21",
			"playbook_source": map[string]any{"mode": "cached", "commit": commit, "captured_at": capturedAt, "size_bytes": 123},
		},
		snapshot: testSnapshot(t, map[string]string{"site.yml": "- hosts: all # from snapshot\n"}),
	}

	jobID, status, errMsg := runJob(t, cp)

	if status != "completed" {
		t.Fatalf("status = %q (%s), want completed", status, errMsg)
	}
	repoDir := filepath.Join("/tmp", "ansible-job-"+jobID, "repo")
	if got, want := stubs.read(t, "playbook_path"), filepath.Join(repoDir, "site.yml"); got != want {
		t.Errorf("ansible-playbook ran %q, want the snapshot's playbook at %q", got, want)
	}
	if got := stubs.read(t, "playbook_content"); got != "- hosts: all # from snapshot" {
		t.Errorf("playbook content = %q, want the snapshot's file", got)
	}
	if got := stubs.read(t, "cwd"); got != repoDir {
		t.Errorf("ansible-playbook ran in %q, want %q", got, repoDir)
	}
	if log := stubs.read(t, "git.log"); log != "" {
		t.Errorf("git was invoked (%q), want no git at all for a cached run with a snapshot", log)
	}
	if cp.snapshotAuth != "Bearer "+testRunnerToken {
		t.Errorf("snapshot request Authorization = %q, want the runner token", cp.snapshotAuth)
	}

	events := cp.recorded()
	want := playbookSourceEvent(playbookSourceMessage(commit, &capturedAt))
	if len(events) != 1 || events[0] != want {
		t.Errorf("events = %+v, want exactly %+v", events, want)
	}
}

func TestRunAnsiblePlaybook_CachedWithoutSnapshotClonesAndSaysSo(t *testing.T) {
	stubs := newJobStubs(t)
	cp := &fakeControlPlane{
		artifacts: map[string]any{
			"playbook_path":   "site.yml",
			"vcs":             map[string]any{"repo_url": "https://example.test/acme/playbooks.git", "branch": "release", "repository": "acme/playbooks"},
			"playbook_source": map[string]any{"mode": "cached"},
		},
	}

	jobID, status, errMsg := runJob(t, cp)

	if status != "completed" {
		t.Fatalf("status = %q (%s), want completed", status, errMsg)
	}
	repoDir := filepath.Join("/tmp", "ansible-job-"+jobID, "repo")
	if got, want := stubs.read(t, "git.log"), "clone --depth 1 --single-branch --branch release -- https://example.test/acme/playbooks.git "+repoDir; got != want {
		t.Errorf("git invocations = %q, want %q", got, want)
	}
	if got := stubs.read(t, "playbook_content"); got != "from clone" {
		t.Errorf("playbook content = %q, want the cloned file", got)
	}
	if cp.snapshotRequests != 0 {
		t.Errorf("snapshot requested %d time(s), want none when the API reported no snapshot", cp.snapshotRequests)
	}
	events := cp.recorded()
	want := playbookSourceEvent(playbookSourceMissMessage())
	if len(events) != 1 || events[0] != want {
		t.Errorf("events = %+v, want exactly %+v", events, want)
	}
}

func TestRunAnsiblePlaybook_ClonesWithoutBanner(t *testing.T) {
	vcsInfo := map[string]any{"repo_url": "https://example.test/acme/playbooks.git", "branch": "main", "repository": "acme/playbooks"}
	tests := []struct {
		name      string
		artifacts map[string]any
	}{
		{"fresh source mode", map[string]any{"playbook_path": "site.yml", "vcs": vcsInfo, "playbook_source": map[string]any{"mode": "fresh"}}},
		// An API that predates source modes on agents sends no playbook_source at all.
		{"no playbook_source in the artifacts", map[string]any{"playbook_path": "site.yml", "vcs": vcsInfo}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubs := newJobStubs(t)
			cp := &fakeControlPlane{artifacts: tt.artifacts}

			_, status, errMsg := runJob(t, cp)

			if status != "completed" {
				t.Fatalf("status = %q (%s), want completed", status, errMsg)
			}
			if log := stubs.read(t, "git.log"); !strings.HasPrefix(log, "clone ") {
				t.Errorf("git invocations = %q, want a clone", log)
			}
			if events := cp.recorded(); len(events) != 0 {
				t.Errorf("events = %+v, want no playbook_source event for a run that clones the current HEAD", events)
			}
			if cp.snapshotRequests != 0 {
				t.Errorf("snapshot requested %d time(s), want none", cp.snapshotRequests)
			}
		})
	}
}

func TestRunAnsiblePlaybook_SnapshotFailuresFailTheJob(t *testing.T) {
	capturedAt := time.Now().UTC()
	tests := []struct {
		name       string
		mutate     func(cp *fakeControlPlane)
		wantPrefix string
	}{
		{"snapshot endpoint answers 404", func(cp *fakeControlPlane) { cp.snapshotStatus = http.StatusNotFound }, "Failed to download playbook snapshot: "},
		{"snapshot is not a gzip tarball", func(cp *fakeControlPlane) { cp.snapshot = []byte("definitely not gzip") }, "Failed to extract playbook snapshot: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stubs := newJobStubs(t)
			cp := &fakeControlPlane{
				artifacts: map[string]any{
					"playbook_path":   "site.yml",
					"playbook_source": map[string]any{"mode": "cached", "commit": "abc1234", "captured_at": capturedAt},
				},
			}
			tt.mutate(cp)

			_, status, errMsg := runJob(t, cp)

			if status != "failed" || !strings.HasPrefix(errMsg, tt.wantPrefix) {
				t.Errorf("result = %q / %q, want failed with an error starting %q", status, errMsg, tt.wantPrefix)
			}
			if ran := stubs.read(t, "cwd"); ran != "" {
				t.Errorf("ansible-playbook ran in %q, want it not to run without a playbook", ran)
			}
			if log := stubs.read(t, "git.log"); log != "" {
				t.Errorf("git was invoked (%q), want no fallback clone", log)
			}
			if events := cp.recorded(); len(events) != 0 {
				t.Errorf("events = %+v, want none when the snapshot could not be prepared", events)
			}
		})
	}
}

func TestLoadAgentConfig_GalaxyCacheDir(t *testing.T) {
	tests := []struct {
		name          string
		cacheDir      string
		workspacesDir string
		want          string
	}{
		{"explicit GALAXY_CACHE_DIR", "/var/cache/stackweaver-galaxy", "/data/workspaces", "/var/cache/stackweaver-galaxy"},
		{"defaults under WORKSPACES_DIR", "", "/data/workspaces", "/data/workspaces/galaxy-cache"},
		{"defaults to the image's workspaces dir", "", "", "/home/stackweaver/workspaces/galaxy-cache"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("STACKWEAVER_SERVER", "https://stackweaver.example.test")
			t.Setenv("STACKWEAVER_TOKEN", "swo-registration-key")
			t.Setenv("RUNNER_AGENT_POOL_ID", "pool-1")
			t.Setenv("GALAXY_CACHE_DIR", tt.cacheDir)
			t.Setenv("WORKSPACES_DIR", tt.workspacesDir)

			if got := loadAgentConfig().GalaxyCacheDir; got != tt.want {
				t.Errorf("GalaxyCacheDir = %q, want %q", got, tt.want)
			}
		})
	}
}

// A cached run whose snapshot carries a requirements.yml: the agent installs it into
// the job's staging dirs through the shared helper, publishes it to the per-project
// cache, and hands ansible-playbook the staged and playbook-local dirs in front of
// whatever the host already resolves.
func TestRunAnsiblePlaybook_InstallsGalaxyRequirements(t *testing.T) {
	stubs := newJobStubs(t)
	galaxy := newGalaxyStub(t)
	t.Setenv("ANSIBLE_COLLECTIONS_PATH", "/opt/site/collections")
	t.Setenv("ANSIBLE_ROLES_PATH", "") // empty counts as unset
	t.Setenv("ANSIBLE_CONFIG", "")

	const projectID = "7c1d5b0a-2f64-4c1e-9d3a-5e8f7a6b4c21"
	capturedAt := time.Date(2026, 10, 11, 8, 15, 0, 0, time.UTC)
	cp := &fakeControlPlane{
		artifacts: map[string]any{
			"playbook_path":   "site.yml",
			"project_id":      projectID,
			"ansible_cfg":     "[galaxy]\nserver_list = private_hub\n",
			"playbook_source": map[string]any{"mode": "cached", "commit": "abc1234", "captured_at": capturedAt},
		},
		snapshot: testSnapshot(t, map[string]string{
			"site.yml":         "- hosts: all\n",
			"requirements.yml": "---\ncollections: []\n",
		}),
	}
	agent := newTestAgent(t, cp)
	agent.config.GalaxyCacheDir = t.TempDir()
	jobID := uuid.NewString()
	artifacts, err := agent.downloadJobArtifacts(jobID)
	if err != nil {
		t.Fatalf("downloading artifacts: %v", err)
	}

	status, errMsg := agent.runAnsiblePlaybook(jobID, artifacts)

	if status != "completed" {
		t.Fatalf("status = %q (%s), want completed", status, errMsg)
	}
	var names []string
	for _, ev := range cp.recorded() {
		names = append(names, ev.Event)
	}
	if got, want := strings.Join(names, ","), "playbook_source,galaxy_install,galaxy_install_complete"; got != want {
		t.Errorf("events = %s, want %s", got, want)
	}

	workDir := filepath.Join("/tmp", "ansible-job-"+jobID)
	env := strings.Split(stubs.read(t, "env"), "\n")
	wantCollections := filepath.Join(workDir, "galaxy", "collections") + ":" + filepath.Join(workDir, "repo", "collections") + ":/opt/site/collections"
	if got, _ := envValue(env, "ANSIBLE_COLLECTIONS_PATH"); got != wantCollections {
		t.Errorf("ansible-playbook saw ANSIBLE_COLLECTIONS_PATH = %q, want %q", got, wantCollections)
	}
	wantRoles := filepath.Join(workDir, "galaxy", "roles") + ":" + filepath.Join(workDir, "repo", "roles") + ":~/.ansible/roles:/usr/share/ansible/roles:/etc/ansible/roles"
	if got, _ := envValue(env, "ANSIBLE_ROLES_PATH"); got != wantRoles {
		t.Errorf("ansible-playbook saw ANSIBLE_ROLES_PATH = %q, want %q", got, wantRoles)
	}

	// The install must read the job's ansible.cfg: that is where a private Galaxy
	// server is configured for a host with no route to galaxy.ansible.com.
	if got, want := galaxy.lines(t, "config.collection"), filepath.Join(workDir, "ansible.cfg"); len(got) != 1 || got[0] != want {
		t.Errorf("ansible-galaxy saw ANSIBLE_CONFIG = %q, want %q", got, want)
	}

	published := filepath.Join(agent.config.GalaxyCacheDir, projectID, "collections", "stub_collection")
	if _, statErr := os.Stat(published); statErr != nil {
		t.Errorf("per-project cache entry %s missing after the job: %v", published, statErr)
	}
}

// unsetEnv removes a variable for the duration of one test.
func unsetEnv(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		t.Setenv(name, "placeholder") // registers the restore of the original value
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unsetting %s: %v", name, err)
		}
	}
}

// A repository with no requirements.yml gets no Galaxy staging, so the agent must leave
// the Ansible search paths alone: an environment variable beats ansible.cfg, and setting
// one would silently override a roles_path or collections_path the repository relies on.
func TestRunAnsiblePlaybook_NoGalaxyLeavesAnsiblePathsAlone(t *testing.T) {
	stubs := newJobStubs(t)
	newGalaxyStub(t)
	unsetEnv(t, "ANSIBLE_COLLECTIONS_PATH", "ANSIBLE_ROLES_PATH", "ANSIBLE_CONFIG")
	capturedAt := time.Now().UTC()
	cp := &fakeControlPlane{
		artifacts: map[string]any{
			"playbook_path":   "site.yml",
			"project_id":      "7c1d5b0a-2f64-4c1e-9d3a-5e8f7a6b4c21",
			"playbook_source": map[string]any{"mode": "cached", "commit": "abc1234", "captured_at": capturedAt},
		},
		snapshot: testSnapshot(t, map[string]string{
			"site.yml":    "- hosts: all\n",
			"ansible.cfg": "[defaults]\nroles_path = vendor/roles\n",
		}),
	}

	_, status, errMsg := runJob(t, cp)

	if status != "completed" {
		t.Fatalf("status = %q (%s), want completed", status, errMsg)
	}
	env := strings.Split(stubs.read(t, "env"), "\n")
	for _, name := range []string{"ANSIBLE_ROLES_PATH", "ANSIBLE_COLLECTIONS_PATH"} {
		if value, count := envValue(env, name); count != 0 {
			t.Errorf("ansible-playbook saw %s=%q, want it unset so ansible.cfg stays in charge", name, value)
		}
	}
}

// When Galaxy did stage an install, the staged directories go in front of the paths the
// effective ansible.cfg configures, not in place of them.
func TestRunAnsiblePlaybook_GalaxyKeepsPathsFromAnsibleCfg(t *testing.T) {
	stubs := newJobStubs(t)
	newGalaxyStub(t)
	unsetEnv(t, "ANSIBLE_COLLECTIONS_PATH", "ANSIBLE_ROLES_PATH", "ANSIBLE_CONFIG")
	capturedAt := time.Now().UTC()
	cp := &fakeControlPlane{
		artifacts: map[string]any{
			"playbook_path":   "site.yml",
			"project_id":      "7c1d5b0a-2f64-4c1e-9d3a-5e8f7a6b4c21",
			"playbook_source": map[string]any{"mode": "cached", "commit": "abc1234", "captured_at": capturedAt},
		},
		snapshot: testSnapshot(t, map[string]string{
			"site.yml":         "- hosts: all\n",
			"requirements.yml": "---\ncollections: []\n",
			"ansible.cfg":      "[defaults]\nroles_path = vendor/roles:/opt/shared/roles\ncollections_path = vendor/collections\n",
		}),
	}
	agent := newTestAgent(t, cp)
	agent.config.GalaxyCacheDir = t.TempDir()
	jobID := uuid.NewString()
	artifacts, err := agent.downloadJobArtifacts(jobID)
	if err != nil {
		t.Fatalf("downloading artifacts: %v", err)
	}

	if status, errMsg := agent.runAnsiblePlaybook(jobID, artifacts); status != "completed" {
		t.Fatalf("status = %q (%s), want completed", status, errMsg)
	}

	workDir := filepath.Join("/tmp", "ansible-job-"+jobID)
	repo := filepath.Join(workDir, "repo")
	env := strings.Split(stubs.read(t, "env"), "\n")
	wantRoles := filepath.Join(workDir, "galaxy", "roles") + ":" + filepath.Join(repo, "roles") + ":" + filepath.Join(repo, "vendor", "roles") + ":/opt/shared/roles"
	if got, _ := envValue(env, "ANSIBLE_ROLES_PATH"); got != wantRoles {
		t.Errorf("ANSIBLE_ROLES_PATH = %q, want %q", got, wantRoles)
	}
	wantCollections := filepath.Join(workDir, "galaxy", "collections") + ":" + filepath.Join(repo, "collections") + ":" + filepath.Join(repo, "vendor", "collections")
	if got, _ := envValue(env, "ANSIBLE_COLLECTIONS_PATH"); got != wantCollections {
		t.Errorf("ANSIBLE_COLLECTIONS_PATH = %q, want %q", got, wantCollections)
	}
}

// Cancelling a job must reach the preparation steps too. A Galaxy install that hangs
// would otherwise hold the agent's only slot until it gave up on its own.
func TestRunAnsiblePlaybook_CancelledDuringGalaxyInstall(t *testing.T) {
	stubs := newJobStubs(t)
	newGalaxyStub(t).hang(t)
	old := jobCancelPollInterval
	jobCancelPollInterval = 50 * time.Millisecond
	t.Cleanup(func() { jobCancelPollInterval = old })

	capturedAt := time.Now().UTC()
	cp := &fakeControlPlane{
		jobStatus: "canceled",
		artifacts: map[string]any{
			"playbook_path":   "site.yml",
			"playbook_source": map[string]any{"mode": "cached", "commit": "abc1234", "captured_at": capturedAt},
		},
		snapshot: testSnapshot(t, map[string]string{"site.yml": "- hosts: all\n", "requirements.yml": "---\ncollections: []\n"}),
	}

	start := time.Now()
	_, status, errMsg := runJob(t, cp)

	if status != "canceled" {
		t.Errorf("status = %q (%s), want canceled", status, errMsg)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("the cancelled job held the agent for %s, want the hung install stopped promptly", elapsed)
	}
	if ran := stubs.read(t, "cwd"); ran != "" {
		t.Errorf("ansible-playbook ran in %q, want a cancelled job not to reach the play", ran)
	}
}

// Without a usable project id there is no cache namespace, so the install still runs
// but nothing is published to the shared cache directory.
func TestRunAnsiblePlaybook_GalaxyWithoutProjectIDDoesNotTouchTheSharedCache(t *testing.T) {
	newJobStubs(t)
	newGalaxyStub(t)
	capturedAt := time.Now().UTC()
	for _, projectID := range []string{"", "../../etc"} {
		t.Run("project_id="+projectID, func(t *testing.T) {
			cp := &fakeControlPlane{
				artifacts: map[string]any{
					"playbook_path":   "site.yml",
					"project_id":      projectID,
					"playbook_source": map[string]any{"mode": "cached", "commit": "abc1234", "captured_at": capturedAt},
				},
				snapshot: testSnapshot(t, map[string]string{"site.yml": "- hosts: all\n", "requirements.yml": "---\ncollections: []\n"}),
			}
			agent := newTestAgent(t, cp)
			agent.config.GalaxyCacheDir = t.TempDir()
			jobID := uuid.NewString()
			artifacts, err := agent.downloadJobArtifacts(jobID)
			if err != nil {
				t.Fatalf("downloading artifacts: %v", err)
			}

			if status, errMsg := agent.runAnsiblePlaybook(jobID, artifacts); status != "completed" {
				t.Fatalf("status = %q (%s), want completed", status, errMsg)
			}
			entries, err := os.ReadDir(agent.config.GalaxyCacheDir)
			if err != nil {
				t.Fatalf("reading cache dir: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("shared cache dir holds %d entries, want none without a valid project id", len(entries))
			}
		})
	}
}
