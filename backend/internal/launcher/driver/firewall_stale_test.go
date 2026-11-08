// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The tools stand-ins keep the host's rules in files: iptables in one listing (`iptables -S`), nft in
// one file per chain (`nft -a list chain`), and log every call that changes them.
const (
	stubIPTables = `#!/bin/sh
case "$1" in
-S)
  if [ -n "$2" ]; then grep -E -- "^-[AN] $2( |\$)" "$STUB_DIR/iptables"; else cat "$STUB_DIR/iptables"; fi ;;
-D)
  shift; rule="-A $*"
  grep -qxF -- "$rule" "$STUB_DIR/iptables" || exit 1
  awk -v r="$rule" '!done && $0 == r { done = 1; next } 1' "$STUB_DIR/iptables" >"$STUB_DIR/iptables.new"
  mv "$STUB_DIR/iptables.new" "$STUB_DIR/iptables"
  echo "iptables -D $*" >>"$STUB_DIR/log" ;;
-F|-X)
  echo "iptables $*" >>"$STUB_DIR/log"
  if [ "$1" = -X ]; then grep -vE -- "^-[AN] $2( |\$)" "$STUB_DIR/iptables" >"$STUB_DIR/iptables.new"; mv "$STUB_DIR/iptables.new" "$STUB_DIR/iptables"; fi ;;
*) exit 2 ;;
esac
`
	stubNFT = `#!/bin/sh
[ "$1" = -a ] && shift
case "$1 $2" in
"list tables") for f in "$STUB_DIR"/nft/inet_*_forward; do t=${f#"$STUB_DIR"/nft/inet_}; echo "table inet ${t%_forward}"; done ;;
"list chain") cat "$STUB_DIR/nft/$3_$4_$5" 2>/dev/null || exit 1 ;;
"delete rule")
  f="$STUB_DIR/nft/$3_$4_$5"
  grep -q "# handle $7\$" "$f" || exit 1
  grep -v "# handle $7\$" "$f" >"$f.new"; mv "$f.new" "$f"
  echo "nft $*" >>"$STUB_DIR/log" ;;
"delete table") rm -f "$STUB_DIR/nft/$3_$4_"*; echo "nft $*" >>"$STUB_DIR/log" ;;
*) exit 2 ;;
esac
`
)

// staleStubs writes the stand-ins and the host's rules into a new directory.
func staleStubs(t *testing.T, iptables string, chains map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil { //nolint:gosec // G301: a test directory
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "nft"), 0o755); err != nil { //nolint:gosec // G301: a test directory
		t.Fatal(err)
	}
	for name, body := range map[string]string{"iptables": stubIPTables, "nft": stubNFT} {
		if err := os.WriteFile(filepath.Join(dir, "bin", name), []byte(body), 0o755); err != nil { //nolint:gosec // G306: a test stub that must be executable
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "iptables"), []byte(iptables), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range chains {
		if err := os.WriteFile(filepath.Join(dir, "nft", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// runStale runs script after the stale-rule functions against the stand-ins in dir and returns the
// changes it made, in order.
func runStale(t *testing.T, dir, script string) []string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	_ = os.Remove(filepath.Join(dir, "log"))
	cmd := exec.CommandContext(t.Context(), sh, "-c", "set -e\n"+staleRulesFunctions+script) //nolint:gosec // G204: the test's own script
	cmd.Env = []string{"PATH=" + filepath.Join(dir, "bin") + ":/usr/bin:/bin", "STUB_DIR=" + dir}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script: %v\n%s", err, out)
	}
	log, err := os.ReadFile(filepath.Join(dir, "log")) //nolint:gosec // G304: the test's own temporary directory
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(log)), "\n")
}

// A jobs network's rules remove the jumps another jobs chain holds for the network's subnet, and
// that chain once nothing jumps to it. A jump of its own, a jump for another subnet, a rule that is
// not a plain jump to a jobs chain, and a chain still jumped to for another subnet all stay. A rerun
// changes nothing.
func TestStaleJumpsAreRemovedForTheSubnetOnly(t *testing.T) {
	dir := staleStubs(t, strings.Join([]string{
		"-P INPUT ACCEPT",
		"-N DOCKER-USER",
		"-N SW-JOBS-DEAD",
		"-N SW-JOBS-DEAD-IN",
		"-N SW-JOBS-SHARED",
		"-N SW-JOBS-OURS",
		"-N SW-JOBS-OTHER",
		"-A INPUT -s 10.1.0.0/24 -j SW-JOBS-DEAD-IN",
		"-A INPUT -s 10.2.0.0/24 -j SW-JOBS-SHARED",
		"-A DOCKER-USER -s 10.1.0.0/24 -j SW-JOBS-OURS",
		"-A DOCKER-USER -s 10.1.0.0/24 -j SW-JOBS-DEAD",
		"-A DOCKER-USER -s 10.1.0.0/24 -j SW-JOBS-SHARED",
		"-A DOCKER-USER -s 10.1.0.0/24 -j RETURN",
		"-A DOCKER-USER -s 10.1.0.0/24 -p tcp -j SW-JOBS-OTHER",
		"-A DOCKER-USER -s 10.1.0.0/2 -j SW-JOBS-OTHER",
		"-A DOCKER-USER -s 110.1.0.0/24 -j SW-JOBS-OTHER",
		"-A DOCKER-USER -s 10.2.0.0/24 -j SW-JOBS-SHARED",
		"-A SW-JOBS-DEAD -j REJECT --reject-with icmp-port-unreachable",
		"-A SW-JOBS-DEAD-IN -j REJECT --reject-with icmp-port-unreachable",
		"",
	}, "\n"), nil)
	script := "sw_drop_stale_jumps iptables DOCKER-USER 10.1.0.0/24 SW-JOBS-OURS\n"
	got := runStale(t, dir, script)
	want := []string{
		"iptables -D DOCKER-USER -s 10.1.0.0/24 -j SW-JOBS-DEAD",
		// The INPUT jump to SW-JOBS-DEAD-IN is no reference to SW-JOBS-DEAD.
		"iptables -F SW-JOBS-DEAD",
		"iptables -X SW-JOBS-DEAD",
		// Still jumped to for 10.2.0.0/24: the chain stays.
		"iptables -D DOCKER-USER -s 10.1.0.0/24 -j SW-JOBS-SHARED",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("changes = %q, want %q", got, want)
	}
	if got := runStale(t, dir, script); len(got) != 0 {
		t.Errorf("a rerun changed %q", got)
	}
	// INPUT with no chain kept: what the nftables backend asks, and what an iptables network asks
	// with its own -IN chain.
	if got, want := runStale(t, dir, "sw_drop_stale_jumps iptables INPUT 10.1.0.0/24 ''\n"), []string{
		"iptables -D INPUT -s 10.1.0.0/24 -j SW-JOBS-DEAD-IN", "iptables -F SW-JOBS-DEAD-IN", "iptables -X SW-JOBS-DEAD-IN",
	}; !slices.Equal(got, want) {
		t.Errorf("changes = %q, want %q", got, want)
	}
	// A host without the chain, or without the tool, has nothing to remove.
	if got := runStale(t, dir, "sw_drop_stale_jumps iptables NO-SUCH-CHAIN 10.1.0.0/24 ''\nsw_drop_stale_jumps ip6tables DOCKER-USER fd00:1::/64 ''\n"); len(got) != 0 {
		t.Errorf("changes = %q, want none", got)
	}
}

// The same holds for another jobs network's nftables tables: the rules sending the subnet's
// traffic to its chains go, and the table once none is left; the table being written, a table
// holding another subnet too, and a subnet that only looks alike stay.
func TestStaleTablesAreRemovedForTheSubnetOnly(t *testing.T) {
	forward := func(table string, rules ...string) string {
		return "table inet " + table + " {\n\tchain forward { # handle 1\n\t\ttype filter hook forward priority filter - 10; policy accept;\n\t\t" +
			strings.Join(rules, "\n\t\t") + "\n\t}\n}\n"
	}
	dir := staleStubs(t, "", map[string]string{
		"inet_sw_jobs_dead_forward":    forward("sw_jobs_dead", "ip saddr 10.1.0.0/24 jump jobs # handle 5", "ip daddr 10.1.0.0/24 jump jobs_inbound # handle 6"),
		"inet_sw_jobs_dead_input":      "ip saddr 10.1.0.0/24 jump jobs_in # handle 7\n",
		"bridge_sw_jobs_dead_forward":  "ip saddr 10.1.0.0/24 ip daddr 10.1.0.0/24 jump jobs # handle 3\n",
		"inet_sw_jobs_dual_forward":    forward("sw_jobs_dual", "ip saddr 10.1.0.0/24 jump jobs # handle 5", "ip6 saddr fd00:1::/64 jump jobs # handle 8"),
		"inet_sw_jobs_dual_input":      "ip saddr 10.1.0.0/24 jump jobs_in # handle 7\nip6 saddr fd00:1::/64 jump jobs_in # handle 9\n",
		"inet_sw_jobs_ours_forward":    forward("sw_jobs_ours", "ip saddr 10.1.0.0/24 jump jobs # handle 5"),
		"inet_sw_jobs_similar_forward": forward("sw_jobs_similar", "ip saddr 110.1.0.0/24 jump jobs # handle 5"),
		"inet_firewalld_forward":       forward("firewalld", "ip saddr 10.1.0.0/24 jump filter_FORWARD # handle 5"),
	})
	script := "sw_drop_stale_tables 10.1.0.0/24 sw_jobs_ours\n"
	got := runStale(t, dir, script)
	want := []string{
		"nft delete rule inet sw_jobs_dead forward handle 5",
		"nft delete rule inet sw_jobs_dead forward handle 6",
		"nft delete rule inet sw_jobs_dead input handle 7",
		"nft delete rule bridge sw_jobs_dead forward handle 3",
		"nft delete table inet sw_jobs_dead",
		"nft delete table bridge sw_jobs_dead",
		"nft delete rule inet sw_jobs_dual forward handle 5",
		"nft delete rule inet sw_jobs_dual input handle 7",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("changes = %q, want %q", got, want)
	}
	if got := runStale(t, dir, script); len(got) != 0 {
		t.Errorf("a rerun changed %q", got)
	}
}

// Both backends' scripts replace what another jobs network left on their subnets before they write
// their own rules, in both firewall forms.
func TestFirewallScriptsReplaceStaleRulesFirst(t *testing.T) {
	pol := NetworkPolicy{Name: "jobs"}
	chain, table := chainName("jobs"), nftTableName("jobs")
	ipt, err := FirewallScript(pol, []string{"10.1.0.0/24"}, []string{"10.1.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ drop, add string }{
		{"sw_drop_stale_jumps iptables DOCKER-USER 10.1.0.0/24 " + chain, "-I DOCKER-USER -s 10.1.0.0/24 -j " + chain},
		{"sw_drop_stale_jumps iptables INPUT 10.1.0.0/24 " + chain + "-IN", "-I INPUT -s 10.1.0.0/24 -j " + chain + "-IN"},
		{"sw_drop_stale_tables 10.1.0.0/24 ''", "-I DOCKER-USER -s 10.1.0.0/24 -j " + chain},
	} {
		drop, add := ruleIndex(ipt, c.drop), strings.Index(ipt, c.add)
		if drop < 0 || add < 0 || strings.Index(ipt, c.drop) > add {
			t.Errorf("%q is missing or does not precede %q:\n%s", c.drop, c.add, ipt)
		}
	}
	nft, err := NFTablesScript(pol, []string{"10.1.0.0/24", "fd00:1::/64"}, []string{"10.1.0.1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	apply := strings.Index(nft, "nft -f -")
	for _, drop := range []string{
		"sw_drop_stale_jumps iptables DOCKER-USER 10.1.0.0/24 ''", "sw_drop_stale_jumps iptables INPUT 10.1.0.0/24 ''",
		"sw_drop_stale_jumps ip6tables DOCKER-USER fd00:1::/64 ''", "sw_drop_stale_jumps ip6tables INPUT fd00:1::/64 ''",
		"sw_drop_stale_tables 10.1.0.0/24 " + table, "sw_drop_stale_tables fd00:1::/64 " + table,
	} {
		if at := ruleIndex(nft, drop); at < 0 || strings.Index(nft, drop) > apply {
			t.Errorf("%q is missing or follows the ruleset:\n%s", drop, nft)
		}
	}
}
