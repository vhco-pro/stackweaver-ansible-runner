// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
)

// configFrom reads a launcher's configuration from env on top of a customer launcher's minimum.
func configFrom(t *testing.T, env map[string]string) (Config, error) {
	t.Helper()
	full := map[string]string{EnvServer: "https://api.test", EnvToken: "swa-agent", EnvPoolID: "pool-1"}
	maps.Copy(full, env)
	return ConfigFromEnv(bothExecutors(), envOf(full))
}

func deprecated(cfg Config) bool {
	return slices.ContainsFunc(cfg.Deprecations, func(w string) bool {
		return strings.Contains(w, EnvImage) && strings.Contains(w, EnvTofuImage) && strings.Contains(w, EnvAnsibleImage)
	})
}

// Each engine runs in its own job image (one-launcher spec, A3): the job image variables default
// to the published job images' latest (OD14); a set variable wins over RUNNER_IMAGE for its engine,
// with the deprecation warning (AC46); RUNNER_IMAGE alone keeps today's meaning, serving the
// engines found in the launcher's own process with a warning naming the new variables (AC18).
func TestJobImages(t *testing.T) {
	t.Run("defaults to each job image's latest", func(t *testing.T) {
		cfg, err := configFrom(t, map[string]string{EnvDriver: driver.NameContainer})
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{EngineTofu: DefaultTofuImage, EngineAnsible: DefaultAnsibleImage}
		if !maps.Equal(cfg.JobImages, want) {
			t.Errorf("job images = %v, want %v", cfg.JobImages, want)
		}
		if DefaultTofuImage != "ghcr.io/vhco-pro/stackweaver-opentofu-runner:latest" || DefaultAnsibleImage != "ghcr.io/vhco-pro/stackweaver-ansible-runner:latest" {
			t.Errorf("defaults = %s, %s", DefaultTofuImage, DefaultAnsibleImage)
		}
		if deprecated(cfg) {
			t.Errorf("a launcher without RUNNER_IMAGE warned: %v", cfg.Deprecations)
		}
	})
	t.Run("a job image variable wins over RUNNER_IMAGE for its engine", func(t *testing.T) {
		cfg, err := configFrom(t, map[string]string{EnvDriver: driver.NameContainer, EnvImage: "old:1", EnvTofuImage: "tofu:2"})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.JobImages[EngineTofu] != "tofu:2" {
			t.Errorf("tofu job image = %q, want the variable's tofu:2", cfg.JobImages[EngineTofu])
		}
		if !slices.Equal(cfg.Engines, []string{EngineTofu, EngineAnsible}) {
			t.Errorf("engines = %v, want both: a job image variable is not the legacy RUNNER_IMAGE-only mode", cfg.Engines)
		}
		if !deprecated(cfg) {
			t.Errorf("no deprecation warning naming the new variables: %v", cfg.Deprecations)
		}
	})
	t.Run("both job image variables", func(t *testing.T) {
		cfg, err := configFrom(t, map[string]string{EnvDriver: driver.NameKubernetes, EnvTofuImage: "tofu:2", EnvAnsibleImage: "ansible:3"})
		if err != nil {
			t.Fatal(err)
		}
		if !maps.Equal(cfg.JobImages, map[string]string{EngineTofu: "tofu:2", EngineAnsible: "ansible:3"}) {
			t.Errorf("job images = %v", cfg.JobImages)
		}
	})
	t.Run("RUNNER_IMAGE alone serves the engines in the launcher's own process", func(t *testing.T) {
		stubToolchains(t, "ansible")
		cfg, err := configFrom(t, map[string]string{EnvDriver: driver.NameContainer, EnvImage: "ghcr.io/vhco-pro/stackweaver-ansible-runner:0.6.128"})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(cfg.Engines, []string{EngineAnsible}) {
			t.Errorf("engines = %v, want the one toolchain in this process", cfg.Engines)
		}
		if cfg.JobImages[EngineAnsible] != "ghcr.io/vhco-pro/stackweaver-ansible-runner:0.6.128" {
			t.Errorf("ansible job image = %q, want RUNNER_IMAGE", cfg.JobImages[EngineAnsible])
		}
		if !deprecated(cfg) {
			t.Errorf("no deprecation warning naming the new variables: %v", cfg.Deprecations)
		}
	})
}

// The executor is at one constant path in every job image (A1), and the launcher sends that
// command for each engine (A3).
func TestExecutorPathIsConstant(t *testing.T) {
	if ExecutorPath != "/usr/local/bin/stackweaver-launcher" {
		t.Errorf("ExecutorPath = %q", ExecutorPath)
	}
	cfg, err := configFrom(t, map[string]string{EnvDriver: driver.NameContainer})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []string{EngineTofu, EngineAnsible} {
		if !slices.Equal(cfg.ExecutorCommand[e], []string{ExecutorPath, "executor"}) {
			t.Errorf("%s executor command = %v", e, cfg.ExecutorCommand[e])
		}
	}
}

// pullCases are the pull policies a launcher's environment implies: RUNNER_JOB_IMAGE_PULL is
// always, missing or never (OD18); unset, it is always, except that RUNNER_IMAGE alone sets no
// policy, today's behaviour, so an existing agent works unchanged (OD29, AC18, AC40, AC46).
var pullCases = []struct {
	name string
	env  map[string]string
	want driver.PullPolicy
}{
	{"no image variable defaults to always", map[string]string{}, driver.PullAlways},
	{"the OpenTofu job image variable defaults to always", map[string]string{EnvTofuImage: "tofu:2"}, driver.PullAlways},
	{"the Ansible job image variable defaults to always", map[string]string{EnvAnsibleImage: "ansible:3"}, driver.PullAlways},
	{"RUNNER_IMAGE alone sets no policy", map[string]string{EnvImage: "old:1"}, ""},
	{"RUNNER_IMAGE with a job image variable defaults to always", map[string]string{EnvImage: "old:1", EnvTofuImage: "tofu:2"}, driver.PullAlways},
	{"always", map[string]string{EnvJobImagePull: "always"}, driver.PullAlways},
	{"missing", map[string]string{EnvJobImagePull: "missing"}, driver.PullMissing},
	{"never", map[string]string{EnvJobImagePull: "never"}, driver.PullNever},
	{"case and spaces are ignored", map[string]string{EnvJobImagePull: " Missing "}, driver.PullMissing},
	{"an explicit always wins over RUNNER_IMAGE alone", map[string]string{EnvImage: "old:1", EnvJobImagePull: "always"}, driver.PullAlways},
	{"an explicit never wins over RUNNER_IMAGE alone", map[string]string{EnvImage: "old:1", EnvJobImagePull: "never"}, driver.PullNever},
	{"an explicit missing wins over a job image variable", map[string]string{EnvTofuImage: "tofu:2", EnvJobImagePull: "missing"}, driver.PullMissing},
}

// pullEnv is a container launcher's environment for a pull case. A RUNNER_IMAGE-only launcher
// serves the engines in its own process, so the test puts a toolchain on PATH.
func pullEnv(t *testing.T, env map[string]string) map[string]string {
	t.Helper()
	stubToolchains(t, "tofu")
	full := map[string]string{EnvDriver: driver.NameContainer}
	maps.Copy(full, env)
	return full
}

func TestJobImagePull(t *testing.T) {
	for _, tc := range pullCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := configFrom(t, pullEnv(t, tc.env))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.JobImagePull != tc.want {
				t.Errorf("policy = %q, want %q", cfg.JobImagePull, tc.want)
			}
		})
	}
	t.Run("an unknown policy is refused naming the variable", func(t *testing.T) {
		_, err := configFrom(t, pullEnv(t, map[string]string{EnvJobImagePull: "sometimes"}))
		if err == nil || !strings.Contains(err.Error(), EnvJobImagePull) {
			t.Errorf("err = %v, want a refusal naming %s", err, EnvJobImagePull)
		}
	})
}

// The container driver gets the pull policy the launcher's environment implies, so
// RUNNER_JOB_IMAGE_PULL and its defaults reach it (A3, OD18, OD29).
func TestContainerDriverFromEnvCarriesPullPolicy(t *testing.T) {
	for _, tc := range pullCases {
		t.Run(tc.name, func(t *testing.T) {
			env := pullEnv(t, tc.env)
			cfg, err := configFrom(t, env)
			if err != nil {
				t.Fatal(err)
			}
			full := map[string]string{EnvServer: "https://api.test", EnvToken: "swa-agent", EnvPoolID: "pool-1"}
			maps.Copy(full, env)
			d, err := DriverFromEnv(t.Context(), bothExecutors(), cfg, envOf(full))
			if err != nil {
				t.Fatal(err)
			}
			c, ok := d.(*driver.Container)
			if !ok {
				t.Fatalf("driver = %T, want *driver.Container", d)
			}
			if c.JobImagePull != tc.want {
				t.Errorf("container pull policy = %q, want %q", c.JobImagePull, tc.want)
			}
		})
	}
}

// RUNNER_FIREWALL_IMAGE defaults to the OpenTofu job image, which carries nft and iptables (OD15,
// AC41).
func TestFirewallImageDefault(t *testing.T) {
	cfg, err := configFrom(t, map[string]string{EnvDriver: driver.NameContainer, EnvTofuImage: "tofu:2"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FirewallImage != "tofu:2" {
		t.Errorf("firewall image = %q, want the OpenTofu job image", cfg.FirewallImage)
	}
	cfg, err = configFrom(t, map[string]string{EnvDriver: driver.NameContainer, EnvFirewallImage: "launcher:1"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FirewallImage != "launcher:1" {
		t.Errorf("firewall image = %q, want RUNNER_FIREWALL_IMAGE", cfg.FirewallImage)
	}
}

// A launcher reports the toolchain versions only for the engines that run in its own process,
// which means in-process; a container or Kubernetes launcher reports none, so no "unknown" version
// reaches the runner detail page (A2, AC8).
func TestToolchainVersionsOnlyInProcess(t *testing.T) {
	stubToolchains(t, "tofu", "ansible")
	cfg, err := configFrom(t, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TofuVersion != "1.13.1" || cfg.AnsibleVersion != "ansible [core 2.18.1]" {
		t.Errorf("in-process versions = %q, %q", cfg.TofuVersion, cfg.AnsibleVersion)
	}
	for _, env := range []map[string]string{
		{EnvDriver: driver.NameContainer},
		{EnvDriver: driver.NameKubernetes},
		{EnvDriver: driver.NameContainer, EnvImage: "ghcr.io/vhco-pro/stackweaver-opentofu-runner:0.2.35"},
	} {
		cfg, err := configFrom(t, env)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.TofuVersion != "" || cfg.AnsibleVersion != "" {
			t.Errorf("%v reports versions %q, %q, want none", env, cfg.TofuVersion, cfg.AnsibleVersion)
		}
	}
}

// RUNNER_PLACEMENT_TIMEOUT bounds how long a Kubernetes job pod may wait for its image before the
// job fails (OD21), with a default.
func TestPlacementTimeout(t *testing.T) {
	cfg, err := configFrom(t, map[string]string{EnvDriver: driver.NameKubernetes})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PlacementTimeout != driver.DefaultPlacementTimeout {
		t.Errorf("default placement timeout = %s, want %s", cfg.PlacementTimeout, driver.DefaultPlacementTimeout)
	}
	cfg, err = configFrom(t, map[string]string{EnvDriver: driver.NameKubernetes, EnvPlacementTimeout: "90s"})
	if err != nil || cfg.PlacementTimeout.Seconds() != 90 {
		t.Errorf("RUNNER_PLACEMENT_TIMEOUT=90s = %s, %v", cfg.PlacementTimeout, err)
	}
	if _, err := configFrom(t, map[string]string{EnvDriver: driver.NameKubernetes, EnvPlacementTimeout: "soon"}); err == nil || !strings.Contains(err.Error(), EnvPlacementTimeout) {
		t.Errorf("an unreadable placement timeout = %v, want refused naming %s", err, EnvPlacementTimeout)
	}
}
