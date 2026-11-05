// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// What a constructed inventory rebuild leaves behind, on the launcher path (#933, D2): the API
// builds the command, the sync executor runs it, and the API persists what it reports. These cases
// moved here from the platform runner's queue path, which ran the build with its own database,
// with the same expectations (#886, #909, #932):
//
//   - a source ansible-inventory cannot parse fails the build, which prunes nothing;
//   - a build that no longer derives a host or a group removes both;
//   - a constructed inventory is derived-only: a rebuild replaces every row;
//   - the rules a user wrote cannot read the launcher's secrets.
//
// ansible-inventory is a stand-in on PATH that fails the way ansible-core does: only when the
// unparsed-source setting is on. The test needs real rows, so it is gated behind `integration`
// and skips unless $TEST_DATABASE_URL is set. Cleanup is scoped to the fixture's own rows.

//go:build integration

package syncexec

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/michielvha/stackweaver/backend/internal/executor"
	"github.com/michielvha/stackweaver/core/dbtest"
	"github.com/michielvha/stackweaver/core/models"
	"github.com/michielvha/stackweaver/core/repository"
	"github.com/michielvha/stackweaver/core/runnercontract"
	"github.com/michielvha/stackweaver/core/services/ansible"
	"gorm.io/gorm"
)

// constructedFixture is a constructed inventory with one input inventory holding one host. The
// constructed inventory already carries what an earlier build derived: that host and a group it
// was placed in by a rule.
type constructedFixture struct {
	db           *gorm.DB
	inventories  *repository.AnsibleInventoryRepository
	export       *ansible.InventoryService
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
	if err := models.AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	sfx := uuid.NewString()[:8]
	inventories := repository.NewAnsibleInventoryRepository(db)

	org := &models.Organization{ID: uuid.New(), Name: "constructed-org-" + sfx}
	hosted := models.NewHostedPool(org.ID)
	input := &models.AnsibleInventory{ID: uuid.New(), OrganizationID: org.ID, Name: "constructed-input-" + sfx, Type: models.InventoryTypeStatic}
	constructed := &models.AnsibleInventory{
		ID: uuid.New(), OrganizationID: org.ID, Name: "constructed-derived-" + sfx, Type: models.InventoryTypeConstructed,
		SourceVars: "groups:\n  prod: env == 'prod'\n",
	}
	for _, row := range []any{org, hosted, input, constructed} {
		if err := db.Create(row).Error; err != nil {
			t.Fatalf("seeding %T: %v", row, err)
		}
	}
	t.Cleanup(func() {
		db.Where("organization_id = ?", org.ID).Delete(&models.WorkItem{})
		for _, id := range []uuid.UUID{constructed.ID, input.ID} {
			if err := inventories.Delete(id); err != nil {
				t.Errorf("removing inventory %s: %v", id, err)
			}
		}
		db.Where("id = ?", hosted.ID).Delete(&models.AgentPool{})
		db.Where("id = ?", org.ID).Delete(&models.Organization{})
	})

	f := &constructedFixture{
		db: db, inventories: inventories, constructed: constructed, hostName: "constructed-web-" + sfx,
		export: ansible.NewInventoryService(inventories, repository.NewOrganizationRepository(db)),
	}
	if err := inventories.SetConstructedInputs(constructed.ID, []uuid.UUID{input.ID}); err != nil {
		t.Fatalf("linking the input inventory: %v", err)
	}
	inputHost := &models.AnsibleInventoryHost{InventoryID: input.ID, Name: f.hostName, Enabled: true, Variables: models.InventoryVariables{"env": "prod"}}
	f.derivedHost = &models.AnsibleInventoryHost{InventoryID: constructed.ID, Name: f.hostName, Enabled: true, Variables: models.InventoryVariables{"env": "prod"}}
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

// rebuild runs one rebuild of the constructed inventory end to end: the work item is requested and
// claimed, the API side builds its artifacts, the sync executor runs them against a stand-in for
// the runner routes, and the API side persists the completion.
func (f *constructedFixture) rebuild(t *testing.T) {
	t.Helper()
	items := ansible.NewWorkItemService(f.db, nil)
	item, err := items.RequestInventorySync(t.Context(), f.constructed, "manual")
	if err != nil {
		t.Fatalf("requesting the rebuild: %v", err)
	}
	rt := ansible.NewWorkItemRuntime(f.db, f.export, nil)
	if err := rt.Started(item); err != nil {
		t.Fatalf("starting the rebuild: %v", err)
	}
	cmd, err := rt.Command(item)
	if err != nil {
		t.Fatalf("building the rebuild's command: %v", err)
	}
	arts := runnercontract.WorkItemArtifacts{ContractVersion: runnercontract.Version, JobID: item.ID, Kind: item.Kind, Inventory: cmd}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/artifacts"):
			_ = json.NewEncoder(w).Encode(arts)
		case strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"status":"running"}`))
		case strings.HasSuffix(r.URL.Path, "/output"):
			var req runnercontract.JobOutputRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if err := rt.AppendOutput(item, req.Output); err != nil {
				t.Errorf("appending output: %v", err)
			}
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case strings.HasSuffix(r.URL.Path, "/complete"):
			var req runnercontract.JobCompleteRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			status := models.WorkItemStatusFailed
			if req.Status == "completed" {
				status = models.WorkItemStatusCompleted
			}
			if _, err := rt.Finished(item, status, req.ErrorMessage, req.WorkResult); err != nil {
				t.Errorf("finishing the rebuild: %v", err)
			}
			_, _ = w.Write([]byte(`{"status":"completed"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	Execute(t.Context(), executor.NewClient(srv.URL, "job-token", "runner"), item.ID, Options{WorkRoot: t.TempDir()})
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

func TestConstructedBuild_RowsNoLongerDerivedAreRemoved(t *testing.T) {
	f := setupConstructedFixture(t)
	stale := &models.AnsibleInventoryHost{InventoryID: f.constructed.ID, Name: "constructed-gone-" + uuid.NewString()[:8], Enabled: true}
	if err := f.inventories.CreateHost(stale); err != nil {
		t.Fatal(err)
	}
	fakeAnsibleInventory(t, "#!/bin/sh\ncat <<'INVENTORY'\n"+f.inputsOnlyOutput()+"\nINVENTORY\n")

	f.rebuild(t)

	if n := f.count(t, "ansible_inventory_groups", f.derivedGroup.ID); n != 0 {
		t.Errorf("the group the rules no longer derive: %d rows, want 0", n)
	}
	if n := f.count(t, "ansible_inventory_hosts", stale.ID); n != 0 {
		t.Errorf("the host no input has any more: %d rows, want 0", n)
	}
	if n := f.count(t, "ansible_inventory_hosts", f.derivedHost.ID); n != 1 {
		t.Errorf("the host the input still has: %d rows, want 1", n)
	}
	if n := f.memberships(t); n != 0 {
		t.Errorf("the host's membership of the derived group: %d rows, want 0", n)
	}
	if inventory := f.storedInventory(t); inventory.LastSyncStatus != "successful" {
		t.Errorf("inventory last_sync_status = %q (%s), want %q", inventory.LastSyncStatus, inventory.LastSyncError, "successful")
	}
	if build := f.onlyBuild(t); build.Status != models.InventorySyncStatusSuccessful {
		t.Errorf("build status = %q, want %q", build.Status, models.InventorySyncStatusSuccessful)
	}
}

// A constructed inventory is derived-only (owner decision 2026-10-16, #909): the service refuses a
// hand-made group, and one left over from before the rule is removed by the next rebuild.
func TestConstructedBuild_IsDerivedOnly(t *testing.T) {
	f := setupConstructedFixture(t)
	if _, err := f.export.CreateGroup(f.constructed.ID, "handmade-"+uuid.NewString()[:8], "added by a person", nil, nil); !errors.Is(err, ansible.ErrConstructedInventory) {
		t.Fatalf("creating a group by hand in a constructed inventory: %v, want ErrConstructedInventory", err)
	}
	leftOver := &models.AnsibleInventoryGroup{InventoryID: f.constructed.ID, Name: "handmade-" + uuid.NewString()[:8], Description: "added by a person before the rule"}
	if err := f.inventories.CreateGroup(leftOver); err != nil {
		t.Fatal(err)
	}
	fakeAnsibleInventory(t, "#!/bin/sh\ncat <<'INVENTORY'\n"+f.inputsOnlyOutput()+"\nINVENTORY\n")

	f.rebuild(t)

	if n := f.count(t, "ansible_inventory_groups", leftOver.ID); n != 0 {
		t.Errorf("the group left over from before the rule: %d rows, want 0 (a rebuild replaces the whole set)", n)
	}
	if n := f.count(t, "ansible_inventory_hosts", f.derivedHost.ID); n != 1 {
		t.Errorf("the host the input still has: %d rows, want 1", n)
	}
}

// The constructed rules a user wrote run with the inherited allowlist, never the launcher's
// secrets (#932).
func TestConstructedBuild_RulesCannotReadRunnerSecrets(t *testing.T) {
	f := setupConstructedFixture(t)
	secrets := map[string]string{
		"DATABASE_PASSWORD": "placeholder-database-password", "STACKWEAVER_TOKEN": "placeholder-agent-token",
		"STACKWEAVER_HOSTED_SECRET": "placeholder-hosted-secret", "SOME_FUTURE_PLATFORM_KEY": "placeholder-future-secret",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}
	record := filepath.Join(t.TempDir(), "env")
	fakeAnsibleInventory(t, "#!/bin/sh\nenv > \""+record+"\"\ncat <<'INVENTORY'\n"+f.inputsOnlyOutput()+"\nINVENTORY\n")

	f.rebuild(t)

	seen, err := os.ReadFile(record) //nolint:gosec // G304: a file this test's own stand-in wrote
	if err != nil {
		t.Fatalf("the stand-in ansible-inventory recorded no environment: %v", err)
	}
	for name, value := range secrets {
		if strings.Contains(string(seen), name+"=") || strings.Contains(string(seen), value) {
			t.Errorf("the constructed rules can read the launcher's %s", name)
		}
	}
}
