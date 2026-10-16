// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// What a constructed inventory build leaves behind when ansible-inventory could not parse one of
// its sources (#886). A build passes every input inventory and the generated constructed.yml as
// separate sources and then prunes whatever the output does not name. The engine's default is to
// skip a source it cannot parse and print the rest, so rules the constructed plugin rejects used
// to yield the input hosts with no derived group at all, recorded as a successful build that
// pruned every derived group. With the shared engine environment such a run exits non-zero, the
// build is recorded as failed and nothing is pruned.
//
// ansible-inventory is a stand-in on PATH that fails the way ansible-core does: only when the
// unparsed-source setting is on. What the real engine does with two sources is checked by
// runner-images/ansible/tests/. The test needs real rows, so it is gated behind `integration`
// and skips unless $TEST_DATABASE_URL is set. Cleanup is scoped to the fixture's own rows.
// Run with:
//
//	go test -tags integration ./cmd/ansible-runner/ -run TestConstructedBuild_

//go:build integration

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/michielvha/stackweaver/core/dbtest"
	"github.com/michielvha/stackweaver/core/models"
	"github.com/michielvha/stackweaver/core/repository"
	"github.com/michielvha/stackweaver/core/services/ansible"
	"gorm.io/gorm"
)

// constructedFixture is a constructed inventory with one input inventory holding one host. The
// constructed inventory already carries what an earlier build derived: that host and a group it
// was placed in by a rule.
type constructedFixture struct {
	db           *gorm.DB
	runner       *AnsibleRunner
	constructed  *models.AnsibleInventory
	hostName     string
	derivedHost  *models.AnsibleInventoryHost
	derivedGroup *models.AnsibleInventoryGroup
}

func setupConstructedFixture(t *testing.T) *constructedFixture {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set - skipping integration test")
	}
	db, err := dbtest.Open(t, dbURL, &gorm.Config{})
	if err != nil {
		t.Fatalf("connecting to the test database: %v", err)
	}
	sfx := uuid.NewString()[:8]
	inventories := repository.NewAnsibleInventoryRepository(db)

	org := &models.Organization{ID: uuid.New(), Name: "constructed-org-" + sfx}
	input := &models.AnsibleInventory{ID: uuid.New(), OrganizationID: org.ID, Name: "constructed-input-" + sfx, Type: models.InventoryTypeStatic}
	constructed := &models.AnsibleInventory{
		ID: uuid.New(), OrganizationID: org.ID, Name: "constructed-derived-" + sfx, Type: models.InventoryTypeConstructed,
		SourceVars: "groups:\n  prod: env == 'prod'\n",
	}
	for _, row := range []any{org, input, constructed} {
		if err := db.Create(row).Error; err != nil {
			t.Fatalf("seeding %T: %v", row, err)
		}
	}
	t.Cleanup(func() {
		// The product's own inventory delete: hosts, groups, memberships, sync history and input
		// links of exactly these two inventories, then the rows themselves.
		for _, id := range []uuid.UUID{constructed.ID, input.ID} {
			if err := inventories.Delete(id); err != nil {
				t.Errorf("removing inventory %s: %v", id, err)
			}
		}
		db.Where("id = ?", org.ID).Delete(&models.Organization{})
	})

	f := &constructedFixture{db: db, constructed: constructed, hostName: "constructed-web-" + sfx}
	if err := inventories.SetConstructedInputs(constructed.ID, []uuid.UUID{input.ID}); err != nil {
		t.Fatalf("linking the input inventory: %v", err)
	}
	inputHost := &models.AnsibleInventoryHost{
		InventoryID: input.ID, Name: f.hostName, Enabled: true, Variables: models.InventoryVariables{"env": "prod"},
	}
	f.derivedHost = &models.AnsibleInventoryHost{
		InventoryID: constructed.ID, Name: f.hostName, Enabled: true, Variables: models.InventoryVariables{"env": "prod"},
	}
	f.derivedGroup = &models.AnsibleInventoryGroup{InventoryID: constructed.ID, Name: "prod"}
	for _, host := range []*models.AnsibleInventoryHost{inputHost, f.derivedHost} {
		if err := inventories.CreateHost(host); err != nil {
			t.Fatalf("seeding host %s: %v", host.Name, err)
		}
	}
	if err := inventories.CreateGroup(f.derivedGroup); err != nil {
		t.Fatalf("seeding the derived group: %v", err)
	}
	if err := inventories.AddHostGroupMembership(f.derivedHost.ID, f.derivedGroup.ID); err != nil {
		t.Fatalf("seeding the membership: %v", err)
	}

	f.runner = &AnsibleRunner{
		inventoryRepo:    inventories,
		inventoryService: ansible.NewInventoryService(inventories, repository.NewOrganizationRepository(db)),
		syncRepo:         repository.NewAnsibleInventorySyncRepository(db),
	}
	return f
}

// fakeAnsibleInventory puts script first on PATH as ansible-inventory for the rest of the test.
func fakeAnsibleInventory(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ansible-inventory"), []byte(script), 0o700); err != nil { //nolint:gosec // an executable is the point
		t.Fatalf("writing the fake ansible-inventory: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// inputsOnlyOutput is what the engine prints when it parsed the input and skipped constructed.yml:
// the input's host, ungrouped, and no derived group.
func (f *constructedFixture) inputsOnlyOutput() string {
	return fmt.Sprintf(
		`{"_meta": {"hostvars": {%q: {"env": "prod"}}}, "all": {"children": ["ungrouped"]}, "ungrouped": {"hosts": [%q]}}`,
		f.hostName, f.hostName,
	)
}

// rebuild runs one queued rebuild of the constructed inventory, as the sync worker would.
func (f *constructedFixture) rebuild(t *testing.T) {
	t.Helper()
	message, err := json.Marshal(InventorySyncMessage{InventoryID: f.constructed.ID, TriggeredBy: "manual"})
	if err != nil {
		t.Fatalf("encoding the sync message: %v", err)
	}
	if err := f.runner.processSyncJob(t.Context(), message); err != nil {
		t.Fatalf("processSyncJob: %v", err)
	}
}

func (f *constructedFixture) count(t *testing.T, table string, id uuid.UUID) int64 {
	t.Helper()
	var n int64
	if err := f.db.Table(table).Where("id = ?", id).Count(&n).Error; err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}

func (f *constructedFixture) memberships(t *testing.T) int64 {
	t.Helper()
	var n int64
	if err := f.db.Table("ansible_inventory_host_groups").
		Where("ansible_inventory_host_id = ? AND ansible_inventory_group_id = ?", f.derivedHost.ID, f.derivedGroup.ID).
		Count(&n).Error; err != nil {
		t.Fatalf("counting memberships: %v", err)
	}
	return n
}

func (f *constructedFixture) storedInventory(t *testing.T) models.AnsibleInventory {
	t.Helper()
	var inventory models.AnsibleInventory
	if err := f.db.First(&inventory, "id = ?", f.constructed.ID).Error; err != nil {
		t.Fatalf("reading the constructed inventory: %v", err)
	}
	return inventory
}

func (f *constructedFixture) onlyBuild(t *testing.T) models.AnsibleInventorySync {
	t.Helper()
	var builds []models.AnsibleInventorySync
	if err := f.db.Where("inventory_id = ?", f.constructed.ID).Find(&builds).Error; err != nil {
		t.Fatalf("reading the sync history: %v", err)
	}
	if len(builds) != 1 {
		t.Fatalf("found %d sync history rows for the constructed inventory, want 1", len(builds))
	}
	return builds[0]
}

func TestConstructedBuild_RejectedRulesKeepDerivedRows(t *testing.T) {
	f := setupConstructedFixture(t)
	// ansible-core 2.21 with a constructed.yml its plugin rejects: the plugin's error is a
	// warning, and the run fails only when the unparsed-source setting is on.
	fakeAnsibleInventory(t, `#!/bin/sh
echo "[WARNING]: Failed to parse inventory with 'auto' plugin: Config 'groups' has an invalid value" >&2
if [ "$ANSIBLE_INVENTORY_ANY_UNPARSED_IS_FAILED" = "true" ]; then
	echo "[ERROR]: Completely failed to parse inventory source constructed.yml" >&2
	exit 1
fi
echo "[WARNING]: Unable to parse constructed.yml as an inventory source" >&2
cat <<'INVENTORY'
`+f.inputsOnlyOutput()+`
INVENTORY
`)

	f.rebuild(t)

	if n := f.count(t, "ansible_inventory_groups", f.derivedGroup.ID); n != 1 {
		t.Errorf("the derived group: %d rows, want 1 (a failed build prunes nothing)", n)
	}
	if n := f.count(t, "ansible_inventory_hosts", f.derivedHost.ID); n != 1 {
		t.Errorf("the derived host: %d rows, want 1 (a failed build prunes nothing)", n)
	}
	if n := f.memberships(t); n != 1 {
		t.Errorf("the host's membership of the derived group: %d rows, want 1", n)
	}

	const engineError = "Completely failed to parse inventory source"
	inventory := f.storedInventory(t)
	if inventory.LastSyncStatus != "failed" {
		t.Errorf("inventory last_sync_status = %q, want %q", inventory.LastSyncStatus, "failed")
	}
	if !strings.Contains(inventory.LastSyncError, engineError) {
		t.Errorf("inventory last_sync_error = %q, want it to carry %q", inventory.LastSyncError, engineError)
	}
	build := f.onlyBuild(t)
	if build.Status != models.InventorySyncStatusFailed {
		t.Errorf("build status = %q, want %q", build.Status, models.InventorySyncStatusFailed)
	}
	for _, want := range []string{"Config 'groups' has an invalid value", engineError} {
		if !strings.Contains(build.Output, want) {
			t.Errorf("build output = %q, want it to carry %q", build.Output, want)
		}
	}
}

// seedDerivedHost adds a host to the constructed inventory the way an earlier build left it: a
// row of that inventory with no dynamic source behind it.
func (f *constructedFixture) seedDerivedHost(t *testing.T, name string) *models.AnsibleInventoryHost {
	t.Helper()
	host := &models.AnsibleInventoryHost{InventoryID: f.constructed.ID, Name: name, Enabled: true}
	if err := f.runner.inventoryRepo.CreateHost(host); err != nil {
		t.Fatalf("seeding host %s: %v", name, err)
	}
	return host
}

// A build that no longer derives a host or a group removes both (#909). The defect this was
// written against: processSyncJob loaded the inventory with its hosts and groups, ran the build,
// which deleted the rows the output no longer names, and then saved the struct it had loaded
// before the build. GORM's Save upserts the loaded associations with it, so every row the build
// had just deleted was written back; only the memberships, which are not on that struct, stayed
// gone. A constructed inventory kept every host and group it had ever derived.
func TestConstructedBuild_RowsNoLongerDerivedAreRemoved(t *testing.T) {
	f := setupConstructedFixture(t)
	// A second host an earlier build derived, which no input has any more.
	staleHost := f.seedDerivedHost(t, "constructed-gone-"+uuid.NewString()[:8])
	fakeAnsibleInventory(t, `#!/bin/sh
cat <<'INVENTORY'
`+f.inputsOnlyOutput()+`
INVENTORY
`)

	f.rebuild(t)

	if n := f.count(t, "ansible_inventory_groups", f.derivedGroup.ID); n != 0 {
		t.Errorf("the group the rules no longer derive: %d rows, want 0", n)
	}
	if n := f.count(t, "ansible_inventory_hosts", staleHost.ID); n != 0 {
		t.Errorf("the host no input has any more: %d rows, want 0", n)
	}
	if n := f.count(t, "ansible_inventory_hosts", f.derivedHost.ID); n != 1 {
		t.Errorf("the host the input still has: %d rows, want 1", n)
	}
	if inventory := f.storedInventory(t); inventory.LastSyncStatus != "successful" {
		t.Errorf("inventory last_sync_status = %q (%s), want %q", inventory.LastSyncStatus, inventory.LastSyncError, "successful")
	}
}

// A constructed inventory is derived-only: its hosts and groups come from its build alone, as in
// AWX (owner decision 2026-10-16, #909). This test used to record, without endorsing it, that a
// group a person added by hand was removed by the next rebuild, because nothing on a row says
// whether a build derived it or a person added it (both have no source_id) and PruneUnseen
// removes every row the output does not name. The decision settles it the other way round from
// marking rows: a person cannot add one, so every row of a constructed inventory is the build's
// and the build may replace them all.
//
// Two halves. The inventory service, which every API route goes through, refuses the hand-made
// group with ErrConstructedInventory (the full list of refused writes is in
// core/services/ansible/inventory_derived_only_test.go). A group added before the rule existed,
// seeded here through the repository as such a row is stored, is removed by the next rebuild like
// any row the build no longer derives. That second half is the accepted consequence of the
// decision for inventories that already carry hand-made rows.
func TestConstructedBuild_IsDerivedOnly(t *testing.T) {
	f := setupConstructedFixture(t)

	_, err := f.runner.inventoryService.CreateGroup(f.constructed.ID, "handmade-"+uuid.NewString()[:8], "added by a person", nil, nil)
	if !errors.Is(err, ansible.ErrConstructedInventory) {
		t.Fatalf("creating a group by hand in a constructed inventory: %v, want ErrConstructedInventory", err)
	}

	leftOver := &models.AnsibleInventoryGroup{InventoryID: f.constructed.ID, Name: "handmade-" + uuid.NewString()[:8], Description: "added by a person before the rule"}
	if err := f.runner.inventoryRepo.CreateGroup(leftOver); err != nil {
		t.Fatalf("seeding a group left over from before the rule: %v", err)
	}
	fakeAnsibleInventory(t, `#!/bin/sh
cat <<'INVENTORY'
`+f.inputsOnlyOutput()+`
INVENTORY
`)

	f.rebuild(t)

	if n := f.count(t, "ansible_inventory_groups", leftOver.ID); n != 0 {
		t.Errorf("the group left over from before the rule: %d rows, want 0 (a rebuild replaces the whole set)", n)
	}
	if n := f.count(t, "ansible_inventory_hosts", f.derivedHost.ID); n != 1 {
		t.Errorf("the host the input still has: %d rows, want 1", n)
	}
}

func TestConstructedBuild_SuccessfulBuildStillReconciles(t *testing.T) {
	f := setupConstructedFixture(t)
	// Every source parsed, and the rules no longer place the host in a group: the same output as
	// above, but from a run that succeeded.
	fakeAnsibleInventory(t, `#!/bin/sh
cat <<'INVENTORY'
`+f.inputsOnlyOutput()+`
INVENTORY
`)

	f.rebuild(t)

	// This used to be asserted on the membership alone, with a note that the group row came back
	// empty because processSyncJob saved the inventory it had loaded before the build. That was
	// #909; the outcome is now recorded on the status columns only, so the group row is gone too.
	if n := f.memberships(t); n != 0 {
		t.Errorf("the host's membership of the derived group: %d rows, want 0 (a build replaces the materialized set)", n)
	}
	if n := f.count(t, "ansible_inventory_groups", f.derivedGroup.ID); n != 0 {
		t.Errorf("the derived group: %d rows, want 0 (the rules no longer derive it)", n)
	}
	if n := f.count(t, "ansible_inventory_hosts", f.derivedHost.ID); n != 1 {
		t.Errorf("the derived host: %d rows, want 1 (the input still has it)", n)
	}
	if inventory := f.storedInventory(t); inventory.LastSyncStatus != "successful" {
		t.Errorf("inventory last_sync_status = %q (%s), want %q", inventory.LastSyncStatus, inventory.LastSyncError, "successful")
	}
	if build := f.onlyBuild(t); build.Status != models.InventorySyncStatusSuccessful {
		t.Errorf("build status = %q, want %q", build.Status, models.InventorySyncStatusSuccessful)
	}
}
