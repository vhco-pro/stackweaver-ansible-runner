// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// The driver conformance suite on Kubernetes (#933, D4, AC20): the job the in-process and container
// drivers run in conformance_integration_test.go, run through the Kubernetes driver against a kind
// cluster from the host, must report the same output, phase state, plan counts, saved plan and
// state as the in-process driver.
//
// The test drives the cluster the way a launcher does, with nothing but core/runnerdriver/kubernetes
// and a ServiceAccount token: the API server and its CA come from the kubeconfig, the token from
// `kubectl create token` on a ServiceAccount bound to the launcher chart's Role in a namespace of
// the test's own, labelled `restricted` and with no NetworkPolicy. The image is the conformance
// image, loaded into the kind nodes; the fake control plane listens on 0.0.0.0 and job pods reach
// it through the kind network's gateway.
//
// Gated behind `integration` and SW_KIND_CONTEXT (the kind cluster's kubectl context); it also
// needs kubectl, kind and the container engine kind runs on. e2e/k8s/driver-conformance.sh runs it:
//
//	SW_KIND_CONTEXT=kind-sw-e2e go test -tags integration ./internal/launcher/driver/ -run TestConformance_Kubernetes

//go:build integration

package driver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	k8s "github.com/michielvha/stackweaver/core/runnerdriver/kubernetes"
)

const (
	// envKindContext names the kind cluster's kubectl context; the test skips without it.
	envKindContext = "SW_KIND_CONTEXT"
	// envKindHostAddr overrides the address job pods reach the host on (the kind network gateway).
	envKindHostAddr = "SW_KIND_HOST_ADDR"
	// kindNetwork is the engine network kind attaches its nodes to.
	kindNetwork = "kind"
	// conformanceNamespaceLabel marks the namespaces this test creates, so the runner script can
	// remove what an interrupted run left.
	conformanceNamespaceLabel = "stackweaver.io/test"
	conformanceSA             = "sw-conformance-launcher"
)

func TestConformance_Kubernetes(t *testing.T) {
	kctx := os.Getenv(envKindContext)
	if kctx == "" {
		t.Skip("set " + envKindContext + " to a kind cluster's kubectl context (e2e/k8s/driver-conformance.sh)")
	}
	// The context opts in, so a missing tool fails the test rather than skipping it.
	for _, tool := range []string{"kubectl", "kind"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s=%s needs %s on PATH: %v", envKindContext, kctx, tool, err)
		}
	}
	cluster := strings.TrimPrefix(kctx, "kind-")
	sock := testSocket(t)

	want := inProcessConformance(t)

	e := newEngine(sock)
	tag := "sw-conformance-" + uuid.NewString()[:8] + ":1"
	buildConformanceImage(t, e, tag)
	loadIntoKind(t, cluster, tag)

	f, port := newConformanceAPI(t)
	host := kindHostAddr(t, e)
	server := "http://" + net.JoinHostPort(host, port)
	probeFromKindNetwork(t, e, tag, server)

	client := kindClient(t, kctx)
	ns := conformanceNamespace(t, kctx)
	token := kubectlOut(t, kctx, "-n", ns, "create", "token", conformanceSA, "--duration=1h")
	client.Token = func() (string, error) { return token, nil }
	d := &Kubernetes{Client: client, Namespace: ns, PlatformImage: tag, ExecutorPath: "/usr/local/bin/runner", PollInterval: 500 * time.Millisecond}
	if err := d.Ping(t.Context()); err != nil {
		t.Fatalf("the launcher Role does not let the driver list the job namespace's pods: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := exec.CommandContext(context.Background(), "kubectl", "--context", kctx, "-n", ns, "get", "events", "--sort-by=.lastTimestamp").CombinedOutput() //nolint:gosec // G204: the test's own context and namespace
			t.Logf("events in %s:\n%s", ns, out)
		}
	})

	got := runPlanAndApply(t, d, f, server)
	assertSameConformance(t, "kubernetes", want, got)
}

// kubectlOut runs kubectl against the kind context and returns its trimmed output.
func kubectlOut(t *testing.T, kctx string, args ...string) string {
	t.Helper()
	return kubectlIn(t, kctx, "", args...)
}

// kubectlIn runs kubectl against the kind context with stdin and returns its trimmed output.
func kubectlIn(t *testing.T, kctx, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "kubectl", append([]string{"--context", kctx}, args...)...) //nolint:gosec // G204: the test's own kubectl arguments
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		var stderr []byte
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = ee.Stderr
		}
		t.Fatalf("kubectl %s: %v: %s", strings.Join(args, " "), err, stderr)
	}
	return strings.TrimSpace(string(out))
}

// kindClient is a client for the kind API server, its address and CA read from the kubeconfig. The
// token is set by the caller: the test talks as a ServiceAccount, never as the kubeconfig's admin.
func kindClient(t *testing.T, kctx string) *k8s.Client {
	t.Helper()
	var cfg struct {
		Clusters []struct {
			Cluster struct {
				Server string `json:"server"`
				CAData string `json:"certificate-authority-data"`
			} `json:"cluster"`
		} `json:"clusters"`
	}
	raw := kubectlOut(t, kctx, "config", "view", "--raw", "--minify", "--flatten", "-o", "json")
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil || len(cfg.Clusters) != 1 {
		t.Fatalf("reading the cluster of context %s from the kubeconfig: %v (%d clusters)", kctx, err, len(cfg.Clusters))
	}
	c := cfg.Clusters[0].Cluster
	ca, err := base64.StdEncoding.DecodeString(c.CAData)
	if err != nil {
		t.Fatalf("decoding the cluster CA of context %s: %v", kctx, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		t.Fatalf("the kubeconfig of context %s holds no PEM cluster CA", kctx)
	}
	return &k8s.Client{
		Server: c.Server,
		HTTP:   &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}},
	}
}

// conformanceNamespace creates the test's job namespace, enforcing the restricted Pod Security
// profile as the chart's job namespace does, with a ServiceAccount bound to the launcher chart's
// Role (deploy/helm/stackweaver/templates/launcher/launcher-rbac.yaml) and no NetworkPolicy. The
// namespace and everything in it are deleted when the test ends.
func conformanceNamespace(t *testing.T, kctx string) string {
	t.Helper()
	ns := "sw-conformance-" + uuid.NewString()[:8]
	manifest := `apiVersion: v1
kind: Namespace
metadata:
  name: ` + ns + `
  labels:
    ` + conformanceNamespaceLabel + `: driver-conformance
    pod-security.kubernetes.io/enforce: restricted
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ` + conformanceSA + `
  namespace: ` + ns + `
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: ` + conformanceSA + `
  namespace: ` + ns + `
rules:
  - apiGroups: [""]
    resources: ["pods", "secrets", "serviceaccounts"]
    verbs: ["get", "list", "create", "patch", "delete"]
  - apiGroups: [""]
    resources: ["pods/log", "configmaps"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ` + conformanceSA + `
  namespace: ` + ns + `
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: ` + conformanceSA + `
subjects:
  - kind: ServiceAccount
    name: ` + conformanceSA + `
    namespace: ` + ns + `
`
	t.Cleanup(func() {
		_ = exec.CommandContext(context.Background(), "kubectl", "--context", kctx, "delete", "namespace", ns, "--wait=false").Run() //nolint:gosec // G204: the test's own namespace
	})
	kubectlIn(t, kctx, manifest, "apply", "-f", "-")
	return ns
}

// loadIntoKind loads the image into every node of the kind cluster, as e2e/k8s/run.sh loads the
// platform images, and removes it from the nodes when the test ends.
func loadIntoKind(t *testing.T, cluster, tag string) {
	t.Helper()
	if out, err := exec.CommandContext(t.Context(), "kind", "load", "docker-image", tag, "--name", cluster).CombinedOutput(); err != nil { //nolint:gosec // G204: the test's own image and cluster
		t.Fatalf("loading %s into kind cluster %s: %v: %s", tag, cluster, err, out)
	}
	t.Cleanup(func() {
		nodes, err := exec.CommandContext(context.Background(), "kind", "get", "nodes", "--name", cluster).Output() //nolint:gosec // G204: the test's own cluster
		if err != nil {
			return
		}
		for _, node := range strings.Fields(string(nodes)) {
			_ = exec.CommandContext(context.Background(), "docker", "exec", node, "crictl", "rmi", "docker.io/library/"+tag).Run() //nolint:gosec // G204: the test's own image
		}
	})
}

// kindHostAddr is the address job pods reach the host on: the IPv4 gateway of the kind network,
// unless SW_KIND_HOST_ADDR names another.
func kindHostAddr(t *testing.T, e *engine) string {
	t.Helper()
	if addr := os.Getenv(envKindHostAddr); addr != "" {
		return addr
	}
	n, err := e.network(t.Context(), kindNetwork)
	if err != nil {
		t.Fatalf("reading the %q network kind attaches its nodes to: %v", kindNetwork, err)
	}
	gw := ipv4Gateway(n)
	if gw == "" {
		t.Fatalf("the %q network has no IPv4 gateway (IPAM %+v); set %s", kindNetwork, n.IPAM.Config, envKindHostAddr)
	}
	return gw
}

// ipv4Gateway is the first IPv4 gateway of a network's IPAM configuration, or "".
func ipv4Gateway(n networkState) string {
	for _, c := range n.IPAM.Config {
		if ip := net.ParseIP(c.Gateway); ip != nil && ip.To4() != nil {
			return c.Gateway
		}
	}
	return ""
}

func TestIPv4Gateway_SkipsTheIPv6Entry(t *testing.T) {
	// Docker lists a dual-stack kind network's IPv6 range first.
	var n networkState
	n.IPAM.Config = append(n.IPAM.Config,
		struct{ Subnet, Gateway string }{"fc00:f853:ccd:e793::/64", "fc00:f853:ccd:e793::1"},
		struct{ Subnet, Gateway string }{"172.18.0.0/16", "172.18.0.1"})
	if got := ipv4Gateway(n); got != "172.18.0.1" {
		t.Errorf("ipv4Gateway = %q, want 172.18.0.1", got)
	}
	n.IPAM.Config = n.IPAM.Config[:1]
	if got := ipv4Gateway(n); got != "" {
		t.Errorf("ipv4Gateway of an IPv6-only network = %q, want none", got)
	}
}

// probeFromKindNetwork fetches the fake control plane from a container on the kind network before
// any pod is placed. Traffic from a job pod leaves its node masqueraded and reaches the host on the
// kind bridge the same way, so a host firewall that drops it (firewalld puts unknown bridges in
// its default zone) fails here, with the fix, rather than as an executor that cannot connect.
func probeFromKindNetwork(t *testing.T, e *engine, image, server string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	id, err := e.create(ctx, "", containerConfig{
		Image:  image,
		Cmd:    []string{"wget", "-q", "-T", "5", "-O", "-", server + "/api/v2/runner/jobs/run-conformance/status"},
		Labels: withTestRun(nil),
		HostConfig: hostConfig{
			NetworkMode: kindNetwork,
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
		},
	})
	if err != nil {
		t.Fatalf("creating the reachability probe on the %q network: %v", kindNetwork, err)
	}
	defer func() { _ = e.remove(context.Background(), id) }()
	if err := e.start(ctx, id); err != nil {
		t.Fatalf("starting the reachability probe: %v", err)
	}
	code, err := e.wait(ctx, id)
	if err != nil || code != 0 {
		t.Fatalf("a container on the %q network cannot reach the fake API at %s (exit %d, %v). If firewalld runs on this host, "+
			"put the kind bridge in a zone that accepts it: sudo firewall-cmd --zone=docker --add-interface=br-<first 12 characters of the kind network id> "+
			"(add --permanent to keep it), or see e2e/k8s/driver-conformance.sh", kindNetwork, server, code, err)
	}
}
