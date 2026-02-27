// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"testing"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
)

// TestUpdateOrganizationAccessFromRequest_MutualExclusivity tests that
// the updateOrganizationAccessFromRequest function properly handles mutual exclusivity
// for radio button groups (projects, workspaces, team permissions).
func TestUpdateOrganizationAccessFromRequest_MutualExclusivity(t *testing.T) {
	handler := &TeamHandlerV2{}

	tests := []struct {
		name           string
		initialAccess  *models.TeamOrganizationAccess
		requestAccess  map[string]interface{}
		expectedAccess *models.TeamOrganizationAccess
		description    string
	}{
		// Project permissions: manage-projects and read-projects are mutually exclusive
		{
			name: "Setting manage-projects clears read-projects",
			initialAccess: &models.TeamOrganizationAccess{
				TeamID:       uuid.New(),
				ReadProjects: true,
			},
			requestAccess: map[string]interface{}{
				"manage-projects": true,
			},
			expectedAccess: &models.TeamOrganizationAccess{
				ManageProjects: true,
				ReadProjects:   false,
			},
			description: "When setting manage-projects to true, read-projects should be cleared",
		},
		{
			name: "Setting read-projects clears manage-projects",
			initialAccess: &models.TeamOrganizationAccess{
				TeamID:         uuid.New(),
				ManageProjects: true,
			},
			requestAccess: map[string]interface{}{
				"read-projects": true,
			},
			expectedAccess: &models.TeamOrganizationAccess{
				ManageProjects: false,
				ReadProjects:   true,
			},
			description: "When setting read-projects to true, manage-projects should be cleared",
		},
		{
			name: "Setting manage-projects to false does not affect read-projects",
			initialAccess: &models.TeamOrganizationAccess{
				TeamID:         uuid.New(),
				ManageProjects: true,
				ReadProjects:   false,
			},
			requestAccess: map[string]interface{}{
				"manage-projects": false,
			},
			expectedAccess: &models.TeamOrganizationAccess{
				ManageProjects: false,
				ReadProjects:   false,
			},
			description: "When setting manage-projects to false, read-projects should remain false (not cleared, just set to false)",
		},
		// Workspace permissions: manage-workspaces and read-workspaces are mutually exclusive
		{
			name: "Setting manage-workspaces clears read-workspaces",
			initialAccess: &models.TeamOrganizationAccess{
				TeamID:         uuid.New(),
				ReadWorkspaces: true,
			},
			requestAccess: map[string]interface{}{
				"manage-workspaces": true,
			},
			expectedAccess: &models.TeamOrganizationAccess{
				ManageWorkspaces: true,
				ReadWorkspaces:   false,
			},
			description: "When setting manage-workspaces to true, read-workspaces should be cleared",
		},
		{
			name: "Setting read-workspaces clears manage-workspaces",
			initialAccess: &models.TeamOrganizationAccess{
				TeamID:           uuid.New(),
				ManageWorkspaces: true,
			},
			requestAccess: map[string]interface{}{
				"read-workspaces": true,
			},
			expectedAccess: &models.TeamOrganizationAccess{
				ManageWorkspaces: false,
				ReadWorkspaces:   true,
			},
			description: "When setting read-workspaces to true, manage-workspaces should be cleared",
		},
		// Team permissions: manage-organization-access, manage-teams, manage-membership are mutually exclusive
		{
			name: "Setting manage-organization-access clears manage-teams and manage-membership",
			initialAccess: &models.TeamOrganizationAccess{
				TeamID:           uuid.New(),
				ManageTeams:      true,
				ManageMembership: true,
			},
			requestAccess: map[string]interface{}{
				"manage-organization-access": true,
			},
			expectedAccess: &models.TeamOrganizationAccess{
				ManageOrganizationAccess: true,
				ManageTeams:              false,
				ManageMembership:         false,
			},
			description: "When setting manage-organization-access to true, manage-teams and manage-membership should be cleared",
		},
		{
			name: "Setting manage-teams clears manage-organization-access",
			initialAccess: &models.TeamOrganizationAccess{
				TeamID:                   uuid.New(),
				ManageOrganizationAccess: true,
			},
			requestAccess: map[string]interface{}{
				"manage-teams": true,
			},
			expectedAccess: &models.TeamOrganizationAccess{
				ManageOrganizationAccess: false,
				ManageTeams:              true,
			},
			description: "When setting manage-teams to true, manage-organization-access should be cleared",
		},
		// Non-mutually-exclusive permissions should update independently
		{
			name: "Non-mutually-exclusive permissions update independently",
			initialAccess: &models.TeamOrganizationAccess{
				TeamID:         uuid.New(),
				ManagePolicies: true,
				ManageProjects: true,
			},
			requestAccess: map[string]interface{}{
				"manage-policies": false,
			},
			expectedAccess: &models.TeamOrganizationAccess{
				ManagePolicies: false,
				ManageProjects: true, // Should remain unchanged
			},
			description: "Non-mutually-exclusive permissions should update independently",
		},
		// Multiple mutually exclusive groups in one request
		{
			name: "Multiple mutually exclusive groups update correctly",
			initialAccess: &models.TeamOrganizationAccess{
				TeamID:         uuid.New(),
				ReadProjects:   true,
				ReadWorkspaces: true,
			},
			requestAccess: map[string]interface{}{
				"manage-projects":   true,
				"manage-workspaces": true,
			},
			expectedAccess: &models.TeamOrganizationAccess{
				ManageProjects:   true,
				ReadProjects:     false,
				ManageWorkspaces: true,
				ReadWorkspaces:   false,
			},
			description: "Multiple mutually exclusive groups should update correctly in one request",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a copy of initial access to avoid modifying the test case
			orgAccess := &models.TeamOrganizationAccess{
				TeamID:                   tt.initialAccess.TeamID,
				ManagePolicies:           tt.initialAccess.ManagePolicies,
				ManagePolicyOverrides:    tt.initialAccess.ManagePolicyOverrides,
				ManageVCSSettings:        tt.initialAccess.ManageVCSSettings,
				ManageProviders:          tt.initialAccess.ManageProviders,
				ManageModules:            tt.initialAccess.ManageModules,
				ManageRunTasks:           tt.initialAccess.ManageRunTasks,
				AccessSecretTeams:        tt.initialAccess.AccessSecretTeams,
				ManageAgentPools:         tt.initialAccess.ManageAgentPools,
				ManageProjects:           tt.initialAccess.ManageProjects,
				ReadProjects:             tt.initialAccess.ReadProjects,
				ManageWorkspaces:         tt.initialAccess.ManageWorkspaces,
				ReadWorkspaces:           tt.initialAccess.ReadWorkspaces,
				ManageMembership:         tt.initialAccess.ManageMembership,
				ManageTeams:              tt.initialAccess.ManageTeams,
				ManageOrganizationAccess: tt.initialAccess.ManageOrganizationAccess,
			}

			// Apply the update
			handler.updateOrganizationAccessFromRequest(orgAccess, tt.requestAccess)

			// Verify the result
			if orgAccess.ManageProjects != tt.expectedAccess.ManageProjects {
				t.Errorf("%s: ManageProjects: expected %v, got %v", tt.description, tt.expectedAccess.ManageProjects, orgAccess.ManageProjects)
			}
			if orgAccess.ReadProjects != tt.expectedAccess.ReadProjects {
				t.Errorf("%s: ReadProjects: expected %v, got %v", tt.description, tt.expectedAccess.ReadProjects, orgAccess.ReadProjects)
			}
			if orgAccess.ManageWorkspaces != tt.expectedAccess.ManageWorkspaces {
				t.Errorf("%s: ManageWorkspaces: expected %v, got %v", tt.description, tt.expectedAccess.ManageWorkspaces, orgAccess.ManageWorkspaces)
			}
			if orgAccess.ReadWorkspaces != tt.expectedAccess.ReadWorkspaces {
				t.Errorf("%s: ReadWorkspaces: expected %v, got %v", tt.description, tt.expectedAccess.ReadWorkspaces, orgAccess.ReadWorkspaces)
			}
			if orgAccess.ManageOrganizationAccess != tt.expectedAccess.ManageOrganizationAccess {
				t.Errorf("%s: ManageOrganizationAccess: expected %v, got %v", tt.description, tt.expectedAccess.ManageOrganizationAccess, orgAccess.ManageOrganizationAccess)
			}
			if orgAccess.ManageTeams != tt.expectedAccess.ManageTeams {
				t.Errorf("%s: ManageTeams: expected %v, got %v", tt.description, tt.expectedAccess.ManageTeams, orgAccess.ManageTeams)
			}
			if orgAccess.ManageMembership != tt.expectedAccess.ManageMembership {
				t.Errorf("%s: ManageMembership: expected %v, got %v", tt.description, tt.expectedAccess.ManageMembership, orgAccess.ManageMembership)
			}
		})
	}
}
