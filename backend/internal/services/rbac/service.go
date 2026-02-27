// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package rbac

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
)

type Service struct {
	orgRepo     *repository.OrganizationRepository
	teamRepo    *repository.TeamRepository
	projectRepo *repository.ProjectRepository
}

func NewService(orgRepo *repository.OrganizationRepository) *Service {
	return &Service{
		orgRepo: orgRepo,
	}
}

// NewServiceWithTeams creates a new RBAC service with team support
func NewServiceWithTeams(orgRepo *repository.OrganizationRepository, teamRepo *repository.TeamRepository, projectRepo *repository.ProjectRepository) *Service {
	return &Service{
		orgRepo:     orgRepo,
		teamRepo:    teamRepo,
		projectRepo: projectRepo,
	}
}

// ResourceType represents the type of resource being checked
type ResourceType string

const (
	ResourceTypeTerraformWorkspace ResourceType = "terraform:workspace"
	ResourceTypeAnsiblePlaybook    ResourceType = "ansible:playbook"
	ResourceTypeAnsibleInventory   ResourceType = "ansible:inventory"
	ResourceTypeAnsibleCredential  ResourceType = "ansible:credential" //nolint:gosec // false positive: string constant, not actual credential
	ResourceTypeAnsibleJobTemplate ResourceType = "ansible:job-template"
	ResourceTypeAnsibleJob         ResourceType = "ansible:job"
	ResourceTypeAnsibleSchedule    ResourceType = "ansible:schedule"
)

type Permission string

const (
	// Organization permissions (legacy - kept for backward compatibility)
	PermissionOrgRead  Permission = "org:read"
	PermissionOrgWrite Permission = "org:write"
	PermissionOrgAdmin Permission = "org:admin"

	// Fine-grained organization permissions (TFE-compatible)
	PermissionOrgManageMembership         Permission = "org:manage-membership"          // Manage organization memberships (add/remove users, change roles)
	PermissionOrgManageTeams              Permission = "org:manage-teams"               // Create/update/delete teams
	PermissionOrgManageOrganizationAccess Permission = "org:manage-organization-access" // Manage team organization access permissions
	PermissionOrgManageProjects           Permission = "org:manage-projects"            // Create/update/delete projects
	PermissionOrgManageWorkspaces         Permission = "org:manage-workspaces"          // Create/update/delete workspaces
	PermissionOrgReadWorkspaces           Permission = "org:read-workspaces"            // Read workspaces
	PermissionOrgReadProjects             Permission = "org:read-projects"              // Read projects
	PermissionOrgManageVCSSettings        Permission = "org:manage-vcs-settings"        // Manage VCS connections
	PermissionOrgManageProviders          Permission = "org:manage-providers"           // Manage provider registrations
	PermissionOrgManageModules            Permission = "org:manage-modules"             // Manage module registrations
	PermissionOrgManagePolicies           Permission = "org:manage-policies"            // Manage Sentinel policies
	PermissionOrgManagePolicyOverrides    Permission = "org:manage-policy-overrides"    // Manage policy overrides
	PermissionOrgManageRunTasks           Permission = "org:manage-run-tasks"           // Manage run tasks
	PermissionOrgAccessSecretTeams        Permission = "org:access-secret-teams"        //nolint:gosec // false positive: string constant, not actual credential
	PermissionOrgManageAgentPools         Permission = "org:manage-agent-pools"         // Manage agent pools

	// Project permissions
	PermissionProjectRead  Permission = "project:read"
	PermissionProjectWrite Permission = "project:write"

	// Terraform workspace permissions
	PermissionWorkspaceRead  Permission = "workspace:read"
	PermissionWorkspaceWrite Permission = "workspace:write"
	PermissionRunRead        Permission = "run:read"
	PermissionRunWrite       Permission = "run:write"

	// Terraform granular permissions (from team access)
	PermissionStateVersions    Permission = "state_versions"
	PermissionVariables        Permission = "variables"
	PermissionRuns             Permission = "runs"
	PermissionSentinelMocks    Permission = "sentinel_mocks"
	PermissionWorkspaceLocking Permission = "workspace_locking"
	PermissionRunTasks         Permission = "run_tasks"

	// Ansible permissions (StackWeaver-specific)
	PermissionAnsiblePlaybookRead     Permission = "ansible:playbook:read"
	PermissionAnsiblePlaybookWrite    Permission = "ansible:playbook:write"
	PermissionAnsibleInventoryRead    Permission = "ansible:inventory:read"
	PermissionAnsibleInventoryWrite   Permission = "ansible:inventory:write"
	PermissionAnsibleCredentialRead   Permission = "ansible:credential:read"  //nolint:gosec // false positive: string constant, not actual credential
	PermissionAnsibleCredentialWrite  Permission = "ansible:credential:write" //nolint:gosec // false positive: string constant, not actual credential
	PermissionAnsibleJobTemplateRead  Permission = "ansible:job-template:read"
	PermissionAnsibleJobTemplateWrite Permission = "ansible:job-template:write"
	PermissionAnsibleJobRead          Permission = "ansible:job:read"
	PermissionAnsibleJobExecute       Permission = "ansible:job:execute"
	PermissionAnsibleScheduleRead     Permission = "ansible:schedule:read"
	PermissionAnsibleScheduleWrite    Permission = "ansible:schedule:write"
)

type Role string

const (
	RoleAdmin  Role = "admin"
	RoleMember Role = "member"
	RoleViewer Role = "viewer"
)

// rolePermissions is DEPRECATED: Roles are deprecated in favor of team-based permissions.
// This will be removed in Phase 2 of the team-based permissions refactor.
//
//nolint:unused // Kept for reference during migration, will be removed
var rolePermissions = map[Role][]Permission{
	RoleAdmin: {
		// Legacy organization permissions (backward compatibility)
		PermissionOrgRead, PermissionOrgWrite, PermissionOrgAdmin,
		// Fine-grained organization permissions (all admin permissions)
		PermissionOrgManageMembership,
		PermissionOrgManageTeams,
		PermissionOrgManageOrganizationAccess,
		PermissionOrgManageProjects,
		PermissionOrgManageWorkspaces,
		PermissionOrgReadWorkspaces,
		PermissionOrgReadProjects,
		PermissionOrgManageVCSSettings,
		PermissionOrgManageProviders,
		PermissionOrgManageModules,
		PermissionOrgManagePolicies,
		PermissionOrgManagePolicyOverrides,
		PermissionOrgManageRunTasks,
		PermissionOrgAccessSecretTeams,
		PermissionOrgManageAgentPools,
		// Project permissions
		PermissionProjectRead, PermissionProjectWrite,
		// Terraform permissions
		PermissionWorkspaceRead, PermissionWorkspaceWrite,
		PermissionRunRead, PermissionRunWrite,
		// Terraform granular permissions
		PermissionStateVersions, PermissionVariables, PermissionRuns,
		PermissionSentinelMocks, PermissionWorkspaceLocking, PermissionRunTasks,
		// Ansible permissions
		PermissionAnsiblePlaybookRead, PermissionAnsiblePlaybookWrite,
		PermissionAnsibleInventoryRead, PermissionAnsibleInventoryWrite,
		PermissionAnsibleCredentialRead, PermissionAnsibleCredentialWrite,
		PermissionAnsibleJobTemplateRead, PermissionAnsibleJobTemplateWrite,
		PermissionAnsibleJobRead, PermissionAnsibleJobExecute,
		PermissionAnsibleScheduleRead, PermissionAnsibleScheduleWrite,
	},
	RoleMember: {
		// Legacy organization permissions (read access)
		PermissionOrgRead,
		// Fine-grained organization permissions (day-to-day operator tasks, but NOT admin tasks)
		PermissionOrgReadWorkspaces, // Can read workspaces
		PermissionOrgReadProjects,   // Can read projects
		// Note: Members can manage workspaces/projects through project/workspace permissions below
		// but NOT through organization-level management (that's admin-only)
		// Project permissions (day-to-day tasks)
		PermissionProjectRead, PermissionProjectWrite,
		// Terraform permissions (day-to-day tasks)
		PermissionWorkspaceRead, PermissionWorkspaceWrite,
		PermissionRunRead, PermissionRunWrite,
		// Terraform granular permissions (day-to-day tasks)
		PermissionStateVersions, PermissionVariables, PermissionRuns,
		PermissionSentinelMocks, PermissionWorkspaceLocking, PermissionRunTasks,
		// Ansible permissions (day-to-day tasks)
		PermissionAnsiblePlaybookRead, PermissionAnsiblePlaybookWrite,
		PermissionAnsibleInventoryRead, PermissionAnsibleInventoryWrite,
		PermissionAnsibleCredentialRead, PermissionAnsibleCredentialWrite,
		PermissionAnsibleJobTemplateRead, PermissionAnsibleJobTemplateWrite,
		PermissionAnsibleJobRead, PermissionAnsibleJobExecute,
		PermissionAnsibleScheduleRead, PermissionAnsibleScheduleWrite,
	},
	RoleViewer: {
		// Legacy organization permissions (read-only)
		PermissionOrgRead,
		// Fine-grained organization permissions (read-only)
		PermissionOrgReadWorkspaces, // Can read workspaces
		PermissionOrgReadProjects,   // Can read projects
		// Project permissions (read-only)
		PermissionProjectRead,
		// Terraform permissions (read-only)
		PermissionWorkspaceRead,
		PermissionRunRead,
		// Note: Viewer does NOT have granular permissions (state_versions, variables, runs)
		// These require explicit read permission checks at the resource level
		// Ansible permissions (read-only)
		PermissionAnsiblePlaybookRead,
		PermissionAnsibleInventoryRead,
		PermissionAnsibleCredentialRead,
		PermissionAnsibleJobTemplateRead,
		PermissionAnsibleJobRead,
		PermissionAnsibleScheduleRead,
	},
}

// CheckPermission is DEPRECATED: Use team-based permission checks instead.
// This method is kept for backward compatibility but always returns false
// as roles are deprecated in favor of team-based permissions.
func (s *Service) CheckPermission(ctx context.Context, userID, organizationID uuid.UUID, permission Permission) (bool, error) {
	// Roles are deprecated - all permissions now come from team memberships
	// This method always returns false to force migration to team-based checks
	return false, nil
}

func (s *Service) RequirePermission(ctx context.Context, userID, organizationID uuid.UUID, permission Permission) error {
	hasPermission, err := s.CheckPermission(ctx, userID, organizationID, permission)
	if err != nil {
		return err
	}

	if !hasPermission {
		return errors.New("insufficient permissions")
	}

	return nil
}

// GetUserRole is DEPRECATED: Roles are deprecated in favor of team-based permissions.
// This method is kept for backward compatibility but returns an error.
func (s *Service) GetUserRole(ctx context.Context, userID, organizationID uuid.UUID) (Role, error) {
	return "", fmt.Errorf("roles are deprecated - use team memberships instead")
}

// CheckResourcePermission checks if a user has permission for a specific resource
// NEW MODEL: Pure team-based additive permission resolution
// Resolution: Org membership check → Collect ALL permissions from ALL teams → Union (additive)
func (s *Service) CheckResourcePermission(
	ctx context.Context,
	userID uuid.UUID,
	resourceType ResourceType,
	resourceID string, // Can be workspace ID (string) or UUID for other resources
	permission Permission,
	projectID *uuid.UUID, // Optional: project ID if resource belongs to a project
) (bool, error) {
	// Get organization ID from project
	var organizationID uuid.UUID
	if projectID == nil {
		// For resources without projects, we need to get org from resource
		// This will be implemented per resource type as needed
		return false, fmt.Errorf("project ID required for resource permission check")
	}

	project, err := s.projectRepo.GetByID(*projectID)
	if err != nil {
		return false, fmt.Errorf("failed to get project: %w", err)
	}
	organizationID = project.OrganizationID

	// 1. Tenant isolation: User must have at least one team in the org (team-based access)
	inOrg, err := s.orgRepo.UserInOrg(userID, organizationID)
	if err != nil || !inOrg {
		return false, nil // Not in any team in org = no access
	}

	// 2. Collect ALL permissions from ALL team memberships (additive model)
	allPermissions := make(map[Permission]bool)

	if s.teamRepo == nil {
		return false, fmt.Errorf("team repository not available")
	}

	// Get all teams user is member of in this organization
	teams, err := s.teamRepo.GetTeamsByUserID(userID, organizationID)
	if err != nil {
		return false, fmt.Errorf("failed to get user teams: %w", err)
	}

	// Collect permissions from each team
	for _, team := range teams {
		// Team organization access permissions (org-level permissions)
		// GetTeamsByUserID already preloads OrganizationAccess, so use it directly
		if team.OrganizationAccess != nil {
			teamOrgPerms := s.getPermissionsFromOrganizationAccess(team.OrganizationAccess)
			for perm := range teamOrgPerms {
				allPermissions[perm] = true
			}
		}

		// Team project access permissions (if projectID provided)
		if projectID != nil {
			projectAccess, err := s.teamRepo.GetProjectAccessByTeamAndProject(team.ID, *projectID)
			if err == nil && projectAccess != nil {
				teamProjectPerms := s.getPermissionsFromProjectAccess(projectAccess, resourceType)
				for perm := range teamProjectPerms {
					allPermissions[perm] = true
				}
			}
		}

		// Team workspace/resource-specific access (overrides project access for this specific resource)
		if resourceID != "" && resourceType == ResourceTypeTerraformWorkspace {
			workspaceAccess, err := s.teamRepo.GetWorkspaceAccessByTeamAndWorkspace(team.ID, resourceID)
			if err == nil && workspaceAccess != nil {
				teamWorkspacePerms := s.getPermissionsFromWorkspaceAccess(workspaceAccess)
				for perm := range teamWorkspacePerms {
					allPermissions[perm] = true
				}
			}
		}
	}

	// 3. Check if permission is in union
	return allPermissions[permission], nil
}

// getPermissionsFromOrganizationAccess extracts all permissions from team organization access
func (s *Service) getPermissionsFromOrganizationAccess(orgAccess *models.TeamOrganizationAccess) map[Permission]bool {
	perms := make(map[Permission]bool)

	// Map organization access fields to permissions
	if orgAccess.ManagePolicies {
		perms[PermissionOrgManagePolicies] = true
	}
	if orgAccess.ManagePolicyOverrides {
		perms[PermissionOrgManagePolicyOverrides] = true
	}
	if orgAccess.ManageWorkspaces {
		perms[PermissionOrgManageWorkspaces] = true
		// ManageWorkspaces implies all workspace-level permissions (TFE-compatible: "Manage all workspaces" grants full access)
		// This matches the behavior described in TFE docs: "Manage all workspaces" is the most permissive level
		perms[PermissionOrgReadWorkspaces] = true
		perms[PermissionWorkspaceRead] = true
		perms[PermissionWorkspaceWrite] = true
		perms[PermissionRunRead] = true
		perms[PermissionRunWrite] = true
		perms[PermissionVariables] = true
		perms[PermissionStateVersions] = true
		perms[PermissionRuns] = true
		perms[PermissionSentinelMocks] = true
		perms[PermissionWorkspaceLocking] = true
		perms[PermissionRunTasks] = true
	}
	if orgAccess.ManageVCSSettings {
		perms[PermissionOrgManageVCSSettings] = true
	}
	if orgAccess.ManageProviders {
		perms[PermissionOrgManageProviders] = true
	}
	if orgAccess.ManageModules {
		perms[PermissionOrgManageModules] = true
	}
	if orgAccess.ManageRunTasks {
		perms[PermissionOrgManageRunTasks] = true
	}
	if orgAccess.ManageProjects {
		perms[PermissionOrgManageProjects] = true
		// ManageProjects implies ReadProjects (if you can manage projects, you can read them)
		perms[PermissionOrgReadProjects] = true
		perms[PermissionProjectRead] = true
	}
	if orgAccess.ReadWorkspaces {
		perms[PermissionOrgReadWorkspaces] = true
		perms[PermissionWorkspaceRead] = true // Implies workspace read
	}
	if orgAccess.ReadProjects {
		perms[PermissionOrgReadProjects] = true
		perms[PermissionProjectRead] = true // Implies project read
	}
	if orgAccess.ManageMembership {
		perms[PermissionOrgManageMembership] = true
	}
	if orgAccess.ManageTeams {
		perms[PermissionOrgManageTeams] = true
	}
	if orgAccess.ManageOrganizationAccess {
		perms[PermissionOrgManageOrganizationAccess] = true
	}
	if orgAccess.AccessSecretTeams {
		perms[PermissionOrgAccessSecretTeams] = true
	}
	if orgAccess.ManageAgentPools {
		perms[PermissionOrgManageAgentPools] = true
	}

	return perms
}

// getPermissionsFromProjectAccess extracts all permissions from team project access
func (s *Service) getPermissionsFromProjectAccess(projectAccess *models.TeamProjectAccess, resourceType ResourceType) map[Permission]bool {
	perms := make(map[Permission]bool)

	if projectAccess.Access != nil {
		accessLevel := *projectAccess.Access
		switch accessLevel {
		case "admin":
			// Admin has all permissions
			perms[PermissionProjectRead] = true
			perms[PermissionProjectWrite] = true
			perms[PermissionWorkspaceRead] = true
			perms[PermissionWorkspaceWrite] = true
			perms[PermissionRunRead] = true
			perms[PermissionRunWrite] = true
			perms[PermissionVariables] = true
			perms[PermissionStateVersions] = true
			perms[PermissionRuns] = true
			perms[PermissionSentinelMocks] = true
			perms[PermissionWorkspaceLocking] = true
			perms[PermissionRunTasks] = true
		case "maintain", "write":
			// Write has write permissions
			perms[PermissionProjectRead] = true
			perms[PermissionProjectWrite] = true
			perms[PermissionWorkspaceRead] = true
			perms[PermissionWorkspaceWrite] = true
			perms[PermissionRunRead] = true
			perms[PermissionRunWrite] = true
			perms[PermissionVariables] = true
			perms[PermissionStateVersions] = true
			perms[PermissionRuns] = true
			perms[PermissionWorkspaceLocking] = true
			perms[PermissionRunTasks] = true
		case "read":
			// Read has read-only permissions (PermissionRuns NOT included - that's for creating/planning)
			perms[PermissionProjectRead] = true
			perms[PermissionWorkspaceRead] = true
			perms[PermissionRunRead] = true
			perms[PermissionStateVersions] = true // Granular permission (level checked separately)
			perms[PermissionVariables] = true     // Granular permission (level checked separately)
			perms[PermissionSentinelMocks] = true
		}
	}

	// Add custom permissions if access is "custom" or custom fields are set
	if projectAccess.Access != nil && *projectAccess.Access == "custom" {
		if projectAccess.WorkspaceRuns != nil {
			level := *projectAccess.WorkspaceRuns
			// WorkspaceRuns granular permission levels:
			// "read" = can view runs (PermissionRunRead)
			// "plan" = can plan runs (PermissionRuns)
			// "apply" = can apply runs (PermissionRuns + PermissionWorkspaceWrite)
			switch level {
			case "read":
				perms[PermissionRunRead] = true
			case "plan", "apply":
				perms[PermissionRuns] = true
			}
		}
		if projectAccess.WorkspaceVariables != nil {
			level := *projectAccess.WorkspaceVariables
			if level == "read" || level == "write" {
				perms[PermissionVariables] = true
			}
		}
		if projectAccess.WorkspaceStateVersions != nil {
			level := *projectAccess.WorkspaceStateVersions
			if level == "read" || level == "read-outputs" || level == "write" {
				perms[PermissionStateVersions] = true
			}
		}
		if projectAccess.WorkspaceSentinelMocks != nil {
			level := *projectAccess.WorkspaceSentinelMocks
			if level == "read" {
				perms[PermissionSentinelMocks] = true
			}
		}
		if projectAccess.WorkspaceLocking != nil && *projectAccess.WorkspaceLocking {
			perms[PermissionWorkspaceLocking] = true
		}
		if projectAccess.WorkspaceRunTasks != nil && *projectAccess.WorkspaceRunTasks {
			perms[PermissionRunTasks] = true
		}
	}

	return perms
}

// getPermissionsFromWorkspaceAccess extracts all permissions from team workspace access
func (s *Service) getPermissionsFromWorkspaceAccess(workspaceAccess *models.TeamWorkspaceAccess) map[Permission]bool {
	perms := make(map[Permission]bool)

	if workspaceAccess.Access != nil {
		accessLevel := *workspaceAccess.Access
		switch accessLevel {
		case "admin":
			// Admin has all permissions
			perms[PermissionWorkspaceRead] = true
			perms[PermissionWorkspaceWrite] = true
			perms[PermissionRunRead] = true
			perms[PermissionRunWrite] = true
			perms[PermissionVariables] = true
			perms[PermissionStateVersions] = true
			perms[PermissionRuns] = true
			perms[PermissionSentinelMocks] = true
			perms[PermissionWorkspaceLocking] = true
			perms[PermissionRunTasks] = true
		case "write":
			// Write has write permissions
			perms[PermissionWorkspaceRead] = true
			perms[PermissionWorkspaceWrite] = true
			perms[PermissionRunRead] = true
			perms[PermissionRunWrite] = true
			perms[PermissionVariables] = true
			perms[PermissionStateVersions] = true
			perms[PermissionRuns] = true
			perms[PermissionWorkspaceLocking] = true
			perms[PermissionRunTasks] = true
		case "plan":
			// Plan has read and plan permissions
			perms[PermissionWorkspaceRead] = true
			perms[PermissionRunRead] = true
			perms[PermissionStateVersions] = true
			perms[PermissionVariables] = true
			perms[PermissionRuns] = true // plan level allows planning
		case "read":
			// Read has read-only permissions (PermissionRuns NOT included)
			perms[PermissionWorkspaceRead] = true
			perms[PermissionRunRead] = true
			perms[PermissionStateVersions] = true // Granular permission (level checked separately)
			perms[PermissionVariables] = true     // Granular permission (level checked separately)
			perms[PermissionSentinelMocks] = true
		}
	}

	// Add custom permissions if custom fields are set
	if workspaceAccess.Runs != nil {
		level := *workspaceAccess.Runs
		// Runs granular permission levels:
		// "read" = can view runs (PermissionRunRead)
		// "plan" = can plan runs (PermissionRuns)
		// "apply" = can apply runs (PermissionRuns + PermissionWorkspaceWrite)
		switch level {
		case "read":
			perms[PermissionRunRead] = true
		case "plan", "apply":
			perms[PermissionRuns] = true
		}
	}
	if workspaceAccess.Variables != nil {
		level := *workspaceAccess.Variables
		if level == "read" || level == "write" {
			perms[PermissionVariables] = true
		}
	}
	if workspaceAccess.StateVersions != nil {
		level := *workspaceAccess.StateVersions
		if level == "read" || level == "read-outputs" || level == "write" {
			perms[PermissionStateVersions] = true
		}
	}
	if workspaceAccess.SentinelMocks != nil {
		level := *workspaceAccess.SentinelMocks
		if level == "read" {
			perms[PermissionSentinelMocks] = true
		}
	}
	if workspaceAccess.WorkspaceLocking != nil && *workspaceAccess.WorkspaceLocking {
		perms[PermissionWorkspaceLocking] = true
	}
	if workspaceAccess.RunTasks != nil && *workspaceAccess.RunTasks {
		perms[PermissionRunTasks] = true
	}

	return perms
}

// checkTeamProjectPermission is DEPRECATED: Replaced by additive team-based permission model in CheckResourcePermission
//
//nolint:unused // Kept for reference, will be removed in cleanup
func (s *Service) checkTeamProjectPermission(
	ctx context.Context,
	userID uuid.UUID,
	projectID uuid.UUID,
	resourceType ResourceType,
	permission Permission,
) (bool, error) {
	// Get all teams the user is a member of in this organization
	project, err := s.projectRepo.GetByID(projectID)
	if err != nil {
		return false, err
	}

	// Get all teams in the organization
	teams, _, err := s.teamRepo.List(project.OrganizationID, 1000, 0) // Large limit to get all teams
	if err != nil {
		return false, err
	}

	// Check if user is in any team with project access
	for _, team := range teams {
		// Check if user is a team member
		members, err := s.teamRepo.GetMembers(team.ID)
		if err != nil {
			continue
		}
		isMember := false
		for _, member := range members {
			if member.ID == userID {
				isMember = true
				break
			}
		}
		if !isMember {
			continue
		}

		// Get team project access
		projectAccess, err := s.teamRepo.GetProjectAccessByTeamAndProject(team.ID, projectID)
		if err != nil {
			continue // No project access for this team
		}

		// Check if project access grants the permission
		if s.projectAccessGrantsPermission(projectAccess, resourceType, permission) {
			return true, nil
		}
	}

	return false, nil
}

// checkTeamResourcePermission is DEPRECATED: Replaced by additive team-based permission model in CheckResourcePermission
//
//nolint:unused // Kept for reference, will be removed in cleanup
func (s *Service) checkTeamResourcePermission(
	ctx context.Context,
	userID uuid.UUID,
	resourceType ResourceType,
	resourceID string,
	permission Permission,
) (bool, error) {
	// Currently only implemented for Terraform workspaces
	// Ansible resources don't have resource-specific access yet (they use project access)
	if resourceType != ResourceTypeTerraformWorkspace {
		return false, nil
	}

	// Get workspace to find organization
	// Note: We need workspace repo, but for now we'll get it from team workspace access
	workspaceAccesses, err := s.teamRepo.GetWorkspaceAccess(resourceID)
	if err != nil {
		return false, nil
	}

	// Check if user is in any team with workspace access
	for _, access := range workspaceAccesses {
		// Check if user is a team member
		members, err := s.teamRepo.GetMembers(access.TeamID)
		if err != nil {
			continue
		}
		isMember := false
		for _, member := range members {
			if member.ID == userID {
				isMember = true
				break
			}
		}
		if !isMember {
			continue
		}

		// Check if workspace access grants the permission
		if s.workspaceAccessGrantsPermission(&access, permission) {
			return true, nil
		}
	}

	return false, nil
}

// projectAccessGrantsPermission is DEPRECATED: Replaced by getPermissionsFromProjectAccess (extracts all permissions)
//
//nolint:unused // Kept for reference, will be removed in cleanup
func (s *Service) projectAccessGrantsPermission(
	access *models.TeamProjectAccess,
	resourceType ResourceType,
	permission Permission,
) bool {
	// Map fixed access levels to permissions
	if access.Access != nil {
		accessLevel := *access.Access
		switch accessLevel {
		case "admin":
			return true // Admin has all permissions
		case "maintain":
			// Maintain has write permissions
			return permission == PermissionProjectWrite ||
				permission == PermissionWorkspaceWrite ||
				permission == PermissionRunWrite ||
				permission == PermissionVariables ||
				permission == PermissionStateVersions ||
				permission == PermissionRuns ||
				permission == PermissionWorkspaceLocking ||
				permission == PermissionRunTasks
		case "write":
			// Write has write permissions but not admin
			return permission == PermissionProjectWrite ||
				permission == PermissionWorkspaceWrite ||
				permission == PermissionRunWrite ||
				permission == PermissionVariables ||
				permission == PermissionStateVersions ||
				permission == PermissionRuns ||
				permission == PermissionWorkspaceLocking ||
				permission == PermissionRunTasks
		case "read":
			// Read has read-only permissions
			// CRITICAL: PermissionRuns is NOT included - it allows creating/planning runs, which read access should NOT allow
			// PermissionStateVersions and PermissionVariables are included for read access, but actual read/write level is checked separately
			return permission == PermissionProjectRead ||
				permission == PermissionWorkspaceRead ||
				permission == PermissionRunRead ||
				permission == PermissionStateVersions || // Granular permission (level checked separately)
				permission == PermissionVariables || // Granular permission (level checked separately)
				permission == PermissionSentinelMocks
		}
	}

	// Check custom permissions if access is "custom" or custom fields are set
	if access.Access != nil && *access.Access == "custom" {
		// Check custom workspace permissions (these apply to all workspaces in project)
		return s.checkCustomWorkspacePermissions(access, permission)
	}

	return false
}

// workspaceAccessGrantsPermission is DEPRECATED: Replaced by getPermissionsFromWorkspaceAccess (extracts all permissions)
//
//nolint:unused // Kept for reference, will be removed in cleanup
func (s *Service) workspaceAccessGrantsPermission(
	access *models.TeamWorkspaceAccess,
	permission Permission,
) bool {
	// Map fixed access levels to permissions
	if access.Access != nil {
		accessLevel := *access.Access
		switch accessLevel {
		case "admin":
			return true // Admin has all permissions
		case "write":
			// Write has write permissions
			return permission == PermissionWorkspaceWrite ||
				permission == PermissionRunWrite ||
				permission == PermissionVariables ||
				permission == PermissionStateVersions ||
				permission == PermissionRuns ||
				permission == PermissionWorkspaceLocking ||
				permission == PermissionRunTasks
		case "plan":
			// Plan has read and plan permissions
			return permission == PermissionWorkspaceRead ||
				permission == PermissionRunRead ||
				permission == PermissionStateVersions ||
				permission == PermissionVariables ||
				permission == PermissionRuns // plan level
		case "read":
			// Read has read-only permissions
			// CRITICAL: PermissionRuns is NOT included - it allows creating/planning runs, which read access should NOT allow
			// PermissionStateVersions and PermissionVariables are included for read access, but actual read/write level is checked separately
			return permission == PermissionWorkspaceRead ||
				permission == PermissionRunRead ||
				permission == PermissionStateVersions || // Granular permission (level checked separately via CheckStateVersionPermission)
				permission == PermissionVariables || // Granular permission (level checked separately via CheckVariablePermission)
				permission == PermissionSentinelMocks
		}
	}

	// Check custom permissions if custom fields are set
	return s.checkCustomWorkspacePermissionsFromAccess(access, permission)
}

// checkCustomWorkspacePermissions is DEPRECATED: Logic moved to getPermissionsFromProjectAccess
//
//nolint:unused // Kept for reference, will be removed in cleanup
func (s *Service) checkCustomWorkspacePermissions(
	access *models.TeamProjectAccess,
	permission Permission,
) bool {
	// Check custom workspace permissions from project access
	//nolint:exhaustive // Only handling workspace-level permissions, not all permissions
	switch permission {
	case PermissionRuns:
		if access.WorkspaceRuns != nil {
			level := *access.WorkspaceRuns
			return level == "read" || level == "plan" || level == "apply"
		}
	case PermissionVariables:
		if access.WorkspaceVariables != nil {
			level := *access.WorkspaceVariables
			return level == "read" || level == "write"
		}
	case PermissionStateVersions:
		if access.WorkspaceStateVersions != nil {
			level := *access.WorkspaceStateVersions
			return level == "read" || level == "read-outputs" || level == "write"
		}
	case PermissionSentinelMocks:
		if access.WorkspaceSentinelMocks != nil {
			level := *access.WorkspaceSentinelMocks
			return level == "read"
		}
	case PermissionWorkspaceLocking:
		if access.WorkspaceLocking != nil {
			return *access.WorkspaceLocking
		}
	case PermissionRunTasks:
		if access.WorkspaceRunTasks != nil {
			return *access.WorkspaceRunTasks
		}
	default:
		// All other permissions not handled by custom workspace permissions
		// Return false (no access)
		return false
	}
	return false
}

// checkCustomWorkspacePermissionsFromAccess is DEPRECATED: Logic moved to getPermissionsFromWorkspaceAccess
//
//nolint:unused // Kept for reference, will be removed in cleanup
func (s *Service) checkCustomWorkspacePermissionsFromAccess(
	access *models.TeamWorkspaceAccess,
	permission Permission,
) bool {
	//nolint:exhaustive // Only handling workspace-level permissions, not all permissions
	switch permission {
	case PermissionRuns:
		if access.Runs != nil {
			level := *access.Runs
			return level == "read" || level == "plan" || level == "apply"
		}
	case PermissionVariables:
		if access.Variables != nil {
			level := *access.Variables
			return level == "read" || level == "write"
		}
	case PermissionStateVersions:
		if access.StateVersions != nil {
			level := *access.StateVersions
			return level == "read" || level == "read-outputs" || level == "write"
		}
	case PermissionSentinelMocks:
		if access.SentinelMocks != nil {
			level := *access.SentinelMocks
			return level == "read"
		}
	case PermissionWorkspaceLocking:
		if access.WorkspaceLocking != nil {
			return *access.WorkspaceLocking
		}
	case PermissionRunTasks:
		if access.RunTasks != nil {
			return *access.RunTasks
		}
	default:
		// All other permissions not handled by custom workspace permissions
		// Return false (no access)
		return false
	}
	return false
}

// CheckWorkspacePermission is a convenience method for checking workspace permissions
// It uses CheckResourcePermission internally with the workspace resource type
func (s *Service) CheckWorkspacePermission(
	ctx context.Context,
	userID uuid.UUID,
	workspaceID string,
	permission Permission,
	projectID uuid.UUID,
) (bool, error) {
	return s.CheckResourcePermission(
		ctx,
		userID,
		ResourceTypeTerraformWorkspace,
		workspaceID,
		permission,
		&projectID,
	)
}

// CheckStateVersionPermission checks if user can access state versions
// Granular permission: none, read, read-outputs, write
func (s *Service) CheckStateVersionPermission(
	ctx context.Context,
	userID uuid.UUID,
	workspaceID string,
	projectID uuid.UUID,
	level string, // "none", "read", "read-outputs", "write"
) (bool, error) {
	switch level {
	case "none":
		return false, nil
	case "read", "read-outputs":
		return s.CheckWorkspacePermission(ctx, userID, workspaceID, PermissionStateVersions, projectID)
	case "write":
		// Write requires both state_versions permission and workspace write
		hasStateVersions, err := s.CheckWorkspacePermission(ctx, userID, workspaceID, PermissionStateVersions, projectID)
		if err != nil || !hasStateVersions {
			return false, err
		}
		return s.CheckWorkspacePermission(ctx, userID, workspaceID, PermissionWorkspaceWrite, projectID)
	default:
		return false, fmt.Errorf("invalid state version permission level: %s", level)
	}
}

// CheckVariablePermission checks if user can access variables
// Granular permission: none, read, write
func (s *Service) CheckVariablePermission(
	ctx context.Context,
	userID uuid.UUID,
	workspaceID string,
	projectID uuid.UUID,
	level string, // "none", "read", "write"
) (bool, error) {
	switch level {
	case "none":
		return false, nil
	case "read":
		return s.CheckWorkspacePermission(ctx, userID, workspaceID, PermissionVariables, projectID)
	case "write":
		// Write requires both variables permission and workspace write
		hasVariables, err := s.CheckWorkspacePermission(ctx, userID, workspaceID, PermissionVariables, projectID)
		if err != nil || !hasVariables {
			return false, err
		}
		return s.CheckWorkspacePermission(ctx, userID, workspaceID, PermissionWorkspaceWrite, projectID)
	default:
		return false, fmt.Errorf("invalid variable permission level: %s", level)
	}
}

// CheckRunPermission checks if user can access runs
// Granular permission: read, plan, apply
func (s *Service) CheckRunPermission(
	ctx context.Context,
	userID uuid.UUID,
	workspaceID string,
	projectID uuid.UUID,
	level string, // "read", "plan", "apply"
) (bool, error) {
	switch level {
	case "read":
		// Read level: Can view runs, but NOT create/plan them
		return s.CheckWorkspacePermission(ctx, userID, workspaceID, PermissionRunRead, projectID)
	case "plan":
		// Plan level: Requires PermissionRuns (can create/plan runs)
		return s.CheckWorkspacePermission(ctx, userID, workspaceID, PermissionRuns, projectID)
	case "apply":
		// Apply level: Requires both PermissionRuns (can plan) and PermissionWorkspaceWrite (can apply)
		hasRuns, err := s.CheckWorkspacePermission(ctx, userID, workspaceID, PermissionRuns, projectID)
		if err != nil || !hasRuns {
			return false, err
		}
		return s.CheckWorkspacePermission(ctx, userID, workspaceID, PermissionWorkspaceWrite, projectID)
	default:
		return false, fmt.Errorf("invalid run permission level: %s", level)
	}
}

// CheckWorkspaceLockingPermission checks if user can lock/unlock workspaces
func (s *Service) CheckWorkspaceLockingPermission(
	ctx context.Context,
	userID uuid.UUID,
	workspaceID string,
	projectID uuid.UUID,
) (bool, error) {
	return s.CheckWorkspacePermission(ctx, userID, workspaceID, PermissionWorkspaceLocking, projectID)
}

// CheckRunTasksPermission checks if user can manage run tasks
func (s *Service) CheckRunTasksPermission(
	ctx context.Context,
	userID uuid.UUID,
	workspaceID string,
	projectID uuid.UUID,
) (bool, error) {
	return s.CheckWorkspacePermission(ctx, userID, workspaceID, PermissionRunTasks, projectID)
}

// CheckAnsibleResourcePermission is a convenience method for checking Ansible resource permissions
func (s *Service) CheckAnsibleResourcePermission(
	ctx context.Context,
	userID uuid.UUID,
	resourceType ResourceType,
	resourceID string,
	permission Permission,
	projectID *uuid.UUID,
) (bool, error) {
	return s.CheckResourcePermission(ctx, userID, resourceType, resourceID, permission, projectID)
}

// CheckOrgManageMembership checks if user can manage organization memberships (add/remove users)
// Team-based: User must be in "owners" team OR have team with manage-membership permission
func (s *Service) CheckOrgManageMembership(ctx context.Context, userID, organizationID uuid.UUID) (bool, error) {
	// Tenant isolation: user must have at least one team in the org (team-based access)
	inOrg, err := s.orgRepo.UserInOrg(userID, organizationID)
	if err != nil || !inOrg {
		return false, nil
	}

	if s.teamRepo == nil {
		return false, fmt.Errorf("team repository not available")
	}

	// Get all teams user is member of
	teams, err := s.teamRepo.GetTeamsByUserID(userID, organizationID)
	if err != nil {
		return false, err
	}

	// Check if user is in "owners" team (always has full permissions)
	for _, team := range teams {
		if team.Name == "owners" {
			return true, nil
		}

		// Check if team has manage-membership permission
		// GetTeamsByUserID already preloads OrganizationAccess, so use it directly
		if team.OrganizationAccess != nil && team.OrganizationAccess.ManageMembership {
			return true, nil
		}
	}

	return false, nil
}

// CheckOrgManageTeams checks if user can manage teams (create/update/delete teams)
// Team-based: User must be in "owners" team OR have team with manage-teams permission
func (s *Service) CheckOrgManageTeams(ctx context.Context, userID, organizationID uuid.UUID) (bool, error) {
	// Tenant isolation: user must have at least one team in the org (team-based access)
	inOrg, err := s.orgRepo.UserInOrg(userID, organizationID)
	if err != nil || !inOrg {
		return false, nil
	}

	if s.teamRepo == nil {
		return false, fmt.Errorf("team repository not available")
	}

	// Get all teams user is member of
	teams, err := s.teamRepo.GetTeamsByUserID(userID, organizationID)
	if err != nil {
		return false, err
	}

	// Check if user is in "owners" team (always has full permissions)
	for _, team := range teams {
		if team.Name == "owners" {
			return true, nil
		}

		// Check if team has manage-teams permission
		// GetTeamsByUserID already preloads OrganizationAccess, so use it directly
		if team.OrganizationAccess != nil && team.OrganizationAccess.ManageTeams {
			return true, nil
		}
	}

	return false, nil
}

// CheckOrgManageProjects checks if user can manage projects (create/update/delete projects)
// Team-based: Check if user has manage-projects permission from any team
func (s *Service) CheckOrgManageProjects(ctx context.Context, userID, organizationID uuid.UUID) (bool, error) {
	return s.checkOrgPermission(ctx, userID, organizationID, PermissionOrgManageProjects)
}

// CheckOrgManageWorkspaces checks if user can manage workspaces (create/update/delete workspaces)
// Team-based: Check if user has manage-workspaces permission from any team
func (s *Service) CheckOrgManageWorkspaces(ctx context.Context, userID, organizationID uuid.UUID) (bool, error) {
	return s.checkOrgPermission(ctx, userID, organizationID, PermissionOrgManageWorkspaces)
}

// CheckOrgManageVCSSettings checks if user can manage VCS settings
func (s *Service) CheckOrgManageVCSSettings(ctx context.Context, userID, organizationID uuid.UUID) (bool, error) {
	return s.checkOrgPermission(ctx, userID, organizationID, PermissionOrgManageVCSSettings)
}

// CheckOrgReadProjects checks if user can read projects
func (s *Service) CheckOrgReadProjects(ctx context.Context, userID, organizationID uuid.UUID) (bool, error) {
	return s.checkOrgPermission(ctx, userID, organizationID, PermissionOrgReadProjects)
}

// CheckOrgReadWorkspaces checks if user can read workspaces
func (s *Service) CheckOrgReadWorkspaces(ctx context.Context, userID, organizationID uuid.UUID) (bool, error) {
	return s.checkOrgPermission(ctx, userID, organizationID, PermissionOrgReadWorkspaces)
}

// CheckOrgManageAgentPools checks if user can manage agent pools (create/update/delete)
func (s *Service) CheckOrgManageAgentPools(ctx context.Context, userID, organizationID uuid.UUID) (bool, error) {
	return s.checkOrgPermission(ctx, userID, organizationID, PermissionOrgManageAgentPools)
}

// checkOrgPermission is a helper to check organization-level permissions from team memberships
func (s *Service) checkOrgPermission(ctx context.Context, userID, organizationID uuid.UUID, permission Permission) (bool, error) {
	// Tenant isolation: user must have at least one team in the org (team-based access)
	inOrg, err := s.orgRepo.UserInOrg(userID, organizationID)
	if err != nil || !inOrg {
		return false, nil
	}

	if s.teamRepo == nil {
		return false, fmt.Errorf("team repository not available")
	}

	// Get all teams user is member of
	teams, err := s.teamRepo.GetTeamsByUserID(userID, organizationID)
	if err != nil {
		return false, err
	}

	// Check if user is in "owners" team (always has full permissions)
	for _, team := range teams {
		if team.Name == "owners" {
			return true, nil
		}
	}

	// Collect all permissions from all teams
	// GetTeamsByUserID already preloads OrganizationAccess, so use it directly
	allPermissions := make(map[Permission]bool)
	for _, team := range teams {
		// Use preloaded OrganizationAccess if available, otherwise fetch it
		var orgAccess *models.TeamOrganizationAccess
		if team.OrganizationAccess != nil {
			orgAccess = team.OrganizationAccess
		} else {
			// Fallback: fetch if not preloaded (shouldn't happen, but be defensive)
			var err error
			orgAccess, err = s.teamRepo.GetOrganizationAccess(team.ID)
			if err != nil {
				continue
			}
		}

		if orgAccess != nil {
			teamPerms := s.getPermissionsFromOrganizationAccess(orgAccess)
			for perm := range teamPerms {
				allPermissions[perm] = true
			}
		}
	}

	return allPermissions[permission], nil
}
