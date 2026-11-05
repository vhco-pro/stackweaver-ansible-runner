// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// /healthz answers while the process runs; /readyz only once the launcher is registered, its
// heartbeat is fresh and its driver can place (#933, D3, AC75).
func TestHealthAndReadiness(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	d := newFakeDriver("container")
	l := New(testConfig(api), d)
	l.goos = "linux"
	h := l.HealthHandler()
	get := func(path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		return rec.Code
	}

	if get("/healthz") != http.StatusOK {
		t.Error("/healthz is not 200 before registration")
	}
	if get("/readyz") != http.StatusServiceUnavailable {
		t.Error("an unregistered launcher is ready")
	}

	runLauncher(t, l)
	eventually(t, "readiness", func() bool { return get("/readyz") == http.StatusOK })

	d.mu.Lock()
	d.pingErr = errors.New("the socket is gone")
	d.mu.Unlock()
	if get("/readyz") != http.StatusServiceUnavailable {
		t.Error("a launcher whose driver cannot place is ready")
	}
	d.mu.Lock()
	d.pingErr = nil
	d.mu.Unlock()

	l.lastBeat.Store(time.Now().Add(-time.Hour).UnixNano())
	if err := l.Ready(t.Context()); err == nil {
		t.Error("a launcher with a stale heartbeat is ready")
	}
}
