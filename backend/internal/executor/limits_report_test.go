// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// The executor writes the driver's limits report into the job's output before the job starts, so
// the run or job states which limits were enforced (#933, D4, AC24): from the environment a
// container or pod driver set, or from the in-process driver's invocation.
func TestPreflightWritesTheLimitsReport(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var req runnercontract.JobOutputRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		lines = append(lines, req.Output)
		mu.Unlock()
	}))
	defer srv.Close()
	t.Setenv(EnvNetworkGuard, "")

	t.Setenv(EnvLimitsReport, "resource limits enforced: cpu=1")
	if !NewClient(srv.URL, "token", "runner-1").Preflight(t.Context(), "job-1", "stdout") {
		t.Fatal("preflight refused the job")
	}
	t.Setenv(EnvLimitsReport, "")
	c := NewClient(srv.URL, "token", "runner-1")
	c.SetLimitsReport("resource limits not enforced by this driver or engine: memory=1Gi")
	if !c.Preflight(t.Context(), "job-2", "stdout") {
		t.Fatal("preflight refused the job")
	}
	c = NewClient(srv.URL, "token", "runner-1")
	if !c.Preflight(t.Context(), "job-3", "stdout") {
		t.Fatal("preflight refused the job")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 2 || !strings.Contains(lines[0], "enforced: cpu=1") || !strings.Contains(lines[1], "not enforced") || !strings.Contains(lines[1], "memory=1Gi") {
		t.Errorf("job output = %q, want one report line for each of the first two jobs and none for the third", lines)
	}
}
