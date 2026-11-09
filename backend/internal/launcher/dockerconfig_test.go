// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
)

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func basicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

// The launcher pulls private job images with a credential of its own, read from Docker config
// files that RUNNER_DOCKER_CONFIG names (one-launcher spec, A3, OD19): one or more files, or a
// directory, with inline auths entries (auth, or username and password) and identitytoken;
// credsStore and credHelpers are reported unsupported at start.
func TestDockerConfig(t *testing.T) {
	dir := t.TempDir()
	ghcr := writeFile(t, filepath.Join(dir, "ghcr.json"), `{"auths":{"ghcr.io":{"auth":"`+basicAuth("bot", "s3cret")+`"}}}`)
	userPass := writeFile(t, filepath.Join(dir, "mirror.json"), `{"auths":{"https://mirror.example:5000/v2/":{"username":"m","password":"p"}}}`)
	token := writeFile(t, filepath.Join(dir, "token.json"), `{"auths":{"registry.example":{"identitytoken":"tok-1"}}}`)

	t.Run("unset reads nothing", func(t *testing.T) {
		cfg, err := configFrom(t, map[string]string{EnvDriver: driver.NameContainer})
		if err != nil {
			t.Fatal(err)
		}
		if a := cfg.JobImageAuth.For("ghcr.io/vhco-pro/stackweaver-opentofu-runner:latest"); a != nil {
			t.Errorf("credential without %s: %+v", EnvDockerConfig, a)
		}
		if len(cfg.Warnings) != 0 {
			t.Errorf("warnings without %s: %v", EnvDockerConfig, cfg.Warnings)
		}
	})

	t.Run("an auth entry", func(t *testing.T) {
		cfg, err := configFrom(t, map[string]string{EnvDriver: driver.NameContainer, EnvDockerConfig: ghcr})
		if err != nil {
			t.Fatal(err)
		}
		a := cfg.JobImageAuth.For("ghcr.io/vhco-pro/stackweaver-opentofu-runner:latest")
		if a == nil || a.Username != "bot" || a.Password != "s3cret" || a.Registry != "ghcr.io" {
			t.Fatalf("ghcr.io credential = %+v, want bot/s3cret for ghcr.io", a)
		}
		if a := cfg.JobImageAuth.For("quay.io/other/image:1"); a != nil {
			t.Errorf("another registry got the ghcr.io credential: %+v", a)
		}
		if a := cfg.JobImageAuth.For("alpine:3"); a != nil {
			t.Errorf("Docker Hub got the ghcr.io credential: %+v", a)
		}
	})

	t.Run("several files, a username and password entry and an identity token", func(t *testing.T) {
		cfg, err := configFrom(t, map[string]string{EnvDriver: driver.NameContainer, EnvDockerConfig: ghcr + ", " + userPass + "," + token})
		if err != nil {
			t.Fatal(err)
		}
		if a := cfg.JobImageAuth.For("mirror.example:5000/stackweaver/opentofu:1"); a == nil || a.Username != "m" || a.Password != "p" || a.Registry != "mirror.example:5000" {
			t.Errorf("mirror credential = %+v, want m/p for mirror.example:5000 (the key's scheme and path dropped)", a)
		}
		if a := cfg.JobImageAuth.For("registry.example/job:1"); a == nil || a.IdentityToken != "tok-1" {
			t.Errorf("identity token credential = %+v, want tok-1", a)
		}
		if a := cfg.JobImageAuth.For("ghcr.io/vhco-pro/stackweaver-ansible-runner:latest"); a == nil || a.Username != "bot" {
			t.Errorf("ghcr.io credential lost in the merge: %+v", a)
		}
	})

	t.Run("a directory reads every json file in it", func(t *testing.T) {
		mounted := t.TempDir()
		writeFile(t, filepath.Join(mounted, "a.json"), `{"auths":{"ghcr.io":{"auth":"`+basicAuth("a", "1")+`"}}}`)
		writeFile(t, filepath.Join(mounted, "b.json"), `{"auths":{"https://index.docker.io/v1/":{"auth":"`+basicAuth("hub", "2")+`"}}}`)
		writeFile(t, filepath.Join(mounted, "README"), "not a config")
		writeFile(t, filepath.Join(mounted, ".hidden.json"), "{broken")
		writeFile(t, filepath.Join(mounted, "..data", "c.json"), "{broken")
		cfg, err := configFrom(t, map[string]string{EnvDriver: driver.NameContainer, EnvDockerConfig: mounted})
		if err != nil {
			t.Fatal(err)
		}
		if a := cfg.JobImageAuth.For("ghcr.io/x/y:1"); a == nil || a.Username != "a" {
			t.Errorf("ghcr.io credential from the directory = %+v", a)
		}
		if a := cfg.JobImageAuth.For("library/alpine:3"); a == nil || a.Username != "hub" || a.Registry != "docker.io" {
			t.Errorf("Docker Hub credential from the directory = %+v, want hub for docker.io", a)
		}
	})

	t.Run("credsStore and credHelpers are reported unsupported", func(t *testing.T) {
		helpers := writeFile(t, filepath.Join(t.TempDir(), "config.json"), `{"auths":{"ghcr.io":{}},"credsStore":"desktop","credHelpers":{"123.dkr.ecr.eu-west-1.amazonaws.com":"ecr-login"}}`)
		cfg, err := configFrom(t, map[string]string{EnvDriver: driver.NameContainer, EnvDockerConfig: helpers})
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"credsStore", "credHelpers"} {
			if !slices.ContainsFunc(cfg.Warnings, func(w string) bool {
				return strings.Contains(w, name) && strings.Contains(w, "not supported") && strings.Contains(w, helpers)
			}) {
				t.Errorf("no warning naming %s as not supported in %s: %v", name, helpers, cfg.Warnings)
			}
		}
		if a := cfg.JobImageAuth.For("ghcr.io/x/y:1"); a != nil {
			t.Errorf("an empty auths entry became a credential: %+v", a)
		}
	})

	t.Run("a missing file or broken json refuses to start", func(t *testing.T) {
		missing := filepath.Join(dir, "absent.json")
		if _, err := configFrom(t, map[string]string{EnvDriver: driver.NameContainer, EnvDockerConfig: missing}); err == nil || !strings.Contains(err.Error(), EnvDockerConfig) || !strings.Contains(err.Error(), missing) {
			t.Errorf("missing file: err = %v, want one naming %s and %s", err, EnvDockerConfig, missing)
		}
		broken := writeFile(t, filepath.Join(t.TempDir(), "broken.json"), "{not json")
		if _, err := configFrom(t, map[string]string{EnvDriver: driver.NameContainer, EnvDockerConfig: broken}); err == nil || !strings.Contains(err.Error(), broken) {
			t.Errorf("broken json: err = %v, want one naming %s", err, broken)
		}
		badAuth := writeFile(t, filepath.Join(t.TempDir(), "bad.json"), `{"auths":{"ghcr.io":{"auth":"not base64!"}}}`)
		if _, err := configFrom(t, map[string]string{EnvDriver: driver.NameContainer, EnvDockerConfig: badAuth}); err == nil || !strings.Contains(err.Error(), "ghcr.io") {
			t.Errorf("undecodable auth: err = %v, want one naming the registry", err)
		}
	})
}
