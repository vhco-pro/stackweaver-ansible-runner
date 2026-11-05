// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// A launcher started while the API is unavailable keeps trying and registers once the API is up,
// without restarting (#933, D3, AC15). The fake API answers 503 and then refuses the connection
// before it starts accepting the registration.
func TestRegisterRetriesUntilTheAPIIsUp(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/runner/register" {
			http.NotFound(w, r)
			return
		}
		if calls.Add(1) < 4 {
			http.Error(w, "starting up", http.StatusServiceUnavailable)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer registration-secret" {
			t.Errorf("Authorization = %q, want the registration secret", got)
		}
		var req runnercontract.RegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name != "launcher-a" {
			t.Errorf("register body = %+v (%v)", req, err)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(runnercontract.RegisterResponse{RunnerID: "runner-1", RunnerAPIKey: "swr-runner-token"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "registration-secret")
	c.backoff = Backoff{Initial: time.Millisecond, Max: 5 * time.Millisecond}
	resp, err := c.Register(t.Context(), runnercontract.RegisterRequest{AgentPoolID: "pool-1", Name: "launcher-a"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.RunnerID != "runner-1" || c.RunnerID() != "runner-1" {
		t.Errorf("registered as %q / %q, want runner-1", resp.RunnerID, c.RunnerID())
	}
	if calls.Load() != 4 {
		t.Errorf("register was tried %d times, want 4", calls.Load())
	}
	// Every later call authenticates with the runner token, not the registration secret.
	if c.token() != "swr-runner-token" {
		t.Errorf("the client's token after registration = %q, want the runner token", c.token())
	}
}

// A registration the API keeps failing ends when the launcher is told to stop.
func TestRegisterStopsWithItsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "registration-secret")
	c.backoff = Backoff{Initial: time.Millisecond, Max: 2 * time.Millisecond}
	ctx, cancel := contextWithTimeout(t, 50*time.Millisecond)
	defer cancel()
	if _, err := c.Register(ctx, runnercontract.RegisterRequest{AgentPoolID: "pool-1", Name: "launcher-a"}); err == nil {
		t.Fatal("Register returned no error once its context ended")
	}
}
