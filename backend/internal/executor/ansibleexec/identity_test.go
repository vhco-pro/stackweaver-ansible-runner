// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansibleexec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An Ansible job's per-run identities arrive as tokens in its artifacts, the same variables an
// OpenTofu run's use; the executor writes the files the cloud SDKs read into the job's work
// directory and points the play at them, never handing it the raw web identity token (#933, D2,
// AC37).
func TestRunAnsiblePlaybook_MaterialisesThePerRunIdentity(t *testing.T) {
	stubs := newJobStubs(t)
	cp := &fakeControlPlane{artifacts: map[string]any{
		"playbook_path":    "site.yml",
		"playbook_content": "- hosts: all\n",
		"environment_vars": map[string]any{ //nolint:gosec // G101: fixture values, not credentials
			"AWS_ROLE_ARN":           "arn:aws:iam::123456789012:role/ci",
			"AWS_ROLE_SESSION_NAME":  "stackweaver-job",
			"AWS_WEB_IDENTITY_TOKEN": "eyJ.placeholder.token",
		},
	}}

	run := runJob(t, cp, Options{})

	if run.result.Status != "completed" {
		t.Fatalf("result = %+v, want completed", run.result)
	}
	got := cloudEnv(stubs, t)
	if _, raw := got["AWS_WEB_IDENTITY_TOKEN"]; raw {
		t.Error("the play was handed the raw AWS web identity token, want only the file it is written to")
	}
	file := got["AWS_WEB_IDENTITY_TOKEN_FILE"]
	if !inSecretDir(run.root, file) {
		t.Errorf("AWS_WEB_IDENTITY_TOKEN_FILE = %q, want a file of the job's secret directory", file)
	}
	if got["AWS_ROLE_ARN"] != "arn:aws:iam::123456789012:role/ci" {
		t.Errorf("AWS_ROLE_ARN = %q, want the role from the artifacts", got["AWS_ROLE_ARN"])
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("the token file outlived the job (stat error: %v)", err)
	}
}

// The deployment's mirror block reaches both the Galaxy install and the play: the proxy, pip's
// index, the Galaxy servers and a CA bundle that keeps the system roots (#933, D2, AC76).
func TestRunAnsiblePlaybook_AppliesTheMirrorBlock(t *testing.T) {
	stubs := newJobStubs(t)
	galaxy := newGalaxyStub(t)
	unsetEnv(t, "ANSIBLE_CONFIG")
	cp := &fakeControlPlane{
		artifacts: map[string]any{
			"playbook_path":   "site.yml",
			"playbook_source": map[string]any{"mode": "cached", "commit": "abc1234", "captured_at": time.Now().UTC()},
			"mirror": map[string]any{
				"https_proxy":     "http://proxy.internal:3128",
				"no_proxy":        "stackweaver.internal",
				"pip_index_url":   "https://pypi.internal/simple",
				"galaxy_servers":  []map[string]any{{"name": "hub", "url": "https://hub.internal/api/galaxy/", "token": "hub-token"}},
				"ca_certificates": "-----BEGIN CERTIFICATE-----\nplaceholder\n-----END CERTIFICATE-----",
			},
		},
		snapshot: testSnapshot(t, map[string]string{"site.yml": "- hosts: all\n", "requirements.yml": "---\ncollections: []\n"}),
	}

	run := runJob(t, cp, Options{})

	if run.result.Status != "completed" {
		t.Fatalf("result = %+v, want completed", run.result)
	}
	full := strings.Split(stubs.read(t, "full_env"), "\n")
	for name, want := range map[string]string{
		"HTTPS_PROXY":                   "http://proxy.internal:3128",
		"https_proxy":                   "http://proxy.internal:3128",
		"NO_PROXY":                      "stackweaver.internal",
		"PIP_INDEX_URL":                 "https://pypi.internal/simple",
		"ANSIBLE_GALAXY_SERVER_LIST":    "hub",
		"ANSIBLE_GALAXY_SERVER_HUB_URL": "https://hub.internal/api/galaxy/",
	} {
		if got, _ := envValue(full, name); got != want {
			t.Errorf("ansible-playbook saw %s=%q, want %q", name, got, want)
		}
	}
	if ca, _ := envValue(full, "SSL_CERT_FILE"); filepath.Base(ca) != "stackweaver-ca-bundle.pem" || !inSecretDir(run.root, ca) {
		t.Errorf("ansible-playbook saw SSL_CERT_FILE=%q, want the CA bundle in the job's secret directory", ca)
	}
	if got := galaxy.lines(t, "server_list.collection"); len(got) != 1 || got[0] != "hub" {
		t.Errorf("ansible-galaxy saw ANSIBLE_GALAXY_SERVER_LIST = %q, want the deployment's servers", got)
	}
}

// A job whose own ansible.cfg names a Galaxy server_list keeps it: the deployment's servers are
// the default, not an override.
func TestRunAnsiblePlaybook_JobGalaxyServerListWins(t *testing.T) {
	stubs := newJobStubs(t)
	cp := &fakeControlPlane{artifacts: map[string]any{
		"playbook_path":    "site.yml",
		"playbook_content": "- hosts: all\n",
		"ansible_cfg":      "[galaxy]\nserver_list = private_hub\n",
		"mirror":           map[string]any{"galaxy_servers": []map[string]any{{"name": "hub", "url": "https://hub.internal/"}}},
	}}

	if run := runJob(t, cp, Options{}); run.result.Status != "completed" {
		t.Fatalf("result = %+v, want completed", run.result)
	}
	full := strings.Split(stubs.read(t, "full_env"), "\n")
	if value, count := envValue(full, "ANSIBLE_GALAXY_SERVER_LIST"); count != 0 {
		t.Errorf("ANSIBLE_GALAXY_SERVER_LIST = %q, want it unset so the job's ansible.cfg stays in charge", value)
	}
	if got, _ := envValue(full, "ANSIBLE_GALAXY_SERVER_HUB_URL"); got != "https://hub.internal/" {
		t.Errorf("ANSIBLE_GALAXY_SERVER_HUB_URL = %q, want the server defined for a job that lists it", got)
	}
}
