// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// An executor whose completion never reached the API exits with ExitUnreported, which a launcher
// reads as "nothing was reported" and reports itself; otherwise a job whose executor could not
// reach the API (a server address its pod cannot resolve) stays running forever.
func TestExitCode_AnUnreportedCompletionLeavesTheReportToTheLauncher(t *testing.T) {
	answering := func(code int) *Client {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
		t.Cleanup(srv.Close)
		return NewClient(srv.URL, "tok", "r1")
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	unreachable := NewClient(gone.URL, "tok", "r1")
	gone.Close()

	for _, tc := range []struct {
		name   string
		client *Client
		status string
		want   int
	}{
		{"completed, reported", answering(http.StatusOK), "completed", ExitCompleted},
		{"failed, reported", answering(http.StatusOK), "failed", ExitFailed},
		{"failed, the API already holds a completion", answering(http.StatusConflict), "failed", ExitFailed},
		{"failed, the job was deleted", answering(http.StatusNotFound), "failed", ExitFailed},
		{"failed, the API unreachable", unreachable, "failed", ExitUnreported},
		{"completed, the API unreachable", unreachable, "completed", ExitUnreported},
		{"failed, the API refusing", answering(http.StatusInternalServerError), "failed", ExitUnreported},
		{"canceled, the API unreachable", unreachable, "canceled", ExitStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = tc.client.Complete(t.Context(), "run-x", runnercontract.JobCompleteRequest{Status: tc.status})
			if got := tc.client.ExitCode(tc.status); got != tc.want {
				t.Errorf("ExitCode(%q) = %d, want %d", tc.status, got, tc.want)
			}
		})
	}
}

// A client that never reported anything has nothing reported.
func TestExitCode_NoCompletionAttemptedIsUnreported(t *testing.T) {
	if got := NewClient("http://127.0.0.1:1", "tok", "r1").ExitCode("failed"); got != ExitUnreported {
		t.Errorf("ExitCode = %d, want ExitUnreported", got)
	}
}
