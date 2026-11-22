// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

type claimRecoveryRoundTrip func(*http.Request) (*http.Response, error)

func (roundTrip claimRecoveryRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

type recoveryHeartbeat struct {
	at      time.Time
	current int
	free    int
}

// recoveryAPI exposes the current protocol while permitting the old missing request fields
// long enough to reproduce its heartbeat starvation. Assertions still require bound requests.
type recoveryAPI struct {
	single             bool
	jobOffers          []runnercontract.PendingJob
	mu                 sync.Mutex
	recoverAt          time.Time
	failure            string
	capabilities       []string
	offered            bool
	stalled            bool
	firstStall         time.Duration
	registeredRecovery bool
	boundRequests      bool
	heartbeats         []recoveryHeartbeat
	failed             []runnercontract.JobCompleteRequest
	startCalls         int
	tokenCalls         int
	pullCalls          int
}

func recoveryOffers() []runnercontract.PendingJob {
	return []runnercontract.PendingJob{
		{JobID: "job-existing", JobType: "ansible_job", Attempt: 1},
		{JobID: "run-recovery", JobType: "tofu_run", Attempt: 3, Phase: "apply", RunType: "apply-destroy"},
	}
}

func recoveryResponse(request *http.Request, status int, payload any) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    request,
	}, nil
}

func (api *recoveryAPI) roundTrip(request *http.Request) (*http.Response, error) {
	path := strings.TrimPrefix(request.URL.Path, "/api/v2/runner")
	if path == "/heartbeat" {
		select {
		case <-time.After(time.Second):
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
		var heartbeat runnercontract.HeartbeatRequest
		if err := json.NewDecoder(request.Body).Decode(&heartbeat); err != nil {
			return nil, err
		}
		api.mu.Lock()
		defer api.mu.Unlock()
		api.heartbeats = append(api.heartbeats, recoveryHeartbeat{time.Now(), heartbeat.CurrentJobs, heartbeat.AvailableCapacity})
		response := runnercontract.HeartbeatResponse{Capabilities: api.capabilities, ActiveClaims: []runnercontract.ActiveClaim{}}
		if !api.offered && heartbeat.AvailableCapacity > 0 {
			response.PendingJobs = recoveryOffers()
			if api.jobOffers != nil {
				response.PendingJobs = api.jobOffers
			}
			api.offered = true
		}
		return recoveryResponse(request, http.StatusOK, response)
	}
	if path == "/register" {
		var registration runnercontract.RegisterRequest
		if err := json.NewDecoder(request.Body).Decode(&registration); err != nil {
			return nil, err
		}
		api.mu.Lock()
		api.registeredRecovery = slices.Contains(registration.Capabilities, runnercontract.CapabilityClaimRecovery)
		capabilities := slices.Clone(api.capabilities)
		api.mu.Unlock()
		return recoveryResponse(request, http.StatusCreated, runnercontract.RegisterResponse{
			RunnerID: "runner-1", RunnerAPIKey: "runner-token", Capabilities: capabilities, ActiveClaims: []runnercontract.ActiveClaim{},
		})
	}
	if path == "/deregister" || strings.HasSuffix(path, "/output") {
		return recoveryResponse(request, http.StatusOK, map[string]string{"status": "ok"})
	}
	if strings.HasSuffix(path, "/status") {
		return recoveryResponse(request, http.StatusOK, map[string]string{"status": "running"})
	}
	if strings.HasSuffix(path, "/complete") {
		var report runnercontract.JobCompleteRequest
		if err := json.NewDecoder(request.Body).Decode(&report); err != nil {
			return nil, err
		}
		api.mu.Lock()
		api.failed = append(api.failed, report)
		api.mu.Unlock()
		return recoveryResponse(request, http.StatusOK, map[string]string{"status": "ok"})
	}
	if strings.HasSuffix(path, "/start") {
		var start runnercontract.JobStartRequest
		if err := json.NewDecoder(request.Body).Decode(&start); err != nil {
			return nil, err
		}
		offer := recoveryOffers()[0]
		if strings.Contains(path, "run-recovery") {
			offer = recoveryOffers()[1]
		}
		api.mu.Lock()
		api.startCalls++
		api.boundRequests = api.boundRequests && start.Attempt == offer.Attempt && start.Phase == offer.Phase && start.RunType == offer.RunType
		api.mu.Unlock()
		return recoveryResponse(request, http.StatusOK, map[string]any{"meta": runnercontract.JobStartResponse{
			Status: "started", Attempt: offer.Attempt, Phase: offer.Phase, RunType: offer.RunType,
		}})
	}
	if strings.HasSuffix(path, "/token") {
		offer := recoveryOffers()[0]
		if strings.Contains(path, "run-recovery") {
			offer = recoveryOffers()[1]
		}
		var tokenRequest runnercontract.JobTokenRequest
		if request.Body != nil {
			if err := json.NewDecoder(request.Body).Decode(&tokenRequest); err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
		}
		api.mu.Lock()
		api.tokenCalls++
		api.boundRequests = api.boundRequests && tokenRequest.Attempt == offer.Attempt && tokenRequest.Phase == offer.Phase && tokenRequest.RunType == offer.RunType
		api.mu.Unlock()
		if offer.JobID == "run-recovery" {
			if response, err := api.outage(request, "token"); response != nil || err != nil {
				return response, err
			}
		}
		token := runnercontract.JobToken{
			Token: "job-token-" + offer.JobID, JobID: offer.JobID, Attempt: offer.Attempt,
			Phase: offer.Phase, RunType: offer.RunType, ContractVersion: runnercontract.Version, TimeoutSeconds: 600,
		}
		if offer.JobID == "run-recovery" && api.failure == "pull" {
			token.Image = &runnercontract.JobImage{ID: "image-1", Ref: "registry.example.test/job:1", PullCredential: true}
		}
		return recoveryResponse(request, http.StatusOK, token)
	}
	if path == "/images/image-1/pull-credential" {
		api.mu.Lock()
		api.pullCalls++
		api.mu.Unlock()
		if response, err := api.outage(request, "pull"); response != nil || err != nil {
			return response, err
		}
		return recoveryResponse(request, http.StatusOK, runnercontract.PullCredential{
			Registry: "registry.example.test", Username: "fixture-user", Password: "fixture-password",
		})
	}
	return recoveryResponse(request, http.StatusNotFound, map[string]string{"error": "unknown fixture route"})
}

func (api *recoveryAPI) outage(request *http.Request, operation string) (*http.Response, error) {
	api.mu.Lock()
	if api.failure != operation || !time.Now().Before(api.recoverAt) {
		api.mu.Unlock()
		return nil, nil //nolint:nilnil // No injected outage lets the fixture continue its ordinary response.
	}
	first := !api.stalled
	api.stalled = true
	api.mu.Unlock()
	if first {
		started := time.Now()
		<-request.Context().Done()
		api.mu.Lock()
		api.firstStall = time.Since(started)
		api.mu.Unlock()
		return nil, request.Context().Err()
	}
	return recoveryResponse(request, http.StatusInternalServerError, map[string]string{"error": "injected control lookup outage"})
}

func runRecoveryLauncher(t *testing.T, api *recoveryAPI, transports ...http.RoundTripper) (*Launcher, *fakeDriver, context.CancelFunc, <-chan error) {
	t.Helper()
	d := newFakeDriver("container")
	launcher := New(Config{
		ServerURL: "http://control.example.test", RegistrationToken: "registration-token", PoolID: "pool-1",
		Engines: []string{"tofu", "ansible"}, Capacity: 2, Single: api.single,
		JobImages:     map[string]string{"tofu": "platform:1", "ansible": "platform:1"},
		HeartbeatWait: time.Second, IdleBeat: time.Second, StatusInterval: time.Second,
		DrainGrace: 5 * time.Second, KillGrace: time.Millisecond,
	}, d)
	launcher.goos = "linux"
	launcher.client.http.Transport = claimRecoveryRoundTrip(api.roundTrip)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	if len(transports) > 0 {
		launcher.client.http.Transport = transports[0]
	}
	go func() { done <- launcher.Run(ctx) }()
	return launcher, d, cancel, done
}

func finishRecoveryLauncher(t *testing.T, d *fakeDriver, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	for _, request := range d.placements() {
		d.end(request.JobID, exitCompleted)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("launcher did not finish its bounded drain")
	}
}

func TestClaimRecovery_HeartbeatAndOtherWorkContinueDuringAcquisition(t *testing.T) {
	for _, operation := range []string{"token", "pull"} {
		t.Run(operation, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started := time.Now()
				api := &recoveryAPI{
					recoverAt: started.Add(95 * time.Second), failure: operation, boundRequests: true,
					capabilities: []string{runnercontract.CapabilityJobToken, runnercontract.CapabilityClaimRecovery},
				}
				_, d, cancel, done := runRecoveryLauncher(t, api)
				defer finishRecoveryLauncher(t, d, cancel, done)
				time.Sleep(105 * time.Second)
				synctest.Wait()
				api.mu.Lock()
				defer api.mu.Unlock()
				if len(api.failed) != 0 {
					t.Errorf("recoverable acquisition produced %d failed reports", len(api.failed))
				}
				if !api.registeredRecovery || !api.boundRequests {
					t.Error("launcher did not retain the fixed original claim protocol")
				}
				if api.firstStall > 20*time.Second || api.firstStall <= 0 {
					t.Errorf("first acquisition request stalled %s, want at most twenty seconds", api.firstStall)
				}
				if len(api.heartbeats) < 40 {
					t.Errorf("only %d heartbeats during the 95-second acquisition outage", len(api.heartbeats))
				}
				for i, heartbeat := range api.heartbeats {
					if i > 0 && heartbeat.at.Sub(api.heartbeats[i-1].at) > 3*time.Second {
						t.Error("acquisition blocked the heartbeat loop")
					}
					elapsed := heartbeat.at.Sub(started)
					if elapsed > 3*time.Second && elapsed < 95*time.Second && (heartbeat.current != 2 || heartbeat.free != 0) {
						t.Errorf("pending acquisition lost capacity after %s, current %d free %d", elapsed, heartbeat.current, heartbeat.free)
					}
				}
				placements := d.placements()
				if len(placements) != 2 {
					t.Fatalf("placed %d jobs, want existing work and one recovered job", len(placements))
				}
				for _, request := range placements {
					if request.JobID != "run-recovery" {
						continue
					}
					if request.Attempt != 3 || request.RunType != "apply-destroy" || request.Labels[driver.LabelRunType] != "apply-destroy" {
						t.Error("recovered placement changed its original destroy claim")
					}
					if operation == "pull" && (request.PullAuth == nil || request.PullAuth.Registry != "registry.example.test") {
						t.Error("recovered image credential was not used by its original placement")
					}
				}
			})
		})
	}
}

func TestClaimRecovery_WaitsForClaimRecoveryCapability(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &recoveryAPI{boundRequests: true, capabilities: []string{runnercontract.CapabilityJobToken}}
		_, d, cancel, done := runRecoveryLauncher(t, api)
		defer finishRecoveryLauncher(t, d, cancel, done)
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if len(d.placements()) != 0 {
			t.Fatal("launcher placed jobs before the API advertised claim recovery")
		}
		api.mu.Lock()
		defer api.mu.Unlock()
		if api.startCalls != 0 || len(api.heartbeats) == 0 {
			t.Errorf("start calls %d, heartbeats %d, want no claims and continuing heartbeats", api.startCalls, len(api.heartbeats))
		}
	})
}

func TestClaimRecovery_StartAcknowledgementMustMatchOriginalOffer(t *testing.T) {
	for _, response := range []any{
		map[string]string{"status": "started"},
		map[string]any{"meta": runnercontract.JobStartResponse{Status: "started", Attempt: 4, Phase: "apply", RunType: "apply-destroy"}},
		map[string]any{"meta": runnercontract.JobStartResponse{Status: "started", Attempt: 3, Phase: "plan", RunType: "plan-destroy"}},
	} {
		t.Run(fmt.Sprint(response), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				api := &recoveryAPI{capabilities: []string{runnercontract.CapabilityJobToken, runnercontract.CapabilityClaimRecovery}, boundRequests: true}
				transport := claimRecoveryRoundTrip(func(request *http.Request) (*http.Response, error) {
					if strings.Contains(request.URL.Path, "run-recovery/start") {
						return recoveryResponse(request, http.StatusOK, response)
					}
					return api.roundTrip(request)
				})
				_, d, cancel, done := runRecoveryLauncher(t, api, transport)
				defer finishRecoveryLauncher(t, d, cancel, done)
				time.Sleep(125 * time.Second)
				synctest.Wait()
				for _, request := range d.placements() {
					if request.JobID == "run-recovery" {
						t.Error("placed a claim without its original acknowledgement")
					}
				}
				api.mu.Lock()
				defer api.mu.Unlock()
				if len(api.failed) != 1 || api.failed[0].Attempt != 3 || api.failed[0].Phase != "apply" {
					t.Errorf("failed startup reports = %+v, want one exact original claim", api.failed)
				}
			})
		})
	}
}

func TestClaimRecovery_StartReplayAndDuplicateOffersPlaceOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		api := &recoveryAPI{
			recoverAt: started.Add(95 * time.Second), failure: "start", boundRequests: true,
			capabilities: []string{runnercontract.CapabilityJobToken, runnercontract.CapabilityClaimRecovery},
		}
		transport := claimRecoveryRoundTrip(func(request *http.Request) (*http.Response, error) {
			response, err := api.roundTrip(request)
			if strings.HasSuffix(request.URL.Path, "run-recovery/start") {
				if failure, failureErr := api.outage(request, "start"); failure != nil || failureErr != nil {
					return failure, failureErr
				}
			}
			if strings.HasSuffix(request.URL.Path, "/heartbeat") && err == nil {
				var heartbeat runnercontract.HeartbeatResponse
				if decodeErr := json.NewDecoder(response.Body).Decode(&heartbeat); decodeErr != nil {
					return nil, decodeErr
				}
				_ = response.Body.Close()
				heartbeat.PendingJobs = recoveryOffers()
				return recoveryResponse(request, response.StatusCode, heartbeat)
			}
			return response, err
		})
		_, d, cancel, done := runRecoveryLauncher(t, api, transport)
		defer finishRecoveryLauncher(t, d, cancel, done)
		time.Sleep(105 * time.Second)
		synctest.Wait()
		if placements := d.placements(); len(placements) != 2 {
			t.Fatalf("placements %d, want each original offer once", len(placements))
		}
		api.mu.Lock()
		defer api.mu.Unlock()
		if !api.boundRequests || api.startCalls < 3 || len(api.failed) != 0 {
			t.Errorf("replay changed identity or failed: starts %d, failures %+v", api.startCalls, api.failed)
		}
	})
}

func TestClaimRecovery_ExpiredTokenGetsFreshExactReportBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		api := &recoveryAPI{
			recoverAt: started.Add(10 * time.Minute), failure: "token", boundRequests: true,
			capabilities: []string{runnercontract.CapabilityJobToken, runnercontract.CapabilityClaimRecovery},
		}
		var reports []runnercontract.JobCompleteRequest
		var firstReportAt, lastReportAt time.Time
		transport := claimRecoveryRoundTrip(func(request *http.Request) (*http.Response, error) {
			if strings.HasSuffix(request.URL.Path, "run-recovery/complete") {
				var report runnercontract.JobCompleteRequest
				if err := json.NewDecoder(request.Body).Decode(&report); err != nil {
					return nil, err
				}
				reports = append(reports, report)
				lastReportAt = time.Now()
				if firstReportAt.IsZero() {
					firstReportAt = lastReportAt
				}
				if time.Since(firstReportAt) < 35*time.Second {
					return recoveryResponse(request, http.StatusServiceUnavailable, nil)
				}
				return recoveryResponse(request, http.StatusOK, nil)
			}
			return api.roundTrip(request)
		})
		launcher, d, cancel, done := runRecoveryLauncher(t, api, transport)
		defer finishRecoveryLauncher(t, d, cancel, done)
		time.Sleep(170 * time.Second)
		synctest.Wait()
		if len(reports) < 2 || firstReportAt.Sub(started) < 120*time.Second || lastReportAt.Sub(firstReportAt) < 35*time.Second {
			t.Fatalf("report budget reused acquisition deadline, reports %d, first %s last %s", len(reports), firstReportAt.Sub(started), lastReportAt.Sub(started))
		}
		for _, report := range reports {
			original, _ := json.Marshal(reports[0])
			actual, _ := json.Marshal(report)
			if !bytes.Equal(original, actual) {
				t.Error("failure payload changed between retries")
			}
			if report.Attempt != 3 || report.Phase != "apply" || report.Status != "failed" {
				t.Errorf("failure not bound to original claim: %+v", report)
			}
		}
		if len(d.placements()) != 1 || launcher.running() != 1 {
			t.Errorf("exhausted claim placed or retained after confirmed failure")
		}
	})
}

func TestClaimRecovery_HardDrainCancelsPendingAcquisition(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &recoveryAPI{
			recoverAt: time.Now().Add(time.Hour), failure: "token", boundRequests: true,
			capabilities: []string{runnercontract.CapabilityJobToken, runnercontract.CapabilityClaimRecovery},
		}
		_, d, cancel, done := runRecoveryLauncher(t, api)
		// Startup discovery runs alongside heartbeats. Wait for actual admission before
		// interrupting acquisition, so this still exercises hard drain after a claim.
		deadline := time.Now().Add(10 * time.Second)
		for len(d.placements()) == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Second)
			synctest.Wait()
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("pending acquisition exceeded drain grace")
		}
		time.Sleep(130 * time.Second)
		synctest.Wait()
		if len(d.placements()) != 1 {
			t.Error("a pending claim was placed after hard drain")
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if !slices.Equal(d.killed, []string{"job-existing"}) {
			t.Errorf("hard drain killed nonexistent placement: %v", d.killed)
		}
	})
}

func TestClaimRecovery_CompletionRequiresCommittedOrCodedTerminalResponse(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		code    string
		retired bool
	}{
		{"committed", 200, "", true},
		{"revoked", 403, runnercontract.ErrorCodeJobTokenRevoked, true},
		{"terminal-forbidden", 403, runnercontract.ErrorCodeJobTerminal, true},
		{"terminal-conflict", 409, runnercontract.ErrorCodeJobTerminal, true},
		{"deleted", 404, runnercontract.ErrorCodeJobDeleted, true},
		{"uncoded-conflict", 409, "", false},
		{"uncoded-not-found", 404, "", false},
		{"accepted", 202, "", false},
		{"no-content", 204, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := NewClient("http://control.example.test", "runner-token")
				client.http.Transport = claimRecoveryRoundTrip(func(request *http.Request) (*http.Response, error) {
					return recoveryResponse(request, test.status, map[string]any{"errors": []map[string]string{{"code": test.code}}})
				})
				err := client.Fail(t.Context(), "run-recovery", "original startup failure", runnercontract.JobTokenRequest{Attempt: 3, Phase: "apply", RunType: "apply-destroy"})
				if (err == nil) != test.retired {
					t.Errorf("completion error %v, retired want %v", err, test.retired)
				}
			})
		})
	}
}

func TestClaimRecovery_RetriesServerFailuresWithErrorCodes(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := NewClient("http://control.example.test", "runner-token")
				calls := 0
				client.http.Transport = claimRecoveryRoundTrip(func(request *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 {
						return recoveryResponse(request, status, map[string]any{"errors": []map[string]string{{"code": "database-unavailable"}}})
					}
					return recoveryResponse(request, http.StatusOK, runnercontract.PullCredential{Registry: "registry.example.test"})
				})
				credential, err := client.PullCredential(t.Context(), "image-1")
				if err != nil || calls != 2 || credential.Registry == "" {
					t.Errorf("server failure was not recovered, calls %d, credential %+v, error %v", calls, credential, err)
				}
			})
		})
	}
}

func TestClaimRecovery_InvalidTokenNeverPlacesLaterClaim(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &recoveryAPI{boundRequests: true, capabilities: []string{runnercontract.CapabilityJobToken, runnercontract.CapabilityClaimRecovery}}
		transport := claimRecoveryRoundTrip(func(request *http.Request) (*http.Response, error) {
			if strings.HasSuffix(request.URL.Path, "run-recovery/token") {
				return recoveryResponse(request, http.StatusOK, runnercontract.JobToken{Token: "later-token", JobID: "run-recovery", Attempt: 4, Phase: "apply", RunType: "apply-destroy", ContractVersion: runnercontract.Version})
			}
			return api.roundTrip(request)
		})
		_, d, cancel, done := runRecoveryLauncher(t, api, transport)
		defer finishRecoveryLauncher(t, d, cancel, done)
		time.Sleep(130 * time.Second)
		synctest.Wait()
		if len(d.placements()) != 1 {
			t.Error("placed a token belonging to a later claim")
		}
		api.mu.Lock()
		defer api.mu.Unlock()
		if len(api.failed) != 1 || api.failed[0].Attempt != 3 || api.failed[0].Phase != "apply" {
			t.Errorf("failure changed the original claim: %+v", api.failed)
		}
	})
}

func TestClaimRecovery_UnconfirmedReportRetainsCapacityAcrossDeliveryWindows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		api := &recoveryAPI{
			recoverAt: started.Add(time.Hour), failure: "token", boundRequests: true,
			capabilities: []string{runnercontract.CapabilityJobToken, runnercontract.CapabilityClaimRecovery},
		}
		var reports []runnercontract.JobCompleteRequest
		transport := claimRecoveryRoundTrip(func(request *http.Request) (*http.Response, error) {
			if strings.HasSuffix(request.URL.Path, "run-recovery/complete") {
				var report runnercontract.JobCompleteRequest
				if err := json.NewDecoder(request.Body).Decode(&report); err != nil {
					return nil, err
				}
				reports = append(reports, report)
				if time.Since(started) < 260*time.Second {
					return recoveryResponse(request, http.StatusBadGateway, nil)
				}
				return recoveryResponse(request, http.StatusOK, nil)
			}
			return api.roundTrip(request)
		})
		launcher, d, cancel, done := runRecoveryLauncher(t, api, transport)
		defer finishRecoveryLauncher(t, d, cancel, done)
		time.Sleep(250 * time.Second)
		synctest.Wait()
		if launcher.running() != 2 || len(d.placements()) != 1 {
			t.Error("unconfirmed failed-start delivery lost its reservation or placed user work")
		}
		time.Sleep(25 * time.Second)
		synctest.Wait()
		if launcher.running() != 1 {
			t.Error("confirmed failure did not retire the reservation")
		}
		for _, report := range reports {
			original, _ := json.Marshal(reports[0])
			actual, _ := json.Marshal(report)
			if !bytes.Equal(original, actual) {
				t.Error("retry windows changed the original payload")
			}
		}
	})
}

// An accepted completion needs no optional response body, including across the parent's deadline.
type blockedCompletionBody struct{ ctx context.Context }

func (body blockedCompletionBody) Read([]byte) (int, error) {
	<-body.ctx.Done()
	return 0, body.ctx.Err()
}

func (blockedCompletionBody) Close() error { return nil }

func TestClaimRecovery_ReceivedCompletionHeadersRetireWithoutBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := NewClient("http://control.example.test", "runner-token")
		client.http.Transport = claimRecoveryRoundTrip(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: blockedCompletionBody{request.Context()}, Request: request}, nil
		})
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		started := time.Now()
		if err := client.Fail(ctx, "job-existing", "original failure", runnercontract.JobTokenRequest{Attempt: 1}); err != nil {
			t.Errorf("received committed acknowledgement was discarded: %v", err)
		}
		if time.Since(started) != 0 {
			t.Errorf("optional completion body delayed committed acknowledgement for %s", time.Since(started))
		}
	})
}

func TestClaimRecovery_ExcludedPlacementWritePreservesResponseReadFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := NewClient("http://control.example.test", "runner-token")
		calls := 0
		client.http.Transport = claimRecoveryRoundTrip(func(request *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: blockedCompletionBody{request.Context()}, Request: request}, nil
		})
		if err := client.ReportPlacement(t.Context(), "job-existing", "job-token", runnercontract.Placement{ImageDigest: "sha256:fixture"}); err == nil {
			t.Error("excluded placement write lost its existing response-body failure")
		}
		if calls != 1 {
			t.Errorf("excluded placement write retried %d times", calls)
		}
	})
}

func TestClaimRecovery_UnconfirmedReportRespectsIssuedCredentialDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		api := &recoveryAPI{boundRequests: true, capabilities: []string{runnercontract.CapabilityJobToken, runnercontract.CapabilityClaimRecovery}}
		transport := claimRecoveryRoundTrip(func(request *http.Request) (*http.Response, error) {
			if strings.HasSuffix(request.URL.Path, "run-recovery/token") {
				return recoveryResponse(request, http.StatusOK, runnercontract.JobToken{
					Token: "original-token", JobID: "run-recovery", Attempt: 3, Phase: "apply", RunType: "apply-destroy",
					ContractVersion: runnercontract.Version, ExpiresAt: started.Add(30 * time.Second), TimeoutSeconds: 10,
				})
			}
			if strings.HasSuffix(request.URL.Path, "run-recovery/complete") {
				return recoveryResponse(request, http.StatusServiceUnavailable, nil)
			}
			return api.roundTrip(request)
		})
		launcher, d, cancel, done := runRecoveryLauncher(t, api, transport)
		defer finishRecoveryLauncher(t, d, cancel, done)
		time.Sleep(3 * time.Second)
		synctest.Wait()
		d.end("run-recovery", 2)
		time.Sleep(20 * time.Second)
		synctest.Wait()
		if launcher.running() != 2 {
			t.Error("unconfirmed report released capacity before its credential deadline")
		}
		time.Sleep(15 * time.Second)
		synctest.Wait()
		if launcher.running() != 1 {
			t.Error("unconfirmed report exceeded its issued credential deadline")
		}
		if _, removed := d.removal("run-recovery"); !removed {
			t.Error("expired delivery retained an ended placement")
		}
	})
}

func TestClaimRecovery_SingleLauncherKeepsHeartbeatsDuringAcquisition(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		api := &recoveryAPI{
			single: true, jobOffers: recoveryOffers()[1:], recoverAt: time.Now().Add(95 * time.Second), failure: "token", boundRequests: true,
			capabilities: []string{runnercontract.CapabilityJobToken, runnercontract.CapabilityClaimRecovery},
		}
		_, d, cancel, done := runRecoveryLauncher(t, api)
		defer finishRecoveryLauncher(t, d, cancel, done)
		time.Sleep(105 * time.Second)
		synctest.Wait()
		api.mu.Lock()
		defer api.mu.Unlock()
		if len(api.heartbeats) < 40 {
			t.Errorf("single launcher had %d heartbeats during acquisition", len(api.heartbeats))
		}
		if placements := d.placements(); len(placements) != 1 || placements[0].JobID != "run-recovery" {
			t.Errorf("single launcher placements %+v", placements)
		}
		if api.startCalls != 1 || len(api.failed) != 0 {
			t.Errorf("single launcher claims %d failures %+v", api.startCalls, api.failed)
		}
	})
}

func TestClaimRecovery_SingleLauncherContinuesAfterLosingCandidate(t *testing.T) {
	for _, denial := range []int{http.StatusConflict, http.StatusForbidden} {
		t.Run(fmt.Sprint(denial), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				api := &recoveryAPI{
					single: true, jobOffers: recoveryOffers()[:1], boundRequests: true,
					capabilities: []string{runnercontract.CapabilityJobToken, runnercontract.CapabilityClaimRecovery},
				}
				foreignReports := 0
				transport := claimRecoveryRoundTrip(func(request *http.Request) (*http.Response, error) {
					if strings.HasSuffix(request.URL.Path, "job-existing/start") {
						api.mu.Lock()
						api.offered = false
						api.jobOffers = recoveryOffers()[1:]
						api.mu.Unlock()
						code := runnercontract.ErrorCodeJobTerminal
						if denial == http.StatusForbidden {
							code = ""
						}
						return recoveryResponse(request, denial, map[string]any{"errors": []map[string]string{{"code": code}}})
					}
					if strings.HasSuffix(request.URL.Path, "job-existing/complete") {
						foreignReports++
						return recoveryResponse(request, http.StatusForbidden, nil)
					}
					return api.roundTrip(request)
				})
				_, d, cancel, done := runRecoveryLauncher(t, api, transport)
				defer finishRecoveryLauncher(t, d, cancel, done)
				time.Sleep(5 * time.Second)
				synctest.Wait()
				if placements := d.placements(); len(placements) != 1 || placements[0].JobID != "run-recovery" {
					t.Errorf("single launcher stopped after losing a candidate: %+v", placements)
				}
				if foreignReports != 0 {
					t.Errorf("reported a foreign candidate failed %d times", foreignReports)
				}
			})
		})
	}
}

func TestClaimRecovery_HardDrainCannotBeMistakenForPlacementFailure(t *testing.T) {
	for iteration := range 32 {
		t.Run(fmt.Sprint(iteration), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := &digestDriver{fakeDriver: newFakeDriver("container"), digest: "sha256:fixture"}
				launcher := New(Config{ServerURL: "http://control.example.test", RegistrationToken: "runner-token", StatusInterval: time.Hour}, d)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				launcher.client.http.Transport = claimRecoveryRoundTrip(func(request *http.Request) (*http.Response, error) {
					if strings.HasSuffix(request.URL.Path, "/output") {
						cancel()
						if request.Context().Err() == nil {
							t.Error("image digest report escaped the worker hard deadline")
						}
						// An in-progress independent placement report lets the driver waiter observe
						// cancellation before supervision resumes and handles its ready channels.
						time.Sleep(5 * time.Second)
					}
					return recoveryResponse(request, http.StatusOK, nil)
				})
				placement := driver.Placement{ID: "hard-drain", Labels: map[string]string{driver.LabelJob: "job-existing"}}
				done := make(chan struct{})
				go func() {
					defer close(done)
					launcher.watch(ctx, &supervised{jobID: "job-existing", placement: placement, token: "job-token"}, 0)
				}()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("hard drain did not finish")
				}
				d.mu.Lock()
				defer d.mu.Unlock()
				if !slices.Equal(d.killed, []string{"job-existing"}) || len(d.removed) != 1 {
					t.Errorf("hard drain discarded a live placement, killed %v removed %d", d.killed, len(d.removed))
				}
			})
		})
	}
}
