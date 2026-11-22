// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
)

// An indeterminate status must leave the placement alive until an explicit cancel arrives.
func TestSupervisionRetainsPlacementAcrossIndeterminateStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"bare404", 404, `{"errors":[{"status":"404","title":"Not Found"}]}`},
		{"malformed404", 404, `upstream unavailable`},
		{"bare401", 401, `{"errors":[{"title":"Unauthorized"}]}`},
		{"bare403", 403, `{"errors":[{"title":"Forbidden"}]}`},
		{"revoked403", 403, `{"errors":[{"code":"job-token-revoked"}]}`},
		{"terminal403", 403, `{"errors":[{"code":"job-terminal"}]}`},
		{"wrongCode404", 404, `{"errors":[{"code":"job-terminal"}]}`},
		{"server500", 500, `{"errors":[{"code":"job-deleted"}]}`},
		{"gateway502", 502, `bad gateway`},
		{"unavailable503", 503, `unavailable`},
		{"timeout504", 504, `gateway timeout`},
		{"network", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var polls, reports atomic.Int32
			nextPoll := make(chan struct{})
			resume := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v2/runner/jobs/job-status/status" {
					reports.Add(1)
					w.WriteHeader(http.StatusOK)
					return
				}
				if polls.Add(1) == 1 {
					if tc.status == 0 {
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Errorf("hijacking status connection: %v", err)
							return
						}
						if err := connection.Close(); err != nil {
							t.Errorf("closing status connection: %v", err)
						}
						return
					}
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
					return
				}
				if polls.Load() == 2 {
					close(nextPoll)
				}
				select {
				case <-resume:
					_ = json.NewEncoder(w).Encode(struct {
						Status string `json:"status"`
					}{Status: "canceled"})
				case <-r.Context().Done():
				}
			}))
			t.Cleanup(srv.Close)
			d := newFakeDriver("container")
			killed := make(chan string, 2)
			d.onKill = func(job string) {
				select {
				case killed <- job:
				default:
				}
			}
			l := New(Config{ServerURL: srv.URL, RegistrationToken: "launcher-token", StatusInterval: time.Millisecond}, d)
			placement := driver.Placement{ID: "status-placement", Labels: map[string]string{driver.LabelJob: "job-status"}}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() {
				defer close(done)
				l.watch(ctx, &supervised{jobID: "job-status", placement: placement, token: "job-token"}, 0)
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("supervision did not stop after test cancellation")
				}
			})
			select {
			case job := <-killed:
				t.Fatalf("indeterminate status stopped placement %s", job)
			case <-nextPoll:
			case <-time.After(5 * time.Second):
				t.Fatal("supervision did not poll again after indeterminate status")
			}
			d.mu.Lock()
			killCount := len(d.killed)
			d.mu.Unlock()
			if killCount != 0 {
				t.Fatal("indeterminate status killed the placement before the next poll")
			}
			close(resume)
			select {
			case <-killed:
			case <-time.After(5 * time.Second):
				t.Fatal("explicit cancellation did not stop the placement")
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("canceled placement was not removed")
			}
			if removed, ok := d.removal("job-status"); !ok || !removed.Canceled {
				t.Fatalf("canceled placement removal = %+v, present %t", removed, ok)
			}
			if reports.Load() != 0 {
				t.Fatal("status uncertainty caused a synthetic completion report")
			}
		})
	}
}
