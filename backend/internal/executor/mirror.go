// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/michielvha/logger"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// systemCABundles are where Linux distributions keep the system roots as one PEM file, in the order
// Go's crypto/x509 looks for them. A job's CA bundle starts from the first that exists, because
// SSL_CERT_FILE replaces the system roots rather than adding to them.
var systemCABundles = []string{
	"/etc/ssl/certs/ca-certificates.crt", // Debian, Ubuntu, Alpine
	"/etc/pki/tls/certs/ca-bundle.crt",   // Fedora, RHEL, CentOS Stream
	"/etc/ssl/ca-bundle.pem",             // openSUSE
	"/etc/pki/tls/cacert.pem",            // OpenELEC
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem",
	"/etc/ssl/cert.pem", // Alpine, macOS
}

// galaxyServerName is what a Galaxy server name may be: it becomes part of an environment variable
// name (ANSIBLE_GALAXY_SERVER_<NAME>_URL).
var galaxyServerName = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// MirrorEnv is the environment a job's tools run with for the deployment's mirror block (#933, D2):
// the proxy variables, pip's index, the Galaxy servers' definitions and a CA bundle of the system
// roots plus the deployment's extra roots, written into dir. A nil or empty block yields nothing.
// The Galaxy server list itself is not set here, because a job's own ansible.cfg may name one;
// GalaxyServerList returns it for the Ansible executor to apply.
func MirrorEnv(m *runnercontract.Mirror, dir string) (map[string]string, error) {
	env := map[string]string{}
	if m == nil {
		return env, nil
	}
	set := func(value string, names ...string) {
		if value == "" {
			return
		}
		for _, name := range names {
			env[name] = value
		}
	}
	// Both spellings: curl, git and Python read the lower-case ones, Go and others the upper-case.
	set(m.HTTPProxy, "HTTP_PROXY", "http_proxy")
	set(m.HTTPSProxy, "HTTPS_PROXY", "https_proxy")
	set(m.NoProxy, "NO_PROXY", "no_proxy")
	set(m.PipIndexURL, "PIP_INDEX_URL")
	set(m.PipTrustedHost, "PIP_TRUSTED_HOST")
	for _, s := range m.GalaxyServers {
		if !galaxyServerName.MatchString(s.Name) || s.URL == "" {
			logger.Warnf("Skipping the Galaxy server %q: a name must be letters, digits and underscores, and a URL is required", s.Name)
			continue
		}
		prefix := "ANSIBLE_GALAXY_SERVER_" + strings.ToUpper(s.Name)
		env[prefix+"_URL"] = s.URL
		set(s.Token, prefix+"_TOKEN")
	}
	if strings.TrimSpace(m.CACertificates) != "" {
		bundle, err := writeCABundle(m.CACertificates, dir)
		if err != nil {
			return nil, err
		}
		// OpenSSL and Go read SSL_CERT_FILE, Python's requests REQUESTS_CA_BUNDLE, git GIT_SSL_CAINFO,
		// pip PIP_CERT and curl CURL_CA_BUNDLE.
		set(bundle, "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "GIT_SSL_CAINFO", "PIP_CERT", "CURL_CA_BUNDLE")
	}
	return env, nil
}

// GalaxyServerList is the ANSIBLE_GALAXY_SERVER_LIST value for the block's Galaxy servers, in
// order, or "" when it names none.
func GalaxyServerList(m *runnercontract.Mirror) string {
	if m == nil {
		return ""
	}
	names := make([]string, 0, len(m.GalaxyServers))
	for _, s := range m.GalaxyServers {
		if galaxyServerName.MatchString(s.Name) && s.URL != "" {
			names = append(names, s.Name)
		}
	}
	return strings.Join(names, ",")
}

// writeCABundle writes the system roots followed by extra into dir and returns the file's path.
func writeCABundle(extra, dir string) (string, error) {
	var bundle []byte
	for _, path := range systemCABundles {
		if data, err := os.ReadFile(path); err == nil { //nolint:gosec // G304: a fixed list of system paths
			bundle = append(bundle, data...)
			bundle = append(bundle, '\n')
			break
		}
	}
	bundle = append(bundle, []byte(strings.TrimSpace(extra)+"\n")...)
	path := filepath.Join(dir, "stackweaver-ca-bundle.pem")
	if err := os.WriteFile(path, bundle, 0o600); err != nil { //nolint:gosec // G703: path is the job's work directory plus a constant name
		return "", fmt.Errorf("writing the job's CA bundle: %w", err)
	}
	return path, nil
}
