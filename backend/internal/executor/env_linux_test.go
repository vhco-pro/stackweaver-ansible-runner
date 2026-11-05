// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

//go:build linux

package executor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const (
	executorRoleEnv = "EXECUTOR_TEST_ROLE"
	testJobToken    = "swj-placeholder-job-token-933" //nolint:gosec // G101: fixture value, not a credential
)

// TestMain lets the test binary act as an executor: started with a job token in its environment,
// it reads its invocation and starts a child the way the engines' children are started, with the
// executor's own environment, and the child reports whether it can see the token in its environment
// or in the executor's /proc/<pid>/environ. The token has to be in the environment the process was
// started with, because /proc shows that one and not later os.Unsetenv changes.
func TestMain(m *testing.M) {
	role := os.Getenv(executorRoleEnv)
	switch role {
	case "":
		os.Exit(m.Run())
	case "executor":
		if _, err := InvocationFromEnv(); err != nil {
			fmt.Println("invocation failed:", err)
			os.Exit(3)
		}
	case "unprotected":
		// The control: the token only removed from the Go environment, the process left dumpable.
		_ = os.Unsetenv(EnvJobToken)
	}
	cmd := exec.CommandContext(context.Background(), "/bin/sh", "-c", `env; echo ---; tr '\0' '\n' < /proc/$PPID/environ`)
	// cmd.Env is left nil: the child inherits everything the executor still holds.
	out, err := cmd.CombinedOutput()
	own, parent, _ := strings.Cut(string(out), "---")
	fmt.Printf("own=%t parent=%t err=%v\n", strings.Contains(own, testJobToken), strings.Contains(parent, testJobToken), err)
	os.Exit(0)
}

func runAsExecutor(t *testing.T, role string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0]) //nolint:gosec // G204: os.Args[0] is this test binary
	cmd.Env = []string{
		executorRoleEnv + "=" + role, "PATH=/usr/bin:/bin",
		EnvServer + "=http://127.0.0.1:1", EnvJobID + "=job-1", EnvJobToken + "=" + testJobToken,
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("executor helper (%s): %v: %s", role, err, out)
	}
	return strings.TrimSpace(string(out))
}

// The job token stays in the executor's memory (#933, D1, AC5): a process the job starts, which
// runs as the executor's own user, finds it neither in its own environment nor in the executor's
// /proc/<pid>/environ.
func TestJobTokenNeverReachesTheJobsProcesses(t *testing.T) {
	// Control: a dumpable process that only unset the variable still leaks it through /proc, so the
	// pass below is not vacuous.
	if got := runAsExecutor(t, "unprotected"); !strings.HasPrefix(got, "own=false parent=true") {
		t.Fatalf("control: a same-user child should read an unprotected executor's environ, got %q", got)
	}
	got := runAsExecutor(t, "executor")
	if !strings.HasPrefix(got, "own=false parent=false") {
		t.Fatalf("the job's child saw the job token: %q", got)
	}
}
