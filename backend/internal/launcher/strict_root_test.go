// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"strings"
	"testing"

	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
)

// Under hosted.strict a launcher refuses RUNNER_ALLOW_ROOT before it builds its driver: a hosted
// job never runs as root (#933, D4, D9).
func TestStrictRefusesAllowRoot(t *testing.T) {
	env := map[string]string{EnvDriver: "container", EnvAllowRoot: "true"}
	getenv := func(k string) string { return env[k] }
	_, err := DriverFromEnv(t.Context(), bothExecutors(), Config{Engines: []string{"tofu", "ansible"}, JobImages: map[string]string{"tofu": "platform:1", "ansible": "platform:1"}, Strict: true}, getenv)
	if err == nil || !strings.Contains(err.Error(), "hosted.strict") {
		t.Errorf("DriverFromEnv = %v, want RUNNER_ALLOW_ROOT refused under hosted.strict", err)
	}
}

// A container launcher in the host-identity network mode places jobs that may reach the node's
// identity; in the default platform mode they may not (#933, D5, AC22).
func TestContainerDriverFollowsTheNetworkMode(t *testing.T) {
	getenv := func(k string) string { return map[string]string{EnvDriver: "container"}[k] }
	for mode, want := range map[string]bool{NetworkPlatform: false, NetworkHostIdentity: true} {
		d, err := DriverFromEnv(t.Context(), bothExecutors(), Config{Engines: []string{"tofu", "ansible"}, JobImages: map[string]string{"tofu": "platform:1", "ansible": "platform:1"}, NetworkMode: mode}, getenv)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		c, ok := d.(*driver.Container)
		if !ok {
			t.Fatalf("%s: driver %T, want the container driver", mode, d)
		}
		if c.HostIdentity != want {
			t.Errorf("%s: HostIdentity = %v, want %v", mode, c.HostIdentity, want)
		}
	}
}
