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

// A heartbeat a proxy cuts is polled again at once, with nothing logged, and the offer it would
// have carried arrives on the next one (#933, D3, AC70).
func TestPollRepollsACutConnection(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req runnercontract.HeartbeatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.WaitSeconds != 20 {
			t.Errorf("heartbeat wait = %d, want 20", req.WaitSeconds)
		}
		if calls.Add(1) == 1 {
			// The proxy's idle timeout: the connection is closed with no answer.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		_ = json.NewEncoder(w).Encode(runnercontract.HeartbeatResponse{PendingJobs: []runnercontract.PendingJob{{JobID: "run-1"}}})
	}))
	defer srv.Close()

	var logged atomic.Int32
	p := &Poller{Client: NewClient(srv.URL, "registration"), Wait: 20 * time.Second}
	p.Client.Log = func(string, ...any) { logged.Add(1) }
	start := time.Now()
	resp, err := p.Poll(t.Context(), runnercontract.HeartbeatRequest{Status: "online"})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(resp.PendingJobs) != 1 || resp.PendingJobs[0].JobID != "run-1" {
		t.Fatalf("offers = %+v", resp.PendingJobs)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the re-poll after a cut connection took %s, want it at once", took)
	}
	if logged.Load() != 0 {
		t.Errorf("a single cut connection was logged %d time(s), want none", logged.Load())
	}
}

// An API that keeps failing is polled with backoff and logged, until the launcher stops.
func TestPollBacksOffAFailingAPI(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()
	var logged atomic.Int32
	p := &Poller{Client: NewClient(srv.URL, "registration"), Wait: time.Second}
	p.Client.Log = func(string, ...any) { logged.Add(1) }
	p.Client.backoff = Backoff{Initial: 20 * time.Millisecond, Max: 50 * time.Millisecond}
	ctx, cancel := contextWithTimeout(t, 400*time.Millisecond)
	defer cancel()
	if _, err := p.Poll(ctx, runnercontract.HeartbeatRequest{}); err == nil {
		t.Fatal("Poll returned no error once its context ended")
	}
	if n := calls.Load(); n < 3 || n > 40 {
		t.Errorf("a failing API was polled %d times in 400ms, want a backed-off handful", n)
	}
	if logged.Load() == 0 {
		t.Error("repeated heartbeat failures were never logged")
	}
}
