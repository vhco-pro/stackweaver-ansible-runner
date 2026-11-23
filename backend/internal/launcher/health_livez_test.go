// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// recordingControlPlane serves the fake runner API and records whether the launcher registered.
func recordingControlPlane(t *testing.T) (*fakeAPI, string, *atomic.Bool) {
	t.Helper()
	api := newFakeAPI(t, runnercontract.CapabilityJobToken, runnercontract.CapabilityClaimRecovery)
	var registered atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/register") {
			registered.Store(true)
		}
		api.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return api, srv.URL, &registered
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func getStatus(t *testing.T, url string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// launcherFromEnv builds a launcher through ConfigFromEnv, as Main does, with the test's driver.
func launcherFromEnv(t *testing.T, serverURL, healthAddr string) *Launcher {
	t.Helper()
	stubToolchains(t, "tofu", "ansible")
	env := map[string]string{
		EnvServer: serverURL, EnvToken: "swa-agent", EnvPoolID: "pool-1", EnvName: "launcher-livez",
		EnvHealthAddr: healthAddr, EnvCacheDir: "off",
	}
	cfg, err := ConfigFromEnv(bothExecutors(), envOf(env))
	if err != nil {
		t.Fatalf("reading the launcher configuration: %v", err)
	}
	cfg.HeartbeatWait, cfg.IdleBeat, cfg.StatusInterval = time.Second, 20*time.Millisecond, 20*time.Millisecond
	cfg.DrainGrace, cfg.KillGrace = 5*time.Second, 100*time.Millisecond
	l := New(cfg, newFakeDriver("container"))
	l.goos = "linux"
	return l
}

// A launcher whose health address is set serves /livez beside /healthz there and registers; with
// RUNNER_HEALTH_ADDR=off it registers and opens no listener.
func TestLauncherLivez(t *testing.T) {
	t.Run("health address set", func(t *testing.T) {
		_, serverURL, registered := recordingControlPlane(t)
		addr := freeAddr(t)
		l := launcherFromEnv(t, serverURL, addr)
		runLauncher(t, l)
		eventually(t, "registration", registered.Load)
		for _, path := range []string{"/livez", "/healthz"} {
			url := "http://" + addr + path
			eventually(t, path+" answering 200", func() bool { return getStatus(t, url) == http.StatusOK })
		}
	})

	t.Run("RUNNER_HEALTH_ADDR=off", func(t *testing.T) {
		_, serverURL, registered := recordingControlPlane(t)
		l := launcherFromEnv(t, serverURL, "off")
		var listened atomic.Bool
		l.onHealthListen = func(net.Addr) { listened.Store(true) }
		runLauncher(t, l)
		eventually(t, "registration", registered.Load)
		if listened.Load() {
			t.Error("a launcher with RUNNER_HEALTH_ADDR=off opened a health listener")
		}
	})
}
