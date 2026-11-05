// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// The sync executor's clone holds the connection's credential in memory only (#933, D2, decision
// 18, AC68): a minted token and a Generic Git long-lived token reach git through a one-shot
// GIT_ASKPASS helper, and an SSH deploy key through GIT_SSH_COMMAND on a memory-backed file with
// the pinned host key. The credential is in no argument list, no environment and no file of the
// job's filesystem.

package syncexec

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/cgi" //nolint:gosec // G504: the test serves git over HTTP on this host only
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// envFakeSSH turns the test binary into a stand-in ssh that records what it was given.
const envFakeSSH = "SYNCEXEC_TEST_FAKE_SSH"

func TestMain(m *testing.M) {
	if IsAskpass() {
		os.Exit(AskpassMain(os.Args[1:]))
	}
	if out := os.Getenv(envFakeSSH); out != "" {
		os.Exit(fakeSSH(out, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// sshRecord is what the stand-in ssh saw.
type sshRecord struct {
	Args       []string `json:"args"`
	Key        string   `json:"key"`
	KnownHosts string   `json:"known_hosts"`
}

func fakeSSH(out string, args []string) int {
	rec := sshRecord{Args: args}
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-i" && i+1 < len(args):
			data, _ := os.ReadFile(args[i+1]) //nolint:gosec // G703: the stand-in ssh reads what it was handed
			rec.Key = string(data)
		case args[i] == "-o" && i+1 < len(args) && strings.HasPrefix(args[i+1], "UserKnownHostsFile="):
			data, _ := os.ReadFile(strings.TrimPrefix(args[i+1], "UserKnownHostsFile=")) //nolint:gosec // G703: as above
			rec.KnownHosts = string(data)
		}
	}
	raw, _ := json.Marshal(rec)       //nolint:errchkjson // a struct of strings
	_ = os.WriteFile(out, raw, 0o600) //nolint:gosec // G703: the test names the file
	return 255
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec,noctx // G204: the test runs git on its own repository
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// gitServer serves a bare repository over HTTPS behind basic authentication, as a VCS host does.
func gitServer(t *testing.T, user, token string) (url, caFile, commit string) {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	gitIn(t, work, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "site.yml"), []byte("- hosts: all\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, work, "add", "site.yml")
	gitIn(t, work, "commit", "-q", "-m", "first")
	gitIn(t, root, "clone", "-q", "--bare", work, "repo.git")
	head, err := exec.Command("git", "-C", work, "rev-parse", "HEAD").Output() //nolint:gosec,noctx // G204: as above
	if err != nil {
		t.Fatal(err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	backend := &cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != user || p != token {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	caFile = filepath.Join(root, "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return srv.URL + "/repo.git", caFile, strings.TrimSpace(string(head))
}

// noFileHolds fails when any file under dir contains secret.
func noFileHolds(t *testing.T, dir, secret string) {
	t.Helper()
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		if data, err := os.ReadFile(path); err == nil && bytes.Contains(data, []byte(secret)) { //nolint:gosec // G304: a walk of the test's own directory
			t.Errorf("%s holds the credential", path)
		}
		return nil
	})
}

func newTestCloner(t *testing.T, secretDir string, env ...string) (*cloner, *[][]string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var seen [][]string
	c := &cloner{
		askpass: exe, ssh: exe, secretDir: secretDir,
		env: append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}, env...),
		observe: func(args, env []string) {
			seen = append(seen, args, env)
		},
	}
	return c, &seen
}

func TestClone_TokenThroughOneShotAskpass(t *testing.T) {
	for name, user := range map[string]string{
		"a minted installation token":    "x-access-token",
		"a Generic Git long-lived token": "deploy-bot",
	} {
		t.Run(name, func(t *testing.T) {
			const token = "tok-9f8e7d6c5b4a39281706" //nolint:gosec // G101: a test value
			url, ca, commit := gitServer(t, user, token)
			work, secrets := t.TempDir(), t.TempDir()
			c, seen := newTestCloner(t, secrets, "GIT_SSL_CAINFO="+ca)

			var log bytes.Buffer
			got, err := c.clone(t.Context(), &runnercontract.CloneSource{URL: url, Branch: "main", Username: user, Token: token}, filepath.Join(work, "repo"), &log)
			if err != nil {
				t.Fatalf("clone: %v\n%s", err, log.String())
			}
			if got != commit {
				t.Errorf("clone resolved %s, want %s", got, commit)
			}
			for _, list := range *seen {
				if slices.ContainsFunc(list, func(s string) bool { return strings.Contains(s, token) }) {
					t.Errorf("the token is in an argument list or the environment: %v", list)
				}
			}
			noFileHolds(t, work, token)
			noFileHolds(t, secrets, token)
			if _, err := os.Stat(filepath.Join(work, "repo", "site.yml")); err != nil {
				t.Errorf("the clone has no site.yml: %v", err)
			}

			// The server is not open: a wrong token is refused.
			if _, err := c.clone(t.Context(), &runnercontract.CloneSource{URL: url, Branch: "main", Username: user, Token: "wrong"}, filepath.Join(work, "again"), &log); err == nil {
				t.Error("a clone with a wrong token succeeded")
			}
		})
	}
}

func TestClone_SSHKeyOnAMemoryFileWithAPinnedHostKey(t *testing.T) {
	const key = "-----BEGIN OPENSSH PRIVATE KEY-----\nsecret-deploy-key-material\n-----END OPENSSH PRIVATE KEY-----"
	const knownHosts = "git.example.test ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExamplePinnedHostKey\n"
	work, secrets := t.TempDir(), t.TempDir()
	record := filepath.Join(t.TempDir(), "ssh.json")
	c, seen := newTestCloner(t, secrets, envFakeSSH+"="+record)

	_, err := c.clone(t.Context(), &runnercontract.CloneSource{URL: "git@git.example.test:acme/playbooks.git", Branch: "main", SSHPrivateKey: key, KnownHosts: knownHosts}, filepath.Join(work, "repo"), &bytes.Buffer{})
	if err == nil {
		t.Fatal("the clone through the stand-in ssh succeeded")
	}
	raw, readErr := os.ReadFile(record) //nolint:gosec // G304: the test's own record
	if readErr != nil {
		t.Fatalf("ssh was not run: %v (clone error %v)", readErr, err)
	}
	var rec sshRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(rec.Key) != key || rec.KnownHosts != knownHosts {
		t.Errorf("ssh read key %q and known hosts %q, want the connection's", rec.Key, rec.KnownHosts)
	}
	if !slices.Contains(rec.Args, "StrictHostKeyChecking=yes") || slices.Contains(rec.Args, "StrictHostKeyChecking=no") {
		t.Errorf("ssh ran with %v, want the host key pinned", rec.Args)
	}
	for _, list := range append(*seen, rec.Args) {
		if slices.ContainsFunc(list, func(s string) bool { return strings.Contains(s, "secret-deploy-key-material") }) {
			t.Errorf("the key is in an argument list or the environment: %v", list)
		}
	}
	noFileHolds(t, work, "secret-deploy-key-material")
	noFileHolds(t, secrets, "secret-deploy-key-material")

	// Without the pinned host key there is no clone at all.
	if _, err := c.clone(t.Context(), &runnercontract.CloneSource{URL: "git@git.example.test:acme/playbooks.git", SSHPrivateKey: key}, filepath.Join(work, "x"), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "pinned host key") {
		t.Errorf("an SSH clone without a pinned host key = %v, want it refused", err)
	}
}

func TestClone_RefusesURLsAndBranchesGitWouldParseAsOptions(t *testing.T) {
	c, _ := newTestCloner(t, t.TempDir())
	for _, src := range []runnercontract.CloneSource{
		{URL: "https://oauth2:tok@gitlab.com/acme/repo.git"}, //nolint:gosec // G101: a refused test value
		{URL: "--upload-pack=touch /tmp/x"},
		{URL: "https://gitlab.com/acme/repo.git", Branch: "--upload-pack=evil"},
		{URL: "ext::sh -c touch% /tmp/pwned"},
	} {
		if _, err := c.clone(t.Context(), &src, filepath.Join(t.TempDir(), "r"), &bytes.Buffer{}); err == nil {
			t.Errorf("cloned %+v", src)
		}
	}
}
