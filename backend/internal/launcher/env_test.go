// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"slices"
	"strings"
	"testing"
)

// What a placed executor starts with (#933, D1, AC1): the server, the job, its token, and the
// settings every process needs, never the launcher's own secrets: its registration token, its
// runner token, or anything else its environment holds.
func TestExecutorEnvCarriesNoLauncherSecrets(t *testing.T) {
	secrets := map[string]string{
		"STACKWEAVER_TOKEN":          "placeholder-registration-token",
		"STACKWEAVER_HOSTED_SECRET":  "placeholder-hosted-secret",
		"DATABASE_PASSWORD":          "placeholder-database-password",
		"STORAGE_SECRET_KEY":         "placeholder-storage-secret",
		"REDIS_PASSWORD":             "placeholder-redis-password",
		"ENCRYPTION_KEY":             "placeholder-encryption-key",
		"OIDC_SIGNING_KEY":           "placeholder-signing-key",
		"GITHUB_APP_PRIVATE_KEY":     "placeholder-github-key",
		"SOME_FUTURE_LAUNCHER_VALUE": "placeholder-future",
	}
	environ := []string{"PATH=/usr/bin", "HTTPS_PROXY=http://proxy:3128", "TZ=UTC"}
	for k, v := range secrets {
		environ = append(environ, k+"="+v)
	}

	env := ExecutorEnv(environ, ExecutorInvocation{ServerURL: "https://api.example.test", JobID: "run-1", Token: "swj-job", RunType: "plan", RunnerID: "runner-1"})

	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		if _, secret := secrets[name]; secret {
			t.Errorf("the executor is handed the launcher's %s", name)
		}
		for secretName, secretValue := range secrets {
			if strings.Contains(value, secretValue) {
				t.Errorf("%s carries the launcher's %s", name, secretName)
			}
		}
	}
	for _, want := range []string{"STACKWEAVER_SERVER=https://api.example.test", "STACKWEAVER_JOB_ID=run-1", "STACKWEAVER_JOB_TOKEN=swj-job", "STACKWEAVER_JOB_RUN_TYPE=plan", "STACKWEAVER_RUNNER_ID=runner-1", "HTTPS_PROXY=http://proxy:3128", "TZ=UTC"} {
		if !slices.Contains(env, want) {
			t.Errorf("the executor's environment lacks %s", want)
		}
	}
}

// The operator's passthrough list reaches a placed executor with the variables it names, so the
// executor passes them on to OpenTofu and Ansible as an in-process job would (#933, D8): without
// the list itself, the executor's own allowlist would drop every name on it.
func TestExecutorEnvCarriesThePassthroughList(t *testing.T) {
	env := ExecutorEnv([]string{"PATH=/bin", "STACKWEAVER_RUN_ENV_PASSTHROUGH=CORP_TOKEN", "CORP_TOKEN=abc", "OTHER=x"}, ExecutorInvocation{ServerURL: "https://api", JobID: "run-1"})
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	if got["CORP_TOKEN"] != "abc" || got["STACKWEAVER_RUN_ENV_PASSTHROUGH"] != "CORP_TOKEN" {
		t.Errorf("executor env = %v, want CORP_TOKEN and the passthrough list", env)
	}
	if _, ok := got["OTHER"]; ok {
		t.Error("a variable the list does not name reached the executor")
	}
}

// A placed executor runs in its engine's job image, whose own PATH finds that engine's tools: the
// launcher's PATH is the launcher's image's, and an OpenTofu launcher's would hide Ansible from an
// Ansible job it places in the Ansible job image (one-launcher spec, A3).
func TestExecutorEnvLeavesPathToTheJobImage(t *testing.T) {
	env := ExecutorEnv([]string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/bin", "TZ=UTC"}, ExecutorInvocation{ServerURL: "https://api", JobID: "job-1"})
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			t.Errorf("the placed executor is handed the launcher's %s", kv)
		}
	}
	if !slices.Contains(env, "TZ=UTC") {
		t.Errorf("executor env = %v, want the rest of the base list kept", env)
	}
}
