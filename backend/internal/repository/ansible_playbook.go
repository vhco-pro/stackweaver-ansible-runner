// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/michielvha/logger"
	"gorm.io/gorm"
)

type AnsiblePlaybookRepository struct {
	db *gorm.DB
}

func NewAnsiblePlaybookRepository(db *gorm.DB) *AnsiblePlaybookRepository {
	return &AnsiblePlaybookRepository{db: db}
}

func (r *AnsiblePlaybookRepository) Create(playbook *models.AnsiblePlaybook) error {
	return r.db.Create(playbook).Error
}

func (r *AnsiblePlaybookRepository) GetByID(id uuid.UUID) (*models.AnsiblePlaybook, error) {
	var playbook models.AnsiblePlaybook
	err := r.db.Preload("Project").Preload("Project.Organization").Preload("VCSConnection").
		First(&playbook, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &playbook, nil
}

func (r *AnsiblePlaybookRepository) GetByProjectAndName(projectID uuid.UUID, name string) (*models.AnsiblePlaybook, error) {
	var playbook models.AnsiblePlaybook
	err := r.db.First(&playbook, "project_id = ? AND name = ?", projectID, name).Error
	if err != nil {
		return nil, err
	}
	return &playbook, nil
}

func (r *AnsiblePlaybookRepository) ListByProject(projectID uuid.UUID, limit, offset int) ([]models.AnsiblePlaybook, int64, error) {
	var playbooks []models.AnsiblePlaybook
	var total int64

	if err := r.db.Model(&models.AnsiblePlaybook{}).Where("project_id = ?", projectID).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("project_id = ?", projectID).
		Preload("VCSConnection").
		Order("name ASC").
		Limit(limit).
		Offset(offset).
		Find(&playbooks).Error
	return playbooks, total, err
}

func (r *AnsiblePlaybookRepository) ListByOrganization(orgID uuid.UUID, limit, offset int) ([]models.AnsiblePlaybook, int64, error) {
	var playbooks []models.AnsiblePlaybook
	var total int64

	query := r.db.Model(&models.AnsiblePlaybook{}).
		Joins("JOIN projects ON ansible_playbooks.project_id = projects.id").
		Where("projects.organization_id = ?", orgID)

	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := query.
		Preload("Project").
		Preload("VCSConnection").
		Order("ansible_playbooks.name ASC").
		Limit(limit).
		Offset(offset).
		Find(&playbooks).Error
	return playbooks, total, err
}

// ListAll lists all playbooks across all organizations
func (r *AnsiblePlaybookRepository) ListAll(limit, offset int) ([]models.AnsiblePlaybook, int64, error) {
	var playbooks []models.AnsiblePlaybook
	var total int64

	if err := r.db.Model(&models.AnsiblePlaybook{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Preload("Project").
		Order("created_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&playbooks).Error
	return playbooks, total, err
}

// ListByVCSRepository lists all playbooks that use a specific VCS repository
func (r *AnsiblePlaybookRepository) ListByVCSRepository(repoFullName string) ([]models.AnsiblePlaybook, error) {
	var playbooks []models.AnsiblePlaybook
	err := r.db.Where("vcs_repository = ?", repoFullName).
		Preload("Project").
		Find(&playbooks).Error
	return playbooks, err
}

// ListByVCSRepositoryAndBranch lists playbooks matching repo and branch
func (r *AnsiblePlaybookRepository) ListByVCSRepositoryAndBranch(repoFullName, branch string) ([]models.AnsiblePlaybook, error) {
	var playbooks []models.AnsiblePlaybook
	err := r.db.Where("vcs_repository = ? AND (vcs_branch = ? OR vcs_branch = '')", repoFullName, branch).
		Preload("Project").
		Find(&playbooks).Error
	return playbooks, err
}

func (r *AnsiblePlaybookRepository) Update(playbook *models.AnsiblePlaybook) error {
	return r.db.Save(playbook).Error
}

// CountJobTemplatesByPlaybook counts job templates using this playbook
func (r *AnsiblePlaybookRepository) CountJobTemplatesByPlaybook(id uuid.UUID) (int64, error) {
	var count int64
	err := r.db.Model(&models.AnsibleJobTemplate{}).Where("playbook_id = ?", id).Count(&count).Error
	return count, err
}

// CountJobsByPlaybook counts jobs using this playbook
func (r *AnsiblePlaybookRepository) CountJobsByPlaybook(id uuid.UUID) (int64, error) {
	var count int64
	err := r.db.Model(&models.AnsibleJob{}).Where("playbook_id = ?", id).Count(&count).Error
	return count, err
}

func (r *AnsiblePlaybookRepository) Delete(id uuid.UUID) error {
	return r.db.Delete(&models.AnsiblePlaybook{}, "id = ?", id).Error
}

// Job Template operations

type AnsibleJobTemplateRepository struct {
	db *gorm.DB
}

func NewAnsibleJobTemplateRepository(db *gorm.DB) *AnsibleJobTemplateRepository {
	return &AnsibleJobTemplateRepository{db: db}
}

func (r *AnsibleJobTemplateRepository) Create(template *models.AnsibleJobTemplate) error {
	return r.db.Create(template).Error
}

func (r *AnsibleJobTemplateRepository) GetByID(id uuid.UUID) (*models.AnsibleJobTemplate, error) {
	var template models.AnsibleJobTemplate
	err := r.db.Preload("Project").Preload("Playbook").Preload("Inventory").Preload("Credential").
		First(&template, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &template, nil
}

func (r *AnsibleJobTemplateRepository) GetByProjectAndName(projectID uuid.UUID, name string) (*models.AnsibleJobTemplate, error) {
	var template models.AnsibleJobTemplate
	err := r.db.First(&template, "project_id = ? AND name = ?", projectID, name).Error
	if err != nil {
		return nil, err
	}
	return &template, nil
}

func (r *AnsibleJobTemplateRepository) ListByProject(projectID uuid.UUID, limit, offset int) ([]models.AnsibleJobTemplate, int64, error) {
	var templates []models.AnsibleJobTemplate
	var total int64

	if err := r.db.Model(&models.AnsibleJobTemplate{}).Where("project_id = ?", projectID).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("project_id = ?", projectID).
		Preload("Playbook").
		Preload("Inventory").
		Order("name ASC").
		Limit(limit).
		Offset(offset).
		Find(&templates).Error
	return templates, total, err
}

func (r *AnsibleJobTemplateRepository) ListByOrganization(orgID uuid.UUID, limit, offset int) ([]models.AnsibleJobTemplate, int64, error) {
	var templates []models.AnsibleJobTemplate
	var total int64

	query := r.db.Model(&models.AnsibleJobTemplate{}).
		Joins("JOIN projects ON ansible_job_templates.project_id = projects.id").
		Where("projects.organization_id = ?", orgID)

	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := query.
		Preload("Project").
		Preload("Playbook").
		Preload("Inventory").
		Preload("Credential").
		Order("ansible_job_templates.name ASC").
		Limit(limit).
		Offset(offset).
		Find(&templates).Error
	return templates, total, err
}

func (r *AnsibleJobTemplateRepository) ListByPlaybook(playbookID uuid.UUID, limit, offset int) ([]models.AnsibleJobTemplate, int64, error) {
	var templates []models.AnsibleJobTemplate
	var total int64

	if err := r.db.Model(&models.AnsibleJobTemplate{}).Where("playbook_id = ?", playbookID).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("playbook_id = ?", playbookID).
		Preload("Inventory").
		Order("name ASC").
		Limit(limit).
		Offset(offset).
		Find(&templates).Error
	return templates, total, err
}

func (r *AnsibleJobTemplateRepository) Update(template *models.AnsibleJobTemplate) error {
	logger.Debugf("AnsibleJobTemplateRepository.Update: Saving template ID=%s, InventoryID=%s, CredentialID=%v",
		template.ID.String(), template.InventoryID.String(), template.CredentialID)

	// GORM's Updates() with preloaded relationships can ignore foreign key fields in the map
	// Use explicit updates map so all fields including agent_pool_id are persisted
	updates := map[string]interface{}{
		"name":             template.Name,
		"description":      template.Description,
		"extra_vars":       template.ExtraVars,
		"limit":            template.Limit,
		"tags":             template.Tags,
		"skip_tags":        template.SkipTags,
		"verbosity":        template.Verbosity,
		"forks":            template.Forks,
		"become_enabled":   template.BecomeEnabled,
		"diff_mode":        template.DiffMode,
		"schedule_enabled": template.ScheduleEnabled,
		"schedule_cron":    template.ScheduleCron,
		"inventory_id":     template.InventoryID,
		"credential_id":    template.CredentialID,
		"agent_pool_id":    template.AgentPoolID,
		"updated_at":       template.UpdatedAt,
	}

	// GORM's Updates() with preloaded relationships uses relationship data instead of field values
	// Use a fresh model instance and Omit() to exclude relationships so GORM uses the field values
	err := r.db.Model(&models.AnsibleJobTemplate{}).
		Where("id = ?", template.ID).
		Omit("Project", "Playbook", "Inventory", "Credential", "AgentPool").
		Updates(updates).Error
	if err != nil {
		logger.Debugf("AnsibleJobTemplateRepository.Update: Updates error: %v", err)
	} else {
		logger.Debugf("AnsibleJobTemplateRepository.Update: Updates successful")
		// Verify what was actually saved by querying the database
		var savedTemplate models.AnsibleJobTemplate
		if queryErr := r.db.Select("inventory_id", "credential_id").First(&savedTemplate, "id = ?", template.ID).Error; queryErr == nil {
			logger.Debugf("AnsibleJobTemplateRepository.Update: After Updates - DB has InventoryID=%s, CredentialID=%v",
				savedTemplate.InventoryID.String(), savedTemplate.CredentialID)
		}
	}
	return err
}

func (r *AnsibleJobTemplateRepository) Delete(id uuid.UUID) error {
	return r.db.Delete(&models.AnsibleJobTemplate{}, "id = ?", id).Error
}
