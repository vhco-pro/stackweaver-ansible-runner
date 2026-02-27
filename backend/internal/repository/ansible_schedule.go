// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"time"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

// AnsibleScheduleRepository handles schedule database operations
type AnsibleScheduleRepository struct {
	db *gorm.DB
}

// NewAnsibleScheduleRepository creates a new schedule repository
func NewAnsibleScheduleRepository(db *gorm.DB) *AnsibleScheduleRepository {
	return &AnsibleScheduleRepository{db: db}
}

// Create creates a new schedule
func (r *AnsibleScheduleRepository) Create(schedule *models.AnsibleSchedule) error {
	return r.db.Create(schedule).Error
}

// GetByID retrieves a schedule by ID
func (r *AnsibleScheduleRepository) GetByID(id uuid.UUID) (*models.AnsibleSchedule, error) {
	var schedule models.AnsibleSchedule
	err := r.db.Preload("Organization").
		Preload("JobTemplate").
		Preload("InventorySource").
		Preload("Playbook").
		First(&schedule, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &schedule, nil
}

// ListByOrganization lists all schedules for an organization
func (r *AnsibleScheduleRepository) ListByOrganization(orgID uuid.UUID, limit, offset int) ([]models.AnsibleSchedule, int64, error) {
	var schedules []models.AnsibleSchedule
	var total int64

	if err := r.db.Model(&models.AnsibleSchedule{}).
		Where("organization_id = ?", orgID).
		Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("organization_id = ?", orgID).
		Preload("JobTemplate").
		Preload("InventorySource").
		Preload("Playbook").
		Order("name ASC").
		Limit(limit).
		Offset(offset).
		Find(&schedules).Error
	return schedules, total, err
}

// ListByType lists schedules by type
func (r *AnsibleScheduleRepository) ListByType(scheduleType models.ScheduleType, limit, offset int) ([]models.AnsibleSchedule, int64, error) {
	var schedules []models.AnsibleSchedule
	var total int64

	if err := r.db.Model(&models.AnsibleSchedule{}).
		Where("type = ?", scheduleType).
		Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("type = ?", scheduleType).
		Preload("Organization").
		Preload("JobTemplate").
		Preload("InventorySource").
		Preload("Playbook").
		Order("name ASC").
		Limit(limit).
		Offset(offset).
		Find(&schedules).Error
	return schedules, total, err
}

// ListEnabled lists all enabled schedules
func (r *AnsibleScheduleRepository) ListEnabled() ([]models.AnsibleSchedule, error) {
	var schedules []models.AnsibleSchedule
	now := time.Now()
	err := r.db.Where("status = ?", models.ScheduleStatusEnabled).
		Where("(start_date_time IS NULL OR start_date_time <= ?)", now).
		Where("(end_date_time IS NULL OR end_date_time >= ?)", now).
		Preload("Organization").
		Preload("JobTemplate").
		Preload("InventorySource").
		Preload("Playbook").
		Find(&schedules).Error
	return schedules, err
}

// ListDue lists schedules that are due for execution (next_run_at <= now)
func (r *AnsibleScheduleRepository) ListDue() ([]models.AnsibleSchedule, error) {
	var schedules []models.AnsibleSchedule
	now := time.Now()
	err := r.db.Where("status = ?", models.ScheduleStatusEnabled).
		Where("(start_date_time IS NULL OR start_date_time <= ?)", now).
		Where("(end_date_time IS NULL OR end_date_time >= ?)", now).
		Where("next_run_at <= ?", now).
		Preload("Organization").
		Preload("JobTemplate").
		Preload("InventorySource").
		Preload("Playbook").
		Find(&schedules).Error
	return schedules, err
}

// ListByJobTemplate lists schedules for a specific job template
func (r *AnsibleScheduleRepository) ListByJobTemplate(templateID uuid.UUID) ([]models.AnsibleSchedule, error) {
	var schedules []models.AnsibleSchedule
	err := r.db.Where("job_template_id = ?", templateID).
		Order("name ASC").
		Find(&schedules).Error
	return schedules, err
}

// ListByInventorySource lists schedules for a specific inventory source
func (r *AnsibleScheduleRepository) ListByInventorySource(sourceID uuid.UUID) ([]models.AnsibleSchedule, error) {
	var schedules []models.AnsibleSchedule
	err := r.db.Where("inventory_source_id = ?", sourceID).
		Order("name ASC").
		Find(&schedules).Error
	return schedules, err
}

// ListByPlaybook lists schedules for a specific playbook
func (r *AnsibleScheduleRepository) ListByPlaybook(playbookID uuid.UUID) ([]models.AnsibleSchedule, error) {
	var schedules []models.AnsibleSchedule
	err := r.db.Where("playbook_id = ?", playbookID).
		Order("name ASC").
		Find(&schedules).Error
	return schedules, err
}

// Update updates a schedule
func (r *AnsibleScheduleRepository) Update(schedule *models.AnsibleSchedule) error {
	return r.db.Save(schedule).Error
}

// UpdateNextRun updates the next_run_at field
func (r *AnsibleScheduleRepository) UpdateNextRun(id uuid.UUID, nextRunAt time.Time) error {
	return r.db.Model(&models.AnsibleSchedule{}).
		Where("id = ?", id).
		Update("next_run_at", nextRunAt).Error
}

// UpdateLastRun updates the last run information
func (r *AnsibleScheduleRepository) UpdateLastRun(id uuid.UUID, lastRunAt time.Time, jobID *uuid.UUID, status string) error {
	updates := map[string]interface{}{
		"last_run_at":     lastRunAt,
		"last_job_id":     jobID,
		"last_run_status": status,
		"run_count":       gorm.Expr("run_count + 1"),
	}
	return r.db.Model(&models.AnsibleSchedule{}).
		Where("id = ?", id).
		Updates(updates).Error
}

// UpdateStatus updates the status of a schedule
func (r *AnsibleScheduleRepository) UpdateStatus(id uuid.UUID, status models.ScheduleStatus) error {
	return r.db.Model(&models.AnsibleSchedule{}).
		Where("id = ?", id).
		Update("status", status).Error
}

// Delete deletes a schedule
func (r *AnsibleScheduleRepository) Delete(id uuid.UUID) error {
	return r.db.Delete(&models.AnsibleSchedule{}, "id = ?", id).Error
}

// DeleteByOrganization deletes all schedules for an organization
func (r *AnsibleScheduleRepository) DeleteByOrganization(orgID uuid.UUID) error {
	return r.db.Delete(&models.AnsibleSchedule{}, "organization_id = ?", orgID).Error
}

// DeleteByJobTemplate deletes all schedules for a job template
func (r *AnsibleScheduleRepository) DeleteByJobTemplate(templateID uuid.UUID) error {
	return r.db.Delete(&models.AnsibleSchedule{}, "job_template_id = ?", templateID).Error
}

// GetByOrganizationAndName gets a schedule by organization and name
func (r *AnsibleScheduleRepository) GetByOrganizationAndName(orgID uuid.UUID, name string) (*models.AnsibleSchedule, error) {
	var schedule models.AnsibleSchedule
	err := r.db.First(&schedule, "organization_id = ? AND name = ?", orgID, name).Error
	if err != nil {
		return nil, err
	}
	return &schedule, nil
}
