// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// `<runner> healthcheck` is the container healthcheck: it reads the launcher's /readyz on its
// health address, so no image needs wget or curl (#933, D3, AC75).
func TestHealthcheck(t *testing.T) {
	ready := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" || !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	env := func(k string) string {
		if k == EnvHealthAddr {
			return ":" + port
		}
		return ""
	}
	if code := Healthcheck(t.Context(), env); code != 0 {
		t.Errorf("a ready launcher's healthcheck = %d, want 0", code)
	}
	ready = false
	if code := Healthcheck(t.Context(), env); code != 1 {
		t.Errorf("an unready launcher's healthcheck = %d, want 1", code)
	}
	if code := Healthcheck(t.Context(), func(k string) string {
		if k == EnvHealthAddr {
			return "off"
		}
		return ""
	}); code != 1 {
		t.Errorf("a healthcheck with the health listener off = %d, want 1", code)
	}
}
