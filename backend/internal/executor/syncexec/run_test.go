// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// The sync executor end to end against a stand-in API (#933, D2, AC10): a playbook snapshot is
// cloned, scoped and uploaded with its commit; a constructed rebuild runs the command its
// artifacts carry, with the files written into the secret directory, and reports the inventory;
// the secret directory is gone afterwards.

package syncexec

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/michielvha/stackweaver/backend/internal/executor"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

type fakeAPI struct {
	arts runnercontract.WorkItemArtifacts

	mu       sync.Mutex
	output   strings.Builder
	snapshot []byte
	commit   string
	complete *runnercontract.JobCompleteRequest
}

func (f *fakeAPI) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/artifacts"):
			_ = json.NewEncoder(w).Encode(f.arts)
		case strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = io.WriteString(w, `{"status":"running"}`)
		case strings.HasSuffix(r.URL.Path, "/output"):
			var req runnercontract.JobOutputRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.output.WriteString(req.Output)
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		case strings.HasSuffix(r.URL.Path, "/snapshot"):
			f.snapshot, _ = io.ReadAll(r.Body)
			f.commit = r.Header.Get(runnercontract.HeaderSnapshotCommit)
			_, _ = io.WriteString(w, `{"status":"stored"}`)
		case strings.HasSuffix(r.URL.Path, "/complete"):
			var req runnercontract.JobCompleteRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.complete = &req
			_, _ = io.WriteString(w, `{"status":"completed"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestExecute_PlaybookSnapshot(t *testing.T) {
	const token = "tok-snapshot-0123456789" //nolint:gosec // G101: a test value
	url, ca, commit := gitServer(t, "x-access-token", token)
	caPEM, err := os.ReadFile(ca) //nolint:gosec // G304: the test server's CA
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{arts: runnercontract.WorkItemArtifacts{
		ContractVersion: runnercontract.Version, JobID: "wi-snap", Kind: "playbook_snapshot", PlaybookPath: "site.yml",
		Clone:  &runnercontract.CloneSource{URL: url, Branch: "main", Username: "x-access-token", Token: token},
		Mirror: &runnercontract.Mirror{CACertificates: string(caPEM)},
	}}
	srv := api.serve(t)
	root := t.TempDir()
	res := Execute(t.Context(), executor.NewClient(srv.URL, "job-token", "runner"), "wi-snap", Options{WorkRoot: root})
	if res.Status != "completed" {
		t.Fatalf("work item ended %q: %s\noutput:\n%s", res.Status, res.ErrorMessage, api.output.String())
	}
	if len(api.snapshot) == 0 || api.commit != commit {
		t.Errorf("uploaded %d bytes at %q, want the snapshot at %s", len(api.snapshot), api.commit, commit)
	}
	if api.complete == nil || api.complete.Status != "completed" || api.complete.WorkResult == nil || api.complete.WorkResult.Commit != commit {
		t.Errorf("completion = %+v", api.complete)
	}
	if strings.Contains(api.output.String(), token) {
		t.Error("the output carries the token")
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("the work root still holds %d entries after the work item", len(entries))
	}
}

func TestExecute_ConstructedRebuildRunsItsCommand(t *testing.T) {
	if _, err := exec.LookPath("ansible-inventory"); err != nil {
		t.Skip("ansible-inventory is not installed")
	}
	api := &fakeAPI{arts: runnercontract.WorkItemArtifacts{
		ContractVersion: runnercontract.Version, JobID: "wi-con", Kind: "constructed_rebuild",
		Inventory: &runnercontract.InventoryCommand{
			Files: map[string]string{
				"00_input.json":   `{"all":{"hosts":{"web1":{"role":"web"},"db1":{"role":"db"}}}}`,
				"constructed.yml": "plugin: ansible.builtin.constructed\nstrict: false\nkeyed_groups:\n  - key: role\n    prefix: role\n",
			},
			Args: []string{"-i", runnercontract.SecretDirPlaceholder + "/00_input.json", "-i", runnercontract.SecretDirPlaceholder + "/constructed.yml", "--list"},
		},
	}}
	srv := api.serve(t)
	res := Execute(t.Context(), executor.NewClient(srv.URL, "job-token", "runner"), "wi-con", Options{WorkRoot: t.TempDir()})
	if res.Status != "completed" {
		t.Fatalf("work item ended %q: %s\noutput:\n%s", res.Status, res.ErrorMessage, api.output.String())
	}
	var inv map[string]any
	if api.complete == nil || api.complete.WorkResult == nil || json.Unmarshal(api.complete.WorkResult.Inventory, &inv) != nil {
		t.Fatalf("completion carries no inventory: %+v", api.complete)
	}
	if _, ok := inv["role_web"]; !ok {
		t.Errorf("the rebuilt inventory has no role_web group: %v", inv)
	}
}

func TestExecute_ConfigurationSnapshot(t *testing.T) {
	const token = "tok-config-0123456789" //nolint:gosec // G101: a test value
	url, ca, commit := gitServer(t, "oauth2", token)
	caPEM, err := os.ReadFile(ca) //nolint:gosec // G304: the test server's CA
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{arts: runnercontract.WorkItemArtifacts{
		ContractVersion: runnercontract.Version, JobID: "wi-cfg", Kind: "configuration_snapshot",
		Clone:  &runnercontract.CloneSource{URL: url, Branch: "main", Username: "oauth2", Token: token},
		Mirror: &runnercontract.Mirror{CACertificates: string(caPEM)},
	}}
	srv := api.serve(t)
	res := Execute(t.Context(), executor.NewClient(srv.URL, "job-token", "runner"), "wi-cfg", Options{WorkRoot: t.TempDir()})
	if res.Status != "completed" {
		t.Fatalf("work item ended %q: %s", res.Status, res.ErrorMessage)
	}
	if api.commit != commit {
		t.Errorf("uploaded at %q, want %s", api.commit, commit)
	}
	dir := t.TempDir()
	if err := executor.ExtractTarGz(bytes.NewReader(api.snapshot), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "site.yml")); err != nil {
		t.Errorf("the configuration lacks the repository's files: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		t.Error("the configuration carries the repository's .git")
	}
}

// commitIn makes a commit in the work repository that changes site.yml to content and returns it.
func commitIn(t *testing.T, work, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(work, "site.yml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, work, "commit", "-q", "-am", content)
	head, err := exec.Command("git", "-C", work, "rev-parse", "HEAD").Output() //nolint:gosec,noctx // G204: the test's own repository
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(head))
}

// A capture asked for a commit gets that commit, even when the branch has moved past it before
// the executor runs and the executor's clone is shallow (#938, AC-20). A commit the branch does not
// hold arrives through the clone source's fetch ref, as a pull request from a fork does; a commit
// the host does not have fails the capture.
func TestExecute_ConfigurationSnapshotAtARequestedCommit(t *testing.T) {
	const token = "tok-pinned-0123456789" //nolint:gosec // G101: a test value
	url, ca, first, root := gitServerAt(t, "oauth2", token)
	work, bare := filepath.Join(root, "work"), filepath.Join(root, "repo.git")
	second := commitIn(t, work, "- hosts: second\n")
	gitIn(t, work, "push", "-q", bare, "main")
	gitIn(t, work, "checkout", "-q", "-b", "fork", first)
	pull := commitIn(t, work, "- hosts: pull-request\n")
	gitIn(t, work, "push", "-q", bare, "fork:refs/pull/1/head")
	caPEM, err := os.ReadFile(ca) //nolint:gosec // G304: the test server's CA
	if err != nil {
		t.Fatal(err)
	}
	capture := func(t *testing.T, src runnercontract.CloneSource) (*fakeAPI, Result) {
		t.Helper()
		src.URL, src.Branch, src.Username, src.Token = url, "main", "oauth2", token
		api := &fakeAPI{arts: runnercontract.WorkItemArtifacts{
			ContractVersion: runnercontract.Version, JobID: "wi-pin", Kind: "configuration_snapshot",
			Clone: &src, Mirror: &runnercontract.Mirror{CACertificates: string(caPEM)},
		}}
		srv := api.serve(t)
		return api, Execute(t.Context(), executor.NewClient(srv.URL, "job-token", "runner"), "wi-pin", Options{WorkRoot: t.TempDir()})
	}
	contentOf := func(t *testing.T, api *fakeAPI) string {
		t.Helper()
		dir := t.TempDir()
		if err := executor.ExtractTarGz(bytes.NewReader(api.snapshot), dir); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(dir, "site.yml")) //nolint:gosec // G304: the test's own directory
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	setAllowReachable := func(t *testing.T, allow bool) {
		t.Helper()
		gitIn(t, bare, "config", "uploadpack.allowReachableSHA1InWant", strconv.FormatBool(allow))
		gitIn(t, bare, "config", "uploadpack.allowAnySHA1InWant", "false")
	}

	t.Run("an earlier commit of the branch, fetched by id", func(t *testing.T) {
		setAllowReachable(t, true)
		api, res := capture(t, runnercontract.CloneSource{Commit: first})
		if res.Status != "completed" {
			t.Fatalf("work item ended %q: %s", res.Status, res.ErrorMessage)
		}
		if api.commit != first || contentOf(t, api) != "- hosts: all\n" {
			t.Errorf("captured %s with %q, want the requested %s, not the branch head %s", api.commit, contentOf(t, api), first, second)
		}
	})
	t.Run("an earlier commit of the branch, when the host refuses a fetch by id", func(t *testing.T) {
		setAllowReachable(t, false)
		api, res := capture(t, runnercontract.CloneSource{Commit: first})
		if res.Status != "completed" {
			t.Fatalf("work item ended %q: %s", res.Status, res.ErrorMessage)
		}
		if api.commit != first || contentOf(t, api) != "- hosts: all\n" {
			t.Errorf("captured %s with %q, want the requested %s", api.commit, contentOf(t, api), first)
		}
	})
	t.Run("a commit only the fetch ref holds", func(t *testing.T) {
		setAllowReachable(t, false)
		api, res := capture(t, runnercontract.CloneSource{Commit: pull, FetchRef: "refs/pull/1/head"})
		if res.Status != "completed" {
			t.Fatalf("work item ended %q: %s", res.Status, res.ErrorMessage)
		}
		if api.commit != pull || contentOf(t, api) != "- hosts: pull-request\n" {
			t.Errorf("captured %s with %q, want the pull request's %s", api.commit, contentOf(t, api), pull)
		}
	})
	t.Run("a commit the host does not have fails the capture", func(t *testing.T) {
		setAllowReachable(t, true)
		api, res := capture(t, runnercontract.CloneSource{Commit: strings.Repeat("0", 39) + "1"})
		if res.Status == "completed" || api.snapshot != nil {
			t.Errorf("the capture of an absent commit ended %q and uploaded %d byte(s); it must fail without uploading", res.Status, len(api.snapshot))
		}
	})
}
