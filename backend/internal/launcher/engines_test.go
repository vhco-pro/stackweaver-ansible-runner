// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
)

// stubToolchains puts a PATH holding only the named engine toolchains ("tofu", "ansible") in
// place for the test, each a stub that answers its version probe.
func stubToolchains(t *testing.T, tools ...string) {
	t.Helper()
	dir := t.TempDir()
	scripts := map[string]string{
		"tofu":    "#!/bin/sh\necho 'OpenTofu v1.13.1'\necho 'on linux_amd64'\n",
		"ansible": "#!/bin/sh\necho 'ansible [core 2.18.1]'\necho '  config file = None'\n",
	}
	for _, tool := range tools {
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(scripts[tool]), 0o755); err != nil { //nolint:gosec // G306: a test stub must be executable
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

// bothExecutors is what the one binary brings: an executor for each engine.
func bothExecutors() Binary {
	noop := func(context.Context, driver.Invocation) int { return 0 }
	return Binary{Executors: map[string]driver.ExecutorFunc{"tofu": noop, "ansible": noop}}
}

// A launcher works out the engines it serves (one-launcher spec, A2, AC4, AC5): RUNNER_ENGINES
// wins; otherwise an in-process launcher serves the toolchains that answer on its host and refuses
// to start with none; the container and Kubernetes drivers serve both, each in its job image.
func TestEnginesALauncherServes(t *testing.T) {
	cases := []struct {
		name    string
		tools   []string
		driver  string
		engines string
		want    []string
		wantErr []string
	}{
		{name: "in-process with only tofu", tools: []string{"tofu"}, want: []string{"tofu"}},
		{name: "in-process with only ansible", tools: []string{"ansible"}, want: []string{"ansible"}},
		{name: "in-process with both", tools: []string{"tofu", "ansible"}, want: []string{"tofu", "ansible"}},
		{name: "in-process named explicitly", tools: []string{"tofu", "ansible"}, driver: driver.NameInProcess, want: []string{"tofu", "ansible"}},
		{name: "in-process with neither", wantErr: []string{"tofu version", "ansible --version"}},
		{name: "container defaults to both", driver: driver.NameContainer, want: []string{"tofu", "ansible"}},
		{name: "kubernetes defaults to both", driver: driver.NameKubernetes, want: []string{"tofu", "ansible"}},
		{name: "RUNNER_ENGINES restricts in-process", tools: []string{"tofu", "ansible"}, engines: "ansible", want: []string{"ansible"}},
		{name: "RUNNER_ENGINES restricts container", driver: driver.NameContainer, engines: "tofu", want: []string{"tofu"}},
		{name: "RUNNER_ENGINES restricts kubernetes", driver: driver.NameKubernetes, engines: "ansible", want: []string{"ansible"}},
		{name: "RUNNER_ENGINES lists both", driver: driver.NameContainer, engines: "tofu, ansible", want: []string{"tofu", "ansible"}},
		{name: "RUNNER_ENGINES names a missing toolchain in-process", tools: []string{"tofu"}, engines: "tofu,ansible", wantErr: []string{"ansible --version"}},
		{name: "RUNNER_ENGINES names an unknown engine", driver: driver.NameContainer, engines: "puppet", wantErr: []string{EnvEngines, "puppet"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubToolchains(t, tc.tools...)
			env := map[string]string{
				EnvServer: "https://api.test", EnvToken: "swa-agent", EnvPoolID: "pool-1",
				EnvDriver: tc.driver, EnvEngines: tc.engines,
			}
			cfg, err := ConfigFromEnv(bothExecutors(), envOf(env))
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("engines = %v, want a refusal naming %v", cfg.Engines, tc.wantErr)
				}
				for _, s := range tc.wantErr {
					if !strings.Contains(err.Error(), s) {
						t.Errorf("refusal %q does not name %q", err, s)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(cfg.Engines, tc.want) {
				t.Errorf("engines = %v, want %v", cfg.Engines, tc.want)
			}
		})
	}
}
