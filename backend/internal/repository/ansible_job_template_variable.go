// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type AnsibleJobTemplateVariableRepository struct {
	db *gorm.DB
}

func NewAnsibleJobTemplateVariableRepository(db *gorm.DB) *AnsibleJobTemplateVariableRepository {
	return &AnsibleJobTemplateVariableRepository{db: db}
}

func (r *AnsibleJobTemplateVariableRepository) Create(variable *models.AnsibleJobTemplateVariable) error {
	return r.db.Create(variable).Error
}

func (r *AnsibleJobTemplateVariableRepository) GetByID(id string) (*models.AnsibleJobTemplateVariable, error) {
	var variable models.AnsibleJobTemplateVariable
	err := r.db.First(&variable, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &variable, nil
}

func (r *AnsibleJobTemplateVariableRepository) GetByJobTemplateAndKey(jobTemplateID uuid.UUID, key string) (*models.AnsibleJobTemplateVariable, error) {
	var variable models.AnsibleJobTemplateVariable
	err := r.db.Where("job_template_id = ? AND key = ?", jobTemplateID, key).First(&variable).Error
	if err != nil {
		return nil, err
	}
	return &variable, nil
}

func (r *AnsibleJobTemplateVariableRepository) ListByJobTemplate(jobTemplateID uuid.UUID) ([]models.AnsibleJobTemplateVariable, error) {
	var variables []models.AnsibleJobTemplateVariable
	err := r.db.Where("job_template_id = ?", jobTemplateID).Find(&variables).Error
	return variables, err
}

func (r *AnsibleJobTemplateVariableRepository) Update(variable *models.AnsibleJobTemplateVariable) error {
	return r.db.Save(variable).Error
}

func (r *AnsibleJobTemplateVariableRepository) Delete(id string) error {
	return r.db.Delete(&models.AnsibleJobTemplateVariable{}, "id = ?", id).Error
}

func (r *AnsibleJobTemplateVariableRepository) DeleteByJobTemplate(jobTemplateID uuid.UUID) error {
	return r.db.Where("job_template_id = ?", jobTemplateID).Delete(&models.AnsibleJobTemplateVariable{}).Error
}
