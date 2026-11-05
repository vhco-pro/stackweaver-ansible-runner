// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Healthcheck is the `healthcheck` subcommand, a container healthcheck that needs nothing in the
// image but the runner binary: it reads /readyz on the launcher's health address (RUNNER_HEALTH_ADDR,
// default :9090) and returns 0 when the launcher is ready, 1 otherwise.
func Healthcheck(ctx context.Context, getenv func(string) string) int {
	addr := firstOf(getenv(EnvHealthAddr), DefaultHealthAddr)
	if strings.EqualFold(addr, "off") {
		fmt.Fprintln(os.Stderr, "the health listener is off (RUNNER_HEALTH_ADDR=off)")
		return 1
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s=%q: %v\n", EnvHealthAddr, addr, err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/readyz", nil)
	if err != nil {
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "/readyz answered %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
