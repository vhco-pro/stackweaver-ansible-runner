// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type AgentPoolRepository struct {
	db *gorm.DB
}

func NewAgentPoolRepository(db *gorm.DB) *AgentPoolRepository {
	return &AgentPoolRepository{db: db}
}

// Create creates an agent pool.
func (r *AgentPoolRepository) Create(pool *models.AgentPool) error {
	return r.db.Create(pool).Error
}

// GetByID returns an agent pool by ID, optionally with relations.
func (r *AgentPoolRepository) GetByID(id uuid.UUID, preloadRelations bool) (*models.AgentPool, error) {
	var pool models.AgentPool
	q := r.db
	if preloadRelations {
		q = q.Preload("Organization").Preload("AllowedWorkspaces").Preload("AllowedProjects").Preload("ExcludedWorkspaces")
	}
	err := q.First(&pool, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &pool, nil
}

// GetByOrganizationAndName returns an agent pool by org and name (for import/lookup).
func (r *AgentPoolRepository) GetByOrganizationAndName(orgID uuid.UUID, name string) (*models.AgentPool, error) {
	var pool models.AgentPool
	err := r.db.First(&pool, "organization_id = ? AND name = ?", orgID, name).Error
	if err != nil {
		return nil, err
	}
	return &pool, nil
}

// ListByOrganization lists agent pools for an org with optional filters and pagination.
func (r *AgentPoolRepository) ListByOrganization(orgID uuid.UUID, opts ListAgentPoolsOptions) ([]models.AgentPool, int64, error) {
	var pools []models.AgentPool
	var total int64

	q := r.db.Model(&models.AgentPool{}).Where("organization_id = ?", orgID)
	if opts.Query != "" {
		q = q.Where("name ILIKE ?", "%"+opts.Query+"%")
	}
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	q = r.db.Where("organization_id = ?", orgID)
	if opts.Query != "" {
		q = q.Where("name ILIKE ?", "%"+opts.Query+"%")
	}
	switch opts.Sort {
	case "name":
		q = q.Order("name")
	case "created-at", "created_at":
		q = q.Order("created_at DESC")
	default:
		q = q.Order("created_at DESC")
	}
	if opts.Limit > 0 {
		q = q.Limit(opts.Limit)
	}
	if opts.Offset > 0 {
		q = q.Offset(opts.Offset)
	}
	err := q.Preload("Organization").Preload("AllowedWorkspaces").Preload("AllowedProjects").Preload("ExcludedWorkspaces").Find(&pools).Error
	return pools, total, err
}

// ListAgentPoolsOptions holds list filters and pagination.
type ListAgentPoolsOptions struct {
	AllowedWorkspaceName string
	AllowedProjectName   string
	Query                string
	Sort                 string
	Limit                int
	Offset               int
}

// Update updates an agent pool (name, organization_scoped). Does not touch relations.
func (r *AgentPoolRepository) Update(pool *models.AgentPool) error {
	return r.db.Model(pool).Updates(map[string]interface{}{
		"name":                pool.Name,
		"organization_scoped": pool.OrganizationScoped,
	}).Error
}

// ReplaceAllowedWorkspaces replaces the allowed-workspaces set for a pool.
func (r *AgentPoolRepository) ReplaceAllowedWorkspaces(poolID uuid.UUID, workspaceIDs []string) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM agent_pool_allowed_workspaces WHERE agent_pool_id = ?", poolID).Error; err != nil {
			return err
		}
		for _, wid := range workspaceIDs {
			if err := tx.Exec("INSERT INTO agent_pool_allowed_workspaces (agent_pool_id, workspace_id) VALUES (?, ?)", poolID, wid).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// ReplaceAllowedProjects replaces the allowed-projects set for a pool.
func (r *AgentPoolRepository) ReplaceAllowedProjects(poolID uuid.UUID, projectIDs []uuid.UUID) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM agent_pool_allowed_projects WHERE agent_pool_id = ?", poolID).Error; err != nil {
			return err
		}
		for _, pid := range projectIDs {
			if err := tx.Exec("INSERT INTO agent_pool_allowed_projects (agent_pool_id, project_id) VALUES (?, ?)", poolID, pid).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// ReplaceExcludedWorkspaces replaces the excluded-workspaces set for a pool.
func (r *AgentPoolRepository) ReplaceExcludedWorkspaces(poolID uuid.UUID, workspaceIDs []string) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM agent_pool_excluded_workspaces WHERE agent_pool_id = ?", poolID).Error; err != nil {
			return err
		}
		for _, wid := range workspaceIDs {
			if err := tx.Exec("INSERT INTO agent_pool_excluded_workspaces (agent_pool_id, workspace_id) VALUES (?, ?)", poolID, wid).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// Delete deletes an agent pool. Join tables are removed via FK cascade or we delete explicitly.
func (r *AgentPoolRepository) Delete(id uuid.UUID) error {
	// GORM many2many uses join tables; we must clear them before deleting the pool if no CASCADE.
	_ = r.db.Exec("DELETE FROM agent_pool_allowed_workspaces WHERE agent_pool_id = ?", id).Error
	_ = r.db.Exec("DELETE FROM agent_pool_allowed_projects WHERE agent_pool_id = ?", id).Error
	_ = r.db.Exec("DELETE FROM agent_pool_excluded_workspaces WHERE agent_pool_id = ?", id).Error
	res := r.db.Delete(&models.AgentPool{}, "id = ?", id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}
