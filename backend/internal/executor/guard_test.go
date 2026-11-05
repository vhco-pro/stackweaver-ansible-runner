// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"net"
	"strings"
	"testing"
)

// With the guard on, an answering metadata endpoint refuses the job; nothing answering lets it run
// (#933, D5, AC21 on a rootless engine).
func TestGuardRefusesAReachableMetadataEndpoint(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	if err := guardAgainst(t.Context(), []string{ln.Addr().String()}); err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Errorf("guard = %v, want the reachable endpoint refused", err)
	}
	closed, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := closed.Addr().String()
	_ = closed.Close()
	if err := guardAgainst(t.Context(), []string{addr}); err != nil {
		t.Errorf("guard = %v, want nothing refused when no endpoint answers", err)
	}
	t.Setenv(EnvNetworkGuard, "")
	if err := GuardNetwork(t.Context()); err != nil {
		t.Errorf("GuardNetwork with the guard off = %v", err)
	}
}
