// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"
)

// MetadataCIDRs are the cloud metadata ranges no job on a platform-identity pool may reach (D5):
// AWS, Azure and GCP instance metadata and the EKS Pod Identity agent live in link-local space,
// with the AWS IPv6 endpoints in fd00:ec2::/64 and GCP's in fd20:ce::/64.
var MetadataCIDRs = []string{"169.254.0.0/16", "fd00:ec2::/64", "fd20:ce::/64"}

// AlwaysDenyCIDRs are refused to every job in every network mode, host-identity included (D5):
// Azure's platform address, behind which sit the WireServer, the HostGAPlugin and the platform DNS.
// No job needs it: a job resolves names through the engine's resolver (Docker's embedded resolver,
// Podman's aardvark-dns), which asks the host's nameservers from the host, never from the job.
var AlwaysDenyCIDRs = []string{"168.63.129.16/32"}

// NetworkPolicy is what the jobs network lets a job reach (D5): DNS and the internet, the API on
// its address and port, and nothing in Deny (the metadata ranges, the platform services' subnets,
// the engine's own gateway). Jobs on the network cannot reach each other: the network's own subnet
// is denied but for the allowed endpoints, so the API may sit on the jobs network itself.
type NetworkPolicy struct {
	// Name is the jobs network, one per launcher host.
	Name string
	// AllowTCP are address:port pairs a job may reach even inside a denied range: the API.
	AllowTCP []string
	// Deny are CIDRs no job may reach, in addition to MetadataCIDRs.
	Deny []string
	// DenyOtherNetworks denies the subnet of every other network on the engine too: the platform's
	// Compose services and anything else running beside the launcher. Nothing else has to name what
	// a job may not reach through a port the host publishes: the rules refuse every connection the
	// host rewrote, on any network. The list covers a network's own addresses, and a placement adds
	// the networks that appeared since (closeNewNetworks) (D5).
	DenyOtherNetworks bool
	// FirewallImage runs the one-shot rule writer on a rootful engine; it needs iptables for
	// Docker's iptables backend and nft for every other engine.
	FirewallImage string
	// AllowMetadata leaves MetadataCIDRs open: the host-identity network mode, where jobs use the
	// node's identity (D5). Every other denial stays, AlwaysDenyCIDRs included.
	AllowMetadata bool

	// firewall overrides the backend the engine is detected to use; tests set it.
	firewall firewallBackend
}

// firewallBackend is how the jobs network's rules are written.
type firewallBackend int

const (
	firewallDetect firewallBackend = iota
	// firewallIPTables is Docker's iptables backend: a chain jumped from DOCKER-USER, which Docker's
	// forward path consults before its own rules, so an accept there passes its inter-container drop.
	firewallIPTables
	// firewallNFTables is every engine without a DOCKER-USER chain (Podman, whose netavark programs
	// nftables or iptables itself, and Docker's nftables backend): an nftables table of our own whose
	// base chains run before the engine's and firewalld's.
	firewallNFTables
)

func (b firewallBackend) String() string {
	switch b {
	case firewallIPTables:
		return "iptables"
	case firewallNFTables:
		return "nftables"
	case firewallDetect:
	}
	return "detect"
}

// detectFirewall is the backend the engine's rules are written for, and whether the engine is
// Podman. Podman never creates DOCKER-USER, whichever of nftables or iptables netavark (or CNI)
// writes, and its networks resolve through aardvark-dns on the gateway. Docker names its backend in
// GET /info from 28 on; an older Docker has only iptables. Its embedded resolver answers inside the
// container, so nothing on the host side opens for it.
func detectFirewall(ctx context.Context, e *engine) (firewallBackend, bool, error) {
	var version struct{ Components []struct{ Name string } }
	if err := e.do(ctx, http.MethodGet, "/version", nil, nil, &version); err != nil {
		return firewallDetect, false, fmt.Errorf("reading the engine's version: %w", err)
	}
	for _, c := range version.Components {
		if strings.Contains(c.Name, "Podman") {
			return firewallNFTables, true, nil
		}
	}
	var info struct{ FirewallBackend struct{ Driver string } }
	if err := e.do(ctx, http.MethodGet, "/info", nil, nil, &info); err != nil {
		return firewallDetect, false, fmt.Errorf("reading the engine's firewall backend: %w", err)
	}
	if strings.HasPrefix(info.FirewallBackend.Driver, "nftables") {
		return firewallNFTables, false, nil
	}
	return firewallIPTables, false, nil
}

// jobsNetworkOptions are the engine options the jobs network is created with. Docker's
// inter-container switch is set where Docker's iptables rules enforce it and our DOCKER-USER chain
// accepts the API past it, and on a rootless engine, whose own isolation is all there is (Podman
// reads it as isolate). Under a table of our own an engine-side drop would refuse the allowed API
// on the jobs network, and an accept in another table cannot undo a drop, so the table is the
// whole policy there.
func jobsNetworkOptions(b firewallBackend, rootless bool) map[string]string {
	if b == firewallIPTables || rootless {
		return map[string]string{"com.docker.network.bridge.enable_icc": "false"}
	}
	return map[string]string{}
}

// libpodAPIVersion is the version of Podman's own API the jobs network is created through.
const libpodAPIVersion = "v4.0.0"

// createJobsNetwork creates the jobs network. On rootful Podman it goes through Podman's own API:
// Podman's Docker API creates every network with isolate=true, for Docker compatibility, and
// netavark then drops traffic between isolated networks in its own table, where no accept of ours
// can undo it, so a job could not reach an allowed endpoint that another network's container
// publishes. Created without isolation, the jobs table is the whole policy there: it refuses every
// other Podman bridge and every rewritten connection but the allowed endpoints, in both directions.
func createJobsNetwork(ctx context.Context, e *engine, name string, backend firewallBackend, podman, rootless bool) error {
	labels := map[string]string{LabelManaged: "true"}
	if podman && !rootless {
		return e.doAt(ctx, libpodAPIVersion, http.MethodPost, "/libpod/networks/create", nil, map[string]any{
			"name": name, "driver": "bridge", "labels": labels, "dns_enabled": true,
		}, nil)
	}
	return e.createNetwork(ctx, name, labels, jobsNetworkOptions(backend, rootless))
}

// EnsureNetwork creates the jobs network if it is missing and, on a rootful engine, programs the
// host firewall for it: a chain under Docker's DOCKER-USER on Docker's iptables backend, an
// nftables table of our own on every other engine. A rootless engine cannot touch the host
// firewall: there the network is created the same way and the executor refuses to start if a
// metadata address is reachable from it (STACKWEAVER_NETWORK_GUARD), so a misconfigured host fails
// closed. It returns whether the rules were programmed.
func (d *Container) EnsureNetwork(ctx context.Context, pol NetworkPolicy) (bool, error) {
	e := d.engine()
	info, err := e.info(ctx)
	if err != nil {
		return false, err
	}
	rootless := slices.ContainsFunc(info.SecurityOptions, func(o string) bool { return strings.Contains(o, "rootless") })
	backend, podman, err := detectFirewall(ctx, e)
	if err != nil {
		return false, err
	}
	if pol.firewall != firewallDetect {
		backend = pol.firewall
	}
	if _, err := e.network(ctx, pol.Name); isNotFound(err) {
		// Jobs never reach each other: on Docker's iptables backend inter-container traffic on the
		// network is off (Docker loads the bridge netfilter module for it), and only the allowed
		// endpoints are accepted past it; elsewhere the rules below refuse it.
		if err := createJobsNetwork(ctx, e, pol.Name, backend, podman, rootless); err != nil {
			return false, fmt.Errorf("creating the jobs network %s: %w", pol.Name, err)
		}
	} else if err != nil {
		return false, fmt.Errorf("inspecting the jobs network %s: %w", pol.Name, err)
	}
	d.Network = pol.Name
	net, err := e.network(ctx, pol.Name)
	if err != nil {
		return false, err
	}
	if rootless {
		return false, nil
	}
	var subnets []string
	for _, c := range net.IPAM.Config {
		if c.Subnet != "" {
			subnets = append(subnets, c.Subnet)
		}
	}
	if len(subnets) == 0 {
		return false, fmt.Errorf("the jobs network %s has no subnet", pol.Name)
	}
	var others []string
	if pol.DenyOtherNetworks {
		if others, err = e.subnetsExcept(ctx, pol.Name); err != nil {
			return false, fmt.Errorf("listing the engine's networks: %w", err)
		}
		pol.Deny = append(append([]string{}, pol.Deny...), others...)
	}
	var script string
	if backend == firewallIPTables {
		script, err = FirewallScript(pol, subnets, gateways(net))
	} else {
		script, err = NFTablesScript(pol, subnets, gateways(net), podman)
	}
	if err != nil {
		return false, err
	}
	if err := d.runFirewall(ctx, pol.FirewallImage, script); err != nil {
		return false, err
	}
	d.fw = nil
	if pol.DenyOtherNetworks {
		known := make(map[string]bool, len(others))
		for _, s := range others {
			known[s] = true
		}
		d.fw = &jobsFirewall{network: pol.Name, backend: backend, image: pol.FirewallImage, known: known}
	}
	return true, nil
}

// jobsFirewall is what EnsureNetwork wrote for a network that denies the engine's other networks.
type jobsFirewall struct {
	network string
	backend firewallBackend
	image   string

	mu sync.Mutex
	// known are the subnets the rules deny by address.
	known map[string]bool
}

// closeNewNetworks denies the subnets of the networks that appeared since the rules were written,
// before a job is placed. The rules already refuse what a host port forwards to any network, and
// the engine's own isolation refuses the rest of a network whose bridge it knows; this reaches what
// neither can name, such as a network with a bridge of its own on Podman, or a network Docker
// leaves routable. A job is not placed if that fails, and the next placement tries again.
func (d *Container) closeNewNetworks(ctx context.Context) error {
	fw := d.fw
	if fw == nil {
		return nil
	}
	fw.mu.Lock()
	defer fw.mu.Unlock()
	subnets, err := d.engine().subnetsExcept(ctx, fw.network)
	if err != nil {
		return fmt.Errorf("listing the engine's networks: %w", err)
	}
	var fresh []string
	for _, s := range subnets {
		if !fw.known[s] {
			fresh = append(fresh, s)
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	script, err := denySubnetsScript(fw.backend, fw.network, fresh)
	if err != nil {
		return err
	}
	if err := d.runFirewall(ctx, fw.image, script); err != nil {
		return fmt.Errorf("denying the networks that appeared on the engine: %w", err)
	}
	for _, s := range fresh {
		fw.known[s] = true
	}
	return nil
}

// denySubnetsScript appends denials of subnets to the jobs network's rules. The chains end in their
// denials, so the allowed endpoints stay ahead of these, and nothing is flushed: the rules stay in
// force for the jobs running while it happens.
func denySubnetsScript(backend firewallBackend, network string, subnets []string) (string, error) {
	var b strings.Builder
	b.WriteString("set -e\n")
	for _, s := range subnets {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return "", fmt.Errorf("parsing the subnet %q: %w", s, err)
		}
		p = p.Masked()
		chain := chainName(network)
		switch {
		case backend == firewallNFTables:
			fmt.Fprintf(&b, "nft add rule inet %s jobs %s daddr %s reject\n", nftTableName(network), nftAddr(p.Addr()), p)
		case p.Addr().Is4():
			fmt.Fprintf(&b, "iptables -C %[1]s -d %[2]s -j REJECT 2>/dev/null || iptables -A %[1]s -d %[2]s -j REJECT\n", chain, p)
		default:
			fmt.Fprintf(&b, "if ip6tables -L %[1]s >/dev/null 2>&1; then ip6tables -C %[1]s -d %[2]s -j REJECT 2>/dev/null || ip6tables -A %[1]s -d %[2]s -j REJECT; fi\n", chain, p)
		}
	}
	return b.String(), nil
}

func gateways(n networkState) []string {
	var out []string
	for _, c := range n.IPAM.Config {
		if c.Gateway != "" {
			out = append(out, c.Gateway)
		}
	}
	return out
}

// chainName is the firewall chain of one jobs network.
func chainName(network string) string {
	sum := sha256.Sum256([]byte(network))
	return "SW-JOBS-" + strings.ToUpper(hex.EncodeToString(sum[:])[:8])
}

// jobsRules is the policy of one jobs network, parsed, in the order every backend applies it.
type jobsRules struct {
	// allow are the endpoints a job reaches even inside a denied range: the API.
	allow []netip.AddrPort
	// deny are the ranges refused to a new connection from a job: AlwaysDenyCIDRs, the metadata
	// ranges unless the pool uses the node's identity, the policy's own, then the network's subnets
	// and gateways, so jobs never find each other or the host through the network.
	deny []netip.Prefix
	// subnets are the network's own, whose traffic the rules apply to.
	subnets []netip.Prefix
	// gateways are the network's gateways: the host as a job on the network reaches it.
	gateways []netip.Addr
}

func parseJobsRules(pol NetworkPolicy, subnets, gateways []string) (jobsRules, error) {
	var r jobsRules
	for _, hp := range pol.AllowTCP {
		ap, err := netip.ParseAddrPort(hp)
		if err != nil {
			return r, fmt.Errorf("parsing the allowed endpoint %q: %w", hp, err)
		}
		r.allow = append(r.allow, ap)
	}
	for _, s := range subnets {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return r, fmt.Errorf("parsing the jobs network's subnet %q: %w", s, err)
		}
		r.subnets = append(r.subnets, p.Masked())
	}
	for _, g := range gateways {
		a, err := netip.ParseAddr(g)
		if err != nil {
			return r, fmt.Errorf("parsing the jobs network's gateway %q: %w", g, err)
		}
		r.gateways = append(r.gateways, a)
	}
	deny := append([]string{}, AlwaysDenyCIDRs...)
	if !pol.AllowMetadata {
		deny = append(deny, MetadataCIDRs...)
	}
	deny = append(deny, pol.Deny...)
	for _, cidr := range deny {
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			a, aerr := netip.ParseAddr(cidr)
			if aerr != nil {
				return r, fmt.Errorf("parsing the denied range %q: %w", cidr, err)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		r.deny = append(r.deny, p.Masked())
	}
	// Jobs never need to find each other: the network's own subnet is denied too, after the allowed
	// endpoints, so an API attached to the jobs network stays reachable.
	r.deny = append(r.deny, r.subnets...)
	for _, g := range r.gateways {
		r.deny = append(r.deny, netip.PrefixFrom(g, g.BitLen()))
	}
	return r, nil
}

// FirewallScript is the shell script that programs the jobs network's rules on Docker's iptables
// backend: one chain per network, flushed and refilled so it is idempotent, jumped to from
// DOCKER-USER for traffic from the network's subnets. Allowed API endpoints come first and are
// accepted outright, past Docker's inter-container drop, so an API on the jobs network stays
// reachable; they match the connection's original destination (conntrack), so an API container
// behind a port the host publishes, whose connection Docker's DNAT has already rewritten to the
// container's address, is reached as well. Every other connection the host rewrote is refused
// without naming a network, which covers a published port on a network created later; Docker's own
// rules refuse the addresses of such a network. Then the denied ranges; anything else (the internet)
// falls through to Docker's own rules. A job's DNS goes to Docker's embedded resolver inside the
// container, which asks the host's nameservers from the host's own namespace.
//
// A second chain, jumped to from INPUT, covers what a job reaches on the launcher's own host
// through a gateway (host.docker.internal): forwarded traffic never meets it, so DOCKER-USER alone
// would let a job reach every port the host publishes, a dev or single-VM install's database
// among them. There only the allowed endpoints pass; everything else a job opens to the host is
// refused (AC21).
func FirewallScript(pol NetworkPolicy, subnets, gateways []string) (string, error) {
	r, err := parseJobsRules(pol, subnets, gateways)
	if err != nil {
		return "", err
	}
	chain := chainName(pol.Name)
	inChain := chain + "-IN"
	var v4, v6, in4, in6 []string
	add := func(rule string, addr netip.Addr) {
		if addr.Is4() {
			v4 = append(v4, rule)
		} else {
			v6 = append(v6, rule)
		}
	}
	for _, ap := range r.allow {
		// Matched on the connection's original destination: Docker rewrites a connection to a port the
		// host publishes (an API container behind host.docker.internal:<port>) to the container's
		// address before DOCKER-USER sees it, and that address sits in a range denied below. For a
		// connection nothing rewrote, the original destination is the destination.
		add(fmt.Sprintf("-A %s -p tcp -m conntrack --ctorigdst %s --ctorigdstport %d -j ACCEPT", chain, ap.Addr(), ap.Port()), ap.Addr())
		if ap.Addr().Is4() {
			in4 = append(in4, fmt.Sprintf("-A %s -d %s -p tcp --dport %d -j ACCEPT", inChain, ap.Addr(), ap.Port()))
		} else {
			in6 = append(in6, fmt.Sprintf("-A %s -d %s -p tcp --dport %d -j ACCEPT", inChain, ap.Addr(), ap.Port()))
		}
	}
	in4 = append(in4, fmt.Sprintf("-A %s -j REJECT", inChain))
	in6 = append(in6, fmt.Sprintf("-A %s -j REJECT", inChain))
	// Anything else the host rewrote is on its way to a port it publishes, on whichever network,
	// also one created after these rules were written.
	dnat := fmt.Sprintf("-A %s -m conntrack --ctstate DNAT -j REJECT", chain)
	if slices.ContainsFunc(r.subnets, func(p netip.Prefix) bool { return p.Addr().Is4() }) {
		v4 = append(v4, dnat)
	}
	if slices.ContainsFunc(r.subnets, func(p netip.Prefix) bool { return p.Addr().Is6() }) {
		v6 = append(v6, dnat)
	}
	for _, p := range r.deny {
		add(fmt.Sprintf("-A %s -d %s -j REJECT", chain, p), p.Addr())
	}
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString(staleRulesFunctions)
	// We keep no nftables table on this backend: one on the network's subnet is another network's.
	for _, s := range r.subnets {
		fmt.Fprintf(&b, "sw_drop_stale_tables %s ''\n", s)
	}
	write := func(tool string, rules, inRules []string, family func(netip.Prefix) bool) {
		for _, c := range []struct {
			name, from string
			rules      []string
		}{{chain, "DOCKER-USER", rules}, {inChain, "INPUT", inRules}} {
			fmt.Fprintf(&b, "%s -N %s 2>/dev/null || true\n%s -F %s\n", tool, c.name, tool, c.name)
			// Replies to what a job may open (and the API's answers to it) pass; the denials below
			// apply to new connections only.
			fmt.Fprintf(&b, "%s -A %s -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT\n", tool, c.name)
			for _, rule := range c.rules {
				fmt.Fprintf(&b, "%s %s\n", tool, rule)
			}
			for _, s := range r.subnets {
				if !family(s) {
					continue
				}
				fmt.Fprintf(&b, "sw_drop_stale_jumps %s %s %s %s\n", tool, c.from, s, c.name)
				fmt.Fprintf(&b, "%s -C %s -s %s -j %s 2>/dev/null || %s -I %s -s %s -j %s\n", tool, c.from, s, c.name, tool, c.from, s, c.name)
			}
		}
	}
	write("iptables", v4, in4, func(p netip.Prefix) bool { return p.Addr().Is4() })
	if len(v6) > 0 {
		b.WriteString("if command -v ip6tables >/dev/null && ip6tables -L DOCKER-USER >/dev/null 2>&1; then\n")
		write("ip6tables", v6, in6, func(p netip.Prefix) bool { return p.Addr().Is6() })
		b.WriteString("fi\n")
	}
	return b.String(), nil
}

// nftTableName is the nftables table of one jobs network, in the inet and the bridge family.
func nftTableName(network string) string {
	return "sw_jobs_" + strings.ToLower(strings.TrimPrefix(chainName(network), "SW-JOBS-"))
}

// nftAddr is the nftables selector of addr's family: ip or ip6.
func nftAddr(a netip.Addr) string {
	if a.Is4() {
		return "ip"
	}
	return "ip6"
}

// NFTablesScript is the shell script that programs the jobs network's rules on an engine without
// Docker's DOCKER-USER chain: Podman with netavark (which writes nftables, or iptables) or CNI, and
// Docker's nftables backend. It holds the same policy as FirewallScript, as nftables tables of our
// own that `nft -f` replaces in one transaction, so a rerun is idempotent and a failure leaves the
// previous rules in place.
//
// nftables evaluates every base chain on a hook: a drop in any table is final, while an accept
// ends only its own chain. So the tables refuse what the policy denies, before the engine's and
// firewalld's chains run, and leave the rest to them; they never need to accept past an engine's
// drop, which is why the jobs network carries no engine-side isolation on this backend
// (jobsNetworkOptions).
//
//   - inet forward, for traffic from the network's subnets: replies pass, then the allowed
//     endpoints by the connection's original destination (ahead of the engine's DNAT of a port the
//     host publishes), then every other connection the host rewrote, on any network, and on Podman
//     every Podman bridge by name, because netavark leaves those open to each other and neither
//     rule needs a list; then every denied range. The internet falls through. Traffic to the
//     network from anywhere else passes only as a reply, as Docker's network isolation has it.
//   - inet input, what a job opens to the launcher's host through a gateway: replies, the allowed
//     endpoints and, where the engine's resolver listens on the network's gateway (Podman's
//     aardvark-dns), DNS to the gateway pass; everything else is refused (AC21).
//   - bridge forward, traffic between two addresses on the network: the inet hooks see bridged
//     traffic only where something has loaded br_netfilter, which Podman does not, so jobs on one
//     bridge would reach each other. Only an allowed endpoint on the network and its answers pass.
func NFTablesScript(pol NetworkPolicy, subnets, gateways []string, podman bool) (string, error) {
	r, err := parseJobsRules(pol, subnets, gateways)
	if err != nil {
		return "", err
	}
	table := nftTableName(pol.Name)
	var rs strings.Builder
	line := func(indent int, format string, args ...any) {
		rs.WriteString(strings.Repeat("\t", indent))
		fmt.Fprintf(&rs, format, args...)
		rs.WriteString("\n")
	}
	for _, family := range []string{"inet", "bridge"} {
		// Declared first so the delete never fails on a first run.
		line(0, "add table %s %s", family, table)
		line(0, "delete table %s %s", family, table)
	}

	line(0, "table inet %s {", table)
	line(1, "chain forward {")
	line(2, "type filter hook forward priority filter - 10; policy accept;")
	for _, s := range r.subnets {
		line(2, "%s saddr %s jump jobs", nftAddr(s.Addr()), s)
	}
	for _, s := range r.subnets {
		line(2, "%s daddr %s jump jobs_inbound", nftAddr(s.Addr()), s)
	}
	line(1, "}")
	line(1, "chain input {")
	line(2, "type filter hook input priority filter - 10; policy accept;")
	for _, s := range r.subnets {
		line(2, "%s saddr %s jump jobs_in", nftAddr(s.Addr()), s)
	}
	line(1, "}")
	line(1, "chain jobs {")
	line(2, "ct state established,related accept")
	for _, ap := range r.allow {
		line(2, "meta l4proto tcp ct original %s daddr %s ct original proto-dst %d accept", nftAddr(ap.Addr()), ap.Addr(), ap.Port())
	}
	line(2, "ct status dnat reject")
	if podman {
		line(2, `oifname "podman*" reject`)
	}
	for _, p := range r.deny {
		line(2, "%s daddr %s reject", nftAddr(p.Addr()), p)
	}
	line(1, "}")
	line(1, "chain jobs_inbound {")
	line(2, "ct state established,related accept")
	line(2, "reject")
	line(1, "}")
	line(1, "chain jobs_in {")
	line(2, "ct state established,related accept")
	if slices.ContainsFunc(r.subnets, func(p netip.Prefix) bool { return p.Addr().Is6() }) {
		// IPv6 needs neighbour discovery with the gateway; it opens no port.
		line(2, "icmpv6 type { nd-neighbor-solicit, nd-neighbor-advert } accept")
	}
	for _, ap := range r.allow {
		line(2, "%s daddr %s tcp dport %d accept", nftAddr(ap.Addr()), ap.Addr(), ap.Port())
	}
	if podman {
		for _, g := range r.gateways {
			line(2, "%s daddr %s udp dport 53 accept", nftAddr(g), g)
			line(2, "%s daddr %s tcp dport 53 accept", nftAddr(g), g)
		}
	}
	line(2, "reject")
	line(1, "}")
	line(0, "}")

	line(0, "table bridge %s {", table)
	line(1, "chain forward {")
	line(2, "type filter hook forward priority filter - 10; policy accept;")
	for _, s := range r.subnets {
		line(2, "%[1]s saddr %[2]s %[1]s daddr %[2]s jump jobs", nftAddr(s.Addr()), s)
	}
	line(1, "}")
	line(1, "chain jobs {")
	for _, ap := range r.allow {
		line(2, "%s daddr %s tcp dport %d accept", nftAddr(ap.Addr()), ap.Addr(), ap.Port())
		line(2, "%s saddr %s tcp sport %d accept", nftAddr(ap.Addr()), ap.Addr(), ap.Port())
	}
	line(2, "drop")
	line(1, "}")
	line(0, "}")

	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString("command -v nft >/dev/null 2>&1 || { echo 'the firewall image has no nft, which this engine needs (RUNNER_FIREWALL_IMAGE)' >&2; exit 1; }\n")
	b.WriteString(staleRulesFunctions)
	// We keep no iptables chain on this backend: a jump for the network's subnet is another
	// network's, or this one's from before the engine changed backend. Our own table is replaced
	// below.
	for _, s := range r.subnets {
		tool := "iptables"
		if s.Addr().Is6() {
			tool = "ip6tables"
		}
		fmt.Fprintf(&b, "sw_drop_stale_jumps %s DOCKER-USER %s ''\n", tool, s)
		fmt.Fprintf(&b, "sw_drop_stale_jumps %s INPUT %s ''\n", tool, s)
		fmt.Fprintf(&b, "sw_drop_stale_tables %s %s\n", s, table)
	}
	b.WriteString("nft -f - <<'SW_JOBS_RULESET'\n")
	b.WriteString(rs.String())
	b.WriteString("SW_JOBS_RULESET\n")
	return b.String(), nil
}

// labelRole marks the driver's one-shot helper containers (a firewall writer), which are not jobs.
const labelRole = "sh.stackweaver.role"

// roleFirewall is the firewall writer's role.
const roleFirewall = "firewall"

// staleOneShotAge is how long a helper container may sit unstarted or ended before the next one
// removes it: well past a create-to-remove cycle, so another launcher's writer is never touched.
const staleOneShotAge = 10 * time.Minute

// removeStaleOneShots removes helper containers carrying every label in selector that are not
// running and were created more than olderThan ago: what a launcher that stopped between creating
// and removing one left behind. Failures are ignored; the next writer tries again.
func removeStaleOneShots(ctx context.Context, e *engine, selector map[string]string, olderThan time.Duration) {
	found, err := e.list(ctx, selector)
	if err != nil {
		return
	}
	for _, c := range found {
		if c.State == "running" || c.State == "restarting" || time.Since(time.Unix(c.Created, 0)) < olderThan {
			continue
		}
		_ = e.remove(ctx, c.ID)
	}
}

// staleRulesFunctions are the shell functions with which a jobs network's rules replace what
// another jobs network left on the same subnet. Rules are keyed on the network's subnet, the engine
// hands a freed subnet out again, and a launcher that stopped without removing its rules leaves
// them on the host once its network is gone. Left there, they judge the next network on that
// subnet: an accept in one nftables table does not undo a reject in another, so a stale chain or
// table refuses what the new network's own rules allow. A subnet belongs to one live network on an
// engine, so whatever another jobs network holds on it belongs to one that is gone.
//
//   - sw_drop_stale_jumps TOOL FROM SUBNET KEEP removes FROM's jumps from SUBNET to a jobs chain other
//     than KEEP (an empty KEEP keeps none), and each such chain once nothing jumps to it. It
//     touches no jump for another subnet and no rule that is not a jump to a jobs chain.
//   - sw_drop_stale_tables SUBNET KEEP removes, from every jobs table but KEEP, the rules that send
//     SUBNET's traffic to its chains, and the table once none sends any.
//
// Both are idempotent, and a host without the other firewall's tool or chains has nothing to remove.
// They run before the network's own rules are written. `iptables` and `ip6tables` are the host's
// variant (iptablesVariantPrelude).
const staleRulesFunctions = `sw_drop_unused_chain() {
  "$1" -S 2>/dev/null | while IFS= read -r sw_rule; do case "$sw_rule" in (*" -j $2") exit 1 ;; esac; done || return 0
  "$1" -F "$2" 2>/dev/null || true
  "$1" -X "$2" 2>/dev/null || true
}
sw_drop_stale_jumps() {
  sw_rules=$("$1" -S "$2" 2>/dev/null) || return 0
  for sw_target in $(printf '%s\n' "$sw_rules" | while IFS= read -r sw_rule; do
    case "$sw_rule" in ("-A $2 -s $3 -j SW-JOBS-"*) ;; (*) continue ;; esac
    sw_target=${sw_rule#"-A $2 -s $3 -j "}
    case "$sw_target" in (*" "*|"$4") ;; (*) echo "$sw_target" ;; esac
  done); do
    "$1" -D "$2" -s "$3" -j "$sw_target"
    sw_drop_unused_chain "$1" "$sw_target"
  done
}
sw_drop_stale_rules() {
  for sw_handle in $(nft -a list chain "$1" "$2" "$3" 2>/dev/null | while IFS= read -r sw_rule; do
    case "$sw_rule" in (*"saddr $4 "*"jump "*"# handle "*|*"daddr $4 jump jobs_inbound # handle "*) echo "${sw_rule##*# handle }" ;; esac
  done); do
    nft delete rule "$1" "$2" "$3" handle "$sw_handle"
  done
}
sw_drop_stale_tables() {
  command -v nft >/dev/null 2>&1 || return 0
  for sw_table in $(nft list tables inet 2>/dev/null | while read -r _ _ sw_name; do
    case "$sw_name" in (sw_jobs_*) [ "$sw_name" = "$2" ] || echo "$sw_name" ;; esac
  done); do
    sw_drop_stale_rules inet "$sw_table" forward "$1"
    sw_drop_stale_rules inet "$sw_table" input "$1"
    sw_drop_stale_rules bridge "$sw_table" forward "$1"
    case "$(nft list chain inet "$sw_table" forward 2>/dev/null; nft list chain inet "$sw_table" input 2>/dev/null)" in
      (*" jump "*) ;;
      (*) nft delete table inet "$sw_table" 2>/dev/null || true; nft delete table bridge "$sw_table" 2>/dev/null || true ;;
    esac
  done
}
`

// iptablesVariantPrelude makes a firewall script's `iptables` and `ip6tables` the variant whose
// tables hold Docker's DOCKER-USER chain. Docker writes its chains with the host's variant
// (iptables-nft on most current distributions, iptables-legacy on some), and the writer's image
// carries its own default, which need not match: the launcher image's `iptables` is the legacy
// variant, the OpenTofu job image's the nftables one. With neither holding the chain the plain
// names stay, as before.
const iptablesVariantPrelude = `sw_iptables_variant() {
  for v in "$1-nft" "$1-legacy"; do
    if command -v "$v" >/dev/null 2>&1 && "$v" -S DOCKER-USER >/dev/null 2>&1; then echo "$v"; return; fi
  done
  echo "$1"
}
SW_IPTABLES=$(sw_iptables_variant iptables)
SW_IP6TABLES=$(sw_iptables_variant ip6tables)
iptables() { command "$SW_IPTABLES" "$@"; }
ip6tables() { command "$SW_IP6TABLES" "$@"; }
`

// runFirewall runs script in a one-shot container on the host network with NET_ADMIN: the only
// privilege the driver uses, for rules, never for a job.
func (d *Container) runFirewall(ctx context.Context, image, script string) error {
	if image == "" {
		// Unset, the writer runs in the OpenTofu job image, which carries nft and iptables (OD15).
		image = d.JobImages["tofu"]
	}
	// Pulled only when missing, whatever the job image policy says: in a shipped configuration it
	// is the launcher's own image, whose tag must not move under the running launcher (OD28).
	if _, err := d.ensureImage(ctx, image, PullMissing, d.Auth.For(image), false); err != nil {
		return fmt.Errorf("preparing the firewall writer: %w", err)
	}
	e := d.engine()
	// A writer whose launcher stopped before removing it stays behind as a created container.
	removeStaleOneShots(ctx, e, map[string]string{LabelManaged: "true", labelRole: roleFirewall}, staleOneShotAge)
	id, err := e.create(ctx, "", containerConfig{
		Image:      image,
		Entrypoint: []string{"/bin/sh", "-c"},
		Cmd:        []string{iptablesVariantPrelude + script},
		User:       "0",
		Labels:     map[string]string{LabelManaged: "true", labelRole: roleFirewall},
		HostConfig: hostConfigFirewall(),
	})
	if err != nil {
		return fmt.Errorf("creating the firewall writer: %w", err)
	}
	defer func() { _ = e.remove(context.WithoutCancel(ctx), id) }()
	if err := e.start(ctx, id); err != nil {
		return fmt.Errorf("starting the firewall writer: %w", err)
	}
	code, err := e.wait(ctx, id)
	if err != nil {
		return fmt.Errorf("waiting for the firewall writer: %w", err)
	}
	if code != 0 {
		var out strings.Builder
		if rc, lerr := e.logs(ctx, id); lerr == nil {
			buf := make([]byte, 4096)
			n, _ := rc.Read(buf)
			out.Write(buf[:n])
			_ = rc.Close()
		}
		return fmt.Errorf("programming the jobs network's firewall failed (exit %d): %s", code, strings.TrimSpace(out.String()))
	}
	return nil
}
