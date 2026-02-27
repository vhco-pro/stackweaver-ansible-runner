// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type ModuleDownloadRepository struct {
	db *gorm.DB
}

func NewModuleDownloadRepository(db *gorm.DB) *ModuleDownloadRepository {
	return &ModuleDownloadRepository{db: db}
}

func (r *ModuleDownloadRepository) Create(download *models.ModuleDownload) error {
	return r.db.Create(download).Error
}

func (r *ModuleDownloadRepository) GetDownloadsByVersion(moduleVersionID uuid.UUID, limit, offset int) ([]models.ModuleDownload, int64, error) {
	var downloads []models.ModuleDownload
	var total int64

	if err := r.db.Model(&models.ModuleDownload{}).
		Where("module_version_id = ?", moduleVersionID).
		Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("module_version_id = ?", moduleVersionID).
		Order("downloaded_at DESC").
		Limit(limit).Offset(offset).
		Find(&downloads).Error

	return downloads, total, err
}

func (r *ModuleDownloadRepository) GetDownloadStats(moduleVersionID uuid.UUID) (map[string]interface{}, error) {
	var total int64
	var week int64
	var month int64
	var year int64

	// Total downloads
	if err := r.db.Model(&models.ModuleDownload{}).
		Where("module_version_id = ?", moduleVersionID).
		Count(&total).Error; err != nil {
		return nil, err
	}

	// Week (last 7 days)
	weekAgo := r.db.NowFunc().AddDate(0, 0, -7)
	if err := r.db.Model(&models.ModuleDownload{}).
		Where("module_version_id = ? AND downloaded_at >= ?", moduleVersionID, weekAgo).
		Count(&week).Error; err != nil {
		return nil, err
	}

	// Month (last 30 days)
	monthAgo := r.db.NowFunc().AddDate(0, 0, -30)
	if err := r.db.Model(&models.ModuleDownload{}).
		Where("module_version_id = ? AND downloaded_at >= ?", moduleVersionID, monthAgo).
		Count(&month).Error; err != nil {
		return nil, err
	}

	// Year (last 365 days)
	yearAgo := r.db.NowFunc().AddDate(0, 0, -365)
	if err := r.db.Model(&models.ModuleDownload{}).
		Where("module_version_id = ? AND downloaded_at >= ?", moduleVersionID, yearAgo).
		Count(&year).Error; err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"week":  week,
		"month": month,
		"year":  year,
		"total": total,
	}, nil
}
