// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

// AnsibleInventorySourceRepository handles inventory source database operations
type AnsibleInventorySourceRepository struct {
	db *gorm.DB
}

// NewAnsibleInventorySourceRepository creates a new inventory source repository
func NewAnsibleInventorySourceRepository(db *gorm.DB) *AnsibleInventorySourceRepository {
	return &AnsibleInventorySourceRepository{db: db}
}

// Create creates a new inventory source
func (r *AnsibleInventorySourceRepository) Create(source *models.AnsibleInventorySource) error {
	return r.db.Create(source).Error
}

// GetByID retrieves an inventory source by ID
func (r *AnsibleInventorySourceRepository) GetByID(id uuid.UUID) (*models.AnsibleInventorySource, error) {
	var source models.AnsibleInventorySource
	err := r.db.Preload("Inventory").Preload("Credential").
		First(&source, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &source, nil
}

// ListByInventory lists all inventory sources for an inventory
func (r *AnsibleInventorySourceRepository) ListByInventory(inventoryID uuid.UUID, limit, offset int) ([]models.AnsibleInventorySource, int64, error) {
	var sources []models.AnsibleInventorySource
	var total int64

	if err := r.db.Model(&models.AnsibleInventorySource{}).
		Where("inventory_id = ?", inventoryID).
		Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("inventory_id = ?", inventoryID).
		Preload("Credential").
		Order("name ASC").
		Limit(limit).
		Offset(offset).
		Find(&sources).Error
	return sources, total, err
}

// ListByType lists inventory sources by type across all inventories
func (r *AnsibleInventorySourceRepository) ListByType(sourceType models.InventorySourceType, limit, offset int) ([]models.AnsibleInventorySource, int64, error) {
	var sources []models.AnsibleInventorySource
	var total int64

	if err := r.db.Model(&models.AnsibleInventorySource{}).
		Where("type = ?", sourceType).
		Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("type = ?", sourceType).
		Preload("Inventory").
		Preload("Credential").
		Order("name ASC").
		Limit(limit).
		Offset(offset).
		Find(&sources).Error
	return sources, total, err
}

// ListEnabled lists all enabled inventory sources
func (r *AnsibleInventorySourceRepository) ListEnabled() ([]models.AnsibleInventorySource, error) {
	var sources []models.AnsibleInventorySource
	err := r.db.Where("enabled = ?", true).
		Preload("Inventory").
		Preload("Credential").
		Find(&sources).Error
	return sources, err
}

// ListByStatus lists inventory sources by sync status
func (r *AnsibleInventorySourceRepository) ListByStatus(status models.InventorySourceStatus) ([]models.AnsibleInventorySource, error) {
	var sources []models.AnsibleInventorySource
	err := r.db.Where("status = ?", status).
		Preload("Inventory").
		Preload("Credential").
		Find(&sources).Error
	return sources, err
}

// ListNeedingSync lists inventory sources that need synchronization
// (either never synced or cache has expired)
func (r *AnsibleInventorySourceRepository) ListNeedingSync() ([]models.AnsibleInventorySource, error) {
	var sources []models.AnsibleInventorySource
	err := r.db.Where("enabled = ? AND (status = ? OR (update_cache_timeout > 0 AND last_sync_at < NOW() - INTERVAL '1 minute' * update_cache_timeout))",
		true, models.InventorySourceStatusNeverSynced).
		Preload("Inventory").
		Preload("Credential").
		Find(&sources).Error
	return sources, err
}

// Update updates an inventory source
func (r *AnsibleInventorySourceRepository) Update(source *models.AnsibleInventorySource) error {
	return r.db.Save(source).Error
}

// UpdateSyncStatus updates the sync status of an inventory source
func (r *AnsibleInventorySourceRepository) UpdateSyncStatus(id uuid.UUID, status models.InventorySourceStatus, errorMsg string, hostsCount int, lastSyncLog ...string) error {
	syncLog := ""
	if len(lastSyncLog) > 0 {
		syncLog = lastSyncLog[0]
	}
	updates := map[string]interface{}{
		"status":          status,
		"last_sync_at":    gorm.Expr("NOW()"),
		"last_sync_error": errorMsg,
		"last_sync_log":   syncLog,
		"hosts_count":     hostsCount,
	}
	return r.db.Model(&models.AnsibleInventorySource{}).
		Where("id = ?", id).
		Updates(updates).Error
}

// Delete deletes an inventory source
func (r *AnsibleInventorySourceRepository) Delete(id uuid.UUID) error {
	return r.db.Delete(&models.AnsibleInventorySource{}, "id = ?", id).Error
}

// DeleteByInventory deletes all inventory sources for an inventory
func (r *AnsibleInventorySourceRepository) DeleteByInventory(inventoryID uuid.UUID) error {
	return r.db.Delete(&models.AnsibleInventorySource{}, "inventory_id = ?", inventoryID).Error
}

// GetByInventoryAndName gets an inventory source by inventory ID and name
func (r *AnsibleInventorySourceRepository) GetByInventoryAndName(inventoryID uuid.UUID, name string) (*models.AnsibleInventorySource, error) {
	var source models.AnsibleInventorySource
	err := r.db.First(&source, "inventory_id = ? AND name = ?", inventoryID, name).Error
	if err != nil {
		return nil, err
	}
	return &source, nil
}
