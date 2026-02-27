// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"time"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type RunnerRepository struct {
	db *gorm.DB
}

func NewRunnerRepository(db *gorm.DB) *RunnerRepository {
	return &RunnerRepository{db: db}
}

// Create creates a new runner.
func (r *RunnerRepository) Create(runner *models.Runner) error {
	return r.db.Create(runner).Error
}

// GetByID returns a runner by ID.
func (r *RunnerRepository) GetByID(id uuid.UUID) (*models.Runner, error) {
	var runner models.Runner
	err := r.db.Preload("Organization").Preload("AgentPool").First(&runner, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &runner, nil
}

// GetByName returns a runner by organization and name.
func (r *RunnerRepository) GetByName(orgID uuid.UUID, name string) (*models.Runner, error) {
	var runner models.Runner
	err := r.db.First(&runner, "organization_id = ? AND name = ?", orgID, name).Error
	if err != nil {
		return nil, err
	}
	return &runner, nil
}

// ListByOrganization lists runners for an organization.
func (r *RunnerRepository) ListByOrganization(orgID uuid.UUID, opts ListRunnersOptions) ([]models.Runner, int64, error) {
	var runners []models.Runner
	var total int64

	q := r.db.Model(&models.Runner{}).Where("organization_id = ?", orgID)
	if opts.AgentPoolID != nil {
		q = q.Where("agent_pool_id = ?", *opts.AgentPoolID)
	}
	if opts.Status != "" {
		q = q.Where("status = ?", opts.Status)
	}
	if opts.RunnerType != "" {
		q = q.Where("runner_type = ?", opts.RunnerType)
	}
	if opts.Query != "" {
		q = q.Where("name ILIKE ? OR hostname ILIKE ?", "%"+opts.Query+"%", "%"+opts.Query+"%")
	}
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	// Reset query for actual fetch
	q = r.db.Where("organization_id = ?", orgID)
	if opts.AgentPoolID != nil {
		q = q.Where("agent_pool_id = ?", *opts.AgentPoolID)
	}
	if opts.Status != "" {
		q = q.Where("status = ?", opts.Status)
	}
	if opts.RunnerType != "" {
		q = q.Where("runner_type = ?", opts.RunnerType)
	}
	if opts.Query != "" {
		q = q.Where("name ILIKE ? OR hostname ILIKE ?", "%"+opts.Query+"%", "%"+opts.Query+"%")
	}

	switch opts.Sort {
	case "name":
		q = q.Order("name")
	case "status":
		q = q.Order("status")
	case "last_heartbeat_at", "last-heartbeat-at":
		q = q.Order("last_heartbeat_at DESC NULLS LAST")
	default:
		q = q.Order("registered_at DESC")
	}

	if opts.Limit > 0 {
		q = q.Limit(opts.Limit)
	}
	if opts.Offset > 0 {
		q = q.Offset(opts.Offset)
	}

	err := q.Preload("AgentPool").Find(&runners).Error
	return runners, total, err
}

// ListByAgentPool lists runners for an agent pool (TFE-compatible agents endpoint).
func (r *RunnerRepository) ListByAgentPool(poolID uuid.UUID) ([]models.Runner, error) {
	var runners []models.Runner
	err := r.db.Where("agent_pool_id = ?", poolID).Find(&runners).Error
	return runners, err
}

// ListRunnersOptions holds list filters and pagination.
type ListRunnersOptions struct {
	AgentPoolID *uuid.UUID
	Status      string
	RunnerType  string
	Query       string
	Sort        string
	Limit       int
	Offset      int
}

// Update updates runner fields.
func (r *RunnerRepository) Update(runner *models.Runner) error {
	return r.db.Save(runner).Error
}

// UpdateStatus updates only the status field.
func (r *RunnerRepository) UpdateStatus(id uuid.UUID, status models.RunnerStatus) error {
	return r.db.Model(&models.Runner{}).Where("id = ?", id).Update("status", status).Error
}

// UpdateHeartbeat updates the last heartbeat timestamp and status.
func (r *RunnerRepository) UpdateHeartbeat(id uuid.UUID, status models.RunnerStatus, currentJobs int) error {
	now := time.Now()
	return r.db.Model(&models.Runner{}).Where("id = ?", id).Updates(map[string]interface{}{
		"status":            status,
		"last_heartbeat_at": now,
	}).Error
}

// Delete deletes a runner and cleans up related records.
func (r *RunnerRepository) Delete(id uuid.UUID) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		// Remove runner_job_executions referencing this runner
		if err := tx.Where("runner_id = ?", id).Delete(&models.RunnerJobExecution{}).Error; err != nil {
			return err
		}

		// Nullify runner_id on ansible_jobs that were executed by this runner
		if err := tx.Model(&models.AnsibleJob{}).Where("runner_id = ?", id).Update("runner_id", nil).Error; err != nil {
			return err
		}

		// Nullify runner_id on terraform runs that were executed by this runner
		if err := tx.Model(&models.Run{}).Where("runner_id = ?", id).Update("runner_id", nil).Error; err != nil {
			return err
		}

		// Delete the runner
		res := tx.Delete(&models.Runner{}, "id = ?", id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
}

// MarkOfflineIfStale marks runners as offline if they haven't sent a heartbeat within the threshold.
func (r *RunnerRepository) MarkOfflineIfStale(threshold time.Duration) (int64, error) {
	cutoff := time.Now().Add(-threshold)
	res := r.db.Model(&models.Runner{}).
		Where("status IN (?, ?) AND (last_heartbeat_at < ? OR last_heartbeat_at IS NULL)",
			models.RunnerStatusOnline, models.RunnerStatusBusy, cutoff).
		Update("status", models.RunnerStatusOffline)
	return res.RowsAffected, res.Error
}

// CountByOrganization returns the count of runners by status for an organization.
func (r *RunnerRepository) CountByOrganization(orgID uuid.UUID) (total int64, online int64, err error) {
	err = r.db.Model(&models.Runner{}).Where("organization_id = ?", orgID).Count(&total).Error
	if err != nil {
		return
	}
	err = r.db.Model(&models.Runner{}).
		Where("organization_id = ? AND status IN (?, ?)", orgID, models.RunnerStatusOnline, models.RunnerStatusBusy).
		Count(&online).Error
	return
}

// CountByAgentPool returns the count of runners in a pool.
func (r *RunnerRepository) CountByAgentPool(poolID uuid.UUID) (int64, error) {
	var count int64
	err := r.db.Model(&models.Runner{}).Where("agent_pool_id = ?", poolID).Count(&count).Error
	return count, err
}

// FindAvailableRunner finds an available runner in a pool that can execute the given job type.
func (r *RunnerRepository) FindAvailableRunner(poolID uuid.UUID, jobType models.JobType, requiredLabels []string) (*models.Runner, error) {
	var runners []models.Runner

	q := r.db.Where("agent_pool_id = ? AND status = ?", poolID, models.RunnerStatusOnline)

	// Filter by runner type based on job type
	switch jobType {
	case models.JobTypeTerraformRun:
		q = q.Where("runner_type IN (?, ?)", models.RunnerTypeTerraform, models.RunnerTypeCombined)
	case models.JobTypeAnsibleJob:
		q = q.Where("runner_type IN (?, ?)", models.RunnerTypeAnsible, models.RunnerTypeCombined)
	}

	err := q.Find(&runners).Error
	if err != nil {
		return nil, err
	}

	// Filter by labels if required
	for _, runner := range runners {
		if len(requiredLabels) == 0 || runner.HasAllLabels(requiredLabels) {
			return &runner, nil
		}
	}

	return nil, gorm.ErrRecordNotFound
}

// RunnerJobExecutionRepository handles job execution tracking

type RunnerJobExecutionRepository struct {
	db *gorm.DB
}

func NewRunnerJobExecutionRepository(db *gorm.DB) *RunnerJobExecutionRepository {
	return &RunnerJobExecutionRepository{db: db}
}

// Create creates a new job execution record.
func (r *RunnerJobExecutionRepository) Create(exec *models.RunnerJobExecution) error {
	return r.db.Create(exec).Error
}

// GetByID returns a job execution by ID.
func (r *RunnerJobExecutionRepository) GetByID(id uuid.UUID) (*models.RunnerJobExecution, error) {
	var exec models.RunnerJobExecution
	err := r.db.First(&exec, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &exec, nil
}

// GetByJobID returns execution record by job ID.
func (r *RunnerJobExecutionRepository) GetByJobID(jobID uuid.UUID) (*models.RunnerJobExecution, error) {
	var exec models.RunnerJobExecution
	err := r.db.First(&exec, "job_id = ?", jobID).Error
	if err != nil {
		return nil, err
	}
	return &exec, nil
}

// ListByRunner lists job executions for a runner.
func (r *RunnerJobExecutionRepository) ListByRunner(runnerID uuid.UUID, limit int) ([]models.RunnerJobExecution, error) {
	var execs []models.RunnerJobExecution
	q := r.db.Where("runner_id = ?", runnerID).Order("created_at DESC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	err := q.Find(&execs).Error
	return execs, err
}

// Update updates a job execution record.
func (r *RunnerJobExecutionRepository) Update(exec *models.RunnerJobExecution) error {
	return r.db.Save(exec).Error
}

// UpdateStatus updates the status of a job execution.
func (r *RunnerJobExecutionRepository) UpdateStatus(id uuid.UUID, status models.JobExecutionStatus, errorMsg string) error {
	updates := map[string]interface{}{"status": status}
	if errorMsg != "" {
		updates["error_message"] = errorMsg
	}
	if status == models.JobExecutionStatusRunning {
		now := time.Now()
		updates["started_at"] = now
	}
	if status == models.JobExecutionStatusCompleted || status == models.JobExecutionStatusFailed || status == models.JobExecutionStatusCanceled {
		now := time.Now()
		updates["finished_at"] = now
	}
	return r.db.Model(&models.RunnerJobExecution{}).Where("id = ?", id).Updates(updates).Error
}

// CountActiveByRunner counts active (pending/running) jobs for a runner.
func (r *RunnerJobExecutionRepository) CountActiveByRunner(runnerID uuid.UUID) (int64, error) {
	var count int64
	err := r.db.Model(&models.RunnerJobExecution{}).
		Where("runner_id = ? AND status IN (?, ?)", runnerID, models.JobExecutionStatusPending, models.JobExecutionStatusRunning).
		Count(&count).Error
	return count, err
}

// AnsibleConfigRepository handles ansible configuration storage

type AnsibleConfigRepository struct {
	db *gorm.DB
}

func NewAnsibleConfigRepository(db *gorm.DB) *AnsibleConfigRepository {
	return &AnsibleConfigRepository{db: db}
}

// GetForWorkspace returns the ansible config for a workspace, falling back to project then org.
func (r *AnsibleConfigRepository) GetForWorkspace(workspaceID string, projectID, orgID uuid.UUID) (*models.AnsibleConfig, error) {
	var config models.AnsibleConfig

	// Try workspace first
	err := r.db.First(&config, "workspace_id = ?", workspaceID).Error
	if err == nil {
		return &config, nil
	}
	if err != gorm.ErrRecordNotFound {
		return nil, err
	}

	// Try project
	err = r.db.First(&config, "project_id = ? AND workspace_id IS NULL", projectID).Error
	if err == nil {
		return &config, nil
	}
	if err != gorm.ErrRecordNotFound {
		return nil, err
	}

	// Try organization
	err = r.db.First(&config, "organization_id = ? AND project_id IS NULL AND workspace_id IS NULL", orgID).Error
	if err == nil {
		return &config, nil
	}

	return nil, err
}

// GetByOrganization returns the org-level ansible config.
func (r *AnsibleConfigRepository) GetByOrganization(orgID uuid.UUID) (*models.AnsibleConfig, error) {
	var config models.AnsibleConfig
	err := r.db.First(&config, "organization_id = ? AND project_id IS NULL AND workspace_id IS NULL", orgID).Error
	if err != nil {
		return nil, err
	}
	return &config, nil
}

// GetByProject returns the project-level ansible config.
func (r *AnsibleConfigRepository) GetByProject(projectID uuid.UUID) (*models.AnsibleConfig, error) {
	var config models.AnsibleConfig
	err := r.db.First(&config, "project_id = ? AND workspace_id IS NULL", projectID).Error
	if err != nil {
		return nil, err
	}
	return &config, nil
}

// GetByWorkspace returns the workspace-level ansible config.
func (r *AnsibleConfigRepository) GetByWorkspace(workspaceID string) (*models.AnsibleConfig, error) {
	var config models.AnsibleConfig
	err := r.db.First(&config, "workspace_id = ?", workspaceID).Error
	if err != nil {
		return nil, err
	}
	return &config, nil
}

// Upsert creates or updates an ansible config.
func (r *AnsibleConfigRepository) Upsert(config *models.AnsibleConfig) error {
	// Check if exists based on scope
	var existing models.AnsibleConfig
	var err error

	switch {
	case config.WorkspaceID != nil:
		err = r.db.First(&existing, "workspace_id = ?", *config.WorkspaceID).Error
	case config.ProjectID != nil:
		err = r.db.First(&existing, "project_id = ? AND workspace_id IS NULL", *config.ProjectID).Error
	case config.OrganizationID != nil:
		err = r.db.First(&existing, "organization_id = ? AND project_id IS NULL AND workspace_id IS NULL", *config.OrganizationID).Error
	}

	if err == gorm.ErrRecordNotFound {
		// Create new
		return r.db.Create(config).Error
	}
	if err != nil {
		return err
	}

	// Update existing
	existing.ConfigContent = config.ConfigContent
	existing.UpdatedByID = config.UpdatedByID
	return r.db.Save(&existing).Error
}

// Delete deletes an ansible config.
func (r *AnsibleConfigRepository) Delete(id uuid.UUID) error {
	return r.db.Delete(&models.AnsibleConfig{}, "id = ?", id).Error
}
