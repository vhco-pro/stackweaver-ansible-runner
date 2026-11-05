// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"slices"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// A launcher started --single (RUNNER_SINGLE) runs one job, claims no other even with capacity
// left, then deregisters and returns: the ephemeral launcher an operator starts per job with a
// just-in-time token (#933, D11).
func TestSingleRunsOneJobThenDeregisters(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	d := newFakeDriver("container")
	cfg := testConfig(api)
	cfg.Single = true
	l := New(cfg, d)
	l.goos = "linux"
	api.offer(runnercontract.PendingJob{JobID: "run-1", JobType: "tofu_run", RunType: "plan"}, runnercontract.PendingJob{JobID: "run-2", JobType: "tofu_run", RunType: "plan"})

	result := make(chan error, 1)
	go func() { result <- l.Run(t.Context()) }()
	eventually(t, "one job placed", func() bool { return len(d.placements()) == 1 })
	time.Sleep(100 * time.Millisecond)
	if n := len(d.placements()); n != 1 {
		t.Fatalf("a --single launcher placed %d jobs", n)
	}
	d.end(d.placements()[0].JobID, exitCompleted)

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the --single launcher did not return after its job")
	}
	started, _, _, deregistered := api.snapshot()
	if !deregistered || len(started) != 1 || slices.Contains(started, "") {
		t.Errorf("started %v, deregistered %v; want one job and a deregistration", started, deregistered)
	}
}

// --single on the command line or RUNNER_SINGLE in the environment.
func TestSingleFromArgsOrEnv(t *testing.T) {
	b := Binary{Engine: "tofu"}
	env := map[string]string{EnvServer: "https://api.test", EnvToken: "swa-agent", EnvPoolID: "pool-1"}
	getenv := func(k string) string { return env[k] }
	if cfg, err := ConfigFromEnv(b, getenv); err != nil || cfg.Single {
		t.Fatalf("default = %+v %v, want not single", cfg.Single, err)
	}
	env[EnvSingle] = "true"
	if cfg, _ := ConfigFromEnv(b, getenv); !cfg.Single {
		t.Error("RUNNER_SINGLE=true is not single")
	}
	if !SingleFromArgs([]string{"runner", "--single"}) || SingleFromArgs([]string{"runner", "executor"}) {
		t.Error("--single is not read from the arguments")
	}
}
