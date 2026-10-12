// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// What the platform runner clones with when the row it works on names a VCS connection of another
// organization (#916). The API refuses to store such an identifier, but a row written before it
// did may hold one, and the runner is where the clone URL is built from the database when the API
// did not send one: it must not be built from another organization's token.
//
// The defect this test was written against: the runner loaded the connection by identifier alone
// and embedded its token in the clone URL, whatever organization the connection belonged to.
//
// This is the queue-mode half. A self-hosted runner gets its clone URL from the API's artifacts
// endpoint instead; see TestRunnerArtifacts_VCSConnectionOfAnotherOrganization in
// internal/api/v2/handlers.
//
// The test needs real rows, so it is gated behind `integration` and skips unless
// $TEST_DATABASE_URL is set. Cleanup is scoped to the fixture's own rows. Run with:
//
//	go test -tags integration ./cmd/ansible-runner/ -run TestCloneURL_

//go:build integration

package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/michielvha/stackweaver/core/models"
	"github.com/michielvha/stackweaver/core/repository"
	"github.com/michielvha/stackweaver/core/services/ansible"
	"github.com/michielvha/stackweaver/core/services/vcs"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// The two tokens are distinct strings, so that a clone URL shows whose connection built it.
// GitLab is the provider whose clone URL is built from the stored token with no network.
const (
	ownVCSToken   = "token-of-its-own-organization"
	otherVCSToken = "token-of-the-other-organization"
)

// cloneScopeFixture is two organizations with a VCS connection each, and a runner over them.
type cloneScopeFixture struct {
	db         *gorm.DB
	runner     *AnsibleRunner
	org        *models.Organization
	project    *models.Project
	own, other *models.VCSConnection
}

func setupCloneScope(t *testing.T) *cloneScopeFixture {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set - skipping integration test")
	}
	db, err := gorm.Open(postgres.Open(dbURL), &gorm.Config{})
	if err != nil {
		t.Fatalf("connecting to the test database: %v", err)
	}
	if err := db.AutoMigrate(
		&models.Organization{}, &models.Project{}, &models.VCSConnection{},
		&models.AnsiblePlaybook{}, &models.AnsibleInventory{},
	); err != nil {
		t.Fatalf("migrating the tables: %v", err)
	}
	sfx := uuid.NewString()[:8]
	org := &models.Organization{ID: uuid.New(), Name: "clonescope-a-" + sfx}
	otherOrg := &models.Organization{ID: uuid.New(), Name: "clonescope-b-" + sfx}
	project := &models.Project{ID: uuid.New(), OrganizationID: org.ID, Name: "clonescope-proj-" + sfx}
	connection := func(organizationID uuid.UUID, token string) *models.VCSConnection {
		return &models.VCSConnection{
			ID: uuid.New(), OrganizationID: organizationID, Provider: models.VCSProviderGitLab,
			AccessToken: token, AccountName: "clonescope-" + sfx,
		}
	}
	own, other := connection(org.ID, ownVCSToken), connection(otherOrg.ID, otherVCSToken)
	for _, row := range []any{org, otherOrg, project, own, other} {
		if err := db.Create(row).Error; err != nil {
			t.Fatalf("seeding %T: %v", row, err)
		}
	}
	connectionIDs := []uuid.UUID{own.ID, other.ID}
	t.Cleanup(func() {
		// Children before parents (foreign-key order), each statement scoped to this run's rows.
		db.Where("vcs_connection_id IN ?", connectionIDs).Delete(&models.AnsiblePlaybook{})
		db.Where("vcs_connection_id IN ?", connectionIDs).Delete(&models.AnsibleInventory{})
		db.Where("id IN ?", connectionIDs).Delete(&models.VCSConnection{})
		db.Where("id = ?", project.ID).Delete(&models.Project{})
		db.Where("id IN ?", []uuid.UUID{org.ID, otherOrg.ID}).Delete(&models.Organization{})
	})
	return &cloneScopeFixture{
		db: db, org: org, project: project, own: own, other: other,
		runner: &AnsibleRunner{
			vcsConnectionRepo: repository.NewVCSConnectionRepository(db),
			vcsRegistry:       vcs.NewProviderRegistry(nil, nil, nil, nil),
		},
	}
}

// assertNoForeignClone requires a refusal that is the shared "not found" answer and names
// nothing of the other organization.
func (f *cloneScopeFixture) assertNoForeignClone(t *testing.T, cloneURL string, err error) {
	t.Helper()
	if cloneURL != "" {
		t.Errorf("a clone URL was built for a row naming another organization's connection")
	}
	if strings.Contains(cloneURL, otherVCSToken) {
		t.Errorf("the clone URL carries the other organization's token")
	}
	if !errors.Is(err, ansible.ErrInvalidInput) || !strings.HasSuffix(err.Error(), "VCS connection not found") {
		t.Errorf("err = %v, want the same refusal as for a connection that does not exist", err)
	}
	if err != nil && strings.Contains(err.Error(), f.other.ID.String()) {
		t.Errorf("the refusal names the other organization's connection: %v", err)
	}
}

func TestCloneURL_PlaybookConnectionOfAnotherOrganization(t *testing.T) {
	f := setupCloneScope(t)
	// Written directly: the shape of a playbook row stored before the API checked the connection.
	stored := func(connection *models.VCSConnection) *models.AnsiblePlaybook {
		t.Helper()
		playbook := &models.AnsiblePlaybook{
			ID: uuid.New(), ProjectID: f.project.ID, Name: "clonescope-" + uuid.NewString()[:8],
			PlaybookPath: "site.yml", SourceMode: models.PlaybookSourceModeFresh,
			VCSConnectionID: &connection.ID, VCSRepository: "acme/playbooks", VCSBranch: "main",
		}
		if err := f.db.Create(playbook).Error; err != nil {
			t.Fatalf("writing the playbook row: %v", err)
		}
		return playbook
	}

	cloneURL, err := f.runner.playbookCloneURL(t.Context(), stored(f.other))
	f.assertNoForeignClone(t, cloneURL, err)

	cloneURL, err = f.runner.playbookCloneURL(t.Context(), stored(f.own))
	if err != nil {
		t.Fatalf("resolving the clone URL of a playbook naming its organization's connection: %v", err)
	}
	if !strings.Contains(cloneURL, ownVCSToken) {
		t.Errorf("clone URL %q is not built with the organization's own token", cloneURL)
	}
}

func TestCloneURL_InventoryConnectionOfAnotherOrganization(t *testing.T) {
	f := setupCloneScope(t)
	// Written directly: the shape of an inventory row stored before the API checked the connection.
	stored := func(connection *models.VCSConnection) *models.AnsibleInventory {
		t.Helper()
		inventory := &models.AnsibleInventory{
			ID: uuid.New(), OrganizationID: f.org.ID, Name: "clonescope-" + uuid.NewString()[:8], Type: models.InventoryTypeVCS,
			VCSConnectionID: &connection.ID, VCSRepository: "acme/inventories", VCSBranch: "main", InventoryPath: "hosts.yml",
		}
		if err := f.db.Create(inventory).Error; err != nil {
			t.Fatalf("writing the inventory row: %v", err)
		}
		return inventory
	}

	cloneURL, err := f.runner.inventoryCloneURL(t.Context(), stored(f.other))
	f.assertNoForeignClone(t, cloneURL, err)

	cloneURL, err = f.runner.inventoryCloneURL(t.Context(), stored(f.own))
	if err != nil {
		t.Fatalf("resolving the clone URL of an inventory naming its organization's connection: %v", err)
	}
	if !strings.Contains(cloneURL, ownVCSToken) {
		t.Errorf("clone URL %q is not built with the organization's own token", cloneURL)
	}
}
