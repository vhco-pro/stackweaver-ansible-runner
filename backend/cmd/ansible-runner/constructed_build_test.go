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

	// Asserted on the membership and not on the group row: processSyncJob saves the inventory it
	// loaded before the build, hosts and groups included, which writes a pruned group back as an
	// empty one. That is a separate defect from the one this file is about.
	if n := f.memberships(t); n != 0 {
		t.Errorf("the host's membership of the derived group: %d rows, want 0 (a build replaces the materialized set)", n)
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
