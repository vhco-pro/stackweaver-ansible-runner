// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
	"github.com/michielvha/stackweaver/core/runnercontract"
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
	if cfg.PlacementTimeout != 5*time.Minute {
		t.Errorf("default placement timeout = %s, want 5m", cfg.PlacementTimeout)
	}
	cfg, err = configFrom(t, map[string]string{EnvDriver: driver.NameKubernetes, EnvPlacementTimeout: "90s"})
	if err != nil || cfg.PlacementTimeout.Seconds() != 90 {
		t.Errorf("RUNNER_PLACEMENT_TIMEOUT=90s = %s, %v", cfg.PlacementTimeout, err)
	}
	if _, err := configFrom(t, map[string]string{EnvDriver: driver.NameKubernetes, EnvPlacementTimeout: "soon"}); err == nil || !strings.Contains(err.Error(), EnvPlacementTimeout) {
		t.Errorf("an unreadable placement timeout = %v, want refused naming %s", err, EnvPlacementTimeout)
	}
}

// RUNNER_INVENTORY_INTERVAL and RUNNER_REAP_INTERVAL set how often the job namespace's inventory
// rides on a heartbeat and how often kept placements are swept. Kind sets them low; an install
// that sets neither keeps one minute each.
func TestInventoryAndReapIntervals(t *testing.T) {
	cfg, err := configFrom(t, map[string]string{EnvDriver: driver.NameKubernetes})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InventoryInterval != time.Minute || cfg.ReapInterval != time.Minute {
		t.Errorf("defaults = inventory %s, reap %s, want 1m each", cfg.InventoryInterval, cfg.ReapInterval)
	}
	for _, tc := range []struct {
		env string
		got func(Config) time.Duration
	}{
		{EnvInventoryInterval, func(c Config) time.Duration { return c.InventoryInterval }},
		{EnvReapInterval, func(c Config) time.Duration { return c.ReapInterval }},
	} {
		t.Run(tc.env, func(t *testing.T) {
			cfg, err := configFrom(t, map[string]string{EnvDriver: driver.NameKubernetes, tc.env: "10s"})
			if err != nil || tc.got(cfg) != 10*time.Second {
				t.Errorf("%s=10s = %s, %v", tc.env, tc.got(cfg), err)
			}
			for _, blank := range []string{"", "   ", "\t"} {
				cfg, err := configFrom(t, map[string]string{EnvDriver: driver.NameKubernetes, tc.env: blank})
				if err != nil || tc.got(cfg) != time.Minute {
					t.Errorf("%s=%q = %s, %v, want the 1m default", tc.env, blank, tc.got(cfg), err)
				}
			}
			for _, bad := range []string{"soon", "0s", "-1s"} {
				if _, err := configFrom(t, map[string]string{EnvDriver: driver.NameKubernetes, tc.env: bad}); err == nil || !strings.Contains(err.Error(), tc.env) {
					t.Errorf("%s=%q = %v, want refused naming %s", tc.env, bad, err, tc.env)
				}
			}
		})
	}
}

// The container driver gets the pull policy and the launcher's registry credential from the
// launcher's environment, so the always default reaches it (one-launcher spec, A3, OD18) and its
// job images are pulled with the credential (OD19).
func TestContainerDriverFromEnvCarriesPullPolicyAndCredential(t *testing.T) {
	path := writeFile(t, t.TempDir()+"/ghcr.json", `{"auths":{"ghcr.io":{"auth":"`+basicAuth("bot", "s3cret")+`"}}}`)
	env := map[string]string{EnvDriver: driver.NameContainer, EnvDockerConfig: path}
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
	if c.JobImagePull != driver.PullAlways {
		t.Errorf("container pull policy = %q, want the always default", c.JobImagePull)
	}
	if a := c.Auth.For(DefaultTofuImage); a == nil || a.Username != "bot" {
		t.Errorf("container credential for %s = %+v, want bot", DefaultTofuImage, a)
	}
}

// A Kubernetes launcher warns at start about an identitytoken entry, which a job pod's pull Secret
// cannot carry (one-launcher spec, A3); a container launcher sends it to the engine and does not.
func TestKubernetesWarnsAboutIdentityTokens(t *testing.T) {
	path := writeFile(t, t.TempDir()+"/token.json", `{"auths":{"registry.example":{"identitytoken":"tok"}}}`)
	for drv, want := range map[string]bool{driver.NameKubernetes: true, driver.NameContainer: false} {
		cfg, err := configFrom(t, map[string]string{EnvDriver: drv, EnvDockerConfig: path})
		if err != nil {
			t.Fatal(err)
		}
		got := slices.ContainsFunc(cfg.Warnings, func(w string) bool {
			return strings.Contains(w, "identitytoken") && strings.Contains(w, "registry.example")
		})
		if got != want {
			t.Errorf("%s: identitytoken warning = %v, want %v (%v)", drv, got, want, cfg.Warnings)
		}
	}
}

// startedLauncher runs the executable's own startup function, buildLauncher, against api with d as
// the driver and logf as the client's log, so a value ConfigFromEnv parses reaches the launcher the
// way the executable's does.
func startedLauncher(t *testing.T, api *fakeAPI, d driver.Driver, env map[string]string, logf func(string, ...any)) *Launcher {
	t.Helper()
	full := map[string]string{
		EnvServer: api.srv.URL, EnvToken: "swa-agent", EnvPoolID: "pool-1", EnvHealthAddr: "off", EnvDriver: driver.NameKubernetes,
	}
	maps.Copy(full, env)
	factory := func(context.Context, Binary, Config, func(string) string) (driver.Driver, error) { return d, nil }
	if logf == nil {
		logf = func(string, ...any) {}
	}
	l, err := buildLauncher(t.Context(), bothExecutors(), envOf(full), nil, factory, logf, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	l.goos = "linux"
	return l
}

// runUntilStopped runs l and returns a function that stops it and waits for Run to return.
func runUntilStopped(t *testing.T, l *Launcher) func() {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = l.Run(ctx)
	}()
	return func() {
		stop()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatal("Run did not return within fifteen seconds")
		}
	}
}

// A launcher started from its environment sweeps at the RUNNER_REAP_INTERVAL it was given. The
// bounds are loose, so a delayed process still passes, and do not overlap, so a fixed interval
// fails at one of the two settings: no cadence gives at least 15 periodic sweeps in two seconds at
// 40ms and at most 12 at 200ms.
func TestStartedLauncherSweepsAtTheConfiguredReapInterval(t *testing.T) {
	for _, c := range []struct {
		interval string
		min, max int
	}{{"40ms", 15, 1 << 30}, {"200ms", 1, 12}} {
		t.Run(c.interval, func(t *testing.T) {
			t.Parallel()
			api := newFakeAPI(t, runnercontract.CapabilityJobToken, runnercontract.CapabilityClaimRecovery)
			d := newFakeDriver("kubernetes")
			l := startedLauncher(t, api, d, map[string]string{EnvReapInterval: c.interval}, nil)
			own := func() int {
				d.mu.Lock()
				defer d.mu.Unlock()
				n := 0
				for _, sel := range d.reaped {
					if sel[driver.LabelRunner] == "runner-1" {
						n++
					}
				}
				return n
			}
			stop := runUntilStopped(t, l)
			eventually(t, "the startup sweep", func() bool { return own() >= 1 })
			first := own()
			time.Sleep(2 * time.Second)
			periodic := own() - first
			stop()
			if periodic < c.min || periodic > c.max {
				t.Errorf("RUNNER_REAP_INTERVAL=%s: %d periodic sweeps in 2s after the startup sweep, want %d to %d", c.interval, periodic, c.min, c.max)
			}
		})
	}
}

// slowReadDriver takes 300ms over each successful collection and records when each began and ended.
type slowReadDriver struct {
	*fakeDriver
	mu           sync.Mutex
	starts, ends []time.Time
}

func (d *slowReadDriver) Inventory(context.Context) ([]runnercontract.InventoryEntry, error) {
	start := time.Now()
	time.Sleep(300 * time.Millisecond)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.starts, d.ends = append(d.starts, start), append(d.ends, time.Now())
	return nil, nil
}

func (d *slowReadDriver) Ensure(context.Context, []runnercontract.EnsureObject) error { return nil }

// A launcher started from its environment, whose heartbeats are answered at once, leaves at least
// RUNNER_INVENTORY_INTERVAL from the end of one successful collection to the start of the next,
// and does collect again: at least three more reads within 15s at 1s and at least one more within
// 12s at 4s. The 1s window allows for the first loop, which sends a plain heartbeat and waits one
// 10s idle beat while startup recovery is not ready. A fixed interval fails one of the two settings.
func TestStartedLauncherCollectsAtTheConfiguredInventoryInterval(t *testing.T) {
	for _, c := range []struct {
		interval time.Duration
		window   time.Duration
		minMore  int
	}{{time.Second, 15 * time.Second, 3}, {4 * time.Second, 12 * time.Second, 1}} {
		t.Run(c.interval.String(), func(t *testing.T) {
			t.Parallel()
			api := newFakeAPI(t, runnercontract.CapabilityJobToken, runnercontract.CapabilityClaimRecovery)
			d := &slowReadDriver{fakeDriver: newFakeDriver("kubernetes")}
			l := startedLauncher(t, api, d, map[string]string{EnvInventoryInterval: c.interval.String()}, nil)
			stop := runUntilStopped(t, l)
			eventually(t, "the first collection", func() bool {
				d.mu.Lock()
				defer d.mu.Unlock()
				return len(d.ends) >= 1
			})
			time.Sleep(c.window)
			stop()
			d.mu.Lock()
			starts, ends := slices.Clone(d.starts), slices.Clone(d.ends)
			d.mu.Unlock()
			more := 0
			for _, s := range starts[1:] {
				if s.Sub(ends[0]) <= c.window {
					more++
				}
			}
			if more < c.minMore {
				t.Errorf("RUNNER_INVENTORY_INTERVAL=%s: %d more collections within %s of the first, want at least %d", c.interval, more, c.window, c.minMore)
			}
			for i := 1; i < len(starts); i++ {
				if gap := starts[i].Sub(ends[i-1]); gap < c.interval {
					t.Errorf("RUNNER_INVENTORY_INTERVAL=%s: collection %d began %s after the previous one ended, want at least %s", c.interval, i, gap, c.interval)
				}
			}
		})
	}
}
