// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"slices"
	"strings"
	"testing"
)

// A job's events state which of its pool's limits the driver enforced and which it could not
// (#933, D4, AC24): the report names each, and a job that asked for none gets no report.
func TestLimitsReport(t *testing.T) {
	both := Resources{CPULimit: "1", MemoryLimit: "1Gi"}
	for _, tc := range []struct {
		name    string
		r       Resources
		applied []string
		want    []string
		absent  []string
	}{
		{"none requested", Resources{}, nil, nil, nil},
		{"all enforced", both, []string{"cpu=1", "memory=1Gi"}, []string{"enforced: cpu=1, memory=1Gi"}, []string{"not enforced"}},
		{"none enforced", both, nil, []string{"not enforced", "cpu=1, memory=1Gi"}, []string{"limits enforced:"}},
		{"memory only", both, []string{"memory=1Gi"}, []string{"enforced: memory=1Gi", "not enforced", "cpu=1"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := LimitsReport(tc.r, tc.applied)
			if tc.want == nil && got != "" {
				t.Fatalf("report = %q, want none", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("report %q lacks %q", got, w)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(got, a) {
					t.Errorf("report %q has %q", got, a)
				}
			}
		})
	}
}

// The container driver's job environment carries the limits report, and the network guard unless
// the launcher's network mode is host-identity, where the node's identity is the point (#933, D5,
// AC22).
func TestContainerJobEnv(t *testing.T) {
	req := Request{Env: []string{"A=1"}, Resources: Resources{CPULimit: "1"}}
	env := (&Container{}).jobEnv(req, []string{"cpu=1"}, nil)
	if !slices.Contains(env, "STACKWEAVER_NETWORK_GUARD=1") {
		t.Errorf("platform network: env %v lacks the network guard", env)
	}
	if !slices.ContainsFunc(env, func(kv string) bool {
		return strings.HasPrefix(kv, EnvLimitsReport+"=") && strings.Contains(kv, "cpu=1")
	}) {
		t.Errorf("env %v lacks the limits report", env)
	}
	if !slices.Contains(env, "A=1") || !slices.Contains(env, "STACKWEAVER_PLACED=1") {
		t.Errorf("env %v lost the request's or the placement's variables", env)
	}
	hostEnv := (&Container{HostIdentity: true}).jobEnv(Request{}, nil, nil)
	if slices.Contains(hostEnv, "STACKWEAVER_NETWORK_GUARD=1") {
		t.Errorf("host-identity: env %v still carries the network guard", hostEnv)
	}
	if slices.ContainsFunc(hostEnv, func(kv string) bool { return strings.HasPrefix(kv, EnvLimitsReport+"=") }) {
		t.Errorf("a job with no limits got a report: %v", hostEnv)
	}
}

// On a host-identity launcher the jobs network leaves the metadata ranges open; every other denial
// stays (#933, D5, AC22).
func TestFirewallScript_HostIdentityLeavesMetadataOpen(t *testing.T) {
	pol := NetworkPolicy{Name: "jobs", Deny: []string{"10.9.0.0/16"}}
	platform, err := FirewallScript(pol, []string{"172.30.0.0/16"}, []string{"172.30.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(platform, "169.254.0.0/16") {
		t.Fatalf("the platform network does not deny the metadata range:\n%s", platform)
	}
	pol.AllowMetadata = true
	host, err := FirewallScript(pol, []string{"172.30.0.0/16"}, []string{"172.30.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, cidr := range MetadataCIDRs {
		if strings.Contains(host, cidr) {
			t.Errorf("host-identity still denies %s:\n%s", cidr, host)
		}
	}
	for _, kept := range []string{"10.9.0.0/16", "172.30.0.0/16", "172.30.0.1/32"} {
		if !strings.Contains(host, kept) {
			t.Errorf("host-identity dropped the denial of %s", kept)
		}
	}
}
