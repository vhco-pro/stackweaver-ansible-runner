// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// Run with:
//
//	go test -tags integration ./internal/launcher/driver/ -run TestStaleOneShotContainersAreRemoved

//go:build integration

package driver

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A firewall writer whose launcher stopped between creating and removing it (a container recreated
// mid-start) stays behind as a created container, holding its image. The next writer removes such
// leftovers once they are older than the window, and never one that runs or is still fresh.
func TestStaleOneShotContainersAreRemoved(t *testing.T) {
	sock := testSocket(t)
	e := newEngine(sock)
	sfx := uuid.NewString()[:8]
	tools := "sw-tools-oneshot-" + sfx + ":1"
	buildToolsImage(t, e, tools)
	labels := withTestRun(map[string]string{LabelManaged: "true", labelRole: roleFirewall})

	create := func(cmd string) string {
		t.Helper()
		id, err := e.create(t.Context(), "", containerConfig{Image: tools, Entrypoint: []string{"/bin/sh", "-c"}, Cmd: []string{cmd}, Labels: labels})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.remove(context.Background(), id) })
		return id
	}
	stale := create("true")
	running := create("sleep 120")
	if err := e.start(t.Context(), running); err != nil {
		t.Fatal(err)
	}
	selector := withTestRun(map[string]string{LabelManaged: "true", labelRole: roleFirewall})

	// Inside the window nothing is removed: another launcher may be about to start it.
	removeStaleOneShots(t.Context(), e, selector, time.Hour)
	if _, err := e.inspect(t.Context(), stale); err != nil {
		t.Fatalf("a fresh created writer was removed: %v", err)
	}

	time.Sleep(1100 * time.Millisecond) // the engine reports creation times in seconds
	removeStaleOneShots(t.Context(), e, selector, time.Second)
	if _, err := e.inspect(t.Context(), stale); !isNotFound(err) {
		t.Errorf("the stale created writer is still there: %v", err)
	}
	if _, err := e.inspect(t.Context(), running); err != nil {
		t.Errorf("the running writer was removed: %v", err)
	}
}
