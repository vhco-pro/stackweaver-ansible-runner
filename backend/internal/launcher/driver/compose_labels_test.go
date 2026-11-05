// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// answerJSON answers a fake engine request with v as JSON.
func answerJSON(w http.ResponseWriter, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(data)
}

// fakeComposeImageEngine answers the Engine API calls create makes: an image inspect that returns
// imageLabels, and a container create whose request body it keeps.
func fakeComposeImageEngine(t *testing.T, imageLabels map[string]string) (*engine, func() containerConfig) {
	t.Helper()
	dir, err := os.MkdirTemp("", "swe")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "e.sock")
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var created containerConfig
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			answerJSON(w, map[string]any{"Id": "sha256:abc", "Config": map[string]any{"Labels": imageLabels}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/containers/create"):
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&created)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			answerJSON(w, map[string]string{"Id": "c1"})
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return newEngine(sock), func() containerConfig {
		mu.Lock()
		defer mu.Unlock()
		return created
	}
}

// A container inherits its image's labels, and an image Compose built carries its project and
// service. A job placed from the launcher's own image would otherwise be taken for a replica of the
// launcher service, which `docker compose up`, `down` and `--remove-orphans` stop or remove. Every
// container the driver creates carries those labels with an empty value, which no project or
// service filter matches, and keeps the driver's own.
func TestCreateClearsComposeIdentityInheritedFromTheImage(t *testing.T) {
	e, created := fakeComposeImageEngine(t, map[string]string{
		"com.docker.compose.project":             "deploy",
		"com.docker.compose.service":             "launcher",
		"com.docker.compose.version":             "5.4.0",
		"com.docker.compose.project.working_dir": "/src/deploy",
		"io.podman.compose.project":              "deploy",
		"org.opencontainers.image.title":         "stackweaver-runner",
	})
	if _, err := e.create(t.Context(), "sw-job-run-1-1", containerConfig{
		Image:  "deploy-launcher",
		Labels: map[string]string{LabelManaged: "true", LabelJob: "run-1", LabelRunner: "r1"},
	}); err != nil {
		t.Fatal(err)
	}
	got := created().Labels
	for _, key := range []string{"com.docker.compose.project", "com.docker.compose.service", "com.docker.compose.version", "com.docker.compose.project.working_dir", "com.docker.compose.oneoff", "com.docker.compose.container-number", "com.docker.compose.config-hash", "io.podman.compose.project"} {
		v, ok := got[key]
		if !ok || v != "" {
			t.Errorf("%s = %q (set %t), want it overridden with an empty value", key, v, ok)
		}
	}
	for key, want := range map[string]string{LabelManaged: "true", LabelJob: "run-1", LabelRunner: "r1"} {
		if got[key] != want {
			t.Errorf("the driver's label %s = %q, want %q", key, got[key], want)
		}
	}
	if _, ok := got["org.opencontainers.image.title"]; ok {
		t.Errorf("a label unrelated to Compose was overridden: %v", got)
	}
}

// An image that is not a Compose build (a published image) still gets the identity labels
// cleared, so the rule does not depend on how the image was made.
func TestCreateClearsComposeIdentityForAnyImage(t *testing.T) {
	e, created := fakeComposeImageEngine(t, nil)
	if _, err := e.create(t.Context(), "", containerConfig{Image: "ghcr.io/vhco-pro/stackweaver-opentofu-runner:1"}); err != nil {
		t.Fatal(err)
	}
	got := created().Labels
	for _, key := range []string{"com.docker.compose.project", "com.docker.compose.service", "com.docker.compose.oneoff"} {
		if v, ok := got[key]; !ok || v != "" {
			t.Errorf("%s = %q (set %t), want an empty override", key, v, ok)
		}
	}
}
