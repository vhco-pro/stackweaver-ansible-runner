// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package terraform

import (
	"testing"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
)

func TestCheckPoolAccess_OrgScoped(t *testing.T) {
	poolID := uuid.New()
	wsAllowed := "ws-allowed000001"
	wsExcluded := "ws-excluded00001"

	pool := &models.AgentPool{
		ID:                 poolID,
		OrganizationScoped: true,
		ExcludedWorkspaces: []models.Workspace{
			{ID: wsExcluded, Name: "excluded-ws"},
		},
	}

	tests := []struct {
		name        string
		workspaceID string
		projectID   *uuid.UUID
		wantAllowed bool
		wantReason  string
	}{
		{
			name:        "allowed workspace in org-scoped pool",
			workspaceID: wsAllowed,
			wantAllowed: true,
			wantReason:  "",
		},
		{
			name:        "excluded workspace in org-scoped pool",
			workspaceID: wsExcluded,
			wantAllowed: false,
			wantReason:  "Workspace is excluded from this agent pool",
		},
		{
			name:        "random workspace in org-scoped pool",
			workspaceID: "ws-random00000001",
			wantAllowed: true,
			wantReason:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed, reason := CheckPoolAccess(pool, tt.workspaceID, tt.projectID)
			if allowed != tt.wantAllowed {
				t.Errorf("CheckPoolAccess() allowed = %v, want %v", allowed, tt.wantAllowed)
			}
			if reason != tt.wantReason {
				t.Errorf("CheckPoolAccess() reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

func TestCheckPoolAccess_NonOrgScoped_AllowedWorkspace(t *testing.T) {
	poolID := uuid.New()
	wsAllowed := "ws-allowed000001"
	wsNotAllowed := "ws-notallowed001"

	pool := &models.AgentPool{
		ID:                 poolID,
		OrganizationScoped: false,
		AllowedWorkspaces: []models.Workspace{
			{ID: wsAllowed, Name: "allowed-ws"},
		},
	}

	tests := []struct {
		name        string
		workspaceID string
		projectID   *uuid.UUID
		wantAllowed bool
	}{
		{
			name:        "workspace in allowed list",
			workspaceID: wsAllowed,
			wantAllowed: true,
		},
		{
			name:        "workspace not in allowed list",
			workspaceID: wsNotAllowed,
			wantAllowed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed, _ := CheckPoolAccess(pool, tt.workspaceID, tt.projectID)
			if allowed != tt.wantAllowed {
				t.Errorf("CheckPoolAccess() allowed = %v, want %v", allowed, tt.wantAllowed)
			}
		})
	}
}

func TestCheckPoolAccess_NonOrgScoped_AllowedProject(t *testing.T) {
	poolID := uuid.New()
	projAllowed := uuid.New()
	projNotAllowed := uuid.New()

	pool := &models.AgentPool{
		ID:                 poolID,
		OrganizationScoped: false,
		AllowedProjects: []models.Project{
			{ID: projAllowed, Name: "allowed-project"},
		},
	}

	tests := []struct {
		name        string
		workspaceID string
		projectID   *uuid.UUID
		wantAllowed bool
	}{
		{
			name:        "project in allowed list",
			workspaceID: "ws-anyworkspace01",
			projectID:   &projAllowed,
			wantAllowed: true,
		},
		{
			name:        "project not in allowed list",
			workspaceID: "ws-anyworkspace01",
			projectID:   &projNotAllowed,
			wantAllowed: false,
		},
		{
			name:        "nil project ID",
			workspaceID: "ws-anyworkspace01",
			projectID:   nil,
			wantAllowed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed, _ := CheckPoolAccess(pool, tt.workspaceID, tt.projectID)
			if allowed != tt.wantAllowed {
				t.Errorf("CheckPoolAccess() allowed = %v, want %v", allowed, tt.wantAllowed)
			}
		})
	}
}

func TestCheckPoolAccess_NonOrgScoped_BothAllowedWorkspaceAndProject(t *testing.T) {
	projAllowed := uuid.New()
	wsAllowed := "ws-allowed000001"

	pool := &models.AgentPool{
		ID:                 uuid.New(),
		OrganizationScoped: false,
		AllowedWorkspaces: []models.Workspace{
			{ID: wsAllowed, Name: "allowed-ws"},
		},
		AllowedProjects: []models.Project{
			{ID: projAllowed, Name: "allowed-project"},
		},
	}

	tests := []struct {
		name        string
		workspaceID string
		projectID   *uuid.UUID
		wantAllowed bool
	}{
		{
			name:        "workspace matches allowed list",
			workspaceID: wsAllowed,
			projectID:   nil,
			wantAllowed: true,
		},
		{
			name:        "project matches allowed list",
			workspaceID: "ws-other000000001",
			projectID:   &projAllowed,
			wantAllowed: true,
		},
		{
			name:        "neither matches",
			workspaceID: "ws-other000000001",
			projectID:   func() *uuid.UUID { id := uuid.New(); return &id }(),
			wantAllowed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed, _ := CheckPoolAccess(pool, tt.workspaceID, tt.projectID)
			if allowed != tt.wantAllowed {
				t.Errorf("CheckPoolAccess() allowed = %v, want %v", allowed, tt.wantAllowed)
			}
		})
	}
}

func TestCheckPoolAccess_NonOrgScoped_EmptyAllowLists(t *testing.T) {
	pool := &models.AgentPool{
		ID:                 uuid.New(),
		OrganizationScoped: false,
		AllowedWorkspaces:  []models.Workspace{},
		AllowedProjects:    []models.Project{},
	}

	projID := uuid.New()
	allowed, reason := CheckPoolAccess(pool, "ws-anything00001", &projID)
	if allowed {
		t.Error("CheckPoolAccess() should deny access when non-org-scoped pool has empty allow lists")
	}
	if reason != "Workspace is not allowed to use this agent pool" {
		t.Errorf("CheckPoolAccess() reason = %q, want denial message", reason)
	}
}

func TestCheckPoolAccess_OrgScoped_NoExclusions(t *testing.T) {
	pool := &models.AgentPool{
		ID:                 uuid.New(),
		OrganizationScoped: true,
		ExcludedWorkspaces: []models.Workspace{},
	}

	allowed, reason := CheckPoolAccess(pool, "ws-anything00001", nil)
	if !allowed {
		t.Errorf("CheckPoolAccess() should allow any workspace in org-scoped pool with no exclusions, got reason: %q", reason)
	}
}

func TestCheckPoolAccess_OrgScoped_MultipleExclusions(t *testing.T) {
	ws1 := "ws-excluded00001"
	ws2 := "ws-excluded00002"
	ws3 := "ws-excluded00003"

	pool := &models.AgentPool{
		ID:                 uuid.New(),
		OrganizationScoped: true,
		ExcludedWorkspaces: []models.Workspace{
			{ID: ws1, Name: "excluded-1"},
			{ID: ws2, Name: "excluded-2"},
			{ID: ws3, Name: "excluded-3"},
		},
	}

	// All excluded workspaces should be denied
	for _, wsID := range []string{ws1, ws2, ws3} {
		allowed, _ := CheckPoolAccess(pool, wsID, nil)
		if allowed {
			t.Errorf("CheckPoolAccess() should deny excluded workspace %s", wsID)
		}
	}

	// Non-excluded workspace should be allowed
	allowed, _ := CheckPoolAccess(pool, "ws-notexcluded001", nil)
	if !allowed {
		t.Error("CheckPoolAccess() should allow non-excluded workspace in org-scoped pool")
	}
}

func TestCheckPoolAccess_NonOrgScoped_RejectReason(t *testing.T) {
	pool := &models.AgentPool{
		ID:                 uuid.New(),
		OrganizationScoped: false,
		AllowedWorkspaces: []models.Workspace{
			{ID: "ws-allowed000001", Name: "allowed-ws"},
		},
	}

	_, reason := CheckPoolAccess(pool, "ws-notallowed001", nil)
	if reason != "Workspace is not allowed to use this agent pool" {
		t.Errorf("CheckPoolAccess() reason = %q, want specific denial message", reason)
	}
}
