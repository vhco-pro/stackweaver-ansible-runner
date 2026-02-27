// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type ProviderPlatformRepository struct {
	db *gorm.DB
}

func NewProviderPlatformRepository(db *gorm.DB) *ProviderPlatformRepository {
	return &ProviderPlatformRepository{db: db}
}

func (r *ProviderPlatformRepository) Create(platform *models.ProviderPlatform) error {
	return r.db.Create(platform).Error
}

func (r *ProviderPlatformRepository) GetByID(id uuid.UUID) (*models.ProviderPlatform, error) {
	var platform models.ProviderPlatform
	err := r.db.Preload("ProviderVersion").First(&platform, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &platform, nil
}

func (r *ProviderPlatformRepository) GetByVersionAndPlatform(providerVersionID uuid.UUID, os, arch string) (*models.ProviderPlatform, error) {
	var platform models.ProviderPlatform
	err := r.db.Preload("ProviderVersion").
		Where("provider_version_id = ? AND os = ? AND arch = ?", providerVersionID, os, arch).
		First(&platform).Error
	if err != nil {
		return nil, err
	}
	return &platform, nil
}

func (r *ProviderPlatformRepository) ListByVersion(providerVersionID uuid.UUID) ([]models.ProviderPlatform, error) {
	var platforms []models.ProviderPlatform
	err := r.db.Where("provider_version_id = ?", providerVersionID).
		Order("os, arch").
		Find(&platforms).Error
	return platforms, err
}

func (r *ProviderPlatformRepository) Delete(id uuid.UUID) error {
	return r.db.Delete(&models.ProviderPlatform{}, "id = ?", id).Error
}
