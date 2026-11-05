// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeBuildEngine serves the engine API on a unix socket with handler and returns its engine.
func fakeBuildEngine(t *testing.T, handler http.HandlerFunc) *engine {
	t.Helper()
	dir, err := os.MkdirTemp("", "sw-build")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "e.sock")
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return newEngine(sock)
}

func TestPostImageBuild_AStalledBuildFailsAtItsDeadlineNamingTheImage(t *testing.T) {
	// The engine answers the headers and then never finishes the build stream, as a stuck daemon
	// build session does.
	e := fakeBuildEngine(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"stream":"Step 1/2"}` + "\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	start := time.Now()
	err := postImageBuild(t.Context(), e, "sw-probe-stalled:1", &bytes.Buffer{}, nil, 200*time.Millisecond)
	if !errors.Is(err, errImageBuildDeadline) {
		t.Fatalf("err = %v, want the build deadline", err)
	}
	if !strings.Contains(err.Error(), "sw-probe-stalled:1") {
		t.Errorf("err = %q, want it to name the image", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the build returned after %s, want soon after its 200ms deadline", elapsed)
	}
}

func TestPostImageBuild_AnErrorInTheBuildStreamIsReturned(t *testing.T) {
	e := fakeBuildEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"error":"no such image: alpine:3.22"}` + "\n"))
	})
	err := postImageBuild(t.Context(), e, "sw-tools-x:1", &bytes.Buffer{}, map[string]string{"a": "b"}, time.Minute)
	if err == nil || errors.Is(err, errImageBuildDeadline) || !strings.Contains(err.Error(), "no such image") || !strings.Contains(err.Error(), "sw-tools-x:1") {
		t.Fatalf("err = %v, want the stream's error naming the image", err)
	}
}

func TestPostImageBuild_ACompletedBuildSucceeds(t *testing.T) {
	var labels string
	e := fakeBuildEngine(t, func(w http.ResponseWriter, r *http.Request) {
		labels = r.URL.Query().Get("labels")
		_, _ = w.Write([]byte(`{"stream":"Successfully built 0123456789ab"}` + "\n"))
	})
	if err := postImageBuild(t.Context(), e, "sw-probe-ok:1", &bytes.Buffer{}, map[string]string{"a": "b"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if labels != `{"a":"b"}` {
		t.Errorf("labels = %q, want the image labels as JSON", labels)
	}
}
