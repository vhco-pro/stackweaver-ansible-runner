// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// What the executor hands the processes that run user-supplied code (#932). ansible-playbook and
// ansible-galaxy run code a user controls: a playbook's env lookup, a module, a collection's
// install hook. The executor's environment may hold a launcher's or a platform's secrets, so each
// test sets them in this process and asserts none reaches the child. The values are placeholders
// that only have to be recognisable.

package ansibleexec

import (
	"os"
	"strings"
	"testing"
)

// runnerSecrets are variables a runner deployment holds for its own use, plus one a future
// deployment might add, which must stay out of a child without anybody listing it.
var runnerSecrets = map[string]string{
	"DATABASE_PASSWORD":        "placeholder-database-password",
	"REDIS_PASSWORD":           "placeholder-redis-password",
	"STORAGE_SECRET_KEY":       "placeholder-storage-secret",
	"ENCRYPTION_KEY":           "placeholder-encryption-key",
	"ANSIBLE_ENCRYPTION_KEY":   "placeholder-ansible-encryption-key",
	"OIDC_SIGNING_KEY":         "placeholder-signing-key",
	"STACKWEAVER_TOKEN":        "placeholder-agent-token",
	"STACKWEAVER_JOB_TOKEN":    "placeholder-job-token",
	"SOME_FUTURE_PLATFORM_KEY": "placeholder-future-secret",
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

func TestGalaxyCommand_LeavesRunnerSecretsBehind(t *testing.T) {
	setRunnerSecrets(t)
	for _, in := range []GalaxyInstall{
		{PlaybookDir: t.TempDir()},
		{PlaybookDir: t.TempDir(), Env: []string{"ANSIBLE_CONFIG=/job/ansible.cfg"}}, // pointed at the job's ansible.cfg
	} {
		cmd := in.galaxyCommand(t.Context(), "collection", "install", "-r", "requirements.yml")
		assertNoRunnerSecrets(t, "ansible-galaxy", cmd.Env)
	}
}

// End to end: the stand-in ansible-playbook plays the user's code and records everything it can
// read.
func TestRunAnsiblePlaybook_UserCodeCannotReadRunnerSecrets(t *testing.T) {
	stubs := newJobStubs(t)
	setRunnerSecrets(t)
	// The playbook comes from its snapshot: the executor never clones (#933, decision 18).
	cp := &fakeControlPlane{
		artifacts: map[string]any{
			"playbook_path":   "site.yml",
			"playbook_source": map[string]any{"mode": "cached", "commit": "0123456789abcdef", "captured_at": "2026-10-11T08:15:00Z", "size_bytes": 1},
		},
		snapshot: testSnapshot(t, map[string]string{"site.yml": "- hosts: all\n"}),
	}

	run := runJob(t, cp, Options{})

	if run.result.Status != "completed" {
		t.Fatalf("result = %+v, want completed", run.result)
	}
	seen := stubs.read(t, "full_env")
	if seen == "" {
		t.Fatal("the stand-in ansible-playbook recorded no environment")
	}
	assertNoRunnerSecrets(t, "ansible-playbook", strings.Split(seen, "\n"))
}
