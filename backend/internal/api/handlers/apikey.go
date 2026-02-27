// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/api/helpers"
	"github.com/iac-platform/backend/internal/services/activity"
	"github.com/iac-platform/backend/internal/services/apikey"
	"github.com/iac-platform/backend/internal/services/auth"
)

type APIKeyHandler struct {
	apiKeyService   *apikey.Service
	authService     *auth.Service
	activityService *activity.Service
}

func NewAPIKeyHandler(apiKeyService *apikey.Service, authService *auth.Service, activityService *activity.Service) *APIKeyHandler {
	return &APIKeyHandler{
		apiKeyService:   apiKeyService,
		authService:     authService,
		activityService: activityService,
	}
}

// CreateAPIKeyRequest represents the request to create an API key
type CreateAPIKeyRequest struct {
	Name      string   `json:"name" binding:"required"`
	Scopes    []string `json:"scopes,omitempty"`     // Optional scopes array
	ExpiresAt *string  `json:"expires_at,omitempty"` // ISO 8601 date string
}

// CreateAPIKeyResponse represents the response when creating an API key
type CreateAPIKeyResponse struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Key            string    `json:"key"` // Only shown once during creation
	KeyPrefix      string    `json:"key_prefix"`
	Scopes         []string  `json:"scopes"`
	OrganizationID *string   `json:"organization_id,omitempty"`
	ProjectID      *string   `json:"project_id,omitempty"`
	ExpiresAt      *string   `json:"expires_at,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// APIKeyResponse represents an API key in responses (without the actual key)
type APIKeyResponse struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	KeyPrefix      string     `json:"key_prefix"`
	Scopes         []string   `json:"scopes"`
	OrganizationID *string    `json:"organization_id,omitempty"`
	ProjectID      *string    `json:"project_id,omitempty"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	LastUsedAt     *time.Time `json:"last_used_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// CreateAPIKey creates a new API key
// POST /api/v2/settings/api-keys
func (h *APIKeyHandler) CreateAPIKey(c *gin.Context) {
	var req CreateAPIKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Get user from context
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	// Parse expiration date if provided
	var expiresAt *time.Time
	if req.ExpiresAt != nil && *req.ExpiresAt != "" {
		parsed, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid expires_at format, use ISO 8601 (RFC3339)"})
			return
		}
		expiresAt = &parsed
	}

	// Create the API key with scopes
	apiKey, plainKey, err := h.apiKeyService.CreateAPIKey(user.ID, req.Name, req.Scopes, expiresAt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create API key", "details": err.Error()})
		return
	}

	// Log activity (non-blocking)
	if h.activityService != nil {
		activityCtx := helpers.GetActivityContext(c)
		if apiKey.OrganizationID != nil {
			activityCtx.OrganizationID = apiKey.OrganizationID
		}
		if apiKey.ProjectID != nil {
			activityCtx.ProjectID = apiKey.ProjectID
		}
		_ = h.activityService.LogAPIKeyCreate(c.Request.Context(), apiKey.ID, apiKey.Name, activityCtx)
	}

	// Format response
	expiresAtStr := ""
	if apiKey.ExpiresAt != nil {
		expiresAtStr = apiKey.ExpiresAt.Format(time.RFC3339)
	}

	var orgIDStr *string
	if apiKey.OrganizationID != nil {
		s := apiKey.OrganizationID.String()
		orgIDStr = &s
	}

	var projectIDStr *string
	if apiKey.ProjectID != nil {
		s := apiKey.ProjectID.String()
		projectIDStr = &s
	}

	c.JSON(http.StatusCreated, CreateAPIKeyResponse{
		ID:             apiKey.ID.String(),
		Name:           apiKey.Name,
		Key:            plainKey, // Only time the full key is shown
		KeyPrefix:      apiKey.KeyPrefix,
		Scopes:         []string(apiKey.Scopes),
		OrganizationID: orgIDStr,
		ProjectID:      projectIDStr,
		ExpiresAt:      &expiresAtStr,
		CreatedAt:      apiKey.CreatedAt,
	})
}

// ListAPIKeys lists all API keys for the current user
// GET /api/v2/settings/api-keys
func (h *APIKeyHandler) ListAPIKeys(c *gin.Context) {
	// Get user from context
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	// List API keys
	apiKeys, err := h.apiKeyService.ListAPIKeys(user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list API keys", "details": err.Error()})
		return
	}

	// Convert to response format
	responses := make([]APIKeyResponse, len(apiKeys))
	for i, key := range apiKeys {
		var orgIDStr *string
		if key.OrganizationID != nil {
			s := key.OrganizationID.String()
			orgIDStr = &s
		}

		var projectIDStr *string
		if key.ProjectID != nil {
			s := key.ProjectID.String()
			projectIDStr = &s
		}

		responses[i] = APIKeyResponse{
			ID:             key.ID.String(),
			Name:           key.Name,
			KeyPrefix:      key.KeyPrefix,
			Scopes:         []string(key.Scopes),
			OrganizationID: orgIDStr,
			ProjectID:      projectIDStr,
			ExpiresAt:      key.ExpiresAt,
			LastUsedAt:     key.LastUsedAt,
			CreatedAt:      key.CreatedAt,
		}
	}

	c.JSON(http.StatusOK, gin.H{"api_keys": responses})
}

// DeleteAPIKey deletes an API key
// DELETE /api/v2/settings/api-keys/:id
func (h *APIKeyHandler) DeleteAPIKey(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid API key ID"})
		return
	}

	// Get user from context
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	// Get API key before deletion for activity logging
	apiKey, _ := h.apiKeyService.GetAPIKey(id, user.ID)

	// Delete the API key
	if err := h.apiKeyService.DeleteAPIKey(id, user.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete API key", "details": err.Error()})
		return
	}

	// Log activity (non-blocking)
	if h.activityService != nil && apiKey != nil {
		activityCtx := helpers.GetActivityContext(c)
		if apiKey.OrganizationID != nil {
			activityCtx.OrganizationID = apiKey.OrganizationID
		}
		if apiKey.ProjectID != nil {
			activityCtx.ProjectID = apiKey.ProjectID
		}
		_ = h.activityService.LogAPIKeyDelete(c.Request.Context(), apiKey.ID, apiKey.Name, activityCtx)
	}

	c.JSON(http.StatusOK, gin.H{"message": "API key deleted successfully"})
}
