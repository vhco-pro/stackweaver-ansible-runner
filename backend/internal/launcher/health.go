// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Network modes a launcher may be configured with (D5); only the installer sets one.
const (
	NetworkPlatform     = "platform"
	NetworkHostIdentity = "host-identity"
)

// Ready reports whether the launcher can take work (AC75): registered, with a heartbeat answered
// recently, and a driver that can place. The error says what is missing.
func (l *Launcher) Ready(ctx context.Context) error {
	if !l.registered.Load() {
		return errors.New("not registered")
	}
	stale := 2*l.poller.wait() + 30*time.Second
	if last := time.Unix(0, l.lastBeat.Load()); time.Since(last) > stale {
		return fmt.Errorf("no heartbeat answered since %s", last.Format(time.RFC3339))
	}
	if err := l.driver.Ping(ctx); err != nil {
		return fmt.Errorf("the %s driver cannot place: %w", l.driver.Name(), err)
	}
	return nil
}

// HealthHandler serves /healthz (the process is up) and /readyz (Ready).
func (l *Launcher) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := l.Ready(ctx); err != nil {
			http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})
	return mux
}

// serveHealth listens on addr until the returned stop is called.
func (l *Launcher) serveHealth(ctx context.Context, addr string) (func(), error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listening for health checks on %s: %w", addr, err)
	}
	srv := &http.Server{Handler: l.HealthHandler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}, nil
}
