// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type AnsibleCredentialRepository struct {
	db *gorm.DB
}

func NewAnsibleCredentialRepository(db *gorm.DB) *AnsibleCredentialRepository {
	return &AnsibleCredentialRepository{db: db}
}

func (r *AnsibleCredentialRepository) Create(credential *models.AnsibleCredential) error {
	return r.db.Create(credential).Error
}

func (r *AnsibleCredentialRepository) GetByID(id uuid.UUID) (*models.AnsibleCredential, error) {
	var credential models.AnsibleCredential
	err := r.db.Preload("Organization").Preload("Project").First(&credential, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &credential, nil
}

func (r *AnsibleCredentialRepository) GetByOrganizationAndName(orgID uuid.UUID, name string) (*models.AnsibleCredential, error) {
	var credential models.AnsibleCredential
	err := r.db.First(&credential, "organization_id = ? AND name = ?", orgID, name).Error
	if err != nil {
		return nil, err
	}
	return &credential, nil
}

func (r *AnsibleCredentialRepository) ListByOrganization(orgID uuid.UUID, limit, offset int) ([]models.AnsibleCredential, int64, error) {
	var credentials []models.AnsibleCredential
	var total int64

	if err := r.db.Model(&models.AnsibleCredential{}).Where("organization_id = ?", orgID).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("organization_id = ?", orgID).
		Order("name ASC").
		Limit(limit).
		Offset(offset).
		Find(&credentials).Error
	return credentials, total, err
}

func (r *AnsibleCredentialRepository) ListByProject(projectID uuid.UUID, limit, offset int) ([]models.AnsibleCredential, int64, error) {
	var credentials []models.AnsibleCredential
	var total int64

	if err := r.db.Model(&models.AnsibleCredential{}).Where("project_id = ?", projectID).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("project_id = ?", projectID).
		Order("name ASC").
		Limit(limit).
		Offset(offset).
		Find(&credentials).Error
	return credentials, total, err
}

func (r *AnsibleCredentialRepository) ListByOrganizationAndType(orgID uuid.UUID, credType models.CredentialType, limit, offset int) ([]models.AnsibleCredential, int64, error) {
	var credentials []models.AnsibleCredential
	var total int64

	query := r.db.Model(&models.AnsibleCredential{}).
		Where("organization_id = ? AND type = ?", orgID, credType)

	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := query.
		Order("name ASC").
		Limit(limit).
		Offset(offset).
		Find(&credentials).Error
	return credentials, total, err
}

func (r *AnsibleCredentialRepository) Update(credential *models.AnsibleCredential) error {
	return r.db.Save(credential).Error
}

func (r *AnsibleCredentialRepository) Delete(id uuid.UUID) error {
	return r.db.Delete(&models.AnsibleCredential{}, "id = ?", id).Error
}

// GetWithSecrets retrieves a credential with all encrypted fields populated
// This should only be used internally for job execution, never for API responses
func (r *AnsibleCredentialRepository) GetWithSecrets(id uuid.UUID) (*models.AnsibleCredential, error) {
	var credential models.AnsibleCredential
	err := r.db.First(&credential, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &credential, nil
}
