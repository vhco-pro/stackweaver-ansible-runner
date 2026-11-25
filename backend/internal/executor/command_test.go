// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func shortOrphanPipeWait(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("needs sh")
	}
	saved := orphanPipeWait
	orphanPipeWait = 100 * time.Millisecond
	t.Cleanup(func() { orphanPipeWait = saved })
}

// runAsync runs cmd through Run in the background and returns its result channel.
func runAsync(ctx context.Context, cmd *exec.Cmd) <-chan error {
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cmd) }()
	return done
}

// A stopped command returns even while a descendant it started still holds its output pipe.
func TestRunReturnsWhileADescendantHoldsThePipeOfAStoppedCommand(t *testing.T) {
	shortOrphanPipeWait(t)
	ctx, cancel := context.WithCancel(t.Context())
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & echo started; wait")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	done := runAsync(ctx, cmd)
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a stopped command reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stopped command waited for the descendant holding its pipe")
	}
}

// slowWriter blocks each write for delay, as a log stream to a loaded API can.
type slowWriter struct {
	delay time.Duration
	mu    sync.Mutex
	buf   bytes.Buffer
}

func (w *slowWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *slowWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// A command that exits 0 succeeds and delivers all its output even when its output writer takes
// longer than the bound: the bound applies only once the command is stopped.
func TestRunDoesNotFailACommandThatExitedWhileItsWriterIsSlow(t *testing.T) {
	shortOrphanPipeWait(t)
	cmd := exec.CommandContext(t.Context(), "sh", "-c", "echo done; echo warning >&2")
	stdout, stderr := &slowWriter{delay: 500 * time.Millisecond}, &slowWriter{delay: 500 * time.Millisecond}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := Run(t.Context(), cmd); err != nil {
		t.Fatalf("a command that exited 0 failed: %v", err)
	}
	if got := stdout.String(); got != "done\n" {
		t.Errorf("stdout = %q, want %q", got, "done\n")
	}
	if got := stderr.String(); got != "warning\n" {
		t.Errorf("stderr = %q, want %q", got, "warning\n")
	}
}

// A command that exits on its own is not cut short while a descendant still writes to its pipe;
// once its context is done, the bound applies.
func TestRunBoundsTheWaitOnlyAfterCancellation(t *testing.T) {
	shortOrphanPipeWait(t)
	ctx, cancel := context.WithCancel(t.Context())
	cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & echo started")
	var out bytes.Buffer
	cmd.Stdout = &out
	done := runAsync(ctx, cmd)
	select {
	case err := <-done:
		t.Fatalf("returned before cancellation while a descendant held the pipe: %v", err)
	case <-time.After(5 * orphanPipeWait):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, exec.ErrWaitDelay) {
			t.Errorf("err = %v, want exec.ErrWaitDelay", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled wait was not bounded")
	}
}

// Output returns stdout and, on a non-zero exit, the command's stderr in the exit error.
func TestOutputCapturesStdoutAndTheStderrOfAFailure(t *testing.T) {
	shortOrphanPipeWait(t)
	got, err := Output(t.Context(), exec.CommandContext(t.Context(), "sh", "-c", "echo head"))
	if err != nil || string(got) != "head\n" {
		t.Fatalf("Output = %q, %v; want %q, nil", got, err, "head\n")
	}
	_, err = Output(t.Context(), exec.CommandContext(t.Context(), "sh", "-c", "echo broken >&2; exit 3"))
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 || !strings.Contains(string(exitErr.Stderr), "broken") {
		t.Errorf("Output error = %v, want exit 3 carrying stderr", err)
	}
}

// CombinedOutput interleaves stdout and stderr into one result.
func TestCombinedOutputCarriesBothStreams(t *testing.T) {
	shortOrphanPipeWait(t)
	got, err := CombinedOutput(t.Context(), exec.CommandContext(t.Context(), "sh", "-c", "echo out; echo err >&2"))
	if err != nil || !strings.Contains(string(got), "out") || !strings.Contains(string(got), "err") {
		t.Fatalf("CombinedOutput = %q, %v", got, err)
	}
}

// A writer that is a file is handed to the child as it is, with no pipe of the executor's.
func TestRunPassesAFileWriterThrough(t *testing.T) {
	shortOrphanPipeWait(t)
	f, err := createTemp(t)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "sh", "-c", "echo direct")
	cmd.Stdout = f
	if err := Run(t.Context(), cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.Stdout != f {
		t.Error("a file writer was replaced")
	}
}

func createTemp(t *testing.T) (*os.File, error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err == nil {
		t.Cleanup(func() { _ = f.Close() })
	}
	return f, err
}
