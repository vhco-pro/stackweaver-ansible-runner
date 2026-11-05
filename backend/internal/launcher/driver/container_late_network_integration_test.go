// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// A network created after the launcher started is as closed to a job as one that existed before it
// (#933, D5, AC21): the rules that refuse what a host port forwards do not depend on the engine's
// list of networks, and the list itself is brought up to date before a job is placed.
//
//	go test -tags integration ./internal/launcher/driver/ -run 'TestContainerNetworkIsolation/created_later'

//go:build integration

package driver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// startService starts a TCP listener on port 8080 of a container on network, its port published on
// the host at hostPort when that is not empty ("0" lets the engine choose), and returns the
// container's address and the published port.
func startService(t *testing.T, e *engine, tools, name, network, hostPort string) (addr, published string) {
	t.Helper()
	host := map[string]any{"NetworkMode": network}
	if hostPort != "" {
		if hostPort == "0" {
			hostPort = ""
		}
		host["PortBindings"] = map[string]any{"8080/tcp": []map[string]string{{"HostIp": "0.0.0.0", "HostPort": hostPort}}}
	}
	var created struct {
		ID string `json:"Id"`
	}
	body := map[string]any{
		"Image": tools, "Entrypoint": []string{"/bin/sh", "-c"}, "Cmd": []string{"while true; do busybox nc -l -p 8080 -e /bin/true; done"},
		"User": "0", "Labels": withTestRun(map[string]string{LabelManaged: "true"}), "ExposedPorts": map[string]any{"8080/tcp": map[string]any{}},
		"HostConfig": host,
	}
	if err := e.do(t.Context(), http.MethodPost, "/containers/create", url.Values{"name": {name}}, body, &created); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.remove(context.Background(), created.ID) })
	if err := e.start(t.Context(), created.ID); err != nil {
		t.Fatal(err)
	}
	var st struct {
		NetworkSettings struct {
			Networks map[string]struct{ IPAddress string }
			Ports    map[string][]struct{ HostPort string }
		}
	}
	if err := e.do(t.Context(), http.MethodGet, "/containers/"+created.ID+"/json", nil, nil, &st); err != nil {
		t.Fatal(err)
	}
	if ports := st.NetworkSettings.Ports["8080/tcp"]; len(ports) > 0 {
		published = ports[0].HostPort
	}
	return st.NetworkSettings.Networks[network].IPAddress, published
}

// freeHostPort is a TCP port nothing on the host listens on right now.
func freeHostPort(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
}

func lateNetwork(t *testing.T, backend firewallBackend) {
	sfx := uuid.NewString()[:8]
	d := newTestDriver(t, "sw-probe-late-"+sfx+":1")
	e := d.engine()
	buildProbeImage(t, e, d.PlatformImage, "1001:1001")
	tools := "sw-tools-" + sfx + ":1"
	buildToolsImage(t, e, tools)
	removeNet := func(name string) { _ = e.do(context.Background(), http.MethodDelete, "/networks/"+name, nil, nil, nil) }

	jobsNet := jobsNetworkName(sfx)
	t.Cleanup(func() { removeNet(jobsNet) })
	t.Cleanup(func() { removeFirewallChains(context.Background(), d, tools, jobsNet) })
	pol := NetworkPolicy{Name: jobsNet, DenyOtherNetworks: true, FirewallImage: tools, firewall: backend}
	programmed, err := d.EnsureNetwork(t.Context(), pol)
	if err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}
	if !programmed {
		t.Skip("a rootless engine: the rules are the executor's guard, covered by its own test")
	}
	jn, err := e.network(t.Context(), jobsNet)
	if err != nil {
		t.Fatal(err)
	}
	if isPodman(t, e) {
		letPastDockersForwardDrop(t, d, tools, jn.IPAM.Config[0].Subnet)
	}
	// The API's address is known before the launcher starts, and the service behind it may arrive
	// after: the allowance names the original destination, whichever network ends up behind it.
	hostGW := hostGateway(t, e, tools, jobsNet)
	allowedPort := freeHostPort(t)
	pol.AllowTCP = []string{net.JoinHostPort(hostGW, allowedPort)}
	if _, err := d.EnsureNetwork(t.Context(), pol); err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}

	// Everything from here on exists only after the launcher's rules were written.
	late := "sw-test-late-" + sfx
	if err := e.createNetwork(t.Context(), late, withTestRun(map[string]string{LabelManaged: "true"}), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeNet(late) })
	lateAddr, publishedPort := startService(t, e, tools, "sw-test-late-service-"+sfx, late, "0")
	_, _ = startService(t, e, tools, "sw-test-late-api-"+sfx, late, allowedPort)
	if publishedPort == "" {
		t.Fatal("the late service published no port")
	}
	// The listener is up: the host itself reaches it.
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", publishedPort))
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the late service never answered on the host: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// No placement has happened since the network appeared, so nothing has refreshed the list: these
	// are what a job already running sees.
	for addr, want := range map[string]bool{
		net.JoinHostPort(hostGW, publishedPort): false,
		net.JoinHostPort(lateAddr, "8080"):      false,
		net.JoinHostPort(hostGW, allowedPort):   true,
	} {
		if got := dialFrom(t, e, tools, jobsNet, addr); got != want {
			t.Errorf("reaching %s from the jobs network = %t, want %t", addr, got, want)
		}
	}

	// A placement brings the list up to date, which covers what no rule can tell from the outside
	// world: a network whose bridge has a name of its own.
	res, _, _ := runProbe(t, d, Request{JobID: "late-" + sfx, Attempt: 1, Env: []string{"PROBE_DIAL=" + net.JoinHostPort(hostGW, publishedPort)}, Labels: map[string]string{}})
	if res.Reach[net.JoinHostPort(hostGW, publishedPort)] {
		t.Errorf("a job placed after the network appeared reaches its published port")
	}
	ln, err := e.network(t.Context(), late)
	if err != nil {
		t.Fatal(err)
	}
	subnet := ln.IPAM.Config[0].Subnet
	check := fmt.Sprintf("iptables -C %s -d %s -j REJECT", chainName(jobsNet), subnet)
	if backend == firewallNFTables {
		check = fmt.Sprintf("nft list chain inet %s jobs | grep -qF %q", nftTableName(jobsNet), "daddr "+subnet)
	}
	if err := d.runFirewall(t.Context(), tools, check); err != nil && strings.Contains(err.Error(), "exit") {
		t.Errorf("the placement did not deny the late network's subnet %s: %v", subnet, err)
	} else if err != nil {
		t.Fatal(err)
	}
}
