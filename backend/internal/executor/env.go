// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/michielvha/stackweaver/core/runnercontract"
	"github.com/michielvha/stackweaver/core/security/runenv"
)

// The environment a driver starts the `executor` subcommand with (#933, D1, D4), named once in
// the runner contract for the launcher and the executor alike.
const (
	EnvServer   = runnercontract.EnvServer
	EnvJobID    = runnercontract.EnvJobID
	EnvJobToken = runnercontract.EnvJobToken
	EnvRunType  = runnercontract.EnvRunType
	EnvRunnerID = runnercontract.EnvRunnerID
)

// Exit codes of the `executor` subcommand: the job's outcome, so a driver reads it from the
// container or pod without the API.
const (
	ExitCompleted = 0
	ExitFailed    = 1
	// ExitUnreported is an executor whose completion never reached the API: its launcher reports
	// the job instead, as for a crash. A Go panic exits with the same code, having reported nothing
	// either.
	ExitUnreported = 2
	ExitStopped    = 3
)

// Invocation is what the `executor` subcommand is started with.
type Invocation struct {
	ServerURL string
	JobID     string
	Token     string
	RunType   string
	RunnerID  string
	// CacheDir is the launcher's read-only copy of the organization's cache, when one is mounted.
	CacheDir string
}

// InvocationFromEnv reads the invocation and keeps the job token in memory only (AC5): it marks the
// process non-dumpable first, so a process the job starts, which runs as the executor's own user,
// cannot read the token from the executor's /proc/<pid>/environ, and then removes it from the
// environment, so no child inherits it. An executor that cannot protect itself refuses to run.
func InvocationFromEnv() (Invocation, error) {
	if err := runenv.DenySameUserInspection(); err != nil {
		_ = os.Unsetenv(EnvJobToken)
		return Invocation{}, fmt.Errorf("protecting the job token: %w", err)
	}
	inv := Invocation{
		ServerURL: strings.TrimSpace(os.Getenv(EnvServer)),
		JobID:     strings.TrimSpace(os.Getenv(EnvJobID)),
		Token:     strings.TrimSpace(os.Getenv(EnvJobToken)),
		RunType:   strings.TrimSpace(os.Getenv(EnvRunType)),
		RunnerID:  strings.TrimSpace(os.Getenv(EnvRunnerID)),
		CacheDir:  strings.TrimSpace(os.Getenv(EnvCacheDir)),
	}
	_ = os.Unsetenv(EnvJobToken)
	if inv.ServerURL == "" || inv.JobID == "" || inv.Token == "" {
		return inv, errors.New("the executor needs " + EnvServer + ", " + EnvJobID + " and " + EnvJobToken)
	}
	return inv, nil
}

// ExitCode maps a reported status to the subcommand's exit code.
func ExitCode(status string) int {
	switch status {
	case "completed":
		return ExitCompleted
	case "canceled":
		return ExitStopped
	default:
		return ExitFailed
	}
}
