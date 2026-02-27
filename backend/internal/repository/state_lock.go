// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"time"

	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type StateLockRepository struct {
	db *gorm.DB
}

func NewStateLockRepository(db *gorm.DB) *StateLockRepository {
	return &StateLockRepository{db: db}
}

func (r *StateLockRepository) Create(lock *models.StateLock) error {
	return r.db.Create(lock).Error
}

func (r *StateLockRepository) GetByWorkspaceAndLockID(workspaceID string, lockID string) (*models.StateLock, error) {
	var lock models.StateLock
	err := r.db.First(&lock, "workspace_id = ? AND lock_id = ?", workspaceID, lockID).Error
	if err != nil {
		return nil, err
	}
	return &lock, nil
}

// GetByWorkspace returns the most recent active (non-expired) lock for a workspace
func (r *StateLockRepository) GetByWorkspace(workspaceID string) (*models.StateLock, error) {
	var lock models.StateLock
	err := r.db.Where("workspace_id = ? AND expires_at > ?", workspaceID, time.Now()).
		Order("created_at DESC").
		First(&lock).Error
	if err != nil {
		return nil, err
	}
	return &lock, nil
}

func (r *StateLockRepository) Delete(workspaceID string, lockID string) error {
	return r.db.Delete(&models.StateLock{}, "workspace_id = ? AND lock_id = ?", workspaceID, lockID).Error
}

func (r *StateLockRepository) DeleteExpired() error {
	return r.db.Where("expires_at < ?", time.Now()).Delete(&models.StateLock{}).Error
}

func (r *StateLockRepository) CleanupExpiredLocks() error {
	return r.DeleteExpired()
}
