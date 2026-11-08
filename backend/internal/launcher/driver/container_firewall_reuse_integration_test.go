// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// A jobs network's rules are keyed on its subnet, and the engine hands a freed subnet out again. A
// launcher that stopped without removing its rules leaves them behind once its network is gone, so
// the rules a jobs network gets replace whatever another jobs network left on the same subnet, in
// either firewall form, and leave every other rule alone.
//
//	go test -tags integration ./internal/launcher/driver/ -run 'TestContainerFirewallReplacesAStaleRuleForAReusedSubnet'

//go:build integration

package driver

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestContainerFirewallReplacesAStaleRuleForAReusedSubnet(t *testing.T) {
	backends := testFirewallBackends(t)
	for _, stale := range backends {
		for _, fresh := range backends {
			t.Run(stale.String()+" then "+fresh.String(), func(t *testing.T) { reusedSubnet(t, stale, fresh) })
		}
	}
}

// reusedSubnet programs a jobs network with the stale backend, removes only the network, as a
// launcher that died leaves it, then programs a new jobs network on the same subnet with the fresh
// backend, and checks what a job on the new network reaches and what is left on the host.
func reusedSubnet(t *testing.T, staleBackend, freshBackend firewallBackend) {
	sfx := uuid.NewString()[:8]
	d := newTestDriver(t, "sw-probe-reuse-"+sfx+":1")
	e := d.engine()
	tools := "sw-tools-" + sfx + ":1"
	buildToolsImage(t, e, tools)
	removeNet := func(name string) { _ = e.do(context.Background(), http.MethodDelete, "/networks/"+name, nil, nil, nil) }

	subnet, otherSubnet := freeTestSubnets(t, e)
	dead, fresh, neighbour := jobsNetworkName("d"+sfx), jobsNetworkName("f"+sfx), jobsNetworkName("n"+sfx)
	for _, name := range []string{dead, fresh, neighbour} {
		t.Cleanup(func() { removeNet(name) })
		// Also the dead network's, which the test leaves on purpose: removed by chain and table name,
		// whether or not the network still exists.
		t.Cleanup(func() { removeFirewallChains(context.Background(), d, tools, name) })
	}

	// The network a launcher that died had programmed: it lets a job reach one port on the host.
	createJobsNetworkWithSubnet(t, e, dead, subnet, staleBackend)
	hostGW := hostGateway(t, e, tools, dead)
	deadPort, freshPort := hostListener(t), hostListener(t)
	programmed, err := d.EnsureNetwork(t.Context(), NetworkPolicy{
		Name: dead, AllowTCP: []string{net.JoinHostPort(hostGW, deadPort)}, FirewallImage: tools, firewall: staleBackend,
	})
	if err != nil {
		t.Fatalf("EnsureNetwork %s: %v", dead, err)
	}
	if !programmed {
		t.Skip("a rootless engine: the rules are the executor's guard, covered by its own test")
	}
	// Under the dead network's rules the port the new network will allow is refused, so a stale rule
	// still consulted for the subnet would refuse it to the new network too.
	if dialFrom(t, e, tools, dead, net.JoinHostPort(hostGW, freshPort)) {
		t.Fatalf("the dead network's rules let a job reach %s:%s; the reuse proves nothing", hostGW, freshPort)
	}

	// A live jobs network on another subnet, whose rules stay.
	createJobsNetworkWithSubnet(t, e, neighbour, otherSubnet, staleBackend)
	if _, err := d.EnsureNetwork(t.Context(), NetworkPolicy{Name: neighbour, FirewallImage: tools, firewall: staleBackend}); err != nil {
		t.Fatalf("EnsureNetwork %s: %v", neighbour, err)
	}
	// A rule for the same subnet that is no jobs network's jump, which stays as well. It returns from
	// DOCKER-USER for the test's own subnet only, after every other rule there.
	foreign := fmt.Sprintf("DOCKER-USER -s %s -j RETURN", subnet)
	t.Cleanup(func() {
		_ = d.runFirewall(context.Background(), tools, fmt.Sprintf("while iptables -D %s 2>/dev/null; do :; done; true", foreign))
	})
	runScript(t, d, tools, fmt.Sprintf("if iptables -L DOCKER-USER >/dev/null 2>&1; then iptables -A %s; fi", foreign))

	// The launcher dies: its network goes, its rules stay.
	if err := e.do(t.Context(), http.MethodDelete, "/networks/"+dead, nil, nil, nil); err != nil {
		t.Fatalf("removing the network %s: %v", dead, err)
	}
	if err := d.runFirewall(t.Context(), tools, firewallPresent(dead)); err != nil {
		t.Fatalf("the dead network's rules are not on the host after its network went: %v", err)
	}

	// A new jobs network on the same subnet.
	createJobsNetworkWithSubnet(t, e, fresh, subnet, freshBackend)
	if _, err := d.EnsureNetwork(t.Context(), NetworkPolicy{
		Name: fresh, AllowTCP: []string{net.JoinHostPort(hostGW, freshPort)}, FirewallImage: tools, firewall: freshBackend,
	}); err != nil {
		t.Fatalf("EnsureNetwork %s: %v", fresh, err)
	}

	if !dialFrom(t, e, tools, fresh, net.JoinHostPort(hostGW, freshPort)) {
		t.Errorf("a job on the new network does not reach its allowed endpoint %s:%s", hostGW, freshPort)
	}
	if err := d.runFirewall(t.Context(), tools, firewallPresent(dead)); err == nil {
		t.Errorf("the dead network's rules for %s survived the new network's", subnet)
	} else if !strings.Contains(err.Error(), "exit") {
		t.Fatal(err)
	}
	for _, name := range []string{fresh, neighbour} {
		if err := d.runFirewall(t.Context(), tools, firewallPresent(name)); err != nil {
			t.Errorf("the rules of %s are not on the host: %v", name, err)
		}
	}
	if err := d.runFirewall(t.Context(), tools, fmt.Sprintf("if iptables -L DOCKER-USER >/dev/null 2>&1; then iptables -C %s; fi", foreign)); err != nil {
		t.Errorf("a rule for %s that is no jobs network's jump was removed: %v", subnet, err)
	}
}

// runScript runs a firewall script and fails the test if it fails.
func runScript(t *testing.T, d *Container, image, script string) {
	t.Helper()
	if err := d.runFirewall(t.Context(), image, script); err != nil {
		t.Fatalf("%s: %v", script, err)
	}
}

// createJobsNetworkWithSubnet creates a jobs network as EnsureNetwork does (createJobsNetwork), on
// the subnet given, so a test can hand it a subnet another network had.
func createJobsNetworkWithSubnet(t *testing.T, e *engine, name, subnet string, backend firewallBackend) {
	t.Helper()
	info, err := e.info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rootless := slices.ContainsFunc(info.SecurityOptions, func(o string) bool { return strings.Contains(o, "rootless") })
	labels := map[string]string{LabelManaged: "true"}
	if isPodman(t, e) && !rootless {
		err = e.doAt(t.Context(), libpodAPIVersion, http.MethodPost, "/libpod/networks/create", nil, map[string]any{
			"name": name, "driver": "bridge", "labels": labels, "dns_enabled": true, "subnets": []map[string]string{{"subnet": subnet}},
		}, nil)
	} else {
		err = e.do(t.Context(), http.MethodPost, "/networks/create", nil, map[string]any{
			"Name": name, "Driver": "bridge", "CheckDuplicate": true, "Labels": labels, "Options": jobsNetworkOptions(backend, rootless),
			"IPAM": map[string]any{"Config": []map[string]string{{"Subnet": subnet}}},
		}, nil)
	}
	if err != nil {
		t.Fatalf("creating the jobs network %s on %s: %v", name, subnet, err)
	}
}

// freeTestSubnets are two /24 subnets no network on the engine overlaps.
func freeTestSubnets(t *testing.T, e *engine) (string, string) {
	t.Helper()
	used, err := e.subnetsExcept(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	var taken []netip.Prefix
	for _, s := range used {
		if p, err := netip.ParsePrefix(s); err == nil {
			taken = append(taken, p)
		}
	}
	var out []string
	// A random order, so runs on several worktrees at once rarely ask for the same subnet.
	for _, third := range rand.Perm(256) { //nolint:gosec // G404: spreads test subnets, no secret
		p := netip.MustParsePrefix(fmt.Sprintf("10.199.%d.0/24", third))
		if slices.ContainsFunc(taken, p.Overlaps) {
			continue
		}
		out = append(out, p.String())
		if len(out) == 2 {
			return out[0], out[1]
		}
	}
	t.Fatal("no free /24 in 10.199.0.0/16 on the engine")
	return "", ""
}
