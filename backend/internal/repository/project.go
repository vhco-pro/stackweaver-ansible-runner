// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type ProjectRepository struct {
	db *gorm.DB
}

func NewProjectRepository(db *gorm.DB) *ProjectRepository {
	return &ProjectRepository{db: db}
}

func (r *ProjectRepository) Create(project *models.Project) error {
	return r.db.Create(project).Error
}

func (r *ProjectRepository) GetByID(id uuid.UUID) (*models.Project, error) {
	var project models.Project
	err := r.db.Preload("Organization").Preload("Workspaces").First(&project, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &project, nil
}

func (r *ProjectRepository) GetByIDWithResources(id uuid.UUID) (*models.Project, error) {
	var project models.Project
	err := r.db.
		Preload("Organization").
		Preload("Workspaces").
		Preload("Inventories").
		Preload("Playbooks").
		Preload("JobTemplates").
		Preload("Workflows").
		Preload("Credentials").
		First(&project, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &project, nil
}

func (r *ProjectRepository) GetByOrganizationAndName(orgID uuid.UUID, name string) (*models.Project, error) {
	var project models.Project
	err := r.db.First(&project, "organization_id = ? AND name = ?", orgID, name).Error
	if err != nil {
		return nil, err
	}
	return &project, nil
}

func (r *ProjectRepository) ListByOrganization(orgID uuid.UUID, limit, offset int) ([]models.Project, int64, error) {
	var projects []models.Project
	var total int64

	if err := r.db.Model(&models.Project{}).Where("organization_id = ?", orgID).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("organization_id = ?", orgID).Limit(limit).Offset(offset).Find(&projects).Error
	return projects, total, err
}

func (r *ProjectRepository) Update(project *models.Project) error {
	return r.db.Save(project).Error
}

func (r *ProjectRepository) Delete(id uuid.UUID) error {
	// Delete related records first to avoid foreign key constraint violations
	// Order matters due to foreign key relationships:
	// 1. Delete TeamProjectAccess records (foreign key constraint - this was causing the error)
	// 2. Delete VariableSetProject records (many-to-many relationship)
	// 3. Finally delete the project itself
	// Note: Workspaces must be deleted separately before deleting the project
	// as they have their own delete method that handles all their related records

	// 1. Delete TeamProjectAccess records
	if err := r.db.Where("project_id = ?", id).Delete(&models.TeamProjectAccess{}).Error; err != nil {
		return fmt.Errorf("failed to delete team project access records: %w", err)
	}

	// 2. Delete VariableSetProject records (many-to-many relationship)
	if err := r.db.Where("project_id = ?", id).Delete(&models.VariableSetProject{}).Error; err != nil {
		return fmt.Errorf("failed to delete variable set project associations: %w", err)
	}

	// 3. Delete agent pool project associations (many-to-many junction table)
	if err := r.db.Exec("DELETE FROM agent_pool_allowed_projects WHERE project_id = ?", id).Error; err != nil {
		return fmt.Errorf("failed to delete agent pool allowed project associations: %w", err)
	}

	// 4. Finally delete the project itself
	return r.db.Delete(&models.Project{}, "id = ?", id).Error
}
