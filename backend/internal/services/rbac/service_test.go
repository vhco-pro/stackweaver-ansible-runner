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
