// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type StateVersionRepository struct {
	db *gorm.DB
}

func NewStateVersionRepository(db *gorm.DB) *StateVersionRepository {
	return &StateVersionRepository{db: db}
}

func (r *StateVersionRepository) Create(version *models.StateVersion) error {
	return r.db.Create(version).Error
}

func (r *StateVersionRepository) GetByID(id string) (*models.StateVersion, error) {
	var version models.StateVersion
	err := r.db.First(&version, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &version, nil
}

func (r *StateVersionRepository) GetLatest(workspaceID string) (*models.StateVersion, error) {
	var version models.StateVersion
	err := r.db.Where("workspace_id = ?", workspaceID).
		Order("version DESC").
		First(&version).Error
	if err != nil {
		return nil, err
	}
	return &version, nil
}

func (r *StateVersionRepository) GetByVersion(workspaceID string, version int) (*models.StateVersion, error) {
	var stateVersion models.StateVersion
	err := r.db.First(&stateVersion, "workspace_id = ? AND version = ?", workspaceID, version).Error
	if err != nil {
		return nil, err
	}
	return &stateVersion, nil
}

func (r *StateVersionRepository) ListByWorkspace(workspaceID string, limit, offset int) ([]models.StateVersion, int64, error) {
	var versions []models.StateVersion
	var total int64

	if err := r.db.Model(&models.StateVersion{}).Where("workspace_id = ?", workspaceID).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("workspace_id = ?", workspaceID).
		Order("version DESC").
		Limit(limit).
		Offset(offset).
		Find(&versions).Error
	return versions, total, err
}

func (r *StateVersionRepository) GetNextVersion(workspaceID string) (int, error) {
	var maxVersion int
	err := r.db.Model(&models.StateVersion{}).
		Where("workspace_id = ?", workspaceID).
		Select("COALESCE(MAX(version), 0)").
		Scan(&maxVersion).Error
	if err != nil {
		return 0, err
	}
	return maxVersion + 1, nil
}

// GetByRunID returns the state version created by the given run (run_id = runID).
// Returns nil when no state version exists for that run.
func (r *StateVersionRepository) GetByRunID(runID string) (*models.StateVersion, error) {
	var version models.StateVersion
	err := r.db.Where("run_id = ?", runID).First(&version).Error
	if err != nil {
		return nil, err
	}
	return &version, nil
}

func (r *StateVersionRepository) Delete(id string) error {
	return r.db.Delete(&models.StateVersion{}, "id = ?", id).Error
}
