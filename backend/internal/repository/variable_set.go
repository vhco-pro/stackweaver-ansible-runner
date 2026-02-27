// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type VariableSetRepository struct {
	db *gorm.DB
}

func NewVariableSetRepository(db *gorm.DB) *VariableSetRepository {
	return &VariableSetRepository{db: db}
}

func (r *VariableSetRepository) Create(variableSet *models.VariableSet) error {
	return r.db.Create(variableSet).Error
}

func (r *VariableSetRepository) GetByID(id string) (*models.VariableSet, error) {
	var variableSet models.VariableSet
	err := r.db.Preload("Variables").Preload("Workspaces").Preload("Projects").Preload("Project").
		First(&variableSet, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &variableSet, nil
}

func (r *VariableSetRepository) ListByOrganization(organizationID uuid.UUID) ([]models.VariableSet, error) {
	var variableSets []models.VariableSet
	err := r.db.Preload("Variables").Preload("Workspaces").Preload("Projects").Preload("Project").
		Where("organization_id = ?", organizationID).
		Find(&variableSets).Error
	return variableSets, err
}

func (r *VariableSetRepository) ListByWorkspace(workspaceID string) ([]models.VariableSet, error) {
	var variableSets []models.VariableSet

	workspace := &models.Workspace{}
	if err := r.db.First(workspace, "id = ?", workspaceID).Error; err != nil {
		return variableSets, err
	}

	project := &models.Project{}
	if err := r.db.First(project, "id = ?", workspace.ProjectID).Error; err != nil {
		return variableSets, err
	}

	// Get organization-scoped variable sets (organization-owned)
	// If a variable set has project assignments, only include it if the workspace's project is assigned
	// If a variable set has no project assignments, it applies to all workspaces in the org
	var orgScoped []models.VariableSet
	err := r.db.Preload("Variables").Preload("Projects").Preload("Project").
		Where("organization_id = ? AND scope = ? AND project_id IS NULL", project.OrganizationID, "organization").
		Find(&orgScoped).Error
	if err == nil {
		for _, vs := range orgScoped {
			// If variable set has project assignments, check if this workspace's project is assigned
			if len(vs.Projects) > 0 {
				// Check if workspace's project is in the assigned projects
				projectAssigned := false
				for _, p := range vs.Projects {
					if p.ID == project.ID {
						projectAssigned = true
						break
					}
				}
				if projectAssigned {
					variableSets = append(variableSets, vs)
				}
			} else {
				// No project assignments = applies to all workspaces
				variableSets = append(variableSets, vs)
			}
		}
	}

	// Get project-scoped variable sets (project-owned) for this workspace's project
	var projectScoped []models.VariableSet
	err = r.db.Preload("Variables").Preload("Projects").Preload("Project").
		Where("project_id = ?", project.ID).
		Find(&projectScoped).Error
	if err == nil {
		// Project-owned variable sets apply to all workspaces in that project
		variableSets = append(variableSets, projectScoped...)
	}

	// Get workspace-scoped variable sets
	var workspaceScoped []models.VariableSet
	err = r.db.Table("variable_sets").
		Joins("JOIN variable_set_workspaces ON variable_sets.id = variable_set_workspaces.variable_set_id").
		Where("variable_set_workspaces.workspace_id = ? AND variable_sets.scope = ?", workspaceID, "workspace").
		Preload("Variables").
		Find(&workspaceScoped).Error
	if err == nil {
		variableSets = append(variableSets, workspaceScoped...)
	}

	return variableSets, nil
}

func (r *VariableSetRepository) Update(variableSet *models.VariableSet) error {
	return r.db.Save(variableSet).Error
}

func (r *VariableSetRepository) Delete(id string) error {
	// Delete variables first
	r.db.Where("variable_set_id = ?", id).Delete(&models.VariableSetVariable{})
	// Delete workspace associations
	r.db.Where("variable_set_id = ?", id).Delete(&models.VariableSetWorkspace{})
	// Delete project associations
	r.db.Where("variable_set_id = ?", id).Delete(&models.VariableSetProject{})
	// Delete job template associations
	r.db.Where("variable_set_id = ?", id).Delete(&models.VariableSetJobTemplate{})
	// Delete variable set
	return r.db.Delete(&models.VariableSet{}, "id = ?", id).Error
}

func (r *VariableSetRepository) AddWorkspace(variableSetID, workspaceID string) error {
	return r.db.Create(&models.VariableSetWorkspace{
		VariableSetID: variableSetID,
		WorkspaceID:   workspaceID,
	}).Error
}

func (r *VariableSetRepository) RemoveWorkspace(variableSetID, workspaceID string) error {
	return r.db.Where("variable_set_id = ? AND workspace_id = ?", variableSetID, workspaceID).
		Delete(&models.VariableSetWorkspace{}).Error
}

func (r *VariableSetRepository) AddProject(variableSetID string, projectID uuid.UUID) error {
	return r.db.Create(&models.VariableSetProject{
		VariableSetID: variableSetID,
		ProjectID:     projectID,
	}).Error
}

func (r *VariableSetRepository) RemoveProject(variableSetID string, projectID uuid.UUID) error {
	return r.db.Where("variable_set_id = ? AND project_id = ?", variableSetID, projectID).
		Delete(&models.VariableSetProject{}).Error
}

func (r *VariableSetRepository) ListProjects(variableSetID string) ([]models.Project, error) {
	var projects []models.Project
	err := r.db.Table("projects").
		Joins("JOIN variable_set_projects ON projects.id = variable_set_projects.project_id").
		Where("variable_set_projects.variable_set_id = ?", variableSetID).
		Find(&projects).Error
	return projects, err
}

// ListByJobTemplate lists variable sets assigned to a job template
func (r *VariableSetRepository) ListByJobTemplate(jobTemplateID uuid.UUID) ([]models.VariableSet, error) {
	var variableSets []models.VariableSet
	err := r.db.Table("variable_sets").
		Joins("JOIN variable_set_job_templates ON variable_sets.id = variable_set_job_templates.variable_set_id").
		Where("variable_set_job_templates.job_template_id = ?", jobTemplateID).
		Preload("Variables").
		Find(&variableSets).Error
	return variableSets, err
}

// ListByProject lists variable sets that apply to a project (TFE-compatible)
// Includes:
// 1. Organization-scoped variable sets with no project assignments (applies to all projects)
// 2. Organization-scoped variable sets with this project assigned
// 3. Project-scoped variable sets for this project
func (r *VariableSetRepository) ListByProject(projectID uuid.UUID) ([]models.VariableSet, error) {
	var variableSets []models.VariableSet

	project := &models.Project{}
	if err := r.db.First(project, "id = ?", projectID).Error; err != nil {
		return variableSets, err
	}

	// Get organization-scoped variable sets (organization-owned)
	// If a variable set has project assignments, only include it if this project is assigned
	// If a variable set has no project assignments, it applies to all projects in the org
	var orgScoped []models.VariableSet
	err := r.db.Preload("Variables").Preload("Projects").Preload("Project").
		Where("organization_id = ? AND scope = ? AND project_id IS NULL", project.OrganizationID, "organization").
		Find(&orgScoped).Error
	if err == nil {
		for _, vs := range orgScoped {
			// If variable set has project assignments, check if this project is assigned
			if len(vs.Projects) > 0 {
				// Check if this project is in the assigned projects
				projectAssigned := false
				for _, p := range vs.Projects {
					if p.ID == project.ID {
						projectAssigned = true
						break
					}
				}
				if projectAssigned {
					variableSets = append(variableSets, vs)
				}
			} else {
				// No project assignments = applies to all projects
				variableSets = append(variableSets, vs)
			}
		}
	}

	// Get project-scoped variable sets (project-owned) for this project
	var projectScoped []models.VariableSet
	err = r.db.Preload("Variables").Preload("Projects").Preload("Project").
		Where("project_id = ?", project.ID).
		Find(&projectScoped).Error
	if err == nil {
		// Project-owned variable sets apply to all templates in that project
		variableSets = append(variableSets, projectScoped...)
	}

	return variableSets, nil
}

// AddJobTemplate assigns a variable set to a job template
func (r *VariableSetRepository) AddJobTemplate(variableSetID string, jobTemplateID uuid.UUID) error {
	return r.db.Create(&models.VariableSetJobTemplate{
		VariableSetID: variableSetID,
		JobTemplateID: jobTemplateID,
	}).Error
}

// RemoveJobTemplate unassigns a variable set from a job template
func (r *VariableSetRepository) RemoveJobTemplate(variableSetID string, jobTemplateID uuid.UUID) error {
	return r.db.Where("variable_set_id = ? AND job_template_id = ?", variableSetID, jobTemplateID).
		Delete(&models.VariableSetJobTemplate{}).Error
}

// VariableSetVariableRepository handles variable set variables
type VariableSetVariableRepository struct {
	db *gorm.DB
}

func NewVariableSetVariableRepository(db *gorm.DB) *VariableSetVariableRepository {
	return &VariableSetVariableRepository{db: db}
}

func (r *VariableSetVariableRepository) Create(variable *models.VariableSetVariable) error {
	return r.db.Create(variable).Error
}

func (r *VariableSetVariableRepository) GetByID(id string) (*models.VariableSetVariable, error) {
	var variable models.VariableSetVariable
	err := r.db.First(&variable, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &variable, nil
}

func (r *VariableSetVariableRepository) ListByVariableSet(variableSetID string) ([]models.VariableSetVariable, error) {
	var variables []models.VariableSetVariable
	err := r.db.Where("variable_set_id = ?", variableSetID).Find(&variables).Error
	return variables, err
}

func (r *VariableSetVariableRepository) Update(variable *models.VariableSetVariable) error {
	return r.db.Save(variable).Error
}

func (r *VariableSetVariableRepository) Delete(id string) error {
	return r.db.Delete(&models.VariableSetVariable{}, "id = ?", id).Error
}
