// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"slices"
	"strings"

	"github.com/michielvha/stackweaver/core/runnercontract"
	"github.com/michielvha/stackweaver/core/security/runenv"
)

// ExecutorInvocation is what a driver hands a placed executor.
type ExecutorInvocation struct {
	ServerURL string
	JobID     string
	Token     string
	RunType   string
	RunnerID  string
}

// ExecutorEnv is the environment a placed executor starts with (AC1): what every process needs
// from the launcher's own environment (runenv's base list: locale, proxies, CA bundles, pod-level
// cloud identity) and the invocation. Nothing else of environ reaches it: the launcher holds its
// registration and runner tokens there, and on an install that has not split them yet, platform
// secrets. PATH is left to the job image: a placed executor runs in its engine's job image, whose
// own PATH finds that engine's tools, and the launcher's is its own image's (one-launcher spec,
// A3).
func ExecutorEnv(environ []string, inv ExecutorInvocation) []string {
	env := slices.DeleteFunc(runenv.Inherit(environ, runenv.Allowlist{}), func(kv string) bool {
		return strings.HasPrefix(kv, "PATH=")
	})
	// The passthrough list travels with the variables it names: the executor's own allowlist
	// (tofu.RunEnv, ansible.InheritedEnv) reads it to pass them on to the job's tools.
	for _, kv := range environ {
		if strings.HasPrefix(kv, runenv.PassthroughVar+"=") {
			env = append(env, kv)
		}
	}
	env = append(env,
		runnercontract.EnvServer+"="+inv.ServerURL,
		runnercontract.EnvJobID+"="+inv.JobID,
		runnercontract.EnvJobToken+"="+inv.Token,
		runnercontract.EnvRunnerID+"="+inv.RunnerID,
	)
	if inv.RunType != "" {
		env = append(env, runnercontract.EnvRunType+"="+inv.RunType)
	}
	return env
}
