// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// A job resolves names through the engine's resolver even when the jobs network refuses the host's
// own nameservers to it (#933, D5): Docker's embedded resolver and Podman's aardvark-dns ask them
// from the host, never from the job. So denying a nameserver, as AlwaysDenyCIDRs denies Azure's
// 168.63.129.16 in every network mode, breaks no job's DNS. The rules need a rootful engine.
//
//	go test -tags integration ./internal/launcher/driver/ -run TestContainerJobResolves

//go:build integration

package driver

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// hostNameservers are the non-loopback nameservers the host resolves with: systemd-resolved's
// upstreams where it runs (the engines read the same file), else /etc/resolv.conf's.
func hostNameservers(t *testing.T) []string {
	t.Helper()
	for _, path := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		f, err := os.Open(path) //nolint:gosec // G304: the host's resolver configuration
		if err != nil {
			continue
		}
		var out []string
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 2 || fields[0] != "nameserver" {
				continue
			}
			if a, err := netip.ParseAddr(fields[1]); err == nil && !a.IsLoopback() && !a.Is4In6() {
				out = append(out, a.String())
			}
		}
		_ = f.Close()
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func TestContainerJobResolvesNamesWithTheHostNameserversDenied(t *testing.T) {
	const name = "example.org"
	if _, err := net.DefaultResolver.LookupHost(t.Context(), name); err != nil {
		t.Skipf("this host cannot resolve %s: %v", name, err)
	}
	upstream := hostNameservers(t)
	if len(upstream) == 0 {
		t.Skip("this host has no non-loopback nameserver to deny")
	}
	for _, backend := range testFirewallBackends(t) {
		t.Run(backend.String(), func(t *testing.T) { resolvesWithNameserversDenied(t, backend, name, upstream) })
	}
}

func resolvesWithNameserversDenied(t *testing.T, backend firewallBackend, name string, upstream []string) {
	sfx := uuid.NewString()[:8]
	d := newTestDriver(t, "")
	e := d.engine()
	tools := "sw-tools-" + sfx + ":1"
	buildToolsImage(t, e, tools)
	jobsNet := jobsNetworkName(sfx)
	t.Cleanup(func() { _ = e.do(context.Background(), http.MethodDelete, "/networks/"+jobsNet, nil, nil, nil) })
	t.Cleanup(func() { removeFirewallChains(context.Background(), d, tools, jobsNet) })
	pol := NetworkPolicy{Name: jobsNet, Deny: upstream, DenyOtherNetworks: true, FirewallImage: tools, firewall: backend}
	programmed, err := d.EnsureNetwork(t.Context(), pol)
	if err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if !programmed {
		t.Skip("a rootless engine: the rules are the executor's guard, covered by its own test")
	}

	// The same lookups from a network without rules first: a nameserver that does not answer a
	// container directly even there proves nothing when it is silent under the rules.
	control := "sw-test-control-" + sfx
	if err := e.createNetwork(t.Context(), control, withTestRun(map[string]string{LabelManaged: "true"}), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.do(context.Background(), http.MethodDelete, "/networks/"+control, nil, nil, nil) })
	if isPodman(t, e) {
		// Both networks' traffic to the nameservers is routed through the host's forward hook, where
		// a Docker on the same host drops it: let it past for these two subnets, so a silent
		// nameserver means the jobs rules refused it.
		for _, n := range []string{control, jobsNet} {
			info, err := e.network(t.Context(), n)
			if err != nil {
				t.Fatal(err)
			}
			letPastDockersForwardDrop(t, d, tools, info.IPAM.Config[0].Subnet)
		}
	}
	unfirewalled := lookups(t, e, tools, control, name, upstream)
	var answering []string
	for _, ns := range upstream {
		if strings.Contains(unfirewalled, "direct "+ns+"=yes") {
			answering = append(answering, ns)
		}
	}
	if len(answering) == 0 {
		t.Skipf("no host nameserver answers a container directly, so its denial cannot be told from silence:\n%s", unfirewalled)
	}

	// A container on the jobs network meets the rules a job meets: they follow its subnet.
	got := lookups(t, e, tools, jobsNet, name, upstream)
	t.Logf("with %v denied, from a network without rules:\n%sfrom the jobs network:\n%s", upstream, unfirewalled, got)
	if !strings.Contains(got, "resolved=yes") {
		t.Errorf("a job cannot resolve %s once the host's nameservers are denied to it", name)
	}
	for _, ns := range answering {
		if !strings.Contains(got, "direct "+ns+"=no") {
			t.Errorf("a job reaches the denied nameserver %s directly", ns)
		}
	}
}

// lookups runs, in a non-root container without capabilities on network, a lookup of name through
// the container's resolver and one directly against each of upstream, and returns what it printed:
// resolved=yes|no, then direct <nameserver>=yes|no per nameserver.
func lookups(t *testing.T, e *engine, tools, network, name string, upstream []string) string {
	t.Helper()
	script := "timeout 10 nslookup " + name + " >/dev/null 2>&1 && echo resolved=yes || echo resolved=no\n"
	for _, ns := range upstream {
		script += "timeout 5 nslookup " + name + " " + ns + " >/dev/null 2>&1 && echo direct " + ns + "=yes || echo direct " + ns + "=no\n"
	}
	id, err := e.create(t.Context(), "", containerConfig{
		Image: tools, Entrypoint: []string{"/bin/sh", "-c"}, Cmd: []string{script}, User: "1001",
		Labels: withTestRun(map[string]string{LabelManaged: "true"}), HostConfig: hostConfig{NetworkMode: network, CapDrop: []string{"ALL"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.remove(context.Background(), id) })
	if err := e.start(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.wait(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	rc, err := e.logs(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	var out bytes.Buffer
	_ = DemuxLogs(rc, &out)
	return out.String()
}
