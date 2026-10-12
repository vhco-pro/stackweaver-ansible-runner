// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// What a job with a GCP credential runs with, on both runner paths (#912). The platform runner
// builds the environment from the database row; the self-hosted runner builds it from the
// service account the API put in the job's artifacts. Both go through
// ansible.GCPCredentialEnv, so these tests compare each path with that function's answer and do
// not repeat the variable names: the names are pinned once, in core/services/ansible.
//
// The defect these were written against: both paths exported GCP_AUTH_KIND=serviceaccount with
// only GOOGLE_APPLICATION_CREDENTIALS, which no google.cloud module accepts.

package main

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/michielvha/stackweaver/core/models"
	"github.com/michielvha/stackweaver/core/services/ansible"
)

// testServiceAccount stands in for a decrypted service account file. Neither runner reads it, so
// any text will do, and it is deliberately not shaped like a real one, which the secret scans
// match on sight.
const testServiceAccount = `{"fixture": "stands in for a service account file"}`

// wantGCPEnv is what the shared helper answers for a key written into dir.
func wantGCPEnv(t *testing.T, dir string) map[string]string {
	t.Helper()
	want, err := ansible.GCPCredentialEnv(t.TempDir(), testServiceAccount)
	if err != nil {
		t.Fatalf("GCPCredentialEnv: %v", err)
	}
	// The helper wrote into a directory of its own; the path a runner exports is in its job
	// directory, under the same file name.
	for name, value := range want {
		if filepath.IsAbs(value) {
			want[name] = filepath.Join(dir, filepath.Base(value))
		}
	}
	return want
}

func TestApplyCredential_GCP(t *testing.T) {
	jobDir := t.TempDir()
	prep := &preparedCredentials{envVars: map[string]string{}}
	vaults := 0
	credential := &models.AnsibleCredential{Type: models.CredentialTypeGCP, GCPServiceAccount: testServiceAccount}

	if err := (&AnsibleRunner{}).applyCredential(prep, credential, jobDir, &vaults); err != nil {
		t.Fatalf("applyCredential: %v", err)
	}

	want := wantGCPEnv(t, jobDir)
	if !maps.Equal(prep.envVars, want) {
		t.Errorf("job variables = %v, want %v", prep.envVars, want)
	}
	// And they reach ansible-playbook: playbookEnv is what executeJob hands the command.
	env := playbookEnv([]string{"PATH=/usr/bin"}, prep.envVars, jobDir, false, "", "")
	for name, value := range want {
		if !slices.Contains(env, name+"="+value) {
			t.Errorf("ansible-playbook's environment lacks %s=%s", name, value)
		}
	}
	written, err := os.ReadFile(prep.envVars["GOOGLE_APPLICATION_CREDENTIALS"])
	if err != nil {
		t.Fatalf("reading the file the job is pointed at: %v", err)
	}
	if string(written) != testServiceAccount {
		t.Errorf("the file holds %q, want the credential's service account", written)
	}
}

func TestApplyCredential_GCPWithoutServiceAccount(t *testing.T) {
	prep := &preparedCredentials{envVars: map[string]string{}}
	vaults := 0

	if err := (&AnsibleRunner{}).applyCredential(prep, &models.AnsibleCredential{Type: models.CredentialTypeGCP}, t.TempDir(), &vaults); err != nil {
		t.Fatalf("applyCredential: %v", err)
	}

	if len(prep.envVars) != 0 {
		t.Errorf("job variables = %v, want none: an auth kind with no key behind it sends every module to the machine's identity", prep.envVars)
	}
}

// The self-hosted path, from the artifacts document to ansible-playbook's environment.
func TestRunAnsiblePlaybook_GCPCredential(t *testing.T) {
	stubs := newJobStubs(t)
	cp := &fakeControlPlane{artifacts: map[string]any{
		"playbook_path":       "site.yml",
		"playbook_content":    "- hosts: all\n",
		"gcp_service_account": testServiceAccount,
		// What the API sends beside it for another attached credential: left as sent.
		"environment_vars": map[string]any{"AWS_ACCESS_KEY_ID": "not-an-access-key-id"},
	}}

	jobID, status, errMsg := runJob(t, cp)

	if status != "completed" {
		t.Fatalf("status = %q (%s), want completed", status, errMsg)
	}
	workDir := filepath.Join("/tmp", "ansible-job-"+jobID)
	got := map[string]string{}
	for line := range strings.SplitSeq(stubs.read(t, "cloud_env"), "\n") {
		if name, value, ok := strings.Cut(line, "="); ok {
			got[name] = value
		}
	}
	want := wantGCPEnv(t, workDir)
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

// A key that cannot be written fails the job. It used to be logged and the play run without it,
// where a task that asks for application default credentials gets the identity of the machine
// the runner is on.
func TestRunAnsiblePlaybook_GCPCredentialThatCannotBeWritten(t *testing.T) {
	stubs := newJobStubs(t)
	cp := &fakeControlPlane{artifacts: map[string]any{
		"playbook_path":       "site.yml",
		"playbook_content":    "- hosts: all\n",
		"gcp_service_account": testServiceAccount,
	}}
	agent := newTestAgent(t, cp)
	jobID := uuid.NewString()
	artifacts, err := agent.downloadJobArtifacts(jobID)
	if err != nil {
		t.Fatalf("downloading artifacts: %v", err)
	}
	// A directory where the key file goes: the write fails, whoever runs the test.
	workDir := filepath.Join("/tmp", "ansible-job-"+jobID)
	blocker := filepath.Join(workDir, filepath.Base(wantGCPEnv(t, workDir)["GOOGLE_APPLICATION_CREDENTIALS"]))
	if err := os.MkdirAll(blocker, 0o750); err != nil {
		t.Fatalf("occupying the key file's path: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workDir) })

	status, errMsg := agent.runAnsiblePlaybook(jobID, artifacts)

	if status != "failed" || !strings.Contains(errMsg, "GCP") {
		t.Errorf("status = %q (%s), want failed with a message naming the GCP credential", status, errMsg)
	}
	if ran := stubs.read(t, "cwd"); ran != "" {
		t.Errorf("ansible-playbook ran in %q, want it not to run without the credential the job names", ran)
	}
}
