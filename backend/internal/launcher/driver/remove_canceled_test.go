// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEndedContainerEngine answers an inspect of one ended job container, kept for an hour and
// stopped with the executor's stopped code, and records the containers deleted.
func fakeEndedContainerEngine(t *testing.T, exitCode int) (*Container, func() []string) {
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
	var deleted []string
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/c1/json"):
			answerJSON(w, map[string]any{"Id": "c1", "Config": map[string]any{"Labels": map[string]string{LabelManaged: "true", labelKeepFailedFor: "3600"}}, "State": map[string]any{"ExitCode": exitCode}})
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/containers/c1"):
			mu.Lock()
			deleted = append(deleted, "c1")
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return &Container{Socket: sock}, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), deleted...)
	}
}

// stoppedExit is the executor's exit code for a stopped (canceled) job, executor.ExitStopped.
const stoppedExit = 3

// A canceled job is not a failure: its stopped container is removed at once even on a pool that
// keeps failures, so a cancel removes it within its ten seconds (#933, AC12). A stopped container
// that was not canceled (a timeout, which fails the job) is kept like any failure (AC25).
func TestContainerRemovesACanceledJobDespiteTheKeepWindow(t *testing.T) {
	d, deleted := fakeEndedContainerEngine(t, stoppedExit)
	if err := d.Remove(t.Context(), Placement{ID: "c1", Canceled: true}); err != nil {
		t.Fatal(err)
	}
	if got := deleted(); len(got) != 1 {
		t.Fatalf("the canceled job's container was kept as a failure (deleted %v)", got)
	}

	d, deleted = fakeEndedContainerEngine(t, stoppedExit)
	if err := d.Remove(t.Context(), Placement{ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if got := deleted(); len(got) != 0 {
		t.Fatalf("a stopped job that was not canceled was removed inside its keep window (deleted %v)", got)
	}
}
