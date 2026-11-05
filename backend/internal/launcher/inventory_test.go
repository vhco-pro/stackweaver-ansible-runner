// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// objectDriver is a fake driver that manages long-lived objects, as the Kubernetes driver does.
type objectDriver struct {
	*fakeDriver
	ensured chan []runnercontract.EnsureObject
}

func (d *objectDriver) Inventory(context.Context) ([]runnercontract.InventoryEntry, error) {
	return []runnercontract.InventoryEntry{{Kind: "ServiceAccount", Namespace: "jobs", Name: "sw-argo", Present: true, ManagedBy: "external", ExternalManager: "Argo CD application platform"}}, nil
}

func (d *objectDriver) Ensure(_ context.Context, objs []runnercontract.EnsureObject) error {
	d.ensured <- objs
	return nil
}

// A launcher whose driver manages long-lived objects reports its job namespace's inventory on a
// heartbeat and creates the objects the API asks for (#933, D8, AC49b).
func TestLauncherReportsInventoryAndEnsuresObjects(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
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
