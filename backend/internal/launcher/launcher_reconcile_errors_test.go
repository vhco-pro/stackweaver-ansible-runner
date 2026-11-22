// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

func restartedDestroyClaim() runnercontract.ActiveClaim {
	return runnercontract.ActiveClaim{JobID: "run-recovery", JobType: "tofu_run", Attempt: 3, Phase: "apply", RunType: "apply-destroy", TimeoutSeconds: 600}
}

func restartedDestroyPlacement() driver.Placement {
	return driver.Placement{ID: "surviving-destroy", Labels: map[string]string{
		driver.LabelManaged: "true", driver.LabelRunner: "runner-1", driver.LabelJob: "run-recovery",
		driver.LabelKind: "tofu", driver.LabelAttempt: "3", driver.LabelRunType: "apply-destroy",
	}}
}

// reconciliationAPI supplies complete inventories separately from offers. The underlying
// fixture validates every token request against the original claim.
type reconciliationAPI struct {
	mu                  sync.Mutex
	holdHeartbeatsAfter time.Time
	heartbeatHeld       bool
	base                *recoveryAPI
	claims              []runnercontract.ActiveClaim
	claimsReadyAt       time.Time
	statusReadyAt       time.Time
	statusHTTP          int
	statusCode          string
	statusValue         string
	statusNetworkError  bool
	completeReadyAt     time.Time
	tokenClaim          *runnercontract.ActiveClaim
}

func (api *reconciliationAPI) roundTrip(request *http.Request) (*http.Response, error) {
	if strings.HasSuffix(request.URL.Path, "/heartbeat") && !api.holdHeartbeatsAfter.IsZero() && !time.Now().Before(api.holdHeartbeatsAfter) {
		api.mu.Lock()
		hold := !api.heartbeatHeld
		api.heartbeatHeld = true
		api.mu.Unlock()
		if hold {
			<-request.Context().Done()
			return nil, request.Context().Err()
		}
	}
	if strings.HasSuffix(request.URL.Path, "run-recovery/status") && time.Now().Before(api.statusReadyAt) {
		if api.statusNetworkError {
			return nil, errors.New("injected connection failure")
		}
		if api.statusValue != "" {
			return recoveryResponse(request, http.StatusOK, map[string]string{"status": api.statusValue})
		}
		if api.statusHTTP != 0 {
			return recoveryResponse(request, api.statusHTTP, map[string]any{"errors": []map[string]string{{"code": api.statusCode}}})
		}
		return recoveryResponse(request, http.StatusInternalServerError, nil)
	}
	if strings.HasSuffix(request.URL.Path, "/complete") && time.Now().Before(api.completeReadyAt) {
		return recoveryResponse(request, http.StatusServiceUnavailable, nil)
	}
	if strings.HasSuffix(request.URL.Path, "/token") && api.tokenClaim != nil {
		var expected runnercontract.JobTokenRequest
		if err := json.NewDecoder(request.Body).Decode(&expected); err != nil {
			return nil, err
		}
		claim := *api.tokenClaim
		api.base.mu.Lock()
		api.base.tokenCalls++
		api.base.boundRequests = api.base.boundRequests && expected.Attempt == claim.Attempt && expected.Phase == claim.Phase && expected.RunType == claim.RunType
		api.base.mu.Unlock()
		return recoveryResponse(request, http.StatusOK, runnercontract.JobToken{
			JobID: claim.JobID, Attempt: claim.Attempt, Phase: claim.Phase, RunType: claim.RunType,
			Token: "original-token", ContractVersion: runnercontract.Version, TimeoutSeconds: claim.TimeoutSeconds,
		})
	}
	response, err := api.base.roundTrip(request)
	if err != nil || response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return response, err
	}
	claims := api.claims
	if time.Now().Before(api.claimsReadyAt) {
		claims = nil
	}
	if strings.HasSuffix(request.URL.Path, "/register") {
		var registration runnercontract.RegisterResponse
		if err := json.NewDecoder(response.Body).Decode(&registration); err != nil {
			return nil, err
		}
		_ = response.Body.Close()
		registration.ActiveClaims = claims
		return recoveryResponse(request, response.StatusCode, registration)
	}
	if strings.HasSuffix(request.URL.Path, "/heartbeat") {
		var heartbeat runnercontract.HeartbeatResponse
		if err := json.NewDecoder(response.Body).Decode(&heartbeat); err != nil {
			return nil, err
		}
		_ = response.Body.Close()
		heartbeat.ActiveClaims = claims
		return recoveryResponse(request, response.StatusCode, heartbeat)
	}
	return response, nil
}

func startReconciliationLauncher(t *testing.T, api *reconciliationAPI, placementDriver driver.Driver) (*Launcher, context.CancelFunc, <-chan error) {
	t.Helper()
	launcher := New(Config{
		ServerURL: "http://control.example.test", RegistrationToken: "registration-token", PoolID: "pool-1",
		Engines: []string{"tofu", "ansible"}, Capacity: 2,
		JobImages:     map[string]string{"tofu": "platform:1", "ansible": "platform:1"},
		HeartbeatWait: time.Second, IdleBeat: time.Second, StatusInterval: time.Second,
		DrainGrace: 5 * time.Second, KillGrace: time.Millisecond,
	}, placementDriver)
	launcher.goos = "linux"
	launcher.client.http.Transport = claimRecoveryRoundTrip(api.roundTrip)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- launcher.Run(ctx) }()
	return launcher, cancel, done
}

func newReconciliationAPI() *reconciliationAPI {
	return &reconciliationAPI{
		base: &recoveryAPI{
			boundRequests: true, jobOffers: recoveryOffers()[:1],
			capabilities: []string{runnercontract.CapabilityJobToken, runnercontract.CapabilityClaimRecovery},
		},
		claims: []runnercontract.ActiveClaim{restartedDestroyClaim()},
	}
}

func TestReconcileRecovery_PreservesPlacementAndHeartbeatsThroughStatusFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := newReconciliationAPI()
		api.statusReadyAt = time.Now().Add(95 * time.Second)
		d := newFakeDriver("container")
		d.reattach = true
		d.listed = []driver.Placement{restartedDestroyPlacement()}
		launcher, cancel, done := startReconciliationLauncher(t, api, d)
		defer finishRecoveryLauncher(t, d, cancel, done)
		time.Sleep(105 * time.Second)
		synctest.Wait()
		d.mu.Lock()
		if len(d.killed) != 0 || len(d.removed) != 0 {
			t.Errorf("transient status failure discarded owned work, killed %v removed %d", d.killed, len(d.removed))
		}
		d.mu.Unlock()
		if launcher.running() != 2 {
			t.Errorf("running %d, want surviving work and the other job", launcher.running())
		}
		placements := d.placements()
		if len(placements) != 1 || placements[0].JobID != "job-existing" {
			t.Errorf("restart changed or duplicated placement: %+v", placements)
		}
		api.base.mu.Lock()
		defer api.base.mu.Unlock()
		if !api.base.boundRequests || api.base.startCalls != 1 || len(api.base.failed) != 0 {
			t.Errorf("reconciliation changed original identity, starts %d failures %+v", api.base.startCalls, api.base.failed)
		}
		if len(api.base.heartbeats) < 40 {
			t.Errorf("reconciliation starved heartbeats, got %d", len(api.base.heartbeats))
		}
		for _, beat := range api.base.heartbeats {
			if beat.at.Before(api.statusReadyAt) && beat.at.After(api.statusReadyAt.Add(-90*time.Second)) && (beat.current != 2 || beat.free != 0) {
				t.Errorf("existing work lost capacity during recovery, current %d free %d", beat.current, beat.free)
				break
			}
		}
	})
}

type recoveringInventoryDriver struct {
	*fakeDriver
	mu         sync.Mutex
	recoverAt  time.Time
	first      bool
	firstStall time.Duration
}

func (d *recoveringInventoryDriver) List(ctx context.Context, selector map[string]string) ([]driver.Placement, error) {
	d.mu.Lock()
	first := !d.first
	d.first = true
	d.mu.Unlock()
	if first {
		started := time.Now()
		<-ctx.Done()
		d.mu.Lock()
		d.firstStall = time.Since(started)
		d.mu.Unlock()
		return nil, ctx.Err()
	}
	if time.Now().Before(d.recoverAt) {
		return nil, errors.New("injected inventory failure")
	}
	return d.fakeDriver.List(ctx, selector)
}

func TestReconcileRecovery_WaitsForCompleteInventories(t *testing.T) {
	for _, missing := range []string{"claims", "placements"} {
		t.Run(missing, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started := time.Now()
				api := newReconciliationAPI()
				d := newFakeDriver("container")
				d.reattach = true
				d.listed = []driver.Placement{restartedDestroyPlacement()}
				var placementDriver driver.Driver = d
				var recovering *recoveringInventoryDriver
				if missing == "claims" {
					api.claimsReadyAt = time.Now().Add(95 * time.Second)
				} else {
					recovering = &recoveringInventoryDriver{fakeDriver: d, recoverAt: time.Now().Add(95 * time.Second)}
					placementDriver = recovering
				}
				launcher, cancel, done := startReconciliationLauncher(t, api, placementDriver)
				defer finishRecoveryLauncher(t, d, cancel, done)
				time.Sleep(30 * time.Second)
				synctest.Wait()
				if len(d.placements()) != 0 {
					t.Error("admitted offers before the complete startup join")
				}
				api.base.mu.Lock()
				if len(api.base.heartbeats) < 10 {
					t.Error("incomplete inventory blocked heartbeats")
				}
				for _, beat := range api.base.heartbeats {
					if beat.free != 0 || beat.at.After(started.Add(3*time.Second)) && beat.current < 1 {
						t.Errorf("incomplete inventory advertised capacity, current %d free %d", beat.current, beat.free)
						break
					}
				}
				api.base.mu.Unlock()
				time.Sleep(75 * time.Second)
				synctest.Wait()
				if launcher.running() != 2 || len(d.placements()) != 1 {
					t.Error("complete inventories did not recover existing work and admit the other job")
				}
				if recovering != nil {
					recovering.mu.Lock()
					if recovering.firstStall <= 0 || recovering.firstStall > 20*time.Second {
						t.Errorf("inventory request stalled %s, want at most twenty seconds", recovering.firstStall)
					}
					recovering.mu.Unlock()
				}
			})
		})
	}
}

func TestReconcileRecovery_AbandonsClaimWithoutPlacement(t *testing.T) {
	for _, kind := range []string{"ansible_job", runnercontract.JobTypeWorkItem, "tofu_run"} {
		for _, reattach := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reattach=%v", kind, reattach), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					claim := restartedDestroyClaim()
					claim.JobType = kind
					if kind != "tofu_run" {
						claim.Phase, claim.RunType = "", ""
					}
					api := newReconciliationAPI()
					api.claims = []runnercontract.ActiveClaim{claim}
					api.base.jobOffers = []runnercontract.PendingJob{}
					d := newFakeDriver("container")
					d.reattach = reattach
					launcher, cancel, done := startReconciliationLauncher(t, api, d)
					defer finishRecoveryLauncher(t, d, cancel, done)
					time.Sleep(5 * time.Second)
					synctest.Wait()
					api.base.mu.Lock()
					defer api.base.mu.Unlock()
					if len(api.base.failed) != 1 || api.base.failed[0].Attempt != claim.Attempt || api.base.failed[0].Phase != claim.Phase {
						t.Errorf("restart-before-ack reports %+v, want one original claim failure", api.base.failed)
					}
					if api.base.startCalls != 0 || api.base.tokenCalls != 0 || len(d.placements()) != 0 || launcher.running() != 0 {
						t.Error("claim without placement was restarted or left unresolved")
					}
				})
			})
		}
	}
}

func TestReconcileRecovery_HardDrainBoundsInFlightHeartbeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := newReconciliationAPI()
		api.holdHeartbeatsAfter = time.Now().Add(7 * time.Second)
		d := newFakeDriver("container")
		d.reattach = true
		d.listed = []driver.Placement{restartedDestroyPlacement()}
		_, cancel, done := startReconciliationLauncher(t, api, d)
		time.Sleep(6 * time.Second)
		synctest.Wait()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("in-flight drain heartbeat exceeded the configured hard grace")
			if err := <-done; err != nil {
				t.Error(err)
			}
		}
	})
}

func TestReconcileRecovery_UnknownIdentityRetainsCapacity(t *testing.T) {
	for _, field := range []string{driver.LabelAttempt, driver.LabelKind, driver.LabelRunType} {
		t.Run(field, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				api := newReconciliationAPI()
				api.base.jobOffers = []runnercontract.PendingJob{}
				d := newFakeDriver("container")
				d.reattach = true
				placement := restartedDestroyPlacement()
				placement.Labels[field] = "unknown"
				d.listed = []driver.Placement{placement}
				launcher, cancel, done := startReconciliationLauncher(t, api, d)
				defer finishRecoveryLauncher(t, d, cancel, done)
				time.Sleep(100 * time.Second)
				synctest.Wait()
				api.base.mu.Lock()
				defer api.base.mu.Unlock()
				if api.base.tokenCalls != 0 || len(api.base.failed) != 0 || api.base.startCalls != 0 {
					t.Errorf("unknown original identity acquired credentials or changed a claim: token=%d failed=%v start=%d", api.base.tokenCalls, api.base.failed, api.base.startCalls)
				}
				d.mu.Lock()
				defer d.mu.Unlock()
				if launcher.running() != 1 || len(d.killed) != 0 || len(d.removed) != 0 || len(d.placed) != 0 {
					t.Error("unknown placement lost its reservation or was replaced")
				}
			})
		})
	}
}

func TestReconcileRecovery_LaterClaimCannotSupplyPlacementIdentity(t *testing.T) {
	for _, later := range []runnercontract.ActiveClaim{
		{JobID: "run-recovery", JobType: "tofu_run", Attempt: 4, Phase: "apply", RunType: "apply-destroy"},
		{JobID: "run-recovery", JobType: "tofu_run", Attempt: 4, Phase: "plan", RunType: "plan-destroy"},
	} {
		t.Run(later.RunType, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				api := newReconciliationAPI()
				api.claims = []runnercontract.ActiveClaim{later}
				api.base.jobOffers = []runnercontract.PendingJob{}
				d := newFakeDriver("container")
				d.reattach = true
				d.listed = []driver.Placement{restartedDestroyPlacement()}
				launcher, cancel, done := startReconciliationLauncher(t, api, d)
				defer finishRecoveryLauncher(t, d, cancel, done)
				time.Sleep(10 * time.Second)
				synctest.Wait()
				api.base.mu.Lock()
				defer api.base.mu.Unlock()
				if len(api.base.failed) != 1 || api.base.failed[0].Attempt != later.Attempt || api.base.failed[0].Phase != later.Phase {
					t.Errorf("later claim without its own placement was not resolved exactly: %+v", api.base.failed)
				}
				if !api.base.boundRequests || api.base.startCalls != 0 || len(d.placements()) != 0 || launcher.running() != 1 {
					t.Error("old placement was rebound or lost its uncertainty reservation")
				}
			})
		})
	}
}

func TestReconcileRecovery_OriginalOperationAndWorkItemEngine(t *testing.T) {
	for _, operation := range []struct {
		kind, engine, phase, runType, label string
	}{
		{"tofu_run", "tofu", "plan", "plan-destroy", "plan-destroy"},
		{"tofu_run", "tofu", "apply", "apply-destroy", "apply-destroy"},
		{"tofu_run", "tofu", "plan", "plan", "plan-only"},
		{"ansible_job", "ansible", "", "", ""},
		{runnercontract.JobTypeWorkItem, "ansible", "", "", ""},
		{runnercontract.JobTypeWorkItem, "tofu", "", "", ""},
	} {
		t.Run(operation.kind+"/"+operation.engine+"/"+operation.label, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				claim := restartedDestroyClaim()
				claim.JobType, claim.Phase, claim.RunType = operation.kind, operation.phase, operation.runType
				api := newReconciliationAPI()
				api.claims, api.tokenClaim = []runnercontract.ActiveClaim{claim}, &claim
				api.base.jobOffers = []runnercontract.PendingJob{}
				d := newFakeDriver("container")
				d.reattach = true
				placement := restartedDestroyPlacement()
				placement.Labels[driver.LabelKind], placement.Labels[driver.LabelRunType] = operation.engine, operation.label
				d.listed = []driver.Placement{placement}
				launcher, cancel, done := startReconciliationLauncher(t, api, d)
				defer finishRecoveryLauncher(t, d, cancel, done)
				time.Sleep(10 * time.Second)
				synctest.Wait()
				api.base.mu.Lock()
				defer api.base.mu.Unlock()
				d.mu.Lock()
				defer d.mu.Unlock()
				if api.base.tokenCalls != 1 || !api.base.boundRequests || len(api.base.failed) != 0 || d.exits[claim.JobID] == nil || launcher.running() != 1 {
					t.Errorf("original placement was not reattached: tokens=%d bound=%v failed=%+v waiting=%v", api.base.tokenCalls, api.base.boundRequests, api.base.failed, d.exits[claim.JobID] != nil)
				}
			})
		})
	}
}

func TestReconcileRecovery_EndedPlacementRetainsCapacityUntilFailureDelivery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := newReconciliationAPI()
		api.completeReadyAt = time.Now().Add(205 * time.Second)
		api.base.jobOffers = []runnercontract.PendingJob{}
		d := newFakeDriver("container")
		d.reattach = true
		placement := restartedDestroyPlacement()
		placement.Ended = true
		d.listed = []driver.Placement{placement}
		launcher, cancel, done := startReconciliationLauncher(t, api, d)
		defer finishRecoveryLauncher(t, d, cancel, done)
		time.Sleep(150 * time.Second)
		synctest.Wait()
		d.mu.Lock()
		if launcher.running() != 1 || len(d.removed) != 0 {
			t.Error("undelivered ended-placement failure released capacity or removed its evidence")
		}
		d.mu.Unlock()
		time.Sleep(75 * time.Second)
		synctest.Wait()
		api.base.mu.Lock()
		defer api.base.mu.Unlock()
		d.mu.Lock()
		defer d.mu.Unlock()
		if len(api.base.failed) != 1 || api.base.failed[0].Attempt != 3 || api.base.failed[0].Phase != "apply" || launcher.running() != 0 || len(d.removed) != 1 {
			t.Errorf("ended-placement report did not resolve the exact original claim: %+v", api.base.failed)
		}
	})
}

func TestReconcileRecovery_ReducedCapacityPreservesSurvivingWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := newReconciliationAPI()
		api.claims = []runnercontract.ActiveClaim{}
		d := newFakeDriver("container")
		d.reattach = true
		for index := range 3 {
			placement := restartedDestroyPlacement()
			placement.ID = fmt.Sprintf("surviving-%d", index)
			placement.Labels[driver.LabelJob] = fmt.Sprintf("original-%d", index)
			d.listed = append(d.listed, placement)
		}
		launcher, cancel, done := startReconciliationLauncher(t, api, d)
		defer finishRecoveryLauncher(t, d, cancel, done)
		time.Sleep(20 * time.Second)
		synctest.Wait()
		if launcher.running() != 3 || len(d.placements()) != 0 || len(d.killed) != 0 {
			t.Error("reduced configured capacity displaced or replaced surviving work")
		}
		api.base.mu.Lock()
		defer api.base.mu.Unlock()
		for _, heartbeat := range api.base.heartbeats {
			if heartbeat.free != 0 {
				t.Errorf("over-capacity launcher advertised %d free slots", heartbeat.free)
			}
		}
	})
}

func TestReconcileRecovery_RequiresExplicitStatusStop(t *testing.T) {
	for _, scenario := range []struct {
		name, code, status string
		http               int
		network, stop      bool
	}{
		{name: "network", network: true},
		{name: "bare401", http: http.StatusUnauthorized},
		{name: "bare403", http: http.StatusForbidden},
		{name: "coded403", http: http.StatusForbidden, code: runnercontract.ErrorCodeJobTokenRevoked},
		{name: "bare404", http: http.StatusNotFound},
		{name: "wrong404", http: http.StatusNotFound, code: runnercontract.ErrorCodeJobTerminal},
		{name: "server500", http: http.StatusInternalServerError},
		{name: "server503", http: http.StatusServiceUnavailable},
		{name: "final-status", status: "applied"},
		{name: "deleted", http: http.StatusNotFound, code: runnercontract.ErrorCodeJobDeleted, stop: true},
		{name: "canceled", status: "canceled", stop: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				api := newReconciliationAPI()
				api.statusReadyAt = time.Now().Add(95 * time.Second)
				api.statusHTTP, api.statusCode, api.statusValue = scenario.http, scenario.code, scenario.status
				api.statusNetworkError = scenario.network
				api.base.jobOffers = []runnercontract.PendingJob{}
				d := newFakeDriver("container")
				d.reattach = true
				d.listed = []driver.Placement{restartedDestroyPlacement()}
				launcher, cancel, done := startReconciliationLauncher(t, api, d)
				defer finishRecoveryLauncher(t, d, cancel, done)
				time.Sleep(30 * time.Second)
				synctest.Wait()
				if scenario.stop {
					if launcher.running() != 0 || len(d.killed) != 1 || len(d.removed) != 1 || !d.removed[0].Canceled {
						t.Error("explicit stop did not clean the original placement once")
					}
				} else if launcher.running() != 1 || len(d.killed) != 0 || len(d.removed) != 0 {
					t.Error("indeterminate status discarded the original placement")
				}
			})
		})
	}
}

func TestReconcileRecovery_LateReservationInheritsHardDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := newFakeDriver("container")
		launcher := New(Config{IdleBeat: time.Second, KillGrace: time.Millisecond}, d)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		// Startup inventory can finish as hard drain cancels discovery. A reservation
		// created in that interleaving must inherit the same hard cancellation.
		claim := runnercontract.ActiveClaim{JobID: "unknown-claim"}
		launcher.startRecovered(ctx, "claim:"+claim.JobID, claim.JobID, driver.Placement{}, &claim)
		finished := make(chan struct{})
		go func() { launcher.wg.Wait(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Error("late recovery reservation escaped hard drain cancellation")
			launcher.stopAll(t.Context())
			<-finished
		}
	})
}

func TestReconcileRecovery_SettledOriginalClaimReleasesItsPlacement(t *testing.T) {
	for _, scenario := range []struct {
		name, code     string
		status         int
		stop, canceled bool
	}{
		{name: "terminal403", code: runnercontract.ErrorCodeJobTerminal, status: http.StatusForbidden, stop: true},
		{name: "terminal409", code: runnercontract.ErrorCodeJobTerminal, status: http.StatusConflict, stop: true},
		{name: "deleted", code: runnercontract.ErrorCodeJobDeleted, status: http.StatusNotFound, stop: true, canceled: true},
		{name: "bare403", status: http.StatusForbidden},
		{name: "bare404", status: http.StatusNotFound},
		{name: "server-error", code: runnercontract.ErrorCodeJobTerminal, status: http.StatusInternalServerError},
		{name: "unexpected-credential", status: http.StatusOK},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				api := newReconciliationAPI()
				api.claims = []runnercontract.ActiveClaim{}
				api.base.jobOffers = []runnercontract.PendingJob{}
				d := newFakeDriver("container")
				d.reattach = true
				d.listed = []driver.Placement{restartedDestroyPlacement()}
				var mu sync.Mutex
				probes := 0
				bound := true
				launcher := New(Config{ServerURL: "http://control.example.test", RegistrationToken: "fixture", Hosted: true, Capacity: 1, IdleBeat: time.Second, HeartbeatWait: time.Second, StatusInterval: time.Second, DrainGrace: 5 * time.Second, KillGrace: time.Millisecond}, d)
				launcher.goos = "linux"
				launcher.client.http.Transport = claimRecoveryRoundTrip(func(request *http.Request) (*http.Response, error) {
					if strings.HasSuffix(request.URL.Path, "/status") {
						return recoveryResponse(request, http.StatusOK, map[string]string{"status": "applied"})
					}
					if strings.HasSuffix(request.URL.Path, "/token") {
						var expected runnercontract.JobTokenRequest
						if err := json.NewDecoder(request.Body).Decode(&expected); err != nil {
							return nil, err
						}
						mu.Lock()
						probes++
						bound = bound && expected.Attempt == 3 && expected.Phase == "apply" && expected.RunType == "apply-destroy"
						mu.Unlock()
						if scenario.status == http.StatusOK {
							return recoveryResponse(request, http.StatusOK, runnercontract.JobToken{JobID: "run-recovery", Attempt: 3, Phase: "apply", RunType: "apply-destroy", Token: "unmatched-original-token", ContractVersion: runnercontract.Version})
						}
						return recoveryResponse(request, scenario.status, map[string]any{"errors": []map[string]string{{"code": scenario.code}}})
					}
					return api.roundTrip(request)
				})
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- launcher.Run(ctx) }()
				defer finishRecoveryLauncher(t, d, cancel, done)
				time.Sleep(10 * time.Second)
				synctest.Wait()
				mu.Lock()
				defer mu.Unlock()
				d.mu.Lock()
				defer d.mu.Unlock()
				if !bound || probes == 0 {
					t.Errorf("original identity was not validated: probes=%d bound=%v", probes, bound)
				}
				if scenario.stop {
					if probes != 1 || launcher.running() != 0 || len(d.killed) != 1 || len(d.removed) != 1 || d.removed[0].Canceled != scenario.canceled {
						t.Errorf("classified original claim remains reserved or has wrong cancellation: probes=%d running=%d killed=%v removed=%+v", probes, launcher.running(), d.killed, d.removed)
					}
				} else if launcher.running() != 1 || len(d.killed) != 0 || len(d.removed) != 0 || d.exits["run-recovery"] != nil {
					t.Error("unclassified outcome or unmatched credentials changed supervision or capacity")
				}
			})
		})
	}
}

func TestReconcileRecovery_InconsistentClaimKindCannotMintCredentials(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := newReconciliationAPI()
		api.claims = []runnercontract.ActiveClaim{{JobID: "run-recovery", JobType: "ansible_job", Attempt: 3}}
		api.base.jobOffers = []runnercontract.PendingJob{}
		placement := restartedDestroyPlacement()
		placement.Labels[driver.LabelRunType] = ""
		d := newFakeDriver("container")
		d.reattach = true
		d.listed = []driver.Placement{placement}
		launcher, cancel, done := startReconciliationLauncher(t, api, d)
		defer finishRecoveryLauncher(t, d, cancel, done)
		time.Sleep(10 * time.Second)
		synctest.Wait()
		api.base.mu.Lock()
		defer api.base.mu.Unlock()
		if api.base.tokenCalls != 0 || len(api.base.failed) != 0 || launcher.running() != 1 {
			t.Errorf("inconsistent original engine acquired credentials or changed the claim: tokens=%d failed=%v running=%d", api.base.tokenCalls, api.base.failed, launcher.running())
		}
	})
}

func TestReconcileRecovery_UnknownActiveIdentityRetainsOneReservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := newReconciliationAPI()
		claim := restartedDestroyClaim()
		claim.Phase, claim.RunType = "", ""
		api.claims = []runnercontract.ActiveClaim{claim}
		api.base.jobOffers = []runnercontract.PendingJob{}
		d := newFakeDriver("container")
		d.reattach = true
		d.listed = []driver.Placement{restartedDestroyPlacement()}
		launcher, cancel, done := startReconciliationLauncher(t, api, d)
		defer finishRecoveryLauncher(t, d, cancel, done)
		time.Sleep(10 * time.Second)
		synctest.Wait()
		api.base.mu.Lock()
		defer api.base.mu.Unlock()
		if api.base.tokenCalls != 0 || len(api.base.failed) != 0 || launcher.running() != 1 {
			t.Errorf("unknown active phase changed or duplicated the original reservation: tokens=%d failed=%v running=%d", api.base.tokenCalls, api.base.failed, launcher.running())
		}
	})
}
