// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// objectDriver is a fake driver that manages long-lived objects, as the Kubernetes driver does.
type objectDriver struct {
	*fakeDriver
	ensured chan []runnercontract.EnsureObject
	// gate, when set, makes every collection fail until it is closed, so a test can hold the
	// first successful read until the launcher is ready to place work.
	gate chan struct{}
}

func (d *objectDriver) Inventory(context.Context) ([]runnercontract.InventoryEntry, error) {
	if d.gate != nil {
		select {
		case <-d.gate:
		default:
			return nil, fmt.Errorf("namespace not readable yet")
		}
	}
	return []runnercontract.InventoryEntry{{Kind: "ServiceAccount", Namespace: "jobs", Name: "sw-argo", Present: true, ManagedBy: "external", ExternalManager: "Argo CD application platform"}}, nil
}

func (d *objectDriver) Ensure(_ context.Context, objs []runnercontract.EnsureObject) error {
	d.ensured <- objs
	return nil
}

// logLines captures what a launcher logs.
func logLines(l *Launcher) func() []string {
	var mu sync.Mutex
	var lines []string
	l.client.Log = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(lines)
	}
}

func inventoryReads(lines []string) int {
	want := fmt.Sprintf(InventoryReadMessage, 1)
	n := 0
	for _, line := range lines {
		if line == want {
			n++
		}
	}
	return n
}

// anyInventoryLines counts inventory lines of any object count, so a spurious line for an empty
// or failed read cannot hide behind a count of one.
func anyInventoryLines(lines []string) int {
	n := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "Read the job namespace's inventory: ") {
			n++
		}
	}
	return n
}

// listDriver collects a fixed inventory, or fails, so a test can choose the object count.
type listDriver struct {
	*fakeDriver
	entries []runnercontract.InventoryEntry
	err     error
}

func (d *listDriver) Inventory(context.Context) ([]runnercontract.InventoryEntry, error) {
	return d.entries, d.err
}

func (d *listDriver) Ensure(context.Context, []runnercontract.EnsureObject) error { return nil }

// The line a successful collection logs is the exact text an operator reads, with the object
// count, including zero for an empty namespace.
func TestInventoryLineIsExact(t *testing.T) {
	for _, c := range []struct {
		name    string
		entries []runnercontract.InventoryEntry
		want    string
	}{
		{"three objects", []runnercontract.InventoryEntry{{Name: "a"}, {Name: "b"}, {Name: "c"}}, "Read the job namespace's inventory: 3 objects"},
		{"empty namespace", nil, "Read the job namespace's inventory: 0 objects"},
	} {
		t.Run(c.name, func(t *testing.T) {
			api := newFakeAPI(t, runnercontract.CapabilityJobToken, runnercontract.CapabilityClaimRecovery)
			// The executable's own startup function routes the client's log to the logger it is
			// given, so the line reaches the executable's log, not only a test hook.
			var mu sync.Mutex
			var logged []string
			logf := func(format string, args ...any) {
				mu.Lock()
				defer mu.Unlock()
				logged = append(logged, fmt.Sprintf(format, args...))
			}
			lines := func() []string {
				mu.Lock()
				defer mu.Unlock()
				return slices.Clone(logged)
			}
			l := startedLauncher(t, api, &listDriver{fakeDriver: newFakeDriver("kubernetes"), entries: c.entries}, nil, logf)
			if inv := l.inventoryDue(t.Context()); inv == nil {
				t.Fatal("a due read returned nil, want the collected objects")
			}
			if got := lines(); len(got) != 1 || got[0] != c.want {
				t.Fatalf("log = %q, want exactly %q", got, c.want)
			}
		})
	}
}

// A failed collection logs its error and no inventory line of any count.
func TestFailedCollectionLogsNoInventoryLine(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken, runnercontract.CapabilityClaimRecovery)
	l := New(testConfig(api), &listDriver{fakeDriver: newFakeDriver("kubernetes"), err: fmt.Errorf("forbidden")})
	lines := logLines(l)
	if inv := l.inventoryDue(t.Context()); inv != nil {
		t.Fatalf("a failed read = %+v, want none", inv)
	}
	if got := lines(); anyInventoryLines(got) != 0 {
		t.Fatalf("a failed read logged an inventory line: %q", got)
	}
}

// Each read of the job namespace's inventory is logged once with its object count, so a test can
// tell a fresh report from a resent one; a read that is not due logs nothing.
func TestInventoryReadIsLoggedOncePerRead(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken, runnercontract.CapabilityClaimRecovery)
	l := New(testConfig(api), &objectDriver{fakeDriver: newFakeDriver("kubernetes")})
	lines := logLines(l)

	if inv := l.inventoryDue(t.Context()); len(inv) != 1 {
		t.Fatalf("first read = %+v, want the one object", inv)
	}
	if got := lines(); inventoryReads(got) != 1 {
		t.Fatalf("after a due read the log is %q, want one %q", got, fmt.Sprintf(InventoryReadMessage, 1))
	}
	if inv := l.inventoryDue(t.Context()); inv != nil {
		t.Fatalf("a read inside the interval = %+v, want none", inv)
	}
	if got := lines(); anyInventoryLines(got) != 1 {
		t.Errorf("a read inside the interval logged: %q", got)
	}
}

// A heartbeat resent after its response was lost carries the same inventory without reading the
// namespace again, so it writes no second line.
func TestResentHeartbeatLogsNoInventoryRead(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken, runnercontract.CapabilityClaimRecovery)
	d := &objectDriver{fakeDriver: newFakeDriver("kubernetes"), ensured: make(chan []runnercontract.EnsureObject, 4), gate: make(chan struct{})}
	l := New(testConfig(api), d)
	l.goos = "linux"
	lines := logLines(l)
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	go func() { _ = l.Run(ctx) }()

	// Only a launcher ready to place work long-polls and resends a lost heartbeat, so the first
	// successful read waits for readiness; its heartbeat then loses its response once.
	eventually(t, "the launcher ready to place work", l.startupReady.Load)
	api.mu.Lock()
	api.failHeartbeats = 1
	api.inventoryReports = 0
	api.mu.Unlock()
	close(d.gate)

	eventually(t, "the inventory taken in twice, once from the resend", func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		return api.inventoryReports >= 2
	})
	if got := lines(); inventoryReads(got) != 1 || anyInventoryLines(got) != 1 {
		t.Errorf("the log after a resent heartbeat = %q, want one inventory read", got)
	}
}

// A launcher whose driver manages long-lived objects reports its job namespace's inventory on a
// heartbeat and creates the objects the API asks for (#933, D8, AC49b).
func TestLauncherReportsInventoryAndEnsuresObjects(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken, runnercontract.CapabilityClaimRecovery)
	api.ensure = []runnercontract.EnsureObject{{Kind: "ServiceAccount", Name: "sw-deploy", PoolID: "pool-1"}}
	d := &objectDriver{fakeDriver: newFakeDriver("kubernetes"), ensured: make(chan []runnercontract.EnsureObject, 4)}
	l := New(testConfig(api), d)
	l.goos = "linux"
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	go func() { _ = l.Run(ctx) }()

	select {
	case objs := <-d.ensured:
		if len(objs) != 1 || objs[0].Name != "sw-deploy" {
			t.Errorf("ensured %+v", objs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the launcher never ensured the objects the API asked for")
	}
	eventually(t, "the inventory reported", func() bool {
		api.mu.Lock()
		defer api.mu.Unlock()
		return len(api.inventory) == 1 && api.inventory[0].Name == "sw-argo"
	})
}
