// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

// deleteProjectCascade deletes all resources that reference a project (workspaces, Ansible resources, etc.)
// Must be called within a transaction before deleting the project.
func deleteProjectCascade(tx *gorm.DB, projectID uuid.UUID) error {
	var workspaceIDs []string
	if err := tx.Model(&models.Workspace{}).Where("project_id = ?", projectID).Pluck("id", &workspaceIDs).Error; err != nil {
		return fmt.Errorf("failed to get workspace IDs: %w", err)
	}
	for _, wsID := range workspaceIDs {
		if err := deleteWorkspaceCascade(tx, wsID); err != nil {
			return err
		}
	}

	// Delete Ansible resources: jobs -> job template variables -> job templates -> playbooks
	var playbookIDs []uuid.UUID
	if err := tx.Model(&models.AnsiblePlaybook{}).Where("project_id = ?", projectID).Pluck("id", &playbookIDs).Error; err != nil {
		return fmt.Errorf("failed to get playbook IDs: %w", err)
	}
	for _, playbookID := range playbookIDs {
		var jobIDs []uuid.UUID
		if err := tx.Model(&models.AnsibleJob{}).Where("playbook_id = ?", playbookID).Pluck("id", &jobIDs).Error; err != nil {
			return fmt.Errorf("failed to get job IDs: %w", err)
		}
		for _, jobID := range jobIDs {
			if err := tx.Where("job_id = ?", jobID).Delete(&models.AnsibleJobEvent{}).Error; err != nil {
				return fmt.Errorf("failed to delete job events: %w", err)
			}
		}
		if err := tx.Where("playbook_id = ?", playbookID).Delete(&models.AnsibleJob{}).Error; err != nil {
			return fmt.Errorf("failed to delete jobs: %w", err)
		}
	}

	var templateIDs []uuid.UUID
	if err := tx.Model(&models.AnsibleJobTemplate{}).Where("project_id = ?", projectID).Pluck("id", &templateIDs).Error; err != nil {
		return fmt.Errorf("failed to get job template IDs: %w", err)
	}
	for _, templateID := range templateIDs {
		if err := tx.Where("job_template_id = ?", templateID).Delete(&models.AnsibleJobTemplateVariable{}).Error; err != nil {
			return fmt.Errorf("failed to delete job template variables: %w", err)
		}
		if err := tx.Where("job_template_id = ?", templateID).Delete(&models.VariableSetJobTemplate{}).Error; err != nil {
			return fmt.Errorf("failed to delete variable set job template associations: %w", err)
		}
	}
	if err := tx.Where("project_id = ?", projectID).Delete(&models.AnsibleJobTemplate{}).Error; err != nil {
		return fmt.Errorf("failed to delete job templates: %w", err)
	}
	if err := tx.Where("project_id = ?", projectID).Delete(&models.AnsiblePlaybook{}).Error; err != nil {
		return fmt.Errorf("failed to delete playbooks: %w", err)
	}

	// Delete project-scoped Ansible config
	if err := tx.Where("project_id = ?", projectID).Delete(&models.AnsibleConfig{}).Error; err != nil {
		return fmt.Errorf("failed to delete ansible config: %w", err)
	}

	// Delete TeamProjectAccess (redundant if teams already deleted, but teams are deleted by org_id - safe)
	if err := tx.Where("project_id = ?", projectID).Delete(&models.TeamProjectAccess{}).Error; err != nil {
		return fmt.Errorf("failed to delete team project access: %w", err)
	}
	// Delete VariableSetProject associations
	if err := tx.Where("project_id = ?", projectID).Delete(&models.VariableSetProject{}).Error; err != nil {
		return fmt.Errorf("failed to delete variable set project associations: %w", err)
	}
	// Delete agent pool project associations
	if err := tx.Exec("DELETE FROM agent_pool_allowed_projects WHERE project_id = ?", projectID).Error; err != nil {
		return fmt.Errorf("failed to delete agent pool project associations: %w", err)
	}

	return nil
}

// deleteWorkspaceCascade deletes a workspace and all its dependent resources.
// Must be called within a transaction.
func deleteWorkspaceCascade(tx *gorm.DB, workspaceID string) error {
	if err := tx.Model(&models.StateVersion{}).
		Where("workspace_id = ? AND run_id IS NOT NULL", workspaceID).
		Update("run_id", nil).Error; err != nil {
		return fmt.Errorf("failed to clear run_id in state versions: %w", err)
	}

	var runIDs []string
	if err := tx.Model(&models.Run{}).Where("workspace_id = ?", workspaceID).Pluck("id", &runIDs).Error; err != nil {
		return fmt.Errorf("failed to get run IDs: %w", err)
	}
	if len(runIDs) > 0 {
		if err := tx.Where("run_id IN ?", runIDs).Delete(&models.RunPhaseState{}).Error; err != nil {
			return fmt.Errorf("failed to delete run phase states: %w", err)
		}
	}

	if err := tx.Where("workspace_id = ?", workspaceID).Delete(&models.Run{}).Error; err != nil {
		return fmt.Errorf("failed to delete runs: %w", err)
	}
	if err := tx.Where("workspace_id = ?", workspaceID).Delete(&models.ConfigurationVersion{}).Error; err != nil {
		return fmt.Errorf("failed to delete configuration versions: %w", err)
	}
	if err := tx.Where("workspace_id = ?", workspaceID).Delete(&models.StateVersion{}).Error; err != nil {
		return fmt.Errorf("failed to delete state versions: %w", err)
	}
	if err := tx.Where("workspace_id = ?", workspaceID).Delete(&models.Variable{}).Error; err != nil {
		return fmt.Errorf("failed to delete variables: %w", err)
	}
	if err := tx.Where("workspace_id = ?", workspaceID).Delete(&models.StateLock{}).Error; err != nil {
		return fmt.Errorf("failed to delete state locks: %w", err)
	}
	if err := tx.Where("workspace_id = ?", workspaceID).Delete(&models.VariableSetWorkspace{}).Error; err != nil {
		return fmt.Errorf("failed to delete variable set workspace associations: %w", err)
	}
	if err := tx.Where("workspace_id = ?", workspaceID).Delete(&models.TeamWorkspaceAccess{}).Error; err != nil {
		return fmt.Errorf("failed to delete team workspace access: %w", err)
	}
	if err := tx.Exec("DELETE FROM agent_pool_allowed_workspaces WHERE workspace_id = ?", workspaceID).Error; err != nil {
		return fmt.Errorf("failed to delete agent pool allowed workspace associations: %w", err)
	}
	if err := tx.Exec("DELETE FROM agent_pool_excluded_workspaces WHERE workspace_id = ?", workspaceID).Error; err != nil {
		return fmt.Errorf("failed to delete agent pool excluded workspace associations: %w", err)
	}

	return tx.Delete(&models.Workspace{}, "id = ?", workspaceID).Error
}

type OrganizationRepository struct {
	db *gorm.DB
}

func NewOrganizationRepository(db *gorm.DB) *OrganizationRepository {
	return &OrganizationRepository{db: db}
}

func (r *OrganizationRepository) Create(org *models.Organization) error {
	return r.db.Create(org).Error
}

func (r *OrganizationRepository) GetByID(id uuid.UUID) (*models.Organization, error) {
	var org models.Organization
	err := r.db.Preload("Members").Preload("Projects").First(&org, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &org, nil
}

func (r *OrganizationRepository) GetByName(name string) (*models.Organization, error) {
	var org models.Organization
	err := r.db.First(&org, "name = ?", name).Error
	if err != nil {
		return nil, err
	}
	return &org, nil
}

func (r *OrganizationRepository) List(limit, offset int) ([]models.Organization, int64, error) {
	var orgs []models.Organization
	var total int64

	if err := r.db.Model(&models.Organization{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Limit(limit).Offset(offset).Find(&orgs).Error
	return orgs, total, err
}

// ListByUser lists organizations where the user is an org member or has at least one team (TFE-compatible).
// Visibility = organization_members OR team_members, so tfe_organization_membership and team-based flows both work.
func (r *OrganizationRepository) ListByUser(userID uuid.UUID) ([]models.Organization, error) {
	var orgs []models.Organization
	// Subquery: org IDs where user is in organization_members OR has a team in that org
	err := r.db.Model(&models.Organization{}).
		Where("organizations.id IN (SELECT organization_id FROM organization_members WHERE user_id = ?) OR organizations.id IN (SELECT teams.organization_id FROM teams JOIN team_members ON team_members.team_id = teams.id WHERE team_members.user_id = ?)", userID, userID).
		Find(&orgs).Error
	return orgs, err
}

// UserInOrg returns true if the user is in organization_members or has at least one team in the org (TFE-compatible).
// Matches TFE: org membership (tfe_organization_membership) grants access; team membership also grants access.
func (r *OrganizationRepository) UserInOrg(userID, orgID uuid.UUID) (bool, error) {
	// In organization_members?
	var omCount int64
	if err := r.db.Model(&models.OrganizationMember{}).Where("organization_id = ? AND user_id = ?", orgID, userID).Count(&omCount).Error; err != nil {
		return false, err
	}
	if omCount > 0 {
		return true, nil
	}
	// In at least one team in this org?
	var tmCount int64
	err := r.db.Table("teams").
		Joins("JOIN team_members ON team_members.team_id = teams.id").
		Where("teams.organization_id = ? AND team_members.user_id = ?", orgID, userID).
		Count(&tmCount).Error
	if err != nil {
		return false, err
	}
	return tmCount > 0, nil
}

func (r *OrganizationRepository) Update(org *models.Organization) error {
	return r.db.Save(org).Error
}

func (r *OrganizationRepository) Delete(id uuid.UUID) error {
	// Use a transaction to ensure all deletions succeed or none do
	return r.db.Transaction(func(tx *gorm.DB) error {
		// Delete in order to respect foreign key constraints
		// Order matters: delete child resources before parent resources

		// 1. Delete organization members (junction table)
		if err := tx.Where("organization_id = ?", id).Delete(&models.OrganizationMember{}).Error; err != nil {
			return err
		}

		// 2. Delete VCS connections
		if err := tx.Where("organization_id = ?", id).Delete(&models.VCSConnection{}).Error; err != nil {
			return err
		}

		// 3. Delete variable sets and their associations
		var variableSetIDs []uuid.UUID
		if err := tx.Model(&models.VariableSet{}).Where("organization_id = ?", id).Pluck("id", &variableSetIDs).Error; err != nil {
			return err
		}
		if len(variableSetIDs) > 0 {
			// Delete variable set variables
			if err := tx.Where("variable_set_id IN ?", variableSetIDs).Delete(&models.VariableSetVariable{}).Error; err != nil {
				return err
			}
			// Delete workspace associations
			if err := tx.Where("variable_set_id IN ?", variableSetIDs).Delete(&models.VariableSetWorkspace{}).Error; err != nil {
				return err
			}
			// Delete project associations
			if err := tx.Where("variable_set_id IN ?", variableSetIDs).Delete(&models.VariableSetProject{}).Error; err != nil {
				return err
			}
			// Delete variable sets
			if err := tx.Where("organization_id = ?", id).Delete(&models.VariableSet{}).Error; err != nil {
				return err
			}
		}

		// 4. Delete modules (registry modules)
		if err := tx.Where("organization_id = ?", id).Delete(&models.Module{}).Error; err != nil {
			return err
		}

		// 5. Delete providers (registry providers)
		if err := tx.Where("organization_id = ?", id).Delete(&models.Provider{}).Error; err != nil {
			return err
		}

		// 6. Delete GPG keys
		if err := tx.Where("organization_id = ?", id).Delete(&models.GPGKey{}).Error; err != nil {
			return err
		}

		// 7. Delete Ansible workflows and related resources
		var workflowIDs []uuid.UUID
		if err := tx.Model(&models.AnsibleWorkflow{}).Where("organization_id = ?", id).Pluck("id", &workflowIDs).Error; err != nil {
			return err
		}
		if len(workflowIDs) > 0 {
			// Delete workflow edges
			if err := tx.Where("workflow_id IN ?", workflowIDs).Delete(&models.AnsibleWorkflowEdge{}).Error; err != nil {
				return err
			}
			// Delete workflow nodes
			if err := tx.Where("workflow_id IN ?", workflowIDs).Delete(&models.AnsibleWorkflowNode{}).Error; err != nil {
				return err
			}
			// Delete workflow jobs
			if err := tx.Where("workflow_id IN ?", workflowIDs).Delete(&models.AnsibleWorkflowJob{}).Error; err != nil {
				return err
			}
			// Delete workflows
			if err := tx.Where("organization_id = ?", id).Delete(&models.AnsibleWorkflow{}).Error; err != nil {
				return err
			}
		}

		// 8. Delete Ansible inventories and related resources
		var inventoryIDs []uuid.UUID
		if err := tx.Model(&models.AnsibleInventory{}).Where("organization_id = ?", id).Pluck("id", &inventoryIDs).Error; err != nil {
			return err
		}
		if len(inventoryIDs) > 0 {
			// Delete inventory hosts (host-group associations are handled by GORM)
			if err := tx.Where("inventory_id IN ?", inventoryIDs).Delete(&models.AnsibleInventoryHost{}).Error; err != nil {
				return err
			}
			// Delete inventory groups
			if err := tx.Where("inventory_id IN ?", inventoryIDs).Delete(&models.AnsibleInventoryGroup{}).Error; err != nil {
				return err
			}
			// Delete inventories
			if err := tx.Where("organization_id = ?", id).Delete(&models.AnsibleInventory{}).Error; err != nil {
				return err
			}
		}

		// 9. Delete Ansible schedules
		if err := tx.Where("organization_id = ?", id).Delete(&models.AnsibleSchedule{}).Error; err != nil {
			return err
		}

		// 10. Delete Ansible credentials
		if err := tx.Where("organization_id = ?", id).Delete(&models.AnsibleCredential{}).Error; err != nil {
			return err
		}

		// 11. Delete API keys (where organization_id is not null)
		if err := tx.Where("organization_id = ?", id).Delete(&models.APIKey{}).Error; err != nil {
			return err
		}

		// 12. Delete teams and their related resources
		var teamIDs []uuid.UUID
		if err := tx.Model(&models.Team{}).Where("organization_id = ?", id).Pluck("id", &teamIDs).Error; err != nil {
			return err
		}
		if len(teamIDs) > 0 {
			// Delete team members
			if err := tx.Where("team_id IN ?", teamIDs).Delete(&models.TeamMember{}).Error; err != nil {
				return err
			}
			// Delete team organization access
			if err := tx.Where("team_id IN ?", teamIDs).Delete(&models.TeamOrganizationAccess{}).Error; err != nil {
				return err
			}
			// Delete team project access
			if err := tx.Where("team_id IN ?", teamIDs).Delete(&models.TeamProjectAccess{}).Error; err != nil {
				return err
			}
			// Delete team workspace access
			if err := tx.Where("team_id IN ?", teamIDs).Delete(&models.TeamWorkspaceAccess{}).Error; err != nil {
				return err
			}
			// Delete teams
			if err := tx.Where("organization_id = ?", id).Delete(&models.Team{}).Error; err != nil {
				return err
			}
		}

		// 13. Delete project-scoped resources before projects (FK constraints)
		var projectIDs []uuid.UUID
		if err := tx.Model(&models.Project{}).Where("organization_id = ?", id).Pluck("id", &projectIDs).Error; err != nil {
			return err
		}
		for _, projectID := range projectIDs {
			if err := deleteProjectCascade(tx, projectID); err != nil {
				return err
			}
		}

		// 14. Delete projects (now safe - all children removed)
		if err := tx.Where("organization_id = ?", id).Delete(&models.Project{}).Error; err != nil {
			return err
		}

		// 15. Finally, delete the organization itself
		// Note: Audit logs are intentionally NOT deleted for compliance purposes
		return tx.Delete(&models.Organization{}, "id = ?", id).Error
	})
}

// AddMember adds a user to an organization (no role - roles are deprecated, use team memberships instead)
func (r *OrganizationRepository) AddMember(orgID, userID uuid.UUID) error {
	member := &models.OrganizationMember{
		OrganizationID: orgID,
		UserID:         userID,
		Role:           nil, // Deprecated: No role assigned, permissions come from team memberships
	}
	return r.db.Create(member).Error
}

func (r *OrganizationRepository) RemoveMember(orgID, userID uuid.UUID) error {
	return r.db.Delete(&models.OrganizationMember{}, "organization_id = ? AND user_id = ?", orgID, userID).Error
}

func (r *OrganizationRepository) GetMember(orgID, userID uuid.UUID) (*models.OrganizationMember, error) {
	var member models.OrganizationMember
	err := r.db.First(&member, "organization_id = ? AND user_id = ?", orgID, userID).Error
	if err != nil {
		return nil, err
	}
	return &member, nil
}

// UpdateMemberRole is DEPRECATED: Roles are deprecated in favor of team-based permissions.
// This method is kept for backward compatibility but is a no-op (does nothing).
// Permissions should be managed via team memberships instead.
func (r *OrganizationRepository) UpdateMemberRole(memberID uuid.UUID, role string) error {
	// No-op: Roles are deprecated, permissions come from team memberships
	// We don't update the role field (it's nullable and ignored)
	return nil
}

// ListMembers lists all members of an organization with pagination and filters
func (r *OrganizationRepository) ListMembers(orgID uuid.UUID, limit, offset int, emails []string, status string, query string) ([]models.OrganizationMember, int64, error) {
	var members []models.OrganizationMember
	var total int64

	queryBuilder := r.db.Model(&models.OrganizationMember{}).
		Where("organization_id = ?", orgID).
		Preload("User").
		Preload("Organization")

	// Filter by emails if provided
	if len(emails) > 0 {
		queryBuilder = queryBuilder.Joins("JOIN users ON organization_members.user_id = users.id").
			Where("users.email IN ?", emails)
	}

	// Filter by status (active/invited) - StackWeaver uses "role" field, not status
	// For now, all members are considered "active" since we don't have an invitation system
	// This can be extended later if needed

	// Search by query (user name or email)
	if query != "" {
		queryBuilder = queryBuilder.Joins("JOIN users ON organization_members.user_id = users.id").
			Where("users.name ILIKE ? OR users.email ILIKE ?", "%"+query+"%", "%"+query+"%")
	}

	// Get total count
	if err := queryBuilder.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	// Get paginated results
	err := queryBuilder.
		Order("organization_members.created_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&members).Error

	return members, total, err
}

// GetMembersByUserIDs gets organization memberships for multiple users in an organization
// Returns memberships ordered by ID for consistent ordering
func (r *OrganizationRepository) GetMembersByUserIDs(orgID uuid.UUID, userIDs []uuid.UUID) ([]models.OrganizationMember, error) {
	var members []models.OrganizationMember
	err := r.db.Where("organization_id = ? AND user_id IN ?", orgID, userIDs).
		Preload("User").
		Order("id ASC"). // Always order by ID for consistent results
		Find(&members).Error
	if err != nil {
		return nil, err
	}
	return members, nil
}

// GetMemberByID gets an organization membership by its ID
func (r *OrganizationRepository) GetMemberByID(memberID uuid.UUID) (*models.OrganizationMember, error) {
	var member models.OrganizationMember
	err := r.db.Preload("User").Preload("Organization").First(&member, "id = ?", memberID).Error
	if err != nil {
		return nil, err
	}
	return &member, nil
}

// DeleteMemberByID deletes an organization membership by its ID
func (r *OrganizationRepository) DeleteMemberByID(memberID uuid.UUID) error {
	return r.db.Delete(&models.OrganizationMember{}, "id = ?", memberID).Error
}
