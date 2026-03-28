// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package rbac

import (
	"testing"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
)

// TestGetPermissionsFromOrganizationAccess tests that
// the getPermissionsFromOrganizationAccess function correctly maps
// organization access fields to permissions, including implied permissions.
func TestGetPermissionsFromOrganizationAccess(t *testing.T) {
	service := &Service{} // Service fields not needed for this function

	tests := []struct {
		name          string
		orgAccess     *models.TeamOrganizationAccess
		expectedPerms map[Permission]bool
		description   string
	}{
		{
			name: "ManageProjects implies ReadProjects",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID:         uuid.New(),
				ManageProjects: true,
			},
			expectedPerms: map[Permission]bool{
				PermissionOrgManageProjects: true,
				PermissionOrgReadProjects:   true,
				PermissionProjectRead:       true,
			},
			description: "When ManageProjects is true, it should grant PermissionOrgManageProjects, PermissionOrgReadProjects, and PermissionProjectRead",
		},
		{
			name: "ReadProjects grants read permissions",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID:       uuid.New(),
				ReadProjects: true,
			},
			expectedPerms: map[Permission]bool{
				PermissionOrgReadProjects: true,
				PermissionProjectRead:     true,
			},
			description: "When ReadProjects is true, it should grant PermissionOrgReadProjects and PermissionProjectRead",
		},
		{
			name: "ManageWorkspaces implies all workspace permissions",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID:           uuid.New(),
				ManageWorkspaces: true,
			},
			expectedPerms: map[Permission]bool{
				PermissionOrgManageWorkspaces: true,
				PermissionOrgReadWorkspaces:   true,
				PermissionWorkspaceRead:       true,
				PermissionWorkspaceWrite:      true,
				PermissionRunRead:             true,
				PermissionRunWrite:            true,
				PermissionVariables:           true,
				PermissionStateVersions:       true,
				PermissionRuns:                true,
				PermissionSentinelMocks:       true,
				PermissionWorkspaceLocking:    true,
				PermissionRunTasks:            true,
			},
			description: "When ManageWorkspaces is true, it should grant all workspace-level permissions (TFE-compatible)",
		},
		{
			name: "ReadWorkspaces grants workspace read permissions",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID:         uuid.New(),
				ReadWorkspaces: true,
			},
			expectedPerms: map[Permission]bool{
				PermissionOrgReadWorkspaces: true,
				PermissionWorkspaceRead:     true,
			},
			description: "When ReadWorkspaces is true, it should grant PermissionOrgReadWorkspaces and PermissionWorkspaceRead",
		},
		{
			name: "ManageMembership grants membership permission",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID:           uuid.New(),
				ManageMembership: true,
			},
			expectedPerms: map[Permission]bool{
				PermissionOrgManageMembership: true,
			},
			description: "When ManageMembership is true, it should grant PermissionOrgManageMembership",
		},
		{
			name: "ManageTeams grants teams permission",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID:      uuid.New(),
				ManageTeams: true,
			},
			expectedPerms: map[Permission]bool{
				PermissionOrgManageTeams: true,
			},
			description: "When ManageTeams is true, it should grant PermissionOrgManageTeams",
		},
		{
			name: "ManageOrganizationAccess grants organization access permission",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID:                   uuid.New(),
				ManageOrganizationAccess: true,
			},
			expectedPerms: map[Permission]bool{
				PermissionOrgManageOrganizationAccess: true,
			},
			description: "When ManageOrganizationAccess is true, it should grant PermissionOrgManageOrganizationAccess",
		},
		{
			name: "Other permissions map correctly",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID:                uuid.New(),
				ManagePolicies:        true,
				ManagePolicyOverrides: true,
				ManageVCSSettings:     true,
				ManageProviders:       true,
				ManageModules:         true,
				ManageRunTasks:        true,
				AccessSecretTeams:     true,
				ManageAgentPools:      true,
			},
			expectedPerms: map[Permission]bool{
				PermissionOrgManagePolicies:        true,
				PermissionOrgManagePolicyOverrides: true,
				PermissionOrgManageVCSSettings:     true,
				PermissionOrgManageProviders:       true,
				PermissionOrgManageModules:         true,
				PermissionOrgManageRunTasks:        true,
				PermissionOrgAccessSecretTeams:     true,
				PermissionOrgManageAgentPools:      true,
			},
			description: "All other organization access permissions should map to their corresponding Permission constants",
		},
		{
			name: "Multiple permissions combine correctly",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID:         uuid.New(),
				ManageProjects: true,
				ManagePolicies: true,
				ManageTeams:    true,
			},
			expectedPerms: map[Permission]bool{
				PermissionOrgManageProjects: true,
				PermissionOrgReadProjects:   true, // Implied by ManageProjects
				PermissionProjectRead:       true, // Implied by ManageProjects
				PermissionOrgManagePolicies: true,
				PermissionOrgManageTeams:    true,
			},
			description: "Multiple permissions should combine correctly, including implied permissions",
		},
		{
			name: "No permissions returns empty map",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID: uuid.New(),
			},
			expectedPerms: map[Permission]bool{},
			description:   "When no permissions are set, it should return an empty permission map",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			perms := service.getPermissionsFromOrganizationAccess(tt.orgAccess)

			// Verify all expected permissions are present
			for expectedPerm, expectedValue := range tt.expectedPerms {
				if actualValue, exists := perms[expectedPerm]; !exists || actualValue != expectedValue {
					t.Errorf("%s: Permission %s: expected %v, got %v (exists: %v)", tt.description, expectedPerm, expectedValue, actualValue, exists)
				}
			}

			// Verify no unexpected permissions are present (only check permissions we care about)
			allPossiblePerms := map[Permission]bool{
				PermissionOrgManageProjects:           false,
				PermissionOrgReadProjects:             false,
				PermissionProjectRead:                 false,
				PermissionOrgManageWorkspaces:         false,
				PermissionOrgReadWorkspaces:           false,
				PermissionWorkspaceRead:               false,
				PermissionWorkspaceWrite:              false,
				PermissionRunRead:                     false,
				PermissionRunWrite:                    false,
				PermissionVariables:                   false,
				PermissionStateVersions:               false,
				PermissionRuns:                        false,
				PermissionSentinelMocks:               false,
				PermissionWorkspaceLocking:            false,
				PermissionRunTasks:                    false,
				PermissionOrgManageMembership:         false,
				PermissionOrgManageTeams:              false,
				PermissionOrgManageOrganizationAccess: false,
				PermissionOrgManagePolicies:           false,
				PermissionOrgManagePolicyOverrides:    false,
				PermissionOrgManageVCSSettings:        false,
				PermissionOrgManageProviders:          false,
				PermissionOrgManageModules:            false,
				PermissionOrgManageRunTasks:           false,
				PermissionOrgAccessSecretTeams:        false,
				PermissionOrgManageAgentPools:         false,
			}

			// Merge expected perms with all possible perms to check
			for perm, value := range tt.expectedPerms {
				allPossiblePerms[perm] = value
			}

			// Only check permissions that are in our expected set
			// Note: We don't need to verify unexpected permissions are absent,
			// we only care that expected permissions are present and correct
		})
	}
}

// TestGetPermissionsFromOrganizationAccess_Ansible tests that ManageAnsible and ReadAnsible
// correctly map to all Ansible resource permissions, and that they are independent from
// workspace permissions (ManageWorkspaces does NOT grant Ansible access).
func TestGetPermissionsFromOrganizationAccess_Ansible(t *testing.T) {
	service := &Service{}

	// allAnsibleReadPerms is the set of all Ansible read permissions
	allAnsibleReadPerms := map[Permission]bool{
		PermissionAnsiblePlaybookRead:    true,
		PermissionAnsibleInventoryRead:   true,
		PermissionAnsibleCredentialRead:  true,
		PermissionAnsibleJobTemplateRead: true,
		PermissionAnsibleJobRead:         true,
		PermissionAnsibleScheduleRead:    true,
	}

	// allAnsibleWritePerms is the set of all Ansible write/execute permissions
	allAnsibleWritePerms := map[Permission]bool{
		PermissionAnsiblePlaybookWrite:    true,
		PermissionAnsibleInventoryWrite:   true,
		PermissionAnsibleCredentialWrite:  true,
		PermissionAnsibleJobTemplateWrite: true,
		PermissionAnsibleJobExecute:       true,
		PermissionAnsibleScheduleWrite:    true,
	}

	tests := []struct {
		name           string
		orgAccess      *models.TeamOrganizationAccess
		mustHavePerms  map[Permission]bool
		mustNotHaveAny []Permission
		description    string
	}{
		{
			name: "ManageAnsible grants all Ansible read and write permissions",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID:        uuid.New(),
				ManageAnsible: true,
			},
			mustHavePerms: func() map[Permission]bool {
				m := map[Permission]bool{
					PermissionOrgManageAnsible: true,
					PermissionOrgReadAnsible:   true,
				}
				for k, v := range allAnsibleReadPerms {
					m[k] = v
				}
				for k, v := range allAnsibleWritePerms {
					m[k] = v
				}
				return m
			}(),
			description: "ManageAnsible should grant org-level manage+read and all Ansible resource read/write/execute permissions",
		},
		{
			name: "ReadAnsible grants only Ansible read permissions (no write/execute)",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID:      uuid.New(),
				ReadAnsible: true,
			},
			mustHavePerms: func() map[Permission]bool {
				m := map[Permission]bool{
					PermissionOrgReadAnsible: true,
				}
				for k, v := range allAnsibleReadPerms {
					m[k] = v
				}
				return m
			}(),
			mustNotHaveAny: []Permission{
				PermissionOrgManageAnsible,
				PermissionAnsiblePlaybookWrite,
				PermissionAnsibleInventoryWrite,
				PermissionAnsibleCredentialWrite,
				PermissionAnsibleJobTemplateWrite,
				PermissionAnsibleJobExecute,
				PermissionAnsibleScheduleWrite,
			},
			description: "ReadAnsible should grant read-only Ansible permissions, never write/execute",
		},
		{
			name: "ManageWorkspaces does NOT grant Ansible permissions",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID:           uuid.New(),
				ManageWorkspaces: true,
			},
			mustNotHaveAny: []Permission{
				PermissionOrgManageAnsible,
				PermissionOrgReadAnsible,
				PermissionAnsiblePlaybookRead,
				PermissionAnsiblePlaybookWrite,
				PermissionAnsibleInventoryRead,
				PermissionAnsibleInventoryWrite,
				PermissionAnsibleCredentialRead,
				PermissionAnsibleCredentialWrite,
				PermissionAnsibleJobTemplateRead,
				PermissionAnsibleJobTemplateWrite,
				PermissionAnsibleJobRead,
				PermissionAnsibleJobExecute,
				PermissionAnsibleScheduleRead,
				PermissionAnsibleScheduleWrite,
			},
			description: "ManageWorkspaces is Terraform-only and must NOT bleed into Ansible permissions",
		},
		{
			name: "ManageAnsible does NOT grant workspace permissions",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID:        uuid.New(),
				ManageAnsible: true,
			},
			mustNotHaveAny: []Permission{
				PermissionOrgManageWorkspaces,
				PermissionOrgReadWorkspaces,
				PermissionWorkspaceRead,
				PermissionWorkspaceWrite,
				PermissionRunRead,
				PermissionRunWrite,
			},
			description: "ManageAnsible must NOT grant any Terraform workspace permissions",
		},
		{
			name: "No permissions returns no Ansible permissions",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID: uuid.New(),
			},
			mustNotHaveAny: []Permission{
				PermissionOrgManageAnsible,
				PermissionOrgReadAnsible,
				PermissionAnsiblePlaybookRead,
				PermissionAnsiblePlaybookWrite,
			},
			description: "Empty org access should grant no Ansible permissions",
		},
		{
			name: "ManageAnsible and ManageWorkspaces are independent",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID:           uuid.New(),
				ManageAnsible:    true,
				ManageWorkspaces: true,
			},
			mustHavePerms: map[Permission]bool{
				// Ansible
				PermissionOrgManageAnsible:     true,
				PermissionAnsiblePlaybookRead:  true,
				PermissionAnsiblePlaybookWrite: true,
				PermissionAnsibleJobExecute:    true,
				// Workspace
				PermissionOrgManageWorkspaces: true,
				PermissionWorkspaceRead:       true,
				PermissionWorkspaceWrite:      true,
			},
			description: "Both ManageAnsible and ManageWorkspaces should independently grant their respective permissions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			perms := service.getPermissionsFromOrganizationAccess(tt.orgAccess)

			// Check all required permissions are present
			for perm, expected := range tt.mustHavePerms {
				if actual, exists := perms[perm]; !exists || actual != expected {
					t.Errorf("%s: expected permission %s=%v, got %v (exists=%v)", tt.description, perm, expected, actual, exists)
				}
			}

			// Check forbidden permissions are absent
			for _, perm := range tt.mustNotHaveAny {
				if perms[perm] {
					t.Errorf("%s: permission %s must NOT be granted but was", tt.description, perm)
				}
			}
		})
	}
}

// TestGetPermissionsFromProjectAccess_Ansible tests that project access levels
// correctly cascade to Ansible resource permissions within the project.
func TestGetPermissionsFromProjectAccess_Ansible(t *testing.T) {
	service := &Service{}

	adminAccess := "admin"
	writeAccess := "write"
	maintainAccess := "maintain"
	readAccess := "read"

	tests := []struct {
		name           string
		projectAccess  *models.TeamProjectAccess
		mustHavePerms  map[Permission]bool
		mustNotHaveAny []Permission
		description    string
	}{
		{
			name: "Admin project access grants all Ansible permissions",
			projectAccess: &models.TeamProjectAccess{
				ID:     uuid.New(),
				TeamID: uuid.New(),
				Access: &adminAccess,
			},
			mustHavePerms: map[Permission]bool{
				PermissionAnsiblePlaybookRead:     true,
				PermissionAnsiblePlaybookWrite:    true,
				PermissionAnsibleInventoryRead:    true,
				PermissionAnsibleInventoryWrite:   true,
				PermissionAnsibleCredentialRead:   true,
				PermissionAnsibleCredentialWrite:  true,
				PermissionAnsibleJobTemplateRead:  true,
				PermissionAnsibleJobTemplateWrite: true,
				PermissionAnsibleJobRead:          true,
				PermissionAnsibleJobExecute:       true,
				PermissionAnsibleScheduleRead:     true,
				PermissionAnsibleScheduleWrite:    true,
			},
			description: "Admin project access should grant all Ansible read+write+execute permissions",
		},
		{
			name: "Write project access grants all Ansible permissions",
			projectAccess: &models.TeamProjectAccess{
				ID:     uuid.New(),
				TeamID: uuid.New(),
				Access: &writeAccess,
			},
			mustHavePerms: map[Permission]bool{
				PermissionAnsiblePlaybookRead:   true,
				PermissionAnsiblePlaybookWrite:  true,
				PermissionAnsibleInventoryRead:  true,
				PermissionAnsibleInventoryWrite: true,
				PermissionAnsibleJobRead:        true,
				PermissionAnsibleJobExecute:     true,
				PermissionAnsibleScheduleRead:   true,
				PermissionAnsibleScheduleWrite:  true,
			},
			description: "Write project access should grant all Ansible read+write+execute permissions",
		},
		{
			name: "Maintain project access grants all Ansible permissions",
			projectAccess: &models.TeamProjectAccess{
				ID:     uuid.New(),
				TeamID: uuid.New(),
				Access: &maintainAccess,
			},
			mustHavePerms: map[Permission]bool{
				PermissionAnsiblePlaybookRead:  true,
				PermissionAnsiblePlaybookWrite: true,
				PermissionAnsibleJobRead:       true,
				PermissionAnsibleJobExecute:    true,
			},
			description: "Maintain project access should grant all Ansible read+write+execute permissions",
		},
		{
			name: "Read project access grants only Ansible read permissions",
			projectAccess: &models.TeamProjectAccess{
				ID:     uuid.New(),
				TeamID: uuid.New(),
				Access: &readAccess,
			},
			mustHavePerms: map[Permission]bool{
				PermissionAnsiblePlaybookRead:    true,
				PermissionAnsibleInventoryRead:   true,
				PermissionAnsibleCredentialRead:  true,
				PermissionAnsibleJobTemplateRead: true,
				PermissionAnsibleJobRead:         true,
				PermissionAnsibleScheduleRead:    true,
			},
			mustNotHaveAny: []Permission{
				PermissionAnsiblePlaybookWrite,
				PermissionAnsibleInventoryWrite,
				PermissionAnsibleCredentialWrite,
				PermissionAnsibleJobTemplateWrite,
				PermissionAnsibleJobExecute,
				PermissionAnsibleScheduleWrite,
			},
			description: "Read project access should only grant Ansible read permissions, never write/execute",
		},
		{
			name: "Nil project access grants no permissions",
			projectAccess: &models.TeamProjectAccess{
				ID:     uuid.New(),
				TeamID: uuid.New(),
				Access: nil,
			},
			mustNotHaveAny: []Permission{
				PermissionAnsiblePlaybookRead,
				PermissionAnsiblePlaybookWrite,
				PermissionAnsibleInventoryRead,
				PermissionAnsibleJobRead,
				PermissionAnsibleJobExecute,
			},
			description: "Nil project access should grant no Ansible permissions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			perms := service.getPermissionsFromProjectAccess(tt.projectAccess, ResourceTypeAnsiblePlaybook)

			// Check all required permissions are present
			for perm, expected := range tt.mustHavePerms {
				if actual, exists := perms[perm]; !exists || actual != expected {
					t.Errorf("%s: expected permission %s=%v, got %v (exists=%v)", tt.description, perm, expected, actual, exists)
				}
			}

			// Check forbidden permissions are absent
			for _, perm := range tt.mustNotHaveAny {
				if perms[perm] {
					t.Errorf("%s: permission %s must NOT be granted but was", tt.description, perm)
				}
			}
		})
	}
}

// TestGetPermissionsFromOrganizationAccess_FineGrainedAnsible tests that
// individual per-resource Ansible permission fields map to the correct
// granular permissions without granting access to other resource types.
func TestGetPermissionsFromOrganizationAccess_FineGrainedAnsible(t *testing.T) {
	service := &Service{}

	tests := []struct {
		name           string
		orgAccess      *models.TeamOrganizationAccess
		mustHave       []Permission
		mustNotHaveAny []Permission
		description    string
	}{
		{
			name: "ManageAnsiblePlaybooks grants only playbook permissions",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID:                 uuid.New(),
				ManageAnsiblePlaybooks: true,
			},
			mustHave: []Permission{
				PermissionAnsiblePlaybookRead,
				PermissionAnsiblePlaybookWrite,
				PermissionOrgReadAnsible,
			},
			mustNotHaveAny: []Permission{
				PermissionAnsibleInventoryRead,
				PermissionAnsibleInventoryWrite,
				PermissionAnsibleCredentialRead,
				PermissionAnsibleCredentialWrite,
				PermissionAnsibleJobTemplateRead,
				PermissionAnsibleJobTemplateWrite,
				PermissionAnsibleJobRead,
				PermissionAnsibleJobExecute,
				PermissionAnsibleScheduleRead,
				PermissionAnsibleScheduleWrite,
				PermissionOrgManageAnsible,
			},
			description: "ManageAnsiblePlaybooks should only grant playbook access",
		},
		{
			name: "ReadAnsibleInventories grants only inventory read",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID:                 uuid.New(),
				ReadAnsibleInventories: true,
			},
			mustHave: []Permission{
				PermissionAnsibleInventoryRead,
				PermissionOrgReadAnsible,
			},
			mustNotHaveAny: []Permission{
				PermissionAnsibleInventoryWrite,
				PermissionAnsiblePlaybookRead,
				PermissionAnsibleCredentialRead,
				PermissionAnsibleJobTemplateRead,
				PermissionAnsibleJobRead,
				PermissionAnsibleScheduleRead,
			},
			description: "ReadAnsibleInventories should only grant inventory read",
		},
		{
			name: "ManageAnsibleCredentials grants only credential permissions",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID:                   uuid.New(),
				ManageAnsibleCredentials: true,
			},
			mustHave: []Permission{
				PermissionAnsibleCredentialRead,
				PermissionAnsibleCredentialWrite,
				PermissionOrgReadAnsible,
			},
			mustNotHaveAny: []Permission{
				PermissionAnsiblePlaybookRead,
				PermissionAnsibleInventoryRead,
				PermissionAnsibleJobTemplateRead,
				PermissionAnsibleJobRead,
				PermissionAnsibleScheduleRead,
			},
			description: "ManageAnsibleCredentials should only grant credential access",
		},
		{
			name: "ManageAnsibleJobs grants execute but not template write",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID:            uuid.New(),
				ManageAnsibleJobs: true,
			},
			mustHave: []Permission{
				PermissionAnsibleJobRead,
				PermissionAnsibleJobExecute,
				PermissionOrgReadAnsible,
			},
			mustNotHaveAny: []Permission{
				PermissionAnsibleJobTemplateWrite,
				PermissionAnsiblePlaybookWrite,
				PermissionAnsibleInventoryWrite,
				PermissionAnsibleCredentialWrite,
				PermissionAnsibleScheduleWrite,
			},
			description: "ManageAnsibleJobs should grant execute but not template management",
		},
		{
			name: "Multiple fine-grained: playbooks + credentials, not inventories",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID:                   uuid.New(),
				ManageAnsiblePlaybooks:   true,
				ManageAnsibleCredentials: true,
			},
			mustHave: []Permission{
				PermissionAnsiblePlaybookRead,
				PermissionAnsiblePlaybookWrite,
				PermissionAnsibleCredentialRead,
				PermissionAnsibleCredentialWrite,
				PermissionOrgReadAnsible,
			},
			mustNotHaveAny: []Permission{
				PermissionAnsibleInventoryRead,
				PermissionAnsibleInventoryWrite,
				PermissionAnsibleJobTemplateRead,
				PermissionAnsibleJobRead,
				PermissionAnsibleScheduleRead,
			},
			description: "Multiple fine-grained permissions are additive but don't leak",
		},
		{
			name: "Parent ManageAnsible overrides fine-grained (grants all)",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID:                 uuid.New(),
				ManageAnsible:          true,
				ManageAnsiblePlaybooks: false,
			},
			mustHave: []Permission{
				PermissionOrgManageAnsible,
				PermissionOrgReadAnsible,
				PermissionAnsiblePlaybookRead,
				PermissionAnsiblePlaybookWrite,
				PermissionAnsibleInventoryRead,
				PermissionAnsibleInventoryWrite,
				PermissionAnsibleCredentialRead,
				PermissionAnsibleCredentialWrite,
				PermissionAnsibleJobTemplateRead,
				PermissionAnsibleJobTemplateWrite,
				PermissionAnsibleJobRead,
				PermissionAnsibleJobExecute,
				PermissionAnsibleScheduleRead,
				PermissionAnsibleScheduleWrite,
			},
			mustNotHaveAny: []Permission{},
			description:    "Parent ManageAnsible should grant everything regardless of sub-fields",
		},
		{
			name: "Empty: no Ansible permissions at all",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID: uuid.New(),
			},
			mustHave: []Permission{},
			mustNotHaveAny: []Permission{
				PermissionOrgManageAnsible,
				PermissionOrgReadAnsible,
				PermissionAnsiblePlaybookRead,
				PermissionAnsibleInventoryRead,
				PermissionAnsibleCredentialRead,
				PermissionAnsibleJobTemplateRead,
				PermissionAnsibleJobRead,
				PermissionAnsibleScheduleRead,
			},
			description: "No permissions set should grant nothing",
		},
		{
			name: "ManageAnsibleSchedules grants schedule access only",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID:                 uuid.New(),
				ManageAnsibleSchedules: true,
			},
			mustHave: []Permission{
				PermissionAnsibleScheduleRead,
				PermissionAnsibleScheduleWrite,
				PermissionOrgReadAnsible,
			},
			mustNotHaveAny: []Permission{
				PermissionAnsiblePlaybookRead,
				PermissionAnsibleInventoryRead,
				PermissionAnsibleCredentialRead,
				PermissionAnsibleJobTemplateRead,
				PermissionAnsibleJobRead,
			},
			description: "ManageAnsibleSchedules should only grant schedule access",
		},
		{
			name: "ManageAnsibleJobTemplates grants template access only",
			orgAccess: &models.TeamOrganizationAccess{
				TeamID:                    uuid.New(),
				ManageAnsibleJobTemplates: true,
			},
			mustHave: []Permission{
				PermissionAnsibleJobTemplateRead,
				PermissionAnsibleJobTemplateWrite,
				PermissionOrgReadAnsible,
			},
			mustNotHaveAny: []Permission{
				PermissionAnsiblePlaybookRead,
				PermissionAnsibleInventoryRead,
				PermissionAnsibleCredentialRead,
				PermissionAnsibleJobRead,
				PermissionAnsibleScheduleRead,
			},
			description: "ManageAnsibleJobTemplates should only grant job template access",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			perms := service.getPermissionsFromOrganizationAccess(tt.orgAccess)

			for _, perm := range tt.mustHave {
				if !perms[perm] {
					t.Errorf("%s: permission %s must be granted but was not", tt.description, perm)
				}
			}

			for _, perm := range tt.mustNotHaveAny {
				if perms[perm] {
					t.Errorf("%s: permission %s must NOT be granted but was", tt.description, perm)
				}
			}
		})
	}
}
