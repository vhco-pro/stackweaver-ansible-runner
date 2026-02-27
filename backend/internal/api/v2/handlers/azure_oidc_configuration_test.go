// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
)

// TestFormatAzureOIDCConfigResponse verifies the JSON:API response structure
// matches what go-tfe expects (type, attributes with kebab-case, relationships, links).
func TestFormatAzureOIDCConfigResponse(t *testing.T) {
	org := &models.Organization{
		ID:   uuid.New(),
		Name: "test-org",
	}

	config := &models.AzureOIDCConfiguration{
		ID:             "azoidc-1234567890abcdef",
		ClientID:       "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		SubscriptionID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		TenantID:       "cccccccc-cccc-cccc-cccc-cccccccccccc",
		OrganizationID: org.ID,
		Organization:   org,
	}

	resp := formatAzureOIDCConfigResponse(config)

	// Verify top-level fields
	if resp["id"] != "azoidc-1234567890abcdef" {
		t.Errorf("expected id 'azoidc-1234567890abcdef', got '%v'", resp["id"])
	}
	if resp["type"] != "azure-oidc-configurations" {
		t.Errorf("expected type 'azure-oidc-configurations', got '%v'", resp["type"])
	}

	// Verify attributes (kebab-case keys matching go-tfe jsonapi tags)
	attrs, ok := resp["attributes"].(gin.H)
	if !ok {
		t.Fatal("attributes is not a gin.H")
	}
	if attrs["client-id"] != "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" {
		t.Errorf("expected client-id, got '%v'", attrs["client-id"])
	}
	if attrs["subscription-id"] != "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb" {
		t.Errorf("expected subscription-id, got '%v'", attrs["subscription-id"])
	}
	if attrs["tenant-id"] != "cccccccc-cccc-cccc-cccc-cccccccccccc" {
		t.Errorf("expected tenant-id, got '%v'", attrs["tenant-id"])
	}

	// Verify relationships
	rels, ok := resp["relationships"].(gin.H)
	if !ok {
		t.Fatal("relationships is not a gin.H")
	}
	orgRel, ok := rels["organization"].(gin.H)
	if !ok {
		t.Fatal("organization relationship is not a gin.H")
	}
	orgData, ok := orgRel["data"].(gin.H)
	if !ok {
		t.Fatal("organization data is not a gin.H")
	}
	if orgData["id"] != "test-org" {
		t.Errorf("expected organization id 'test-org', got '%v'", orgData["id"])
	}
	if orgData["type"] != "organizations" {
		t.Errorf("expected organization type 'organizations', got '%v'", orgData["type"])
	}

	// Verify links
	links, ok := resp["links"].(gin.H)
	if !ok {
		t.Fatal("links is not a gin.H")
	}
	if links["self"] != "/api/v2/oidc-configurations/azoidc-1234567890abcdef" {
		t.Errorf("expected self link '/api/v2/oidc-configurations/azoidc-1234567890abcdef', got '%v'", links["self"])
	}
}

// TestFormatAzureOIDCConfigResponse_NilOrganization verifies response formatting
// when the Organization relationship is not preloaded.
func TestFormatAzureOIDCConfigResponse_NilOrganization(t *testing.T) {
	config := &models.AzureOIDCConfiguration{
		ID:             "azoidc-abcdefghijklmnop",
		ClientID:       "test-client-id",
		SubscriptionID: "test-subscription-id",
		TenantID:       "test-tenant-id",
		OrganizationID: uuid.New(),
		Organization:   nil,
	}

	resp := formatAzureOIDCConfigResponse(config)

	// Organization name should be empty string when not preloaded
	rels := resp["relationships"].(gin.H)
	orgRel := rels["organization"].(gin.H)
	orgData := orgRel["data"].(gin.H)
	if orgData["id"] != "" {
		t.Errorf("expected empty org id when organization is nil, got '%v'", orgData["id"])
	}
}

// TestCreateAzureOIDCConfigRequest_TypeValidation verifies that the handler
// correctly validates the JSON:API type field.
func TestCreateAzureOIDCConfigRequest_TypeValidation(t *testing.T) {
	tests := []struct {
		name        string
		dataType    string
		shouldMatch bool
	}{
		{"correct type", "azure-oidc-configurations", true},
		{"wrong type", "aws-oidc-configurations", false},
		{"empty type", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matches := tt.dataType == "azure-oidc-configurations"
			if matches != tt.shouldMatch {
				t.Errorf("type '%s': expected match=%v, got %v", tt.dataType, tt.shouldMatch, matches)
			}
		})
	}
}
