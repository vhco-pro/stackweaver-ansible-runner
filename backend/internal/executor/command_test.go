// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"bytes"
	"context"
	"os/exec"
	"testing"
	"time"
)

// A stopped command returns even while a descendant it started still holds its output pipe.
func TestBoundOrphanPipesReturnsWhileADescendantHoldsThePipe(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("needs sh")
	}
	saved := orphanPipeWait
	orphanPipeWait = 100 * time.Millisecond
	t.Cleanup(func() { orphanPipeWait = saved })

	ctx, cancel := context.WithCancel(t.Context())
	cmd := BoundOrphanPipes(exec.CommandContext(ctx, "sh", "-c", "sleep 30 & echo started; wait"))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a stopped command reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stopped command waited for the descendant holding its pipe")
	}
}
