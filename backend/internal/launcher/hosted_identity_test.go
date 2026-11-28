// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// The API refuses an identity it cannot hand to this launcher with 409 at the job token (#1113,
// D3; AC-19, AC-21). The launcher places nothing and reports the claimed job failed through the
// failed-start report, with the runner key, saying it could not obtain a job token and why.
func TestTokenRefusalIsReportedThroughTheFailedStart(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken, runnercontract.CapabilityClaimRecovery)
	reason := `identity binding "files" (azure-workload-identity) needs the kubernetes driver; this pool's launcher uses the container driver; run the job on a Kubernetes pool or select no binding`
	api.mu.Lock()
	api.tokenRefusals["job-refused"] = reason
	api.mu.Unlock()
	d := newFakeDriver("container")
	l := New(testConfig(api), d)
	l.goos = "linux"
	api.offer(runnercontract.PendingJob{JobID: "job-refused", JobType: "ansible_job"})

	runLauncher(t, l)
	eventually(t, "the refused job reported", func() bool { _, _, failed, _ := api.snapshot(); return failed["job-refused"] != "" })
	_, tokens, failed, _ := api.snapshot()
	msg := failed["job-refused"]
	// The report carries the API's error document, so the reason appears JSON-escaped.
	escaped, err := json.Marshal(reason)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(msg, "the launcher could not obtain a job token:") || !strings.Contains(msg, strings.Trim(string(escaped), `"`)) {
		t.Errorf("failure = %q, want the job token refusal and its reason", msg)
	}
	api.mu.Lock()
	by := api.failedBy["job-refused"]
	api.mu.Unlock()
	if by != "swr-1" {
		t.Errorf("failure reported with %q, want the runner key of the failed-start report", by)
	}
	if len(tokens) != 0 {
		t.Errorf("tokens issued = %v, want none", tokens)
	}
	if n := len(d.placements()); n != 0 {
		t.Errorf("placed %d jobs, want none", n)
	}
}

// A hosted launcher whose job namespace has no ServiceAccount of the binding's name fails the job
// with the driver's reason and runs nothing (#1113, D3; AC-10). The token was issued, so the
// failure travels with the attempt's job token, not the failed-start report.
func TestMissingServiceAccountFailsTheJobWithTheDriversReason(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken, runnercontract.CapabilityClaimRecovery)
	api.mu.Lock()
	api.identities = map[string]*runnercontract.Identity{"job-no-sa": {
		BindingID: "4f2c1a0e-0000-5000-8000-000000000001", Mechanism: "azure-workload-identity", ServiceAccount: "stackweaver-hosted-identity",
		Annotations: map[string]string{"azure.workload.identity/client-id": "11111111-2222-3333-4444-555555555555"},
	}}
	api.mu.Unlock()
	d := newFakeDriver("kubernetes")
	d.placeErr = errors.New(`creating the job pod: serviceaccounts "stackweaver-hosted-identity" not found`)
	l := New(testConfig(api), d)
	l.goos = "linux"
	api.offer(runnercontract.PendingJob{JobID: "job-no-sa", JobType: "ansible_job"})

	runLauncher(t, l)
	eventually(t, "the job reported", func() bool { _, _, failed, _ := api.snapshot(); return failed["job-no-sa"] != "" })
	_, _, failed, _ := api.snapshot()
	if msg := failed["job-no-sa"]; !strings.Contains(msg, `serviceaccounts "stackweaver-hosted-identity" not found`) {
		t.Errorf("failure = %q, want the driver's reason", msg)
	}
	api.mu.Lock()
	by := api.failedBy["job-no-sa"]
	api.mu.Unlock()
	if by != "swj-job-no-sa" {
		t.Errorf("failure reported with %q, want the attempt's job token", by)
	}
	if n := len(d.placements()); n != 0 {
		t.Errorf("placed %d jobs, want none", n)
	}
}
