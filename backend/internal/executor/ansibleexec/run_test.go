// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansibleexec

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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
	"github.com/michielvha/stackweaver/backend/internal/executor"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

const (
	testRunnerToken = "swr-runner-token" //nolint:gosec // G101: fixture value, not a credential
	testJobID       = "5d1c9f6e-3b7a-4c58-9e21-7a4f0b6d2c13"
)

// newTestClient returns an executor client whose control plane is the given handler, carrying a
// job token so a test can tell the call used it.
func newTestClient(t *testing.T, controlPlane http.Handler) (*executor.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(controlPlane)
	t.Cleanup(srv.Close)
	return executor.NewClient(srv.URL, testRunnerToken, "runner-1"), srv
}

func TestAPIEventSink_PostsEventWithRunnerToken(t *testing.T) {
	type seen struct {
		method, path, auth, contentType string
		body                            map[string]any
	}
	var got []seen
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		got = append(got, seen{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type"), body})
		w.WriteHeader(http.StatusOK)
	}))

	sink := apiSink{client: client, jobID: testJobID}
	ev := Event{Event: "galaxy_install_failed", Task: "Galaxy Installation Failed", Stdout: "out\n", Stderr: "ERROR! boom\n", Failed: true}
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
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		size = len(raw)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusOK)
	}))

	const reason = "ERROR! the actual reason is on the last line\n"
	big := strings.Repeat("an early line of progress output é\n", 100_000) + reason // about 3.5 MB
	sink := apiSink{client: client, jobID: testJobID}
	if err := sink.Emit(t.Context(), Event{Event: "galaxy_install_failed", Stdout: big, Stderr: big, Failed: true}); err != nil {
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
	if err := sink.Emit(t.Context(), Event{Event: "galaxy_install_complete", Stdout: small}); err != nil {
		t.Fatalf("Emit() error = %v", err)
	}
	if got, _ := body["stdout"].(string); got != small {
		t.Errorf("small stdout = %q, want it unchanged", got)
	}
}

func TestAPIEventSink_NonSuccessResponseIsLoggedNotReturned(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "nope", status)
			}))
			sink := apiSink{client: client, jobID: testJobID}
			if err := sink.Emit(t.Context(), Event{Event: "playbook_source", Stdout: "banner\n"}); err != nil {
				t.Errorf("Emit() error = %v, want a rejected event to be logged and the job to carry on", err)
			}
		})
	}
}

func TestAPIEventSink_UnreachableControlPlaneReturnsError(t *testing.T) {
	client := executor.NewClient("http://127.0.0.1:1", testRunnerToken, "runner-1") // nothing listens here
	sink := apiSink{client: client, jobID: testJobID}
	if err := sink.Emit(t.Context(), Event{Event: "playbook_source"}); err == nil {
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

	// onSnapshot runs when the snapshot is requested, after the executor made its work directory.
	onSnapshot func()

	events                []Event
	artifactsCapabilities string // X-Stackweaver-Runner-Capabilities of the last artifacts request
	snapshotRequests      int
	snapshotAuth          string
	completion            map[string]any // the body of the completion report
	outputs               []string       // the output lines streamed
}

func (cp *fakeControlPlane) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/artifacts"):
		cp.artifactsCapabilities = r.Header.Get("X-Stackweaver-Runner-Capabilities")
		doc := maps.Clone(cp.artifacts)
		if _, set := doc["contract_version"]; !set {
			doc["contract_version"] = runnercontract.Version // what the API serves
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(raw)
	case strings.HasSuffix(r.URL.Path, "/playbook-snapshot"):
		cp.snapshotRequests++
		cp.snapshotAuth = r.Header.Get("Authorization")
		if cp.onSnapshot != nil {
			cp.onSnapshot()
		}
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
		cp.events = append(cp.events, Event{Event: body.Event, Task: body.Task, Stdout: body.Stdout, Stderr: body.Stderr, Failed: body.Failed})
	case strings.HasSuffix(r.URL.Path, "/status"):
		status := cp.jobStatus
		if status == "" {
			status = "running"
		}
		_, _ = w.Write([]byte(`{"status":"` + status + `"}`))
	case strings.HasSuffix(r.URL.Path, "/complete"):
		_ = json.NewDecoder(r.Body).Decode(&cp.completion)
	case strings.HasSuffix(r.URL.Path, "/output"):
		var body struct{ Output string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		cp.outputs = append(cp.outputs, body.Output)
	default: // output lines and anything else the run reports
		w.WriteHeader(http.StatusOK)
	}
}

func (cp *fakeControlPlane) recorded() []Event {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return append([]Event(nil), cp.events...)
}

// jobStubs puts a fake ansible-playbook and git first on PATH. ansible-playbook records
// its working directory, the playbook it was handed (path and content), its whole
// environment, its ANSIBLE_* environment, its cloud credential variables and the file the
// GCP one names; git logs its arguments and "clones" by creating a site.yml in the target.
type jobStubs struct {
	dir string
}

const ansiblePlaybookStub = `#!/bin/sh
rec="$(dirname "$0")"
for a in "$@"; do playbook="$a"; done
pwd > "$rec/cwd"
echo "$@" > "$rec/args"
echo "$playbook" > "$rec/playbook_path"
cat "$playbook" > "$rec/playbook_content"
env | sort > "$rec/full_env"
env | grep '^ANSIBLE_' | sort > "$rec/env"
env | grep -E '^(AWS|AZURE|GCP|GOOGLE|VMWARE)_' | sort > "$rec/cloud_env"
if [ -n "$GOOGLE_APPLICATION_CREDENTIALS" ]; then cat "$GOOGLE_APPLICATION_CREDENTIALS" > "$rec/gcp_credentials"; fi
echo "stub play ok"
if [ -f "$rec/fail" ]; then exit 2; fi
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

// testSnapshot packs files into a gzip-compressed tarball, the shape of a playbook snapshot.
func testSnapshot(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		data := []byte(files[name])
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("writing header for %s: %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}
	return buf.Bytes()
}

// jobRun is one job run end to end against a fake control plane.
type jobRun struct {
	jobID   string
	workDir string
	// root is the WorkRoot: the work directory and the secret directory are made in it.
	root   string
	result Result
}

// inSecretDir reports whether path is a file of a job's secret directory under root.
func inSecretDir(root, path string) bool {
	return strings.HasPrefix(filepath.Dir(path), filepath.Join(root, "stackweaver-secrets-"))
}

// runJob runs a job through Execute, the way the executor subcommand and the agent do: artifacts,
// the play, the completion report. opts.WorkRoot defaults to a temp dir of the test's own.
func runJob(t *testing.T, cp *fakeControlPlane, opts Options) jobRun {
	t.Helper()
	client, _ := newTestClient(t, cp)
	if opts.WorkRoot == "" {
		opts.WorkRoot = t.TempDir()
	}
	if opts.StatusInterval == 0 {
		opts.StatusInterval = 50 * time.Millisecond
	}
	jobID := uuid.NewString()
	res := Execute(t.Context(), client, Job{ID: jobID}, opts)
	return jobRun{jobID: jobID, workDir: filepath.Join(opts.WorkRoot, "ansible-job-"+jobID), root: opts.WorkRoot, result: res}
}

// The API hands the playbook-source contract only to a runner that announces it can download
// snapshots, so the executor must say so on its artifacts request.
func TestExecute_AnnouncesSnapshotCapability(t *testing.T) {
	newJobStubs(t)
	cp := &fakeControlPlane{artifacts: map[string]any{"playbook_path": "site.yml", "playbook_content": "- hosts: all\n"}}
	runJob(t, cp, Options{})

	announced := strings.Split(cp.artifactsCapabilities, ",")
	for i := range announced {
		announced[i] = strings.TrimSpace(announced[i])
	}
	if !slices.Contains(announced, "playbook-snapshot") {
		t.Errorf("X-Stackweaver-Runner-Capabilities = %q, want it to list playbook-snapshot", cp.artifactsCapabilities)
	}
}

// The result is reported to the API as the job's completion, a failure with its reason.
func TestExecute_ReportsTheCompletion(t *testing.T) {
	newJobStubs(t)
	cp := &fakeControlPlane{artifacts: map[string]any{"playbook_path": "missing.yml"}}
	run := runJob(t, cp, Options{})

	if run.result.Status != "failed" {
		t.Fatalf("status = %q, want failed for a playbook that does not exist", run.result.Status)
	}
	if cp.completion["status"] != "failed" || !strings.Contains(fmt.Sprint(cp.completion["error_message"]), "Playbook file not found") {
		t.Errorf("completion = %v, want failed with the reason", cp.completion)
	}
}

// A contract version this executor does not speak is refused before anything runs.
func TestExecute_RefusesAnUnknownContractVersion(t *testing.T) {
	stubs := newJobStubs(t)
	cp := &fakeControlPlane{artifacts: map[string]any{"playbook_path": "site.yml", "playbook_content": "- hosts: all\n", "contract_version": 99}}
	run := runJob(t, cp, Options{})

	if run.result.Status != "failed" || !strings.Contains(run.result.ErrorMessage, "artifacts") {
		t.Errorf("result = %+v, want failed refusing the artifacts", run.result)
	}
	if ran := stubs.read(t, "cwd"); ran != "" {
		t.Errorf("ansible-playbook ran in %q, want nothing run for an unknown contract", ran)
	}
	if !slices.ContainsFunc(cp.outputs, func(o string) bool { return strings.Contains(o, "runner contract") }) {
		t.Errorf("outputs = %q, want the refusal written to the job's output (AC72)", cp.outputs)
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

	run := runJob(t, cp, Options{})

	if run.result.Status != "completed" {
		t.Fatalf("result = %+v, want completed", run.result)
	}
	repoDir := filepath.Join(run.workDir, "repo")
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
		t.Errorf("snapshot request Authorization = %q, want the job's token", cp.snapshotAuth)
	}

	events := cp.recorded()
	want := PlaybookSourceEvent(PlaybookSourceMessage(commit, &capturedAt))
	if len(events) != 1 || events[0] != want {
		t.Errorf("events = %+v, want exactly %+v", events, want)
	}
	if _, err := os.Stat(run.workDir); !os.IsNotExist(err) {
		t.Errorf("the work directory outlived the job (stat error: %v)", err)
	}
}

// The executor never clones (#933, decision 18, AC68): a clone URL in the artifacts, which only an
// old API would send, is not followed, and a job without its snapshot fails rather than fetch its
// files from a VCS host. These cases asserted a clone before the sync work item captured every
// snapshot.
// A fresh-mode playbook is captured at the branch head for each launch and runs from that
// snapshot (#933, decision 18): the executor downloads it, never clones, and the banner says the
// commit was taken at launch rather than from an earlier sync.
func TestRunAnsiblePlaybook_FreshModeRunsTheSnapshotTakenAtLaunch(t *testing.T) {
	stubs := newJobStubs(t)
	capturedAt := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	const commit = "fedcba9876543210fedcba9876543210fedcba98"
	cp := &fakeControlPlane{
		artifacts: map[string]any{
			"job_id":          "ignored",
			"playbook_path":   "site.yml",
			"project_id":      "7c1d5b0a-2f64-4c1e-9d3a-5e8f7a6b4c21",
			"playbook_source": map[string]any{"mode": "fresh", "commit": commit, "captured_at": capturedAt, "size_bytes": 99},
		},
		snapshot: testSnapshot(t, map[string]string{"site.yml": "- hosts: all # captured at launch\n"}),
	}

	run := runJob(t, cp, Options{})

	if run.result.Status != "completed" {
		t.Fatalf("result = %+v, want completed", run.result)
	}
	if got := stubs.read(t, "playbook_content"); got != "- hosts: all # captured at launch" {
		t.Errorf("playbook content = %q, want the launch snapshot's file", got)
	}
	if log := stubs.read(t, "git.log"); log != "" {
		t.Errorf("git was invoked (%q), want no clone for a fresh-mode run", log)
	}
	events := cp.recorded()
	want := PlaybookSourceEvent(FreshSourceMessage(commit))
	if len(events) != 1 || events[0] != want {
		t.Errorf("events = %+v, want exactly %+v", events, want)
	}
	if strings.Contains(events[0].Stdout, "cached snapshot") {
		t.Errorf("a fresh-mode run announced a cached snapshot: %q", events[0].Stdout)
	}
}

func TestRunAnsiblePlaybook_NeverClones(t *testing.T) {
	vcsInfo := map[string]any{"repo_url": "https://example.test/acme/playbooks.git", "branch": "main", "repository": "acme/playbooks"}
	for name, artifacts := range map[string]map[string]any{
		"cached without a snapshot": {"playbook_path": "site.yml", "vcs": vcsInfo, "playbook_source": map[string]any{"mode": "cached"}},
		"fresh source mode":         {"playbook_path": "site.yml", "vcs": vcsInfo, "playbook_source": map[string]any{"mode": "fresh"}},
		"no playbook_source":        {"playbook_path": "site.yml", "vcs": vcsInfo},
	} {
		t.Run(name, func(t *testing.T) {
			stubs := newJobStubs(t)
			cp := &fakeControlPlane{artifacts: artifacts}
			run := runJob(t, cp, Options{})
			if run.result.Status != "failed" {
				t.Errorf("result = %+v, want failed without the playbook's snapshot", run.result)
			}
			if log := stubs.read(t, "git.log"); log != "" {
				t.Errorf("git invocations = %q, want none", log)
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

			run := runJob(t, cp, Options{})

			if run.result.Status != "failed" || !strings.HasPrefix(run.result.ErrorMessage, tt.wantPrefix) {
				t.Errorf("result = %+v, want failed with an error starting %q", run.result, tt.wantPrefix)
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

// A cached run whose snapshot carries a requirements.yml: the executor installs it into the job's
// staging dirs through the shared helper, publishes it to the per-project cache, and hands
// ansible-playbook the staged and playbook-local dirs in front of whatever the host already
// resolves.
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
	run := runJob(t, cp, Options{})

	if run.result.Status != "completed" {
		t.Fatalf("result = %+v, want completed", run.result)
	}
	var names []string
	for _, ev := range cp.recorded() {
		names = append(names, ev.Event)
	}
	if got, want := strings.Join(names, ","), "playbook_source,galaxy_install,galaxy_install_complete"; got != want {
		t.Errorf("events = %s, want %s", got, want)
	}

	workDir := run.workDir
	env := strings.Split(stubs.read(t, "env"), "\n")
	wantCollections := filepath.Join(workDir, "galaxy", "collections") + ":" + filepath.Join(workDir, "repo", "collections") + ":/opt/site/collections"
	if got, _ := envValue(env, "ANSIBLE_COLLECTIONS_PATH"); got != wantCollections {
		t.Errorf("ansible-playbook saw ANSIBLE_COLLECTIONS_PATH = %q, want %q", got, wantCollections)
	}
	wantRoles := filepath.Join(workDir, "galaxy", "roles") + ":" + filepath.Join(workDir, "repo", "roles") + ":~/.ansible/roles:/usr/share/ansible/roles:/etc/ansible/roles"
	if got, _ := envValue(env, "ANSIBLE_ROLES_PATH"); got != wantRoles {
		t.Errorf("ansible-playbook saw ANSIBLE_ROLES_PATH = %q, want %q", got, wantRoles)
	}

	// The install must read the job's ansible.cfg: that is where a private Galaxy server is
	// configured for a host with no route to galaxy.ansible.com.
	if got := galaxy.lines(t, "config.collection"); len(got) != 1 || filepath.Base(got[0]) != "ansible.cfg" || !inSecretDir(run.root, got[0]) {
		t.Errorf("ansible-galaxy saw ANSIBLE_CONFIG = %q, want the job's ansible.cfg in its secret directory", got)
	}

	// The job published nothing: this asserted a per-project warm cache entry before, a cache every
	// job of the project could write, which #933 D7 replaced with the organization's verified,
	// read-only cache (TestRunAnsiblePlaybook_GalaxyWritesNoCache asserts no job writes it).
	_ = projectID
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

// A repository with no requirements.yml gets no Galaxy staging, so the executor must leave the
// Ansible search paths alone: an environment variable beats ansible.cfg, and setting one would
// silently override a roles_path or collections_path the repository relies on.
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

	run := runJob(t, cp, Options{})

	if run.result.Status != "completed" {
		t.Fatalf("result = %+v, want completed", run.result)
	}
	env := strings.Split(stubs.read(t, "env"), "\n")
	for _, name := range []string{"ANSIBLE_ROLES_PATH", "ANSIBLE_COLLECTIONS_PATH"} {
		if value, count := envValue(env, name); count != 0 {
			t.Errorf("ansible-playbook saw %s=%q, want it unset so ansible.cfg stays in charge", name, value)
		}
	}
}

// When Galaxy did stage an install, the staged directories go in front of the paths the effective
// ansible.cfg configures, not in place of them.
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

	run := runJob(t, cp, Options{})
	if run.result.Status != "completed" {
		t.Fatalf("result = %+v, want completed", run.result)
	}

	repo := filepath.Join(run.workDir, "repo")
	env := strings.Split(stubs.read(t, "env"), "\n")
	wantRoles := filepath.Join(run.workDir, "galaxy", "roles") + ":" + filepath.Join(repo, "roles") + ":" + filepath.Join(repo, "vendor", "roles") + ":/opt/shared/roles"
	if got, _ := envValue(env, "ANSIBLE_ROLES_PATH"); got != wantRoles {
		t.Errorf("ANSIBLE_ROLES_PATH = %q, want %q", got, wantRoles)
	}
	wantCollections := filepath.Join(run.workDir, "galaxy", "collections") + ":" + filepath.Join(repo, "collections") + ":" + filepath.Join(repo, "vendor", "collections")
	if got, _ := envValue(env, "ANSIBLE_COLLECTIONS_PATH"); got != wantCollections {
		t.Errorf("ANSIBLE_COLLECTIONS_PATH = %q, want %q", got, wantCollections)
	}
}

// Cancelling a job must reach the preparation steps too. A Galaxy install that hangs would
// otherwise hold the launcher's slot until it gave up on its own.
func TestRunAnsiblePlaybook_CancelledDuringGalaxyInstall(t *testing.T) {
	stubs := newJobStubs(t)
	newGalaxyStub(t).hang(t)

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
	run := runJob(t, cp, Options{})

	if run.result.Status != "canceled" {
		t.Errorf("result = %+v, want canceled", run.result)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("the cancelled job held the executor for %s, want the hung install stopped promptly", elapsed)
	}
	if ran := stubs.read(t, "cwd"); ran != "" {
		t.Errorf("ansible-playbook ran in %q, want a cancelled job not to reach the play", ran)
	}
	if cp.completion["status"] != "canceled" {
		t.Errorf("completion = %v, want canceled reported", cp.completion)
	}
}

// A job writes no cache: the launcher's copy of the organization's cache is read-only to it, and
// the install is staged in the job's own directory whatever its project. This test asserted that a
// job without a usable project id published nothing to the shared per-project cache; that cache is
// gone (#933, D7), so the assertion is now that no job writes the cache it is given.
func TestRunAnsiblePlaybook_GalaxyWritesNoCache(t *testing.T) {
	newJobStubs(t)
	newGalaxyStub(t)
	capturedAt := time.Now().UTC()
	for _, projectID := range []string{"", "../../etc", "7c1d5b0a-2f64-4c1e-9d3a-5e8f7a6b4c21"} {
		t.Run("project_id="+projectID, func(t *testing.T) {
			cp := &fakeControlPlane{
				artifacts: map[string]any{
					"playbook_path":   "site.yml",
					"project_id":      projectID,
					"playbook_source": map[string]any{"mode": "cached", "commit": "abc1234", "captured_at": capturedAt},
				},
				snapshot: testSnapshot(t, map[string]string{"site.yml": "- hosts: all\n", "requirements.yml": "---\ncollections: []\n"}),
			}
			cache := t.TempDir()

			if run := runJob(t, cp, Options{CacheDir: cache}); run.result.Status != "completed" {
				t.Fatalf("result = %+v, want completed", run.result)
			}
			entries, err := os.ReadDir(cache)
			if err != nil {
				t.Fatalf("reading cache dir: %v", err)
			}
			if len(entries) != 0 {
				t.Errorf("the job wrote %d entries into the cache it reads", len(entries))
			}
		})
	}
}
