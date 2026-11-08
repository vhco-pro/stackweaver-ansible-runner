// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

//go:build integration

package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// labelTestRun marks everything this package's integration tests create on the engine with the
// host and process that created it. A test removes what it creates when it ends, but Go runs no
// cleanup when a test binary is killed or hits its -timeout, so the next run removes what a run
// that is gone left behind, and never what a run still going on another worktree is using.
const labelTestRun = "sh.stackweaver.test-run"

const jobsNetworkPrefix = "sw-test-jobs-"

var testRun = func() string {
	host, _ := os.Hostname()
	return host + "/" + strconv.Itoa(os.Getpid())
}()

// jobsNetworkName is the name of a test's jobs network. EnsureNetwork makes that network with the
// product's labels only, so the run's mark goes in the name, where the sweep reads it back.
func jobsNetworkName(sfx string) string { return jobsNetworkNameFor(testRun, sfx) }

// jobsNetworkNameFor is the jobs network name carrying mark, "" for a mark of another form.
func jobsNetworkNameFor(mark, sfx string) string {
	host, pid, ok := strings.Cut(mark, "/")
	if !ok {
		return ""
	}
	return jobsNetworkPrefix + hostTag(host) + "-" + pid + "-" + sfx
}

// jobsNetworkRun is the mark a jobs network's name carries, "" for any other name or another host.
func jobsNetworkRun(name string) string {
	rest, ok := strings.CutPrefix(name, jobsNetworkPrefix)
	if !ok {
		return ""
	}
	parts := strings.SplitN(rest, "-", 3)
	host, _ := os.Hostname()
	if len(parts) != 3 || parts[0] != hostTag(host) {
		return ""
	}
	return host + "/" + parts[1]
}

// hostTag is a short tag of a host's name, which a network name may not be able to hold.
func hostTag(host string) string {
	sum := sha256.Sum256([]byte(host))
	return hex.EncodeToString(sum[:])[:8]
}

// withTestRun returns labels with this run's mark added, keeping a mark labels already carry.
func withTestRun(labels map[string]string) map[string]string {
	out := maps.Clone(labels)
	if out == nil {
		out = map[string]string{}
	}
	if out[labelTestRun] == "" {
		out[labelTestRun] = testRun
	}
	return out
}

func TestMain(m *testing.M) {
	sock := os.Getenv("STACKWEAVER_TEST_ENGINE_SOCKET")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	if newEngine(sock).ping(ctx) == nil {
		if err := sweepEndedRuns(ctx, sock); err != nil {
			fmt.Fprintf(os.Stderr, "removing what ended test runs left on the engine: %v\n", err)
		}
	}
	cancel()
	os.Exit(m.Run())
}

// runEnded reports whether the run a mark names has ended: a process of this host that no longer
// exists. A mark from another host, or a process that is still there, is a run that may be using
// what it made.
func runEnded(mark string) bool {
	host, pidText, ok := strings.Cut(mark, "/")
	if self, _ := os.Hostname(); !ok || host != self {
		return false
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 || pid == os.Getpid() {
		return false
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// sweepEndedRuns removes the containers, the host firewall chains of the jobs networks they were
// on, the networks, the volumes and the images that ended runs left on the engine, in that order.
// A network, volume or image some container still uses is left for a later sweep: nothing is
// removed by force, because Podman's forced removal of any of them removes the containers using
// it, whoever's they are.
func sweepEndedRuns(ctx context.Context, sock string) error {
	e := newEngine(sock)
	d := &Container{Socket: sock}
	filters := url.Values{"filters": {`{"label":["` + labelTestRun + `"]}`}}
	var errs []error

	var containers []struct {
		ID              string `json:"Id"`
		Labels          map[string]string
		NetworkSettings struct{ Networks map[string]struct{} }
	}
	if err := e.do(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"1"}, "filters": filters["filters"]}, nil, &containers); err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}
	networks := map[string]bool{}
	for _, c := range containers {
		if !runEnded(c.Labels[labelTestRun]) {
			continue
		}
		for name := range c.NetworkSettings.Networks {
			if strings.HasPrefix(name, "sw-test-") {
				networks[name] = true
			}
		}
		if err := d.removeContainer(ctx, c.ID, c.Labels); err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("removing container %s: %w", c.ID, err))
		}
	}

	var images []struct {
		ID       string `json:"Id"`
		RepoTags []string
		Labels   map[string]string
	}
	if err := e.do(ctx, http.MethodGet, "/images/json", filters, nil, &images); err != nil {
		return errors.Join(append(errs, fmt.Errorf("listing images: %w", err))...)
	}
	var labelled []struct {
		Name   string
		Labels map[string]string
	}
	if err := e.do(ctx, http.MethodGet, "/networks", filters, nil, &labelled); err != nil {
		errs = append(errs, fmt.Errorf("listing networks: %w", err))
	}
	for _, n := range labelled {
		if runEnded(n.Labels[labelTestRun]) {
			networks[n.Name] = true
		}
	}
	// A jobs network carries the run's mark in its name, not on a label (jobsNetworkName).
	var all []struct{ Name string }
	if err := e.do(ctx, http.MethodGet, "/networks", nil, nil, &all); err != nil {
		errs = append(errs, fmt.Errorf("listing networks: %w", err))
	}
	for _, n := range all {
		if runEnded(jobsNetworkRun(n.Name)) {
			networks[n.Name] = true
		}
	}
	// A jobs network's host firewall rules are removed with an ended run's own image that carries
	// the firewall tools.
	firewallImage := ""
	for _, img := range images {
		for _, tag := range img.RepoTags {
			// Docker lists the name as built; Podman adds a registry (docker.io/library/ for an
			// image built through its Docker API).
			if strings.HasPrefix(path.Base(tag), "sw-tools-") && runEnded(img.Labels[labelTestRun]) {
				firewallImage = tag
			}
		}
	}
	for name := range networks {
		if used(ctx, e, "network", name) {
			continue
		}
		if strings.HasPrefix(name, jobsNetworkPrefix) {
			// The rules are found through the network's name and subnet, so without an image to
			// remove them with the network stays, and says so, rather than leave them unowned.
			if firewallImage == "" {
				fmt.Fprintf(os.Stderr, "leaving the ended run's jobs network %s: no ended run's tools image to remove its firewall rules with\n", name)
				continue
			}
			removeFirewallChains(ctx, d, firewallImage, name)
		}
		if err := e.do(ctx, http.MethodDelete, "/networks/"+name, nil, nil, nil); err != nil && !isNotFound(err) {
			errs = append(errs, fmt.Errorf("removing network %s: %w", name, err))
		}
	}

	var volumes struct {
		Volumes []struct {
			Name   string
			Labels map[string]string
		}
	}
	if err := e.do(ctx, http.MethodGet, "/volumes", filters, nil, &volumes); err != nil {
		errs = append(errs, fmt.Errorf("listing volumes: %w", err))
	}
	for _, v := range volumes.Volumes {
		if runEnded(v.Labels[labelTestRun]) && !used(ctx, e, "volume", v.Name) {
			if err := e.do(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(v.Name), nil, nil, nil); err != nil && !isNotFound(err) {
				errs = append(errs, fmt.Errorf("removing volume %s: %w", v.Name, err))
			}
		}
	}

	for _, img := range images {
		// Docker matches an ancestor by the listed ID, Podman only by the ID without its algorithm.
		if !runEnded(img.Labels[labelTestRun]) || used(ctx, e, "ancestor", img.ID) || used(ctx, e, "ancestor", strings.TrimPrefix(img.ID, "sha256:")) {
			continue
		}
		// Each tag is removed on its own: an image with several tags is not removed by ID
		// without force.
		// An untagged image lists the placeholder <none>:<none>, which is no reference: it goes by ID.
		refs := slices.DeleteFunc(slices.Clone(img.RepoTags), func(tag string) bool { return tag == "<none>:<none>" })
		if len(refs) == 0 {
			refs = []string{img.ID}
		}
		for _, ref := range refs {
			if err := e.do(ctx, http.MethodDelete, "/images/"+ref, nil, nil, nil); err != nil && !isNotFound(err) {
				errs = append(errs, fmt.Errorf("removing image %s: %w", ref, err))
			}
		}
	}
	return errors.Join(errs...)
}

// used reports whether any container on the engine, running or not, matches the container filter
// (network, volume or ancestor) for value. A listing that fails counts as used.
func used(ctx context.Context, e *engine, filter, value string) bool {
	f, err := json.Marshal(map[string][]string{filter: {value}})
	if err != nil {
		return true
	}
	var found []struct {
		ID string `json:"Id"`
	}
	if err := e.do(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"1"}, "filters": {string(f)}}, nil, &found); err != nil {
		return true
	}
	return len(found) > 0
}

// endedRunMark is the mark of a process of this host that has exited.
func endedRunMark(t *testing.T) string {
	t.Helper()
	p, err := os.StartProcess("/bin/true", []string{"true"}, &os.ProcAttr{})
	if err != nil {
		t.Skipf("starting a short-lived process: %v", err)
	}
	if _, err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	return host + "/" + strconv.Itoa(p.Pid)
}

// What a killed or timed-out run left (a job container kept as a failure, the network it was on, a
// volume and its image) is removed by the next run; what a run still going left is not touched.
func TestSweepRemovesWhatAnEndedRunLeftAndKeepsALiveRunsOwn(t *testing.T) {
	sock := testSocket(t)
	ended := endedRunMark(t)
	live := strings.Split(testRun, "/")[0] + "/" + strconv.Itoa(os.Getppid())
	if !runEnded(ended) || runEnded(live) {
		t.Fatalf("runEnded(%s) = %t, runEnded(%s) = %t; want an ended and a live run", ended, runEnded(ended), live, runEnded(live))
	}
	sfx := strconv.FormatInt(time.Now().UnixNano(), 36)
	d := newTestDriver(t, "sw-probe-sweep-"+sfx+":1")
	e := d.engine()
	buildLabelledProbeImage(t, e, d.JobImages["tofu"], "1001:1001", map[string]string{LabelManaged: "true", labelTestRun: ended})

	net := "sw-test-sweep-" + sfx
	if err := e.createNetwork(t.Context(), net, map[string]string{LabelManaged: "true", labelTestRun: ended}, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.do(context.Background(), http.MethodDelete, "/networks/"+net, nil, nil, nil) })
	vol := "sw-test-sweep-" + sfx
	if err := e.createVolume(t.Context(), vol, map[string]string{LabelManaged: "true", labelTestRun: ended}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.removeVolume(context.Background(), vol) })
	place := func(job, mark string) Placement {
		t.Helper()
		p, err := d.Place(t.Context(), Request{JobID: job + "-" + sfx, Attempt: 1, Kind: "tofu", Env: []string{"PROBE_EXIT=1"}, Labels: map[string]string{labelTestRun: mark}})
		if err != nil {
			t.Fatalf("Place: %v", err)
		}
		t.Cleanup(func() { _ = e.remove(context.Background(), p.ID) })
		if _, err := d.Wait(t.Context(), p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// The ended run's job on its network, kept as a failure.
	d.Network = net
	left := place("swept", ended)
	// A live run's container that uses the ended run's image, volume and network: on Podman a forced
	// removal of any of them removes the containers using it.
	kept, err := e.create(t.Context(), "sw-test-kept-"+sfx, containerConfig{
		Image: d.JobImages["tofu"], Entrypoint: []string{"/probe"}, Labels: map[string]string{LabelManaged: "true", labelTestRun: live},
		HostConfig: hostConfig{NetworkMode: net, Mounts: []mountSpec{{Type: "volume", Source: vol, Target: "/v"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.remove(context.Background(), kept) })

	if err := sweepEndedRuns(t.Context(), sock); err != nil {
		t.Fatal(err)
	}
	if _, err := e.inspect(t.Context(), left.ID); !isNotFound(err) {
		t.Errorf("an ended run's container survived the sweep: %v", err)
	}
	if _, err := e.inspect(t.Context(), kept); err != nil {
		t.Fatalf("the sweep removed a live run's container: %v", err)
	}
	if _, err := e.network(t.Context(), net); err != nil {
		t.Errorf("the sweep removed a network a live run's container is on: %v", err)
	}
	if err := e.do(t.Context(), http.MethodGet, "/volumes/"+vol, nil, nil, nil); err != nil {
		t.Errorf("the sweep removed a volume a live run's container uses: %v", err)
	}
	if !e.imageExists(t.Context(), d.JobImages["tofu"]) {
		t.Errorf("the sweep removed an image a live run's container uses")
	}

	// Once nothing uses them, the next sweep takes them.
	if err := e.remove(t.Context(), kept); err != nil {
		t.Fatal(err)
	}
	if err := sweepEndedRuns(t.Context(), sock); err != nil {
		t.Fatal(err)
	}
	if _, err := e.network(t.Context(), net); !isNotFound(err) {
		t.Errorf("an ended run's network survived the sweep: %v", err)
	}
	if err := e.do(t.Context(), http.MethodGet, "/volumes/"+vol, nil, nil, nil); !isNotFound(err) {
		t.Errorf("an ended run's volume survived the sweep: %v", err)
	}
	if e.imageExists(t.Context(), d.JobImages["tofu"]) {
		t.Errorf("an ended run's image survived the sweep")
	}
}

// A jobs network carries only the product's labels, so a killed run's is found by its name, and its
// host firewall rules go with it: the rules name the network's subnet, which the engine hands out
// again. A live run's jobs network is left alone, and so are its rules.
func TestSweepRemovesAnEndedRunsJobsNetworkAndItsFirewall(t *testing.T) {
	sock := testSocket(t)
	ended := endedRunMark(t)
	live := strings.Split(testRun, "/")[0] + "/" + strconv.Itoa(os.Getppid())
	sfx := strconv.FormatInt(time.Now().UnixNano(), 36)
	d := newTestDriver(t, "sw-probe-sweepnet-"+sfx+":1")
	e := d.engine()
	buildProbeImage(t, e, d.JobImages["tofu"], "1001:1001")
	endedTools, liveTools := "sw-tools-swept-"+sfx+":1", "sw-tools-kept-"+sfx+":1"
	buildMarkedToolsImage(t, e, endedTools, ended)
	buildToolsImage(t, e, liveTools)

	endedNet, liveNet := jobsNetworkNameFor(ended, "e"+sfx), jobsNetworkNameFor(live, "l"+sfx)
	if endedNet == "" || liveNet == "" {
		t.Fatalf("no jobs network name for the marks %s and %s", ended, live)
	}
	for _, name := range []string{endedNet, liveNet} {
		t.Cleanup(func() { _ = e.do(context.Background(), http.MethodDelete, "/networks/"+name, nil, nil, nil) })
		t.Cleanup(func() { removeFirewallChains(context.Background(), d, liveTools, name) })
	}
	programmed := false
	for _, name := range []string{endedNet, liveNet} {
		p, err := d.EnsureNetwork(t.Context(), NetworkPolicy{Name: name, FirewallImage: liveTools})
		if err != nil {
			t.Fatalf("EnsureNetwork %s: %v", name, err)
		}
		programmed = p
	}

	if err := sweepEndedRuns(t.Context(), sock); err != nil {
		t.Fatal(err)
	}
	if _, err := e.network(t.Context(), endedNet); !isNotFound(err) {
		t.Errorf("an ended run's jobs network survived the sweep: %v", err)
	}
	if _, err := e.network(t.Context(), liveNet); err != nil {
		t.Errorf("the sweep removed a live run's jobs network: %v", err)
	}
	if !programmed {
		return // A rootless engine writes no rules.
	}
	if err := d.runFirewall(t.Context(), liveTools, firewallPresent(endedNet)); err == nil {
		t.Errorf("the rules of an ended run's jobs network survived the sweep")
	}
	if err := d.runFirewall(t.Context(), liveTools, firewallPresent(liveNet)); err != nil {
		t.Errorf("the sweep removed the rules of a live run's jobs network: %v", err)
	}
}

// firewallPresent is a script that succeeds while network's rules are on the host, in either form.
func firewallPresent(network string) string {
	return fmt.Sprintf("iptables -S %[1]s >/dev/null 2>&1 || nft list table inet %[2]s >/dev/null 2>&1", chainName(network), nftTableName(network))
}
