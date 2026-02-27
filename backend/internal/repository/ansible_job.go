// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type AnsibleJobRepository struct {
	db *gorm.DB
}

func NewAnsibleJobRepository(db *gorm.DB) *AnsibleJobRepository {
	return &AnsibleJobRepository{db: db}
}

func (r *AnsibleJobRepository) Create(job *models.AnsibleJob) error {
	return r.db.Create(job).Error
}

func (r *AnsibleJobRepository) GetByID(id uuid.UUID) (*models.AnsibleJob, error) {
	var job models.AnsibleJob
	err := r.db.
		Preload("Project").
		Preload("Project.Organization").
		Preload("Playbook").
		Preload("Inventory").
		Preload("Template").
		Preload("Credential").
		Preload("AgentPool").
		Preload("Runner").
		First(&job, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &job, nil
}

func (r *AnsibleJobRepository) ListByProject(projectID uuid.UUID, limit, offset int) ([]models.AnsibleJob, int64, error) {
	var jobs []models.AnsibleJob
	var total int64

	if err := r.db.Model(&models.AnsibleJob{}).Where("project_id = ?", projectID).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("project_id = ?", projectID).
		Preload("Playbook").
		Preload("Inventory").
		Preload("AgentPool").
		Preload("Runner").
		Order("created_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&jobs).Error
	return jobs, total, err
}

func (r *AnsibleJobRepository) ListByOrganization(orgID uuid.UUID, limit, offset int) ([]models.AnsibleJob, int64, error) {
	var jobs []models.AnsibleJob
	var total int64

	query := r.db.Model(&models.AnsibleJob{}).
		Joins("JOIN projects ON ansible_jobs.project_id = projects.id").
		Where("projects.organization_id = ?", orgID)

	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := query.
		Preload("Project").
		Preload("Playbook").
		Preload("Inventory").
		Preload("AgentPool").
		Preload("Runner").
		Order("ansible_jobs.created_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&jobs).Error
	return jobs, total, err
}

// ListByUser lists Ansible jobs created by a specific user across all organizations
func (r *AnsibleJobRepository) ListByUser(userID uuid.UUID, limit, offset int) ([]models.AnsibleJob, int64, error) {
	var jobs []models.AnsibleJob
	var total int64

	query := r.db.Model(&models.AnsibleJob{}).Where("created_by = ?", userID)

	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := query.
		Preload("Project").
		Preload("Project.Organization").
		Preload("Playbook").
		Preload("Inventory").
		Preload("AgentPool").
		Preload("Runner").
		Order("created_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&jobs).Error
	return jobs, total, err
}

// ListByOrganizationAndUser lists Ansible jobs for an organization filtered by user
func (r *AnsibleJobRepository) ListByOrganizationAndUser(organizationID, userID uuid.UUID, limit, offset int) ([]models.AnsibleJob, int64, error) {
	var jobs []models.AnsibleJob
	var total int64

	// Join with projects to filter by organization, and filter by user
	query := r.db.Model(&models.AnsibleJob{}).
		Joins("JOIN projects ON ansible_jobs.project_id = projects.id").
		Where("projects.organization_id = ?", organizationID).
		Where("ansible_jobs.created_by = ?", userID)

	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := query.
		Preload("Project").
		Preload("Playbook").
		Preload("Inventory").
		Preload("AgentPool").
		Preload("Runner").
		Order("ansible_jobs.created_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&jobs).Error
	return jobs, total, err
}

func (r *AnsibleJobRepository) ListByPlaybook(playbookID uuid.UUID, limit, offset int) ([]models.AnsibleJob, int64, error) {
	var jobs []models.AnsibleJob
	var total int64

	if err := r.db.Model(&models.AnsibleJob{}).Where("playbook_id = ?", playbookID).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("playbook_id = ?", playbookID).
		Preload("Inventory").
		Order("created_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&jobs).Error
	return jobs, total, err
}

func (r *AnsibleJobRepository) ListByInventory(inventoryID uuid.UUID, limit, offset int) ([]models.AnsibleJob, int64, error) {
	var jobs []models.AnsibleJob
	var total int64

	if err := r.db.Model(&models.AnsibleJob{}).Where("inventory_id = ?", inventoryID).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("inventory_id = ?", inventoryID).
		Preload("Playbook").
		Order("created_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&jobs).Error
	return jobs, total, err
}

func (r *AnsibleJobRepository) ListByStatus(status models.AnsibleJobStatus, limit int) ([]models.AnsibleJob, error) {
	var jobs []models.AnsibleJob
	err := r.db.Where("status = ?", status).
		Preload("Project").
		Preload("Playbook").
		Preload("Inventory").
		Order("created_at ASC").
		Limit(limit).
		Find(&jobs).Error
	return jobs, err
}

func (r *AnsibleJobRepository) ListQueued(orgID uuid.UUID, limit int) ([]models.AnsibleJob, error) {
	var jobs []models.AnsibleJob

	err := r.db.Model(&models.AnsibleJob{}).
		Joins("JOIN projects ON ansible_jobs.project_id = projects.id").
		Where("projects.organization_id = ?", orgID).
		Where("ansible_jobs.status IN ?", []models.AnsibleJobStatus{
			models.AnsibleJobStatusPending,
			models.AnsibleJobStatusRunning,
		}).
		Preload("Project").
		Preload("Playbook").
		Preload("Inventory").
		Order("ansible_jobs.created_at ASC").
		Limit(limit).
		Find(&jobs).Error
	return jobs, err
}

func (r *AnsibleJobRepository) Update(job *models.AnsibleJob) error {
	return r.db.Save(job).Error
}

func (r *AnsibleJobRepository) Delete(id uuid.UUID) error {
	return r.db.Delete(&models.AnsibleJob{}, "id = ?", id).Error
}

// DeleteByTemplateID deletes all jobs associated with a job template
func (r *AnsibleJobRepository) DeleteByTemplateID(templateID uuid.UUID) error {
	// First delete all job events for jobs belonging to this template
	subQuery := r.db.Model(&models.AnsibleJob{}).Select("id").Where("template_id = ?", templateID)
	if err := r.db.Where("job_id IN (?)", subQuery).Delete(&models.AnsibleJobEvent{}).Error; err != nil {
		return err
	}
	// Then delete the jobs themselves
	return r.db.Delete(&models.AnsibleJob{}, "template_id = ?", templateID).Error
}

// Event operations

func (r *AnsibleJobRepository) CreateEvent(event *models.AnsibleJobEvent) error {
	return r.db.Create(event).Error
}

// GetMaxEventCounter returns the highest event counter for a job, or 0 if no events exist
func (r *AnsibleJobRepository) GetMaxEventCounter(jobID uuid.UUID) (int, error) {
	var maxCounter *int
	err := r.db.Model(&models.AnsibleJobEvent{}).
		Where("job_id = ?", jobID).
		Select("MAX(counter)").
		Scan(&maxCounter).Error
	if err != nil {
		return 0, err
	}
	if maxCounter == nil {
		return 0, nil
	}
	return *maxCounter, nil
}

func (r *AnsibleJobRepository) CreateEventsBatch(events []models.AnsibleJobEvent) error {
	if len(events) == 0 {
		return nil
	}
	return r.db.CreateInBatches(events, 100).Error
}

func (r *AnsibleJobRepository) GetEventByID(id uuid.UUID) (*models.AnsibleJobEvent, error) {
	var event models.AnsibleJobEvent
	err := r.db.First(&event, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &event, nil
}

func (r *AnsibleJobRepository) ListEventsByJob(jobID uuid.UUID, limit, offset int) ([]models.AnsibleJobEvent, int64, error) {
	var events []models.AnsibleJobEvent
	var total int64

	if err := r.db.Model(&models.AnsibleJobEvent{}).Where("job_id = ?", jobID).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("job_id = ?", jobID).
		Order("counter ASC").
		Limit(limit).
		Offset(offset).
		Find(&events).Error
	return events, total, err
}

func (r *AnsibleJobRepository) ListEventsByJobAndType(jobID uuid.UUID, eventType string, limit, offset int) ([]models.AnsibleJobEvent, int64, error) {
	var events []models.AnsibleJobEvent
	var total int64

	query := r.db.Model(&models.AnsibleJobEvent{}).
		Where("job_id = ? AND event = ?", jobID, eventType)

	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := query.
		Order("counter ASC").
		Limit(limit).
		Offset(offset).
		Find(&events).Error
	return events, total, err
}

func (r *AnsibleJobRepository) GetEventCount(jobID uuid.UUID) (int64, error) {
	var count int64
	err := r.db.Model(&models.AnsibleJobEvent{}).Where("job_id = ?", jobID).Count(&count).Error
	return count, err
}

func (r *AnsibleJobRepository) GetLastEventCounter(jobID uuid.UUID) (int, error) {
	var event models.AnsibleJobEvent
	err := r.db.Where("job_id = ?", jobID).Order("counter DESC").First(&event).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return 0, nil
		}
		return 0, err
	}
	return event.Counter, nil
}

func (r *AnsibleJobRepository) DeleteEventsByJob(jobID uuid.UUID) error {
	return r.db.Delete(&models.AnsibleJobEvent{}, "job_id = ?", jobID).Error
}
