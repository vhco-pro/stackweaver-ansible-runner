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
	"sync"
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
// envTestRegistryAddr names) and returns its address; an empty user runs it without a login, as a
// public registry serves the published job images. The engine is made to trust the registry's
// certificate: Docker treats 127.0.0.0/8 as an insecure registry and skips the check, and Podman,
// which does not, reads it from its certs.d.
func privateRegistry(t *testing.T, e *engine, user, password string) string {
	t.Helper()
	dir := t.TempDir()
	if user != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "htpasswd"), []byte(user+":"+string(hash)+"\n"), 0o644); err != nil { //nolint:gosec // G306: read by the registry container
			t.Fatal(err)
		}
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
		if err := e.pullBounded(t.Context(), "registry:2", nil, 0); err != nil {
			t.Skipf("registry:2 is not available: %v", err)
		}
	}
	env := []string{"REGISTRY_HTTP_TLS_CERTIFICATE=/auth/cert.pem", "REGISTRY_HTTP_TLS_KEY=/auth/key.pem"}
	if user != "" {
		env = append(env, "REGISTRY_AUTH=htpasswd", "REGISTRY_AUTH_HTPASSWD_REALM=test", "REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd")
	}
	cfg := map[string]any{
		"Image":  "registry:2",
		"Env":    env,
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
	registries.Store(addr, out.ID)
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

// registries maps a test registry's address to its container, so a test can stop it.
var registries sync.Map

// stopRegistry stops the test registry at addr: a registry the engine can no longer reach.
func stopRegistry(t *testing.T, e *engine, addr string) {
	t.Helper()
	id, ok := registries.Load(addr)
	if !ok {
		t.Fatalf("no test registry at %s", addr)
	}
	if err := e.stop(t.Context(), id.(string), 0); err != nil {
		t.Fatalf("stopping the registry at %s: %v", addr, err)
	}
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
	buildProbeImage(t, e, d.JobImages["tofu"], "1001:1001")
	addr := privateRegistry(t, e, "robot", "pull-s3cret")
	auth := &RegistryAuth{Registry: addr, Username: "robot", Password: "pull-s3cret"}
	repo := addr + "/acme/custom-" + sfx + ":1"
	pushTo(t, e, d.JobImages["tofu"], repo, auth)
	// Only the registry has it now.
	if err := e.do(t.Context(), http.MethodDelete, "/images/"+repo, url.Values{"force": {"1"}}, nil, nil); err != nil {
		t.Fatal(err)
	}

	// The image is the job's whole image here, executor included, so no injection is involved.
	d.JobImages = sameJobImage(repo)
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

// imageID is the engine's id of the image ref names, "" when it holds none.
func imageID(t *testing.T, e *engine, ref string) string {
	t.Helper()
	var img struct {
		ID string `json:"Id"`
	}
	if err := e.do(t.Context(), http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil, &img); err != nil {
		return ""
	}
	return img.ID
}

// tagLocal points the engine's tag ref at image.
func tagLocal(t *testing.T, e *engine, image, ref string) {
	t.Helper()
	name, tag := splitImageRef(ref)
	if err := e.do(t.Context(), http.MethodPost, "/images/"+image+"/tag", url.Values{"repo": {name}, "tag": {tag}}, nil, nil); err != nil {
		t.Fatal(err)
	}
}

// dropLocal removes ref from the engine, leaving it only in the registry.
func dropLocal(t *testing.T, e *engine, ref string) {
	t.Helper()
	if err := e.do(t.Context(), http.MethodDelete, "/images/"+url.PathEscape(ref), url.Values{"force": {"1"}}, nil, nil); err != nil && !isNotFound(err) {
		t.Fatal(err)
	}
}

// staleJobImage publishes a probe stamped "new" as repo and leaves the engine holding an older
// probe, stamped "old", under the same tag: a host that ran the job image before its tag moved
// (one-launcher spec, A8). It returns the new image's registry digest.
func staleJobImage(t *testing.T, e *engine, sfx, repo string, auth *RegistryAuth) string {
	t.Helper()
	oldLocal, newLocal := "sw-probe-old-"+sfx+":1", "sw-probe-new-"+sfx+":1"
	buildStampedProbeImage(t, e, "alpine:3.22", oldLocal, "1001:1001", "old", map[string]string{LabelManaged: "true"})
	buildStampedProbeImage(t, e, "alpine:3.22", newLocal, "1001:1001", "new", map[string]string{LabelManaged: "true"})
	pushTo(t, e, newLocal, repo, auth)
	digest, err := e.imageDigest(t.Context(), newLocal)
	if err != nil || !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("the pushed image's digest = %q, %v", digest, err)
	}
	tagLocal(t, e, oldLocal, repo)
	return digest
}

// containerEnv is a placed container's environment.
func containerEnv(t *testing.T, e *engine, id string) []string {
	t.Helper()
	var st struct {
		Config struct{ Env []string }
	}
	if err := e.do(t.Context(), http.MethodGet, "/containers/"+id+"/json", nil, nil, &st); err != nil {
		t.Fatal(err)
	}
	return st.Config.Env
}

// RUNNER_JOB_IMAGE_PULL decides when the container driver pulls a job's image (one-launcher spec,
// A3, OD18): always runs a moved tag's current image and records its digest (AC40), missing runs
// what the engine holds and pulls only an absent image, never refuses an absent image naming it
// (AC46), and a driver built without the setting keeps pulling only a missing image.
func TestContainerJobImagePullPolicy(t *testing.T) {
	sfx := uuid.NewString()[:8]
	e := newEngine(testSocket(t))
	addr := privateRegistry(t, e, "", "")
	public := &RegistryAuth{Registry: addr}
	repo := addr + "/vhco-pro/stackweaver-opentofu-runner-" + sfx + ":latest"
	newDigest := staleJobImage(t, e, sfx, repo, public)

	place := func(t *testing.T, pull PullPolicy, job string) (probeResult, Placement, Exit) {
		t.Helper()
		d := &Container{Socket: e.socket, JobImages: sameJobImage(repo), ExecutorPath: "/probe", JobImagePull: pull}
		return runProbe(t, d, Request{JobID: job + "-" + sfx, Attempt: 1, Kind: "tofu", Labels: map[string]string{LabelJob: job + "-" + sfx}})
	}

	t.Run("missing runs the cached image", func(t *testing.T) {
		if res, _, exit := place(t, PullMissing, "missing"); exit.Code != 0 || res.Stamp != "old" {
			t.Errorf("missing ran %q (exit %+v), want the cached old image", res.Stamp, exit)
		}
	})
	t.Run("unset keeps pulling only a missing image", func(t *testing.T) {
		if res, _, exit := place(t, "", "unset"); exit.Code != 0 || res.Stamp != "old" {
			t.Errorf("an unset policy ran %q (exit %+v), want the cached old image", res.Stamp, exit)
		}
	})
	t.Run("always runs the moved tag's current image", func(t *testing.T) {
		res, p, exit := place(t, PullAlways, "always")
		if exit.Code != 0 || res.Stamp != "new" {
			t.Fatalf("always ran %q (exit %+v), want the registry's new image", res.Stamp, exit)
		}
		if env := containerEnv(t, e, p.ID); !slices.Contains(env, "STACKWEAVER_IMAGE_DIGEST="+newDigest) {
			t.Errorf("the placement reports %v, want the new digest %s", env, newDigest)
		}
	})
	t.Run("never refuses an absent image, naming it", func(t *testing.T) {
		dropLocal(t, e, repo)
		d := &Container{Socket: e.socket, JobImages: sameJobImage(repo), ExecutorPath: "/probe", JobImagePull: PullNever}
		p, err := d.Place(t.Context(), Request{JobID: "never-" + sfx, Attempt: 1, Kind: "tofu", Labels: withTestRun(map[string]string{LabelJob: "never-" + sfx})})
		if err == nil {
			_ = e.remove(context.Background(), p.ID)
			t.Fatal("never placed a job whose image is absent")
		}
		if !strings.Contains(err.Error(), repo) {
			t.Errorf("refusal %q does not name %s", err, repo)
		}
	})
	t.Run("missing pulls an absent image", func(t *testing.T) {
		dropLocal(t, e, repo)
		if res, _, exit := place(t, PullMissing, "absent"); exit.Code != 0 || res.Stamp != "new" {
			t.Errorf("missing ran %q (exit %+v) for an absent image, want it pulled", res.Stamp, exit)
		}
	})
}

// The job image a custom run image's executor is copied from follows the pull policy too, so a
// moved tag's current executor reaches run images (one-launcher spec, A3).
func TestContainerInjectionSourceFollowsThePullPolicy(t *testing.T) {
	sfx := uuid.NewString()[:8]
	e := newEngine(testSocket(t))
	addr := privateRegistry(t, e, "", "")
	public := &RegistryAuth{Registry: addr}
	repo := addr + "/vhco-pro/stackweaver-ansible-runner-" + sfx + ":latest"
	staleJobImage(t, e, sfx, repo, public)
	runLocal := "sw-probe-runimage-" + sfx + ":1"
	buildStampedProbeImage(t, e, "scratch", runLocal, "1001:1001", "run-image", map[string]string{LabelManaged: "true"})
	runImage := addr + "/acme/ee-" + sfx + ":1"
	pushTo(t, e, runLocal, runImage, public)

	for _, tc := range []struct {
		pull PullPolicy
		want string
	}{{PullMissing, "old"}, {PullAlways, "new"}} {
		d := &Container{Socket: e.socket, JobImages: sameJobImage(repo), ExecutorPath: "/probe", JobImagePull: tc.pull}
		job := "inj-" + string(tc.pull) + "-" + sfx
		res, p, exit := runProbe(t, d, Request{JobID: job, Attempt: 1, Kind: "ansible", Image: runImage, Labels: map[string]string{LabelJob: job}})
		t.Cleanup(func() { _ = d.removeContainer(context.Background(), p.ID, p.Labels) })
		if exit.Code != 0 || res.Stamp != tc.want {
			t.Errorf("%s: the run image's executor came from %q (exit %+v), want %q", tc.pull, res.Stamp, exit, tc.want)
		}
	}
}

// The firewall writer's image is pulled only when missing, whatever the pull policy says, so the
// launcher's own image tag never moves under it (OD28); with RUNNER_FIREWALL_IMAGE unset on a host
// without the OpenTofu job image, that image is pulled and writes the jobs network's rules (OD15,
// AC41).
func TestContainerFirewallImageIsPulledOnlyWhenMissing(t *testing.T) {
	sfx := uuid.NewString()[:8]
	e := newEngine(testSocket(t))
	addr := privateRegistry(t, e, "", "")
	public := &RegistryAuth{Registry: addr}
	toolsLocal := "sw-test-tools-" + sfx + ":1"
	buildToolsImage(t, e, toolsLocal)
	t.Cleanup(func() {
		_ = e.do(context.Background(), http.MethodDelete, "/images/"+toolsLocal, url.Values{"force": {"1"}}, nil, nil)
	})

	t.Run("a cached firewall image is not pulled under always", func(t *testing.T) {
		repo := addr + "/vhco-pro/stackweaver-launcher-" + sfx + ":latest"
		pushTo(t, e, toolsLocal, repo, public)
		// The engine now holds a different image under the launcher's tag.
		stale := "sw-probe-stale-fw-" + sfx + ":1"
		buildProbeImageFrom(t, e, "alpine:3.22", stale, "", map[string]string{LabelManaged: "true"})
		tagLocal(t, e, stale, repo)
		before := imageID(t, e, repo)
		d := &Container{Socket: e.socket, JobImages: sameJobImage(repo), JobImagePull: PullAlways}
		if err := d.runFirewall(t.Context(), repo, "true"); err != nil {
			t.Fatalf("runFirewall: %v", err)
		}
		if after := imageID(t, e, repo); after != before {
			t.Errorf("the firewall image's tag moved from %s to %s: it was pulled", before, after)
		}
	})
	t.Run("an absent firewall image is pulled", func(t *testing.T) {
		repo := addr + "/vhco-pro/stackweaver-launcher-absent-" + sfx + ":latest"
		pushTo(t, e, toolsLocal, repo, public)
		dropLocal(t, e, repo)
		d := &Container{Socket: e.socket, JobImages: sameJobImage(repo), JobImagePull: PullNever}
		if err := d.runFirewall(t.Context(), repo, "true"); err != nil {
			t.Fatalf("runFirewall with the image absent: %v", err)
		}
		if imageID(t, e, repo) == "" {
			t.Error("the absent firewall image was not pulled")
		}
	})
	t.Run("unset falls back to the OpenTofu job image and writes the rules", func(t *testing.T) {
		repo := addr + "/vhco-pro/stackweaver-opentofu-runner-" + sfx + ":latest"
		pushTo(t, e, toolsLocal, repo, public)
		dropLocal(t, e, repo)
		d := &Container{Socket: e.socket, JobImages: map[string]string{"tofu": repo, "ansible": "absent-ansible-" + sfx + ":1"}, ExecutorPath: "/probe", JobImagePull: PullAlways}
		backend, _, err := detectFirewall(t.Context(), e)
		if err != nil {
			t.Fatal(err)
		}
		jobsNet := jobsNetworkName(sfx)
		t.Cleanup(func() { _ = e.do(context.Background(), http.MethodDelete, "/networks/"+jobsNet, nil, nil, nil) })
		t.Cleanup(func() { removeFirewallChains(context.Background(), d, toolsLocal, jobsNet) })
		programmed, err := d.EnsureNetwork(t.Context(), NetworkPolicy{Name: jobsNet, DenyOtherNetworks: true, firewall: backend})
		if err != nil {
			t.Fatalf("EnsureNetwork with the firewall image unset: %v", err)
		}
		if !programmed {
			t.Skip("a rootless engine writes no rules")
		}
		if imageID(t, e, repo) == "" {
			t.Error("the OpenTofu job image was not pulled for the firewall writer")
		}
	})
}

// placementReport is what a placed job's executor writes into its output before the job starts.
func placementReport(t *testing.T, e *engine, id string) string {
	t.Helper()
	for _, kv := range containerEnv(t, e, id) {
		if v, ok := strings.CutPrefix(kv, EnvLimitsReport+"="); ok {
			return v
		}
	}
	return ""
}

// With the registry unreachable, a job whose engine default job image is cached runs that copy
// with a warning on the job and its digest recorded, one whose image is not cached fails naming
// the image, and a run image never falls back (one-launcher spec, A3, OD21, OD27, AC43).
func TestContainerRunsTheCachedJobImageWhenTheRegistryIsDown(t *testing.T) {
	sfx := uuid.NewString()[:8]
	e := newEngine(testSocket(t))
	addr := privateRegistry(t, e, "", "")
	public := &RegistryAuth{Registry: addr}
	cached := addr + "/vhco-pro/stackweaver-opentofu-runner-" + sfx + ":latest"
	staleJobImage(t, e, sfx, cached, public)
	cachedDigest, err := e.imageDigest(t.Context(), cached)
	if err != nil {
		t.Fatal(err)
	}
	uncached := addr + "/vhco-pro/stackweaver-ansible-runner-" + sfx + ":latest"
	pushTo(t, e, "sw-probe-new-"+sfx+":1", uncached, public)
	dropLocal(t, e, uncached)
	runLocal := "sw-probe-runimage-" + sfx + ":1"
	buildStampedProbeImage(t, e, "scratch", runLocal, "1001:1001", "run-image", map[string]string{LabelManaged: "true"})
	runImage := addr + "/acme/ee-" + sfx + ":1"
	pushTo(t, e, runLocal, runImage, public)
	stopRegistry(t, e, addr)

	d := &Container{Socket: e.socket, JobImages: map[string]string{"tofu": cached, "ansible": uncached}, ExecutorPath: "/probe", JobImagePull: PullAlways}

	res, p, exit := runProbe(t, d, Request{JobID: "cached-" + sfx, Attempt: 1, Kind: "tofu", Labels: map[string]string{LabelJob: "cached-" + sfx}})
	if exit.Code != 0 || res.Stamp != "old" {
		t.Fatalf("the cached job image's job = %q exit %+v, want the cached copy run", res.Stamp, exit)
	}
	if report := placementReport(t, e, p.ID); !strings.Contains(report, cached) || !strings.Contains(report, "cached") {
		t.Errorf("the job's warning = %q, want the failed pull of %s named and the cached copy said", report, cached)
	}
	if env := containerEnv(t, e, p.ID); !slices.Contains(env, "STACKWEAVER_IMAGE_DIGEST="+cachedDigest) {
		t.Errorf("the placement reports %v, want the cached digest %s", env, cachedDigest)
	}

	_, err = d.Place(t.Context(), Request{JobID: "uncached-" + sfx, Attempt: 1, Kind: "ansible", Labels: withTestRun(map[string]string{LabelJob: "uncached-" + sfx})})
	if err == nil || !strings.Contains(err.Error(), uncached) {
		t.Errorf("an uncached job image with the registry down = %v, want a failure naming %s", err, uncached)
	}

	// The run image is cached too, and still never runs from the cache.
	if imageID(t, e, runImage) == "" {
		t.Fatal("the run image is not cached; the case proves nothing")
	}
	_, err = d.Place(t.Context(), Request{JobID: "runimage-" + sfx, Attempt: 1, Kind: "tofu", Image: runImage, Labels: withTestRun(map[string]string{LabelJob: "runimage-" + sfx})})
	if err == nil || !strings.Contains(err.Error(), runImage) {
		t.Errorf("a cached run image with the registry down = %v, want a failure naming %s", err, runImage)
	}
}

// Each pull attempt is bounded, so a registry that accepts connections and never answers does not
// stall a placement (OD21).
func TestContainerPullOfAStalledRegistryIsBounded(t *testing.T) {
	sfx := uuid.NewString()[:8]
	e := newEngine(testSocket(t))
	// A listener that accepts and never answers, on every address so the engine reaches it.
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	image := ln.Addr().String() + "/vhco-pro/stackweaver-opentofu-runner-" + sfx + ":latest"
	local := "sw-probe-stalled-" + sfx + ":1"
	buildStampedProbeImage(t, e, "alpine:3.22", local, "1001:1001", "cached", map[string]string{LabelManaged: "true"})
	tagLocal(t, e, local, image)
	t.Cleanup(func() {
		_ = e.do(context.Background(), http.MethodDelete, "/images/"+url.PathEscape(image), url.Values{"force": {"1"}}, nil, nil)
	})

	d := &Container{Socket: e.socket, JobImages: sameJobImage(image), ExecutorPath: "/probe", JobImagePull: PullAlways, PullStallTimeout: 3 * time.Second}
	start := time.Now()
	res, _, exit := runProbe(t, d, Request{JobID: "stalled-" + sfx, Attempt: 1, Kind: "tofu", Labels: map[string]string{LabelJob: "stalled-" + sfx}})
	if took := time.Since(start); took > 45*time.Second {
		t.Errorf("the placement took %s against a stalled registry", took)
	}
	if exit.Code != 0 || res.Stamp != "cached" {
		t.Errorf("the job = %q exit %+v, want the cached copy run once the pull gave up", res.Stamp, exit)
	}
}

// A pull the registry refuses (401, 404) never falls back to a cached copy, so a revoked credential
// or a removed image never runs from a shared host's cache (OD27, AC43).
func TestContainerRefusedPullsNeverFallBack(t *testing.T) {
	sfx := uuid.NewString()[:8]
	e := newEngine(testSocket(t))
	local := "sw-probe-refused-" + sfx + ":1"
	buildStampedProbeImage(t, e, "alpine:3.22", local, "1001:1001", "cached", map[string]string{LabelManaged: "true"})

	private := privateRegistry(t, e, "robot", "s3cret")
	unauthorized := private + "/vhco-pro/stackweaver-opentofu-runner-" + sfx + ":latest"
	pushTo(t, e, local, unauthorized, &RegistryAuth{Registry: private, Username: "robot", Password: "s3cret"})

	public := privateRegistry(t, e, "", "")
	missing := public + "/vhco-pro/stackweaver-opentofu-runner-gone-" + sfx + ":latest"
	tagLocal(t, e, local, missing)
	t.Cleanup(func() {
		_ = e.do(context.Background(), http.MethodDelete, "/images/"+url.PathEscape(missing), url.Values{"force": {"1"}}, nil, nil)
	})

	for name, image := range map[string]string{"401": unauthorized, "404": missing} {
		if imageID(t, e, image) == "" {
			t.Fatalf("%s: the image is not cached; the case proves nothing", name)
		}
		d := &Container{Socket: e.socket, JobImages: sameJobImage(image), ExecutorPath: "/probe", JobImagePull: PullAlways}
		p, err := d.Place(t.Context(), Request{JobID: "refused-" + name + "-" + sfx, Attempt: 1, Kind: "tofu", Labels: withTestRun(map[string]string{LabelJob: "refused-" + name + "-" + sfx})})
		if err == nil {
			_ = e.remove(context.Background(), p.ID)
			t.Errorf("%s: the refused pull of %s fell back to the cached copy", name, image)
		}
	}
}
