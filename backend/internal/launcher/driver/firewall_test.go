// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// ruleIndex is the line of script holding rule, or -1.
func ruleIndex(script, rule string) int {
	for i, line := range strings.Split(script, "\n") {
		if strings.TrimSpace(line) == rule {
			return i
		}
	}
	return -1
}

// firstRejectIndex is the first REJECT line appended to chain.
func firstRejectIndex(script, tool, chain string) int {
	for i, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(line, tool+" -A "+chain+" ") && strings.HasSuffix(line, "-j REJECT") {
			return i
		}
	}
	return -1
}

// In Compose the API is a container behind a port the host publishes. A job's connection to
// host.docker.internal:<published port> is rewritten by Docker's DNAT before DOCKER-USER sees it,
// so the forwarded packet carries the API container's address, inside a range the chain rejects
// (the stack's own network). The allow rule therefore matches the connection's original
// destination, which for a connection Docker does not rewrite is the destination itself, and it
// comes before every reject. The INPUT-side rule still lets a job reach an API listening on the
// host itself (a single-VM install, a worktree slot).
func TestFirewallScript_APIAllowMatchesTheOriginalDestination(t *testing.T) {
	pol := NetworkPolicy{Name: "swc-4_jobs", Deny: []string{"192.168.208.0/20"}, AllowTCP: []string{"172.17.0.1:20422"}}
	script, err := FirewallScript(pol, []string{"172.31.0.0/16"}, []string{"172.31.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	chain := chainName(pol.Name)
	allow := fmt.Sprintf("iptables -A %s -p tcp -m conntrack --ctorigdst 172.17.0.1 --ctorigdstport 20422 -j ACCEPT", chain)
	at := ruleIndex(script, allow)
	if at < 0 {
		t.Fatalf("no allow rule on the connection's original destination:\n%s", script)
	}
	if reject := firstRejectIndex(script, "iptables", chain); reject < 0 || at > reject {
		t.Fatalf("the allow rule (line %d) does not precede the first reject (line %d):\n%s", at, reject, script)
	}
	if strings.Contains(script, fmt.Sprintf("-A %s -d 172.17.0.1 -p tcp --dport 20422 -j ACCEPT", chain)) {
		t.Errorf("the forward chain still allows the API by its post-DNAT destination only:\n%s", script)
	}
	// The stack's network and the metadata ranges are still refused for everything else.
	for _, deny := range []string{"192.168.208.0/20", "169.254.0.0/16", "172.31.0.0/16", "172.31.0.1/32"} {
		if ruleIndex(script, fmt.Sprintf("iptables -A %s -d %s -j REJECT", chain, deny)) < 0 {
			t.Errorf("no reject of %s:\n%s", deny, script)
		}
	}
	// A host listener is still reached through INPUT, and nothing else on the host is.
	if ruleIndex(script, fmt.Sprintf("iptables -A %s-IN -d 172.17.0.1 -p tcp --dport 20422 -j ACCEPT", chain)) < 0 {
		t.Errorf("the INPUT-side allow for an API on the host is gone:\n%s", script)
	}
	if ruleIndex(script, fmt.Sprintf("iptables -A %s-IN -j REJECT", chain)) < 0 {
		t.Errorf("the INPUT-side chain no longer refuses the rest of the host:\n%s", script)
	}
}

// The IPv6 rules follow the same shape.
func TestFirewallScript_APIAllowMatchesTheOriginalDestinationOnIPv6(t *testing.T) {
	pol := NetworkPolicy{Name: "swc-4_jobs", Deny: []string{"fd00:10::/64"}, AllowTCP: []string{"[fd00::1]:20422"}}
	script, err := FirewallScript(pol, []string{"fd00:31::/64"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	chain := chainName(pol.Name)
	allow := fmt.Sprintf("ip6tables -A %s -p tcp -m conntrack --ctorigdst fd00::1 --ctorigdstport 20422 -j ACCEPT", chain)
	at := ruleIndex(script, allow)
	if at < 0 {
		t.Fatalf("no IPv6 allow rule on the connection's original destination:\n%s", script)
	}
	if reject := firstRejectIndex(script, "ip6tables", chain); reject < 0 || at > reject {
		t.Fatalf("the IPv6 allow rule (line %d) does not precede the first reject (line %d):\n%s", at, reject, script)
	}
	if ruleIndex(script, fmt.Sprintf("ip6tables -A %s-IN -d fd00::1 -p tcp --dport 20422 -j ACCEPT", chain)) < 0 {
		t.Errorf("the IPv6 INPUT-side allow is gone:\n%s", script)
	}
}

// The Azure platform address (WireServer, HostGAPlugin and the platform DNS) is refused to a job in
// every network mode, host-identity included: no job needs it, and a job's DNS goes through the
// engine's resolver, which asks it from the host (D5).
func TestFirewallScript_DeniesTheAzurePlatformAddressInEveryMode(t *testing.T) {
	for _, hostIdentity := range []bool{false, true} {
		pol := NetworkPolicy{Name: "jobs", AllowMetadata: hostIdentity}
		script, err := FirewallScript(pol, []string{"172.30.0.0/16"}, []string{"172.30.0.1"})
		if err != nil {
			t.Fatal(err)
		}
		if ruleIndex(script, fmt.Sprintf("iptables -A %s -d 168.63.129.16/32 -j REJECT", chainName(pol.Name))) < 0 {
			t.Errorf("host-identity %t: 168.63.129.16 is not refused:\n%s", hostIdentity, script)
		}
	}
}

// nftChain is the rules of chain in the table of family that script declares, one per line, or nil.
func nftChain(script, family, table, chain string) []string {
	var rules []string
	depth, inTable, inChain := 0, false, false
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasSuffix(line, "{") && !strings.Contains(line, "}"):
			depth++
			switch depth {
			case 1:
				inTable = line == fmt.Sprintf("table %s %s {", family, table)
			case 2:
				inChain = inTable && line == "chain "+chain+" {"
			}
		case line == "}":
			if inChain && depth == 2 {
				return rules
			}
			depth--
		case inChain && line != "":
			rules = append(rules, line)
		}
	}
	return rules
}

func indexOf(rules []string, rule string) int {
	for i, r := range rules {
		if r == rule {
			return i
		}
	}
	return -1
}

// On an engine without Docker's DOCKER-USER chain (Podman with netavark, which programs nftables)
// the same policy is a table of our own: base chains on the forward and input hooks that run before
// the engine's and firewalld's, the allowed API matched on the connection's original destination
// ahead of every reject, the metadata ranges, the Azure platform address, the platform's and the
// other networks' subnets, the network itself and its gateway refused. On the host side only the
// allowed endpoints and the engine's resolver on the network's gateway are reached.
func TestNFTablesScript_RendersTheJobsPolicy(t *testing.T) {
	pol := NetworkPolicy{Name: "swc-4_jobs", Deny: []string{"192.168.208.0/20", "10.89.1.0/24"}, AllowTCP: []string{"10.89.0.1:20422", "10.89.0.7:8080"}}
	script, err := NFTablesScript(pol, []string{"10.89.0.0/24"}, []string{"10.89.0.1"}, true)
	if err != nil {
		t.Fatal(err)
	}
	table := nftTableName(pol.Name)
	if !strings.HasPrefix(table, "sw_jobs_") || table != nftTableName(pol.Name) || table == nftTableName("other") {
		t.Fatalf("table name %q is not a stable per-network name", table)
	}
	// Replaced as a whole in one transaction, so a rerun is idempotent.
	for _, line := range []string{"add table inet " + table, "delete table inet " + table, "add table bridge " + table, "delete table bridge " + table} {
		if ruleIndex(script, line) < 0 {
			t.Errorf("no %q:\n%s", line, script)
		}
	}
	if !strings.Contains(script, "nft -f -") {
		t.Errorf("the ruleset is not applied with nft -f:\n%s", script)
	}

	forward := nftChain(script, "inet", table, "forward")
	if indexOf(forward, "type filter hook forward priority filter - 10; policy accept;") != 0 || indexOf(forward, "ip saddr 10.89.0.0/24 jump jobs") < 0 {
		t.Errorf("forward base chain = %q", forward)
	}
	// Nothing outside the network opens a connection to a job: other networks on the engine, which
	// Docker's own isolation refuses, are refused here too; replies to what a job opened pass.
	if at, in := indexOf(forward, "ip daddr 10.89.0.0/24 jump jobs_inbound"), indexOf(forward, "ip saddr 10.89.0.0/24 jump jobs"); at < 0 || at < in {
		t.Errorf("no inbound refusal after the network's own traffic: %q", forward)
	}
	if inbound := nftChain(script, "inet", table, "jobs_inbound"); len(inbound) != 2 || inbound[0] != "ct state established,related accept" || inbound[1] != "reject" {
		t.Errorf("jobs_inbound = %q, want the replies, then a reject", inbound)
	}
	input := nftChain(script, "inet", table, "input")
	if indexOf(input, "type filter hook input priority filter - 10; policy accept;") != 0 || indexOf(input, "ip saddr 10.89.0.0/24 jump jobs_in") < 0 {
		t.Errorf("input base chain = %q", input)
	}

	jobs := nftChain(script, "inet", table, "jobs")
	if indexOf(jobs, "ct state established,related accept") != 0 {
		t.Errorf("replies do not pass first: %q", jobs)
	}
	firstReject := len(jobs)
	for i, r := range jobs {
		if strings.HasSuffix(r, " reject") {
			firstReject = i
			break
		}
	}
	for _, allow := range []string{
		"meta l4proto tcp ct original ip daddr 10.89.0.1 ct original proto-dst 20422 accept",
		"meta l4proto tcp ct original ip daddr 10.89.0.7 ct original proto-dst 8080 accept",
	} {
		if at := indexOf(jobs, allow); at < 0 || at > firstReject {
			t.Errorf("%q is missing or follows the first reject: %q", allow, jobs)
		}
	}
	for _, deny := range []string{
		"ip daddr 168.63.129.16/32 reject", "ip daddr 169.254.0.0/16 reject", "ip6 daddr fd00:ec2::/64 reject", "ip6 daddr fd20:ce::/64 reject",
		"ip daddr 192.168.208.0/20 reject", "ip daddr 10.89.1.0/24 reject", "ip daddr 10.89.0.0/24 reject", "ip daddr 10.89.0.1/32 reject",
	} {
		if indexOf(jobs, deny) < 0 {
			t.Errorf("no %q in %q", deny, jobs)
		}
	}

	in := nftChain(script, "inet", table, "jobs_in")
	for _, allow := range []string{"ip daddr 10.89.0.1 tcp dport 20422 accept", "ip daddr 10.89.0.1 udp dport 53 accept", "ip daddr 10.89.0.1 tcp dport 53 accept"} {
		if indexOf(in, allow) < 0 {
			t.Errorf("no %q in %q", allow, in)
		}
	}
	if len(in) == 0 || in[len(in)-1] != "reject" || in[0] != "ct state established,related accept" {
		t.Errorf("the host side does not end in a reject after the replies: %q", in)
	}

	// Jobs on one bridge never reach each other, also where nothing loads br_netfilter and the
	// inet hooks never see bridged traffic: only the allowed endpoint on the network passes.
	bforward := nftChain(script, "bridge", table, "forward")
	if indexOf(bforward, "ip saddr 10.89.0.0/24 ip daddr 10.89.0.0/24 jump jobs") < 0 {
		t.Errorf("bridge forward chain = %q", bforward)
	}
	bjobs := nftChain(script, "bridge", table, "jobs")
	for _, r := range []string{"ip daddr 10.89.0.7 tcp dport 8080 accept", "ip saddr 10.89.0.7 tcp sport 8080 accept"} {
		if indexOf(bjobs, r) < 0 {
			t.Errorf("no %q in %q", r, bjobs)
		}
	}
	if len(bjobs) == 0 || bjobs[len(bjobs)-1] != "drop" {
		t.Errorf("bridged traffic between jobs is not dropped: %q", bjobs)
	}
}

// The engine's resolver is reached only where it listens on the gateway (Podman's aardvark-dns);
// Docker's resolver lives inside the container, so nothing on the host side opens for it.
func TestNFTablesScript_OpensDNSOnTheGatewayOnlyForAnEngineResolverThere(t *testing.T) {
	pol := NetworkPolicy{Name: "jobs"}
	script, err := NFTablesScript(pol, []string{"10.89.0.0/24"}, []string{"10.89.0.1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "dport 53") {
		t.Errorf("DNS opened on the gateway without a resolver there:\n%s", script)
	}
}

// Host-identity leaves the metadata ranges open and refuses the Azure platform address still.
func TestNFTablesScript_HostIdentityLeavesMetadataOpenButNotTheAzurePlatform(t *testing.T) {
	pol := NetworkPolicy{Name: "jobs", AllowMetadata: true, Deny: []string{"10.9.0.0/16"}}
	script, err := NFTablesScript(pol, []string{"10.89.0.0/24"}, []string{"10.89.0.1"}, true)
	if err != nil {
		t.Fatal(err)
	}
	jobs := nftChain(script, "inet", nftTableName(pol.Name), "jobs")
	for _, cidr := range MetadataCIDRs {
		if strings.Contains(strings.Join(jobs, "\n"), cidr) {
			t.Errorf("host-identity still denies %s: %q", cidr, jobs)
		}
	}
	for _, kept := range []string{"ip daddr 168.63.129.16/32 reject", "ip daddr 10.9.0.0/16 reject", "ip daddr 10.89.0.0/24 reject", "ip daddr 10.89.0.1/32 reject"} {
		if indexOf(jobs, kept) < 0 {
			t.Errorf("host-identity dropped %q: %q", kept, jobs)
		}
	}
}

// IPv6 follows the same shape, and neighbour discovery with the gateway keeps working.
func TestNFTablesScript_IPv6(t *testing.T) {
	pol := NetworkPolicy{Name: "jobs", Deny: []string{"fd00:10::/64"}, AllowTCP: []string{"[fd00:31::1]:20422"}}
	script, err := NFTablesScript(pol, []string{"fd00:31::/64"}, []string{"fd00:31::1"}, true)
	if err != nil {
		t.Fatal(err)
	}
	table := nftTableName(pol.Name)
	if indexOf(nftChain(script, "inet", table, "forward"), "ip6 saddr fd00:31::/64 jump jobs") < 0 {
		t.Errorf("no IPv6 jump:\n%s", script)
	}
	jobs := nftChain(script, "inet", table, "jobs")
	for _, r := range []string{"meta l4proto tcp ct original ip6 daddr fd00:31::1 ct original proto-dst 20422 accept", "ip6 daddr fd00:10::/64 reject", "ip6 daddr fd00:31::/64 reject", "ip6 daddr fd00:31::1/128 reject"} {
		if indexOf(jobs, r) < 0 {
			t.Errorf("no %q in %q", r, jobs)
		}
	}
	in := nftChain(script, "inet", table, "jobs_in")
	for _, r := range []string{"ip6 daddr fd00:31::1 tcp dport 20422 accept", "ip6 daddr fd00:31::1 udp dport 53 accept", "icmpv6 type { nd-neighbor-solicit, nd-neighbor-advert } accept"} {
		if indexOf(in, r) < 0 {
			t.Errorf("no %q in %q", r, in)
		}
	}
	if indexOf(nftChain(script, "bridge", table, "forward"), "ip6 saddr fd00:31::/64 ip6 daddr fd00:31::/64 jump jobs") < 0 {
		t.Errorf("no IPv6 bridge isolation:\n%s", script)
	}
}

// fakeEngine answers GET /version and GET /info as the given engine would. It listens on TCP, not a
// unix socket, so a long temporary directory cannot push the socket past the path limit.
func fakeEngine(t *testing.T, version, info string) *engine {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/version"):
			_, _ = io.WriteString(w, version)
		case strings.HasSuffix(r.URL.Path, "/info"):
			_, _ = io.WriteString(w, info)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	return &engine{socket: addr, http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		},
	}}}
}

// The firewall backend follows the engine: Docker's DOCKER-USER chain exists only under its
// iptables backend; Podman (netavark, or CNI) and Docker's nftables backend get a table of our own.
func TestFirewallBackendFollowsTheEngine(t *testing.T) {
	docker := `{"Components":[{"Name":"Engine","Version":"29.7.2"}]}`
	podman := `{"Components":[{"Name":"Podman Engine","Version":"5.8.4"},{"Name":"Conmon"}]}`
	for _, c := range []struct {
		name, version, info string
		want                firewallBackend
		podman              bool
	}{
		{"docker iptables", docker, `{"FirewallBackend":{"Driver":"iptables"}}`, firewallIPTables, false},
		{"docker iptables with firewalld", docker, `{"FirewallBackend":{"Driver":"iptables+firewalld"}}`, firewallIPTables, false},
		{"docker before the field", docker, `{}`, firewallIPTables, false},
		{"docker nftables", docker, `{"FirewallBackend":{"Driver":"nftables+firewalld"}}`, firewallNFTables, false},
		{"podman", podman, `{"SecurityOptions":["name=seccomp,profile=default"]}`, firewallNFTables, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, podman, err := detectFirewall(t.Context(), fakeEngine(t, c.version, c.info))
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want || podman != c.podman {
				t.Errorf("detectFirewall = %v, podman %t; want %v, %t", got, podman, c.want, c.podman)
			}
		})
	}
}

// Docker's inter-container switch is set only where Docker's own iptables rules enforce it and our
// DOCKER-USER chain can accept the API past it, or on a rootless engine, where the engine's
// isolation is all there is. Under a table of our own an engine-side drop would refuse the allowed
// API on the jobs network, which no accept of ours can undo, so the table is the whole policy.
func TestJobsNetworkOptionsFollowTheBackend(t *testing.T) {
	if got := jobsNetworkOptions(firewallIPTables, false); got["com.docker.network.bridge.enable_icc"] != "false" {
		t.Errorf("iptables: options %v, want inter-container traffic off", got)
	}
	if got := jobsNetworkOptions(firewallNFTables, true); got["com.docker.network.bridge.enable_icc"] != "false" {
		t.Errorf("rootless: options %v, want inter-container traffic off", got)
	}
	if got := jobsNetworkOptions(firewallNFTables, false); len(got) != 0 {
		t.Errorf("nftables: options %v, want none", got)
	}
}

// A connection the host rewrote is on its way to a published port. It is refused on that alone, so
// the rule covers every network on the host, also one created after the rules were written and
// whichever range it sits in. Only the allowed endpoints, matched on the original destination, come
// before it; the replies rule above them concerns established connections, not a new one.
func TestFirewallScript_RefusesWhatTheHostRewroteWhicheverNetworkItLandsOn(t *testing.T) {
	pol := NetworkPolicy{Name: "jobs", AllowTCP: []string{"172.17.0.1:20422"}}
	script, err := FirewallScript(pol, []string{"172.31.0.0/16"}, []string{"172.31.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	chain := chainName(pol.Name)
	allow := ruleIndex(script, fmt.Sprintf("iptables -A %s -p tcp -m conntrack --ctorigdst 172.17.0.1 --ctorigdstport 20422 -j ACCEPT", chain))
	dnat := ruleIndex(script, fmt.Sprintf("iptables -A %s -m conntrack --ctstate DNAT -j REJECT", chain))
	if allow < 0 || dnat < allow {
		t.Fatalf("the refusal of rewritten connections (line %d) must follow the allowed API (line %d):\n%s", dnat, allow, script)
	}
	if strings.Contains(script, "ip6tables -A "+chain+" -m conntrack --ctstate DNAT") {
		t.Errorf("an IPv4 network got an IPv6 rule:\n%s", script)
	}
	v6, err := FirewallScript(NetworkPolicy{Name: "jobs"}, []string{"fd00:31::/64"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ruleIndex(v6, fmt.Sprintf("ip6tables -A %s -m conntrack --ctstate DNAT -j REJECT", chain)) < 0 {
		t.Errorf("an IPv6 network does not refuse rewritten connections:\n%s", v6)
	}
}

func TestNFTablesScript_RefusesWhatTheHostRewroteWhicheverNetworkItLandsOn(t *testing.T) {
	pol := NetworkPolicy{Name: "jobs", AllowTCP: []string{"10.89.0.1:20422"}}
	script, err := NFTablesScript(pol, []string{"10.89.0.0/24"}, []string{"10.89.0.1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	jobs := nftChain(script, "inet", nftTableName(pol.Name), "jobs")
	allow := indexOf(jobs, "meta l4proto tcp ct original ip daddr 10.89.0.1 ct original proto-dst 20422 accept")
	dnat := indexOf(jobs, "ct status dnat reject")
	if allow < 0 || dnat < allow {
		t.Fatalf("the refusal of rewritten connections must follow the allowed API: %q", jobs)
	}
}

// Podman does not refuse traffic between its bridges the way Docker does, and the jobs network
// carries no engine-side isolation here, so a network created later would be open by its address.
// Podman's bridges share a prefix, and a rule on it needs no list. The prefix is Podman's alone:
// on Docker the same kind of rule would have to name br-*, which is also what an administrator
// calls an uplink, while Docker's own rules already refuse the traffic.
func TestNFTablesScript_RefusesEveryPodmanBridgeByName(t *testing.T) {
	pol := NetworkPolicy{Name: "jobs", AllowTCP: []string{"10.89.0.7:8080"}}
	podman, err := NFTablesScript(pol, []string{"10.89.0.0/24"}, []string{"10.89.0.1"}, true)
	if err != nil {
		t.Fatal(err)
	}
	jobs := nftChain(podman, "inet", nftTableName(pol.Name), "jobs")
	allow, bridges := indexOf(jobs, "meta l4proto tcp ct original ip daddr 10.89.0.7 ct original proto-dst 8080 accept"), indexOf(jobs, `oifname "podman*" reject`)
	if allow < 0 || bridges < allow {
		t.Fatalf("the refusal of Podman's bridges must follow the allowed API: %q", jobs)
	}
	docker, err := NFTablesScript(pol, []string{"10.89.0.0/24"}, []string{"10.89.0.1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(docker, "oifname") {
		t.Errorf("a rule on Podman's bridge names on another engine:\n%s", docker)
	}
}

// Appending keeps the allowed endpoints ahead of the new denial and leaves the rest of the chain in
// force: a flush would open the network for a moment under jobs already running.
func TestDenySubnetsScript_AppendsWithoutFlushing(t *testing.T) {
	chain := chainName("jobs")
	iptables, err := denySubnetsScript(firewallIPTables, "jobs", []string{"172.29.0.0/16", "fd00:29::/64"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		fmt.Sprintf("iptables -C %[1]s -d 172.29.0.0/16 -j REJECT 2>/dev/null || iptables -A %[1]s -d 172.29.0.0/16 -j REJECT", chain),
		fmt.Sprintf("if ip6tables -L %[1]s >/dev/null 2>&1; then ip6tables -C %[1]s -d fd00:29::/64 -j REJECT 2>/dev/null || ip6tables -A %[1]s -d fd00:29::/64 -j REJECT; fi", chain),
	} {
		if ruleIndex(iptables, want) < 0 {
			t.Errorf("no %q in:\n%s", want, iptables)
		}
	}
	nft, err := denySubnetsScript(firewallNFTables, "jobs", []string{"10.89.7.0/24", "fd00:29::/64"})
	if err != nil {
		t.Fatal(err)
	}
	table := nftTableName("jobs")
	for _, want := range []string{"add rule inet " + table + " jobs ip daddr 10.89.7.0/24 reject", "add rule inet " + table + " jobs ip6 daddr fd00:29::/64 reject"} {
		if !strings.Contains(nft, want) {
			t.Errorf("no %q in:\n%s", want, nft)
		}
	}
	for _, script := range []string{iptables, nft} {
		if strings.Contains(script, " -F ") || strings.Contains(script, "delete table") || strings.Contains(script, "flush") {
			t.Errorf("the script removes rules:\n%s", script)
		}
	}
}

// firewallEngine answers what closeNewNetworks asks of an engine and records the writers it runs.
type firewallEngine struct {
	mu       sync.Mutex
	networks []string
	writers  []string
	exit     int
}

func (f *firewallEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/networks"):
		var nets []string
		for _, s := range f.networks {
			nets = append(nets, fmt.Sprintf(`{"Name":"net-%[1]s","IPAM":{"Config":[{"Subnet":"%[1]s"}]}}`, s))
		}
		_, _ = fmt.Fprintf(w, "[%s]", strings.Join(nets, ","))
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/create"):
		var cfg containerConfig
		_ = json.NewDecoder(r.Body).Decode(&cfg)
		f.writers = append(f.writers, strings.Join(cfg.Cmd, " "))
		_, _ = io.WriteString(w, `{"Id":"writer"}`)
	case strings.HasSuffix(r.URL.Path, "/wait"):
		_, _ = fmt.Fprintf(w, `{"StatusCode":%d}`, f.exit)
	case strings.HasSuffix(r.URL.Path, "/logs"):
		_, _ = io.WriteString(w, "")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (f *firewallEngine) written() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writers)
}

func (f *firewallEngine) set(exit int, networks ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exit, f.networks = exit, networks
}

// A placement denies only the networks that appeared since the rules were written, runs no writer
// when none did, and refuses to place the job if the writer fails, which it then tries again.
func TestCloseNewNetworksDeniesOnlyWhatAppearedAndFailsClosed(t *testing.T) {
	f := &firewallEngine{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	e := &engine{socket: addr, http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		},
	}}}
	d := &Container{e: e}

	// Before the rules are written, as on a rootless engine, there is nothing to keep up to date.
	f.set(0, "172.18.0.0/16")
	if err := d.closeNewNetworks(t.Context()); err != nil || len(f.written()) != 0 {
		t.Fatalf("without written rules: err %v, writers %q", err, f.written())
	}

	d.fw = &jobsFirewall{network: "jobs", backend: firewallIPTables, image: "tools", known: map[string]bool{"172.18.0.0/16": true}}
	if err := d.closeNewNetworks(t.Context()); err != nil || len(f.written()) != 0 {
		t.Fatalf("with no new network: err %v, writers %q", err, f.written())
	}

	f.set(1, "172.18.0.0/16", "172.29.0.0/16")
	if err := d.closeNewNetworks(t.Context()); err == nil {
		t.Fatal("a failed writer let the placement through")
	}
	f.set(0, "172.18.0.0/16", "172.29.0.0/16")
	if err := d.closeNewNetworks(t.Context()); err != nil {
		t.Fatal(err)
	}
	writers := f.written()
	if len(writers) != 2 || !strings.Contains(writers[1], "172.29.0.0/16") || strings.Contains(writers[1], "172.18.0.0/16") {
		t.Fatalf("writers = %q, want the failed one and a retry naming only the new network", writers)
	}
	if err := d.closeNewNetworks(t.Context()); err != nil || len(f.written()) != 2 {
		t.Fatalf("after the retry: err %v, writers %q", err, f.written())
	}
}
