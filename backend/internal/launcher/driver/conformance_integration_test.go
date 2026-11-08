// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// The driver conformance suite (#933, D4, AC20): one OpenTofu job, plan and then apply of the saved
// plan, run through the in-process driver and through the container driver against the same fake
// control plane, must report identical output, phase state, plan counts, saved plan and state. The
// in-process side calls the executor function the runner binary registers; the container side runs
// the runner binary's `executor` subcommand in an image built for the test. The engine is a stand-in
// tofu script, so the suite needs no network beyond the engine socket.
//
// Gated behind `integration`; skips unless an engine answers (see testSocket). Run with:
//
//	go test -tags integration ./internal/launcher/driver/ -run TestConformance

//go:build integration

package driver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/michielvha/stackweaver/backend/internal/executor"
	"github.com/michielvha/stackweaver/backend/internal/executor/tofuexec"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

const conformanceTofuVersion = "0.0.0-conformance"

// conformanceTofu is the stand-in engine: deterministic output, a saved plan, a state.
const conformanceTofu = `#!/bin/sh
case "$1" in
init) echo "OpenTofu has been successfully initialized!" ;;
plan)
  for a in "$@"; do case "$a" in -out=*) out="${a#-out=}" ;; esac; done
  echo "saved-plan-conformance" > "$out"
  echo "  # null_resource.a will be created"
  echo "Plan: 1 to add, 0 to change, 0 to destroy."
  ;;
show) echo '{"resource_changes":[{"address":"null_resource.a","change":{"actions":["create"]}}]}' ;;
apply)
  echo "null_resource.a: Creating..."
  echo "null_resource.a: Creation complete after 0s [id=1]"
  echo ""
  echo "Apply complete! Resources: 1 added, 0 changed, 0 destroyed."
  echo '{"version":4,"serial":2}' > terraform.tfstate
  ;;
esac
`

// conformanceAPI is the control plane both runs report to.
type conformanceAPI struct {
	mu        sync.Mutex
	artifacts runnercontract.TofuArtifacts
	plan      []byte
	output    strings.Builder
	phase     map[string]*runnercontract.PhaseState
	state     []byte
	completed map[string]runnercontract.JobCompleteRequest
}

func (f *conformanceAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch p := r.URL.Path; {
	case strings.HasSuffix(p, "/artifacts"):
		if err := json.NewEncoder(w).Encode(f.artifacts); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	case strings.HasSuffix(p, "/plan") && r.Method == http.MethodPost:
		f.plan, _ = io.ReadAll(r.Body)
	case strings.HasSuffix(p, "/plan"):
		_, _ = w.Write(f.plan)
	case strings.HasSuffix(p, "/output"):
		var req runnercontract.JobOutputRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.output.WriteString(req.Output)
		if req.PhaseState != nil {
			f.phase[req.PhaseState.Phase] = req.PhaseState
		}
	case strings.HasSuffix(p, "/state"):
		f.state, _ = io.ReadAll(r.Body)
	case strings.HasSuffix(p, "/complete"):
		var req runnercontract.JobCompleteRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		req.RunnerID = ""
		f.completed[f.artifacts.JobType] = req
	case strings.HasSuffix(p, "/status"):
		_, _ = w.Write([]byte(`{"status":"running"}`))
	}
}

// conformanceRun is what one driver's runs reported.
type conformanceRun struct {
	Output    string
	Phase     map[string]*runnercontract.PhaseState
	Plan      string
	State     string
	Completed map[string]runnercontract.JobCompleteRequest
}

func newConformanceAPI(t *testing.T) (*conformanceAPI, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte(`resource "null_resource" "a" {}` + "\n")
	_ = tw.WriteHeader(&tar.Header{Name: "main.tf", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = gz.Close()
	f := &conformanceAPI{
		artifacts: runnercontract.TofuArtifacts{JobID: "run-conformance", ContractVersion: runnercontract.Version, TofuVersion: conformanceTofuVersion, ConfigTarball: base64.StdEncoding.EncodeToString(buf.Bytes())},
		phase:     map[string]*runnercontract.PhaseState{},
		completed: map[string]runnercontract.JobCompleteRequest{},
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: f, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f, strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

// runPlanAndApply runs a plan then an apply of its saved plan through d and returns what they
// reported. Each phase is a claim of its own and so an attempt of its own, as the run's attempt
// counter numbers them (ClaimForDispatch), which keeps a driver from reusing the plan's placement
// name while that placement is still being removed.
func runPlanAndApply(t *testing.T, d Driver, f *conformanceAPI, server string) conformanceRun {
	t.Helper()
	for _, phase := range []struct {
		runType, kind string
		attempt       int
	}{{"plan", "plan", 1}, {"apply", "apply", 2}} {
		f.mu.Lock()
		f.artifacts.JobType = phase.kind
		if phase.kind == "apply" {
			f.artifacts.Plan = &runnercontract.PlanDescriptor{Path: "/api/v2/runner/jobs/run-conformance/plan", SizeBytes: int64(len(f.plan))}
		}
		f.mu.Unlock()
		env := []string{
			runnercontract.EnvServer + "=" + server, runnercontract.EnvJobID + "=run-conformance",
			runnercontract.EnvJobToken + "=swj-conformance", runnercontract.EnvRunType + "=" + phase.runType, //nolint:gosec // G101: a fixture token
			runnercontract.EnvRunnerID + "=runner-conformance",
		}
		p, err := d.Place(t.Context(), Request{ //nolint:gosec // G101: a fixture token
			JobID: "run-conformance", Attempt: phase.attempt, Kind: "tofu", RunType: phase.runType, Env: env,
			Server: server, Token: "swj-conformance", RunnerID: "runner-conformance",
			Labels: withTestRun(map[string]string{LabelJob: "run-conformance-" + phase.kind}),
		})
		if err != nil {
			t.Fatalf("%s %s: Place: %v", d.Name(), phase.kind, err)
		}
		t.Cleanup(func() { _ = d.Remove(context.Background(), p) })
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		exit, err := d.Wait(ctx, p)
		cancel()
		if err != nil || exit.Code != 0 {
			if rc, lerr := d.Logs(t.Context(), p); lerr == nil {
				var out bytes.Buffer
				_ = DemuxLogs(rc, &out)
				_ = rc.Close()
				t.Logf("%s %s log:\n%s", d.Name(), phase.kind, out.String())
			}
			// An executor reports a refusal (the network guard) and its job's output to the API, not
			// to the container's own streams, so the log above can be empty.
			f.mu.Lock()
			t.Logf("%s %s output reported to the API:\n%s", d.Name(), phase.kind, f.output.String())
			f.mu.Unlock()
			if c, ok := d.(*Container); ok && p.ID != "" {
				var inspect map[string]any
				if ierr := c.engine().do(t.Context(), http.MethodGet, "/containers/"+p.ID+"/json", nil, nil, &inspect); ierr == nil {
					st, _ := json.Marshal(inspect["State"])
					hc, _ := json.Marshal(inspect["HostConfig"])
					ns, _ := json.Marshal(inspect["NetworkSettings"])
					t.Logf("%s %s container state: %s\nhost config: %s\nnetworks: %s", d.Name(), phase.kind, st, hc, ns)
				} else {
					t.Logf("%s %s: inspecting the container: %v", d.Name(), phase.kind, ierr)
				}
			}
			t.Fatalf("%s %s: exit %+v, %v", d.Name(), phase.kind, exit, err)
		}
		_ = d.Remove(t.Context(), p)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return conformanceRun{Output: f.output.String(), Phase: f.phase, Plan: string(f.plan), State: string(f.state), Completed: f.completed}
}

// buildConformanceImage is an image with a shell, the stand-in tofu where core/tofu looks for the
// version, and the launcher binary built from this tree as the executor, at the path every job
// image installs it.
func buildConformanceImage(t *testing.T, e *engine, tag string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "runner")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", bin, "../../../cmd/launcher") //nolint:gosec // G204: the test builds the launcher of this tree
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the launcher: %v: %s", err, out)
	}
	runner, err := os.ReadFile(bin) //nolint:gosec // G304: the test's own build output
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := "FROM alpine:3.22\nLABEL " + labelTestRun + "=" + testRun + "\nCOPY runner /usr/local/bin/stackweaver-launcher\nCOPY tofu /usr/local/bin/tofu-" + conformanceTofuVersion + "\nUSER 1001:1001\n"
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range map[string][]byte{"Dockerfile": []byte(dockerfile), "runner": runner, "tofu": []byte(conformanceTofu)} {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))})
		_, _ = tw.Write(content)
	}
	_ = tw.Close()
	if err := buildTestImage(t, e, tag, &buf, nil); err != nil {
		t.Skipf("building the conformance image (needs alpine:3.22): %v", err)
	}
}

// inProcessConformance runs the job through the executor function the runner binary registers,
// with the stand-in tofu where core/tofu's home fallback finds it: the report every other driver
// must match. It runs as a subtest so that the temporary HOME ends with it: the go build of the
// conformance image needs the real one for the module cache and the git credentials.
func inProcessConformance(t *testing.T) conformanceRun {
	t.Helper()
	var run conformanceRun
	if !t.Run("in-process", func(t *testing.T) { run = runInProcess(t) }) {
		t.FailNow()
	}
	return run
}

func runInProcess(t *testing.T) conformanceRun {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	versions := filepath.Join(home, ".tofu-versions")
	if err := os.MkdirAll(versions, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versions, "tofu-"+conformanceTofuVersion), []byte(conformanceTofu), 0o700); err != nil { //nolint:gosec // G306: the stand-in must be executable
		t.Fatal(err)
	}
	inproc := &InProcess{Executors: map[string]ExecutorFunc{"tofu": func(ctx context.Context, inv Invocation) int {
		c := executor.NewClient(inv.ServerURL, inv.Token, inv.RunnerID)
		res := tofuexec.Execute(ctx, c, tofuexec.Job{ID: inv.JobID, RunType: inv.RunType}, tofuexec.Options{Console: io.Discard})
		return executor.ExitCode(res.Status)
	}}}
	f, port := newConformanceAPI(t)
	return runPlanAndApply(t, inproc, f, "http://127.0.0.1:"+port)
}

// assertSameConformance fails unless driver reported what the in-process driver did, and the
// in-process report holds enough to make the comparison mean something.
func assertSameConformance(t *testing.T, driver string, want, got conformanceRun) {
	t.Helper()
	wantJSON, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wantJSON, gotJSON) {
		t.Errorf("the %s driver reported\n%s\nwhile the in-process driver reported\n%s", driver, gotJSON, wantJSON)
	}
	if want.Phase["apply"] == nil || len(want.Phase["apply"].Resources) != 1 || want.Completed["plan"].ResourceAdditions != 1 || want.State == "" || want.Plan == "" {
		t.Errorf("the runs reported too little to compare: %s", wantJSON)
	}
}

func TestConformance_InProcessAndContainerReportTheSame(t *testing.T) {
	sock := testSocket(t)
	want := inProcessConformance(t)

	// Container: the runner's executor subcommand in an image, on a jobs network as a launcher
	// with RUNNER_NETWORK places it. Without that network the executor's guard refuses the job
	// wherever a cloud metadata address answers, as on a CI runner in a cloud VM (D5).
	sfx := uuid.NewString()[:8]
	e := newEngine(sock)
	tag := "sw-conformance-" + sfx + ":1"
	buildConformanceImage(t, e, tag)
	ctr := &Container{Socket: sock, JobImages: sameJobImage(tag), ExecutorPath: "/usr/local/bin/stackweaver-launcher", ExtraHosts: []string{"host.docker.internal:host-gateway"}}
	f, port := newConformanceAPI(t)
	conformanceJobsNetwork(t, ctr, sfx, port)
	got := runPlanAndApply(t, ctr, f, "http://host.docker.internal:"+port)

	assertSameConformance(t, "container", want, got)
}

// conformanceJobsNetwork puts d's jobs on a jobs network with the policy a launcher writes (other
// networks and the metadata addresses denied), allowing the conformance API on the host at
// apiPort, the launcher's API allowance. On a rootless engine nothing is written to the host
// firewall, and the executor's guard is what stands between a job and a metadata address.
func conformanceJobsNetwork(t *testing.T, d *Container, sfx, apiPort string) {
	t.Helper()
	e := d.engine()
	tools := "sw-tools-conformance-" + sfx + ":1"
	buildToolsImage(t, e, tools)
	jobsNet := jobsNetworkName(sfx)
	t.Cleanup(func() { _ = e.do(context.Background(), http.MethodDelete, "/networks/"+jobsNet, nil, nil, nil) })
	// Removed before the network, also when the test fails after the first EnsureNetwork.
	t.Cleanup(func() { removeFirewallChains(context.Background(), d, tools, jobsNet) })
	pol := NetworkPolicy{Name: jobsNet, DenyOtherNetworks: true, FirewallImage: tools}
	programmed, err := d.EnsureNetwork(t.Context(), pol)
	if err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if !programmed {
		return
	}
	jn, err := e.network(t.Context(), jobsNet)
	if err != nil {
		t.Fatal(err)
	}
	if isPodman(t, e) {
		letPastDockersForwardDrop(t, d, tools, jn.IPAM.Config[0].Subnet)
	}
	pol.AllowTCP = []string{net.JoinHostPort(hostGateway(t, e, tools, jobsNet), apiPort)}
	if _, err := d.EnsureNetwork(t.Context(), pol); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
}
