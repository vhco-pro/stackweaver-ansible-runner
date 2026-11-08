// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/backend/internal/executor"
	"github.com/michielvha/stackweaver/backend/internal/executor/syncexec"
	"github.com/michielvha/stackweaver/backend/internal/launcher"
	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// stampedVersion is what the test build stamps with -ldflags -X, as the launcher release does.
const stampedVersion = "9.8.7-test"

var (
	buildOnce sync.Once
	buildDir  string
	builtBin  string
	buildErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

// binary builds this command once, with its version stamped, into a test-owned directory.
func binary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "stackweaver-launcher-test")
		if err != nil {
			buildErr = err
			return
		}
		buildDir = dir
		builtBin = filepath.Join(dir, "stackweaver-launcher")
		out, err := exec.CommandContext(context.Background(), "go", "build", "-ldflags", "-X main.version="+stampedVersion, "-o", builtBin, ".").CombinedOutput() //nolint:gosec // G204: a fixed build of this package
		if err != nil {
			buildErr = &buildError{out: string(out), err: err}
		}
	})
	if buildErr != nil {
		t.Fatalf("building the launcher: %v", buildErr)
	}
	return builtBin
}

type buildError struct {
	out string
	err error
}

func (e *buildError) Error() string { return e.err.Error() + ": " + e.out }

// stubPath is a PATH holding only the named toolchain stubs.
func stubPath(t *testing.T, tools ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range tools {
		script := "#!/bin/sh\necho 'OpenTofu v1.13.1'\n"
		if tool == "ansible" {
			script = "#!/bin/sh\necho 'ansible [core 2.18.1]'\n"
		}
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(script), 0o755); err != nil { //nolint:gosec // G306: a test stub must be executable
			t.Fatal(err)
		}
	}
	return dir
}

// fakeAPI records what the binary sends: the first registration body and every output stream.
type fakeAPI struct {
	mu       sync.Mutex
	register []byte
	streams  []string
	srv      *httptest.Server
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/api/v2/runner/register":
			if f.register == nil {
				f.register = body
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		case strings.HasSuffix(r.URL.Path, "/output"):
			var out runnercontract.JobOutputRequest
			_ = json.Unmarshal(body, &out)
			f.streams = append(f.streams, out.Stream)
			_, _ = w.Write([]byte("{}"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) firstStream() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.streams) == 0 {
		return ""
	}
	return f.streams[0]
}

func (f *fakeAPI) registered() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.register
}

// The binary reports the version its release stamped with -ldflags -X, not "dev" (AC1).
func TestLauncherReportsItsStampedVersion(t *testing.T) {
	bin := binary(t)
	api := newFakeAPI(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin) //nolint:gosec // G204: the binary this test built
	cmd.Env = []string{
		"PATH=" + stubPath(t, "tofu"), "HOME=" + t.TempDir(),
		launcher.EnvServer + "=" + api.srv.URL, launcher.EnvToken + "=swa-agent", launcher.EnvPoolID + "=pool-1",
		launcher.EnvHealthAddr + "=off", launcher.EnvCacheDir + "=off",
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	for api.registered() == nil && ctx.Err() == nil {
		time.Sleep(50 * time.Millisecond)
	}
	var req runnercontract.RegisterRequest
	if err := json.Unmarshal(api.registered(), &req); err != nil {
		t.Fatalf("no registration reached the API: %v", err)
	}
	if req.AgentVersion != stampedVersion {
		t.Errorf("agent_version = %q, want the stamped %q", req.AgentVersion, stampedVersion)
	}
}

// `stackweaver-launcher healthcheck` answers from the launcher's /readyz (AC1).
func TestHealthcheckSubcommand(t *testing.T) {
	bin := binary(t)
	for _, tc := range []struct {
		status int
		ok     bool
	}{{http.StatusOK, true}, {http.StatusServiceUnavailable, false}} {
		ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/readyz" {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(tc.status)
		}))
		cmd := exec.CommandContext(t.Context(), bin, "healthcheck") //nolint:gosec // G204: the binary this test built
		cmd.Env = []string{launcher.EnvHealthAddr + "=" + strings.TrimPrefix(ready.URL, "http://")}
		err := cmd.Run()
		ready.Close()
		if (err == nil) != tc.ok {
			t.Errorf("healthcheck against /readyz %d: err = %v, want ok=%v", tc.status, err, tc.ok)
		}
	}
}

// Started by git as a sync's askpass helper, the binary answers the prompt from the executor's
// socket before anything else runs (AC1).
func TestAskpassEntryPoint(t *testing.T) {
	bin := binary(t)
	sock := filepath.Join(t.TempDir(), "askpass.sock")
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		if _, err := bufio.NewReader(c).ReadString('\n'); err == nil {
			_, _ = c.Write([]byte("s3cret"))
		}
	}()
	cmd := exec.CommandContext(t.Context(), bin, "Password for 'https://git.example':") //nolint:gosec // G204: the binary this test built
	cmd.Env = []string{syncexec.EnvAskpassSocket + "=" + sock}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("askpass: %v", err)
	}
	if strings.TrimSpace(string(out)) != "s3cret" {
		t.Errorf("askpass printed %q, want the socket's answer", out)
	}
}

// `stackweaver-launcher executor` runs an OpenTofu run and an Ansible job from the invocation a
// driver set, each preflighting on its own stream (AC1).
func TestExecutorSubcommandForBothEngines(t *testing.T) {
	bin := binary(t)
	for _, tc := range []struct {
		name, jobID, runType, want string
	}{
		{"opentofu plan", "run-1", "plan", "plan"},
		{"opentofu apply", "run-2", "apply", "apply"},
		{"ansible job", "job-3", "", "stderr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t)
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, "executor") //nolint:gosec // G204: the binary this test built
			cmd.Env = []string{
				"PATH=" + stubPath(t), "HOME=" + t.TempDir(),
				runnercontract.EnvServer + "=" + api.srv.URL, runnercontract.EnvJobID + "=" + tc.jobID,
				runnercontract.EnvJobToken + "=job-token", runnercontract.EnvRunType + "=" + tc.runType,
				executor.EnvLimitsReport + "=limits: none enforced",
			}
			_ = cmd.Run()
			if got := api.firstStream(); got != tc.want {
				t.Errorf("preflight stream = %q, want %q", got, tc.want)
			}
		})
	}
	// Without an invocation there is no job to report on: the launcher reports it.
	cmd := exec.CommandContext(t.Context(), bin, "executor") //nolint:gosec // G204: the binary this test built
	cmd.Env = []string{}
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != executor.ExitUnreported {
		t.Errorf("executor without an invocation: %v, want exit %d", err, executor.ExitUnreported)
	}
}

// execute sends each job to its engine's executor, preflighting on that engine's stream: a run on
// plan or apply, an Ansible job on stderr, a sync on stderr in the Ansible job image and on stdout
// in the OpenTofu one (A1, AC1).
func TestExecuteDispatchesEachKindWithItsPreflightStream(t *testing.T) {
	cases := []struct {
		name, jobID, runType string
		tools                []string
		wantKind             executor.Kind
		wantStream           string
	}{
		{"plan", "run-1", "plan", nil, executor.KindTofu, "plan"},
		{"plan-destroy", "run-1", "plan-destroy", nil, executor.KindTofu, "plan"},
		{"apply", "run-1", "apply", nil, executor.KindTofu, "apply"},
		{"apply-destroy", "run-1", "apply-destroy", nil, executor.KindTofu, "apply"},
		{"ansible job", "job-1", "", []string{"ansible"}, executor.KindAnsible, "stderr"},
		{"sync in the Ansible job image", runnercontract.WorkItemPrefix + "a", "", []string{"ansible"}, executor.KindSync, "stderr"},
		{"sync in the OpenTofu job image", runnercontract.WorkItemPrefix + "b", "", []string{"tofu"}, executor.KindSync, "stdout"},
	}
	saved := executors
	t.Cleanup(func() { executors = saved })
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", stubPath(t, tc.tools...))
			api := newFakeAPI(t)
			var ran executor.Kind
			executors = map[executor.Kind]jobExecutor{}
			for _, k := range []executor.Kind{executor.KindSync, executor.KindTofu, executor.KindAnsible} {
				executors[k] = func(context.Context, *executor.Client, driver.Invocation) string { ran = k; return "completed" }
			}
			execute(t.Context(), driver.Invocation{ServerURL: api.srv.URL, JobID: tc.jobID, Token: "t", RunType: tc.runType, LimitsReport: "limits: none enforced"})
			if ran != tc.wantKind {
				t.Errorf("ran %q, want %q", ran, tc.wantKind)
			}
			if got := api.firstStream(); got != tc.wantStream {
				t.Errorf("preflight stream = %q, want %q", got, tc.wantStream)
			}
		})
	}
}

// The Azure Workload Identity bridge copies AZURE_TENANT_ID to AZURE_TENANT only when the latter
// is unset, so an older azure.azcollection a playbook pins still finds the tenant (AC1).
func TestAzureTenantBridge(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"only AZURE_TENANT_ID", map[string]string{"AZURE_TENANT_ID": "tid"}, "tid"},
		{"explicit AZURE_TENANT wins", map[string]string{"AZURE_TENANT_ID": "tid", "AZURE_TENANT": "explicit"}, "explicit"},
		{"neither", map[string]string{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.env
			bridgeAzureTenant(func(k string) string { return env[k] }, func(k, v string) error { env[k] = v; return nil })
			if env["AZURE_TENANT"] != tc.want {
				t.Errorf("AZURE_TENANT = %q, want %q", env["AZURE_TENANT"], tc.want)
			}
		})
	}
}
