// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// Integration tests for the container driver against a real engine (#933, D4, D5): the job
// container's hardening (AC71), its limits (AC24), the jobs network's rules (AC21), re-attaching
// after a launcher restart (AC16) and keep_failed_jobs_for (AC25). A static probe binary built from
// testdata/probe stands in for the executor, in an image built for the test; everything the tests
// create carries the managed label, a per-run suffix and the run's mark (labelTestRun), and is
// removed at the end, or by the next run when this one is killed or times out.
//
// Gated behind `integration`; skips unless an engine answers on $STACKWEAVER_TEST_ENGINE_SOCKET
// (default /var/run/docker.sock). Run with:
//
//	go test -tags integration ./internal/launcher/driver/ -run TestContainer

//go:build integration

package driver

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testSocket(t *testing.T) string {
	t.Helper()
	sock := os.Getenv("STACKWEAVER_TEST_ENGINE_SOCKET")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	if err := newEngine(sock).ping(t.Context()); err != nil {
		t.Skipf("no container engine on %s: %v", sock, err)
	}
	return sock
}

// buildProbeImage builds a scratch image holding the static probe, as user 1001 or root.
func buildProbeImage(t *testing.T, e *engine, tag, user string) {
	t.Helper()
	buildLabelledProbeImage(t, e, tag, user, map[string]string{LabelManaged: "true"})
}

// buildLabelledProbeImage builds the probe image with the given image labels.
func buildLabelledProbeImage(t *testing.T, e *engine, tag, user string, labels map[string]string) {
	t.Helper()
	buildProbeImageFrom(t, e, "scratch", tag, user, labels)
}

// buildProbeImageFrom builds an image holding the static probe at /probe on base.
func buildProbeImageFrom(t *testing.T, e *engine, base, tag, user string, labels map[string]string) {
	t.Helper()
	buildStampedProbeImage(t, e, base, tag, user, "", labels)
}

// buildStampedProbeImage builds the probe image with the probe reporting stamp, which tells the
// images a test builds apart.
func buildStampedProbeImage(t *testing.T, e *engine, base, tag, user, stamp string, labels map[string]string) {
	t.Helper()
	labels = withTestRun(labels)
	bin := filepath.Join(t.TempDir(), "probe")
	build := exec.CommandContext(t.Context(), "go", "build", "-ldflags", "-X main.stamp="+stamp, "-o", bin, "./testdata/probe") //nolint:gosec // G204: the test builds its own fixture
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the probe: %v: %s", err, out)
	}
	data, err := os.ReadFile(bin) //nolint:gosec // G304: the test's own build output
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := "FROM " + base + "\nCOPY probe /probe\n"
	if user != "" {
		dockerfile += "USER " + user + "\n"
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range map[string][]byte{"Dockerfile": []byte(dockerfile), "probe": data} {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))})
		_, _ = tw.Write(content)
	}
	_ = tw.Close()
	if err := buildTestImage(t, e, tag, &buf, labels); err != nil {
		t.Fatalf("building the probe image: %v", err)
	}
}

type probeResult struct {
	UID           int             `json:"uid"`
	CapEff        string          `json:"CapEff"`
	NoNewPrivs    string          `json:"NoNewPrivs"`
	Seccomp       string          `json:"Seccomp"`
	RootWritable  bool            `json:"root_writable"`
	TmpWritable   bool            `json:"tmp_writable"`
	DockerSocket  bool            `json:"docker_socket"`
	Reach         map[string]bool `json:"reach"`
	Env           []string        `json:"env"`
	Args          []string        `json:"args"`
	CacheDir      string          `json:"cache_dir"`
	CacheFiles    []string        `json:"cache_files"`
	CacheWritable bool            `json:"cache_writable"`
	Stamp         string          `json:"stamp"`
}

// buildToolsImage builds an image with a shell, busybox nc, iptables and nft: the firewall writer's
// image (the launcher images carry both for it) and the listeners standing in for platform services.
func buildToolsImage(t *testing.T, e *engine, tag string) {
	t.Helper()
	buildMarkedToolsImage(t, e, tag, testRun)
}

// buildMarkedToolsImage builds the tools image under a run's mark.
func buildMarkedToolsImage(t *testing.T, e *engine, tag, mark string) {
	t.Helper()
	dockerfile := []byte("FROM alpine:3.22\nRUN apk add --no-cache iptables ip6tables nftables\n")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(dockerfile))})
	_, _ = tw.Write(dockerfile)
	_ = tw.Close()
	if err := buildTestImage(t, e, tag, &buf, map[string]string{labelTestRun: mark}); err != nil {
		t.Skipf("building the tools image (needs the alpine image and its packages): %v", err)
	}
}

// runProbe places a probe job and returns what it reported.
// hostListener accepts TCP connections on every address of the test's host and returns its port.
func hostListener(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

func runProbe(t *testing.T, d *Container, req Request) (probeResult, Placement, Exit) {
	t.Helper()
	req.Labels = withTestRun(req.Labels)
	// A job's engine picks its job image; these probes stand in for an OpenTofu job's executor.
	req.Kind = cmp.Or(req.Kind, "tofu")
	p, err := d.Place(t.Context(), req)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	t.Cleanup(func() { _ = d.engine().remove(context.Background(), p.ID) })
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	exit, err := d.Wait(ctx, p)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	rc, err := d.Logs(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	var out bytes.Buffer
	_ = DemuxLogs(rc, &out)
	var res probeResult
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &res); err != nil {
		t.Fatalf("probe output %q: %v", out.String(), err)
	}
	return res, p, exit
}

func newTestDriver(t *testing.T, image string) *Container {
	t.Helper()
	return &Container{Socket: testSocket(t), JobImages: sameJobImage(image), ExecutorPath: "/probe"}
}

// A job container is non-root, has every capability dropped, no-new-privileges and the default
// seccomp profile, a read-only root with a writable /tmp, no engine socket, and the driver's
// entrypoint (AC71).
func TestContainerHardening(t *testing.T) {
	sfx := uuid.NewString()[:8]
	d := newTestDriver(t, "sw-probe-"+sfx+":1")
	buildProbeImage(t, d.engine(), d.JobImages["tofu"], "1001:1001")

	res, p, exit := runProbe(t, d, Request{JobID: "hard-" + sfx, Attempt: 1, Kind: "tofu", Env: []string{"STACKWEAVER_JOB_TOKEN=swj-x"}, Labels: map[string]string{LabelJob: "hard-" + sfx}})
	if exit.Code != 0 {
		t.Fatalf("exit = %+v", exit)
	}
	if res.UID == 0 {
		t.Error("the job ran as root")
	}
	if res.CapEff != "0000000000000000" {
		t.Errorf("effective capabilities = %s, want none", res.CapEff)
	}
	if res.NoNewPrivs != "1" {
		t.Errorf("NoNewPrivs = %q, want 1", res.NoNewPrivs)
	}
	if res.Seccomp != "2" {
		t.Errorf("Seccomp = %q, want 2 (a filter in force)", res.Seccomp)
	}
	if res.RootWritable || !res.TmpWritable {
		t.Errorf("root writable %t, /tmp writable %t; want a read-only root and a writable /tmp", res.RootWritable, res.TmpWritable)
	}
	if res.DockerSocket {
		t.Error("the engine socket is visible in the job")
	}
	if len(res.Args) != 1 || res.Args[0] != "executor" {
		t.Errorf("the job ran with args %v, want the driver's `executor`", res.Args)
	}
	st, err := d.engine().inspect(t.Context(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.HostConfig.Privileged || !st.HostConfig.ReadonlyRootfs || len(st.HostConfig.CapDrop) == 0 {
		t.Errorf("host config = %+v", st.HostConfig)
	}
}

// An image that runs as root is refused unless the pool allows it (AC71).
func TestContainerRefusesARootImage(t *testing.T) {
	sfx := uuid.NewString()[:8]
	d := newTestDriver(t, "sw-probe-root-"+sfx+":1")
	buildProbeImage(t, d.engine(), d.JobImages["tofu"], "")
	p, err := d.Place(t.Context(), Request{JobID: "root-" + sfx, Attempt: 1, Kind: "tofu", Labels: withTestRun(nil)})
	if err == nil {
		t.Cleanup(func() { _ = d.engine().remove(context.Background(), p.ID) })
	}
	if err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("Place = %v, want the root image refused", err)
	}
}

// Requested limits are applied and reported (AC24).
func TestContainerLimits(t *testing.T) {
	sfx := uuid.NewString()[:8]
	d := newTestDriver(t, "sw-probe-lim-"+sfx+":1")
	buildProbeImage(t, d.engine(), d.JobImages["tofu"], "1001:1001")
	_, p, _ := runProbe(t, d, Request{JobID: "lim-" + sfx, Attempt: 1, Resources: Resources{CPULimit: "500m", MemoryLimit: "256Mi"}, Labels: map[string]string{}})
	if strings.Join(p.Applied, ",") != "cpu=500m,memory=256Mi" {
		t.Errorf("applied = %v", p.Applied)
	}
	st, _ := d.engine().inspect(t.Context(), p.ID)
	if st.HostConfig.NanoCPUs != 500_000_000 || st.HostConfig.Memory != 256<<20 {
		t.Errorf("limits = %d nano-CPUs, %d bytes", st.HostConfig.NanoCPUs, st.HostConfig.Memory)
	}
}

// A custom image gets the executor injected from the platform image and still runs the driver's
// entrypoint, never its own (D4): the job runs the copy in its executor volume.
func TestContainerInjectsTheExecutorIntoACustomImage(t *testing.T) {
	sfx := uuid.NewString()[:8]
	platform := "sw-probe-plat-" + sfx + ":1"
	custom := "sw-probe-custom-" + sfx + ":1"
	d := newTestDriver(t, platform)
	// The copy runs a shell in the platform image, so the platform image here is the probe on
	// Alpine; the custom image is the scratch probe image.
	buildProbeImageFrom(t, d.engine(), "alpine:3.22", platform, "1001:1001", map[string]string{LabelManaged: "true"})
	buildProbeImage(t, d.engine(), custom, "1001:1001")
	res, p, exit := runProbe(t, d, Request{JobID: "inj-" + sfx, Attempt: 1, Image: custom})
	t.Cleanup(func() { _ = d.removeContainer(context.Background(), p.ID, p.Labels) })
	if p.Labels[labelExecutorVolume] == "" {
		t.Fatal("no executor volume was made for the custom image")
	}
	if exit.Code != 0 || res.UID != 1001 {
		t.Errorf("the job in the custom image = exit %+v uid %d", exit, res.UID)
	}
	var st struct {
		Config struct{ Entrypoint []string }
	}
	if err := d.engine().do(t.Context(), http.MethodGet, "/containers/"+p.ID+"/json", nil, nil, &st); err != nil {
		t.Fatal(err)
	}
	if want := []string{ExecutorMountPath + "/stackweaver-executor"}; !slices.Equal(st.Config.Entrypoint, want) || !slices.Equal(res.Args, []string{"executor"}) {
		t.Errorf("the job ran %v %v, want the injected executor %v with executor", st.Config.Entrypoint, res.Args, want)
	}
}

// Re-attach: a restarted launcher finds its running job containers by label and waits on them;
// a container that already died is listed no more (AC16).
func TestContainerReattach(t *testing.T) {
	sfx := uuid.NewString()[:8]
	d := newTestDriver(t, "sw-probe-re-"+sfx+":1")
	buildProbeImage(t, d.engine(), d.JobImages["tofu"], "1001:1001")
	runner := "runner-" + sfx
	labels := func(job string) map[string]string {
		return withTestRun(map[string]string{LabelRunner: runner, LabelJob: job})
	}
	long, err := d.Place(t.Context(), Request{JobID: "a-" + sfx, Attempt: 1, Kind: "tofu", Env: []string{"PROBE_SLEEP=120s"}, Labels: labels("a-" + sfx)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.engine().remove(context.Background(), long.ID) })
	short, err := d.Place(t.Context(), Request{JobID: "b-" + sfx, Attempt: 1, Kind: "tofu", Labels: labels("b-" + sfx)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.engine().remove(context.Background(), short.ID) })
	_, _ = d.Wait(t.Context(), short)

	// A new driver value, as after a launcher restart.
	restarted := &Container{Socket: d.Socket, JobImages: d.JobImages, ExecutorPath: d.ExecutorPath}
	found, err := restarted.List(t.Context(), map[string]string{LabelManaged: "true", LabelRunner: runner})
	if err != nil {
		t.Fatal(err)
	}
	var running []Placement
	for _, p := range found {
		if p.Ended {
			if p.Labels[LabelJob] != "b-"+sfx {
				t.Errorf("listed %s as ended, want only the finished job", p.Labels[LabelJob])
			}
			continue
		}
		running = append(running, p)
	}
	if len(running) != 1 || running[0].Labels[LabelJob] != "a-"+sfx {
		t.Fatalf("listed running %+v, want only the running job", running)
	}
	found = running
	// The re-attached launcher supervises it: stopping it ends the wait with the stop code.
	waited := make(chan Exit, 1)
	go func() {
		exit, _ := restarted.Wait(context.Background(), found[0])
		waited <- exit
	}()
	if err := restarted.Kill(t.Context(), found[0], time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case exit := <-waited:
		if exit.Code == 0 {
			t.Errorf("a killed job exited 0")
		}
	case <-time.After(30 * time.Second):
		t.Error("the re-attached wait never returned")
	}
}

// A failed job's container is kept for keep_failed_jobs_for and reaped afterwards (AC25).
func TestContainerKeepsAFailedJobThenReapsIt(t *testing.T) {
	sfx := uuid.NewString()[:8]
	d := newTestDriver(t, "sw-probe-keep-"+sfx+":1")
	buildProbeImage(t, d.engine(), d.JobImages["tofu"], "1001:1001")
	// The job's pool keeps a failure for keep; the launcher's default would keep it an hour, and the
	// job's own window wins. The window is measured from the container's end, so the checks below
	// read that time from the engine rather than assume how long the steps before them took: on a
	// loaded engine, the probe's wait, log read and removal can take seconds.
	const keep = 20 * time.Second
	d.KeepFailedFor = time.Hour
	_, p, exit := runProbe(t, d, Request{JobID: "keep-" + sfx, Attempt: 1, KeepFailedFor: keep, Env: []string{"PROBE_EXIT=1"}, Labels: map[string]string{LabelRunner: "keep-" + sfx}})
	if exit.Code == 0 {
		t.Fatal("the failing probe succeeded")
	}
	var ended struct {
		State struct{ FinishedAt time.Time }
	}
	if err := d.engine().do(t.Context(), http.MethodGet, "/containers/"+p.ID+"/json", nil, nil, &ended); err != nil {
		t.Fatalf("the failed container is gone before Remove: %v", err)
	}
	if err := d.Remove(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := d.engine().inspect(t.Context(), p.ID); err != nil {
		t.Fatalf("the failed container is gone %s after it ended, inside keep_failed_jobs_for (%s): %v", time.Since(ended.State.FinishedAt).Round(time.Millisecond), keep, err)
	}
	_ = d.Reap(t.Context(), map[string]string{LabelRunner: "keep-" + sfx})
	since := time.Since(ended.State.FinishedAt)
	if _, err := d.engine().inspect(t.Context(), p.ID); err != nil && since < keep {
		t.Fatalf("reaped %s after it ended, inside keep_failed_jobs_for (%s)", since.Round(time.Millisecond), keep)
	}
	t.Logf("first sweep %s after the job ended", since.Round(time.Millisecond))
	// keep_failed_jobs_for passes.
	time.Sleep(time.Until(ended.State.FinishedAt.Add(keep + time.Second)))
	_ = d.Reap(t.Context(), map[string]string{LabelRunner: "keep-" + sfx})
	if _, err := d.engine().inspect(t.Context(), p.ID); !isNotFound(err) {
		t.Errorf("the kept container outlived keep_failed_jobs_for: %v", err)
	}

	// A job whose pool keeps nothing, on a launcher that keeps nothing, is removed at once.
	d.KeepFailedFor = 0
	_, p, _ = runProbe(t, d, Request{JobID: "nokeep-" + sfx, Attempt: 1, Env: []string{"PROBE_EXIT=1"}, Labels: map[string]string{LabelRunner: "keep-" + sfx}})
	if err := d.Remove(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := d.engine().inspect(t.Context(), p.ID); !isNotFound(err) {
		t.Errorf("a failure with no keep window was kept: %v", err)
	}

	// A canceled job, which ends with a non-zero code like a failure, is not a failure: it is removed
	// at once even inside the pool's keep window (AC12).
	_, p, exit = runProbe(t, d, Request{JobID: "canceled-" + sfx, Attempt: 1, KeepFailedFor: keep, Env: []string{"PROBE_EXIT=1"}, Labels: map[string]string{LabelRunner: "keep-" + sfx}})
	if exit.Code == 0 {
		t.Fatal("the stopped probe exited 0")
	}
	p.Canceled = true
	if err := d.Remove(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := d.engine().inspect(t.Context(), p.ID); !isNotFound(err) {
		t.Errorf("a canceled job's container was kept as a failure: %v", err)
	}
}

// Two launchers share one engine (two pools on one host, a worktree slot beside a Compose stack):
// a sweep with one launcher's selector removes only that launcher's ended containers, never the
// other's, kept or not (AC25, AC67).
func TestContainerReapTouchesOnlyItsOwnRunner(t *testing.T) {
	sfx := uuid.NewString()[:8]
	d := newTestDriver(t, "sw-probe-reap-"+sfx+":1")
	buildProbeImage(t, d.engine(), d.JobImages["tofu"], "1001:1001")
	mine, theirs := "reap-mine-"+sfx, "reap-theirs-"+sfx
	// Both failures are ended and past any window: a sweep that matched them would remove them.
	_, own, _ := runProbe(t, d, Request{JobID: "own-" + sfx, Attempt: 1, KeepFailedFor: time.Millisecond, Env: []string{"PROBE_EXIT=1"}, Labels: map[string]string{LabelRunner: mine}})
	_, other, _ := runProbe(t, d, Request{JobID: "other-" + sfx, Attempt: 1, KeepFailedFor: time.Millisecond, Env: []string{"PROBE_EXIT=1"}, Labels: map[string]string{LabelRunner: theirs}})
	time.Sleep(10 * time.Millisecond)

	if err := d.Reap(t.Context(), map[string]string{LabelRunner: mine}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.engine().inspect(t.Context(), own.ID); !isNotFound(err) {
		t.Errorf("the sweep left its own ended container: %v", err)
	}
	if _, err := d.engine().inspect(t.Context(), other.ID); err != nil {
		t.Errorf("one launcher's sweep removed another launcher's container: %v", err)
	}
	if found, err := d.List(t.Context(), map[string]string{LabelManaged: "true", LabelRunner: mine}); err != nil || len(found) != 0 {
		t.Errorf("listing by its own runner = %v, %v; want nothing left", found, err)
	}
	if found, err := d.List(t.Context(), map[string]string{LabelManaged: "true", LabelRunner: theirs}); err != nil || len(found) != 1 || found[0].ID != other.ID {
		t.Errorf("listing by the other runner = %v, %v; want only its container", found, err)
	}
}

// From the jobs network: the metadata addresses, a platform service and another job are refused;
// the API endpoint the policy allows and the internet are reached (AC21). The rules need a rootful
// engine. Each firewall backend the engine can carry is proved: Docker's iptables chain and our own
// nftables table on Docker, and the nftables table on Podman, where netavark leaves no DOCKER-USER.
func TestContainerNetworkIsolation(t *testing.T) {
	for _, backend := range testFirewallBackends(t) {
		t.Run(backend.String(), func(t *testing.T) { networkIsolation(t, backend) })
		t.Run("created later/"+backend.String(), func(t *testing.T) { lateNetwork(t, backend) })
	}
}

// testFirewallBackends are the backends the test engine can carry: the one it is detected to use
// and, on Docker's iptables backend, our own nftables table too, which every engine without a
// DOCKER-USER chain gets.
func testFirewallBackends(t *testing.T) []firewallBackend {
	t.Helper()
	backend, _, err := detectFirewall(t.Context(), newEngine(testSocket(t)))
	if err != nil {
		t.Fatal(err)
	}
	if backend == firewallIPTables {
		return []firewallBackend{firewallIPTables, firewallNFTables}
	}
	return []firewallBackend{backend}
}

// isPodman reports whether the test engine is Podman.
func isPodman(t *testing.T, e *engine) bool {
	t.Helper()
	var version struct{ Components []struct{ Name string } }
	if err := e.do(t.Context(), http.MethodGet, "/version", nil, nil, &version); err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(version.Components, func(c struct{ Name string }) bool { return strings.Contains(c.Name, "Podman") })
}

// hostGateway is the address the engine gives a container on network for host-gateway, the
// launcher's host as a job reaches it (host.docker.internal): Docker's default bridge gateway, and
// on rootful Podman the network's own gateway.
func hostGateway(t *testing.T, e *engine, tools, network string) string {
	t.Helper()
	id, err := e.create(t.Context(), "", containerConfig{
		Image: tools, Entrypoint: []string{"/bin/sh", "-c"}, Cmd: []string{"awk '$2 == \"sw-host-gateway\" { print $1; exit }' /etc/hosts"},
		Labels: withTestRun(map[string]string{LabelManaged: "true"}), HostConfig: hostConfig{NetworkMode: network, ExtraHosts: []string{"sw-host-gateway:host-gateway"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.remove(context.Background(), id) })
	if err := e.start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if code, err := e.wait(t.Context(), id); err != nil || code != 0 {
		t.Fatalf("reading host-gateway: exit %d, %v", code, err)
	}
	rc, err := e.logs(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	var out bytes.Buffer
	_ = DemuxLogs(rc, &out)
	addr := strings.TrimSpace(out.String())
	if _, err := netip.ParseAddr(addr); err != nil {
		t.Fatalf("the engine's host-gateway on %s is %q", network, addr)
	}
	return addr
}

// letPastDockersForwardDrop lets a Podman subnet past Docker's FORWARD drop policy for the
// test's length, where Docker runs on the same host: Docker sets the iptables FORWARD policy to
// drop, and nftables evaluates every table's base chain, so no accept of Podman's or ours undoes it
// and every rootful Podman container on such a host is cut off from the internet and from a
// published port. This is the host-side allowance Docker documents for another engine beside it;
// the policy under test is our nftables table, which refuses what it denies before this chain
// runs. It does nothing on a host without Docker.
func letPastDockersForwardDrop(t *testing.T, d *Container, tools, subnet string) {
	t.Helper()
	script := fmt.Sprintf("if iptables -S FORWARD 2>/dev/null | grep -qx -- '-P FORWARD DROP' && iptables -L DOCKER-USER >/dev/null 2>&1; then "+
		"iptables -I DOCKER-USER -s %[1]s -j ACCEPT && iptables -I DOCKER-USER -d %[1]s -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT; fi", subnet)
	if err := d.runFirewall(t.Context(), tools, script); err != nil {
		t.Fatalf("letting %s past Docker's forward drop: %v", subnet, err)
	}
	t.Cleanup(func() { _ = d.runFirewall(context.Background(), tools, dockerForwardAllowanceRemoval(subnet)) })
}

// dockerForwardAllowanceRemoval removes what letPastDockersForwardDrop inserted for subnet.
func dockerForwardAllowanceRemoval(subnet string) string {
	return fmt.Sprintf("while iptables -D DOCKER-USER -s %[1]s -j ACCEPT 2>/dev/null; do :; done; "+
		"while iptables -D DOCKER-USER -d %[1]s -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null; do :; done; true", subnet)
}

func networkIsolation(t *testing.T, backend firewallBackend) {
	sfx := uuid.NewString()[:8]
	d := newTestDriver(t, "sw-probe-net-"+sfx+":1")
	buildProbeImage(t, d.engine(), d.JobImages["tofu"], "1001:1001")
	tools := "sw-tools-" + sfx + ":1"
	e := d.engine()
	buildToolsImage(t, e, tools)
	removeNet := func(name string) { _ = e.do(context.Background(), http.MethodDelete, "/networks/"+name, nil, nil, nil) }

	// listener starts a TCP listener on port 8080 on network and returns its address there.
	listener := func(name, network string) string {
		t.Helper()
		id, err := e.create(t.Context(), name+"-"+sfx, containerConfig{
			Image: tools, Entrypoint: []string{"/bin/sh", "-c"}, Cmd: []string{"while true; do busybox nc -l -p 8080 -e /bin/true; done"},
			User: "0", Labels: withTestRun(map[string]string{LabelManaged: "true"}), HostConfig: hostConfig{NetworkMode: network},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.remove(context.Background(), id) })
		if err := e.start(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		var st struct {
			NetworkSettings struct {
				Networks map[string]struct{ IPAddress string }
			}
		}
		_ = e.do(t.Context(), http.MethodGet, "/containers/"+id+"/json", nil, nil, &st)
		return st.NetworkSettings.Networks[network].IPAddress
	}

	// published starts the same listener on network with its port 8080 published on the host, as
	// Compose publishes the API, and returns the host port Docker chose.
	published := func(name, network string) string {
		t.Helper()
		var created struct {
			ID string `json:"Id"`
		}
		body := map[string]any{
			"Image": tools, "Entrypoint": []string{"/bin/sh", "-c"}, "Cmd": []string{"while true; do busybox nc -l -p 8080 -e /bin/true; done"},
			"User": "0", "Labels": withTestRun(map[string]string{LabelManaged: "true"}), "ExposedPorts": map[string]any{"8080/tcp": map[string]any{}},
			"HostConfig": map[string]any{"NetworkMode": network, "PortBindings": map[string]any{"8080/tcp": []map[string]string{{"HostIp": "0.0.0.0", "HostPort": ""}}}},
		}
		if err := e.do(t.Context(), http.MethodPost, "/containers/create", url.Values{"name": {name + "-" + sfx}}, body, &created); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.remove(context.Background(), created.ID) })
		if err := e.start(t.Context(), created.ID); err != nil {
			t.Fatal(err)
		}
		var st struct {
			NetworkSettings struct {
				Ports map[string][]struct{ HostPort string }
			}
		}
		_ = e.do(t.Context(), http.MethodGet, "/containers/"+created.ID+"/json", nil, nil, &st)
		if len(st.NetworkSettings.Ports["8080/tcp"]) == 0 {
			t.Fatalf("%s published no port", name)
		}
		return st.NetworkSettings.Ports["8080/tcp"][0].HostPort
	}

	// The platform services on a network of their own, standing in for Compose's.
	platformNet := "sw-test-platform-" + sfx
	if err := e.createNetwork(t.Context(), platformNet, withTestRun(map[string]string{LabelManaged: "true"}), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeNet(platformNet) })
	db := listener("sw-test-db", platformNet)
	pn, err := e.network(t.Context(), platformNet)
	if err != nil {
		t.Fatal(err)
	}
	// Another network on the engine that the policy does not name: a Compose project's, whose
	// published ports Docker forwards to its containers' addresses. DenyOtherNetworks covers it.
	otherNet := "sw-test-other-" + sfx
	if err := e.createNetwork(t.Context(), otherNet, withTestRun(map[string]string{LabelManaged: "true"}), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeNet(otherNet) })
	otherService := listener("sw-test-otherservice", otherNet)
	// The API as Compose runs it: a container on the platform network behind a port the host
	// publishes, which a job reaches as host.docker.internal:<port> and Docker's DNAT forwards to
	// the container's address, inside the denied platform range. A platform service published the
	// same way, which the policy does not allow, stays refused.
	publishedAPI := published("sw-test-publishedapi", platformNet)
	publishedDB := published("sw-test-publisheddb", platformNet)

	jobsNet := jobsNetworkName(sfx)
	t.Cleanup(func() { removeNet(jobsNet) })
	// The first EnsureNetwork already writes the network's rules: they are removed before the
	// network, also when the test fails or skips after it.
	t.Cleanup(func() { removeFirewallChains(context.Background(), d, tools, jobsNet) })
	pol := NetworkPolicy{Name: jobsNet, Deny: []string{pn.IPAM.Config[0].Subnet}, DenyOtherNetworks: true, FirewallImage: tools, firewall: backend}
	if _, err := d.EnsureNetwork(t.Context(), pol); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	// The API on the jobs network, as Compose attaches it, and another job listening there.
	api := listener("sw-test-api", jobsNet)
	other := listener("sw-test-otherjob", jobsNet)
	// Services on the launcher's host, which a job reaches through the engine's host-gateway
	// (host.docker.internal): a platform port it must not reach, and an API published on the host,
	// as a dev or single-VM install has it, which it must.
	hostService := hostListener(t)
	hostAPI := hostListener(t)
	jn, err := e.network(t.Context(), jobsNet)
	if err != nil {
		t.Fatal(err)
	}
	jobsGW, hostGW := jn.IPAM.Config[0].Gateway, hostGateway(t, e, tools, jobsNet)
	if isPodman(t, e) {
		letPastDockersForwardDrop(t, d, tools, jn.IPAM.Config[0].Subnet)
		// The control dials from the other network below cross the same forward drop.
		on, err := e.network(t.Context(), otherNet)
		if err != nil {
			t.Fatal(err)
		}
		letPastDockersForwardDrop(t, d, tools, on.IPAM.Config[0].Subnet)
	}
	pol.AllowTCP = []string{net.JoinHostPort(api, "8080"), net.JoinHostPort(hostGW, hostAPI), net.JoinHostPort(hostGW, publishedAPI)}
	programmed, err := d.EnsureNetwork(t.Context(), pol)
	if err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if !programmed {
		t.Skip("a rootless engine: the rules are the executor's guard, covered by its own test")
	}

	internet := "1.1.1.1:443"
	dial := []string{
		"169.254.169.254:80", net.JoinHostPort(db, "8080"), net.JoinHostPort(api, "8080"), net.JoinHostPort(other, "8080"), internet,
		net.JoinHostPort(jobsGW, hostService), net.JoinHostPort(hostGW, hostService), net.JoinHostPort(hostGW, hostAPI),
		net.JoinHostPort(otherService, "8080"), net.JoinHostPort(hostGW, publishedAPI), net.JoinHostPort(hostGW, publishedDB),
	}
	res, _, _ := runProbe(t, d, Request{JobID: "net-" + sfx, Attempt: 1, Env: []string{"PROBE_DIAL=" + strings.Join(dial, ",")}, Labels: map[string]string{}})
	want := map[string]bool{dial[0]: false, dial[1]: false, dial[2]: true, dial[3]: false, dial[5]: false, dial[6]: false, dial[7]: true, dial[8]: false, dial[9]: true, dial[10]: false}
	if c, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(t.Context(), "tcp", internet); err == nil {
		_ = c.Close()
		want[internet] = true
	}
	for addr, w := range want {
		if res.Reach[addr] != w {
			t.Errorf("reaching %s from a job = %t, want %t", addr, res.Reach[addr], w)
		}
	}
	// Nothing on another network opens a connection to a job, while that network's own services
	// answer the same dial.
	if !dialFrom(t, e, tools, otherNet, net.JoinHostPort(otherService, "8080")) {
		t.Fatal("a container cannot reach a service on its own network; the dial proves nothing")
	}
	if dialFrom(t, e, tools, otherNet, net.JoinHostPort(other, "8080")) {
		t.Error("a container on another network reaches a job")
	}
}

// dialFrom reports whether a container on network opens a TCP connection to addr.
func dialFrom(t *testing.T, e *engine, tools, network, addr string) bool {
	t.Helper()
	host, port, _ := net.SplitHostPort(addr)
	id, err := e.create(t.Context(), "", containerConfig{
		Image: tools, Entrypoint: []string{"/bin/sh", "-c"}, Cmd: []string{"nc -z -w 3 " + host + " " + port},
		Labels: withTestRun(map[string]string{LabelManaged: "true"}), HostConfig: hostConfig{NetworkMode: network},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.remove(context.Background(), id) })
	if err := e.start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	code, err := e.wait(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return code == 0
}

// removeFirewallChains removes a test jobs network's rules from the host firewall: its iptables
// chains, its nftables tables, and the allowance past Docker's forward drop a Podman run inserts.
func removeFirewallChains(ctx context.Context, d *Container, image, network string) {
	chain := chainName(network)
	table := nftTableName(network)
	script := fmt.Sprintf("iptables -S DOCKER-USER | grep -- '-j %[1]s$' | sed 's/^-A //' | while read -r rule; do iptables -D $rule; done; "+
		"iptables -S INPUT | grep -- '-j %[1]s-IN$' | sed 's/^-A //' | while read -r rule; do iptables -D $rule; done; "+
		"iptables -F %[1]s 2>/dev/null; iptables -X %[1]s 2>/dev/null; iptables -F %[1]s-IN 2>/dev/null; iptables -X %[1]s-IN 2>/dev/null; "+
		"if command -v nft >/dev/null; then nft delete table inet %[2]s 2>/dev/null; nft delete table bridge %[2]s 2>/dev/null; fi; ", chain, table)
	if n, err := d.engine().network(ctx, network); err == nil {
		for _, c := range n.IPAM.Config {
			if c.Subnet != "" {
				script += dockerForwardAllowanceRemoval(c.Subnet) + "; "
			}
		}
	}
	_ = d.runFirewall(ctx, image, script+"true")
}
