// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"time"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type RunRepository struct {
	db *gorm.DB
}

func NewRunRepository(db *gorm.DB) *RunRepository {
	return &RunRepository{db: db}
}

func (r *RunRepository) Create(run *models.Run) error {
	return r.db.Create(run).Error
}

func (r *RunRepository) GetByID(id string) (*models.Run, error) {
	var run models.Run
	err := r.db.Preload("Workspace").Preload("Workspace.Project").Preload("AgentPool").Preload("Runner").First(&run, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func (r *RunRepository) ListByWorkspace(workspaceID string, limit, offset int) ([]models.Run, int64, error) {
	var runs []models.Run
	var total int64

	if err := r.db.Model(&models.Run{}).Where("workspace_id = ?", workspaceID).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("workspace_id = ?", workspaceID).
		Omit("PlanOutput").
		Preload("AgentPool").Preload("Runner").
		Order("created_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&runs).Error
	return runs, total, err
}

// GetLatestByWorkspaceIDs returns the most recent run for each of the given workspace IDs
// in a single query. The returned map is keyed by workspace ID.
func (r *RunRepository) GetLatestByWorkspaceIDs(workspaceIDs []string) (map[string]*models.Run, error) {
	if len(workspaceIDs) == 0 {
		return map[string]*models.Run{}, nil
	}

	// Use DISTINCT ON (PostgreSQL) to get the latest run per workspace in one query
	var runs []models.Run
	err := r.db.Raw(`
		SELECT DISTINCT ON (workspace_id) *
		FROM runs
		WHERE workspace_id IN ?
		ORDER BY workspace_id, created_at DESC
	`, workspaceIDs).Scan(&runs).Error
	if err != nil {
		return nil, err
	}

	result := make(map[string]*models.Run, len(runs))
	for i := range runs {
		result[runs[i].WorkspaceID] = &runs[i]
	}
	return result, nil
}

func (r *RunRepository) Update(run *models.Run) error {
	return r.db.Save(run).Error
}

func (r *RunRepository) ListByStatus(status models.RunStatus, limit int) ([]models.Run, error) {
	var runs []models.Run
	err := r.db.Where("status = ?", status).Omit("PlanOutput").Limit(limit).Find(&runs).Error
	return runs, err
}

// ListByOrganization lists runs for all workspaces in an organization
func (r *RunRepository) ListByOrganization(organizationID uuid.UUID, limit, offset int) ([]models.Run, int64, error) {
	var runs []models.Run
	var total int64

	// Join with workspaces and projects to filter by organization
	query := r.db.Model(&models.Run{}).
		Joins("JOIN workspaces ON runs.workspace_id = workspaces.id").
		Joins("JOIN projects ON workspaces.project_id = projects.id").
		Where("projects.organization_id = ?", organizationID)

	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := query.
		Omit("PlanOutput").
		Preload("Workspace").
		Preload("Workspace.Project").
		Preload("AgentPool").
		Preload("Runner").
		Order("runs.created_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&runs).Error
	return runs, total, err
}

// ListQueued lists runs that are queued (pending or running) for an organization
// Excludes cancelled, completed, and failed runs
// TFE-compatible: Only returns runs that are actually queued (pending or running)
func (r *RunRepository) ListQueued(organizationID uuid.UUID, limit int) ([]models.Run, error) {
	var runs []models.Run

	// Join with workspaces and projects to filter by organization
	// Only return pending or running runs - the WHERE clause already excludes cancelled/completed/failed
	// The status IN clause is sufficient - no need for additional exclusions
	err := r.db.Model(&models.Run{}).
		Joins("JOIN workspaces ON runs.workspace_id = workspaces.id").
		Joins("JOIN projects ON workspaces.project_id = projects.id").
		Where("projects.organization_id = ?", organizationID).
		Where("runs.status IN ?", []models.RunStatus{models.RunStatusPending, models.RunStatusRunning}).
		Omit("PlanOutput").
		Preload("Workspace").
		Preload("Workspace.Project").
		Order("runs.created_at ASC").
		Limit(limit).
		Find(&runs).Error
	return runs, err
}

// FindStuckRuns finds runs that are stuck (running too long or pending too long without updates)
// - Running runs that have exceeded their timeout (based on workspace RunTimeout, default 1 hour)
// - Pending runs that have been pending for more than maxAge (default 30 minutes) - only truly abandoned runs
// Note: We don't use a fixed time threshold for running runs because normal Terraform runs can take 30+ minutes
func (r *RunRepository) FindStuckRuns(maxAge time.Duration) ([]models.Run, error) {
	var runs []models.Run
	now := time.Now()

	// Find running runs that have exceeded their timeout
	// Use workspace RunTimeout if available, otherwise use default (1 hour = 3600 seconds)
	// PostgreSQL: Use NOW() - (run_timeout || 3600) * INTERVAL '1 second'
	err := r.db.Model(&models.Run{}).
		Joins("JOIN workspaces ON runs.workspace_id = workspaces.id").
		Where("runs.status = ?", models.RunStatusRunning).
		Where("runs.started_at IS NOT NULL").
		Where("runs.started_at < NOW() - INTERVAL '1 second' * COALESCE(NULLIF(workspaces.run_timeout, 0), 3600)").
		Omit("PlanOutput").
		Preload("Workspace").
		Find(&runs).Error
	if err != nil {
		return nil, err
	}

	// Find pending runs that have been pending for more than maxAge (default 30 minutes)
	// Only clean up truly abandoned pending runs - normal pending runs should wait
	var pendingRuns []models.Run
	err = r.db.Model(&models.Run{}).
		Where("status = ?", models.RunStatusPending).
		Where("created_at < ?", now.Add(-maxAge)).
		Omit("PlanOutput").
		Preload("Workspace").
		Find(&pendingRuns).Error
	if err != nil {
		return nil, err
	}

	// Combine both lists
	runs = append(runs, pendingRuns...)
	return runs, nil
}

// MarkAsFailed marks a run as failed with an error message
func (r *RunRepository) MarkAsFailed(runID string, errorMessage string) error {
	now := time.Now()
	return r.db.Model(&models.Run{}).
		Where("id = ?", runID).
		Updates(map[string]interface{}{
			"status":        models.RunStatusFailed,
			"error_message": errorMessage,
			"completed_at":  now,
			"updated_at":    now,
		}).Error
}

// ListByWorkspaceAndOperationAndConfigVersion lists runs by workspace, operation, and configuration version
// Used for deduplication logic to prevent duplicate runs from Terraform CLI retries
func (r *RunRepository) ListByWorkspaceAndOperationAndConfigVersion(
	workspaceID string,
	operation models.RunOperation,
	configVersionID *string,
	limit int,
) ([]models.Run, error) {
	var runs []models.Run
	query := r.db.Where("workspace_id = ?", workspaceID).
		Where("operation = ?", operation)

	if configVersionID != nil {
		query = query.Where("configuration_version_id = ?", *configVersionID)
	} else {
		query = query.Where("configuration_version_id IS NULL")
	}

	err := query.
		Omit("PlanOutput").
		Order("created_at DESC").
		Limit(limit).
		Find(&runs).Error
	return runs, err
}

// ListByUser lists runs created by a specific user across all organizations
func (r *RunRepository) ListByUser(userID uuid.UUID, limit, offset int) ([]models.Run, int64, error) {
	var runs []models.Run
	var total int64

	query := r.db.Model(&models.Run{}).Where("created_by = ?", userID)

	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := query.
		Omit("PlanOutput").
		Preload("Workspace").
		Preload("Workspace.Project").
		Order("created_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&runs).Error
	return runs, total, err
}

// ListByOrganizationAndUser lists runs for an organization filtered by user
func (r *RunRepository) ListByOrganizationAndUser(organizationID, userID uuid.UUID, limit, offset int) ([]models.Run, int64, error) {
	var runs []models.Run
	var total int64

	// Join with workspaces and projects to filter by organization, and filter by user
	query := r.db.Model(&models.Run{}).
		Joins("JOIN workspaces ON runs.workspace_id = workspaces.id").
		Joins("JOIN projects ON workspaces.project_id = projects.id").
		Where("projects.organization_id = ?", organizationID).
		Where("runs.created_by = ?", userID)

	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := query.
		Omit("PlanOutput").
		Preload("Workspace").
		Preload("Workspace.Project").
		Order("runs.created_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&runs).Error
	return runs, total, err
}

// ListPRRunsForStatusChecks lists runs with speculative configuration versions that need status check updates
// These are PR runs (speculative plans) that should have their GitHub status checks updated
func (r *RunRepository) ListPRRunsForStatusChecks(limit int) ([]models.Run, error) {
	var runs []models.Run

	// Find runs with speculative config versions that are in states that need status check updates
	// Join with configuration_versions to filter by speculative=true and commit_hash IS NOT NULL
	err := r.db.Model(&models.Run{}).
		Joins("JOIN configuration_versions ON runs.configuration_version_id = configuration_versions.id").
		Where("configuration_versions.speculative = ?", true).
		Where("configuration_versions.commit_hash != ''").
		Where("configuration_versions.commit_hash IS NOT NULL").
		Where("runs.status IN ?", []models.RunStatus{
			models.RunStatusPending,
			models.RunStatusPlanning,
			models.RunStatusPlanned,
			models.RunStatusFailed,
			models.RunStatusCancelled,
		}).
		Omit("PlanOutput").
		Preload("Workspace").
		Preload("Workspace.Project").
		Preload("Workspace.Project.Organization").
		Order("runs.updated_at DESC").
		Limit(limit).
		Find(&runs).Error

	return runs, err
}
