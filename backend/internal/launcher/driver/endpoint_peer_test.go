// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import "testing"

// The API and each RUNNER_ALLOW_TCP endpoint become one network policy peer on its port, IPv4 and
// IPv6 alike (#933, D5, AC76).
func TestEndpointPeer(t *testing.T) {
	for in, want := range map[string]string{"10.96.0.10:8080": "10.96.0.10/32", "[fd00::5]:443": "fd00::5/128"} {
		p, ok := endpointPeer(in)
		if !ok || p.CIDR != want {
			t.Errorf("endpointPeer(%q) = %+v %v, want %s", in, p, ok, want)
		}
	}
	for _, bad := range []string{"", "10.96.0.10", "host:port"} {
		if _, ok := endpointPeer(bad); ok {
			t.Errorf("endpointPeer(%q) accepted", bad)
		}
	}
}
