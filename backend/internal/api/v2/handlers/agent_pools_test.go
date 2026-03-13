// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
)

func TestFormatAgentPoolResponse_Basic(t *testing.T) {
	poolID := uuid.New()
	orgName := "test-org"
	pool := &models.AgentPool{
		ID:                 poolID,
		OrganizationScoped: true,
		Name:               "production-pool",
		CreatedAt:          time.Date(2025, 6, 15, 10, 0, 0, 0, time.UTC),
	}

	resp := formatAgentPoolResponse(pool, orgName, 3)

	// Verify top-level fields
	if resp["id"] != poolID.String() {
		t.Errorf("id = %v, want %s", resp["id"], poolID.String())
	}
	if resp["type"] != "agent-pools" {
		t.Errorf("type = %v, want agent-pools", resp["type"])
	}

	// Verify attributes
	attrs, ok := resp["attributes"].(gin.H)
	if !ok {
		t.Fatal("attributes is not gin.H")
	}
	if attrs["name"] != "production-pool" {
		t.Errorf("attributes.name = %v, want production-pool", attrs["name"])
	}
	if attrs["agent-count"] != 3 {
		t.Errorf("attributes.agent-count = %v, want 3", attrs["agent-count"])
	}
	if attrs["organization-scoped"] != true {
		t.Errorf("attributes.organization-scoped = %v, want true", attrs["organization-scoped"])
	}

	// Verify relationships
	rels, ok := resp["relationships"].(gin.H)
	if !ok {
		t.Fatal("relationships is not gin.H")
	}
	orgRel, ok := rels["organization"].(gin.H)
	if !ok {
		t.Fatal("relationships.organization is not gin.H")
	}
	orgData, ok := orgRel["data"].(gin.H)
	if !ok {
		t.Fatal("relationships.organization.data is not gin.H")
	}
	if orgData["id"] != orgName {
		t.Errorf("organization.data.id = %v, want %s", orgData["id"], orgName)
	}
	if orgData["type"] != "organizations" {
		t.Errorf("organization.data.type = %v, want organizations", orgData["type"])
	}

	// Verify links
	links, ok := resp["links"].(gin.H)
	if !ok {
		t.Fatal("links is not gin.H")
	}
	expectedSelf := "/api/v2/agent-pools/" + poolID.String()
	if links["self"] != expectedSelf {
		t.Errorf("links.self = %v, want %s", links["self"], expectedSelf)
	}
}

func TestFormatAgentPoolResponse_WithAllowedWorkspaces(t *testing.T) {
	pool := &models.AgentPool{
		ID:                 uuid.New(),
		OrganizationScoped: false,
		Name:               "scoped-pool",
		CreatedAt:          time.Now(),
		AllowedWorkspaces: []models.Workspace{
			{ID: "ws-workspace00001", Name: "ws-1"},
			{ID: "ws-workspace00002", Name: "ws-2"},
		},
	}

	resp := formatAgentPoolResponse(pool, "org", 0)
	rels, ok := resp["relationships"].(gin.H)
	if !ok {
		t.Fatal("relationships is not gin.H")
	}

	awRel, ok := rels["allowed-workspaces"].(gin.H)
	if !ok {
		t.Fatal("relationships should include allowed-workspaces when workspaces are present")
	}
	data, ok := awRel["data"].([]gin.H)
	if !ok {
		t.Fatal("allowed-workspaces.data should be []gin.H")
	}
	if len(data) != 2 {
		t.Fatalf("allowed-workspaces.data has %d items, want 2", len(data))
	}
	if data[0]["id"] != "ws-workspace00001" {
		t.Errorf("allowed-workspaces.data[0].id = %v, want ws-workspace00001", data[0]["id"])
	}
	if data[0]["type"] != "workspaces" {
		t.Errorf("allowed-workspaces.data[0].type = %v, want workspaces", data[0]["type"])
	}
}

func TestFormatAgentPoolResponse_WithAllowedProjects(t *testing.T) {
	projID := uuid.New()
	pool := &models.AgentPool{
		ID:                 uuid.New(),
		OrganizationScoped: false,
		Name:               "project-pool",
		CreatedAt:          time.Now(),
		AllowedProjects: []models.Project{
			{ID: projID, Name: "project-1"},
		},
	}

	resp := formatAgentPoolResponse(pool, "org", 0)
	rels := resp["relationships"].(gin.H)
	apRel, ok := rels["allowed-projects"].(gin.H)
	if !ok {
		t.Fatal("relationships should include allowed-projects when projects are present")
	}
	data := apRel["data"].([]gin.H)
	if len(data) != 1 {
		t.Fatalf("allowed-projects.data has %d items, want 1", len(data))
	}
	if data[0]["id"] != projID.String() {
		t.Errorf("allowed-projects.data[0].id = %v, want %s", data[0]["id"], projID.String())
	}
	if data[0]["type"] != "projects" {
		t.Errorf("allowed-projects.data[0].type = %v, want projects", data[0]["type"])
	}
}

func TestFormatAgentPoolResponse_WithExcludedWorkspaces(t *testing.T) {
	pool := &models.AgentPool{
		ID:                 uuid.New(),
		OrganizationScoped: true,
		Name:               "org-pool",
		CreatedAt:          time.Now(),
		ExcludedWorkspaces: []models.Workspace{
			{ID: "ws-excluded00001", Name: "excluded"},
		},
	}

	resp := formatAgentPoolResponse(pool, "org", 0)
	rels := resp["relationships"].(gin.H)
	ewRel, ok := rels["excluded-workspaces"].(gin.H)
	if !ok {
		t.Fatal("relationships should include excluded-workspaces when exclusions are present")
	}
	data := ewRel["data"].([]gin.H)
	if len(data) != 1 {
		t.Fatalf("excluded-workspaces.data has %d items, want 1", len(data))
	}
	if data[0]["id"] != "ws-excluded00001" {
		t.Errorf("excluded-workspaces.data[0].id = %v, want ws-excluded00001", data[0]["id"])
	}
}

func TestFormatAgentPoolResponse_NoRelationships(t *testing.T) {
	pool := &models.AgentPool{
		ID:                 uuid.New(),
		OrganizationScoped: true,
		Name:               "empty-pool",
		CreatedAt:          time.Now(),
	}

	resp := formatAgentPoolResponse(pool, "org", 0)
	rels := resp["relationships"].(gin.H)

	// Should only have organization relationship, not allowed/excluded
	if _, exists := rels["allowed-workspaces"]; exists {
		t.Error("relationships should not include allowed-workspaces when empty")
	}
	if _, exists := rels["allowed-projects"]; exists {
		t.Error("relationships should not include allowed-projects when empty")
	}
	if _, exists := rels["excluded-workspaces"]; exists {
		t.Error("relationships should not include excluded-workspaces when empty")
	}
	if _, exists := rels["organization"]; !exists {
		t.Error("relationships should always include organization")
	}
}

func TestFormatAgentPoolResponse_ZeroAgentCount(t *testing.T) {
	pool := &models.AgentPool{
		ID:        uuid.New(),
		Name:      "new-pool",
		CreatedAt: time.Now(),
	}

	resp := formatAgentPoolResponse(pool, "org", 0)
	attrs := resp["attributes"].(gin.H)
	if attrs["agent-count"] != 0 {
		t.Errorf("attributes.agent-count = %v, want 0", attrs["agent-count"])
	}
}

func TestExtractWorkspaceIDs(t *testing.T) {
	tests := []struct {
		name     string
		refs     []jsonAPIRef
		expected []string
	}{
		{
			name:     "multiple refs",
			refs:     []jsonAPIRef{{ID: "ws-abc"}, {ID: "ws-def"}},
			expected: []string{"ws-abc", "ws-def"},
		},
		{
			name:     "empty refs",
			refs:     []jsonAPIRef{},
			expected: []string{},
		},
		{
			name:     "skip empty IDs",
			refs:     []jsonAPIRef{{ID: "ws-abc"}, {ID: ""}, {ID: "ws-def"}},
			expected: []string{"ws-abc", "ws-def"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractWorkspaceIDs(tt.refs)
			if len(got) != len(tt.expected) {
				t.Fatalf("extractWorkspaceIDs() returned %d items, want %d", len(got), len(tt.expected))
			}
			for i, id := range got {
				if id != tt.expected[i] {
					t.Errorf("extractWorkspaceIDs()[%d] = %s, want %s", i, id, tt.expected[i])
				}
			}
		})
	}
}

func TestExtractProjectIDs(t *testing.T) {
	id1 := uuid.New()
	id2 := uuid.New()

	tests := []struct {
		name     string
		refs     []jsonAPIRef
		expected []uuid.UUID
	}{
		{
			name:     "valid UUIDs",
			refs:     []jsonAPIRef{{ID: id1.String()}, {ID: id2.String()}},
			expected: []uuid.UUID{id1, id2},
		},
		{
			name:     "empty refs",
			refs:     []jsonAPIRef{},
			expected: []uuid.UUID{},
		},
		{
			name:     "skip invalid UUIDs",
			refs:     []jsonAPIRef{{ID: id1.String()}, {ID: "not-a-uuid"}, {ID: id2.String()}},
			expected: []uuid.UUID{id1, id2},
		},
		{
			name:     "skip empty IDs",
			refs:     []jsonAPIRef{{ID: id1.String()}, {ID: ""}},
			expected: []uuid.UUID{id1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractProjectIDs(tt.refs)
			if len(got) != len(tt.expected) {
				t.Fatalf("extractProjectIDs() returned %d items, want %d", len(got), len(tt.expected))
			}
			for i, id := range got {
				if id != tt.expected[i] {
					t.Errorf("extractProjectIDs()[%d] = %s, want %s", i, id, tt.expected[i])
				}
			}
		})
	}
}
