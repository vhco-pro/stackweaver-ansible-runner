// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type ProviderVersionRepository struct {
	db *gorm.DB
}

func NewProviderVersionRepository(db *gorm.DB) *ProviderVersionRepository {
	return &ProviderVersionRepository{db: db}
}

func (r *ProviderVersionRepository) Create(version *models.ProviderVersion) error {
	return r.db.Create(version).Error
}

func (r *ProviderVersionRepository) GetByID(id uuid.UUID) (*models.ProviderVersion, error) {
	var version models.ProviderVersion
	err := r.db.Preload("Provider").Preload("Platforms").
		First(&version, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &version, nil
}

func (r *ProviderVersionRepository) GetByProviderAndVersion(providerID uuid.UUID, version string) (*models.ProviderVersion, error) {
	var versionModel models.ProviderVersion
	err := r.db.Preload("Provider").Preload("Platforms").
		Where("provider_id = ? AND version = ?", providerID, version).
		First(&versionModel).Error
	if err != nil {
		return nil, err
	}
	return &versionModel, nil
}

func (r *ProviderVersionRepository) ListByProvider(providerID uuid.UUID) ([]models.ProviderVersion, error) {
	var versions []models.ProviderVersion
	err := r.db.Preload("Platforms").
		Where("provider_id = ?", providerID).
		Order("version DESC").
		Find(&versions).Error
	return versions, err
}

func (r *ProviderVersionRepository) GetLatest(providerID uuid.UUID) (*models.ProviderVersion, error) {
	var version models.ProviderVersion
	err := r.db.Preload("Platforms").
		Where("provider_id = ?", providerID).
		Order("version DESC").
		First(&version).Error
	if err != nil {
		return nil, err
	}
	return &version, nil
}

func (r *ProviderVersionRepository) Exists(providerID uuid.UUID, version string) bool {
	var count int64
	r.db.Model(&models.ProviderVersion{}).
		Where("provider_id = ? AND version = ?", providerID, version).
		Count(&count)
	return count > 0
}

func (r *ProviderVersionRepository) IncrementDownloads(id uuid.UUID) error {
	return r.db.Model(&models.ProviderVersion{}).
		Where("id = ?", id).
		UpdateColumn("downloads", gorm.Expr("downloads + 1")).Error
}

func (r *ProviderVersionRepository) Delete(id uuid.UUID) error {
	return r.db.Delete(&models.ProviderVersion{}, "id = ?", id).Error
}
