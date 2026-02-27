// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type VariableRepository struct {
	db *gorm.DB
}

func NewVariableRepository(db *gorm.DB) *VariableRepository {
	return &VariableRepository{db: db}
}

func (r *VariableRepository) Create(variable *models.Variable) error {
	return r.db.Create(variable).Error
}

func (r *VariableRepository) GetByID(id string) (*models.Variable, error) {
	var variable models.Variable
	err := r.db.First(&variable, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &variable, nil
}

func (r *VariableRepository) GetByWorkspaceAndKey(workspaceID string, key string) (*models.Variable, error) {
	var variable models.Variable
	err := r.db.First(&variable, "workspace_id = ? AND key = ?", workspaceID, key).Error
	if err != nil {
		return nil, err
	}
	return &variable, nil
}

func (r *VariableRepository) ListByWorkspace(workspaceID string) ([]models.Variable, error) {
	var variables []models.Variable
	err := r.db.Where("workspace_id = ?", workspaceID).Find(&variables).Error
	return variables, err
}

func (r *VariableRepository) Update(variable *models.Variable) error {
	return r.db.Save(variable).Error
}

func (r *VariableRepository) Delete(id string) error {
	return r.db.Delete(&models.Variable{}, "id = ?", id).Error
}

func (r *VariableRepository) DeleteByWorkspaceAndKey(workspaceID string, key string) error {
	return r.db.Delete(&models.Variable{}, "workspace_id = ? AND key = ?", workspaceID, key).Error
}
