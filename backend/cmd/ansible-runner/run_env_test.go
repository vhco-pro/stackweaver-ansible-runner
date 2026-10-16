// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// What the Ansible runner hands the processes that run user-supplied code (#932).
//
// The platform runner is deployed with the platform's own secrets in its environment (database,
// Redis and storage credentials, the encryption key, the workload identity signing key, VCS app
// secrets), and a self-hosted agent holds its runner token there. ansible-playbook,
// ansible-inventory and ansible-galaxy run code a user controls: a playbook's env lookup, an
// inventory plugin or script, a module. The defect these tests were written against: every one of
// them was started with the runner's whole environment, so that code could read every one of
// those values.
//
// Each test sets those variables in this process, as the runner's deployment does, and asserts
// none of them reaches the child. The values are placeholders that only have to be recognisable.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runnerSecrets are variables the platform passes a runner for its own use, plus one a future
// deployment might add, which must stay out of a child without anybody listing it.
var runnerSecrets = map[string]string{
	"DATABASE_PASSWORD":          "placeholder-database-password",
	"REDIS_PASSWORD":             "placeholder-redis-password",
	"STORAGE_ACCESS_KEY":         "placeholder-storage-access",
	"STORAGE_SECRET_KEY":         "placeholder-storage-secret",
	"ENCRYPTION_KEY":             "placeholder-encryption-key",
	"ANSIBLE_ENCRYPTION_KEY":     "placeholder-ansible-encryption-key",
	"OIDC_SIGNING_KEY":           "placeholder-signing-key",
	"GITHUB_APP_PRIVATE_KEY":     "placeholder-github-app-key",
	"AZURE_DEVOPS_CLIENT_SECRET": "placeholder-azure-devops-secret",
	"STACKWEAVER_TOKEN":          "placeholder-agent-token",
	"SOME_FUTURE_PLATFORM_KEY":   "placeholder-future-secret",
}

// setRunnerSecrets puts runnerSecrets in this process's environment for the rest of the test.
func setRunnerSecrets(t *testing.T) {
	t.Helper()
	for name, value := range runnerSecrets {
		t.Setenv(name, value)
	}
}

// assertNoRunnerSecrets fails for every runner secret env carries, by name or by value. A nil env
// is what os/exec treats as "inherit this process's environment", so it is checked as that.
func assertNoRunnerSecrets(t *testing.T, site string, env []string) {
	t.Helper()
	if env == nil {
		env = os.Environ()
	}
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		if _, secret := runnerSecrets[name]; secret {
			t.Errorf("%s: the child is handed the runner's %s", site, name)
			continue
		}
		for secretName, secretValue := range runnerSecrets {
			if strings.Contains(value, secretValue) {
				t.Errorf("%s: %s carries the value of the runner's %s", site, name, secretName)
			}
		}
	}
}

func TestPlaybookEnv_LeavesRunnerSecretsBehind(t *testing.T) {
	setRunnerSecrets(t)
	jobDir := t.TempDir()

	env := playbookEnv(map[string]string{"HOME": filepath.Join(jobDir, "home"), "ANSIBLE_HOME": filepath.Join(jobDir, ".ansible")}, jobDir, false, "", "")

	assertNoRunnerSecrets(t, "ansible-playbook (platform runner)", env)
}

func TestInventorySyncEnv_LeavesRunnerSecretsBehind(t *testing.T) {
	setRunnerSecrets(t)

	env := inventorySyncEnv(t.TempDir(), "00000000-0000-0000-0000-000000000000")

	assertNoRunnerSecrets(t, "ansible-inventory (repository inventory sync)", env)
}

func TestGalaxyCommand_LeavesRunnerSecretsBehind(t *testing.T) {
	setRunnerSecrets(t)
	for _, in := range []galaxyInstallInput{
		{playbookDir: t.TempDir()}, // the platform runner adds nothing
		{playbookDir: t.TempDir(), env: []string{"ANSIBLE_CONFIG=/job/ansible.cfg"}}, // the agent points it at the job's ansible.cfg
	} {
		cmd := in.galaxyCommand(t.Context(), "collection", "install", "-r", "requirements.yml")
		assertNoRunnerSecrets(t, "ansible-galaxy", cmd.Env)
	}
}

// End to end on the self-hosted path: the stand-in ansible-playbook plays the user's code and
// records everything it can read. The agent's environment holds its runner token.
func TestRunAnsiblePlaybook_UserCodeCannotReadRunnerSecrets(t *testing.T) {
	stubs := newJobStubs(t)
	setRunnerSecrets(t)
	cp := &fakeControlPlane{artifacts: map[string]any{
		"playbook_path": "site.yml",
		"vcs":           map[string]any{"repo_url": "https://example.invalid/org/repo.git", "branch": "main"},
	}}

	_, status, errMsg := runJob(t, cp)

	if status != "completed" {
		t.Fatalf("status = %q (%s), want completed", status, errMsg)
	}
	seen := stubs.read(t, "full_env")
	if seen == "" {
		t.Fatal("the stand-in ansible-playbook recorded no environment")
	}
	assertNoRunnerSecrets(t, "ansible-playbook (self-hosted agent)", strings.Split(seen, "\n"))
}
