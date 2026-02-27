// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package terraform

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/rbac"
	"github.com/iac-platform/backend/internal/services/variable"
)

type VariableHandlerV2 struct {
	variableRepo    *repository.VariableRepository
	workspaceRepo   *repository.WorkspaceRepository
	orgRepo         *repository.OrganizationRepository
	projectRepo     *repository.ProjectRepository
	authService     *auth.Service
	rbacService     *rbac.Service
	variableService *variable.Service
}

func NewVariableHandlerV2(
	variableRepo *repository.VariableRepository,
	workspaceRepo *repository.WorkspaceRepository,
	authService *auth.Service,
	rbacService *rbac.Service,
	variableService *variable.Service,
) *VariableHandlerV2 {
	return &VariableHandlerV2{
		variableRepo:    variableRepo,
		workspaceRepo:   workspaceRepo,
		authService:     authService,
		rbacService:     rbacService,
		variableService: variableService,
	}
}

// SetRepositories allows setting org and project repos for building TFE-compatible links
func (h *VariableHandlerV2) SetRepositories(orgRepo *repository.OrganizationRepository, projectRepo *repository.ProjectRepository) {
	h.orgRepo = orgRepo
	h.projectRepo = projectRepo
}

// formatVariableResponse formats a variable in TFE-compatible JSON:API format
// Reference: https://developer.hashicorp.com/terraform/enterprise/api-docs/workspace-variables
func (h *VariableHandlerV2) formatVariableResponse(variable *models.Variable, workspaceID string) gin.H {
	// Get workspace to build proper links
	workspace, err := h.workspaceRepo.GetByID(workspaceID)
	var orgName, workspaceName string
	if err == nil {
		// Try to get organization and workspace names for links
		if h.projectRepo != nil && h.orgRepo != nil {
			project, _ := h.projectRepo.GetByID(workspace.ProjectID)
			if project != nil {
				org, _ := h.orgRepo.GetByID(project.OrganizationID)
				if org != nil {
					orgName = org.Name
					workspaceName = workspace.Name
				}
			}
		}
	}

	// Build configurable relationship link
	var configurableLink string
	if orgName != "" && workspaceName != "" {
		configurableLink = fmt.Sprintf("/api/v2/organizations/%s/workspaces/%s", orgName, workspaceName)
	} else {
		configurableLink = fmt.Sprintf("/api/v2/workspaces/%s", workspaceID)
	}

	// TFE-compatible response format
	// Note: TFE uses "configurable" relationship, not "workspace"
	// Also uses type "vars" not "variables"
	// TFE spec: Sensitive variable values must be masked in API responses
	value := variable.Value
	if variable.Sensitive {
		value = "••••••••"
	}

	return gin.H{
		"id":   variable.ID,
		"type": "vars", // TFE uses "vars" not "variables"
		"attributes": gin.H{
			"key":         variable.Key,
			"value":       value, // Masked if sensitive
			"description": variable.Description,
			"sensitive":   variable.Sensitive,
			"category":    variable.Category,
			"hcl":         variable.HCL,
			"version-id":  "", // TFE includes this, we can leave empty for now
		},
		"relationships": gin.H{
			"configurable": gin.H{ // TFE uses "configurable" not "workspace"
				"data": gin.H{
					"id":   workspaceID,
					"type": "workspaces",
				},
				"links": gin.H{
					"related": configurableLink,
				},
			},
		},
		"links": gin.H{
			"self": fmt.Sprintf("/api/v2/workspaces/%s/vars/%s", workspaceID, variable.ID),
		},
	}
}

// CreateVariableRequestV2 uses JSON:API format (TFE-compatible)
// Reference: https://developer.hashicorp.com/terraform/enterprise/api-docs/workspace-variables#create-a-variable
type CreateVariableRequestV2 struct {
	Data struct {
		Type       string `json:"type"` // Must be "vars"
		Attributes struct {
			Key         string `json:"key" binding:"required"`
			Value       string `json:"value" binding:"required"`
			Description string `json:"description,omitempty"`
			Category    string `json:"category,omitempty"`  // "terraform" or "env", defaults to "terraform"
			HCL         bool   `json:"hcl,omitempty"`       // Defaults to false
			Sensitive   bool   `json:"sensitive,omitempty"` // Defaults to false
		} `json:"attributes"`
	} `json:"data"`
}

// UpdateVariableRequestV2 uses JSON:API format (TFE-compatible)
// Reference: https://developer.hashicorp.com/terraform/enterprise/api-docs/workspace-variables#update-variables
type UpdateVariableRequestV2 struct {
	Data struct {
		ID         string `json:"id"`   // Variable ID
		Type       string `json:"type"` // Must be "vars"
		Attributes struct {
			Key         string `json:"key,omitempty"`
			Value       string `json:"value,omitempty"`
			Description string `json:"description,omitempty"`
			Category    string `json:"category,omitempty"`
			HCL         *bool  `json:"hcl,omitempty"`
			Sensitive   *bool  `json:"sensitive,omitempty"`
		} `json:"attributes"`
	} `json:"data"`
}

// ListByWorkspace lists variables for a workspace (TFE-compatible)
// GET /api/v2/workspaces/:id/variables
func (h *VariableHandlerV2) ListByWorkspace(c *gin.Context) {
	workspaceID := c.Param("id")
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid workspace ID",
				},
			},
		})
		return
	}

	// Verify workspace exists and get project ID
	workspace, err := h.workspaceRepo.GetByID(workspaceID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Workspace not found",
				},
			},
		})
		return
	}

	// Check permission: variables read
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{
					"status": "401",
					"title":  "Unauthorized",
					"detail": "Authentication required",
				},
			},
		})
		return
	}

	hasPermission, err := h.rbacService.CheckVariablePermission(c.Request.Context(), user.ID, workspaceID, workspace.ProjectID, "read")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to check permissions",
				},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Insufficient permissions to view variables",
				},
			},
		})
		return
	}

	variables, err := h.variableRepo.ListByWorkspace(workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to list variables",
				},
			},
		})
		return
	}

	// Format variables in TFE-compatible JSON:API format
	variablesData := make([]gin.H, len(variables))
	for i := range variables {
		variablesData[i] = h.formatVariableResponse(&variables[i], workspaceID)
	}

	// TFE-compatible response format
	c.JSON(http.StatusOK, gin.H{
		"data": variablesData,
	})
}

// Get returns a single workspace variable by ID (TFE-compatible).
// GET /api/v2/workspaces/:id/vars/:variable_id
// Provider uses this for Read/refresh; missing endpoint caused 404 → "resource gone" → drift.
func (h *VariableHandlerV2) Get(c *gin.Context) {
	workspaceID := c.Param("id")
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid workspace ID"},
			},
		})
		return
	}
	variableID := c.Param("variable_id")
	if variableID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid variable ID"},
			},
		})
		return
	}

	workspace, err := h.workspaceRepo.GetByID(workspaceID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "Workspace not found"},
			},
		})
		return
	}

	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{"status": "401", "title": "Unauthorized", "detail": "Authentication required"},
			},
		})
		return
	}
	hasPermission, err := h.rbacService.CheckVariablePermission(c.Request.Context(), user.ID, workspaceID, workspace.ProjectID, "read")
	if err != nil || !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "Insufficient permissions to view variables"},
			},
		})
		return
	}

	variable, err := h.variableRepo.GetByID(variableID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "Variable not found"},
			},
		})
		return
	}
	if variable.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "Variable not found"},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": h.formatVariableResponse(variable, workspaceID),
	})
}

// Create creates a new variable for a workspace (TFE-compatible)
// POST /api/v2/workspaces/:id/variables
func (h *VariableHandlerV2) Create(c *gin.Context) {
	workspaceID := c.Param("id")
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid workspace ID",
				},
			},
		})
		return
	}

	// Verify workspace exists and get project ID
	workspace, err := h.workspaceRepo.GetByID(workspaceID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Workspace not found",
				},
			},
		})
		return
	}

	// Check permission: variables write
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{
					"status": "401",
					"title":  "Unauthorized",
					"detail": "Authentication required",
				},
			},
		})
		return
	}

	hasPermission, err := h.rbacService.CheckVariablePermission(c.Request.Context(), user.ID, workspaceID, workspace.ProjectID, "write")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to check permissions",
				},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Insufficient permissions to create variables",
				},
			},
		})
		return
	}

	var req CreateVariableRequestV2
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": err.Error(),
				},
			},
		})
		return
	}

	// Validate JSON:API format
	if req.Data.Type != "vars" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "data.type must be 'vars'",
				},
			},
		})
		return
	}

	attrs := req.Data.Attributes

	// Set defaults
	category := attrs.Category
	if category == "" {
		category = "terraform" // TFE default
	}
	if category != "terraform" && category != "env" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "category must be 'terraform' or 'env'",
				},
			},
		})
		return
	}

	// Check if variable with same key already exists
	existing, _ := h.variableRepo.GetByWorkspaceAndKey(workspaceID, attrs.Key)
	if existing != nil {
		c.JSON(http.StatusConflict, gin.H{
			"errors": []gin.H{
				{
					"status": "409",
					"title":  "Conflict",
					"detail": "Variable with this key already exists in this workspace",
				},
			},
		})
		return
	}

	// Encrypt sensitive values
	var finalValue string
	var encrypted bool
	if attrs.Sensitive && h.variableService != nil {
		encryptedValue, err := h.variableService.Encrypt(attrs.Value)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{
					{
						"status": "500",
						"title":  "Internal Server Error",
						"detail": fmt.Sprintf("Failed to encrypt variable: %v", err),
					},
				},
			})
			return
		}
		finalValue = encryptedValue
		encrypted = true
	} else {
		finalValue = attrs.Value
		encrypted = false
	}

	variable := &models.Variable{
		WorkspaceID: workspaceID,
		Key:         attrs.Key,
		Value:       finalValue,
		Description: attrs.Description,
		Category:    category,
		HCL:         attrs.HCL,
		Encrypted:   encrypted,
		Sensitive:   attrs.Sensitive,
	}

	if err := h.variableRepo.Create(variable); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to create variable",
				},
			},
		})
		return
	}

	// TFE-compatible response format
	c.JSON(http.StatusCreated, gin.H{
		"data": h.formatVariableResponse(variable, workspaceID),
	})
}

// Update updates a variable by ID (TFE-compatible)
// PATCH /api/v2/workspaces/:id/variables/:variable_id
func (h *VariableHandlerV2) Update(c *gin.Context) {
	workspaceID := c.Param("id")
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid workspace ID",
				},
			},
		})
		return
	}

	variableID := c.Param("variable_id")
	if variableID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid variable ID",
				},
			},
		})
		return
	}

	// Get workspace for permission check
	workspace, err := h.workspaceRepo.GetByID(workspaceID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Workspace not found",
				},
			},
		})
		return
	}

	// Check permission: variables write
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{
					"status": "401",
					"title":  "Unauthorized",
					"detail": "Authentication required",
				},
			},
		})
		return
	}

	hasPermission, err := h.rbacService.CheckVariablePermission(c.Request.Context(), user.ID, workspaceID, workspace.ProjectID, "write")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to check permissions",
				},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Insufficient permissions to update variables",
				},
			},
		})
		return
	}

	variable, err := h.variableRepo.GetByID(variableID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Variable not found",
				},
			},
		})
		return
	}

	// Verify variable belongs to workspace
	if variable.WorkspaceID != workspaceID {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Variable does not belong to this workspace",
				},
			},
		})
		return
	}

	var req UpdateVariableRequestV2
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": err.Error(),
				},
			},
		})
		return
	}

	// Validate JSON:API format
	if req.Data.Type != "vars" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "data.type must be 'vars'",
				},
			},
		})
		return
	}

	// Validate ID matches
	if req.Data.ID != "" && req.Data.ID != variableID {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "data.id must match the variable ID in the URL",
				},
			},
		})
		return
	}

	attrs := req.Data.Attributes

	// Determine if variable should be sensitive after update
	willBeSensitive := variable.Sensitive
	if attrs.Sensitive != nil {
		willBeSensitive = *attrs.Sensitive
	}

	// Update fields if provided
	if attrs.Key != "" {
		// Check if new key conflicts with existing variable
		if attrs.Key != variable.Key {
			existing, _ := h.variableRepo.GetByWorkspaceAndKey(workspaceID, attrs.Key)
			if existing != nil {
				c.JSON(http.StatusConflict, gin.H{
					"errors": []gin.H{
						{
							"status": "409",
							"title":  "Conflict",
							"detail": "Variable with this key already exists in this workspace",
						},
					},
				})
				return
			}
		}
		variable.Key = attrs.Key
	}
	if attrs.Value != "" {
		// Encrypt value if sensitive
		if willBeSensitive && h.variableService != nil {
			encryptedValue, err := h.variableService.Encrypt(attrs.Value)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{
					"errors": []gin.H{
						{
							"status": "500",
							"title":  "Internal Server Error",
							"detail": fmt.Sprintf("Failed to encrypt variable: %v", err),
						},
					},
				})
				return
			}
			variable.Value = encryptedValue
			variable.Encrypted = true
		} else {
			variable.Value = attrs.Value
			// If changing from sensitive to non-sensitive, clear encryption
			if !willBeSensitive {
				variable.Encrypted = false
			}
		}
	}
	if attrs.Description != "" {
		variable.Description = attrs.Description
	}
	if attrs.Category != "" {
		if attrs.Category != "terraform" && attrs.Category != "env" {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "category must be 'terraform' or 'env'",
					},
				},
			})
			return
		}
		variable.Category = attrs.Category
	}
	if attrs.HCL != nil {
		variable.HCL = *attrs.HCL
	}
	if attrs.Sensitive != nil {
		variable.Sensitive = *attrs.Sensitive
		// If changing from sensitive to non-sensitive and value wasn't updated, we need to decrypt
		// But since we don't have the plaintext, we'll leave it encrypted in the DB
		// The value will be decrypted when read via the service
	}

	if err := h.variableRepo.Update(variable); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to update variable",
				},
			},
		})
		return
	}

	// TFE-compatible response format
	c.JSON(http.StatusOK, gin.H{
		"data": h.formatVariableResponse(variable, workspaceID),
	})
}

// Delete deletes a variable by ID (TFE-compatible)
// DELETE /api/v2/workspaces/:id/variables/:variable_id
func (h *VariableHandlerV2) Delete(c *gin.Context) {
	workspaceID := c.Param("id")
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid workspace ID",
				},
			},
		})
		return
	}

	variableID := c.Param("variable_id")
	if variableID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid variable ID",
				},
			},
		})
		return
	}

	// Get workspace for permission check
	workspace, err := h.workspaceRepo.GetByID(workspaceID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Workspace not found",
				},
			},
		})
		return
	}

	// Check permission: variables write
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{
					"status": "401",
					"title":  "Unauthorized",
					"detail": "Authentication required",
				},
			},
		})
		return
	}

	hasPermission, err := h.rbacService.CheckVariablePermission(c.Request.Context(), user.ID, workspaceID, workspace.ProjectID, "write")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to check permissions",
				},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Insufficient permissions to delete variables",
				},
			},
		})
		return
	}

	variable, err := h.variableRepo.GetByID(variableID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Variable not found",
				},
			},
		})
		return
	}

	// Verify variable belongs to workspace
	if variable.WorkspaceID != workspaceID {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Variable does not belong to this workspace",
				},
			},
		})
		return
	}

	if err := h.variableRepo.Delete(variableID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to delete variable",
				},
			},
		})
		return
	}

	c.Status(http.StatusNoContent)
}

// GetPlatformVariableKeys returns the list of platform variable keys for a workspace.
// GET /api/v2/workspaces/:id/platform-variables
// Used by frontend to show warnings when users create variables that would override platform variables.
func (h *VariableHandlerV2) GetPlatformVariableKeys(c *gin.Context) {
	workspaceID := c.Param("id")
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid workspace ID",
				},
			},
		})
		return
	}

	// Verify workspace exists
	workspace, err := h.workspaceRepo.GetByID(workspaceID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Workspace not found",
				},
			},
		})
		return
	}

	// Check permission: variables read (same permission as listing variables)
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{
					"status": "401",
					"title":  "Unauthorized",
					"detail": "Authentication required",
				},
			},
		})
		return
	}

	hasPermission, err := h.rbacService.CheckVariablePermission(c.Request.Context(), user.ID, workspaceID, workspace.ProjectID, "read")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to check permissions",
				},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Insufficient permissions to view platform variables",
				},
			},
		})
		return
	}

	// Get platform variable keys
	keys, err := h.variableService.GetPlatformVariableKeys(c.Request.Context(), workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to get platform variable keys",
				},
			},
		})
		return
	}

	// Return simple JSON array of keys
	c.JSON(http.StatusOK, gin.H{
		"data": keys,
	})
}
