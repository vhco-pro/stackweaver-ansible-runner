// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type RunPhaseStateRepository struct {
	db *gorm.DB
}

func NewRunPhaseStateRepository(db *gorm.DB) *RunPhaseStateRepository {
	return &RunPhaseStateRepository{db: db}
}

func (r *RunPhaseStateRepository) Create(phaseState *models.RunPhaseState) error {
	return r.db.Create(phaseState).Error
}

func (r *RunPhaseStateRepository) GetByRunIDAndPhase(runID string, phase string) (*models.RunPhaseState, error) {
	var phaseState models.RunPhaseState
	err := r.db.Where("run_id = ? AND phase = ?", runID, phase).First(&phaseState).Error
	if err != nil {
		return nil, err
	}
	return &phaseState, nil
}

func (r *RunPhaseStateRepository) Update(phaseState *models.RunPhaseState) error {
	return r.db.Save(phaseState).Error
}

func (r *RunPhaseStateRepository) Upsert(phaseState *models.RunPhaseState) error {
	// Use ON CONFLICT to upsert (PostgreSQL specific)
	// GORM doesn't have native upsert, so we'll use a transaction
	return r.db.Transaction(func(tx *gorm.DB) error {
		var existing models.RunPhaseState
		err := tx.Where("run_id = ? AND phase = ?", phaseState.RunID, phaseState.Phase).First(&existing).Error

		if err == gorm.ErrRecordNotFound {
			// Create new
			return tx.Create(phaseState).Error
		} else if err != nil {
			return err
		}

		// Update existing
		phaseState.ID = existing.ID
		return tx.Save(phaseState).Error
	})
}

func (r *RunPhaseStateRepository) DeleteByRunID(runID string) error {
	return r.db.Where("run_id = ?", runID).Delete(&models.RunPhaseState{}).Error
}
