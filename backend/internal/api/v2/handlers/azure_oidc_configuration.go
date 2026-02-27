// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/rbac"
	"gorm.io/gorm"
)

// AzureOIDCConfigurationHandlerV2 handles TFE-compatible Azure OIDC configuration API.
// Reference: go-tfe/azure_oidc_configuration.go
type AzureOIDCConfigurationHandlerV2 struct {
	configRepo  *repository.AzureOIDCConfigurationRepository
	orgRepo     *repository.OrganizationRepository
	authService *auth.Service
	rbacService *rbac.Service
}

// NewAzureOIDCConfigurationHandlerV2 creates an AzureOIDCConfigurationHandlerV2.
func NewAzureOIDCConfigurationHandlerV2(
	configRepo *repository.AzureOIDCConfigurationRepository,
	orgRepo *repository.OrganizationRepository,
	authService *auth.Service,
	rbacService *rbac.Service,
) *AzureOIDCConfigurationHandlerV2 {
	return &AzureOIDCConfigurationHandlerV2{
		configRepo:  configRepo,
		orgRepo:     orgRepo,
		authService: authService,
		rbacService: rbacService,
	}
}

// CreateAzureOIDCConfigRequest is the JSON:API request for creating an Azure OIDC configuration.
type CreateAzureOIDCConfigRequest struct {
	Data struct {
		Type       string `json:"type" binding:"required"`
		Attributes struct {
			ClientID       string `json:"client-id"`
			SubscriptionID string `json:"subscription-id"`
			TenantID       string `json:"tenant-id"`
		} `json:"attributes" binding:"required"`
	} `json:"data" binding:"required"`
}

// UpdateAzureOIDCConfigRequest is the JSON:API request for updating an Azure OIDC configuration.
type UpdateAzureOIDCConfigRequest struct {
	Data struct {
		Type       string `json:"type" binding:"required"`
		Attributes struct {
			ClientID       *string `json:"client-id,omitempty"`
			SubscriptionID *string `json:"subscription-id,omitempty"`
			TenantID       *string `json:"tenant-id,omitempty"`
		} `json:"attributes"`
	} `json:"data" binding:"required"`
}

// formatAzureOIDCConfigResponse formats an Azure OIDC configuration as a JSON:API response.
func formatAzureOIDCConfigResponse(config *models.AzureOIDCConfiguration) gin.H {
	orgName := ""
	if config.Organization != nil {
		orgName = config.Organization.Name
	}

	return gin.H{
		"id":   config.ID,
		"type": "azure-oidc-configurations",
		"attributes": gin.H{
			"client-id":       config.ClientID,
			"subscription-id": config.SubscriptionID,
			"tenant-id":       config.TenantID,
		},
		"relationships": gin.H{
			"organization": gin.H{
				"data": gin.H{"id": orgName, "type": "organizations"},
			},
		},
		"links": gin.H{
			"self": "/api/v2/oidc-configurations/" + config.ID,
		},
	}
}

// List returns all Azure OIDC configurations for an organization.
// GET /api/v2/organizations/:name/oidc-configurations
func (h *AzureOIDCConfigurationHandlerV2) List(c *gin.Context) {
	orgName := c.Param("name")
	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}}})
		return
	}

	// RBAC: user must be in the organization
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}
	ok, err := h.rbacService.CheckOrgManageVCSSettings(c.Request.Context(), user.ID, org.ID)
	if err != nil || !ok {
		c.JSON(http.StatusForbidden, gin.H{"errors": []gin.H{{"status": "403", "title": "Forbidden", "detail": "You do not have permission to read OIDC configurations"}}})
		return
	}

	configs, err := h.configRepo.GetByOrganization(org.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to list OIDC configurations"}}})
		return
	}

	data := make([]gin.H, 0, len(configs))
	for i := range configs {
		data = append(data, formatAzureOIDCConfigResponse(&configs[i]))
	}

	c.JSON(http.StatusOK, gin.H{"data": data})
}

// Create creates a new Azure OIDC configuration.
// POST /api/v2/organizations/:name/oidc-configurations
func (h *AzureOIDCConfigurationHandlerV2) Create(c *gin.Context) {
	orgName := c.Param("name")
	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}}})
		return
	}

	// RBAC: user must be in the organization
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}
	ok, err := h.rbacService.CheckOrgManageVCSSettings(c.Request.Context(), user.ID, org.ID)
	if err != nil || !ok {
		c.JSON(http.StatusForbidden, gin.H{"errors": []gin.H{{"status": "403", "title": "Forbidden", "detail": "You do not have permission to manage OIDC configurations"}}})
		return
	}

	var req CreateAzureOIDCConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}}})
		return
	}

	if req.Data.Type != "azure-oidc-configurations" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "data.type must be 'azure-oidc-configurations'"}}})
		return
	}

	// Validate required fields
	if req.Data.Attributes.ClientID == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"errors": []gin.H{{"status": "422", "title": "Unprocessable Entity", "detail": "client-id is required"}}})
		return
	}
	if req.Data.Attributes.SubscriptionID == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"errors": []gin.H{{"status": "422", "title": "Unprocessable Entity", "detail": "subscription-id is required"}}})
		return
	}
	if req.Data.Attributes.TenantID == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"errors": []gin.H{{"status": "422", "title": "Unprocessable Entity", "detail": "tenant-id is required"}}})
		return
	}

	config := &models.AzureOIDCConfiguration{
		ClientID:       req.Data.Attributes.ClientID,
		SubscriptionID: req.Data.Attributes.SubscriptionID,
		TenantID:       req.Data.Attributes.TenantID,
		OrganizationID: org.ID,
	}

	if err := h.configRepo.Create(config); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to create Azure OIDC configuration"}}})
		return
	}

	// Reload with organization preloaded
	config, err = h.configRepo.GetByID(config.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to reload Azure OIDC configuration"}}})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": formatAzureOIDCConfigResponse(config)})
}

// Read returns an Azure OIDC configuration by ID.
// GET /api/v2/oidc-configurations/:id
func (h *AzureOIDCConfigurationHandlerV2) Read(c *gin.Context) {
	configID := c.Param("id")

	config, err := h.configRepo.GetByID(configID)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "OIDC configuration not found"}}})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to get OIDC configuration"}}})
		return
	}

	// RBAC: user must be in the organization
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}
	ok, err := h.rbacService.CheckOrgManageVCSSettings(c.Request.Context(), user.ID, config.OrganizationID)
	if err != nil || !ok {
		c.JSON(http.StatusForbidden, gin.H{"errors": []gin.H{{"status": "403", "title": "Forbidden", "detail": "You do not have permission to read OIDC configurations"}}})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": formatAzureOIDCConfigResponse(config)})
}

// Update updates an Azure OIDC configuration (partial update).
// PATCH /api/v2/oidc-configurations/:id
func (h *AzureOIDCConfigurationHandlerV2) Update(c *gin.Context) {
	configID := c.Param("id")

	config, err := h.configRepo.GetByID(configID)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "OIDC configuration not found"}}})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to get OIDC configuration"}}})
		return
	}

	// RBAC: user must be in the organization
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}
	ok, err := h.rbacService.CheckOrgManageVCSSettings(c.Request.Context(), user.ID, config.OrganizationID)
	if err != nil || !ok {
		c.JSON(http.StatusForbidden, gin.H{"errors": []gin.H{{"status": "403", "title": "Forbidden", "detail": "You do not have permission to manage OIDC configurations"}}})
		return
	}

	var req UpdateAzureOIDCConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}}})
		return
	}

	if req.Data.Type != "azure-oidc-configurations" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "data.type must be 'azure-oidc-configurations'"}}})
		return
	}

	// Build update map with only non-nil fields (partial update)
	updates := make(map[string]interface{})
	if req.Data.Attributes.ClientID != nil {
		updates["client_id"] = *req.Data.Attributes.ClientID
	}
	if req.Data.Attributes.SubscriptionID != nil {
		updates["subscription_id"] = *req.Data.Attributes.SubscriptionID
	}
	if req.Data.Attributes.TenantID != nil {
		updates["tenant_id"] = *req.Data.Attributes.TenantID
	}

	if len(updates) > 0 {
		config, err = h.configRepo.Update(configID, updates)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to update OIDC configuration"}}})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"data": formatAzureOIDCConfigResponse(config)})
}

// Delete deletes an Azure OIDC configuration.
// DELETE /api/v2/oidc-configurations/:id
func (h *AzureOIDCConfigurationHandlerV2) Delete(c *gin.Context) {
	configID := c.Param("id")

	config, err := h.configRepo.GetByID(configID)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "OIDC configuration not found"}}})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to get OIDC configuration"}}})
		return
	}

	// RBAC: user must be in the organization
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}
	ok, err := h.rbacService.CheckOrgManageVCSSettings(c.Request.Context(), user.ID, config.OrganizationID)
	if err != nil || !ok {
		c.JSON(http.StatusForbidden, gin.H{"errors": []gin.H{{"status": "403", "title": "Forbidden", "detail": "You do not have permission to manage OIDC configurations"}}})
		return
	}

	if err := h.configRepo.Delete(configID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to delete OIDC configuration"}}})
		return
	}

	c.Status(http.StatusNoContent)
}
