// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type WorkspaceRepository struct {
	db *gorm.DB
}

func NewWorkspaceRepository(db *gorm.DB) *WorkspaceRepository {
	return &WorkspaceRepository{db: db}
}

func (r *WorkspaceRepository) Create(workspace *models.Workspace) error {
	return r.db.Create(workspace).Error
}

func (r *WorkspaceRepository) GetByID(id string) (*models.Workspace, error) {
	var workspace models.Workspace
	err := r.db.Preload("Project").Preload("Project.Organization").Preload("AgentPool").Preload("VCSConnection").First(&workspace, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &workspace, nil
}

func (r *WorkspaceRepository) GetByProjectAndName(projectID uuid.UUID, name string) (*models.Workspace, error) {
	var workspace models.Workspace
	err := r.db.First(&workspace, "project_id = ? AND name = ?", projectID, name).Error
	if err != nil {
		return nil, err
	}
	return &workspace, nil
}

func (r *WorkspaceRepository) ListByProject(projectID uuid.UUID, limit, offset int) ([]models.Workspace, int64, error) {
	var workspaces []models.Workspace
	var total int64

	if err := r.db.Model(&models.Workspace{}).Where("project_id = ?", projectID).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("project_id = ?", projectID).Limit(limit).Offset(offset).Find(&workspaces).Error
	return workspaces, total, err
}

func (r *WorkspaceRepository) Update(workspace *models.Workspace) error {
	return r.db.Save(workspace).Error
}

func (r *WorkspaceRepository) Delete(id string) error {
	// Delete related records first to avoid foreign key constraint violations
	// Order matters due to foreign key relationships:
	// 1. State versions reference runs, so we need to clear the RunID reference first
	// 2. Then delete runs
	// 3. Then delete state versions
	// 4. Then delete other related records

	// First, clear RunID references in state versions to break foreign key relationship
	if err := r.db.Model(&models.StateVersion{}).
		Where("workspace_id = ? AND run_id IS NOT NULL", id).
		Update("run_id", nil).Error; err != nil {
		return fmt.Errorf("failed to clear run_id in state versions: %w", err)
	}

	// Delete run phase states (must be deleted before runs due to foreign key constraint)
	// First get all run IDs for this workspace
	var runIDs []string
	if err := r.db.Model(&models.Run{}).Where("workspace_id = ?", id).Pluck("id", &runIDs).Error; err != nil {
		return fmt.Errorf("failed to get run IDs: %w", err)
	}
	if len(runIDs) > 0 {
		if err := r.db.Where("run_id IN ?", runIDs).Delete(&models.RunPhaseState{}).Error; err != nil {
			return fmt.Errorf("failed to delete run phase states: %w", err)
		}
	}

	// Delete runs (now safe since run phase states no longer reference them)
	if err := r.db.Where("workspace_id = ?", id).Delete(&models.Run{}).Error; err != nil {
		return fmt.Errorf("failed to delete runs: %w", err)
	}

	// Delete configuration versions
	if err := r.db.Where("workspace_id = ?", id).Delete(&models.ConfigurationVersion{}).Error; err != nil {
		return fmt.Errorf("failed to delete configuration versions: %w", err)
	}

	// Delete state versions (now safe since run_id references are cleared)
	if err := r.db.Where("workspace_id = ?", id).Delete(&models.StateVersion{}).Error; err != nil {
		return fmt.Errorf("failed to delete state versions: %w", err)
	}

	// Delete variables
	if err := r.db.Where("workspace_id = ?", id).Delete(&models.Variable{}).Error; err != nil {
		return fmt.Errorf("failed to delete variables: %w", err)
	}

	// Delete state locks
	if err := r.db.Where("workspace_id = ?", id).Delete(&models.StateLock{}).Error; err != nil {
		return fmt.Errorf("failed to delete state locks: %w", err)
	}

	// Delete variable set workspace associations (many-to-many relationship)
	if err := r.db.Where("workspace_id = ?", id).Delete(&models.VariableSetWorkspace{}).Error; err != nil {
		return fmt.Errorf("failed to delete variable set workspace associations: %w", err)
	}

	// Delete team workspace access records
	if err := r.db.Where("workspace_id = ?", id).Delete(&models.TeamWorkspaceAccess{}).Error; err != nil {
		return fmt.Errorf("failed to delete team workspace access: %w", err)
	}

	// Delete agent pool workspace associations (many-to-many junction tables)
	if err := r.db.Exec("DELETE FROM agent_pool_allowed_workspaces WHERE workspace_id = ?", id).Error; err != nil {
		return fmt.Errorf("failed to delete agent pool allowed workspace associations: %w", err)
	}
	if err := r.db.Exec("DELETE FROM agent_pool_excluded_workspaces WHERE workspace_id = ?", id).Error; err != nil {
		return fmt.Errorf("failed to delete agent pool excluded workspace associations: %w", err)
	}

	// Finally delete the workspace
	return r.db.Delete(&models.Workspace{}, "id = ?", id).Error
}

// GetByOrganizationAndName gets a workspace by organization name and workspace name (for TFE compatibility)
func (r *WorkspaceRepository) GetByOrganizationAndName(orgName, workspaceName string) (*models.Workspace, error) {
	var workspace models.Workspace
	err := r.db.Joins("JOIN projects ON workspaces.project_id = projects.id").
		Joins("JOIN organizations ON projects.organization_id = organizations.id").
		Where("organizations.name = ? AND workspaces.name = ?", orgName, workspaceName).
		Preload("Project").Preload("Project.Organization").Preload("AgentPool").Preload("VCSConnection").
		First(&workspace).Error
	if err != nil {
		return nil, err
	}
	return &workspace, nil
}

// ListByOrganization lists workspaces by organization name (for TFE compatibility)
func (r *WorkspaceRepository) ListByOrganization(orgName string, limit, offset int) ([]models.Workspace, int64, error) {
	var workspaces []models.Workspace
	var total int64

	baseQuery := r.db.Model(&models.Workspace{}).
		Joins("JOIN projects ON workspaces.project_id = projects.id").
		Joins("JOIN organizations ON projects.organization_id = organizations.id").
		Where("organizations.name = ?", orgName)

	if err := baseQuery.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := baseQuery.Limit(limit).Offset(offset).
		Preload("Project").Preload("Project.Organization").Preload("AgentPool").Preload("VCSConnection").
		Find(&workspaces).Error
	return workspaces, total, err
}

// ListByOrganizationAndIDs lists workspaces by organization name and workspace IDs (for permission filtering)
func (r *WorkspaceRepository) ListByOrganizationAndIDs(orgName string, workspaceIDs []string, limit, offset int) ([]models.Workspace, int64, error) {
	var workspaces []models.Workspace
	var total int64

	baseQuery := r.db.Model(&models.Workspace{}).
		Joins("JOIN projects ON workspaces.project_id = projects.id").
		Joins("JOIN organizations ON projects.organization_id = organizations.id").
		Where("organizations.name = ? AND workspaces.id IN ?", orgName, workspaceIDs)

	if err := baseQuery.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := baseQuery.Limit(limit).Offset(offset).
		Preload("Project").Preload("Project.Organization").Preload("AgentPool").Preload("VCSConnection").
		Find(&workspaces).Error
	return workspaces, total, err
}

// FindByVCSRepositoryAndBranch finds workspaces by VCS repository and branch
// Used for webhook-triggered runs
func (r *WorkspaceRepository) FindByVCSRepositoryAndBranch(repository string, branch string) ([]models.Workspace, error) {
	var workspaces []models.Workspace
	err := r.db.Where("vcs_repository = ? AND vcs_branch = ? AND auto_queue_runs = ?", repository, branch, true).
		Preload("Project").Preload("Project.Organization").
		Find(&workspaces).Error
	return workspaces, err
}

// FindByVCSRepository finds any workspace by VCS repository (used to determine org for webhook events)
func (r *WorkspaceRepository) FindByVCSRepository(repository string) ([]models.Workspace, error) {
	var workspaces []models.Workspace
	err := r.db.Where("vcs_repository = ?", repository).
		Preload("Project").Preload("Project.Organization").
		Limit(1).
		Find(&workspaces).Error
	return workspaces, err
}

// ListWithDriftDetectionEnabled lists workspaces with drift detection enabled
func (r *WorkspaceRepository) ListWithDriftDetectionEnabled() ([]models.Workspace, error) {
	var workspaces []models.Workspace
	err := r.db.Where("drift_detection_enabled = ?", true).
		Preload("Project").Preload("Project.Organization").
		Find(&workspaces).Error
	return workspaces, err
}

// HasAppliedRuns checks if a workspace has any applied runs
func (r *WorkspaceRepository) HasAppliedRuns(workspaceID string) (bool, error) {
	var count int64
	err := r.db.Model(&models.Run{}).
		Where("workspace_id = ? AND status = ?", workspaceID, models.RunStatusApplied).
		Count(&count).Error
	return count > 0, err
}

// HasActiveInfrastructure checks if a workspace has active infrastructure
// (has applied runs but hasn't been successfully destroyed)
func (r *WorkspaceRepository) HasActiveInfrastructure(workspaceID string) (bool, error) {
	// First check if there are any applied runs
	var appliedCount int64
	err := r.db.Model(&models.Run{}).
		Where("workspace_id = ? AND status = ?", workspaceID, models.RunStatusApplied).
		Count(&appliedCount).Error
	if err != nil {
		return false, err
	}

	// If no applied runs, no active infrastructure
	if appliedCount == 0 {
		return false, nil
	}

	// Check if the most recent completed destroy was successful
	// If the last successful operation was a destroy, the infrastructure is gone
	// Destroy runs are successful when status is "applied" (not "completed")
	var lastDestroyRun models.Run
	err = r.db.Model(&models.Run{}).
		Where("workspace_id = ? AND operation = ? AND status IN ? AND completed_at IS NOT NULL", workspaceID, models.RunOperationDestroy, []models.RunStatus{models.RunStatusApplied, models.RunStatusCompleted}).
		Order("completed_at DESC").
		First(&lastDestroyRun).Error

	if err == nil && lastDestroyRun.CompletedAt != nil {
		// Found a successful destroy - check if any applies happened after it
		var appliesAfterDestroy int64
		err = r.db.Model(&models.Run{}).
			Where("workspace_id = ? AND status = ? AND completed_at > ?",
				workspaceID, models.RunStatusApplied, lastDestroyRun.CompletedAt).
			Count(&appliesAfterDestroy).Error
		if err != nil {
			return false, err
		}
		// If no applies after the last destroy, infrastructure is gone
		return appliesAfterDestroy > 0, nil
	}

	// No successful destroy found (or destroy has no completed_at), so applied runs mean active infrastructure
	return true, nil
}
