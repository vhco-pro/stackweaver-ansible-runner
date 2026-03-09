// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type ModuleRepository struct {
	db *gorm.DB
}

func NewModuleRepository(db *gorm.DB) *ModuleRepository {
	return &ModuleRepository{db: db}
}

func (r *ModuleRepository) Create(module *models.Module) error {
	return r.db.Create(module).Error
}

func (r *ModuleRepository) GetByID(id uuid.UUID) (*models.Module, error) {
	var module models.Module
	err := r.db.Preload("Organization").Preload("Versions").Preload("VCSConnection").
		First(&module, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &module, nil
}

func (r *ModuleRepository) GetByOrganizationAndName(organizationID uuid.UUID, name, provider string) (*models.Module, error) {
	var module models.Module
	err := r.db.Preload("Organization").Preload("Versions").Preload("VCSConnection").
		Where("organization_id = ? AND name = ? AND provider = ?", organizationID, name, provider).
		First(&module).Error
	if err != nil {
		return nil, err
	}
	return &module, nil
}

func (r *ModuleRepository) List(organizationID *uuid.UUID, provider string, verified *bool, limit, offset int) ([]models.Module, int64, error) {
	var modules []models.Module
	var total int64

	query := r.db.Model(&models.Module{})

	if organizationID != nil {
		query = query.Where("organization_id = ?", *organizationID)
	}

	if provider != "" {
		query = query.Where("provider = ?", provider)
	}

	if verified != nil {
		query = query.Where("verified = ?", *verified)
	}

	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := query.Preload("Organization").Preload("Versions").Preload("VCSConnection").
		Order("created_at DESC").
		Limit(limit).Offset(offset).
		Find(&modules).Error

	return modules, total, err
}

func (r *ModuleRepository) Search(query string, organizationID *uuid.UUID, provider string, verified *bool, limit, offset int) ([]models.Module, int64, error) {
	var modules []models.Module
	var total int64

	dbQuery := r.db.Model(&models.Module{})

	// Search in name, description
	if query != "" {
		dbQuery = dbQuery.Where("name ILIKE ? OR description ILIKE ?", "%"+query+"%", "%"+query+"%")
	}

	if organizationID != nil {
		dbQuery = dbQuery.Where("organization_id = ?", *organizationID)
	}

	if provider != "" {
		dbQuery = dbQuery.Where("provider = ?", provider)
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
		Find(&modules).Error

	return modules, total, err
}

func (r *ModuleRepository) FindByVCSRepository(vcsRepository string) ([]models.Module, error) {
	var modules []models.Module
	err := r.db.Preload("Organization").Preload("Versions").
		Where("vcs_repository = ? AND auto_publish_tags = ?", vcsRepository, true).
		Find(&modules).Error
	return modules, err
}

func (r *ModuleRepository) Update(module *models.Module) error {
	return r.db.Save(module).Error
}

func (r *ModuleRepository) Delete(id uuid.UUID) error {
	return r.db.Delete(&models.Module{}, "id = ?", id).Error
}
