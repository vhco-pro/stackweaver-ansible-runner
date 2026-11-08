// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ExecutorMountPath is where an injected executor is mounted in a custom image (D4).
const ExecutorMountPath = "/stackweaver"

// Container places each executor in a fresh container through the Docker Engine API, on a Docker
// or a Podman socket (D4). Every job container is hardened: never privileged, all capabilities
// dropped, no-new-privileges, the engine's default seccomp profile, a read-only root filesystem
// with a writable /tmp, no socket and no host path beyond the mounts the launcher names, and a
// non-root user unless the pool allows root (AC71).
type Container struct {
	// Socket is the engine's unix socket.
	Socket string
	// JobImages maps each engine ("tofu", "ansible") to its job image (one-launcher spec, A3): a
	// job naming no image runs its engine's, and a custom run image gets the executor copied out
	// of it.
	JobImages map[string]string
	// ExecutorPath is the executor binary inside the job images, for a request that carries no
	// Command; the driver runs `<ExecutorPath> executor` there and injects a copy of it into any
	// other image.
	ExecutorPath string
	// JobImagePull is when a job's image and the job image its executor is copied from are pulled
	// (RUNNER_JOB_IMAGE_PULL); the zero value pulls only a missing image. The firewall writer's
	// image is pulled only when missing, whatever it says (OD28).
	JobImagePull PullPolicy
	// PullStallTimeout bounds how long a pull may go without progress from the engine before it is
	// given up as a registry that cannot be reached; zero means DefaultPullStallTimeout.
	PullStallTimeout time.Duration
	// Network is the jobs network (EnsureNetwork); empty means the engine's default bridge.
	Network string
	// User is the uid:gid job containers run as when set; otherwise the image's own user, which
	// must not be root unless AllowRoot.
	User      string
	AllowRoot bool
	// Runtime is the engine runtime for job containers (runsc for gVisor, kata), container.runtime.
	Runtime string
	// KeepFailedFor is the keep window of a job whose pool sets none (RUNNER_KEEP_FAILED_FOR); a
	// job's own window (Request.KeepFailedFor) wins. It keeps a failed job's container this long;
	// Reap removes it afterwards.
	KeepFailedFor time.Duration
	// ExtraHosts are host:ip entries for every job container (host.docker.internal:host-gateway
	// for an API reached through the host).
	ExtraHosts []string
	// HostIdentity is the launcher's host-identity network mode (D5): jobs may reach the node's
	// metadata service, so the jobs network leaves it open and the executor's guard is off.
	HostIdentity bool

	e *engine
	// fw is what EnsureNetwork wrote to the host firewall; nil where it wrote nothing.
	fw *jobsFirewall
}

func (d *Container) engine() *engine {
	if d.e == nil {
		d.e = newEngine(d.Socket)
	}
	return d.e
}

// jobEnv is a job container's environment before the placement's own variables: the request's,
// the writable temporary home, the placement's report, and the network guard, which makes the
// executor refuse a network where a metadata address is reachable. A host-identity launcher leaves
// the guard out, because there the node's identity is what jobs are meant to reach (D5). The
// report carries the limits report and the placement's warnings, each a line the executor writes
// into the job's output before the job starts.
func (d *Container) jobEnv(req Request, applied, warnings []string) []string {
	env := append(append([]string{}, req.Env...), "HOME=/tmp", "TMPDIR=/tmp", "STACKWEAVER_PLACED=1")
	if !d.HostIdentity {
		env = append(env, "STACKWEAVER_NETWORK_GUARD=1")
	}
	lines := appendNonEmpty(nil, LimitsReport(req.Resources, applied))
	lines = append(lines, warnings...)
	if len(lines) > 0 {
		// The executor prefixes the report's first line with "Stackweaver: "; the rest get it here.
		env = append(env, EnvLimitsReport+"="+strings.Join(lines, "\nStackweaver: "))
	}
	return env
}

func appendNonEmpty(list []string, s string) []string {
	if s != "" {
		return append(list, s)
	}
	return list
}

// Name is "container".
func (d *Container) Name() string { return NameContainer }

// Capabilities: a container isolates the filesystem and, on the jobs network, the network; limits
// are enforced where the engine can (cgroup v2), and a restarted launcher re-attaches.
func (d *Container) Capabilities() Capabilities {
	return Capabilities{EnforcesLimits: true, IsolatesNetwork: d.Network != "", IsolatesFilesystem: true, SupportsReattach: true, SupportsRuntime: true}
}

// Ping reports whether the engine answers.
func (d *Container) Ping(ctx context.Context) error { return d.engine().ping(ctx) }

// containerName is a job container's name: unique per attempt, so a retry never collides with a
// kept failed container of the attempt before.
func containerName(req Request) string {
	return "sw-job-" + sanitizeName(req.JobID) + "-" + strconv.Itoa(req.Attempt)
}

func sanitizeName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, s)
}

// Place creates and starts the job's container.
func (d *Container) Place(ctx context.Context, req Request) (Placement, error) {
	if err := d.closeNewNetworks(ctx); err != nil {
		return Placement{}, err
	}
	e := d.engine()
	jobImage, err := d.jobImage(req.Kind)
	if err != nil {
		return Placement{}, err
	}
	image := cmp.Or(req.Image, jobImage)
	// Only an engine's default job image, and below the job image a run image's executor is
	// copied from, may run from the cache when the registry cannot be reached; a run image or an
	// execution environment never does (OD27).
	var warnings []string
	warning, err := d.ensureImage(ctx, image, d.JobImagePull, req.PullAuth, image == jobImage)
	if err != nil {
		return Placement{}, err
	}
	warnings = appendNonEmpty(warnings, warning)
	if image != jobImage {
		warning, err := d.ensureImage(ctx, jobImage, d.JobImagePull, nil, true)
		if err != nil {
			return Placement{}, err
		}
		warnings = appendNonEmpty(warnings, warning)
	}
	user, err := d.jobUser(ctx, image)
	if err != nil {
		return Placement{}, err
	}
	applied, nano, mem, err := d.limits(ctx, req.Resources)
	if err != nil {
		return Placement{}, err
	}

	labels := maps.Clone(req.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[LabelManaged] = "true"
	if keep := firstPositive(req.KeepFailedFor, d.KeepFailedFor); keep > 0 {
		labels[labelKeepFailedFor] = strconv.Itoa(int(keep.Seconds()))
	}
	pids := int64(4096)
	cfg := containerConfig{
		Image:  image,
		Env:    append(d.jobEnv(req, applied, warnings), d.placementEnv(ctx, image)...),
		Labels: labels,
		User:   user,
		HostConfig: hostConfig{
			Tmpfs:          map[string]string{"/tmp": "rw,exec,nosuid,nodev"},
			NetworkMode:    d.Network,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges:true"},
			ReadonlyRootfs: true,
			NanoCPUs:       nano,
			Memory:         mem,
			PidsLimit:      &pids,
			Runtime:        firstNonEmpty(req.Runtime, d.Runtime),
			ExtraHosts:     d.ExtraHosts,
		},
	}
	for _, m := range req.Mounts {
		cfg.HostConfig.Mounts = append(cfg.HostConfig.Mounts, mountSpec{Type: "bind", Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	// The launcher's node-local cache, the job's organization's part only, read-only (decision 19).
	if c := req.Cache; c != nil {
		switch {
		case c.Volume != "" && c.Subpath != "":
			cfg.HostConfig.Mounts = append(cfg.HostConfig.Mounts, mountSpec{Type: "volume", Source: c.Volume, Target: CacheMountPath, ReadOnly: true, VolumeOptions: &volumeOptions{Subpath: c.Subpath}})
			cfg.Env = append(cfg.Env, "STACKWEAVER_CACHE_DIR="+CacheMountPath)
		case c.Dir != "" && !strings.Contains(c.Dir, ":"):
			cfg.HostConfig.Binds = append(cfg.HostConfig.Binds, cacheBind(c.Dir))
			cfg.Env = append(cfg.Env, "STACKWEAVER_CACHE_DIR="+CacheMountPath)
		case c.Dir != "":
			// A path a bind string cannot carry is mounted without the label.
			cfg.HostConfig.Mounts = append(cfg.HostConfig.Mounts, mountSpec{Type: "bind", Source: c.Dir, Target: CacheMountPath, ReadOnly: true})
			cfg.Env = append(cfg.Env, "STACKWEAVER_CACHE_DIR="+CacheMountPath)
		}
	}
	command := d.executorCommand(req)
	if image == jobImage {
		cfg.Entrypoint = command[:1]
		cfg.Cmd = command[1:]
	} else {
		// A custom image: the executor comes from its engine's job image through a per-job volume,
		// and the entrypoint is always the driver's, never the image's (D4).
		vol, err := d.injectExecutor(ctx, req, jobImage, command[0])
		if err != nil {
			return Placement{}, err
		}
		labels[labelExecutorVolume] = vol
		cfg.HostConfig.Mounts = append(cfg.HostConfig.Mounts, mountSpec{Type: "volume", Source: vol, Target: ExecutorMountPath, ReadOnly: true})
		cfg.Entrypoint = []string{ExecutorMountPath + "/stackweaver-executor"}
		cfg.Cmd = command[1:]
	}

	name := containerName(req)
	id, err := e.create(ctx, name, cfg)
	if err != nil {
		return Placement{}, fmt.Errorf("creating the job container: %w", err)
	}
	if c := req.Cache; c != nil && c.Volume != "" && c.Subpath != "" {
		if id, err = d.confineCacheVolume(ctx, id, name, cfg, c); err != nil {
			return Placement{}, err
		}
	}
	if err := e.start(ctx, id); err != nil {
		_ = e.remove(context.WithoutCancel(ctx), id)
		return Placement{}, fmt.Errorf("starting the job container: %w", err)
	}
	return Placement{ID: id, Labels: labels, Applied: applied}, nil
}

// cacheBind binds dir read-only at the cache path with the shared container label: an engine that
// confines its containers with SELinux (rootless Podman, or a Docker daemon with SELinux on) keeps
// a bound directory's host label otherwise, which a job cannot read, and the shared label lets
// every job of the organization read it at once. An engine without SELinux ignores it.
func cacheBind(dir string) string { return dir + ":" + CacheMountPath + ":ro,z" }

// withoutCache is cfg with no cache mount and no STACKWEAVER_CACHE_DIR.
func withoutCache(cfg containerConfig) containerConfig {
	cfg.HostConfig.Mounts = slices.DeleteFunc(slices.Clone(cfg.HostConfig.Mounts), func(m mountSpec) bool { return m.Target == CacheMountPath })
	cfg.HostConfig.Binds = slices.DeleteFunc(slices.Clone(cfg.HostConfig.Binds), func(b string) bool { return strings.Contains(b, ":"+CacheMountPath+":") })
	cfg.Env = slices.DeleteFunc(slices.Clone(cfg.Env), func(kv string) bool { return strings.HasPrefix(kv, "STACKWEAVER_CACHE_DIR=") })
	return cfg
}

// confineCacheVolume makes sure the created container id sees only its organization's part of the
// cache volume, and returns the container to start. An engine that ignored the volume subpath
// would show the job every organization's cache, so such a container is never started. Podman's
// Docker-compatible API takes no subpath: there the organization's directory is bound read-only
// from the local volume's path on the engine's host, and the bind is checked the same way. When
// neither can be trusted, the job gets no cache mount and reads its entries through its blob route.
func (d *Container) confineCacheVolume(ctx context.Context, id, name string, cfg containerConfig, c *CacheMount) (string, error) {
	e := d.engine()
	if st, err := e.inspect(ctx, id); err == nil && st.subpathHonoured(CacheMountPath, c.Subpath) {
		return id, nil
	}
	_ = e.remove(context.WithoutCancel(ctx), id)
	bare := withoutCache(cfg)
	if dir, ok := e.volumeSubdir(ctx, c.Volume, c.Subpath); ok {
		bound := bare
		bound.HostConfig.Binds = append(slices.Clone(bare.HostConfig.Binds), cacheBind(dir))
		bound.Env = append(slices.Clone(bare.Env), "STACKWEAVER_CACHE_DIR="+CacheMountPath)
		id, err := e.create(ctx, name, bound)
		if err != nil {
			return "", fmt.Errorf("creating the job container: %w", err)
		}
		if st, err := e.inspect(ctx, id); err == nil && st.boundReadOnly(CacheMountPath, dir) {
			return id, nil
		}
		_ = e.remove(context.WithoutCancel(ctx), id)
	}
	id, err := e.create(ctx, name, bare)
	if err != nil {
		return "", fmt.Errorf("creating the job container: %w", err)
	}
	return id, nil
}

// placementEnv tells the job's executor the digest of its image and the host it runs on, which it
// reports as the job's placement (D4, AC77). A lookup that fails leaves the member out.
func (d *Container) placementEnv(ctx context.Context, image string) []string {
	e := d.engine()
	var env []string
	if digest, err := e.imageDigest(ctx, image); err == nil && digest != "" {
		env = append(env, "STACKWEAVER_IMAGE_DIGEST="+digest)
	}
	if info, err := e.info(ctx); err == nil && info.Name != "" {
		env = append(env, "STACKWEAVER_NODE_NAME="+info.Name)
	}
	return env
}

// labelKeepFailedFor carries a job container's keep_failed_jobs_for, in seconds, so Remove and Reap
// apply the window of the pool the job came from (AC25).
const labelKeepFailedFor = "sh.stackweaver.keep-failed-for"

// keepFailedFor is the keep window a job container was placed with.
func keepFailedFor(labels map[string]string) time.Duration {
	n, err := strconv.Atoi(labels[labelKeepFailedFor])
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

func firstPositive(ds ...time.Duration) time.Duration {
	for _, d := range ds {
		if d > 0 {
			return d
		}
	}
	return 0
}

// labelExecutorVolume names the per-job volume an injected executor lives in, so Remove deletes it.
const labelExecutorVolume = "sh.stackweaver.executor-volume"

// DefaultPullStallTimeout is how long a pull may go without progress when PullStallTimeout is
// unset.
const DefaultPullStallTimeout = 60 * time.Second

// ensureImage makes image present on the engine under policy: always pulls it, never refuses an
// absent one, and missing (or no policy) pulls only an absent one. With fallback, a pull that
// could not reach the registry runs the copy the engine already holds and returns a warning for
// the job (OD21); a refusal never falls back (OD27).
func (d *Container) ensureImage(ctx context.Context, image string, policy PullPolicy, auth *RegistryAuth, fallback bool) (warning string, err error) {
	e := d.engine()
	switch policy {
	case PullAlways:
	case PullNever:
		if !e.imageExists(ctx, image) {
			return "", fmt.Errorf("the job image %s is not on this engine, and the pull policy is never", image)
		}
		return "", nil
	case PullMissing:
		if e.imageExists(ctx, image) {
			return "", nil
		}
	default:
		if e.imageExists(ctx, image) {
			return "", nil
		}
	}
	err = e.pullBounded(ctx, image, auth, cmp.Or(d.PullStallTimeout, DefaultPullStallTimeout))
	if err == nil {
		return "", nil
	}
	if fallback && isTransportPullError(err) && e.imageExists(ctx, image) {
		return fmt.Sprintf("the registry could not be reached to pull %s (%v); the job runs the copy cached on this host", image, err), nil
	}
	return "", fmt.Errorf("pulling the job image %s: %w", image, err)
}

// jobImage is the job image of an engine.
func (d *Container) jobImage(kind string) (string, error) {
	image := d.JobImages[kind]
	if image == "" {
		return "", fmt.Errorf("this launcher has no job image for the %q engine", kind)
	}
	return image, nil
}

// executorCommand is the executor's command line in a job image: the request's, which the
// launcher sets per engine, else `<ExecutorPath> executor`.
func (d *Container) executorCommand(req Request) []string {
	if len(req.Command) > 0 {
		return req.Command
	}
	return []string{d.ExecutorPath, "executor"}
}

// injectExecutor copies the executor at path out of the job image source into a fresh volume.
func (d *Container) injectExecutor(ctx context.Context, req Request, source, path string) (string, error) {
	e := d.engine()
	vol := containerName(req) + "-executor"
	if err := e.createVolume(ctx, vol, map[string]string{LabelManaged: "true", LabelJob: req.JobID}); err != nil {
		return "", fmt.Errorf("creating the executor volume: %w", err)
	}
	// The copy runs as root because a fresh volume belongs to root; it runs no user code and has
	// no network.
	id, err := e.create(ctx, containerName(req)+"-inject", containerConfig{
		Image:      source,
		Entrypoint: []string{"/bin/sh", "-c"},
		Cmd:        []string{fmt.Sprintf("cp %s %s/stackweaver-executor && chmod 0555 %s/stackweaver-executor", path, ExecutorMountPath, ExecutorMountPath)},
		User:       "0",
		Labels:     map[string]string{LabelManaged: "true", LabelJob: req.JobID},
		HostConfig: hostConfig{
			Mounts:      []mountSpec{{Type: "volume", Source: vol, Target: ExecutorMountPath}},
			NetworkMode: "none",
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges:true"},
		},
	})
	if err != nil {
		_ = e.removeVolume(ctx, vol)
		return "", fmt.Errorf("creating the executor copy: %w", err)
	}
	defer func() { _ = e.remove(context.WithoutCancel(ctx), id) }()
	if err := e.start(ctx, id); err != nil {
		_ = e.removeVolume(ctx, vol)
		return "", fmt.Errorf("starting the executor copy: %w", err)
	}
	if code, err := e.wait(ctx, id); err != nil || code != 0 {
		_ = e.removeVolume(ctx, vol)
		return "", fmt.Errorf("copying the executor into the job image failed (exit %d): %w", code, err)
	}
	return vol, nil
}

// jobUser is the user a job container runs as: the configured one, or the image's own, which must
// not be root unless the pool allows it.
func (d *Container) jobUser(ctx context.Context, image string) (string, error) {
	if d.User != "" {
		return d.User, nil
	}
	var img struct {
		Config struct {
			User string
		}
	}
	if err := d.engine().do(ctx, http.MethodGet, "/images/"+image+"/json", nil, nil, &img); err != nil {
		return "", fmt.Errorf("inspecting the job image %s: %w", image, err)
	}
	u := strings.TrimSpace(img.Config.User)
	if (u == "" || u == "0" || u == "root" || strings.HasPrefix(u, "0:") || strings.HasPrefix(u, "root:")) && !d.AllowRoot {
		return "", fmt.Errorf("the job image %s runs as root; set a non-root USER in the image or allow root on the pool (refused under hosted.strict)", image)
	}
	return u, nil
}

// limits converts the requested resources into engine limits and says which were applied. A limit
// the engine cannot enforce (cgroup v1 rootless) is dropped and named, or refuses the placement when
// the pool requires it (AC24).
func (d *Container) limits(ctx context.Context, r Resources) (applied []string, nanoCPUs, memory int64, err error) {
	if r.CPULimit == "" && r.MemoryLimit == "" {
		return nil, 0, 0, nil
	}
	info, err := d.engine().info(ctx)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("reading the engine's capabilities: %w", err)
	}
	enforce := info.CgroupVersion == "2" || info.MemoryLimit
	if !enforce {
		if r.Required {
			return nil, 0, 0, errors.New("the pool requires enforced limits and this engine cannot enforce them (cgroup v2 with delegation is needed)")
		}
		return nil, 0, 0, nil
	}
	if r.CPULimit != "" {
		cpus, perr := strconv.ParseFloat(strings.TrimSuffix(r.CPULimit, "m"), 64)
		if perr != nil {
			return nil, 0, 0, fmt.Errorf("parsing the CPU limit %q: %w", r.CPULimit, perr)
		}
		if strings.HasSuffix(r.CPULimit, "m") {
			cpus /= 1000
		}
		nanoCPUs = int64(cpus * 1e9)
		applied = append(applied, "cpu="+r.CPULimit)
	}
	if r.MemoryLimit != "" {
		memory, err = parseBytes(r.MemoryLimit)
		if err != nil {
			return nil, 0, 0, err
		}
		applied = append(applied, "memory="+r.MemoryLimit)
	}
	return applied, nanoCPUs, memory, nil
}

// parseBytes reads a Kubernetes-style quantity (512Mi, 2Gi, 1G, 1048576).
func parseBytes(s string) (int64, error) {
	units := []struct {
		suffix string
		mult   int64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}}
	for _, u := range units {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			v, err := strconv.ParseFloat(n, 64)
			if err != nil {
				return 0, fmt.Errorf("parsing the memory limit %q: %w", s, err)
			}
			return int64(v * float64(u.mult)), nil
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing the memory limit %q: %w", s, err)
	}
	return v, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Wait blocks until the container stops.
func (d *Container) Wait(ctx context.Context, p Placement) (Exit, error) {
	code, err := d.engine().wait(ctx, p.ID)
	if err != nil {
		if isNotFound(err) {
			return Exit{}, ErrNotFound
		}
		return Exit{}, err
	}
	exit := Exit{Code: code}
	if st, err := d.engine().inspect(ctx, p.ID); err == nil && st.State.OOMKilled {
		exit.Reason = "OOMKilled"
	}
	return exit, nil
}

// Kill stops the container: SIGTERM, which the executor turns into a cancellation it reports, then
// SIGKILL after grace.
func (d *Container) Kill(ctx context.Context, p Placement, grace time.Duration) error {
	err := d.engine().stop(ctx, p.ID, grace)
	if isNotFound(err) {
		return nil
	}
	return err
}

// Remove deletes an ended container and its executor volume, except a failed one kept for
// inspection on a pool with keep_failed_jobs_for, which Reap removes later (AC25).
func (d *Container) Remove(ctx context.Context, p Placement) error {
	e := d.engine()
	st, err := e.inspect(ctx, p.ID)
	if isNotFound(err) {
		return nil
	}
	if err == nil && !p.Canceled && keepFailedFor(st.Config.Labels) > 0 && st.State.ExitCode != 0 {
		return nil
	}
	return d.removeContainer(ctx, p.ID, st.Config.Labels)
}

func (d *Container) removeContainer(ctx context.Context, id string, labels map[string]string) error {
	e := d.engine()
	if err := e.remove(ctx, id); err != nil && !isNotFound(err) {
		return err
	}
	if vol := labels[labelExecutorVolume]; vol != "" {
		_ = e.removeVolume(ctx, vol)
	}
	return nil
}

// Reap removes ended job containers whose keep_failed_jobs_for has passed, and every ended one when
// the pool keeps none.
func (d *Container) Reap(ctx context.Context, selector map[string]string) error {
	e := d.engine()
	sel := maps.Clone(selector)
	if sel == nil {
		sel = map[string]string{}
	}
	sel[LabelManaged] = "true"
	all, err := e.list(ctx, sel)
	if err != nil {
		return err
	}
	for _, c := range all {
		if c.State == "running" || c.State == "created" || c.State == "restarting" {
			continue
		}
		var st struct {
			State struct {
				FinishedAt time.Time
			}
		}
		if err := e.do(ctx, http.MethodGet, "/containers/"+c.ID+"/json", nil, nil, &st); err != nil {
			continue
		}
		if time.Since(st.State.FinishedAt) >= keepFailedFor(c.Labels) {
			_ = d.removeContainer(ctx, c.ID, c.Labels)
		}
	}
	return nil
}

// List returns the job containers carrying every label in selector.
func (d *Container) List(ctx context.Context, selector map[string]string) ([]Placement, error) {
	found, err := d.engine().list(ctx, selector)
	if err != nil {
		return nil, err
	}
	out := make([]Placement, 0, len(found))
	for _, c := range found {
		if c.Labels[labelRole] != "" {
			continue // a firewall writer or executor copy, not a job
		}
		out = append(out, Placement{ID: c.ID, Labels: c.Labels, Ended: c.State != "running" && c.State != "created" && c.State != "restarting"})
	}
	return out, nil
}

// Logs returns the container's output stream (the engine's multiplexed log format).
func (d *Container) Logs(ctx context.Context, p Placement) (io.ReadCloser, error) {
	return d.engine().logs(ctx, p.ID)
}

// hostConfigFirewall is the firewall writer's host configuration: the host network and NET_ADMIN,
// nothing else.
func hostConfigFirewall() hostConfig {
	return hostConfig{NetworkMode: "host", CapDrop: []string{"ALL"}, CapAdd: []string{"NET_ADMIN", "NET_RAW"}}
}
