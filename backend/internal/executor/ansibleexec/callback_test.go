// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansibleexec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// Every play reports through the callback the executor brings, written into the job, so an
// execution environment without ansible.posix reports the same events as the platform image
// (#933, D6, AC28).
func TestPlayEnvUsesTheInjectedCallback(t *testing.T) {
	secrets := t.TempDir()
	env, msg := playEnv(t.Context(), &runnercontract.AnsibleArtifacts{}, secrets, "", nil)
	if msg != "" {
		t.Fatal(msg)
	}
	stdout, _ := envValue(env, "ANSIBLE_STDOUT_CALLBACK")
	if stdout != CallbackName {
		t.Errorf("ANSIBLE_STDOUT_CALLBACK = %q, want %s", stdout, CallbackName)
	}
	plugins, _ := envValue(env, "ANSIBLE_CALLBACK_PLUGINS")
	dir, _, _ := strings.Cut(plugins, ":")
	if !strings.HasPrefix(dir, secrets) {
		t.Fatalf("ANSIBLE_CALLBACK_PLUGINS = %q, want the job's own directory first", plugins)
	}
	got, err := os.ReadFile(filepath.Join(dir, CallbackName+".py")) //nolint:gosec // G304: the test's directory
	if err != nil || string(got) != callbackSource {
		t.Errorf("the callback written into the job differs from the executor's (%v)", err)
	}
	if !strings.Contains(plugins, "/usr/share/ansible/plugins/callback") {
		t.Errorf("ANSIBLE_CALLBACK_PLUGINS = %q drops Ansible's own search path", plugins)
	}
}

// Ansible's home and temporary directories are the job's own when the image sets none: an
// execution environment whose user's home is on the read-only root filesystem would otherwise fail
// every task ("Failed to create temporary directory"), and one the image or operator sets is kept
// (#933, D6, AC28).
func TestPlayEnvGivesAnsibleWritableDirectories(t *testing.T) {
	for _, name := range []string{"ANSIBLE_HOME", "ANSIBLE_REMOTE_TMP", "ANSIBLE_LOCAL_TEMP"} {
		t.Setenv(name, "")
	}
	secrets := t.TempDir()
	env, msg := playEnv(t.Context(), &runnercontract.AnsibleArtifacts{}, secrets, "", nil)
	if msg != "" {
		t.Fatal(msg)
	}
	for _, name := range []string{"ANSIBLE_HOME", "ANSIBLE_REMOTE_TMP", "ANSIBLE_LOCAL_TEMP"} {
		if v, _ := envValue(env, name); !strings.HasPrefix(v, secrets) {
			t.Errorf("%s = %q, want a directory of the job", name, v)
		}
	}

	t.Setenv("ANSIBLE_REMOTE_TMP", "/tmp/image-remote-tmp")
	env, _ = playEnv(t.Context(), &runnercontract.AnsibleArtifacts{}, secrets, "", nil)
	if v, _ := envValue(env, "ANSIBLE_REMOTE_TMP"); v != "/tmp/image-remote-tmp" {
		t.Errorf("ANSIBLE_REMOTE_TMP = %q, want the image's own", v)
	}
}

// An Azure OIDC configuration's per-run token reaches a playbook as a file too, which
// azure.azcollection's workload identity and the Azure SDKs read (AZURE_FEDERATED_TOKEN_FILE).
func TestPlayEnvWritesTheAzureFederatedTokenFile(t *testing.T) {
	secrets := t.TempDir()
	arts := &runnercontract.AnsibleArtifacts{EnvironmentVars: map[string]string{"AZURE_CLIENT_ID": "c", "AZURE_FEDERATED_TOKEN": "h.p.s"}}
	env, msg := playEnv(t.Context(), arts, secrets, "", nil)
	if msg != "" {
		t.Fatal(msg)
	}
	path, _ := envValue(env, "AZURE_FEDERATED_TOKEN_FILE")
	if !strings.HasPrefix(path, secrets) {
		t.Fatalf("AZURE_FEDERATED_TOKEN_FILE = %q, want a file of the job", path)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "h.p.s" { //nolint:gosec // the test's own directory
		t.Errorf("token file = %q (%v)", data, err)
	}
}
