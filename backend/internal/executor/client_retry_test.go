// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// During a rolling upgrade the executor's requests reach API replicas of two releases (#933, D14,
// AC72). A replica that predates job tokens answers a valid one 401, and a proxy whose replica is
// going away answers 502, 503 or 504; the executor sends the same request again rather than lose
// its output, its plan, its state or its completion. A coded 403 stops the job, and every other
// answer is final, as before.

// flakyAPI answers the first len(first) requests with those statuses and every later one with
// 200, recording each request body.
type flakyAPI struct {
	mu     sync.Mutex
	first  []int
	bodies []string
	srv    *httptest.Server
}

func newFlakyAPI(t *testing.T, first ...int) *flakyAPI {
	t.Helper()
	f := &flakyAPI{first: first}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(body))
		n := len(f.bodies)
		f.mu.Unlock()
		if n <= len(f.first) {
			w.WriteHeader(f.first[n-1])
			_, _ = w.Write([]byte(`{"errors":[{"status":"` + http.StatusText(f.first[n-1]) + `"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","contract_version":1}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *flakyAPI) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

// fastRetry shortens the waits between attempts for the test.
func fastRetry(t *testing.T) {
	t.Helper()
	first, maxWait := retryFirstWait, retryMaxWait
	retryFirstWait, retryMaxWait = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { retryFirstWait, retryMaxWait = first, maxWait })
}

func TestClient_SendsAgainThroughAnUpgrade(t *testing.T) {
	fastRetry(t)
	calls := map[string]func(c *Client) error{
		"output": func(c *Client) error {
			return c.Output(t.Context(), "run-1", runnercontract.JobOutputRequest{Output: "line\n", Stream: "plan"})
		},
		"complete": func(c *Client) error {
			return c.Complete(t.Context(), "run-1", runnercontract.JobCompleteRequest{Status: "completed"})
		},
		"plan":     func(c *Client) error { return c.UploadPlan(t.Context(), "run-1", []byte("PK plan")) },
		"state":    func(c *Client) error { return c.UploadState(t.Context(), "run-1", []byte(`{"version":4}`)) },
		"snapshot": func(c *Client) error { return c.UploadSnapshot(t.Context(), "wi-1", "abc", []byte("tarball")) },
		"artifacts": func(c *Client) error {
			var arts runnercontract.TofuArtifacts
			return c.Artifacts(t.Context(), "run-1", &arts, func() int { return arts.ContractVersion })
		},
		"get": func(c *Client) error { _, err := c.Get(t.Context(), "/api/v2/runner/jobs/run-1/plan"); return err },
		"download": func(c *Client) error {
			return c.Download(t.Context(), "/api/v2/runner/jobs/1/playbook-snapshot", filepath.Join(t.TempDir(), "snap"), 1<<20)
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			api := newFlakyAPI(t, http.StatusUnauthorized, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusUnauthorized)
			if err := call(NewClient(api.srv.URL, "swj-token", "runner-1")); err != nil {
				t.Fatalf("%s through an upgrade: %v, want it delivered once a replica of the new release answers", name, err)
			}
			got := api.requests()
			if len(got) != 6 {
				t.Fatalf("%s was sent %d times, want 6: five refusals of an upgrade, then the delivery", name, len(got))
			}
			for i, b := range got {
				if b != got[0] {
					t.Errorf("attempt %d sent %q, want the same body as the first, %q", i+1, b, got[0])
				}
			}
		})
	}
}

func TestClient_AFinalAnswerIsNotSentAgain(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		wantStop bool
	}{
		{"job-token-revoked", http.StatusForbidden, `{"errors":[{"status":"403","code":"job-token-revoked"}]}`, true},
		{"job-terminal", http.StatusForbidden, `{"errors":[{"status":"403","code":"job-terminal"}]}`, true},
		{"a bare 403", http.StatusForbidden, `{"errors":[{"status":"403"}]}`, false},
		{"a conflict", http.StatusConflict, `{"errors":[{"status":"409"}]}`, false},
		{"too large", http.StatusRequestEntityTooLarge, `{"errors":[{"status":"413"}]}`, false},
		{"a server error", http.StatusInternalServerError, `oops`, false},
		{"not found", http.StatusNotFound, `{"errors":[{"status":"404"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var n int
			var mu sync.Mutex
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				n++
				mu.Unlock()
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			err := NewClient(srv.URL, "swj-token", "runner-1").Complete(t.Context(), "run-1", runnercontract.JobCompleteRequest{Status: "completed"})
			var se *StatusError
			if !errors.As(err, &se) || se.Code != tc.status || se.Stops() != tc.wantStop {
				t.Fatalf("Complete = %v, want status %d (stops %t)", err, tc.status, tc.wantStop)
			}
			mu.Lock()
			defer mu.Unlock()
			if n != 1 {
				t.Errorf("sent %d times, want once: %s is the API's answer", n, tc.name)
			}
		})
	}
}

func TestClient_StopsSendingAgainWhenItsContextEnds(t *testing.T) {
	fastRetry(t)
	api := newFlakyAPI(t, slices.Repeat([]int{http.StatusUnauthorized}, 1_000_000)...) // a replica that never learns job tokens
	ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := NewClient(api.srv.URL, "swj-token", "runner-1").Output(ctx, "run-1", runnercontract.JobOutputRequest{Output: "x", Stream: "plan"})
	if err == nil {
		t.Fatal("Output succeeded against a replica that refuses every request")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Output returned after %s, want it bounded by its context", elapsed)
	}
	if n := len(api.requests()); n < 2 {
		t.Errorf("Output was sent %d times before its context ended, want it sent again", n)
	}
}

// The status poll is not sent again inside one poll: the watcher's interval is its retry, so a
// stop answer is acted on at the next tick, never later.
func TestClient_StatusIsOnePollPerTick(t *testing.T) {
	api := newFlakyAPI(t, http.StatusUnauthorized)
	if _, err := NewClient(api.srv.URL, "swj-token", "runner-1").Status(t.Context(), "run-1"); err == nil {
		t.Fatal("Status hid a 401 from an older replica, want it returned for the next tick")
	}
	if got := len(api.requests()); got != 1 {
		t.Errorf("Status sent %d requests in one poll, want 1", got)
	}
}

// Executors that start retrying together must not stay in step: behind a round-robin balancer
// with one replica of the old release, executors retrying on the same schedule can keep reaching
// that replica. Each wait is drawn between half the backoff and the whole of it.
func TestRetryWaitIsJittered(t *testing.T) {
	const wait = 400 * time.Millisecond
	seen := map[time.Duration]bool{}
	for range 200 {
		d := jittered(wait)
		if d < wait/2 || d > wait {
			t.Fatalf("jittered(%s) = %s, want within [%s, %s]", wait, d, wait/2, wait)
		}
		seen[d] = true
	}
	if len(seen) < 50 {
		t.Errorf("200 draws gave %d distinct waits: the retries would stay in step", len(seen))
	}
}
