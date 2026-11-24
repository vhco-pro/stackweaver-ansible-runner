// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// fakeAPI is the runner control plane a launcher talks to.
type fakeAPI struct {
	mu sync.Mutex
	// failHeartbeats makes the next n heartbeats fail after their inventory is taken in, as a
	// response lost on the way back would; inventoryReports counts every inventory taken in.
	failHeartbeats   int
	inventoryReports int
	capabilities     []string
	offers           []runnercontract.PendingJob  // offered once each
	activeClaims     []runnercontract.ActiveClaim // complete startup inventory, empty by default
	claims           map[string]runnercontract.PendingJob
	statuses         map[string]string
	started          []string
	placements       map[string]*runnercontract.Placement
	reported         map[string]*runnercontract.Placement // POST /output with a placement alone
	timeouts         map[string]int                       // a job's TimeoutSeconds in its token
	pools            map[string]*runnercontract.PoolSettings
	caches           map[string]*runnercontract.CacheDescriptor // a job's descriptor in its token
	images           map[string]*runnercontract.JobImage        // a job's run image in its token
	pullCreds        map[string]runnercontract.PullCredential   // GET /runner/images/:id/pull-credential
	cacheObjects     map[string][]byte                          // GET /runner/cache: org/kind/sha -> body
	cacheFetches     []string
	promotions       []runnercontract.CachePromotion // handed out once, with the next heartbeat
	uploads          []string                        // PUT /runner/cache: path?query and body
	drops            map[string]map[string]any
	ensure           []runnercontract.EnsureObject
	inventory        []runnercontract.InventoryEntry
	tokens           []string
	failed           map[string]string
	deregistered     bool
	drain            bool     // the heartbeat answers Drain, as after POST /runners/:id/actions/drain
	reapRunners      []string // the heartbeat names these lost runners of the pool for reaping
	heartbeats       int
	// onStart runs when a job is claimed (POST /start), before the claim is answered.
	onStart func(job string)
	srv     *httptest.Server
}

func newFakeAPI(t *testing.T, caps ...string) *fakeAPI {
	t.Helper()
	f := &fakeAPI{activeClaims: []runnercontract.ActiveClaim{}, capabilities: caps, statuses: map[string]string{}, failed: map[string]string{}}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) offer(jobs ...runnercontract.PendingJob) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claims == nil {
		f.claims = map[string]runnercontract.PendingJob{}
	}
	for _, job := range jobs {
		// Current API offers include the next immutable claim identity. Older test cases that
		// exercise ordinary placement use their first attempt and the default plan operation.
		if job.Attempt == 0 {
			job.Attempt = 1
		}
		if job.JobType == "tofu_run" {
			if job.RunType == "" {
				job.RunType = "plan"
			}
			if strings.HasPrefix(job.RunType, "apply") {
				job.Phase = "apply"
			} else {
				job.Phase = "plan"
			}
		}
		f.claims[job.JobID] = job
		f.offers = append(f.offers, job)
	}
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/v2/runner")
	writeJSON := func(v any) {
		if err := json.NewEncoder(w).Encode(v); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
	switch {
	case path == "/register":
		w.WriteHeader(http.StatusCreated)
		writeJSON(runnercontract.RegisterResponse{RunnerID: "runner-1", RunnerAPIKey: "swr-1", Capabilities: f.capabilities, ActiveClaims: f.activeClaims})
	case path == "/heartbeat":
		f.heartbeats++
		var req runnercontract.HeartbeatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		var out []runnercontract.PendingJob
		if req.AvailableCapacity > 0 {
			n := min(req.AvailableCapacity, len(f.offers))
			out, f.offers = f.offers[:n], f.offers[n:]
		}
		if req.Inventory != nil {
			f.inventory = req.Inventory
			f.inventoryReports++
		}
		if f.failHeartbeats > 0 {
			f.failHeartbeats--
			http.Error(w, "response lost", http.StatusBadGateway)
			return
		}
		resp := runnercontract.HeartbeatResponse{PendingJobs: out, Ensure: f.ensure, ReapRunners: f.reapRunners, ActiveClaims: f.activeClaims}
		if f.drain {
			// A draining runner is offered nothing (#933, D11).
			f.offers = append(out, f.offers...)
			resp = runnercontract.HeartbeatResponse{Drain: true, ActiveClaims: f.activeClaims}
		}
		if len(f.promotions) > 0 {
			resp.CachePromotions, f.promotions = f.promotions, nil
			resp.Mirror = &runnercontract.Mirror{TofuReleasesURL: "https://releases.mirror.test"}
		}
		writeJSON(resp)
	case strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/pull-credential"):
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer swj-") {
			http.Error(w, "a job token may not use this endpoint", http.StatusForbidden)
			return
		}
		cred, ok := f.pullCreds[strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/pull-credential")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(cred)
	case strings.HasPrefix(path, "/cache/promotions/") && strings.HasSuffix(path, "/drop"):
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if f.drops == nil {
			f.drops = map[string]map[string]any{}
		}
		f.drops[strings.TrimSuffix(strings.TrimPrefix(path, "/cache/promotions/"), "/drop")] = body
		writeJSON(map[string]string{"status": "dropped"})
	case strings.HasPrefix(path, "/cache/") && r.Method == http.MethodPut:
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer swj-") {
			http.Error(w, "a job token may not use this endpoint", http.StatusForbidden)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.uploads = append(f.uploads, path+"?"+r.URL.RawQuery+" "+string(body))
		w.WriteHeader(http.StatusCreated)
	case strings.HasPrefix(path, "/cache/") && r.Method == http.MethodGet:
		// The launcher's read of an entry, under its runner token, never a job token.
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer swj-") {
			http.Error(w, "a job token may not use this endpoint", http.StatusForbidden)
			return
		}
		parts := strings.Split(strings.TrimPrefix(path, "/cache/"), "/")
		key := r.URL.Query().Get("organization") + "/" + strings.Join(parts, "/")
		f.cacheFetches = append(f.cacheFetches, key)
		body, ok := f.cacheObjects[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	case path == "/deregister":
		f.deregistered = true
		writeJSON(map[string]string{"status": "ok"})
	case strings.HasSuffix(path, "/start"):
		f.started = append(f.started, jobOf(path))
		if f.onStart != nil {
			f.onStart(jobOf(path))
		}
		var req runnercontract.JobStartRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if f.placements == nil {
			f.placements = map[string]*runnercontract.Placement{}
		}
		f.placements[jobOf(path)] = req.Placement
		claim := f.claims[jobOf(path)]
		if req.Attempt != claim.Attempt || req.Phase != claim.Phase || req.RunType != claim.RunType {
			http.Error(w, "start must name the offered claim", http.StatusBadRequest)
			return
		}
		writeJSON(map[string]any{"meta": runnercontract.JobStartResponse{Status: "started", Attempt: claim.Attempt, Phase: claim.Phase, RunType: claim.RunType}})
	case strings.HasSuffix(path, "/output"):
		if r.Header.Get("Authorization") != "Bearer swj-"+jobOf(path) {
			http.Error(w, "a placement is reported with the job's token", http.StatusForbidden)
			return
		}
		var req runnercontract.JobOutputRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if f.reported == nil {
			f.reported = map[string]*runnercontract.Placement{}
		}
		f.reported[jobOf(path)] = req.Placement
		writeJSON(map[string]string{"status": "received"})
	case strings.HasSuffix(path, "/token"):
		claim, fixed := f.claims[jobOf(path)]
		if fixed {
			var req runnercontract.JobTokenRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Attempt != claim.Attempt || req.Phase != claim.Phase || req.RunType != claim.RunType {
				http.Error(w, "token must name the original claim", http.StatusBadRequest)
				return
			}
		} else {
			claim.Attempt = 1
		}
		if f.statuses[jobOf(path)] == "applied" {
			w.WriteHeader(http.StatusForbidden)
			writeJSON(map[string]any{"errors": []map[string]string{{"code": runnercontract.ErrorCodeJobTerminal}}})
			return
		}
		f.tokens = append(f.tokens, jobOf(path))
		writeJSON(runnercontract.JobToken{Token: "swj-" + jobOf(path), JobID: jobOf(path), Attempt: claim.Attempt, Phase: claim.Phase, RunType: claim.RunType, ContractVersion: runnercontract.Version, TimeoutSeconds: f.timeouts[jobOf(path)], Pool: f.pools[jobOf(path)], Cache: f.caches[jobOf(path)], Image: f.images[jobOf(path)]})
	case strings.HasSuffix(path, "/status"):
		status := f.statuses[jobOf(path)]
		if status == "" {
			status = "running"
		}
		if status == "deleted" {
			w.Header().Set("Content-Type", "application/vnd.api+json")
			w.WriteHeader(http.StatusNotFound)
			writeJSON(map[string]any{"errors": []map[string]string{{
				"status": "404", "code": runnercontract.ErrorCodeJobDeleted, "title": "Not Found",
			}}})
			return
		}
		writeJSON(map[string]string{"status": status})
	case strings.HasSuffix(path, "/complete"):
		var req runnercontract.JobCompleteRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if slices.Contains(f.capabilities, runnercontract.CapabilityJobToken) && r.Header.Get("Authorization") != "Bearer swj-"+jobOf(path) {
			claim, exists := f.claims[jobOf(path)]
			if r.Header.Get("Authorization") != "Bearer swr-1" || !exists || req.Status != "failed" || req.Attempt != claim.Attempt || req.Phase != claim.Phase {
				http.Error(w, "runner-key completion requires an exact failed startup", http.StatusForbidden)
				return
			}
		}
		f.failed[jobOf(path)] = req.ErrorMessage
		writeJSON(map[string]string{"status": "ok"})
	default:
		http.NotFound(w, r)
	}
}

func jobOf(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) >= 3 {
		return parts[2]
	}
	return ""
}

// startPlacement is the placement a launcher reported when it claimed job.
func (f *fakeAPI) startPlacement(job string) *runnercontract.Placement {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.placements[job]
}

func (f *fakeAPI) snapshot() (started, tokens []string, failed map[string]string, deregistered bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.started...), append([]string(nil), f.tokens...), maps.Clone(f.failed), f.deregistered
}

// fakeDriver places nothing real: each placement ends when the test (or Kill) ends it.
type fakeDriver struct {
	name     string
	mu       sync.Mutex
	placed   []driver.Request
	exits    map[string]chan driver.Exit
	pingErr  error
	placeErr error // every Place fails with it when set
	reattach bool
	// onKill runs when a placement is killed, before its executor exits.
	onKill  func(job string)
	reaped  []map[string]string
	listed  []driver.Placement
	killed  []string
	removed []driver.Placement
}

func newFakeDriver(name string) *fakeDriver {
	return &fakeDriver{name: name, exits: map[string]chan driver.Exit{}}
}

func (d *fakeDriver) Name() string { return d.name }
func (d *fakeDriver) Capabilities() driver.Capabilities {
	return driver.Capabilities{SupportsReattach: d.reattach}
}
func (d *fakeDriver) Ping(context.Context) error { return d.pingErr }
func (d *fakeDriver) Place(ctx context.Context, req driver.Request) (driver.Placement, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// An engine or a cluster refuses a request whose context is done.
	if err := ctx.Err(); err != nil {
		return driver.Placement{}, err
	}
	if d.placeErr != nil {
		return driver.Placement{}, d.placeErr
	}
	d.placed = append(d.placed, req)
	d.exits[req.JobID] = make(chan driver.Exit, 1)
	return driver.Placement{ID: "c-" + req.JobID, Labels: req.Labels}, nil
}

func (d *fakeDriver) exitCh(p driver.Placement) chan driver.Exit {
	d.mu.Lock()
	defer d.mu.Unlock()
	job := p.Labels[driver.LabelJob]
	if d.exits[job] == nil {
		d.exits[job] = make(chan driver.Exit, 1)
	}
	return d.exits[job]
}

func (d *fakeDriver) Wait(ctx context.Context, p driver.Placement) (driver.Exit, error) {
	select {
	case e := <-d.exitCh(p):
		return e, nil
	case <-ctx.Done():
		return driver.Exit{}, ctx.Err()
	}
}

func (d *fakeDriver) Kill(_ context.Context, p driver.Placement, _ time.Duration) error {
	d.mu.Lock()
	d.killed = append(d.killed, p.Labels[driver.LabelJob])
	onKill := d.onKill
	d.mu.Unlock()
	if onKill != nil {
		onKill(p.Labels[driver.LabelJob])
	}
	select {
	case d.exitCh(p) <- driver.Exit{Code: exitStopped}:
	default:
	}
	return nil
}

func (d *fakeDriver) Remove(_ context.Context, p driver.Placement) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removed = append(d.removed, p)
	return nil
}

// removal is the placement the launcher removed for job, and whether it did.
func (d *fakeDriver) removal(job string) (driver.Placement, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, p := range d.removed {
		if p.Labels[driver.LabelJob] == job {
			return p, true
		}
	}
	return driver.Placement{}, false
}

// Reap records the selector of each sweep of kept placements.
func (d *fakeDriver) Reap(_ context.Context, selector map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reaped = append(d.reaped, maps.Clone(selector))
	return nil
}

func (d *fakeDriver) List(context.Context, map[string]string) ([]driver.Placement, error) {
	return d.listed, nil
}

func (d *fakeDriver) Logs(context.Context, driver.Placement) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

// end ends job's placement with code.
func (d *fakeDriver) end(job string, code int) {
	d.exitCh(driver.Placement{Labels: map[string]string{driver.LabelJob: job}}) <- driver.Exit{Code: code}
}

func (d *fakeDriver) placements() []driver.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]driver.Request(nil), d.placed...)
}

// testConfig is a customer launcher's configuration against f.
func testConfig(f *fakeAPI) Config {
	return Config{
		ServerURL: f.srv.URL, RegistrationToken: "swa-agent", PoolID: "pool-1", Name: "launcher-test",
		Engines: []string{"tofu", "ansible"}, Capacity: 2, JobImages: map[string]string{"tofu": "platform:1", "ansible": "platform:1"},
		HeartbeatWait: time.Second, IdleBeat: 20 * time.Millisecond, StatusInterval: 20 * time.Millisecond,
		DrainGrace: 5 * time.Second, KillGrace: 100 * time.Millisecond,
	}
}

// eventually polls cond until it holds or two seconds pass.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// runLauncher runs l until the test ends, then stops it and waits for its drain, so no launcher
// outlives its test.
func runLauncher(t *testing.T, l *Launcher) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = l.Run(ctx)
	}()
	t.Cleanup(func() {
		stop()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the launcher did not stop within ten seconds")
		}
	})
}
