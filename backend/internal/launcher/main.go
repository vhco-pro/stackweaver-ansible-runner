// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/michielvha/logger"
	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
	"github.com/michielvha/stackweaver/core/envfile"
	"github.com/michielvha/stackweaver/core/settings"
)

// The environment a launcher is configured from (#933, D3, D4, D9, D12).
const (
	EnvServer       = "STACKWEAVER_SERVER"
	EnvToken        = "STACKWEAVER_TOKEN"
	EnvHosted       = "STACKWEAVER_HOSTED"
	EnvHostedSecret = "STACKWEAVER_HOSTED_SECRET"
	EnvHostedGroup  = "STACKWEAVER_HOSTED_GROUP"
	EnvHostedStrict = "STACKWEAVER_HOSTED_STRICT"
	EnvPoolID       = "RUNNER_AGENT_POOL_ID"
	EnvName         = "RUNNER_NAME"
	EnvLabels       = "RUNNER_LABELS"
	EnvCapacity     = "RUNNER_MAX_CONCURRENT_JOBS"
	EnvDriver       = "RUNNER_DRIVER"
	EnvNetworkMode  = "RUNNER_NETWORK_MODE"
	EnvNetwork      = "RUNNER_NETWORK"
	// EnvImage is the one image of a launcher that serves its own image's engine. Deprecated in
	// favour of the job image variables: alone it keeps its meaning for the compatibility window
	// (one-launcher spec, A3, OD6).
	EnvImage = "RUNNER_IMAGE"
	// EnvTofuImage and EnvAnsibleImage are each engine's job image: a job that names no run image
	// runs in it, and a custom run image gets the executor from it (A3).
	EnvTofuImage    = "RUNNER_TOFU_IMAGE"
	EnvAnsibleImage = "RUNNER_ANSIBLE_IMAGE"
	// EnvJobImagePull is when job images are pulled: always, missing or never (OD18). Unset, it is
	// always, or no policy (today's behaviour) for a launcher configured with RUNNER_IMAGE alone
	// (OD29).
	EnvJobImagePull = "RUNNER_JOB_IMAGE_PULL"
	// EnvPlacementTimeout is how long a Kubernetes job pod may fail to pull an image before its
	// job fails (OD21).
	EnvPlacementTimeout = "RUNNER_PLACEMENT_TIMEOUT"
	EnvSocket           = "RUNNER_DOCKER_SOCKET"
	EnvJobUser          = "RUNNER_JOB_USER"
	EnvAllowRoot        = "RUNNER_ALLOW_ROOT"
	EnvContainerRuntime = "RUNNER_CONTAINER_RUNTIME"
	EnvExtraHosts       = "RUNNER_EXTRA_HOSTS"
	EnvKeepFailedFor    = "RUNNER_KEEP_FAILED_FOR"
	// EnvDenyCIDRs adds ranges no job may reach, beside the metadata ranges and the engine's other
	// networks: the platform's database or storage when they live off the engine.
	EnvDenyCIDRs = "RUNNER_DENY_CIDRS"
	// EnvAllowTCP lists, comma separated, the host:port endpoints (or http and https URLs) a job
	// reaches beside the API even inside the denied ranges: an air-gapped install's mirrors on the
	// same engine or cluster (#933, D2, D5).
	EnvAllowTCP      = "RUNNER_ALLOW_TCP"
	EnvFirewallImage = "RUNNER_FIREWALL_IMAGE"
	EnvHealthAddr    = "RUNNER_HEALTH_ADDR"
	// The Kubernetes driver's settings (#933, D4, D5, D11).
	EnvJobNamespace      = "RUNNER_JOB_NAMESPACE"
	EnvPodTemplate       = "RUNNER_POD_TEMPLATE_CONFIGMAP"
	EnvRuntimeClass      = "RUNNER_RUNTIME_CLASS"
	EnvJobUID            = "RUNNER_JOB_UID"
	EnvClusterCIDRs      = "RUNNER_CLUSTER_CIDRS"
	EnvAPIEndpointForJob = "RUNNER_JOB_API_ENDPOINT"
	EnvManageNetPolicy   = "RUNNER_MANAGE_NETWORK_POLICY"
	EnvDrainGrace        = "RUNNER_DRAIN_GRACE"
	// EnvSingle runs one job and exits, as --single does (D11).
	EnvSingle = "RUNNER_SINGLE"

	// The node-local read-through cache (#933, D7, decision 19).
	EnvCacheDir    = "RUNNER_CACHE_DIR"
	EnvCacheVolume = "RUNNER_CACHE_VOLUME"
	EnvCacheLimit  = "RUNNER_CACHE_LOCAL_SIZE_LIMIT"
	// EnvVerifyTransparencyLog is cache.verify_transparency_log (default true), refused off under
	// hosted.strict (AC76).
	EnvVerifyTransparencyLog = "CACHE_VERIFY_TRANSPARENCY_LOG"
	EnvCosign                = "RUNNER_COSIGN"
)

// DefaultCacheLimit is cache.local_size_limit when RUNNER_CACHE_LOCAL_SIZE_LIMIT is unset.
const DefaultCacheLimit int64 = 20 << 30

// The job images a launcher's engines run in when the job image variables are unset: each
// published job image's latest, pulled on every placement (OD14).
const (
	DefaultTofuImage    = "ghcr.io/vhco-pro/stackweaver-opentofu-runner:latest"
	DefaultAnsibleImage = "ghcr.io/vhco-pro/stackweaver-ansible-runner:latest"
)

// ExecutorPath is where every job image installs the launcher binary, which a container or
// Kubernetes driver runs as `<ExecutorPath> executor` (A1, A5).
const ExecutorPath = "/usr/local/bin/stackweaver-launcher"

// DefaultHealthAddr is where /healthz and /readyz are served when RUNNER_HEALTH_ADDR is unset.
const DefaultHealthAddr = ":9090"

// Binary is what the launcher binary brings to its launcher: the executors that run each engine's
// jobs in the launcher's process.
type Binary struct {
	// Executors maps an engine ("tofu", "ansible") to the executor that runs its jobs in this
	// process (the in-process driver).
	Executors map[string]driver.ExecutorFunc
	// Version is reported at registration.
	Version string
}

// ConfigFromEnv reads a launcher's configuration through getenv.
func ConfigFromEnv(b Binary, getenv func(string) string) (Config, error) {
	cfg := Config{
		ServerURL:    strings.TrimRight(strings.TrimSpace(getenv(EnvServer)), "/"),
		Name:         strings.TrimSpace(getenv(EnvName)),
		NetworkMode:  firstOf(getenv(EnvNetworkMode), NetworkPlatform),
		AgentVersion: b.Version,
		HealthAddr:   firstOf(getenv(EnvHealthAddr), DefaultHealthAddr),
		Capacity:     1,
		DrainGrace:   5 * time.Minute,
		Single:       truthy(getenv(EnvSingle)),
	}
	if cfg.ServerURL == "" {
		return cfg, errors.New(EnvServer + " is required")
	}
	if cfg.HealthAddr == "off" {
		cfg.HealthAddr = ""
	}
	if v := strings.TrimSpace(getenv(EnvCapacity)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return cfg, fmt.Errorf("%s must be a positive number, not %q", EnvCapacity, v)
		}
		cfg.Capacity = n
	}
	if v := strings.TrimSpace(getenv(EnvDrainGrace)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("%s: %w", EnvDrainGrace, err)
		}
		cfg.DrainGrace = d
	}
	if err := cacheFromEnv(&cfg, getenv); err != nil {
		return cfg, err
	}
	if err := jobImagesFromEnv(&cfg, b, getenv); err != nil {
		return cfg, err
	}
	for _, l := range strings.Split(getenv(EnvLabels), ",") {
		if l = strings.TrimSpace(l); l != "" {
			cfg.Labels = append(cfg.Labels, l)
		}
	}
	if truthy(getenv(EnvHosted)) {
		cfg.Hosted = true
		cfg.RegistrationToken = strings.TrimSpace(getenv(EnvHostedSecret))
		cfg.HostedGroup = firstOf(getenv(EnvHostedGroup), "default")
		cfg.Strict = truthy(getenv(EnvHostedStrict))
		if cfg.RegistrationToken == "" {
			return cfg, errors.New("a hosted launcher needs " + EnvHostedSecret)
		}
	} else {
		cfg.RegistrationToken = strings.TrimSpace(getenv(EnvToken))
		cfg.PoolID = strings.TrimSpace(getenv(EnvPoolID))
		if cfg.RegistrationToken == "" {
			return cfg, errors.New("a launcher needs " + EnvToken + " (an agent token of its pool) or " + EnvHosted + "=1 with " + EnvHostedSecret)
		}
	}
	return cfg, nil
}

// jobImagesFromEnv reads the engines the launcher serves, each engine's job image, the pull policy
// and the firewall image (one-launcher spec, A2, A3). RUNNER_IMAGE alone keeps its meaning: the
// launcher runs in that image, which is its jobs' image too, and serves the engines whose
// toolchain it finds in its own process, on every driver.
func jobImagesFromEnv(cfg *Config, b Binary, getenv func(string) string) error {
	driverName := firstOf(getenv(EnvDriver), driver.NameInProcess)
	legacy := strings.TrimSpace(getenv(EnvImage))
	tofu, ansible := strings.TrimSpace(getenv(EnvTofuImage)), strings.TrimSpace(getenv(EnvAnsibleImage))
	legacyOnly := legacy != "" && tofu == "" && ansible == ""
	choice, err := chooseEngines(b, driverName, legacyOnly, getenv)
	if err != nil {
		return err
	}
	cfg.Engines = choice.engines
	// Only a toolchain that runs in this process is the one its jobs run on.
	if driverName == driver.NameInProcess {
		cfg.TofuVersion, cfg.AnsibleVersion = choice.versions[EngineTofu], choice.versions[EngineAnsible]
	}
	cfg.ExecutorCommand = executorCommands()
	cfg.JobImages = map[string]string{
		EngineTofu:    cmp.Or(tofu, legacy, DefaultTofuImage),
		EngineAnsible: cmp.Or(ansible, legacy, DefaultAnsibleImage),
	}
	switch {
	case legacy != "" && (tofu != "" || ansible != ""):
		cfg.Deprecations = append(cfg.Deprecations, fmt.Sprintf("%s is deprecated and ignored for each engine whose job image is set; set %s and %s instead", EnvImage, EnvTofuImage, EnvAnsibleImage))
	case legacy != "":
		cfg.Deprecations = append(cfg.Deprecations, fmt.Sprintf("%s is deprecated; set %s and %s, each engine's job image, instead", EnvImage, EnvTofuImage, EnvAnsibleImage))
	}
	switch v := driver.PullPolicy(strings.ToLower(strings.TrimSpace(getenv(EnvJobImagePull)))); v {
	case "":
		// The job image variables default to always (OD18). RUNNER_IMAGE alone sets no policy, so an
		// existing agent works unchanged: the container driver pulls when missing and a Kubernetes
		// pod keeps the cluster's default (OD29).
		cfg.JobImagePull = driver.PullAlways
		if legacyOnly {
			cfg.JobImagePull = ""
		}
	case driver.PullAlways, driver.PullMissing, driver.PullNever:
		cfg.JobImagePull = v
	default:
		return fmt.Errorf("%s=%q is not a pull policy (always, missing, never)", EnvJobImagePull, getenv(EnvJobImagePull))
	}
	cfg.FirewallImage = firstOf(getenv(EnvFirewallImage), cfg.JobImages[EngineTofu])
	cfg.PlacementTimeout = driver.DefaultPlacementTimeout
	if v := strings.TrimSpace(getenv(EnvPlacementTimeout)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return fmt.Errorf("%s=%q is not a positive duration", EnvPlacementTimeout, v)
		}
		cfg.PlacementTimeout = d
	}
	return nil
}

// executorCommands is the executor's command in each engine's job image: the one binary at its
// constant path.
func executorCommands() map[string][]string {
	return map[string][]string{
		EngineTofu:    {ExecutorPath, "executor"},
		EngineAnsible: {ExecutorPath, "executor"},
	}
}

// DriverFromEnv builds the driver RUNNER_DRIVER names: inprocess (the default) or container.
func DriverFromEnv(ctx context.Context, b Binary, cfg Config, getenv func(string) string) (driver.Driver, error) {
	switch name := firstOf(getenv(EnvDriver), driver.NameInProcess); name {
	case driver.NameInProcess:
		executors := map[string]driver.ExecutorFunc{}
		for _, e := range cfg.Engines {
			executors[e] = b.Executors[e]
		}
		return &driver.InProcess{JobImages: cfg.JobImages, Executors: executors}, nil
	case driver.NameContainer:
		if cfg.Strict && truthy(getenv(EnvAllowRoot)) {
			return nil, errors.New(EnvAllowRoot + " is refused under hosted.strict: a hosted job never runs as root")
		}
		d := &driver.Container{
			Socket:       firstOf(getenv(EnvSocket), "/var/run/docker.sock"),
			JobImages:    cfg.JobImages,
			JobImagePull: cfg.JobImagePull,
			ExecutorPath: ExecutorPath,
			User:         strings.TrimSpace(getenv(EnvJobUser)),
			AllowRoot:    truthy(getenv(EnvAllowRoot)),
			Runtime:      strings.TrimSpace(getenv(EnvContainerRuntime)),
			// Host-identity jobs reach the node's metadata service (D5); hosted.strict refuses the mode.
			HostIdentity: cfg.NetworkMode == NetworkHostIdentity,
		}
		for _, h := range strings.Split(getenv(EnvExtraHosts), ",") {
			if h = strings.TrimSpace(h); h != "" {
				d.ExtraHosts = append(d.ExtraHosts, h)
			}
		}
		if v := strings.TrimSpace(getenv(EnvKeepFailedFor)); v != "" {
			keep, err := time.ParseDuration(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", EnvKeepFailedFor, err)
			}
			d.KeepFailedFor = keep
		}
		if network := strings.TrimSpace(getenv(EnvNetwork)); network != "" {
			pol := driver.NetworkPolicy{Name: network, FirewallImage: cfg.FirewallImage, DenyOtherNetworks: true, AllowMetadata: d.HostIdentity}
			for _, c := range strings.Split(getenv(EnvDenyCIDRs), ",") {
				if c = strings.TrimSpace(c); c != "" {
					pol.Deny = append(pol.Deny, c)
				}
			}
			allow, err := allowedEndpoints(ctx, apiEndpoint(ctx, cfg.ServerURL), getenv(EnvAllowTCP))
			if err != nil {
				return nil, err
			}
			pol.AllowTCP = allow
			programmed, err := d.EnsureNetwork(ctx, pol)
			if err != nil {
				return nil, fmt.Errorf("preparing the jobs network: %w", err)
			}
			if !programmed {
				logger.Warnf("The jobs network %s is not firewalled on this engine (rootless): executors refuse to start where a metadata address is reachable", network)
			}
		}
		return d, nil
	case driver.NameKubernetes:
		o := driver.KubernetesOptions{
			Namespace: strings.TrimSpace(getenv(EnvJobNamespace)), JobImages: cfg.JobImages, ExecutorPath: ExecutorPath, JobImagePull: cfg.JobImagePull,
			PlacementTimeout:  cfg.PlacementTimeout,
			TemplateConfigMap: strings.TrimSpace(getenv(EnvPodTemplate)), RuntimeClass: strings.TrimSpace(getenv(EnvRuntimeClass)),
			HostIdentity:       cfg.NetworkMode == NetworkHostIdentity,
			LeaveNetworkPolicy: strings.EqualFold(strings.TrimSpace(getenv(EnvManageNetPolicy)), "false"),
			APIEndpoint:        firstOf(getenv(EnvAPIEndpointForJob), apiEndpoint(ctx, cfg.ServerURL)),
		}
		extra, err := allowedEndpoints(ctx, "", getenv(EnvAllowTCP))
		if err != nil {
			return nil, err
		}
		o.AllowEndpoints = extra
		if v := strings.TrimSpace(getenv(EnvJobUID)); v != "" {
			uid, err := strconv.ParseInt(v, 10, 64)
			if err != nil || uid <= 0 {
				return nil, fmt.Errorf("%s=%q is not a non-root uid", EnvJobUID, v)
			}
			o.RunAsUser = uid
		}
		for _, c := range strings.Split(getenv(EnvClusterCIDRs), ",") {
			if c = strings.TrimSpace(c); c != "" {
				o.ClusterCIDRs = append(o.ClusterCIDRs, c)
			}
		}
		if v := strings.TrimSpace(getenv(EnvKeepFailedFor)); v != "" {
			keep, err := time.ParseDuration(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", EnvKeepFailedFor, err)
			}
			o.KeepFailedFor = keep
		}
		return driver.NewKubernetes(ctx, o)
	default:
		return nil, fmt.Errorf("%s=%q is not a driver this launcher has (inprocess, container, kubernetes)", EnvDriver, name)
	}
}

// apiEndpoint is the API's address:port as a job reaches it, when the server URL names an IP.
func apiEndpoint(ctx context.Context, server string) string {
	u, err := url.Parse(server)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
	if err != nil || len(addrs) == 0 {
		return ""
	}
	return net.JoinHostPort(addrs[0].IP.String(), port)
}

// allowedEndpoints are the address:port pairs a job reaches inside the denied ranges: the API's
// (already resolved, "" when unknown), then each entry of raw (RUNNER_ALLOW_TCP), a host:port or
// an http or https URL, its name resolved now, because firewall rules and network policies take
// addresses.
func allowedEndpoints(ctx context.Context, api, raw string) ([]string, error) {
	var out []string
	if api != "" {
		out = append(out, api)
	}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		hostport := entry
		if strings.Contains(entry, "://") {
			u, err := url.Parse(entry)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
				return nil, fmt.Errorf("%s: %q is not an http or https URL", EnvAllowTCP, entry)
			}
			port := u.Port()
			if port == "" {
				port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
			}
			hostport = net.JoinHostPort(u.Hostname(), port)
		}
		host, port, err := net.SplitHostPort(hostport)
		if err != nil || host == "" {
			return nil, fmt.Errorf("%s: %q is not host:port", EnvAllowTCP, entry)
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%s: %q has no valid port", EnvAllowTCP, entry)
		}
		if ip := net.ParseIP(host); ip != nil {
			out = append(out, net.JoinHostPort(ip.String(), port))
			continue
		}
		lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
		addrs, err := net.DefaultResolver.LookupIPAddr(lookup, host)
		cancel()
		if err != nil || len(addrs) == 0 {
			return nil, fmt.Errorf("%s: resolving %q: %w", EnvAllowTCP, host, err)
		}
		for _, a := range addrs {
			out = append(out, net.JoinHostPort(a.IP.String(), port))
		}
	}
	return out, nil
}

// SingleFromArgs reports whether the command line asks for one job (--single).
func SingleFromArgs(args []string) bool {
	for _, a := range args[min(1, len(args)):] {
		if a == "--single" || a == "-single" {
			return true
		}
	}
	return false
}

// Main runs a runner binary as a launcher until SIGTERM or SIGINT, then drains, and returns the
// process exit code.
func Main(b Binary) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	// A Compose install generates the hosted registration secret into .env after Compose has
	// fixed this container's environment, so it is read from the file at start, and nothing else
	// of the file, which carries the platform's own secrets too (STACKWEAVER_ENV_FILE).
	if err := envfile.LoadOnlyFromEnvironment(EnvHostedSecret); err != nil {
		logger.Errorf("launcher: %v", err)
		return 1
	}
	cfg, err := ConfigFromEnv(b, os.Getenv)
	if err != nil {
		logger.Errorf("launcher: %v", err)
		return 1
	}
	for _, w := range cfg.Deprecations {
		logger.Warnf("launcher: %s", w)
	}
	cfg.Single = cfg.Single || SingleFromArgs(os.Args)
	d, err := DriverFromEnv(ctx, b, cfg, os.Getenv)
	if err != nil {
		logger.Errorf("launcher: %v", err)
		return 1
	}
	l := New(cfg, d)
	// What the launcher does and every retried failure go to its log.
	l.Client().Log = logger.Infof
	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Errorf("launcher: %v", err)
		return 1
	}
	return 0
}

func firstOf(v, fallback string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return fallback
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// falsy reports a setting turned off explicitly; unset is not.
func falsy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off":
		return true
	}
	return false
}

// cacheFromEnv reads the node-local cache's directory, volume and bound. The directory defaults to
// the user's cache directory, so the read-through is on unless RUNNER_CACHE_DIR=off.
func cacheFromEnv(cfg *Config, getenv func(string) string) error {
	dir := strings.TrimSpace(getenv(EnvCacheDir))
	switch dir {
	case "off":
		dir = ""
	case "":
		if base, err := os.UserCacheDir(); err == nil {
			dir = filepath.Join(base, "stackweaver", "launcher-cache")
		}
	}
	cfg.CacheDir = dir
	cfg.CacheVolume = strings.TrimSpace(getenv(EnvCacheVolume))
	cfg.VerifyTransparencyLog = !falsy(getenv(EnvVerifyTransparencyLog))
	cfg.Cosign = strings.TrimSpace(getenv(EnvCosign))
	cfg.CacheLimit = DefaultCacheLimit
	if v := strings.TrimSpace(getenv(EnvCacheLimit)); v != "" {
		n, err := settings.ParseSize(v)
		if err != nil {
			return fmt.Errorf("%s: %w", EnvCacheLimit, err)
		}
		cfg.CacheLimit = n
	}
	return nil
}
