// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// The Kubernetes driver's placement timeout on a real cluster (one-launcher spec, A3, OD21, AC43):
// a job pod whose image cannot be pulled stays pending through the kubelet's retries until the
// placement timeout, then is removed and its job fails naming the image; a pod whose registry comes
// back before the timeout starts and its job completes.
//
// Gated behind `integration` and SW_KIND_CONTEXT, like TestConformance_Kubernetes; run it with
// e2e/k8s/driver-conformance.sh:
//
//	CONFORMANCE_RUN='^TestKubernetesPlacementTimeout' e2e/k8s/driver-conformance.sh

//go:build integration

package driver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	k8s "github.com/michielvha/stackweaver/core/runnerdriver/kubernetes"
)

// kindPullDriver is a Kubernetes driver in a namespace of the test's own on the kind cluster, whose
// job image is image, pulled on every placement.
func kindPullDriver(t *testing.T, kctx, image, executorPath string, timeout time.Duration) *Kubernetes {
	t.Helper()
	client := kindClient(t, kctx)
	ns := conformanceNamespace(t, kctx)
	token := kubectlOut(t, kctx, "-n", ns, "create", "token", conformanceSA, "--duration=1h")
	client.Token = func() (string, error) { return token, nil }
	d := &Kubernetes{
		Client: client, Namespace: ns, JobImages: sameJobImage(image), ExecutorPath: executorPath,
		JobImagePull: PullAlways, PlacementTimeout: timeout, PollInterval: time.Second,
	}
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := exec.CommandContext(context.Background(), "kubectl", "--context", kctx, "-n", ns, "get", "events", "--sort-by=.lastTimestamp").CombinedOutput() //nolint:gosec // G204: the test's own context and namespace
			t.Logf("events in %s:\n%s", ns, out)
		}
	})
	return d
}

// kindContext is the kind cluster's context, or a skip.
func kindContext(t *testing.T) string {
	t.Helper()
	kctx := os.Getenv(envKindContext)
	if kctx == "" {
		t.Skip("set " + envKindContext + " to a kind cluster's kubectl context (e2e/k8s/driver-conformance.sh)")
	}
	for _, tool := range []string{"kubectl", "kind"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s=%s needs %s on PATH: %v", envKindContext, kctx, tool, err)
		}
	}
	return kctx
}

// pullingReason is the waiting reason of the pod's executor container when it waits on a pull.
func pullingReason(t *testing.T, d *Kubernetes, pod string) string {
	t.Helper()
	var p k8s.Pod
	if err := d.Client.Get(t.Context(), k8s.Pods, d.Namespace, pod, &p); err != nil {
		return ""
	}
	for _, cs := range p.Status.ContainerStatuses {
		if w := cs.State.Waiting; w != nil && (w.Reason == "ErrImagePull" || w.Reason == "ImagePullBackOff") {
			return w.Reason
		}
	}
	return ""
}

// An image whose registry does not exist keeps its pod pulling until the placement timeout; then
// the pod is removed and the job fails naming the image, not at the first ErrImagePull.
func TestKubernetesPlacementTimeout_UnreachableImageFailsAfterTheTimeout(t *testing.T) {
	kctx := kindContext(t)
	image := "sw-test-noregistry-" + uuid.NewString()[:8] + ".invalid:5000/vhco-pro/stackweaver-opentofu-runner:latest"
	const timeout = 45 * time.Second
	d := kindPullDriver(t, kctx, image, "/usr/local/bin/stackweaver-launcher", timeout)

	start := time.Now()
	p, err := d.Place(t.Context(), Request{JobID: "pull-timeout-" + uuid.NewString()[:8], Attempt: 1, Kind: "tofu", RunType: "plan", Labels: withTestRun(nil)})
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	exit, err := d.Wait(ctx, p)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	took := time.Since(start)
	if exit.Code != exitCodeUnreported || !strings.Contains(exit.Reason, image) {
		t.Errorf("exit = %+v, want an unreported failure naming %s", exit, image)
	}
	if took < timeout {
		t.Errorf("the job failed after %s, before the %s placement timeout", took, timeout)
	}
	var gone k8s.Pod
	if err := d.Client.Get(t.Context(), k8s.Pods, d.Namespace, p.ID, &gone); err == nil && gone.DeletionTimestamp == "" {
		t.Errorf("the pod %s stuck pulling was left in place", p.ID)
	}
}

// A registry that is down for less than the placement timeout and comes back lets the pod start:
// the job completes instead of failing at the first pull error.
func TestKubernetesPlacementTimeout_RegistryRecoversBeforeTheTimeout(t *testing.T) {
	kctx := kindContext(t)
	cluster := strings.TrimPrefix(kctx, "kind-")
	e := newEngine(testSocket(t))
	sfx := uuid.NewString()[:8]

	// A registry of the test's own on the kind network, plain HTTP and anonymous, which the node's
	// containerd reaches by name through a hosts.toml (kind-config.yaml sets config_path), and the
	// host pushes to on loopback.
	name := "sw-test-pullreg-" + sfx
	if !e.imageExists(t.Context(), "registry:2") {
		if err := e.pullBounded(t.Context(), "registry:2", nil, 0); err != nil {
			t.Skipf("registry:2 is not available: %v", err)
		}
	}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()
	var created struct {
		ID string `json:"Id"`
	}
	cfg := map[string]any{
		"Image":        "registry:2",
		"Labels":       withTestRun(map[string]string{LabelManaged: "true"}),
		"ExposedPorts": map[string]any{"5000/tcp": map[string]any{}},
		"HostConfig": map[string]any{
			"NetworkMode":  kindNetwork,
			"PortBindings": map[string]any{"5000/tcp": []map[string]string{{"HostIp": "127.0.0.1", "HostPort": port}}},
		},
	}
	if err := e.do(t.Context(), http.MethodPost, "/containers/create", url.Values{"name": {name}}, cfg, &created); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.remove(context.Background(), created.ID) })
	if err := e.start(t.Context(), created.ID); err != nil {
		t.Fatal(err)
	}
	waitForRegistry(t, "127.0.0.1:"+port)
	node := cluster + "-control-plane"
	host := name + ":5000"
	hosts := fmt.Sprintf("server = \"http://%s\"\n\n[host.\"http://%s\"]\n  capabilities = [\"pull\", \"resolve\"]\n", host, host)
	if out, err := exec.CommandContext(t.Context(), "docker", "exec", node, "sh", "-c", //nolint:gosec // G204: the test's own node and registry name
		"mkdir -p /etc/containerd/certs.d/"+host+" && cat > /etc/containerd/certs.d/"+host+"/hosts.toml <<'EOF'\n"+hosts+"EOF").CombinedOutput(); err != nil {
		t.Fatalf("telling the node's containerd about %s: %v: %s", host, err, out)
	}
	t.Cleanup(func() {
		_ = exec.CommandContext(context.Background(), "docker", "exec", node, "rm", "-rf", "/etc/containerd/certs.d/"+host).Run() //nolint:gosec // G204: the test's own node and registry name
	})

	// The probe stands in for the executor: it reports and exits 0, which is a completed job.
	local := "sw-probe-pullreg-" + sfx + ":1"
	buildProbeImage(t, e, local, "1001:1001")
	pushTo(t, e, local, "127.0.0.1:"+port+"/vhco-pro/stackweaver-opentofu-runner:"+sfx, &RegistryAuth{Registry: "127.0.0.1:" + port})
	image := host + "/vhco-pro/stackweaver-opentofu-runner:" + sfx

	// Down before the placement, so the first pulls fail.
	if err := e.stop(t.Context(), created.ID, 0); err != nil {
		t.Fatal(err)
	}
	d := kindPullDriver(t, kctx, image, "/probe", 4*time.Minute)
	p, err := d.Place(t.Context(), Request{JobID: "pull-recover-" + sfx, Attempt: 1, Kind: "tofu", RunType: "plan", Labels: withTestRun(nil)})
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for pullingReason(t, d, p.ID) == "" {
		if time.Now().After(deadline) {
			t.Fatal("the pod never reported a failed pull with the registry down; the case proves nothing")
		}
		time.Sleep(2 * time.Second)
	}
	if err := e.start(t.Context(), created.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	exit, err := d.Wait(ctx, p)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit.Code != exitCodeCompleted {
		t.Errorf("exit = %+v, want the job completed once the registry came back", exit)
	}
	_ = d.Remove(context.WithoutCancel(t.Context()), p)
}

// waitForRegistry waits until the registry at addr answers on /v2/.
func waitForRegistry(t *testing.T, addr string) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/v2/", nil) //nolint:gosec // G704: the test's own local registry
		resp, err := client.Do(req)                                                                   //nolint:gosec // G704: the test's own local registry
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the registry at %s did not start: %v", addr, err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
