// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

//go:build cloudproof

package launcher

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// TestCloudProof places one job through the driver the launcher builds from its environment
// (ConfigFromEnv, then DriverFromEnv: the same two calls the launcher's main makes), waits for it,
// prints its log and removes it. It is the run half of the cloud identity procedures (#933,
// AC38 to AC42, docs/internal/plans/infrastructure/runners/procedures/): the job's image is a
// probe whose "executor" checks the identity endpoints, so what is under test is the driver's
// placement, network and identity rendering, not a run's configuration.
//
// It never runs in CI: the build tag keeps it out of every suite. Inputs:
//
//	PROOF_LAUNCHER_ENV  a file of KEY=VALUE lines, the launcher's environment (RUNNER_*)
//	PROOF_JOB_ENV       a file of KEY=VALUE lines added to the job's environment
//	PROOF_JOB_ID        the job id (default proof-<unix time>)
//	PROOF_IDENTITY      mechanism,serviceaccount[,per-job] for a Kubernetes identity binding
//	PROOF_SA_ANNOTATIONS key=value;key=value annotations of that binding's ServiceAccount
//	PROOF_ENSURE_SA     "1" creates the binding's ServiceAccount the way the API-created mode does
//	PROOF_POOL_ID       the pool id stamped on an ensured ServiceAccount
//	PROOF_EXPECT_EXIT   the probe's expected exit code (default 0)
//	PROOF_KEEP          "1" leaves the placement for inspection
//	PROOF_COMMAND_FILE  a probe script run as the executor container's command (Kubernetes only),
//	                    for a public image that has no probe at the executor path
func TestCloudProof(t *testing.T) {
	launcherEnv := readEnvFile(t, os.Getenv("PROOF_LAUNCHER_ENV"))
	getenv := func(k string) string { return launcherEnv[k] }
	b := Binary{Engine: "tofu", ExecutorPath: firstOf(launcherEnv["PROOF_EXECUTOR_PATH"], "/stackweaver-probe")}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cfg, err := ConfigFromEnv(b, getenv)
	if err != nil {
		t.Fatalf("reading the launcher configuration: %v", err)
	}
	d, err := DriverFromEnv(ctx, b, cfg, getenv)
	if err != nil {
		t.Fatalf("building the driver: %v", err)
	}
	t.Logf("driver=%s network_mode=%s capabilities=%+v", d.Name(), cfg.NetworkMode, d.Capabilities())
	if path := os.Getenv("PROOF_COMMAND_FILE"); path != "" {
		overrideCommand(t, d, path)
	}

	req := driver.Request{
		JobID:   firstOf(os.Getenv("PROOF_JOB_ID"), "proof-"+strconv.FormatInt(time.Now().Unix(), 10)),
		Attempt: 1,
		Kind:    "tofu",
		RunType: "plan",
		Labels:  map[string]string{driver.LabelPool: firstOf(os.Getenv("PROOF_POOL_ID"), "proof-pool")},
	}
	for k, v := range readEnvFile(t, os.Getenv("PROOF_JOB_ENV")) {
		req.Env = append(req.Env, k+"="+v)
	}
	if spec := os.Getenv("PROOF_IDENTITY"); spec != "" {
		parts := strings.Split(spec, ",")
		id := &driver.Identity{Mechanism: parts[0], Annotations: map[string]string{}}
		if len(parts) > 1 {
			id.ServiceAccount = parts[1]
		}
		id.PerJob = len(parts) > 2 && parts[2] == "per-job"
		for _, kv := range strings.Split(os.Getenv("PROOF_SA_ANNOTATIONS"), ";") {
			if k, v, ok := strings.Cut(kv, "="); ok {
				id.Annotations[k] = v
			}
		}
		req.Identity = id
		if os.Getenv("PROOF_ENSURE_SA") == "1" {
			kd, ok := d.(*driver.Kubernetes)
			if !ok {
				t.Fatalf("PROOF_ENSURE_SA needs the kubernetes driver, have %s", d.Name())
			}
			obj := runnercontract.EnsureObject{Kind: "ServiceAccount", Name: id.ServiceAccount, PoolID: firstOf(os.Getenv("PROOF_POOL_ID"), "proof-pool"), Annotations: id.Annotations}
			if err := kd.Ensure(ctx, []runnercontract.EnsureObject{obj}); err != nil {
				t.Fatalf("ensuring the binding's service account: %v", err)
			}
			inv, err := kd.Inventory(ctx)
			if err != nil {
				t.Fatalf("reading the inventory: %v", err)
			}
			for _, e := range inv {
				t.Logf("inventory: %s %s/%s present=%v managed_by=%s external=%q", e.Kind, e.Namespace, e.Name, e.Present, e.ManagedBy, e.ExternalManager)
			}
		}
	}

	p, err := d.Place(ctx, req)
	if err != nil {
		t.Fatalf("placing the probe job: %v", err)
	}
	t.Logf("placed id=%s node=%s labels=%v", p.ID, p.Node, p.Labels)
	exit, err := d.Wait(ctx, p)
	if err != nil {
		t.Fatalf("waiting for the probe job: %v", err)
	}
	if rc, lerr := d.Logs(ctx, p); lerr == nil {
		var log io.Reader = rc
		if d.Name() == driver.NameContainer {
			// The engine multiplexes stdout and stderr; the launcher reads it through DemuxLogs too.
			var buf bytes.Buffer
			if err := driver.DemuxLogs(rc, &buf); err != nil {
				t.Logf("demultiplexing the probe log: %v", err)
			}
			log = &buf
		}
		s := bufio.NewScanner(log)
		s.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for s.Scan() {
			t.Logf("probe| %s", s.Text())
		}
		_ = rc.Close()
	} else {
		t.Logf("reading the probe log: %v", lerr)
	}
	t.Logf("exit code=%d reason=%q", exit.Code, exit.Reason)
	if os.Getenv("PROOF_KEEP") != "1" {
		p.Canceled = true // removed whatever the keep window says
		if err := d.Remove(context.WithoutCancel(ctx), p); err != nil {
			t.Errorf("removing the probe job: %v", err)
		}
	}
	want, _ := strconv.Atoi(firstOf(os.Getenv("PROOF_EXPECT_EXIT"), "0"))
	if exit.Code != want {
		t.Fatalf("probe exit %d, want %d", exit.Code, want)
	}
}

func readEnvFile(t *testing.T, path string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if path == "" {
		return out
	}
	f, err := os.Open(path) //nolint:gosec // G304: the operator's proof input
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			out[k] = v
		}
	}
	return out
}

// overrideCommand makes every pod the driver applies run the probe script as the executor
// container's command, and changes nothing else in the request: the pod spec, labels, identity and
// ServiceAccount stay the driver's. It lets a public image (mcr.microsoft.com/azure-cli on AKS)
// carry the probe without a probe image being pushed anywhere.
func overrideCommand(t *testing.T, d driver.Driver, path string) {
	t.Helper()
	kd, ok := d.(*driver.Kubernetes)
	if !ok {
		t.Fatalf("PROOF_COMMAND_FILE needs the kubernetes driver, have %s", d.Name())
	}
	script, err := os.ReadFile(path) //nolint:gosec // G304: the operator's proof input
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	base := kd.Client.HTTP.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	command := []string{"/bin/sh", "-c", string(script), "stackweaver-probe", "executor"}
	kd.Client.HTTP.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPatch || !strings.Contains(r.URL.Path, "/pods/") || r.Body == nil {
			return base.RoundTrip(r)
		}
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			return nil, err
		}
		var pod map[string]any
		if err := json.Unmarshal(body, &pod); err != nil {
			return nil, err
		}
		spec, _ := pod["spec"].(map[string]any)
		containers, _ := spec["containers"].([]any)
		for _, c := range containers {
			if m, ok := c.(map[string]any); ok && m["name"] == "executor" {
				t.Logf("command override: %v replaced by the probe script", m["command"])
				m["command"] = command
			}
		}
		if body, err = json.Marshal(pod); err != nil {
			return nil, err
		}
		r = r.Clone(r.Context())
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		return base.RoundTrip(r)
	})
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
