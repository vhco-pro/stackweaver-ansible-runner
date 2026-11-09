// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// The OpenTofu executor against a fake control plane (#933, D2): the saved plan travels from the
// plan phase to the apply phase through the API and is applied as it is (AC6), the apply streams
// per-resource phase state (AC8), the job's registry token and the deployment's mirror block reach
// tofu, and an unknown contract version is refused before anything runs (AC72).
//
// The tofu binary is a shell script installed where core/tofu looks for a versioned binary. It
// writes a plan file whose content names the run, applies only a plan file it is given and records
// what it was handed.

package tofuexec

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/michielvha/stackweaver/backend/internal/executor"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

const stubTofuVersion = "0.0.0-executetest"

// stubTofu is the stand-in engine. $STUB_RECORD is where it records; a file named "stale" there
// makes apply refuse the saved plan the way OpenTofu does when the state moved.
const stubTofu = `#!/bin/sh
rec="$STUB_RECORD"
sub="$1"
echo "$@" >> "$rec/calls"
env > "$rec/$sub.env"
case "$sub" in
init)
  if [ -n "$TF_CLI_CONFIG_FILE" ]; then
    cp "$TF_CLI_CONFIG_FILE" "$rec/tofurc"
    p=$(sed -n 's/^ *path *= *"\(.*\)"$/\1/p' "$TF_CLI_CONFIG_FILE")
    [ -n "$p" ] && (cd "$p" && find . -type f | sort > "$rec/fsmirror")
  fi
  [ -f .terraform.lock.hcl ] && cp .terraform.lock.hcl "$rec/init_lock"
  echo "OpenTofu has been successfully initialized!" ;;
plan)
  for a in "$@"; do case "$a" in -out=*) out="${a#-out=}" ;; -var-file=*) cp "${a#-var-file=}" "$rec/varfile" ;; esac; done
  echo "saved-plan-for-$STUB_RUN" > "$out"
  echo "  # null_resource.a will be created"
  echo "Plan: 1 to add, 0 to change, 0 to destroy."
  ;;
show)
  echo '{"resource_changes":[{"address":"null_resource.a","change":{"actions":["create"]}}]}'
  ;;
apply)
  for a in "$@"; do last="$a"; done
  if [ -f "$rec/stale" ]; then
    echo "Error: Saved plan is stale" >&2
    echo "The given plan file can no longer be applied because the state was changed by another operation after the plan was created." >&2
    exit 1
  fi
  cat "$last" > "$rec/applied_plan" 2>/dev/null
  echo "null_resource.a: Creating..."
  echo "null_resource.a: Creation complete after 0s [id=1]"
  echo ""
  echo "Apply complete! Resources: 1 added, 0 changed, 0 destroyed."
  echo '{"version":4,"serial":2}' > terraform.tfstate
  ;;
esac
exit 0
`

// fakeTofuAPI is the slice of the runner control plane an OpenTofu job touches.
type fakeTofuAPI struct {
	mu        sync.Mutex
	artifacts runnercontract.TofuArtifacts
	plan      []byte // stored by POST /plan, served by GET /plan
	outputs   []runnercontract.JobOutputRequest
	state     []byte
	completed *runnercontract.JobCompleteRequest
	planGets  int
}

func (f *fakeTofuAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/artifacts"):
		if err := json.NewEncoder(w).Encode(f.artifacts); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	case strings.HasSuffix(r.URL.Path, "/plan") && r.Method == http.MethodPost:
		f.plan, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"status":"stored"}`))
	case strings.HasSuffix(r.URL.Path, "/plan"):
		f.planGets++
		if f.plan == nil {
			http.Error(w, `{"errors":[{"status":"404","title":"No saved plan for this run"}]}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write(f.plan)
	case strings.HasSuffix(r.URL.Path, "/output"):
		var req runnercontract.JobOutputRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.outputs = append(f.outputs, req)
	case strings.HasSuffix(r.URL.Path, "/state"):
		f.state, _ = io.ReadAll(r.Body)
	case strings.HasSuffix(r.URL.Path, "/complete"):
		var req runnercontract.JobCompleteRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.completed = &req
	case strings.HasSuffix(r.URL.Path, "/status"):
		_, _ = w.Write([]byte(`{"status":"running"}`))
	}
}

// configTarball is a one-file configuration, base64-encoded as the artifacts carry it.
func configTarball(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte(`resource "null_resource" "a" {}` + "\n")
	if err := tw.WriteHeader(&tar.Header{Name: "main.tf", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = gz.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// tofuHarness installs the stand-in engine and a fake API for one test.
type tofuHarness struct {
	api    *fakeTofuAPI
	client *executor.Client
	record string
}

func newTofuHarness(t *testing.T, runID string) *tofuHarness {
	t.Helper()
	home, record := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("STUB_RECORD", record)
	t.Setenv("STUB_RUN", runID)
	versions := filepath.Join(home, ".tofu-versions")
	if err := os.MkdirAll(versions, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versions, "tofu-"+stubTofuVersion), []byte(stubTofu), 0o700); err != nil { //nolint:gosec // G306: the stand-in must be executable
		t.Fatal(err)
	}
	api := &fakeTofuAPI{artifacts: runnercontract.TofuArtifacts{
		JobID: runID, ContractVersion: runnercontract.Version, TofuVersion: stubTofuVersion, ConfigTarball: configTarball(t),
	}}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	return &tofuHarness{api: api, client: executor.NewClient(srv.URL, "placeholder-job-token", "runner-1"), record: record}
}

// passStubEnv lets the stand-in see where to record: the run's environment admits only allowlisted
// variables, so the test hands them over as the run's own.
func (h *tofuHarness) passStubEnv(t *testing.T) {
	t.Helper()
	if h.api.artifacts.EnvironmentVars == nil {
		h.api.artifacts.EnvironmentVars = map[string]string{}
	}
	h.api.artifacts.EnvironmentVars["STUB_RECORD"] = os.Getenv("STUB_RECORD")
	h.api.artifacts.EnvironmentVars["STUB_RUN"] = os.Getenv("STUB_RUN")
}

func (h *tofuHarness) execute(t *testing.T, runType string) Result {
	t.Helper()
	h.passStubEnv(t)
	return Execute(t.Context(), h.client, Job{ID: h.api.artifacts.JobID, RunType: runType}, Options{Console: io.Discard})
}

func (h *tofuHarness) read(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.record, name)) //nolint:gosec // G304: a file this test's stand-in wrote
	if err != nil {
		return ""
	}
	return string(data)
}

// The plan phase uploads its saved plan and the apply phase applies exactly those bytes, with no
// second plan (AC6).
func TestExecute_AppliesTheSavedPlanOfThePlanPhase(t *testing.T) {
	h := newTofuHarness(t, "run-saved-plan")

	if res := h.execute(t, "plan"); res.Status != "completed" {
		t.Fatalf("plan = %+v", res)
	}
	if want := "saved-plan-for-run-saved-plan\n"; string(h.api.plan) != want {
		t.Fatalf("uploaded plan = %q, want the plan phase's tfplan %q", h.api.plan, want)
	}

	// The API now serves the apply phase with the plan descriptor.
	h.api.artifacts.Plan = &runnercontract.PlanDescriptor{Path: "/api/v2/runner/jobs/run-saved-plan/plan", SizeBytes: int64(len(h.api.plan))}
	if err := os.WriteFile(filepath.Join(h.record, "calls"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	res := h.execute(t, "apply")
	if res.Status != "completed" {
		t.Fatalf("apply = %+v", res)
	}
	if got := h.read(t, "applied_plan"); got != "saved-plan-for-run-saved-plan\n" {
		t.Errorf("applied plan = %q, want the plan phase's bytes", got)
	}
	calls := h.read(t, "calls")
	if strings.Contains(calls, "plan ") {
		t.Errorf("the apply phase planned again: %q", calls)
	}
	var applyLine string
	for line := range strings.SplitSeq(calls, "\n") {
		if strings.HasPrefix(line, "apply") {
			applyLine = line
		}
	}
	if !strings.HasSuffix(applyLine, "/tfplan") || strings.Contains(applyLine, "-auto-approve") {
		t.Errorf("apply invocation = %q, want `apply ... <secret dir>/tfplan`", applyLine)
	}
	if string(h.api.state) == "" {
		t.Error("the apply phase uploaded no state")
	}
}

// A plan-only run is never applied, so it uploads no saved plan.
func TestExecute_PlanOnlyUploadsNoSavedPlan(t *testing.T) {
	h := newTofuHarness(t, "run-plan-only")
	if res := h.execute(t, "plan-only"); res.Status != "completed" {
		t.Fatalf("plan-only = %+v", res)
	}
	if h.api.plan != nil {
		t.Errorf("a plan-only run uploaded a saved plan (%q)", h.api.plan)
	}
}

// An apply phase without a saved plan fails instead of planning again: a re-plan would apply
// something nobody reviewed (AC6).
func TestExecute_ApplyWithoutASavedPlanFails(t *testing.T) {
	h := newTofuHarness(t, "run-no-plan")
	res := h.execute(t, "apply")
	if res.Status != "failed" || !strings.Contains(res.Output+errString(res.Err), "saved plan") {
		t.Errorf("apply without a plan = %+v, want failed naming the missing saved plan", res)
	}
	if calls := h.read(t, "calls"); strings.Contains(calls, "apply") || strings.Contains(calls, "plan") {
		t.Errorf("tofu ran %q, want neither a plan nor an apply", calls)
	}
}

// A saved plan the state moved past is refused by tofu, and the run fails with tofu's message
// (AC6), never a silent re-plan.
func TestExecute_StaleSavedPlanFailsTheApply(t *testing.T) {
	h := newTofuHarness(t, "run-stale")
	h.api.plan = []byte("saved-plan-for-run-stale\n")
	h.api.artifacts.Plan = &runnercontract.PlanDescriptor{Path: "/api/v2/runner/jobs/run-stale/plan", SizeBytes: int64(len(h.api.plan))}
	if err := os.WriteFile(filepath.Join(h.record, "stale"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	res := h.execute(t, "apply")
	if res.Status != "failed" {
		t.Fatalf("stale apply = %+v, want failed", res)
	}
	if h.api.completed == nil || !strings.Contains(h.api.completed.ErrorMessage+h.api.completed.Output, "Saved plan is stale") {
		t.Errorf("completion = %+v, want tofu's stale-plan message", h.api.completed)
	}
	if strings.Contains(h.read(t, "calls"), "plan ") {
		t.Error("a stale saved plan was answered with a re-plan")
	}
}

// The apply streams per-resource phase state the API stores as RunPhaseState (AC8): the planned
// resources from the saved plan, completed as the engine reports them.
func TestExecute_StreamsPhaseState(t *testing.T) {
	h := newTofuHarness(t, "run-phase-state")
	h.api.plan = []byte("saved-plan-for-run-phase-state\n")
	h.api.artifacts.Plan = &runnercontract.PlanDescriptor{Path: "/api/v2/runner/jobs/run-phase-state/plan", SizeBytes: int64(len(h.api.plan))}
	if res := h.execute(t, "apply"); res.Status != "completed" {
		t.Fatalf("apply = %+v", res)
	}
	var last *runnercontract.PhaseState
	for _, o := range h.api.outputs {
		if o.PhaseState != nil {
			last = o.PhaseState
		}
	}
	if last == nil {
		t.Fatal("no output carried phase state")
	}
	if last.Phase != "apply" || len(last.Resources) != 1 || last.Resources[0].Address != "null_resource.a" {
		t.Fatalf("final phase state = %+v, want the apply of null_resource.a", last)
	}
	if last.Summary.Additions != 1 {
		t.Errorf("summary = %+v, want one added", last.Summary)
	}
}

// The job's registry token reaches tofu as TF_TOKEN_<host>, and the deployment's provider mirror
// as a CLI configuration file tofu is pointed at.
func TestExecute_RegistryTokenAndMirrorReachTofu(t *testing.T) {
	h := newTofuHarness(t, "run-registry")
	h.api.artifacts.Registry = &runnercontract.RegistryBlock{Hosts: []runnercontract.RegistryHostToken{{Host: "stackweaver.example-corp.test", Token: "swj-registry"}}}
	h.api.artifacts.Mirror = &runnercontract.Mirror{ProviderNetworkMirror: "https://mirror.internal/providers", HTTPSProxy: "http://proxy.internal:3128"}
	if res := h.execute(t, "plan-only"); res.Status != "completed" {
		t.Fatalf("plan = %+v", res)
	}
	env := h.read(t, "init.env")
	if !strings.Contains(env, "TF_TOKEN_stackweaver_example__corp_test=swj-registry\n") {
		t.Error("tofu init was not handed the job's registry token")
	}
	if !strings.Contains(env, "HTTPS_PROXY=http://proxy.internal:3128\n") {
		t.Error("tofu init was not handed the deployment's proxy")
	}
	var cliConfig string
	for line := range strings.SplitSeq(env, "\n") {
		if v, ok := strings.CutPrefix(line, "TF_CLI_CONFIG_FILE="); ok {
			cliConfig = v
		}
	}
	if cliConfig == "" {
		t.Fatal("tofu was not pointed at a CLI configuration")
	}
	// The file lived in the job's work directory; its content is what the executor wrote.
	if _, err := os.Stat(cliConfig); !os.IsNotExist(err) {
		t.Errorf("the CLI configuration outlived the job (stat error: %v)", err)
	}
}

func TestCLIConfig(t *testing.T) {
	got := cliConfig("https://mirror.internal/providers")
	if !strings.Contains(got, "network_mirror") || !strings.Contains(got, `url = "https://mirror.internal/providers/"`) {
		t.Errorf("cliConfig = %q, want a network_mirror block with a slash-terminated URL", got)
	}
}

// An artifacts document of a contract major this executor does not speak is refused with an output
// line saying so, before anything runs (AC72).
func TestExecute_RefusesAnUnknownContractVersion(t *testing.T) {
	h := newTofuHarness(t, "run-contract")
	h.api.artifacts.ContractVersion = runnercontract.Version + 1
	res := h.execute(t, "plan")
	if res.Status != "failed" {
		t.Fatalf("result = %+v, want failed", res)
	}
	if calls := h.read(t, "calls"); calls != "" {
		t.Errorf("tofu ran %q, want nothing run", calls)
	}
	var said bool
	for _, o := range h.api.outputs {
		if strings.Contains(o.Output, "job contract") {
			said = true
		}
	}
	if !said {
		t.Error("the refusal was not written to the job's output")
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// The run's variables, its saved plan and every other credential it materialises live in one
// secret directory outside the working tree, which is gone when the phase ends (#933, D2, AC25).
func TestExecute_CredentialsLiveInTheShreddedSecretDir(t *testing.T) {
	h := newTofuHarness(t, "run-secrets")
	h.api.artifacts.Variables = map[string]string{"db_password": "placeholder-db-password"}
	if res := h.execute(t, "plan"); res.Status != "completed" {
		t.Fatalf("plan = %+v", res)
	}
	if got := h.read(t, "varfile"); !strings.Contains(got, `db_password = "placeholder-db-password"`) {
		t.Errorf("the plan read no variables file holding the run's variables (%q)", got)
	}
	var varFile, planOut string
	for _, arg := range strings.Fields(h.read(t, "calls")) {
		if v, ok := strings.CutPrefix(arg, "-var-file="); ok {
			varFile = v
		}
		if v, ok := strings.CutPrefix(arg, "-out="); ok {
			planOut = v
		}
	}
	if varFile == "" || filepath.Dir(varFile) != filepath.Dir(planOut) || !strings.Contains(filepath.Dir(varFile), "stackweaver-secrets-") {
		t.Fatalf("var file %q and saved plan %q, want both in the secret directory", varFile, planOut)
	}
	if _, err := os.Stat(filepath.Dir(varFile)); !os.IsNotExist(err) {
		t.Errorf("the secret directory outlived the phase (stat error %v)", err)
	}
}
