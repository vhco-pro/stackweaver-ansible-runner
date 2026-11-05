// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// What a job with a GCP credential runs with (#912). The executor builds the environment from the
// service account the API put in the job's artifacts, through ansible.GCPCredentialEnv, so these
// tests compare with that function's answer and do not repeat the variable names: the names are
// pinned once, in core/services/ansible.

package ansibleexec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/core/services/ansible"
)

// testServiceAccount stands in for a decrypted service account file. The executor does not read
// it, so any text will do, and it is deliberately not shaped like a real one, which the secret
// scans match on sight.
const testServiceAccount = `{"fixture": "stands in for a service account file"}`

// wantGCPEnv is what the shared helper answers for a key written into dir.
func wantGCPEnv(t *testing.T, dir string) map[string]string {
	t.Helper()
	want, err := ansible.GCPCredentialEnv(t.TempDir(), testServiceAccount)
	if err != nil {
		t.Fatalf("GCPCredentialEnv: %v", err)
	}
	// The helper wrote into a directory of its own; the path the executor exports is in its job
	// directory, under the same file name.
	for name, value := range want {
		if filepath.IsAbs(value) {
			want[name] = filepath.Join(dir, filepath.Base(value))
		}
	}
	return want
}

// cloudEnv parses what the ansible-playbook stub recorded of its cloud variables.
func cloudEnv(stubs *jobStubs, t *testing.T) map[string]string {
	t.Helper()
	got := map[string]string{}
	for line := range strings.SplitSeq(stubs.read(t, "cloud_env"), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			got[name] = value
		}
	}
	return got
}

// From the artifacts document to ansible-playbook's environment.
func TestRunAnsiblePlaybook_GCPCredential(t *testing.T) {
	stubs := newJobStubs(t)
	cp := &fakeControlPlane{artifacts: map[string]any{
		"playbook_path":       "site.yml",
		"playbook_content":    "- hosts: all\n",
		"gcp_service_account": testServiceAccount,
		// What the API sends beside it for another attached credential: left as sent.
		"environment_vars": map[string]any{"AWS_ACCESS_KEY_ID": "not-an-access-key-id"},
	}}

	run := runJob(t, cp, Options{})

	if run.result.Status != "completed" {
		t.Fatalf("result = %+v, want completed", run.result)
	}
	got := cloudEnv(stubs, t)
	key := got["GOOGLE_APPLICATION_CREDENTIALS"]
	if !inSecretDir(run.root, key) {
		t.Fatalf("GOOGLE_APPLICATION_CREDENTIALS = %q, want a file of the job's secret directory", key)
	}
	want := wantGCPEnv(t, filepath.Dir(key))
	want["AWS_ACCESS_KEY_ID"] = "not-an-access-key-id"
	// Name by name, because the play also inherits whatever cloud variables the machine running
	// this test happens to have.
	for name, value := range want {
		if got[name] != value {
			t.Errorf("ansible-playbook saw %s=%q, want %q", name, got[name], value)
		}
	}
	if got := stubs.read(t, "gcp_credentials"); got != testServiceAccount {
		t.Errorf("GOOGLE_APPLICATION_CREDENTIALS named a file holding %q, want the service account from the artifacts", got)
	}
	// The decrypted key is on disk for the run and no longer.
	if _, err := os.Stat(want["GOOGLE_APPLICATION_CREDENTIALS"]); !os.IsNotExist(err) {
		t.Errorf("the service account file outlived the job (stat error: %v)", err)
	}
}

// A key that cannot be written fails the job: a play run without it, where a task asks for
// application default credentials, gets the identity of the machine the executor is on.
func TestRunAnsiblePlaybook_GCPCredentialThatCannotBeWritten(t *testing.T) {
	stubs := newJobStubs(t)
	root := t.TempDir()
	cp := &fakeControlPlane{
		artifacts: map[string]any{
			"playbook_path":       "site.yml",
			"gcp_service_account": testServiceAccount,
			"playbook_source":     map[string]any{"mode": "cached", "commit": "abc1234", "captured_at": time.Now().UTC()},
		},
		snapshot: testSnapshot(t, map[string]string{"site.yml": "- hosts: all\n"}),
	}
	// Once the executor has made its secret directory (before the snapshot is fetched), a directory
	// takes the key file's path, so the write fails whoever runs the test.
	cp.onSnapshot = func() {
		matches, _ := filepath.Glob(filepath.Join(root, "stackweaver-secrets-*"))
		for _, secretDir := range matches {
			blocker := filepath.Join(secretDir, filepath.Base(wantGCPEnv(t, secretDir)["GOOGLE_APPLICATION_CREDENTIALS"]))
			if err := os.MkdirAll(blocker, 0o750); err != nil {
				t.Errorf("occupying the key file's path: %v", err)
			}
		}
	}

	run := runJob(t, cp, Options{WorkRoot: root})

	if run.result.Status != "failed" || !strings.Contains(run.result.ErrorMessage, "GCP") {
		t.Errorf("result = %+v, want failed with a message naming the GCP credential", run.result)
	}
	if ran := stubs.read(t, "cwd"); ran != "" {
		t.Errorf("ansible-playbook ran in %q, want it not to run without the credential the job names", ran)
	}
}
