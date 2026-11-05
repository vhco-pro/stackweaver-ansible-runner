// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// The container driver pulls a run image from a private registry with its credential (#933, D6,
// AC27): a registry requiring a login refuses the pull without one, and the job runs with one.
//
//	go test -tags integration ./internal/launcher/driver/ -run TestContainerPull

//go:build integration

package driver

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// envTestRegistryAddr names the address the private registry listens on, instead of a free port
// the test picks: the Podman gate sets it on a rootful run, where the engine reads registry trust
// only from /etc/containers/certs.d, and makes that address's directory there writable to the test
// with sudo for the run's length (scripts/test-e2e-podman.sh).
const envTestRegistryAddr = "STACKWEAVER_TEST_REGISTRY_ADDR"

// privateRegistry runs registry:2 behind htpasswd and TLS on a free host port (or the one
// envTestRegistryAddr names) and returns its address. The engine is made to trust the registry's
// certificate: Docker treats 127.0.0.0/8 as an insecure registry and skips the check, and Podman,
// which does not, reads it from its certs.d.
func privateRegistry(t *testing.T, e *engine, user, password string) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "htpasswd"), []byte(user+":"+string(hash)+"\n"), 0o644); err != nil { //nolint:gosec // G306: read by the registry container
		t.Fatal(err)
	}
	certPEM, keyPEM := registryCertificate(t)
	if err := os.WriteFile(filepath.Join(dir, "cert.pem"), certPEM, 0o644); err != nil { //nolint:gosec // G306: read by the registry container
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), keyPEM, 0o644); err != nil { //nolint:gosec // G306: a throwaway key the registry container reads
		t.Fatal(err)
	}
	_ = os.Chmod(dir, 0o755) //nolint:gosec // G302: read by the registry container
	addr := os.Getenv(envTestRegistryAddr)
	if addr == "" {
		ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr = ln.Addr().String()
		_ = ln.Close()
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("%s=%q is not a 127.0.0.1 address: %v", envTestRegistryAddr, addr, err)
	}
	trustRegistry(t, e, addr, certPEM)
	if !e.imageExists(t.Context(), "registry:2") {
		if err := e.pull(t.Context(), "registry:2", nil); err != nil {
			t.Skipf("registry:2 is not available: %v", err)
		}
	}
	cfg := map[string]any{
		"Image": "registry:2",
		"Env": []string{
			"REGISTRY_AUTH=htpasswd", "REGISTRY_AUTH_HTPASSWD_REALM=test", "REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd",
			"REGISTRY_HTTP_TLS_CERTIFICATE=/auth/cert.pem", "REGISTRY_HTTP_TLS_KEY=/auth/key.pem",
		},
		"Labels": withTestRun(map[string]string{LabelManaged: "true"}),
		"HostConfig": map[string]any{
			// The shared container label, or an engine that confines containers with SELinux
			// (rootless Podman on Fedora) keeps the registry from reading the files.
			"Binds":        []string{dir + ":/auth:ro,z"},
			"PortBindings": map[string]any{"5000/tcp": []map[string]string{{"HostIp": "127.0.0.1", "HostPort": port}}},
		},
		"ExposedPorts": map[string]any{"5000/tcp": map[string]any{}},
	}
	var out struct {
		ID string `json:"Id"`
	}
	if err := e.do(t.Context(), http.MethodPost, "/containers/create", url.Values{"name": {"sw-test-registry-" + uuid.NewString()[:8]}}, cfg, &out); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.remove(context.Background(), out.ID) })
	if err := e.start(t.Context(), out.ID); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	deadline := time.Now().Add(20 * time.Second)
	for {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+addr+"/v2/", nil) //nolint:gosec // G704: the test's own local registry
		resp, err := client.Do(req)                                                                    //nolint:gosec // G704: the test's own local registry
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the registry did not start: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return addr
}

// registryCertificate is a self-signed certificate for 127.0.0.1 and its key, in PEM.
func registryCertificate(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "stackweaver test registry"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// trustRegistry makes a Podman engine trust the registry at addr: its certs.d, per user when the
// engine is rootless, holds the certificate for the test's length. Docker needs nothing. A rootful
// engine reads /etc/containers/certs.d, which the test can write to only where the gate made the
// registry's own directory there writable to it (envTestRegistryAddr).
func trustRegistry(t *testing.T, e *engine, addr string, certPEM []byte) {
	t.Helper()
	if !isPodman(t, e) {
		return
	}
	info, err := e.info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	certsD := "/etc/containers/certs.d"
	if slices.ContainsFunc(info.SecurityOptions, func(o string) bool { return strings.Contains(o, "rootless") }) {
		config, err := os.UserConfigDir()
		if err != nil {
			t.Fatal(err)
		}
		certsD = filepath.Join(config, "containers", "certs.d")
	}
	hostDir := filepath.Join(certsD, addr)
	// The directories this creates are removed again, deepest first, and only while empty.
	var made []string
	for dir := hostDir; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(dir); !os.IsNotExist(err) || dir == filepath.Dir(dir) { //nolint:gosec // G703: the certs.d path of the test's own 127.0.0.1 registry
			break
		}
		made = append(made, dir)
	}
	if err := os.MkdirAll(hostDir, 0o755); err != nil { //nolint:gosec // G301: Podman reads it
		t.Fatalf("trusting the test registry in %s: %v (on rootful Podman run scripts/test-e2e-podman.sh with PODMAN_ROOTFUL=1, which prepares %s for it)", certsD, err, envTestRegistryAddr)
	}
	t.Cleanup(func() {
		_ = os.Remove(filepath.Join(hostDir, "ca.crt")) //nolint:gosec // G703: the certificate this test wrote
		for _, dir := range made {
			_ = os.Remove(dir) //nolint:gosec // G703: a directory this test created
		}
	})
	if err := os.WriteFile(filepath.Join(hostDir, "ca.crt"), certPEM, 0o644); err != nil { //nolint:gosec // G306: a public certificate
		t.Fatal(err)
	}
}

// pushTo tags image into the registry and pushes it with auth.
func pushTo(t *testing.T, e *engine, image, repo string, auth *RegistryAuth) {
	t.Helper()
	name, tag := splitImageRef(repo)
	if err := e.do(t.Context(), http.MethodPost, "/images/"+image+"/tag", url.Values{"repo": {name}, "tag": {tag}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = e.do(context.Background(), http.MethodDelete, "/images/"+repo, url.Values{"force": {"1"}}, nil, nil)
	})
	header, err := json.Marshal(map[string]string{"username": auth.Username, "password": auth.Password, "serveraddress": auth.Registry})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://engine/"+engineAPIVersion+"/images/"+name+"/push?tag="+tag, nil)
	req.Header.Set("X-Registry-Auth", base64.URLEncoding.EncodeToString(header))
	resp, err := e.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := streamError(resp.Body); err != nil {
		t.Fatalf("pushing %s: %v", repo, err)
	}
}

func TestContainerPullsAPrivateRunImageWithItsCredential(t *testing.T) {
	sfx := uuid.NewString()[:8]
	d := newTestDriver(t, "sw-probe-pull-"+sfx+":1")
	e := d.engine()
	buildProbeImage(t, e, d.PlatformImage, "1001:1001")
	addr := privateRegistry(t, e, "robot", "pull-s3cret")
	auth := &RegistryAuth{Registry: addr, Username: "robot", Password: "pull-s3cret"}
	repo := addr + "/acme/custom-" + sfx + ":1"
	pushTo(t, e, d.PlatformImage, repo, auth)
	// Only the registry has it now.
	if err := e.do(t.Context(), http.MethodDelete, "/images/"+repo, url.Values{"force": {"1"}}, nil, nil); err != nil {
		t.Fatal(err)
	}

	// The image is the job's whole image here, executor included, so no injection is involved.
	d.PlatformImage = repo
	req := Request{JobID: "pull-" + sfx, Attempt: 1, Kind: "tofu", Image: repo, Labels: withTestRun(map[string]string{LabelJob: "pull-" + sfx})}
	p, err := d.Place(t.Context(), req)
	if err == nil {
		t.Cleanup(func() { _ = e.remove(context.Background(), p.ID) })
	}
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unauthorized") && !strings.Contains(strings.ToLower(err.Error()), "no basic auth") && !strings.Contains(strings.ToLower(err.Error()), "denied") {
		t.Fatalf("a pull without the credential = %v, want refused", err)
	}
	req.PullAuth = auth
	res, _, exit := runProbe(t, d, req)
	if exit.Code != 0 || res.UID == 0 {
		t.Fatalf("the job in the private image = exit %+v uid %d", exit, res.UID)
	}
}
