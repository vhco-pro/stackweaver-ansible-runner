// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// TestStatusPollStopRule pins how an executor reads its status poll (#933, D2, AC67, AC72): a 403
// whose JSON:API code is job-token-revoked or job-terminal means the job is over for this executor,
// exactly like canceled; a bare 401, a 403 without that code, a 5xx or a network error is retried,
// because during a rolling upgrade an older API replica answers 401 to a valid job token.
func TestStatusPollStopRule(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantStop bool
		wantErr  bool
	}{
		{"running", 200, `{"status":"running"}`, false, false},
		{"canceled", 200, `{"status":"canceled"}`, true, false},
		{"revoked", 403, `{"errors":[{"status":"403","code":"job-token-revoked","title":"Forbidden"}]}`, true, false},
		{"terminal", 403, `{"errors":[{"status":"403","code":"job-terminal","title":"Forbidden"}]}`, true, false},
		{"job deleted", 404, `{"errors":[{"status":"404","title":"Not Found"}]}`, true, false},
		{"bare 403", 403, `{"errors":[{"status":"403","title":"Forbidden"}]}`, false, true},
		{"bare 401", 401, `{"errors":[{"status":"401","title":"Unauthorized"}]}`, false, true},
		{"server error", 502, `bad gateway`, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			c := NewClient(srv.URL, "token", "runner-1")
			stop, err := c.Status(t.Context(), "job-1")
			if stop != tt.wantStop || (err != nil) != tt.wantErr {
				t.Fatalf("Status = stop %t, err %v; want stop %t, err %t", stop, err, tt.wantStop, tt.wantErr)
			}
		})
	}
}

// TestWatchCancellation checks the watcher stops the job on a stop answer and keeps polling through
// errors.
func TestWatchCancellation(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusUnauthorized) // an old replica during an upgrade
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":[{"status":"403","code":"job-token-revoked"}]}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "token", "runner-1")

	ctx, cancel := context.WithCancelCause(t.Context())
	go c.WatchCancellation(ctx, "job-1", 10*time.Millisecond, cancel)
	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), ErrJobStopped) {
			t.Fatalf("stopped with cause %v, want ErrJobStopped", context.Cause(ctx))
		}
		if calls.Load() < 3 {
			t.Fatalf("stopped after %d polls, want it to retry through the 401s", calls.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher never stopped the job")
	}
}

// A placed executor reports where it runs with its first output, once (#933, D4, AC77).
func TestOutputReportsThePlacementOnce(t *testing.T) {
	t.Setenv(EnvPlaced, "1")
	t.Setenv("STACKWEAVER_NODE_NAME", "node-7")
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Placement *struct {
				PlacementID string `json:"placement_id"`
				Node        string `json:"node"`
			} `json:"placement"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Placement != nil {
			seen = append(seen, req.Placement.PlacementID+"@"+req.Placement.Node)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "token", "runner-1")
	for range 3 {
		_ = c.Output(t.Context(), "job-1", runnercontract.JobOutputRequest{Output: "line\n", Stream: "stdout"})
	}
	host, _ := os.Hostname()
	if len(seen) != 1 || seen[0] != host+"@node-7" {
		t.Errorf("placements reported = %v, want one with this host and node-7", seen)
	}
}

// A placement the driver handed the executor (in-process) wins over the environment, and a placed
// executor reports the image digest its driver told it (#933, D4, AC77).
func TestOutputReportsTheDriversPlacementAndDigest(t *testing.T) {
	t.Setenv(EnvPlaced, "1")
	t.Setenv("STACKWEAVER_IMAGE_DIGEST", "sha256:feed")
	var seen []runnercontract.Placement
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var req runnercontract.JobOutputRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Placement != nil {
			seen = append(seen, *req.Placement)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "token", "runner-1")
	_ = c.Output(t.Context(), "job-1", runnercontract.JobOutputRequest{Output: "x", Stream: "stdout"})
	if len(seen) != 1 || seen[0].ImageDigest != "sha256:feed" {
		t.Errorf("placed executor reported %+v, want the digest sha256:feed", seen)
	}

	seen = nil
	t.Setenv(EnvPlaced, "")
	c = NewClient(srv.URL, "token", "runner-1")
	c.SetPlacement(&runnercontract.Placement{PlacementID: "inprocess-job-1-1", Node: "host-a"})
	_ = c.Output(t.Context(), "job-1", runnercontract.JobOutputRequest{Output: "x", Stream: "stdout"})
	if len(seen) != 1 || seen[0].PlacementID != "inprocess-job-1-1" || seen[0].Node != "host-a" {
		t.Errorf("in-process executor reported %+v, want the driver's placement", seen)
	}
}
