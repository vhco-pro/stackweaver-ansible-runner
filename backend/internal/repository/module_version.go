// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type ModuleVersionRepository struct {
	db *gorm.DB
}

func NewModuleVersionRepository(db *gorm.DB) *ModuleVersionRepository {
	return &ModuleVersionRepository{db: db}
}

func (r *ModuleVersionRepository) Create(version *models.ModuleVersion) error {
	return r.db.Create(version).Error
}

func (r *ModuleVersionRepository) GetByID(id uuid.UUID) (*models.ModuleVersion, error) {
	var version models.ModuleVersion
	err := r.db.Preload("Module").First(&version, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &version, nil
}

func (r *ModuleVersionRepository) GetByModuleAndVersion(moduleID uuid.UUID, version string) (*models.ModuleVersion, error) {
	var versionModel models.ModuleVersion
	err := r.db.Preload("Module").
		Where("module_id = ? AND version = ?", moduleID, version).
		First(&versionModel).Error
	if err != nil {
		return nil, err
	}
	return &versionModel, nil
}

func (r *ModuleVersionRepository) ListByModule(moduleID uuid.UUID) ([]models.ModuleVersion, error) {
	var versions []models.ModuleVersion
	err := r.db.Where("module_id = ?", moduleID).
		Order("published_at DESC").
		Find(&versions).Error
	return versions, err
}

func (r *ModuleVersionRepository) GetLatest(moduleID uuid.UUID) (*models.ModuleVersion, error) {
	var version models.ModuleVersion
	err := r.db.Preload("Module").
		Where("module_id = ?", moduleID).
		Order("published_at DESC").
		First(&version).Error
	if err != nil {
		return nil, err
	}
	return &version, nil
}

func (r *ModuleVersionRepository) Exists(moduleID uuid.UUID, version string) bool {
	var count int64
	r.db.Model(&models.ModuleVersion{}).
		Where("module_id = ? AND version = ?", moduleID, version).
		Count(&count)
	return count > 0
}

func (r *ModuleVersionRepository) IncrementDownloads(id uuid.UUID) error {
	return r.db.Model(&models.ModuleVersion{}).
		Where("id = ?", id).
		UpdateColumn("downloads", gorm.Expr("downloads + 1")).Error
}

func (r *ModuleVersionRepository) Delete(id uuid.UUID) error {
	return r.db.Delete(&models.ModuleVersion{}, "id = ?", id).Error
}
