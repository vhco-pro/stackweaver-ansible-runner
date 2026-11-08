// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// End to end for a dynamic inventory source sync (#932): the stand-in ansible-inventory plays the
// inventory plugin a user configured and records everything it can read. Every trigger of a source
// sync is a work item that this executor runs on a launcher (#885), so the environment the plugin
// gets is the one this executor builds. The test was written against the API's own sync, which ran
// the engine in the API process and is gone. It needs real rows, so it is gated behind
// `integration` and skips unless $TEST_DATABASE_URL is set. Run with:
//
//	go test -tags integration ./internal/executor/syncexec/ -run TestSyncInventorySource_PluginCannotReadRunnerSecrets

//go:build integration

package syncexec

import (
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

// runnerSecrets are variables a launcher may hold that an inventory plugin must never read.
var runnerSecrets = map[string]string{
	"DATABASE_PASSWORD":          "placeholder-database-password",
	"REDIS_PASSWORD":             "placeholder-redis-password",
	"STORAGE_SECRET_KEY":         "placeholder-storage-secret",
	"ENCRYPTION_KEY":             "placeholder-encryption-key",
	"ANSIBLE_ENCRYPTION_KEY":     "placeholder-ansible-encryption-key",
	"OIDC_SIGNING_KEY":           "placeholder-signing-key",
	"AZURE_DEVOPS_CLIENT_SECRET": "placeholder-azure-devops-secret",
	"STACKWEAVER_TOKEN":          "placeholder-agent-token",
	"STACKWEAVER_HOSTED_SECRET":  "placeholder-hosted-secret",
	"SOME_FUTURE_PLATFORM_KEY":   "placeholder-future-secret",
}

func TestSyncInventorySource_PluginCannotReadRunnerSecrets(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set - skipping integration test")
	}
	db, err := dbtest.Open(t, dbURL, &gorm.Config{})
	if err != nil {
		t.Fatalf("connecting to the test database: %v", err)
	}
	if err := models.AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	sfx := uuid.NewString()[:8]
	org := &models.Organization{ID: uuid.New(), Name: "srcenv-org-" + sfx}
	hosted := models.NewHostedPool(org.ID)
	inventory := &models.AnsibleInventory{ID: uuid.New(), OrganizationID: org.ID, Name: "srcenv-inv-" + sfx, Type: models.InventoryTypeStatic}
	// A VMware source needs no credential, so the engine is reached with nothing attached.
	source := &models.AnsibleInventorySource{
		ID: uuid.New(), InventoryID: inventory.ID, Name: "srcenv-src-" + sfx, Type: models.InventorySourceTypeVMware,
		Config: models.InventorySourceConfig{"hostname": "vcenter.invalid"}, Enabled: true,
	}
	for _, row := range []any{org, hosted, inventory, source} {
		if err := db.Create(row).Error; err != nil {
			t.Fatalf("seeding %T: %v", row, err)
		}
	}
	t.Cleanup(func() {
		db.Where("organization_id = ?", org.ID).Delete(&models.WorkItem{})
		db.Where("inventory_id = ?", inventory.ID).Delete(&models.AnsibleInventorySync{})
		db.Where("id = ?", source.ID).Delete(&models.AnsibleInventorySource{})
		db.Where("id = ?", inventory.ID).Delete(&models.AnsibleInventory{})
		db.Where("id = ?", hosted.ID).Delete(&models.AgentPool{})
		db.Where("id = ?", org.ID).Delete(&models.Organization{})
	})

	for name, value := range runnerSecrets {
		t.Setenv(name, value)
	}
	record := filepath.Join(t.TempDir(), "env")
	fakeAnsibleInventory(t, `#!/bin/sh
env > "`+record+`"
printf '%s' '{"_meta": {"hostvars": {}}, "all": {"children": ["ungrouped"]}}'
`)

	item, err := ansible.NewWorkItemService(db, nil).RequestSourceSync(t.Context(), source, "manual")
	if err != nil {
		t.Fatalf("requesting the sync: %v", err)
	}
	sources := ansible.NewInventorySourceService(
		repository.NewAnsibleInventorySourceRepository(db), repository.NewAnsibleInventoryRepository(db),
		repository.NewAnsibleCredentialRepository(db), nil,
	)
	executeWorkItem(t, ansible.NewWorkItemRuntime(db, nil, sources), item)

	seen, err := os.ReadFile(record) //nolint:gosec // G304: a file this test's own stand-in wrote
	if err != nil {
		t.Fatalf("the stand-in ansible-inventory recorded no environment: %v", err)
	}
	for name, value := range runnerSecrets {
		if strings.Contains(string(seen), name+"=") || strings.Contains(string(seen), value) {
			t.Errorf("the inventory plugin of a dynamic source sync can read the launcher's %s", name)
		}
	}
	var stored models.WorkItem
	if err := db.First(&stored, "id = ?", item.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != models.WorkItemStatusCompleted {
		t.Errorf("the sync work item is %q (%s), want completed", stored.Status, stored.ErrorMessage)
	}
}
