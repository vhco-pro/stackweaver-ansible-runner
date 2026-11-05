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
