// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// What the platform runner prepares for a job whose credential set names a credential of another
// organization (#915). The API refuses to store such a set, but a job row written before it did
// may hold one, and the runner is where the credential is decrypted: it must not be.
//
// The defect this test was written against: the runner listed the identifiers in the job's
// credential set and decrypted each one, whatever organization it belonged to, so the other
// organization's secret became an environment variable of the job.
//
// This is the queue-mode half. A self-hosted runner gets its credentials from the API's artifacts
// endpoint instead; see TestRunnerJobArtifacts_CredentialOfAnotherOrganization in
// internal/api/v2/handlers.
//
// The test needs real rows, so it is gated behind `integration` and skips unless
// $TEST_DATABASE_URL is set. Cleanup is scoped to the fixture's own rows. Run with:
//
//	go test -tags integration ./cmd/ansible-runner/ -run TestPrepareCredentials_

//go:build integration

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/michielvha/stackweaver/core/crypto"
	"github.com/michielvha/stackweaver/core/models"
	"github.com/michielvha/stackweaver/core/repository"
	"github.com/michielvha/stackweaver/core/services/ansible"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestPrepareCredentials_CredentialOfAnotherOrganization(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set - skipping integration test")
	}
	db, err := gorm.Open(postgres.Open(dbURL), &gorm.Config{})
	if err != nil {
		t.Fatalf("connecting to the test database: %v", err)
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	cryptoService, err := crypto.NewCryptoService(key)
	if err != nil {
		t.Fatalf("creating the crypto service: %v", err)
	}
	encrypt := func(plaintext string) string {
		t.Helper()
		encrypted, err := cryptoService.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("encrypting %q: %v", plaintext, err)
		}
		return encrypted
	}
	sfx := uuid.NewString()[:8]
	const otherSecret, ownSecret = "aws-secret-of-the-other-organization", "aws-secret-of-its-own-organization"

	org := &models.Organization{ID: uuid.New(), Name: "jobcred-a-" + sfx}
	otherOrg := &models.Organization{ID: uuid.New(), Name: "jobcred-b-" + sfx}
	project := &models.Project{ID: uuid.New(), OrganizationID: org.ID, Name: "jobcred-proj-" + sfx}
	inventory := &models.AnsibleInventory{ID: uuid.New(), OrganizationID: org.ID, Name: "jobcred-inv-" + sfx, Type: models.InventoryTypeStatic}
	own := &models.AnsibleCredential{
		ID: uuid.New(), OrganizationID: org.ID, Name: "jobcred-aws-" + sfx, Type: models.CredentialTypeAWSAccessKey,
		AWSAccessKeyID: encrypt("AKIAOWNORGANIZATION"), AWSSecretAccessKey: encrypt(ownSecret),
	}
	other := &models.AnsibleCredential{
		ID: uuid.New(), OrganizationID: otherOrg.ID, Name: "jobcred-aws-" + sfx, Type: models.CredentialTypeAWSAccessKey,
		AWSAccessKeyID: encrypt("AKIAOTHERORGANIZATION"), AWSSecretAccessKey: encrypt(otherSecret),
	}
	job := func(name string) *models.AnsibleJob {
		return &models.AnsibleJob{
			ID: uuid.New(), ProjectID: project.ID, InventoryID: inventory.ID, Name: name + sfx,
			JobType: models.AnsibleJobTypeAdHoc, Module: "ping", Status: models.AnsibleJobStatusPending,
		}
	}
	ownJob, storedEarlier := job("jobcred-own-"), job("jobcred-stored-")
	for _, row := range []any{org, otherOrg, project, inventory, own, other, ownJob, storedEarlier} {
		if err := db.Create(row).Error; err != nil {
			t.Fatalf("seeding %T: %v", row, err)
		}
	}
	jobIDs := []uuid.UUID{ownJob.ID, storedEarlier.ID}
	t.Cleanup(func() {
		// Children before parents (foreign-key order), each statement scoped to this run's rows.
		db.Exec("DELETE FROM ansible_job_credentials WHERE ansible_job_id IN ?", jobIDs)
		db.Where("id IN ?", jobIDs).Delete(&models.AnsibleJob{})
		db.Where("id IN ?", []uuid.UUID{own.ID, other.ID}).Delete(&models.AnsibleCredential{})
		db.Where("id = ?", inventory.ID).Delete(&models.AnsibleInventory{})
		db.Where("id = ?", project.ID).Delete(&models.Project{})
		db.Where("id IN ?", []uuid.UUID{org.ID, otherOrg.ID}).Delete(&models.Organization{})
	})
	// Written directly: the credential set of a job row stored before a launch checked it.
	attach := func(jobID, credentialID uuid.UUID) {
		t.Helper()
		if err := db.Exec("INSERT INTO ansible_job_credentials (ansible_job_id, ansible_credential_id) VALUES (?, ?)", jobID, credentialID).Error; err != nil {
			t.Fatalf("writing the job credential row: %v", err)
		}
	}
	attach(ownJob.ID, own.ID)
	attach(storedEarlier.ID, own.ID)
	attach(storedEarlier.ID, other.ID)

	credentialRepo := repository.NewAnsibleCredentialRepository(db)
	runner := &AnsibleRunner{
		jobRepo:           repository.NewAnsibleJobRepository(db),
		credentialService: ansible.NewCredentialService(credentialRepo, key),
	}

	t.Run("a job holding a credential of another organization gets no credentials", func(t *testing.T) {
		prep, err := runner.prepareCredentials(t.Context(), t.TempDir(), storedEarlier)
		if err == nil {
			t.Errorf("prepareCredentials succeeded, want a refusal")
		}
		if err != nil && (strings.Contains(err.Error(), other.ID.String()) || strings.Contains(err.Error(), other.Name)) {
			t.Errorf("the refusal names the other organization's credential: %v", err)
		}
		if prep != nil {
			for name, value := range prep.envVars {
				if strings.Contains(value, otherSecret) {
					t.Errorf("%s carries the other organization's secret", name)
				}
			}
		}
	})
	t.Run("a job holding its organization's credential gets it", func(t *testing.T) {
		prep, err := runner.prepareCredentials(t.Context(), t.TempDir(), ownJob)
		if err != nil {
			t.Fatalf("prepareCredentials: %v", err)
		}
		if got := prep.envVars["AWS_SECRET_ACCESS_KEY"]; got != ownSecret {
			t.Errorf("AWS_SECRET_ACCESS_KEY = %q, want its own organization's secret", got)
		}
	})
}
