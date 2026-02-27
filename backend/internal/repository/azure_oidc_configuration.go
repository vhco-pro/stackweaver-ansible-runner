// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

// AzureOIDCConfigurationRepository handles CRUD operations for Azure OIDC configurations.
type AzureOIDCConfigurationRepository struct {
	db *gorm.DB
}

// NewAzureOIDCConfigurationRepository creates a new AzureOIDCConfigurationRepository.
func NewAzureOIDCConfigurationRepository(db *gorm.DB) *AzureOIDCConfigurationRepository {
	return &AzureOIDCConfigurationRepository{db: db}
}

// Create creates a new Azure OIDC configuration.
func (r *AzureOIDCConfigurationRepository) Create(config *models.AzureOIDCConfiguration) error {
	return r.db.Create(config).Error
}

// GetByID retrieves an Azure OIDC configuration by ID, preloading the Organization.
func (r *AzureOIDCConfigurationRepository) GetByID(id string) (*models.AzureOIDCConfiguration, error) {
	var config models.AzureOIDCConfiguration
	err := r.db.Preload("Organization").First(&config, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &config, nil
}

// GetByOrganization retrieves all Azure OIDC configurations for a given organization.
func (r *AzureOIDCConfigurationRepository) GetByOrganization(orgID uuid.UUID) ([]models.AzureOIDCConfiguration, error) {
	var configs []models.AzureOIDCConfiguration
	err := r.db.Preload("Organization").Where("organization_id = ?", orgID).Find(&configs).Error
	return configs, err
}

// Update updates specific fields of an Azure OIDC configuration.
func (r *AzureOIDCConfigurationRepository) Update(id string, updates map[string]interface{}) (*models.AzureOIDCConfiguration, error) {
	if err := r.db.Model(&models.AzureOIDCConfiguration{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		return nil, err
	}
	return r.GetByID(id)
}

// Delete deletes an Azure OIDC configuration by ID.
func (r *AzureOIDCConfigurationRepository) Delete(id string) error {
	return r.db.Delete(&models.AzureOIDCConfiguration{}, "id = ?", id).Error
}
