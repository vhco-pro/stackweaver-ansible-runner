// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type TeamRepository struct {
	db *gorm.DB
}

func NewTeamRepository(db *gorm.DB) *TeamRepository {
	return &TeamRepository{db: db}
}

// Create creates a new team
func (r *TeamRepository) Create(team *models.Team) error {
	return r.db.Create(team).Error
}

// GetByID retrieves a team by ID
// CRITICAL: Order team members by user_id for consistent ordering
// This ensures organization memberships are returned in a consistent order
func (r *TeamRepository) GetByID(id uuid.UUID) (*models.Team, error) {
	var team models.Team
	err := r.db.Preload("Organization").
		Preload("Members", func(db *gorm.DB) *gorm.DB {
			// Order by user_id (UUID) for consistent ordering across reads
			// This prevents drift when organization memberships are queried
			return db.Order("team_members.user_id ASC")
		}).
		Preload("Members.User").
		Preload("ProjectAccess.Project").
		Preload("WorkspaceAccess.Workspace").
		Preload("OrganizationAccess").
		First(&team, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &team, nil
}

// GetByName retrieves a team by name within an organization
func (r *TeamRepository) GetByName(orgID uuid.UUID, name string) (*models.Team, error) {
	var team models.Team
	err := r.db.Preload("OrganizationAccess").Where("organization_id = ? AND name = ?", orgID, name).First(&team).Error
	if err != nil {
		return nil, err
	}
	return &team, nil
}

// List retrieves teams for an organization
func (r *TeamRepository) List(orgID uuid.UUID, limit, offset int) ([]models.Team, int64, error) {
	var teams []models.Team
	var total int64

	if err := r.db.Model(&models.Team{}).Where("organization_id = ?", orgID).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Preload("OrganizationAccess").
		Preload("Members", func(db *gorm.DB) *gorm.DB {
			// Order by user_id for consistent ordering
			return db.Order("team_members.user_id ASC")
		}).
		Where("organization_id = ?", orgID).
		Limit(limit).Offset(offset).
		Order("name ASC").
		Find(&teams).Error

	return teams, total, err
}

// Update updates a team
func (r *TeamRepository) Update(team *models.Team) error {
	return r.db.Save(team).Error
}

// Delete deletes a team (cascades to team_members, team_project_accesses, and team_workspace_accesses)
func (r *TeamRepository) Delete(id uuid.UUID) error {
	// Delete in transaction to ensure all related records are removed
	return r.db.Transaction(func(tx *gorm.DB) error {
		// Delete team organization access first (foreign key constraint)
		if err := tx.Where("team_id = ?", id).Delete(&models.TeamOrganizationAccess{}).Error; err != nil {
			return err
		}

		// Delete team project access
		if err := tx.Where("team_id = ?", id).Delete(&models.TeamProjectAccess{}).Error; err != nil {
			return err
		}

		// Delete team workspace access
		if err := tx.Where("team_id = ?", id).Delete(&models.TeamWorkspaceAccess{}).Error; err != nil {
			return err
		}

		// Delete team members
		if err := tx.Where("team_id = ?", id).Delete(&models.TeamMember{}).Error; err != nil {
			return err
		}

		// Finally delete the team itself
		return tx.Delete(&models.Team{}, "id = ?", id).Error
	})
}

// AddMember adds a user to a team
func (r *TeamRepository) AddMember(teamID, userID uuid.UUID) error {
	member := &models.TeamMember{
		TeamID: teamID,
		UserID: userID,
	}
	return r.db.Create(member).Error
}

// RemoveMember removes a user from a team
func (r *TeamRepository) RemoveMember(teamID, userID uuid.UUID) error {
	return r.db.Delete(&models.TeamMember{}, "team_id = ? AND user_id = ?", teamID, userID).Error
}

// GetMembers retrieves all members of a team
func (r *TeamRepository) GetMembers(teamID uuid.UUID) ([]models.User, error) {
	var users []models.User
	err := r.db.Table("users").
		Joins("JOIN team_members ON team_members.user_id = users.id").
		Where("team_members.team_id = ?", teamID).
		Find(&users).Error
	return users, err
}

// CreateProjectAccess creates a new team project access entry
func (r *TeamRepository) CreateProjectAccess(access *models.TeamProjectAccess) error {
	return r.db.Create(access).Error
}

// GetProjectAccessByID retrieves a team project access by ID
func (r *TeamRepository) GetProjectAccessByID(id uuid.UUID) (*models.TeamProjectAccess, error) {
	var access models.TeamProjectAccess
	err := r.db.Preload("Team").Preload("Project").First(&access, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &access, nil
}

// GetProjectAccessByTeamAndProject retrieves team project access by team and project
func (r *TeamRepository) GetProjectAccessByTeamAndProject(teamID, projectID uuid.UUID) (*models.TeamProjectAccess, error) {
	var access models.TeamProjectAccess
	err := r.db.Where("team_id = ? AND project_id = ?", teamID, projectID).Preload("Team").Preload("Project").First(&access).Error
	if err != nil {
		return nil, err
	}
	return &access, nil
}

// UpdateProjectAccess updates a team project access entry
func (r *TeamRepository) UpdateProjectAccess(access *models.TeamProjectAccess) error {
	return r.db.Save(access).Error
}

// DeleteProjectAccess deletes a team project access by ID
func (r *TeamRepository) DeleteProjectAccess(id uuid.UUID) error {
	return r.db.Delete(&models.TeamProjectAccess{}, "id = ?", id).Error
}

// AddProjectAccess adds team access to a project (legacy method, kept for backward compatibility)
func (r *TeamRepository) AddProjectAccess(teamID, projectID uuid.UUID, access string) error {
	accessEntry := &models.TeamProjectAccess{
		TeamID:    teamID,
		ProjectID: projectID,
		Access:    &access,
	}
	return r.db.Create(accessEntry).Error
}

// RemoveProjectAccess removes team access from a project (legacy method, kept for backward compatibility)
func (r *TeamRepository) RemoveProjectAccess(teamID, projectID uuid.UUID) error {
	return r.db.Delete(&models.TeamProjectAccess{}, "team_id = ? AND project_id = ?", teamID, projectID).Error
}

// GetProjectAccess retrieves team access for a project
func (r *TeamRepository) GetProjectAccess(projectID uuid.UUID) ([]models.TeamProjectAccess, error) {
	var access []models.TeamProjectAccess
	err := r.db.Where("project_id = ?", projectID).Preload("Team").Preload("Project").Find(&access).Error
	return access, err
}

// AddWorkspaceAccess adds team access to a workspace (legacy method, kept for backward compatibility)
func (r *TeamRepository) AddWorkspaceAccess(teamID uuid.UUID, workspaceID string, access string) error {
	accessStr := access
	accessEntry := &models.TeamWorkspaceAccess{
		TeamID:      teamID,
		WorkspaceID: workspaceID,
		Access:      &accessStr,
	}
	return r.db.Create(accessEntry).Error
}

// RemoveWorkspaceAccess removes team access from a workspace (legacy method, kept for backward compatibility)
func (r *TeamRepository) RemoveWorkspaceAccess(teamID uuid.UUID, workspaceID string) error {
	return r.db.Delete(&models.TeamWorkspaceAccess{}, "team_id = ? AND workspace_id = ?", teamID, workspaceID).Error
}

// GetWorkspaceAccess retrieves team access for a workspace
func (r *TeamRepository) GetWorkspaceAccess(workspaceID string) ([]models.TeamWorkspaceAccess, error) {
	var access []models.TeamWorkspaceAccess
	err := r.db.Where("workspace_id = ?", workspaceID).Preload("Team").Find(&access).Error
	return access, err
}

// CreateWorkspaceAccess creates a new team workspace access entry
func (r *TeamRepository) CreateWorkspaceAccess(access *models.TeamWorkspaceAccess) error {
	return r.db.Create(access).Error
}

// UpdateWorkspaceAccess updates a team workspace access entry
func (r *TeamRepository) UpdateWorkspaceAccess(access *models.TeamWorkspaceAccess) error {
	return r.db.Save(access).Error
}

// GetWorkspaceAccessByID retrieves a team workspace access by ID
func (r *TeamRepository) GetWorkspaceAccessByID(accessID uuid.UUID) (*models.TeamWorkspaceAccess, error) {
	var access models.TeamWorkspaceAccess
	err := r.db.Preload("Team").Preload("Workspace").First(&access, "id = ?", accessID).Error
	if err != nil {
		return nil, err
	}
	return &access, nil
}

// GetWorkspaceAccessByTeamAndWorkspace retrieves team workspace access by team and workspace IDs
func (r *TeamRepository) GetWorkspaceAccessByTeamAndWorkspace(teamID uuid.UUID, workspaceID string) (*models.TeamWorkspaceAccess, error) {
	var access models.TeamWorkspaceAccess
	err := r.db.Where("team_id = ? AND workspace_id = ?", teamID, workspaceID).Preload("Team").Preload("Workspace").First(&access).Error
	if err != nil {
		return nil, err
	}
	return &access, nil
}

// DeleteWorkspaceAccess deletes a team workspace access by ID
func (r *TeamRepository) DeleteWorkspaceAccess(accessID uuid.UUID) error {
	return r.db.Delete(&models.TeamWorkspaceAccess{}, "id = ?", accessID).Error
}

// CreateOrganizationAccess creates organization access for a team
func (r *TeamRepository) CreateOrganizationAccess(access *models.TeamOrganizationAccess) error {
	return r.db.Create(access).Error
}

// GetOrCreateOrganizationAccess gets or creates organization access for a team
func (r *TeamRepository) GetOrCreateOrganizationAccess(teamID uuid.UUID) (*models.TeamOrganizationAccess, error) {
	var access models.TeamOrganizationAccess
	err := r.db.Where("team_id = ?", teamID).First(&access).Error
	if err == gorm.ErrRecordNotFound {
		// Create default organization access (all permissions false)
		access = models.TeamOrganizationAccess{
			TeamID: teamID,
		}
		if err := r.db.Create(&access).Error; err != nil {
			return nil, err
		}
		return &access, nil
	}
	if err != nil {
		return nil, err
	}
	return &access, nil
}

// UpdateOrganizationAccess updates organization access for a team
func (r *TeamRepository) UpdateOrganizationAccess(access *models.TeamOrganizationAccess) error {
	return r.db.Save(access).Error
}

// GetOrganizationAccess retrieves organization access for a team
func (r *TeamRepository) GetOrganizationAccess(teamID uuid.UUID) (*models.TeamOrganizationAccess, error) {
	var access models.TeamOrganizationAccess
	err := r.db.Where("team_id = ?", teamID).First(&access).Error
	if err != nil {
		return nil, err
	}
	return &access, nil
}

// GetTeamsByUserID retrieves all teams a user is a member of in a specific organization
func (r *TeamRepository) GetTeamsByUserID(userID, orgID uuid.UUID) ([]models.Team, error) {
	var teams []models.Team
	// Use Model() instead of Table() to ensure Preload works correctly
	err := r.db.Model(&models.Team{}).
		Joins("JOIN team_members ON teams.id = team_members.team_id").
		Where("team_members.user_id = ? AND teams.organization_id = ?", userID, orgID).
		Preload("OrganizationAccess").
		Find(&teams).Error
	return teams, err
}

// FindBySSOTeamIDs returns all teams where sso_team_id matches any of the given IDs
// within a specific organization. Used for automatic team assignment based on SSO group claims.
func (r *TeamRepository) FindBySSOTeamIDs(orgID uuid.UUID, ssoTeamIDs []string) ([]models.Team, error) {
	var teams []models.Team
	err := r.db.Where("organization_id = ? AND sso_team_id IN ?", orgID, ssoTeamIDs).
		Find(&teams).Error
	return teams, err
}

// FindAllBySSOTeamIDs returns all teams (across all organizations) where sso_team_id
// matches any of the given IDs. Used when syncing a user's teams across all organizations.
func (r *TeamRepository) FindAllBySSOTeamIDs(ssoTeamIDs []string) ([]models.Team, error) {
	var teams []models.Team
	err := r.db.Where("sso_team_id IN ?", ssoTeamIDs).
		Find(&teams).Error
	return teams, err
}

// GetSSOTeamsByUserID retrieves all SSO-managed teams (teams with non-null sso_team_id)
// that a user is a member of, across all organizations.
func (r *TeamRepository) GetSSOTeamsByUserID(userID uuid.UUID) ([]models.Team, error) {
	var teams []models.Team
	err := r.db.Model(&models.Team{}).
		Joins("JOIN team_members ON teams.id = team_members.team_id").
		Where("team_members.user_id = ? AND teams.sso_team_id IS NOT NULL AND teams.sso_team_id != ''", userID).
		Find(&teams).Error
	return teams, err
}

// IsMember checks if a user is already a member of a team
func (r *TeamRepository) IsMember(teamID, userID uuid.UUID) (bool, error) {
	var count int64
	err := r.db.Model(&models.TeamMember{}).
		Where("team_id = ? AND user_id = ?", teamID, userID).
		Count(&count).Error
	return count > 0, err
}
