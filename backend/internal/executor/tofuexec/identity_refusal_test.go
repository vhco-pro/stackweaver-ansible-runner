// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package tofuexec

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/michielvha/stackweaver/backend/internal/executor"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// TestExecute_ARefusedIdentityFailsTheRunWithItsReason pins that a run the API refuses its
// artifacts, because its configured identity cannot be minted, fails with the API's reason and
// not with the raw error document (#933, D8, AC37).
func TestExecute_ARefusedIdentityFailsTheRunWithItsReason(t *testing.T) {
	const reason = `the run's AWS per-run identity could not be minted: invalid name "a:b"`
	var completed runnercontract.JobCompleteRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/artifacts"):
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"status": "409", "title": "Conflict", "detail": reason}}})
		case strings.HasSuffix(r.URL.Path, "/complete"):
			_ = json.NewDecoder(r.Body).Decode(&completed)
		}
	}))
	defer srv.Close()

	res := Execute(t.Context(), executor.NewClient(srv.URL, "token", "runner-1"), Job{ID: "run-1", RunType: "plan"}, Options{})
	if res.Status != "failed" || completed.Status != "failed" {
		t.Fatalf("status = %q, reported %q; want failed", res.Status, completed.Status)
	}
	if !strings.Contains(completed.ErrorMessage, reason) {
		t.Errorf("error message = %q, want the reason", completed.ErrorMessage)
	}
	if strings.Contains(completed.ErrorMessage, `{"errors"`) {
		t.Errorf("error message carries the raw document: %q", completed.ErrorMessage)
	}
}
