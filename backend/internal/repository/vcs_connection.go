// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type VCSConnectionRepository struct {
	db *gorm.DB
}

func NewVCSConnectionRepository(db *gorm.DB) *VCSConnectionRepository {
	return &VCSConnectionRepository{db: db}
}

func (r *VCSConnectionRepository) Create(connection *models.VCSConnection) error {
	return r.db.Create(connection).Error
}

func (r *VCSConnectionRepository) GetByID(id uuid.UUID) (*models.VCSConnection, error) {
	var connection models.VCSConnection
	err := r.db.Preload("Organization").First(&connection, "id = ?", id).Error
	return &connection, err
}

func (r *VCSConnectionRepository) ListByOrganization(organizationID uuid.UUID) ([]models.VCSConnection, error) {
	var connections []models.VCSConnection
	err := r.db.Where("organization_id = ?", organizationID).Order("created_at DESC").Find(&connections).Error
	return connections, err
}

func (r *VCSConnectionRepository) GetByOrganizationAndProvider(organizationID uuid.UUID, provider models.VCSProvider) (*models.VCSConnection, error) {
	var connection models.VCSConnection
	err := r.db.Where("organization_id = ? AND provider = ?", organizationID, provider).First(&connection).Error
	return &connection, err
}

func (r *VCSConnectionRepository) Update(connection *models.VCSConnection) error {
	return r.db.Save(connection).Error
}

func (r *VCSConnectionRepository) Delete(id uuid.UUID) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		// Detach workspaces referencing this VCS connection
		if err := tx.Model(&models.Workspace{}).
			Where("vcs_connection_id = ?", id).
			Update("vcs_connection_id", gorm.Expr("NULL")).Error; err != nil {
			return err
		}

		// Detach modules referencing this VCS connection
		if err := tx.Model(&models.Module{}).
			Where("vcs_connection_id = ?", id).
			Update("vcs_connection_id", gorm.Expr("NULL")).Error; err != nil {
			return err
		}

		// Finally delete the VCS connection
		if err := tx.Delete(&models.VCSConnection{}, "id = ?", id).Error; err != nil {
			return err
		}

		return nil
	})
}

func (r *VCSConnectionRepository) DeleteByOrganization(organizationID uuid.UUID) error {
	return r.db.Delete(&models.VCSConnection{}, "organization_id = ?", organizationID).Error
}

func (r *VCSConnectionRepository) ListByProvider(provider models.VCSProvider) ([]models.VCSConnection, error) {
	var connections []models.VCSConnection
	err := r.db.Where("provider = ?", provider).Find(&connections).Error
	return connections, err
}

func (r *VCSConnectionRepository) GetByInstallationID(installationID string) (*models.VCSConnection, error) {
	var connection models.VCSConnection
	err := r.db.Where("installation_id = ?", installationID).First(&connection).Error
	return &connection, err
}

// GetByInstallationIDAndOrganization returns the VCS connection for the given GitHub App installation ID
// that belongs to the given organization. Use this when creating/updating workspaces so the connection
// is validated for the workspace's org.
func (r *VCSConnectionRepository) GetByInstallationIDAndOrganization(installationID string, organizationID uuid.UUID) (*models.VCSConnection, error) {
	var connection models.VCSConnection
	err := r.db.Where("installation_id = ? AND organization_id = ?", installationID, organizationID).First(&connection).Error
	return &connection, err
}
