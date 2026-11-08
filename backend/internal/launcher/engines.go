// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
)

// EnvEngines restricts a launcher to the named engines: "tofu", "ansible" or "tofu,ansible".
const EnvEngines = "RUNNER_ENGINES"

// The engines a launcher can serve, in the order it declares them.
const (
	EngineTofu    = "tofu"
	EngineAnsible = "ansible"
)

var allEngines = []string{EngineTofu, EngineAnsible}

// toolchainProbes are the commands that tell whether an engine's toolchain runs in this process.
var toolchainProbes = map[string][]string{
	EngineTofu:    {"tofu", "version"},
	EngineAnsible: {"ansible", "--version"},
}

// probeName is an engine's probe as an operator would type it.
func probeName(engine string) string { return strings.Join(toolchainProbes[engine], " ") }

// probeToolchain runs an engine's version probe in this process and returns the version it
// reports: OpenTofu's bare version ("1.13.1"), the first line of `ansible --version`, which the
// API parses.
func probeToolchain(engine string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	argv := toolchainProbes[engine]
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output() //nolint:gosec // G204: the probes are package constants
	if err != nil {
		return "", fmt.Errorf("%s: %w", probeName(engine), err)
	}
	line, _, _ := strings.Cut(string(out), "\n")
	line = strings.TrimSpace(line)
	if engine == EngineTofu {
		// "OpenTofu v1.13.1"
		if f := strings.Fields(line); len(f) >= 2 {
			return strings.TrimPrefix(f[1], "v"), nil
		}
	}
	return line, nil
}

// engineChoice is the engines a launcher serves and the toolchain versions it found in its own
// process.
type engineChoice struct {
	engines  []string
	versions map[string]string
}

// chooseEngines applies the engine rule (one-launcher spec, A2): RUNNER_ENGINES wins, refused
// in-process for an engine whose toolchain does not answer; otherwise an in-process launcher
// serves the engines whose toolchain answers on its host and refuses to start with none; otherwise
// the container and Kubernetes drivers serve both, each in its job image. probeHost applies the
// in-process rule on any driver.
func chooseEngines(b Binary, driverName string, probeHost bool, getenv func(string) string) (engineChoice, error) {
	named, err := parseEngines(getenv(EnvEngines))
	if err != nil {
		return engineChoice{}, err
	}
	probe := probeHost || driverName == driver.NameInProcess
	if !probe {
		return engineChoice{engines: firstNonNil(named, allEngines)}, nil
	}
	candidates := named
	if candidates == nil {
		for _, e := range allEngines {
			if b.Executors[e] != nil {
				candidates = append(candidates, e)
			}
		}
	}
	choice := engineChoice{versions: map[string]string{}}
	var failed []string
	for _, e := range candidates {
		if b.Executors[e] == nil {
			return engineChoice{}, fmt.Errorf("%s names %s, and this binary has no %s executor", EnvEngines, e, e)
		}
		v, err := probeToolchain(e)
		if err != nil {
			failed = append(failed, "`"+probeName(e)+"`")
			continue
		}
		choice.engines = append(choice.engines, e)
		choice.versions[e] = v
	}
	if named != nil && len(failed) > 0 {
		return engineChoice{}, fmt.Errorf("%s names an engine whose toolchain does not answer on this host: %s failed; install it or remove it from %s", EnvEngines, strings.Join(failed, " and "), EnvEngines)
	}
	if len(choice.engines) == 0 {
		return engineChoice{}, fmt.Errorf("no engine toolchain answers on this host: %s failed; install OpenTofu or Ansible, or use the container or kubernetes driver", strings.Join(failed, " and "))
	}
	return choice, nil
}

// parseEngines reads RUNNER_ENGINES; unset is nil.
func parseEngines(v string) ([]string, error) {
	var out []string
	for _, e := range strings.Split(v, ",") {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !slices.Contains(allEngines, e) {
			return nil, fmt.Errorf("%s=%q names %q, which is not an engine (tofu, ansible)", EnvEngines, v, e)
		}
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	// Declared in the canonical order whatever order they were listed in.
	slices.SortFunc(out, func(a, b string) int { return slices.Index(allEngines, a) - slices.Index(allEngines, b) })
	return out, nil
}

func firstNonNil(vals ...[]string) []string {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}
