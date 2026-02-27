// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type ConfigurationVersionRepository struct {
	db *gorm.DB
}

func NewConfigurationVersionRepository(db *gorm.DB) *ConfigurationVersionRepository {
	return &ConfigurationVersionRepository{db: db}
}

func (r *ConfigurationVersionRepository) Create(cv *models.ConfigurationVersion) error {
	return r.db.Create(cv).Error
}

func (r *ConfigurationVersionRepository) GetByID(id string) (*models.ConfigurationVersion, error) {
	var cv models.ConfigurationVersion
	err := r.db.Where("id = ?", id).First(&cv).Error
	if err != nil {
		return nil, err
	}
	return &cv, nil
}

func (r *ConfigurationVersionRepository) GetByWorkspaceID(workspaceID string) ([]models.ConfigurationVersion, error) {
	var cvs []models.ConfigurationVersion
	err := r.db.Where("workspace_id = ?", workspaceID).
		Order("created_at DESC").
		Find(&cvs).Error
	return cvs, err
}

func (r *ConfigurationVersionRepository) GetLatestByWorkspaceID(workspaceID string) (*models.ConfigurationVersion, error) {
	var cv models.ConfigurationVersion
	err := r.db.Where("workspace_id = ?", workspaceID).
		Order("created_at DESC").
		First(&cv).Error
	if err != nil {
		return nil, err
	}
	return &cv, nil
}

func (r *ConfigurationVersionRepository) Update(cv *models.ConfigurationVersion) error {
	return r.db.Save(cv).Error
}

func (r *ConfigurationVersionRepository) UpdateStatus(id string, status models.ConfigurationVersionStatus) error {
	return r.db.Model(&models.ConfigurationVersion{}).
		Where("id = ?", id).
		Update("status", status).Error
}
