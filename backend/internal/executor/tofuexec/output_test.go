// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package tofuexec

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/backend/internal/executor"
)

// TestJobOutputWritersConcurrentStreams pins AUD-028: os/exec copies stdout and stderr on two
// goroutines when the writers differ, so both must be able to write the shared streamer and
// output buffer at once. Run with -race to catch a regression; without it the byte counts below
// still catch lost writes.
func TestJobOutputWritersConcurrentStreams(t *testing.T) {
	var (
		mu       sync.Mutex
		received strings.Builder
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding output payload: %v", err)
		}
		mu.Lock()
		out, _ := body["output"].(string)
		received.WriteString(out)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	streamer := &streamWriter{ctx: t.Context(), client: executor.NewClient(srv.URL, "token", "runner-1"), jobID: "run-1", phase: "plan", buffer: &bytes.Buffer{}, lastSend: time.Now()}
	fullOutput := &bytes.Buffer{}

	stdout, stderr := jobOutputWriters(streamer, fullOutput, io.Discard, io.Discard)

	const writesPerStream = 500
	line := strings.Repeat("x", 63) + "\n" // 64 bytes: crosses the 4KB flush threshold repeatedly

	var wg sync.WaitGroup
	for _, w := range []io.Writer{stdout, stderr} {
		wg.Go(func() {
			for range writesPerStream {
				if _, err := w.Write([]byte(line)); err != nil {
					t.Errorf("Write() error = %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
	streamer.flush()

	want := 2 * writesPerStream * len(line)
	if fullOutput.Len() != want {
		t.Fatalf("full output has %d bytes, want %d", fullOutput.Len(), want)
	}
	mu.Lock()
	defer mu.Unlock()
	if received.Len() != want {
		t.Fatalf("server received %d bytes, want %d", received.Len(), want)
	}
}
