// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/michielvha/stackweaver/backend/internal/launcher/cache"
	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// Config is a launcher's configuration. The binaries fill it from their environment (ConfigFromEnv).
type Config struct {
	ServerURL         string
	RegistrationToken string
	// PoolID is the customer pool the launcher serves; empty for a hosted launcher.
	PoolID string
	// Hosted makes this a hosted launcher, serving every hosted pool of its group (D9).
	Hosted      bool
	HostedGroup string
	// Strict is hosted.strict: no in-process placement, no host-identity network.
	Strict bool
	// Single runs one job, then deregisters and returns: the ephemeral launcher an operator
	// starts per job with a just-in-time registration token (--single, RUNNER_SINGLE; D11).
	Single   bool
	Name     string
	Hostname string
	// Engines are what the launcher's placements can run: "tofu", "ansible" or both (AC78).
	Engines []string
	// Capacity is how many jobs run at once; registration reports it so the reservation never
	// hands the launcher more.
	Capacity    int
	NetworkMode string
	// JobImages maps each engine to its job image, which a job that names no run image runs in
	// (one-launcher spec, A3).
	JobImages map[string]string
	// JobImagePull is when the drivers pull a job image (RUNNER_JOB_IMAGE_PULL).
	JobImagePull driver.PullPolicy
	// FirewallImage is the image the container driver writes the jobs network's rules from.
	FirewallImage string
	// PlacementTimeout is how long a Kubernetes job pod may fail to pull an image before its job
	// fails (RUNNER_PLACEMENT_TIMEOUT).
	PlacementTimeout time.Duration
	// JobImageAuth are the launcher's own registry credentials (RUNNER_DOCKER_CONFIG): the drivers
	// pull job images, the executor's copy source and the firewall image with them, never a run
	// image.
	JobImageAuth driver.RegistryCredentials
	// Deprecations are the settings this configuration uses that are going away, logged at start.
	Deprecations []string
	// Warnings are what the configuration asks for that the launcher cannot do, logged at start.
	Warnings []string
	// ExecutorCommand is the executor's command inside a job image, per engine.
	ExecutorCommand map[string][]string
	Labels          []string
	AgentVersion    string
	TofuVersion     string
	AnsibleVersion  string
	// HeartbeatWait is the long-poll wait; zero means DefaultHeartbeatWait.
	HeartbeatWait time.Duration
	// IdleBeat is the heartbeat interval while the launcher cannot take work (full, or waiting for
	// an API capability); zero means ten seconds.
	IdleBeat time.Duration
	// StatusInterval is how often a supervised job's status is read; zero means five seconds.
	StatusInterval time.Duration
	// DrainGrace bounds how long in-flight jobs may run once the launcher is told to stop.
	DrainGrace time.Duration
	// KillGrace is how long a killed executor gets to report before it is stopped hard.
	KillGrace time.Duration
	// HealthAddr is where /healthz and /readyz are served; empty serves none.
	HealthAddr string
	// ReapInterval is how often the failed placements kept for inspection are swept once their
	// keep_failed_jobs_for has passed; zero means a minute.
	ReapInterval time.Duration
	// CacheDir is the launcher-owned node-local copy of the caches of the organizations whose jobs
	// it runs (#933, D7, decision 19); empty switches the read-through off.
	CacheDir string
	// CacheVolume is the named volume CacheDir lives on, which the container driver mounts the
	// job's organization's subpath of when the launcher itself runs in a container.
	CacheVolume string
	// CacheLimit bounds CacheDir in bytes (cache.local_size_limit); zero means no bound.
	CacheLimit int64
	// VerifyTransparencyLog is cache.verify_transparency_log: the populator verifies tofu releases
	// against the transparency log. Off is refused under hosted.strict (AC76).
	VerifyTransparencyLog bool
	// Cosign is the cosign binary the populator verifies tofu releases with; empty means PATH's.
	Cosign string
}

func (c Config) reapInterval() time.Duration {
	if c.ReapInterval > 0 {
		return c.ReapInterval
	}
	return time.Minute
}

func (c Config) idleBeat() time.Duration {
	if c.IdleBeat > 0 {
		return c.IdleBeat
	}
	return 10 * time.Second
}

func (c Config) statusInterval() time.Duration {
	if c.StatusInterval > 0 {
		return c.StatusInterval
	}
	return 5 * time.Second
}

func (c Config) killGrace() time.Duration {
	if c.KillGrace > 0 {
		return c.KillGrace
	}
	return 60 * time.Second
}

// Launcher registers once, holds one heartbeat, claims what it is offered and has its driver place
// an executor for each claim (#933, D3).
type Launcher struct {
	cfg    Config
	client *Client
	driver driver.Driver
	poller *Poller
	goos   string
	// onHealthListen, when set, is called with the health listener's address once it listens.
	onHealthListen func(net.Addr)

	// apiReady requires job-token and claim-recovery support: without them the launcher
	// places nothing, because the only alternative would be handing the executor the runner token
	// (AC72).
	apiReady   atomic.Bool
	registered atomic.Bool
	lastBeat   atomic.Int64
	draining   atomic.Bool
	// singlePlaced records successful placement, so a lost candidate does not consume single mode.
	singlePlaced atomic.Bool
	startupReady atomic.Bool
	// recoveryCancel bounds startup discovery at hard drain. It is assigned before the loop starts.
	recoveryCancel context.CancelFunc

	// readThrough is the node-local cache, nil when CacheDir is empty.
	readThrough *cache.ReadThrough
	inContainer bool
	// verifiers builds the populator's verifiers for a mirror block; tests replace it.
	verifiers  func(*runnercontract.Mirror) map[runnercontract.CacheKind]cache.Verifier
	promotions chan promotion
	promoting  map[string]struct{}

	// lostPeers are the lost runners of this launcher's pool the API last named (ReapRunners),
	// whose leftover placements the reap sweep stops and removes (AC67).
	lostPeers atomic.Pointer[[]string]

	mu       sync.Mutex
	inflight map[string]*supervised
	// Startup inventories retain the last successful evidence while a later read is unavailable.
	activeClaims      []runnercontract.ActiveClaim
	claimsAvailable   bool
	startupPlacements []driver.Placement
	// lastInventory is when the job namespace's inventory was last reported.
	lastInventory time.Time
	done          chan struct{} // a job ended: capacity is free
	wg            sync.WaitGroup
}

// New builds a launcher placing through d.
func New(cfg Config, d driver.Driver) *Launcher {
	if cfg.Capacity <= 0 {
		cfg.Capacity = 1
	}
	c := NewClient(cfg.ServerURL, cfg.RegistrationToken)
	l := &Launcher{
		cfg: cfg, client: c, driver: d, goos: runtime.GOOS,
		poller:      &Poller{Client: c, Wait: cfg.HeartbeatWait},
		inflight:    map[string]*supervised{},
		done:        make(chan struct{}, 1),
		inContainer: runsInAContainer(),
		promotions:  make(chan promotion, 64),
		promoting:   map[string]struct{}{},
	}
	l.verifiers = l.defaultVerifiers
	if cfg.CacheDir != "" {
		l.readThrough = &cache.ReadThrough{Dir: cfg.CacheDir, Source: clientSource{c}, Limit: cfg.CacheLimit, Logf: c.logf}
	}
	return l
}

// Client is the launcher's control-plane client.
func (l *Launcher) Client() *Client { return l.client }

// Run runs the launcher until ctx ends, then drains: it stops claiming, lets in-flight jobs finish
// for up to DrainGrace, and deregisters (AC18).
func (l *Launcher) Run(ctx context.Context) error {
	if err := CheckPlatform(l.goos); err != nil {
		return err
	}
	if err := l.checkConfig(); err != nil {
		return err
	}
	if l.cfg.HealthAddr != "" {
		stop, err := l.serveHealth(ctx, l.cfg.HealthAddr)
		if err != nil {
			return err
		}
		defer stop()
	}

	resp, err := l.client.Register(ctx, l.registerRequest())
	if err != nil {
		return fmt.Errorf("registering: %w", err)
	}
	l.registered.Store(true)
	l.lastBeat.Store(time.Now().UnixNano())
	l.noteAPICapabilities(resp.Capabilities)
	l.client.logf("Launcher registered with ID %s (driver %s, capacity %d)", resp.RunnerID, l.driver.Name(), l.cfg.Capacity)

	l.noteActiveClaims(resp.ActiveClaims)
	recoveryCtx, stopRecovery := context.WithCancel(context.WithoutCancel(ctx))
	l.recoveryCancel = stopRecovery
	defer stopRecovery()
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		l.reconcile(recoveryCtx)
	}()
	go l.reapKept(ctx)
	go l.populate(ctx)
	l.loop(ctx)
	return l.drain(ctx)
}

// Reaper is a driver that keeps failed placements for inspection (keep_failed_jobs_for) and
// removes them once their window has passed.
type Reaper interface {
	Reap(ctx context.Context, selector map[string]string) error
}

// reapKept sweeps this launcher's kept placements at start and then every ReapInterval (AC25).
func (l *Launcher) reapKept(ctx context.Context) {
	r, ok := l.driver.(Reaper)
	if !ok {
		return
	}
	sel := map[string]string{driver.LabelManaged: "true", driver.LabelRunner: l.client.RunnerID()}
	t := time.NewTicker(l.cfg.reapInterval())
	defer t.Stop()
	for {
		if err := r.Reap(ctx, sel); err != nil && ctx.Err() == nil {
			l.client.logf("Sweeping kept placements: %v", err)
		}
		l.reapLostPeers(ctx, r)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// reapLostPeers stops and removes what the lost runners the API named left on this driver: a
// replaced launcher's running pods or containers, whose work the API has already settled and
// whose tokens it revoked (#933, D3, AC67). Only placements labelled with a named runner are
// touched, so a live peer's work never is; the API names a runner only once the monitor has
// marked it offline.
func (l *Launcher) reapLostPeers(ctx context.Context, r Reaper) {
	peers := l.lostPeers.Load()
	if peers == nil {
		return
	}
	self := l.client.RunnerID()
	for _, peer := range *peers {
		if peer == "" || peer == self {
			continue
		}
		sel := map[string]string{driver.LabelManaged: "true", driver.LabelRunner: peer}
		placed, err := l.driver.List(ctx, sel)
		if err != nil {
			if ctx.Err() == nil {
				l.client.logf("Listing the placements of lost launcher %s: %v", peer, err)
			}
			continue
		}
		for _, p := range placed {
			if p.Ended || p.Labels[driver.LabelRunner] != peer {
				continue
			}
			l.client.logf("Stopping %s, left running by lost launcher %s", p.ID, peer)
			if err := l.driver.Kill(ctx, p, l.cfg.KillGrace); err != nil && ctx.Err() == nil {
				l.client.logf("Stopping %s of lost launcher %s: %v", p.ID, peer, err)
			}
		}
		if err := r.Reap(ctx, sel); err != nil && ctx.Err() == nil {
			l.client.logf("Sweeping the placements of lost launcher %s: %v", peer, err)
		}
	}
}

// checkConfig refuses what the deployment forbids: hosted.strict allows neither the in-process
// driver nor the host-identity network (AC72, AC22).
func (l *Launcher) checkConfig() error {
	if l.cfg.Strict && l.driver.Name() == driver.NameInProcess {
		return errors.New("strict hosted isolation (STACKWEAVER_HOSTED_STRICT) forbids the in-process driver: a hosted launcher places every job in a container or pod")
	}
	if l.cfg.Strict && l.cfg.NetworkMode == NetworkHostIdentity {
		return errors.New("strict hosted isolation (STACKWEAVER_HOSTED_STRICT) forbids the host-identity network mode")
	}
	if err := (cache.PopulatorConfig{Strict: l.cfg.Strict, VerifyTransparencyLog: l.cfg.VerifyTransparencyLog}).Check(); err != nil {
		return fmt.Errorf("%w: %w", errStrictOffline, err)
	}
	if !l.cfg.Hosted && l.cfg.PoolID == "" {
		return errors.New("a launcher of an agent pool needs the pool it serves (RUNNER_AGENT_POOL_ID)")
	}
	return nil
}

// noteAPICapabilities records whether the API supports original-claim recovery. Older APIs are
// waited for, never worked around.
func (l *Launcher) noteAPICapabilities(caps []string) {
	ready := slices.Contains(caps, runnercontract.CapabilityJobToken) && slices.Contains(caps, runnercontract.CapabilityClaimRecovery)
	if !ready && l.apiReady.Load() == ready {
		l.client.logf("The API does not support job tokens and claim recovery yet: this launcher waits before placing work")
	}
	l.apiReady.Store(ready)
}

func (l *Launcher) registerRequest() runnercontract.RegisterRequest {
	hostname := l.cfg.Hostname
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	name := l.cfg.Name
	if name == "" {
		name = hostname
	}
	verification := runnercontract.CacheVerificationTransparencyLog
	if !l.cfg.VerifyTransparencyLog {
		verification = runnercontract.CacheVerificationOffline
	}
	var localLimit int64
	if l.cfg.CacheDir != "" {
		localLimit = l.cfg.CacheLimit
	}
	return runnercontract.RegisterRequest{
		AgentPoolID:         l.cfg.PoolID,
		Name:                name,
		Hostname:            hostname,
		OSType:              runtime.GOOS,
		AgentVersion:        l.cfg.AgentVersion,
		TofuVersion:         l.cfg.TofuVersion,
		AnsibleVersion:      l.cfg.AnsibleVersion,
		MaxConcurrentJobs:   l.cfg.Capacity,
		Labels:              l.cfg.Labels,
		Engines:             l.cfg.Engines,
		Driver:              l.driver.Name(),
		NetworkMode:         l.cfg.NetworkMode,
		Hosted:              l.cfg.Hosted,
		HostedGroup:         l.cfg.HostedGroup,
		HostedStrict:        l.cfg.Strict,
		CacheVerification:   verification,
		LocalCache:          l.cfg.CacheDir != "",
		LocalCacheSizeLimit: localLimit,
	}
}

// running is the number of in-flight jobs.
func (l *Launcher) running() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.startupReady.Load() {
		return len(l.inflight)
	}
	known := make(map[string]struct{}, len(l.inflight)+len(l.activeClaims)+len(l.startupPlacements))
	for key := range l.inflight {
		known[key] = struct{}{}
	}
	for _, claim := range l.activeClaims {
		known[claim.JobID] = struct{}{}
	}
	for _, placement := range l.startupPlacements {
		key := placement.Labels[driver.LabelJob]
		if key == "" {
			key = "placement:" + placement.ID
		}
		known[key] = struct{}{}
	}
	return len(known)
}

// loop heartbeats and claims until ctx ends.
func (l *Launcher) loop(ctx context.Context) {
	for ctx.Err() == nil {
		current := l.running()
		if l.cfg.Single && l.singlePlaced.Load() && current == 0 {
			return
		}
		free := max(0, l.cfg.Capacity-current)
		if l.cfg.Single {
			free = min(free, 1)
			if current > 0 || l.singlePlaced.Load() {
				free = 0
			}
		}
		if !l.apiReady.Load() || !l.startupReady.Load() {
			free = 0
		}
		state := runnercontract.HeartbeatRequest{Status: "online", CurrentJobs: current, AvailableCapacity: free}
		if state.CurrentJobs > 0 {
			state.Status = "busy"
		}
		state.Inventory = l.inventoryDue(ctx)
		var resp runnercontract.HeartbeatResponse
		var err error
		if free > 0 {
			resp, err = l.poller.Poll(ctx, state)
		} else {
			// Full, or waiting for the API: a plain heartbeat keeps the runner online, then wait
			// for a job to end or the next beat.
			resp, err = l.client.Heartbeat(ctx, state, 0)
		}
		if err == nil {
			l.lastBeat.Store(time.Now().UnixNano())
			l.noteActiveClaims(resp.ActiveClaims)
			if resp.Capabilities != nil {
				l.noteAPICapabilities(resp.Capabilities)
			}
			// Every answer replaces the list: a runner the API no longer names (back online under
			// the same identity) is never reaped from a stale one.
			peers := slices.Clone(resp.ReapRunners)
			l.lostPeers.Store(&peers)
			if resp.Drain {
				l.client.logf("The API asked this launcher to drain")
				return
			}
			l.ensureObjects(ctx, resp.Ensure)
			l.queuePromotions(resp.CachePromotions, resp.Mirror)
			for _, offer := range resp.PendingJobs {
				if free <= 0 || ctx.Err() != nil || !l.apiReady.Load() || !l.startupReady.Load() {
					break
				}
				if l.claim(ctx, offer) {
					free--
				}
			}
		}
		if free > 0 && err == nil && len(resp.PendingJobs) > 0 {
			continue
		}
		if free <= 0 || err != nil {
			select {
			case <-ctx.Done():
			case <-l.done:
			case <-time.After(l.cfg.idleBeat()):
			}
		}
	}
}

// claim reserves capacity before sending any request. One worker retains the original identity
// through acquisition, placement and supervision, while the heartbeat loop continues.
func (l *Launcher) claim(parent context.Context, offer runnercontract.PendingJob) bool {
	if parent.Err() != nil || !validOffer(offer) {
		return false
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	s := &supervised{jobID: offer.JobID, cancel: cancel, expected: runnercontract.JobTokenRequest{
		Attempt: offer.Attempt, Phase: offer.Phase, RunType: offer.RunType,
	}}
	l.mu.Lock()
	if len(l.inflight) >= l.cfg.Capacity || l.draining.Load() {
		l.mu.Unlock()
		cancel()
		return false
	}
	for _, existing := range l.inflight {
		if existing.jobID == offer.JobID {
			l.mu.Unlock()
			cancel()
			return false
		}
	}
	l.inflight[offer.JobID] = s
	l.wg.Add(1)
	l.mu.Unlock()
	go func() {
		defer l.wg.Done()
		defer cancel()
		defer l.finished(offer.JobID)
		l.acquire(ctx, offer, s)
	}()
	return true
}

func validOffer(offer runnercontract.PendingJob) bool {
	if offer.JobID == "" || offer.Attempt <= 0 {
		return false
	}
	switch offer.JobType {
	case "tofu_run":
		switch offer.RunType {
		case "plan", "plan-only", "plan-destroy":
			return offer.Phase == "plan"
		case "apply", "apply-destroy":
			return offer.Phase == "apply"
		}
	case "ansible_job", runnercontract.JobTypeWorkItem:
		return offer.Phase == "" && offer.RunType == ""
	}
	return false
}

func (l *Launcher) acquire(ctx context.Context, offer runnercontract.PendingJob, s *supervised) {
	kind := l.engineOf(offer.JobType)
	image := l.cfg.JobImages[kind]
	// Every job runs its engine's job image, the platform default, until run images resolve a job's
	// own image and level with its offer (D4).
	report := &runnercontract.Placement{Driver: l.driver.Name(), Image: image, ImageLevel: runnercontract.ImageLevelPlatform}
	if err := l.client.Start(ctx, offer, report); err != nil {
		if !errors.Is(err, ErrClaimLost) && !terminalRefusal(err) {
			l.client.logf("Claiming job %s failed: %v", offer.JobID, err)
			l.deliverFailure(ctx, s, "the launcher could not confirm startup: "+err.Error())
		}
		return
	}
	tok, err := l.client.JobToken(ctx, offer.JobID, s.expected)
	if err != nil {
		l.client.logf("Job %s claimed but no job token was issued: %v", offer.JobID, err)
		if !terminalRefusal(err) {
			l.deliverFailure(ctx, s, "the launcher could not obtain a job token: "+err.Error())
		}
		return
	}
	s.token = tok.Token
	// A token's existing expiry bounds pending delivery as well as execution. Recovery never
	// extends the server-issued lifetime, and hard drain can still cancel this worker sooner.
	if !tok.ExpiresAt.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, tok.ExpiresAt)
		defer cancel()
	}
	req := driver.Request{
		JobID: offer.JobID, Attempt: tok.Attempt, Kind: kind, RunType: offer.RunType, Image: image,
		Command:     l.cfg.ExecutorCommand[kind],
		NetworkMode: l.cfg.NetworkMode,
		Server:      l.cfg.ServerURL, RunnerID: l.client.RunnerID(), Token: tok.Token,
		Labels: l.labels(offer.JobID, kind, offer.RunType, tok.Attempt),
	}
	if id := tok.Identity; id != nil {
		req.Identity = &driver.Identity{Mechanism: id.Mechanism, ServiceAccount: id.ServiceAccount, Annotations: id.Annotations, PerJob: id.PerJob}
	}
	if p := tok.Pool; p != nil {
		req.Resources = driver.Resources{CPULimit: p.CPULimit, MemoryLimit: p.MemoryLimit, Required: p.LimitsRequired}
		req.KeepFailedFor = time.Duration(p.KeepFailedForSeconds) * time.Second
	}
	if tok.Pool == nil || !tok.Pool.NoLocalReadthrough {
		req.Cache, req.CacheRemoteExhausted = l.prepareCache(ctx, offer.JobID, tok.Cache)
	}
	req.Env = ExecutorEnv(os.Environ(), ExecutorInvocation{
		ServerURL: req.Server, JobID: req.JobID, Token: req.Token, RunType: req.RunType, RunnerID: req.RunnerID,
		CacheRemoteExhausted: req.CacheRemoteExhausted,
	})
	// The job's run image, by its level rule, pulled with the image's registry credential (#933,
	// D6); none runs the platform image.
	if img := tok.Image; img != nil {
		req.Image, req.Digest = img.Ref, img.Digest
		if img.PullSecret != "" && l.driver.Name() == driver.NameKubernetes {
			// The pool's long-lived pull Secret for the registry: the pod names it, and the
			// launcher reads no credential (D8).
			req.PullSecret = img.PullSecret
		} else if img.PullCredential {
			cred, err := l.client.PullCredential(ctx, img.ID)
			if err != nil {
				l.client.logf("Job %s: reading the pull credential of its run image failed: %v", offer.JobID, err)
				l.deliverFailure(ctx, s, "the launcher could not read the pull credential of the run image "+img.Ref+": "+err.Error())
				return
			}
			req.PullAuth = &driver.RegistryAuth{Registry: cred.Registry, Username: cred.Username, Password: cred.Password}
		}
	}
	if ctx.Err() != nil {
		return
	}
	p, err := l.driver.Place(ctx, req)
	if err != nil {
		l.client.logf("Placing job %s failed: %v", offer.JobID, err)
		l.deliverFailure(ctx, s, "the job could not be placed: "+err.Error())
		return
	}
	s.placement = p
	if ctx.Err() != nil {
		_ = l.driver.Kill(context.WithoutCancel(ctx), p, l.cfg.killGrace())
		_ = l.driver.Remove(context.WithoutCancel(ctx), p)
		return
	}
	l.singlePlaced.Store(true)
	l.watch(ctx, s, time.Duration(tok.TimeoutSeconds)*time.Second)
}

// labels are what a placement carries so a restarted launcher, or another of the pool, finds it.
func (l *Launcher) labels(jobID, kind, runType string, attempt int) map[string]string {
	pool := l.cfg.PoolID
	if l.cfg.Hosted {
		pool = "hosted:" + l.cfg.HostedGroup
	}
	return map[string]string{
		driver.LabelManaged: "true",
		driver.LabelPool:    pool,
		driver.LabelRunner:  l.client.RunnerID(),
		driver.LabelJob:     jobID,
		driver.LabelAttempt: strconv.Itoa(attempt),
		driver.LabelKind:    kind,
		driver.LabelRunType: runType,
	}
}

// engineOf maps an offer's job type to the engine whose executor runs it. A work item runs in the
// launcher's own executor, which carries the sync executor (#933, D2): the Ansible one when the
// launcher has it, since only it runs the inventory kinds.
func (l *Launcher) engineOf(jobType string) string {
	switch jobType {
	case "tofu_run":
		return "tofu"
	case runnercontract.JobTypeWorkItem:
		if len(l.cfg.Engines) > 0 && !slices.Contains(l.cfg.Engines, "ansible") {
			return l.cfg.Engines[0]
		}
	}
	return "ansible"
}

// drain waits for in-flight jobs up to DrainGrace, heartbeating so the launcher is not marked
// offline meanwhile, stops what is left, and deregisters.
func (l *Launcher) drain(ctx context.Context) error {
	l.draining.Store(true)
	// The drain runs after the launcher was told to stop, so it does not inherit that cancellation.
	bg := context.WithoutCancel(ctx)
	deadline := time.Now().Add(l.cfg.DrainGrace)
	finished := make(chan struct{})
	go func() { l.wg.Wait(); close(finished) }()
	ticker := time.NewTicker(l.cfg.idleBeat())
	defer ticker.Stop()
	for waiting := true; waiting; {
		if !time.Now().Before(deadline) {
			l.stopAll(bg)
			<-finished
			break
		}
		if n := l.running(); n > 0 {
			l.client.logf("Draining: waiting for %d job(s) to finish", n)
		}
		select {
		case <-finished:
			waiting = false
		case <-ticker.C:
			beatCtx, cancel := context.WithDeadline(bg, deadline)
			_, _ = l.client.Heartbeat(beatCtx, runnercontract.HeartbeatRequest{Status: "busy", CurrentJobs: l.running()}, 0)
			cancel()
		case <-time.After(time.Until(deadline)):
			l.stopAll(bg)
			<-finished
			waiting = false
		}
	}
	if err := l.client.Deregister(bg); err != nil {
		return fmt.Errorf("deregistering: %w", err)
	}
	l.client.logf("Launcher deregistered")
	return nil
}

// ObjectManager is a driver that keeps long-lived objects in its job namespace (the Kubernetes
// driver, D8): it reports what is there and who manages it, and creates what its pool needs.
type ObjectManager interface {
	Inventory(ctx context.Context) ([]runnercontract.InventoryEntry, error)
	Ensure(ctx context.Context, objs []runnercontract.EnsureObject) error
}

// inventoryInterval is how often the job namespace's inventory rides on a heartbeat (D8).
const inventoryInterval = time.Minute

// inventoryDue returns the job namespace's inventory when a report is due, else nil.
func (l *Launcher) inventoryDue(ctx context.Context) []runnercontract.InventoryEntry {
	om, ok := l.driver.(ObjectManager)
	if !ok || time.Since(l.lastInventory) < inventoryInterval {
		return nil
	}
	inv, err := om.Inventory(ctx)
	if err != nil {
		l.client.logf("Reading the job namespace's inventory: %v", err)
		return nil
	}
	l.lastInventory = time.Now()
	if inv == nil {
		inv = []runnercontract.InventoryEntry{}
	}
	return inv
}

// ensureObjects creates the objects the API asked for; the driver's pre-write check leaves any
// that something else manages alone.
func (l *Launcher) ensureObjects(ctx context.Context, objs []runnercontract.EnsureObject) {
	om, ok := l.driver.(ObjectManager)
	if !ok || len(objs) == 0 {
		return
	}
	if err := om.Ensure(ctx, objs); err != nil {
		l.client.logf("Creating the pool's objects: %v", err)
	}
}
