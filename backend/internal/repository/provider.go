// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type ProviderRepository struct {
	db *gorm.DB
}

func NewProviderRepository(db *gorm.DB) *ProviderRepository {
	return &ProviderRepository{db: db}
}

func (r *ProviderRepository) Create(provider *models.Provider) error {
	return r.db.Create(provider).Error
}

func (r *ProviderRepository) GetByID(id uuid.UUID) (*models.Provider, error) {
	var provider models.Provider
	err := r.db.Preload("Organization").Preload("Versions").First(&provider, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &provider, nil
}

func (r *ProviderRepository) GetByOrganizationAndName(organizationID uuid.UUID, name string) (*models.Provider, error) {
	var provider models.Provider
	err := r.db.Preload("Organization").Preload("Versions").
		Where("organization_id = ? AND name = ?", organizationID, name).
		First(&provider).Error
	if err != nil {
		return nil, err
	}
	return &provider, nil
}

func (r *ProviderRepository) List(organizationID *uuid.UUID, verified *bool, limit, offset int) ([]models.Provider, int64, error) {
	var providers []models.Provider
	var total int64

	query := r.db.Model(&models.Provider{})

	if organizationID != nil {
		query = query.Where("organization_id = ?", *organizationID)
	}

	if verified != nil {
		query = query.Where("verified = ?", *verified)
	}

	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := query.Preload("Organization").Preload("Versions").
		Order("created_at DESC").
		Limit(limit).Offset(offset).
		Find(&providers).Error

	return providers, total, err
}

func (r *ProviderRepository) Search(query string, organizationID *uuid.UUID, verified *bool, limit, offset int) ([]models.Provider, int64, error) {
	var providers []models.Provider
	var total int64

	dbQuery := r.db.Model(&models.Provider{}).
		Where("name ILIKE ? OR description ILIKE ?", "%"+query+"%", "%"+query+"%")

	if organizationID != nil {
		dbQuery = dbQuery.Where("organization_id = ?", *organizationID)
	}

	if verified != nil {
		dbQuery = dbQuery.Where("verified = ?", *verified)
	}

	if err := dbQuery.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := dbQuery.Preload("Organization").Preload("Versions").
		Order("created_at DESC").
		Limit(limit).Offset(offset).
		Find(&providers).Error

	return providers, total, err
}

func (r *ProviderRepository) Update(provider *models.Provider) error {
	return r.db.Save(provider).Error
}

func (r *ProviderRepository) Delete(id uuid.UUID) error {
	return r.db.Delete(&models.Provider{}, "id = ?", id).Error
}
