// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"os"
	"strings"
	"testing"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// The mirror block becomes the job's environment (#933, D2, AC76): proxies in both spellings, the
// pip index, Galaxy server definitions, and a CA bundle that adds the deployment's roots to the
// system's instead of replacing them. A Galaxy server whose name cannot be part of a variable name
// is left out rather than mangled.
func TestMirrorEnv(t *testing.T) {
	if env, err := MirrorEnv(nil, t.TempDir()); err != nil || len(env) != 0 {
		t.Fatalf("MirrorEnv(nil) = %v, %v; want nothing", env, err)
	}

	const extra = "-----BEGIN CERTIFICATE-----\nplaceholder\n-----END CERTIFICATE-----"
	m := &runnercontract.Mirror{
		HTTPProxy:      "http://proxy:3128",
		PipIndexURL:    "https://pypi.internal/simple",
		GalaxyServers:  []runnercontract.GalaxyServer{{Name: "hub", URL: "https://hub/", Token: "tok"}, {Name: "bad-name", URL: "https://x/"}},
		CACertificates: extra,
	}
	env, err := MirrorEnv(m, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"HTTP_PROXY": "http://proxy:3128", "http_proxy": "http://proxy:3128",
		"PIP_INDEX_URL":                   "https://pypi.internal/simple",
		"ANSIBLE_GALAXY_SERVER_HUB_URL":   "https://hub/",
		"ANSIBLE_GALAXY_SERVER_HUB_TOKEN": "tok",
	} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}
	for name := range env {
		if strings.Contains(name, "BAD") {
			t.Errorf("an invalid Galaxy server name reached the environment as %s", name)
		}
	}
	if got := GalaxyServerList(m); got != "hub" {
		t.Errorf("GalaxyServerList = %q, want hub", got)
	}

	bundle, err := os.ReadFile(env["SSL_CERT_FILE"]) //nolint:gosec // G304: the bundle MirrorEnv just wrote
	if err != nil {
		t.Fatalf("reading the CA bundle: %v", err)
	}
	if !strings.HasSuffix(strings.TrimSpace(string(bundle)), extra) {
		t.Error("the CA bundle does not end with the deployment's roots")
	}
	for _, path := range systemCABundles {
		if system, err := os.ReadFile(path); err == nil { //nolint:gosec // G304: the fixed list of system bundles
			if !strings.HasPrefix(string(bundle), string(system)) {
				t.Errorf("the CA bundle does not start with the system roots of %s", path)
			}
			break
		}
	}
	for _, name := range []string{"REQUESTS_CA_BUNDLE", "GIT_SSL_CAINFO", "PIP_CERT", "CURL_CA_BUNDLE"} {
		if env[name] != env["SSL_CERT_FILE"] {
			t.Errorf("%s = %q, want the bundle", name, env[name])
		}
	}
}
