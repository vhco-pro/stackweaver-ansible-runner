// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type ProviderDownloadRepository struct {
	db *gorm.DB
}

func NewProviderDownloadRepository(db *gorm.DB) *ProviderDownloadRepository {
	return &ProviderDownloadRepository{db: db}
}

func (r *ProviderDownloadRepository) Create(download *models.ProviderDownload) error {
	return r.db.Create(download).Error
}

func (r *ProviderDownloadRepository) GetDownloadsByPlatform(providerPlatformID uuid.UUID, limit, offset int) ([]models.ProviderDownload, int64, error) {
	var downloads []models.ProviderDownload
	var total int64

	if err := r.db.Model(&models.ProviderDownload{}).
		Where("provider_platform_id = ?", providerPlatformID).
		Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("provider_platform_id = ?", providerPlatformID).
		Order("downloaded_at DESC").
		Limit(limit).Offset(offset).
		Find(&downloads).Error

	return downloads, total, err
}

func (r *ProviderDownloadRepository) GetDownloadStats(providerPlatformID uuid.UUID) (map[string]interface{}, error) {
	var total int64
	var week int64
	var month int64
	var year int64

	// Total downloads
	if err := r.db.Model(&models.ProviderDownload{}).
		Where("provider_platform_id = ?", providerPlatformID).
		Count(&total).Error; err != nil {
		return nil, err
	}

	// Week (last 7 days)
	weekAgo := r.db.NowFunc().AddDate(0, 0, -7)
	if err := r.db.Model(&models.ProviderDownload{}).
		Where("provider_platform_id = ? AND downloaded_at >= ?", providerPlatformID, weekAgo).
		Count(&week).Error; err != nil {
		return nil, err
	}

	// Month (last 30 days)
	monthAgo := r.db.NowFunc().AddDate(0, 0, -30)
	if err := r.db.Model(&models.ProviderDownload{}).
		Where("provider_platform_id = ? AND downloaded_at >= ?", providerPlatformID, monthAgo).
		Count(&month).Error; err != nil {
		return nil, err
	}

	// Year (last 365 days)
	yearAgo := r.db.NowFunc().AddDate(0, 0, -365)
	if err := r.db.Model(&models.ProviderDownload{}).
		Where("provider_platform_id = ? AND downloaded_at >= ?", providerPlatformID, yearAgo).
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
