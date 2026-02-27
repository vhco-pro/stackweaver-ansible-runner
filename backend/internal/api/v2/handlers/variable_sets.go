// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/michielvha/logger"
	"gorm.io/gorm"
)

type VariableSetHandlerV2 struct {
	variableSetRepo         *repository.VariableSetRepository
	variableSetVariableRepo *repository.VariableSetVariableRepository
	orgRepo                 *repository.OrganizationRepository
	projectRepo             *repository.ProjectRepository
	workspaceRepo           *repository.WorkspaceRepository
	jobTemplateRepo         *repository.AnsibleJobTemplateRepository
	authService             *auth.Service
}

func NewVariableSetHandlerV2(
	variableSetRepo *repository.VariableSetRepository,
	variableSetVariableRepo *repository.VariableSetVariableRepository,
	orgRepo *repository.OrganizationRepository,
	projectRepo *repository.ProjectRepository,
	workspaceRepo *repository.WorkspaceRepository,
	jobTemplateRepo *repository.AnsibleJobTemplateRepository,
	authService *auth.Service,
) *VariableSetHandlerV2 {
	return &VariableSetHandlerV2{
		variableSetRepo:         variableSetRepo,
		variableSetVariableRepo: variableSetVariableRepo,
		orgRepo:                 orgRepo,
		projectRepo:             projectRepo,
		workspaceRepo:           workspaceRepo,
		jobTemplateRepo:         jobTemplateRepo,
		authService:             authService,
	}
}

// CreateVariableSetRequestV2 uses JSON:API format (TFE-compatible)
// Reference: https://developer.hashicorp.com/terraform/enterprise/api-docs/variable-sets#create-a-variable-set
type CreateVariableSetRequestV2 struct {
	Data struct {
		Type       string `json:"type" binding:"required"` // Must be "varsets"
		Attributes struct {
			Name        string `json:"name" binding:"required"`
			Description string `json:"description,omitempty"`
			Global      bool   `json:"global,omitempty"`   // TFE: when true, applies to all workspaces
			Priority    bool   `json:"priority,omitempty"` // TFE: when true, overrides other variables
		} `json:"attributes" binding:"required"`
		Relationships struct {
			Workspaces struct {
				Data []gin.H `json:"data,omitempty"`
			} `json:"workspaces,omitempty"`
			Projects struct {
				Data []gin.H `json:"data,omitempty"`
			} `json:"projects,omitempty"`
			Vars struct {
				Data []gin.H `json:"data,omitempty"`
			} `json:"vars,omitempty"`
			Parent struct {
				Data gin.H `json:"data,omitempty"`
			} `json:"parent,omitempty"`
		} `json:"relationships,omitempty"`
	} `json:"data" binding:"required"`
}

// UpdateVariableSetRequestV2 uses JSON:API format (TFE-compatible)
// Reference: https://developer.hashicorp.com/terraform/enterprise/api-docs/variable-sets#update-a-variable-set
type UpdateVariableSetRequestV2 struct {
	Data struct {
		Type       string `json:"type" binding:"required"` // Must be "varsets"
		Attributes struct {
			Name        *string `json:"name,omitempty"`
			Description *string `json:"description,omitempty"`
			Global      *bool   `json:"global,omitempty"`   // TFE: when true, applies to all workspaces
			Priority    *bool   `json:"priority,omitempty"` // TFE: when true, overrides other variables
		} `json:"attributes"`
		Relationships struct {
			Workspaces struct {
				Data []gin.H `json:"data,omitempty"`
			} `json:"workspaces,omitempty"`
			Projects struct {
				Data []gin.H `json:"data,omitempty"`
			} `json:"projects,omitempty"`
			Vars struct {
				Data []gin.H `json:"data,omitempty"`
			} `json:"vars,omitempty"`
			Parent struct {
				Data gin.H `json:"data,omitempty"`
			} `json:"parent,omitempty"`
		} `json:"relationships,omitempty"`
	} `json:"data" binding:"required"`
}

// ListVariableSets handles GET /api/v2/organizations/:name/variable-sets
func (h *VariableSetHandlerV2) ListVariableSets(c *gin.Context) {
	orgName := c.Param("name")

	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}
	_ = user

	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}}})
		return
	}

	// TODO: Check user has permission to view variable sets in this organization
	_ = user

	variableSets, err := h.variableSetRepo.ListByOrganization(org.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to list variable sets"}}})
		return
	}

	data := make([]gin.H, len(variableSets))
	for i, vs := range variableSets {
		// Include full variable details in relationships (they're already preloaded)
		variablesData := make([]gin.H, len(vs.Variables))
		for j, v := range vs.Variables {
			value := v.Value
			if v.Sensitive {
				value = "••••••••"
			}
			variablesData[j] = gin.H{
				"id":   v.ID,
				"type": "vars", // TFE uses "vars" not "variable-set-variables"
				"attributes": gin.H{
					"key":         v.Key,
					"value":       value,
					"description": v.Description,
					"sensitive":   v.Sensitive,
					"category":    v.Category,
					"hcl":         v.HCL,
				},
			}
		}

		// Build parent relationship - project-owned or organization-owned
		parentData := gin.H{
			"id":   org.Name,
			"type": "organizations",
		}
		if vs.ProjectID != nil {
			// Find project to get its ID
			for _, p := range vs.Projects {
				if p.ID == *vs.ProjectID {
					parentData = gin.H{
						"id":   p.ID.String(),
						"type": "projects",
					}
					break
				}
			}
			// If not found in preloaded projects, fetch it
			if parentData["type"] == "organizations" {
				project, err := h.projectRepo.GetByID(*vs.ProjectID)
				if err == nil && project != nil {
					parentData = gin.H{
						"id":   project.ID.String(),
						"type": "projects",
					}
				}
			}
		}

		relationships := gin.H{
			"organization": gin.H{
				"data": gin.H{
					"id":   org.Name,
					"type": "organizations",
				},
			},
			"parent": gin.H{
				"data": parentData,
			},
			"vars": gin.H{
				"data": variablesData,
			},
		}

		// Include projects if organization-scoped and has projects assigned
		if vs.Scope == "organization" && len(vs.Projects) > 0 {
			projectsData := make([]gin.H, len(vs.Projects))
			for j, p := range vs.Projects {
				projectsData[j] = gin.H{
					"id":   p.ID.String(),
					"type": "projects",
				}
			}
			relationships["projects"] = gin.H{
				"data": projectsData,
			}
		}

		// Include workspaces if workspace-scoped and has workspaces assigned
		if vs.Scope == "workspace" && len(vs.Workspaces) > 0 {
			workspacesData := make([]gin.H, len(vs.Workspaces))
			for j, w := range vs.Workspaces {
				workspacesData[j] = gin.H{
					"id":   w.ID,
					"type": "workspaces",
				}
			}
			relationships["workspaces"] = gin.H{
				"data": workspacesData,
			}
		}

		// TFE uses "global" and "priority" instead of "scope"
		global := vs.Scope == "organization"

		attributes := gin.H{
			"name":            vs.Name,
			"description":     vs.Description,
			"global":          global,      // TFE-compatible
			"priority":        vs.Priority, // TFE-compatible
			"updated-at":      vs.UpdatedAt.Format("2006-01-02T15:04:05Z"),
			"var-count":       len(vs.Variables),
			"workspace-count": len(vs.Workspaces),
			"project-count":   len(vs.Projects),
		}

		data[i] = gin.H{
			"id":            vs.ID,
			"type":          "varsets", // TFE uses "varsets" not "variable-sets"
			"attributes":    attributes,
			"relationships": relationships,
		}
	}

	c.JSON(http.StatusOK, gin.H{"data": data})
}

// GetVariableSet handles GET /api/v2/varsets/:id or GET /api/v2/organizations/:name/varsets/:id
// TFE spec: GET /api/v2/varsets/:varset_id
// Reference: https://developer.hashicorp.com/terraform/enterprise/api-docs/variable-sets#show-variable-set
func (h *VariableSetHandlerV2) GetVariableSet(c *gin.Context) {
	variableSetID := c.Param("id")
	orgName := c.Param("name") // May be empty if called via /varsets/:id

	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}
	_ = user

	if variableSetID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid variable set ID"}}})
		return
	}

	variableSet, err := h.variableSetRepo.GetByID(variableSetID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable set not found"}}})
		return
	}

	// If orgName is provided, verify variable set belongs to organization
	var org *models.Organization
	if orgName != "" {
		org, err = h.orgRepo.GetByName(orgName)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}}})
			return
		}
		if variableSet.OrganizationID != org.ID {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable set not found"}}})
			return
		}
	}

	// Get variables for this set
	variables, err := h.variableSetVariableRepo.ListByVariableSet(variableSetID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to load variables"}}})
		return
	}

	variablesData := make([]gin.H, len(variables))
	for i, v := range variables {
		value := v.Value
		if v.Sensitive {
			value = "••••••••"
		}
		variablesData[i] = gin.H{
			"id":   v.ID,
			"type": "vars", // TFE uses "vars" not "variable-set-variables"
			"attributes": gin.H{
				"key":         v.Key,
				"value":       value,
				"description": v.Description,
				"sensitive":   v.Sensitive,
				"category":    v.Category,
				"hcl":         v.HCL,
			},
		}
	}

	// Get projects for this set (if organization-scoped)
	// TFE spec: projects relationship is just id/type, not full attributes
	projectsData := make([]gin.H, 0)
	if variableSet.Scope == "organization" && len(variableSet.Projects) > 0 {
		for _, p := range variableSet.Projects {
			projectsData = append(projectsData, gin.H{
				"id":   p.ID.String(),
				"type": "projects",
			})
		}
	}

	// Get workspaces for this set (if workspace-scoped)
	// TFE spec: workspaces relationship is just id/type, not full attributes
	workspacesData := make([]gin.H, 0)
	if variableSet.Scope == "workspace" && len(variableSet.Workspaces) > 0 {
		for _, w := range variableSet.Workspaces {
			workspacesData = append(workspacesData, gin.H{
				"id":   w.ID,
				"type": "workspaces",
			})
		}
	}

	// Get organization for relationships if not already retrieved
	if org == nil {
		org, _ = h.orgRepo.GetByID(variableSet.OrganizationID)
	}

	relationships := gin.H{
		"organization": gin.H{
			"data": gin.H{
				"id":   org.Name,
				"type": "organizations",
			},
		},
		"parent": gin.H{
			"data": func() gin.H {
				// If project-owned, return project; otherwise organization
				if variableSet.ProjectID != nil {
					project, _ := h.projectRepo.GetByID(*variableSet.ProjectID)
					if project != nil {
						return gin.H{
							"id":   project.ID.String(),
							"type": "projects",
						}
					}
				}
				return gin.H{
					"id":   org.Name,
					"type": "organizations",
				}
			}(),
		},
		"vars": gin.H{
			"data": variablesData,
		},
	}

	// Include projects relationship if there are projects assigned
	if len(projectsData) > 0 {
		relationships["projects"] = gin.H{
			"data": projectsData,
		}
	}

	// Include workspaces relationship if there are workspaces assigned
	if len(workspacesData) > 0 {
		relationships["workspaces"] = gin.H{
			"data": workspacesData,
		}
	}

	// TFE uses "global" and "priority" instead of "scope"
	global := variableSet.Scope == "organization"
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"id":   variableSet.ID,
			"type": "varsets", // TFE uses "varsets" not "variable-sets"
			"attributes": gin.H{
				"name":            variableSet.Name,
				"description":     variableSet.Description,
				"global":          global,               // TFE-compatible
				"priority":        variableSet.Priority, // TFE-compatible
				"updated-at":      variableSet.UpdatedAt.Format("2006-01-02T15:04:05Z"),
				"var-count":       len(variables),
				"workspace-count": len(workspacesData),
				"project-count":   len(projectsData),
			},
			"relationships": relationships,
			"links": gin.H{
				"self": fmt.Sprintf("/api/v2/varsets/%s", variableSet.ID),
			},
		},
	})
}

// CreateVariableSet handles POST /api/v2/organizations/:name/varsets
// TFE spec: POST /api/v2/organizations/:organization_name/varsets
// Reference: https://developer.hashicorp.com/terraform/enterprise/api-docs/variable-sets#create-a-variable-set
func (h *VariableSetHandlerV2) CreateVariableSet(c *gin.Context) {
	orgName := c.Param("name")

	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}
	_ = user

	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}}})
		return
	}

	var req CreateVariableSetRequestV2
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}}})
		return
	}

	// Validate JSON:API format
	if req.Data.Type != "varsets" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "data.type must be 'varsets'"}}})
		return
	}

	attrs := req.Data.Attributes

	// TFE uses "global" instead of "scope"
	// global=true means organization-scoped (applies to all workspaces)
	// global=false means workspace-scoped (applies to specific workspaces)
	scope := "workspace" // Default
	if attrs.Global {
		scope = "organization"
	}

	// Handle parent relationship - determine if variable set is project-owned or organization-owned
	var projectID *uuid.UUID
	if req.Data.Relationships.Parent.Data != nil {
		parentType, _ := req.Data.Relationships.Parent.Data["type"].(string)
		parentID, _ := req.Data.Relationships.Parent.Data["id"].(string)

		// If parent is a project, verify it exists and belongs to org
		if parentType == "projects" {
			projectUUID, err := uuid.Parse(parentID)
			if err == nil {
				project, err := h.projectRepo.GetByID(projectUUID)
				if err == nil && project.OrganizationID == org.ID {
					projectID = &projectUUID
					// If parent is project, global must be false (TFE requirement)
					if attrs.Global {
						c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Project-owned variable sets cannot be global"}}})
						return
					}
				}
			}
		}
		// If parent is organization, it must match the org in the URL
		if parentType == "organizations" && parentID != org.Name {
			c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Parent organization must match URL organization"}}})
			return
		}
	}

	variableSet := &models.VariableSet{
		OrganizationID: org.ID,
		Name:           attrs.Name,
		Description:    attrs.Description,
		Scope:          scope,
		Priority:       attrs.Priority,
		ProjectID:      projectID, // nil for organization-owned, set for project-owned
		CreatedBy:      user.ID,
	}

	if err := h.variableSetRepo.Create(variableSet); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to create variable set"}}})
		return
	}

	// Handle relationships.vars if provided (create variables in the set)
	if len(req.Data.Relationships.Vars.Data) > 0 {
		for _, varData := range req.Data.Relationships.Vars.Data {
			varType, _ := varData["type"].(string)
			if varType != "vars" {
				continue
			}
			attrs, _ := varData["attributes"].(map[string]interface{})
			if attrs == nil {
				continue
			}

			key, _ := attrs["key"].(string)
			value, _ := attrs["value"].(string)
			if key == "" || value == "" {
				continue
			}

			category := "terraform"
			if cat, ok := attrs["category"].(string); ok && (cat == "terraform" || cat == "env") {
				category = cat
			}

			hcl := false
			if hclVal, ok := attrs["hcl"].(bool); ok {
				hcl = hclVal
			}

			sensitive := false
			if sensVal, ok := attrs["sensitive"].(bool); ok {
				sensitive = sensVal
			}

			description := ""
			if desc, ok := attrs["description"].(string); ok {
				description = desc
			}

			variable := &models.VariableSetVariable{
				VariableSetID: variableSet.ID,
				Key:           key,
				Value:         value,
				Description:   description,
				Category:      category,
				HCL:           hcl,
				Sensitive:     sensitive,
			}

			_ = h.variableSetVariableRepo.Create(variable) // Ignore errors for now
		}
	}

	// Handle relationships.workspaces if provided
	if len(req.Data.Relationships.Workspaces.Data) > 0 {
		for _, wsData := range req.Data.Relationships.Workspaces.Data {
			wsID, _ := wsData["id"].(string)
			if wsID != "" {
				_ = h.variableSetRepo.AddWorkspace(variableSet.ID, wsID) // Ignore errors for now
			}
		}
	}

	// Handle relationships.projects if provided
	if len(req.Data.Relationships.Projects.Data) > 0 {
		for _, projData := range req.Data.Relationships.Projects.Data {
			projID, _ := projData["id"].(string)
			projUUID, err := uuid.Parse(projID)
			if err == nil {
				_ = h.variableSetRepo.AddProject(variableSet.ID, projUUID) // Ignore errors for now
			}
		}
	}

	// TFE uses "global" instead of "scope"
	global := variableSet.Scope == "organization"
	c.JSON(http.StatusCreated, gin.H{
		"data": gin.H{
			"id":   variableSet.ID,
			"type": "varsets", // TFE uses "varsets" not "variable-sets"
			"attributes": gin.H{
				"name":            variableSet.Name,
				"description":     variableSet.Description,
				"global":          global,               // TFE-compatible
				"priority":        variableSet.Priority, // TFE-compatible
				"updated-at":      variableSet.UpdatedAt.Format("2006-01-02T15:04:05Z"),
				"var-count":       0,
				"workspace-count": 0,
				"project-count":   0,
			},
			"relationships": gin.H{
				"organization": gin.H{
					"data": gin.H{
						"id":   org.Name,
						"type": "organizations",
					},
				},
				"parent": gin.H{
					"data": gin.H{
						"id":   org.Name,
						"type": "organizations",
					},
				},
				"vars": gin.H{
					"data": []gin.H{},
				},
			},
			"links": gin.H{
				"self": fmt.Sprintf("/api/v2/varsets/%s", variableSet.ID),
			},
		},
	})
}

// UpdateVariableSet handles PATCH /api/v2/varsets/:id or PATCH /api/v2/organizations/:name/varsets/:id
// TFE spec: PUT/PATCH /api/v2/varsets/:varset_id
// Reference: https://developer.hashicorp.com/terraform/enterprise/api-docs/variable-sets#update-a-variable-set
func (h *VariableSetHandlerV2) UpdateVariableSet(c *gin.Context) {
	variableSetID := c.Param("id")
	orgName := c.Param("name") // May be empty if called via /varsets/:id

	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}
	_ = user

	if variableSetID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid variable set ID"}}})
		return
	}

	variableSet, err := h.variableSetRepo.GetByID(variableSetID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable set not found"}}})
		return
	}

	// If orgName is provided, verify variable set belongs to organization
	var org *models.Organization
	if orgName != "" {
		org, err = h.orgRepo.GetByName(orgName)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}}})
			return
		}
		if variableSet.OrganizationID != org.ID {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable set not found"}}})
			return
		}
	}
	// Note: org is only needed when orgName is provided for validation
	// When orgName is empty, we don't need to fetch org

	var req UpdateVariableSetRequestV2
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}}})
		return
	}

	// Validate JSON:API format
	if req.Data.Type != "varsets" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "data.type must be 'varsets'"}}})
		return
	}

	attrs := req.Data.Attributes

	if attrs.Name != nil {
		variableSet.Name = *attrs.Name
	}
	if attrs.Description != nil {
		variableSet.Description = *attrs.Description
	}
	if attrs.Global != nil {
		// TFE uses "global" instead of "scope"
		if *attrs.Global {
			variableSet.Scope = "organization"
		} else {
			variableSet.Scope = "workspace"
		}
	}
	if attrs.Priority != nil {
		variableSet.Priority = *attrs.Priority
	}

	// Handle parent relationship update if provided
	if req.Data.Relationships.Parent.Data != nil {
		parentType, _ := req.Data.Relationships.Parent.Data["type"].(string)
		parentID, _ := req.Data.Relationships.Parent.Data["id"].(string)

		switch parentType {
		case "projects":
			projectUUID, err := uuid.Parse(parentID)
			if err == nil {
				project, err := h.projectRepo.GetByID(projectUUID)
				if err == nil && project.OrganizationID == variableSet.OrganizationID {
					variableSet.ProjectID = &projectUUID
					// If parent is project, global must be false (TFE requirement)
					if variableSet.Scope == "organization" {
						c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Project-owned variable sets cannot be global"}}})
						return
					}
				}
			}
		case "organizations":
			// Organization-owned: clear project ID
			variableSet.ProjectID = nil
		}
	}

	if err := h.variableSetRepo.Update(variableSet); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to update variable set"}}})
		return
	}

	// Get organization for relationships (org already declared above, use = not :=)
	if org == nil {
		org, _ = h.orgRepo.GetByID(variableSet.OrganizationID)
	}

	// Build parent relationship - project-owned or organization-owned
	parentData := gin.H{
		"id":   org.Name,
		"type": "organizations",
	}
	if variableSet.ProjectID != nil {
		project, err := h.projectRepo.GetByID(*variableSet.ProjectID)
		if err == nil && project != nil {
			parentData = gin.H{
				"id":   project.ID.String(),
				"type": "projects",
			}
		}
	}

	// TFE uses "global" instead of "scope"
	global := variableSet.Scope == "organization"
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"id":   variableSet.ID,
			"type": "varsets", // TFE uses "varsets" not "variable-sets"
			"attributes": gin.H{
				"name":            variableSet.Name,
				"description":     variableSet.Description,
				"global":          global,               // TFE-compatible
				"priority":        variableSet.Priority, // TFE-compatible
				"updated-at":      variableSet.UpdatedAt.Format("2006-01-02T15:04:05Z"),
				"var-count":       0, // Will be populated if variables are loaded
				"workspace-count": 0,
				"project-count":   0,
			},
			"relationships": gin.H{
				"organization": gin.H{
					"data": gin.H{
						"id":   org.Name,
						"type": "organizations",
					},
				},
				"parent": gin.H{
					"data": parentData,
				},
			},
			"links": gin.H{
				"self": fmt.Sprintf("/api/v2/varsets/%s", variableSet.ID),
			},
		},
	})
}

// DeleteVariableSet handles DELETE /api/v2/varsets/:id or DELETE /api/v2/organizations/:name/varsets/:id
// TFE spec: DELETE /api/v2/varsets/:varset_id
// Reference: https://developer.hashicorp.com/terraform/enterprise/api-docs/variable-sets#delete-a-variable-set
func (h *VariableSetHandlerV2) DeleteVariableSet(c *gin.Context) {
	variableSetID := c.Param("id")
	orgName := c.Param("name") // May be empty if called via /varsets/:id

	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}
	_ = user

	if variableSetID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid variable set ID"}}})
		return
	}

	variableSet, err := h.variableSetRepo.GetByID(variableSetID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable set not found"}}})
		return
	}

	// If orgName is provided, verify variable set belongs to organization
	var org *models.Organization
	if orgName != "" {
		org, err = h.orgRepo.GetByName(orgName)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}}})
			return
		}
		if variableSet.OrganizationID != org.ID {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable set not found"}}})
			return
		}
	}
	// Note: org is only needed when orgName is provided for validation
	// When orgName is empty, we don't need to fetch org

	if err := h.variableSetRepo.Delete(variableSetID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to delete variable set"}}})
		return
	}

	c.Status(http.StatusNoContent)
}

// AssignWorkspace handles POST /api/v2/varsets/:id/relationships/workspaces
// TFE spec: POST /api/v2/varsets/:varset_id/relationships/workspaces
// Reference: https://developer.hashicorp.com/terraform/enterprise/api-docs/variable-sets#apply-variable-set-to-workspaces
func (h *VariableSetHandlerV2) AssignWorkspace(c *gin.Context) {
	variableSetID := c.Param("id")

	// TFE spec: Request body contains array of workspace references
	var req struct {
		Data []struct {
			Type string `json:"type"` // Must be "workspaces"
			ID   string `json:"id"`   // Workspace ID
		} `json:"data"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}}})
		return
	}

	if len(req.Data) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "data array cannot be empty"}}})
		return
	}

	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}
	_ = user

	if variableSetID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid variable set ID"}}})
		return
	}

	variableSet, err := h.variableSetRepo.GetByID(variableSetID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable set not found"}}})
		return
	}

	// Only workspace-scoped variable sets can be assigned to workspaces
	if variableSet.Scope != "workspace" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Only workspace-scoped variable sets can be assigned to workspaces"}}})
		return
	}

	// Process each workspace in the request
	for _, workspaceRef := range req.Data {
		if workspaceRef.Type != "workspaces" {
			c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "data[].type must be 'workspaces'"}}})
			return
		}

		workspaceID := workspaceRef.ID
		if workspaceID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": fmt.Sprintf("Invalid workspace ID: %s", workspaceRef.ID)}}})
			return
		}

		workspace, err := h.workspaceRepo.GetByID(workspaceID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": fmt.Sprintf("Workspace not found: %s", workspaceRef.ID)}}})
			return
		}

		// Verify workspace belongs to same organization as variable set
		project, err := h.projectRepo.GetByID(workspace.ProjectID)
		if err != nil || project.OrganizationID != variableSet.OrganizationID {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": fmt.Sprintf("Workspace not found: %s", workspaceRef.ID)}}})
			return
		}

		if err := h.variableSetRepo.AddWorkspace(variableSetID, workspaceID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": fmt.Sprintf("Failed to assign variable set to workspace: %v", err)}}})
			return
		}
	}

	c.Status(http.StatusNoContent)
}

// UnassignWorkspace handles DELETE /api/v2/varsets/:id/relationships/workspaces
// TFE spec: DELETE /api/v2/varsets/:varset_id/relationships/workspaces
// Reference: https://developer.hashicorp.com/terraform/enterprise/api-docs/variable-sets#remove-a-variable-set-from-workspaces
func (h *VariableSetHandlerV2) UnassignWorkspace(c *gin.Context) {
	variableSetID := c.Param("id")

	// TFE spec: Request body contains array of workspace references
	var req struct {
		Data []struct {
			Type string `json:"type"` // Must be "workspaces"
			ID   string `json:"id"`   // Workspace ID
		} `json:"data"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}}})
		return
	}

	if len(req.Data) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "data array cannot be empty"}}})
		return
	}

	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}
	_ = user

	if variableSetID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid variable set ID"}}})
		return
	}

	// Verify variable set exists
	_, err = h.variableSetRepo.GetByID(variableSetID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable set not found"}}})
		return
	}

	// Process each workspace in the request
	for _, workspaceRef := range req.Data {
		if workspaceRef.Type != "workspaces" {
			c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "data[].type must be 'workspaces'"}}})
			return
		}

		workspaceID := workspaceRef.ID
		if workspaceID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": fmt.Sprintf("Invalid workspace ID: %s", workspaceRef.ID)}}})
			return
		}

		if err := h.variableSetRepo.RemoveWorkspace(variableSetID, workspaceID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": fmt.Sprintf("Failed to unassign variable set from workspace: %v", err)}}})
			return
		}
	}

	c.Status(http.StatusNoContent)
}

// AssignProject handles POST /api/v2/varsets/:id/relationships/projects
// TFE spec: POST /api/v2/varsets/:varset_id/relationships/projects
// Reference: https://developer.hashicorp.com/terraform/enterprise/api-docs/variable-sets#apply-variable-set-to-projects
func (h *VariableSetHandlerV2) AssignProject(c *gin.Context) {
	variableSetID := c.Param("id")

	// TFE spec: Request body contains array of project references
	var req struct {
		Data []struct {
			Type string `json:"type"` // Must be "projects"
			ID   string `json:"id"`   // Project ID
		} `json:"data"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}}})
		return
	}

	if len(req.Data) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "data array cannot be empty"}}})
		return
	}

	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}
	_ = user

	if variableSetID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid variable set ID"}}})
		return
	}

	variableSet, err := h.variableSetRepo.GetByID(variableSetID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable set not found"}}})
		return
	}

	// Only organization-scoped variable sets can be assigned to projects
	if variableSet.Scope != "organization" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Only organization-scoped variable sets can be assigned to projects"}}})
		return
	}

	// Process each project in the request
	for _, projectRef := range req.Data {
		if projectRef.Type != "projects" {
			c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "data[].type must be 'projects'"}}})
			return
		}

		projectUUID, err := uuid.Parse(projectRef.ID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": fmt.Sprintf("Invalid project ID: %s", projectRef.ID)}}})
			return
		}

		project, err := h.projectRepo.GetByID(projectUUID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": fmt.Sprintf("Project not found: %s", projectRef.ID)}}})
			return
		}

		// Verify project belongs to same organization as variable set
		if project.OrganizationID != variableSet.OrganizationID {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": fmt.Sprintf("Project not found: %s", projectRef.ID)}}})
			return
		}

		if err := h.variableSetRepo.AddProject(variableSetID, projectUUID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": fmt.Sprintf("Failed to assign variable set to project: %v", err)}}})
			return
		}
	}

	c.Status(http.StatusNoContent)
}

// UnassignProject handles DELETE /api/v2/varsets/:id/relationships/projects
// TFE spec: DELETE /api/v2/varsets/:varset_id/relationships/projects
// Reference: https://developer.hashicorp.com/terraform/enterprise/api-docs/variable-sets#remove-a-variable-set-from-projects
func (h *VariableSetHandlerV2) UnassignProject(c *gin.Context) {
	variableSetID := c.Param("id")

	// TFE spec: Request body contains array of project references
	var req struct {
		Data []struct {
			Type string `json:"type"` // Must be "projects"
			ID   string `json:"id"`   // Project ID
		} `json:"data"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}}})
		return
	}

	if len(req.Data) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "data array cannot be empty"}}})
		return
	}

	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}
	_ = user

	if variableSetID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid variable set ID"}}})
		return
	}

	// Verify variable set exists
	_, err = h.variableSetRepo.GetByID(variableSetID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable set not found"}}})
		return
	}

	// Process each project in the request
	for _, projectRef := range req.Data {
		if projectRef.Type != "projects" {
			c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "data[].type must be 'projects'"}}})
			return
		}

		projectUUID, err := uuid.Parse(projectRef.ID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": fmt.Sprintf("Invalid project ID: %s", projectRef.ID)}}})
			return
		}

		if err := h.variableSetRepo.RemoveProject(variableSetID, projectUUID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": fmt.Sprintf("Failed to unassign variable set from project: %v", err)}}})
			return
		}
	}

	c.Status(http.StatusNoContent)
}

// AssignJobTemplate handles POST /api/v2/varsets/:id/relationships/job-templates (StackWeaver-specific, not TFE)
func (h *VariableSetHandlerV2) AssignJobTemplate(c *gin.Context) {
	variableSetID := c.Param("id")

	var req struct {
		Data []struct {
			Type string `json:"type"` // Must be "job-templates"
			ID   string `json:"id"`   // Job Template ID (UUID)
		} `json:"data"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}}})
		return
	}

	if len(req.Data) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "data array cannot be empty"}}})
		return
	}

	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}
	_ = user

	if variableSetID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid variable set ID"}}})
		return
	}

	variableSet, err := h.variableSetRepo.GetByID(variableSetID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable set not found"}}})
		return
	}

	// Process each job template in the request
	for _, templateRef := range req.Data {
		if templateRef.Type != "job-templates" {
			c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "data[].type must be 'job-templates'"}}})
			return
		}

		templateUUID, err := uuid.Parse(templateRef.ID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": fmt.Sprintf("Invalid job template ID: %s", templateRef.ID)}}})
			return
		}

		template, err := h.jobTemplateRepo.GetByID(templateUUID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": fmt.Sprintf("Job template not found: %s", templateRef.ID)}}})
			return
		}

		// Verify job template belongs to same organization as variable set
		project, err := h.projectRepo.GetByID(template.ProjectID)
		if err != nil || project.OrganizationID != variableSet.OrganizationID {
			c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": fmt.Sprintf("Job template does not belong to the same organization as variable set: %s", templateRef.ID)}}})
			return
		}

		if err := h.variableSetRepo.AddJobTemplate(variableSetID, templateUUID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": fmt.Sprintf("Failed to assign variable set to job template: %v", err)}}})
			return
		}
	}

	c.Status(http.StatusNoContent)
}

// UnassignJobTemplate handles DELETE /api/v2/varsets/:id/relationships/job-templates (StackWeaver-specific, not TFE)
func (h *VariableSetHandlerV2) UnassignJobTemplate(c *gin.Context) {
	variableSetID := c.Param("id")

	var req struct {
		Data []struct {
			Type string `json:"type"` // Must be "job-templates"
			ID   string `json:"id"`   // Job Template ID (UUID)
		} `json:"data"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}}})
		return
	}

	if len(req.Data) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "data array cannot be empty"}}})
		return
	}

	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}
	_ = user

	if variableSetID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid variable set ID"}}})
		return
	}

	// Verify variable set exists
	_, err = h.variableSetRepo.GetByID(variableSetID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable set not found"}}})
		return
	}

	// Process each job template in the request
	for _, templateRef := range req.Data {
		if templateRef.Type != "job-templates" {
			c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "data[].type must be 'job-templates'"}}})
			return
		}

		templateUUID, err := uuid.Parse(templateRef.ID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": fmt.Sprintf("Invalid job template ID: %s", templateRef.ID)}}})
			return
		}

		if err := h.variableSetRepo.RemoveJobTemplate(variableSetID, templateUUID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": fmt.Sprintf("Failed to unassign variable set from job template: %v", err)}}})
			return
		}
	}

	c.Status(http.StatusNoContent)
}

// ListVariableSetsByJobTemplate handles GET /api/v2/ansible/job-templates/:id/variable-sets
// Returns variable sets that apply to the job template's project (TFE-compatible: automatic inheritance)
func (h *VariableSetHandlerV2) ListVariableSetsByJobTemplate(c *gin.Context) {
	jobTemplateIDStr := c.Param("id")
	jobTemplateID, err := uuid.Parse(jobTemplateIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid job template ID"}}})
		return
	}

	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}
	_ = user

	// Verify job template exists and get its project
	template, err := h.jobTemplateRepo.GetByID(jobTemplateID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Job template not found"}}})
		return
	}

	// Get variable sets that apply to this job template's project (TFE-compatible: automatic inheritance)
	variableSets, err := h.variableSetRepo.ListByProject(template.ProjectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to list variable sets"}}})
		return
	}

	// Get project and organization for relationships
	project, _ := h.projectRepo.GetByID(template.ProjectID)
	var org *models.Organization
	if project != nil {
		org, _ = h.orgRepo.GetByID(project.OrganizationID)
	}

	// Format response similar to ListVariableSets
	data := make([]gin.H, len(variableSets))
	for i, vs := range variableSets {
		// Get variables for this set
		variables, _ := h.variableSetVariableRepo.ListByVariableSet(vs.ID)

		relationships := gin.H{}
		if org != nil {
			relationships["organization"] = gin.H{
				"data": gin.H{
					"id":   org.Name,
					"type": "organizations",
				},
			}
		}

		// Include projects if organization-scoped and has projects assigned
		if vs.Scope == "organization" && len(vs.Projects) > 0 {
			projectsData := make([]gin.H, len(vs.Projects))
			for j, p := range vs.Projects {
				projectsData[j] = gin.H{
					"id":   p.ID.String(),
					"type": "projects",
				}
			}
			relationships["projects"] = gin.H{
				"data": projectsData,
			}
		}

		// TFE uses "global" instead of "scope"
		global := vs.Scope == "organization"

		data[i] = gin.H{
			"id":   vs.ID,
			"type": "varsets",
			"attributes": gin.H{
				"name":            vs.Name,
				"description":     vs.Description,
				"global":          global,
				"priority":        vs.Priority,
				"updated-at":      vs.UpdatedAt.Format("2006-01-02T15:04:05Z"),
				"var-count":       len(variables),
				"workspace-count": 0,
				"project-count":   len(vs.Projects),
			},
			"relationships": relationships,
			"links": gin.H{
				"self": fmt.Sprintf("/api/v2/varsets/%s", vs.ID),
			},
		}
	}

	c.JSON(http.StatusOK, gin.H{"data": data})
}

// CreateVariableSetVariableRequestV2 uses JSON:API format (TFE-compatible)
// Reference: https://developer.hashicorp.com/terraform/enterprise/api-docs/variable-sets#add-variable
type CreateVariableSetVariableRequestV2 struct {
	Data struct {
		Type       string `json:"type" binding:"required"` // Must be "vars"
		Attributes struct {
			Key         string `json:"key" binding:"required"`
			Value       string `json:"value" binding:"required"`
			Description string `json:"description,omitempty"`
			Category    string `json:"category,omitempty"`  // "terraform" or "env", defaults to "terraform"
			HCL         bool   `json:"hcl,omitempty"`       // Defaults to false
			Sensitive   bool   `json:"sensitive,omitempty"` // Defaults to false
		} `json:"attributes" binding:"required"`
	} `json:"data" binding:"required"`
}

// UpdateVariableSetVariableRequestV2 uses JSON:API format (TFE-compatible)
// Reference: https://developer.hashicorp.com/terraform/enterprise/api-docs/variable-sets#update-a-variable-in-a-variable-set
type UpdateVariableSetVariableRequestV2 struct {
	Data struct {
		Type       string `json:"type" binding:"required"` // Must be "vars"
		Attributes struct {
			Key         *string `json:"key,omitempty"`
			Value       *string `json:"value,omitempty"`
			Description *string `json:"description,omitempty"`
			Category    *string `json:"category,omitempty"`
			HCL         *bool   `json:"hcl,omitempty"`
			Sensitive   *bool   `json:"sensitive,omitempty"`
		} `json:"attributes"`
	} `json:"data" binding:"required"`
}

// ListVariableSetVariables handles GET /api/v2/varsets/:id/relationships/vars
// TFE spec: GET /api/v2/varsets/:varset_id/relationships/vars
// Reference: https://developer.hashicorp.com/terraform/enterprise/api-docs/variable-sets#list-variables-in-a-variable-set
func (h *VariableSetHandlerV2) ListVariableSetVariables(c *gin.Context) {
	variableSetID := c.Param("id")
	orgName := c.Param("name") // May be empty if called via /varsets/:id

	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}
	_ = user

	if variableSetID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid variable set ID"}}})
		return
	}

	variableSet, err := h.variableSetRepo.GetByID(variableSetID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable set not found"}}})
		return
	}

	// If orgName is provided, verify variable set belongs to organization
	var org *models.Organization
	if orgName != "" {
		org, err = h.orgRepo.GetByName(orgName)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}}})
			return
		}
		if variableSet.OrganizationID != org.ID {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable set not found"}}})
			return
		}
	}
	// Note: org is only needed when orgName is provided for validation
	// When orgName is empty, we don't need to fetch org

	variables, err := h.variableSetVariableRepo.ListByVariableSet(variableSetID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to list variables"}}})
		return
	}

	data := make([]gin.H, len(variables))
	for i, v := range variables {
		value := v.Value
		if v.Sensitive {
			value = "••••••••"
		}
		data[i] = gin.H{
			"id":   v.ID,
			"type": "vars", // TFE uses "vars" not "variable-set-variables"
			"attributes": gin.H{
				"key":         v.Key,
				"value":       value,
				"description": v.Description,
				"sensitive":   v.Sensitive,
				"category":    v.Category,
				"hcl":         v.HCL,
				"created-at":  v.CreatedAt.Format("2006-01-02T15:04:05Z"),
			},
			"relationships": gin.H{
				"varset": gin.H{
					"data": gin.H{
						"id":   variableSet.ID,
						"type": "varsets",
					},
					"links": gin.H{
						"related": fmt.Sprintf("/api/v2/varsets/%s", variableSet.ID),
					},
				},
			},
			"links": gin.H{
				"self": fmt.Sprintf("/api/v2/vars/%s", v.ID),
			},
		}
	}

	c.JSON(http.StatusOK, gin.H{"data": data})
}

// GetVariableSetVariable handles GET /api/v2/varsets/:id/relationships/vars/:variable_id
// TFE-compatible: Show variable in set. Provider uses this for Read/refresh; missing → 404 → drift.
func (h *VariableSetHandlerV2) GetVariableSetVariable(c *gin.Context) {
	variableSetID := c.Param("id")
	variableID := c.Param("variable_id")
	orgName := c.Param("name")

	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}
	_ = user

	if variableSetID == "" || variableID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid variable set or variable ID"}}})
		return
	}

	variableSet, err := h.variableSetRepo.GetByID(variableSetID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable set not found"}}})
		return
	}

	if orgName != "" {
		org, err := h.orgRepo.GetByName(orgName)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}}})
			return
		}
		if variableSet.OrganizationID != org.ID {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable set not found"}}})
			return
		}
	}

	variable, err := h.variableSetVariableRepo.GetByID(variableID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable not found"}}})
		return
	}
	if variable.VariableSetID != variableSetID {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable not found"}}})
		return
	}

	value := variable.Value
	if variable.Sensitive {
		value = "••••••••"
	}
	data := gin.H{
		"id":   variable.ID,
		"type": "vars",
		"attributes": gin.H{
			"key":         variable.Key,
			"value":       value,
			"description": variable.Description,
			"sensitive":   variable.Sensitive,
			"category":    variable.Category,
			"hcl":         variable.HCL,
			"created-at":  variable.CreatedAt.Format("2006-01-02T15:04:05Z"),
		},
		"relationships": gin.H{
			"varset": gin.H{
				"data":  gin.H{"id": variableSet.ID, "type": "varsets"},
				"links": gin.H{"related": fmt.Sprintf("/api/v2/varsets/%s", variableSet.ID)},
			},
		},
		"links": gin.H{"self": fmt.Sprintf("/api/v2/vars/%s", variable.ID)},
	}

	c.JSON(http.StatusOK, gin.H{"data": data})
}

// CreateVariableSetVariable handles POST /api/v2/varsets/:id/relationships/vars
// TFE spec: POST /api/v2/varsets/:varset_id/relationships/vars
// Reference: https://developer.hashicorp.com/terraform/enterprise/api-docs/variable-sets#add-variable
func (h *VariableSetHandlerV2) CreateVariableSetVariable(c *gin.Context) {
	variableSetID := c.Param("id")
	orgName := c.Param("name") // May be empty if called via /varsets/:id

	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}
	_ = user

	if variableSetID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid variable set ID"}}})
		return
	}

	variableSet, err := h.variableSetRepo.GetByID(variableSetID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable set not found"}}})
		return
	}

	// If orgName is provided, verify variable set belongs to organization
	var org *models.Organization
	if orgName != "" {
		org, err = h.orgRepo.GetByName(orgName)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}}})
			return
		}
		if variableSet.OrganizationID != org.ID {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable set not found"}}})
			return
		}
	} else {
		// Get organization from variable set to validate it exists
		_, err = h.orgRepo.GetByID(variableSet.OrganizationID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}}})
			return
		}
	}

	var req CreateVariableSetVariableRequestV2
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}}})
		return
	}

	// Validate JSON:API format
	if req.Data.Type != "vars" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "data.type must be 'vars'"}}})
		return
	}

	attrs := req.Data.Attributes

	// Set defaults
	category := attrs.Category
	if category == "" {
		category = "terraform" // TFE default
	}
	if category != "terraform" && category != "env" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "category must be 'terraform' or 'env'"}}})
		return
	}

	variable := &models.VariableSetVariable{
		VariableSetID: variableSetID,
		Key:           attrs.Key,
		Value:         attrs.Value,
		Description:   attrs.Description,
		Category:      category,
		HCL:           attrs.HCL,
		Sensitive:     attrs.Sensitive,
		Encrypted:     false, // Legacy field, not in TFE spec
	}

	if err := h.variableSetVariableRepo.Create(variable); err != nil {
		logger.Infof("Failed to create variable set variable: %v", err)

		// Check for duplicate key error
		errStr := strings.ToLower(err.Error())
		if strings.Contains(errStr, "duplicate key") ||
			strings.Contains(errStr, "unique constraint") ||
			strings.Contains(errStr, "idx_variable_set_key") ||
			err == gorm.ErrDuplicatedKey {
			c.JSON(http.StatusConflict, gin.H{
				"errors": []gin.H{{
					"status": "409",
					"title":  "Conflict",
					"detail": fmt.Sprintf("A variable with the key '%s' already exists in this variable set. Variable keys must be unique within a variable set.", req.Data.Attributes.Key),
				}},
			})
			return
		}

		// Check for foreign key constraint (variable set doesn't exist)
		if strings.Contains(errStr, "foreign key") ||
			strings.Contains(errStr, "violates foreign key constraint") {
			c.JSON(http.StatusNotFound, gin.H{
				"errors": []gin.H{{
					"status": "404",
					"title":  "Not Found",
					"detail": "Variable set not found",
				}},
			})
			return
		}

		// Generic error
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{{
				"status": "500",
				"title":  "Internal Server Error",
				"detail": fmt.Sprintf("Failed to create variable: %v", err),
			}},
		})
		return
	}

	value := variable.Value
	if variable.Sensitive {
		value = "••••••••"
	}

	c.JSON(http.StatusCreated, gin.H{
		"data": gin.H{
			"id":   variable.ID,
			"type": "vars", // TFE uses "vars" not "variable-set-variables"
			"attributes": gin.H{
				"key":         variable.Key,
				"value":       value,
				"description": variable.Description,
				"sensitive":   variable.Sensitive,
				"category":    variable.Category,
				"hcl":         variable.HCL,
			},
			"relationships": gin.H{
				"varset": gin.H{
					"data": gin.H{
						"id":   variableSet.ID,
						"type": "varsets",
					},
					"links": gin.H{
						"related": fmt.Sprintf("/api/v2/varsets/%s", variableSet.ID),
					},
				},
			},
			"links": gin.H{
				"self": fmt.Sprintf("/api/v2/vars/%s", variable.ID),
			},
		},
	})
}

// UpdateVariableSetVariable handles PATCH /api/v2/varsets/:id/relationships/vars/:variable_id
// TFE spec: PATCH /api/v2/varsets/:varset_id/relationships/vars/:var_id
// Reference: https://developer.hashicorp.com/terraform/enterprise/api-docs/variable-sets#update-a-variable-in-a-variable-set
func (h *VariableSetHandlerV2) UpdateVariableSetVariable(c *gin.Context) {
	variableSetID := c.Param("id")
	variableID := c.Param("variable_id")
	orgName := c.Param("name") // May be empty if called via /varsets/:id

	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}
	_ = user

	if variableSetID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid variable set ID"}}})
		return
	}

	if variableID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid variable ID"}}})
		return
	}

	variableSet, err := h.variableSetRepo.GetByID(variableSetID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable set not found"}}})
		return
	}

	// If orgName is provided, verify variable set belongs to organization
	var org *models.Organization
	if orgName != "" {
		org, err = h.orgRepo.GetByName(orgName)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}}})
			return
		}
		if variableSet.OrganizationID != org.ID {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable set not found"}}})
			return
		}
	} else {
		// Get organization from variable set to validate it exists
		_, err = h.orgRepo.GetByID(variableSet.OrganizationID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}}})
			return
		}
	}

	variable, err := h.variableSetVariableRepo.GetByID(variableID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable not found"}}})
		return
	}

	if variable.VariableSetID != variableSetID {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable not found"}}})
		return
	}

	var req UpdateVariableSetVariableRequestV2
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}}})
		return
	}

	// Validate JSON:API format
	if req.Data.Type != "vars" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "data.type must be 'vars'"}}})
		return
	}

	attrs := req.Data.Attributes

	if attrs.Key != nil {
		variable.Key = *attrs.Key
	}
	if attrs.Value != nil {
		variable.Value = *attrs.Value
	}
	if attrs.Description != nil {
		variable.Description = *attrs.Description
	}
	if attrs.Category != nil {
		if *attrs.Category != "terraform" && *attrs.Category != "env" {
			c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "category must be 'terraform' or 'env'"}}})
			return
		}
		variable.Category = *attrs.Category
	}
	if attrs.HCL != nil {
		variable.HCL = *attrs.HCL
	}
	if attrs.Sensitive != nil {
		variable.Sensitive = *attrs.Sensitive
	}

	if err := h.variableSetVariableRepo.Update(variable); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to update variable"}}})
		return
	}

	value := variable.Value
	if variable.Sensitive {
		value = "••••••••"
	}

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"id":   variable.ID,
			"type": "vars", // TFE uses "vars" not "variable-set-variables"
			"attributes": gin.H{
				"key":         variable.Key,
				"value":       value,
				"description": variable.Description,
				"sensitive":   variable.Sensitive,
				"category":    variable.Category,
				"hcl":         variable.HCL,
			},
			"relationships": gin.H{
				"varset": gin.H{
					"data": gin.H{
						"id":   variableSet.ID,
						"type": "varsets",
					},
					"links": gin.H{
						"related": fmt.Sprintf("/api/v2/varsets/%s", variableSet.ID),
					},
				},
			},
			"links": gin.H{
				"self": fmt.Sprintf("/api/v2/vars/%s", variable.ID),
			},
		},
	})
}

// DeleteVariableSetVariable handles DELETE /api/v2/varsets/:id/relationships/vars/:variable_id
// TFE spec: DELETE /api/v2/varsets/:varset_id/relationships/vars/:var_id
// Reference: https://developer.hashicorp.com/terraform/enterprise/api-docs/variable-sets#delete-a-variable-in-a-variable-set
func (h *VariableSetHandlerV2) DeleteVariableSetVariable(c *gin.Context) {
	variableSetID := c.Param("id")
	variableID := c.Param("variable_id")
	orgName := c.Param("name") // May be empty if called via /varsets/:id

	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}
	_ = user

	if variableSetID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid variable set ID"}}})
		return
	}

	if variableID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid variable ID"}}})
		return
	}

	variableSet, err := h.variableSetRepo.GetByID(variableSetID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable set not found"}}})
		return
	}

	// If orgName is provided, verify variable set belongs to organization
	var org *models.Organization
	if orgName != "" {
		org, err = h.orgRepo.GetByName(orgName)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}}})
			return
		}
		if variableSet.OrganizationID != org.ID {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable set not found"}}})
			return
		}
	} else {
		// Get organization from variable set to validate it exists
		_, err = h.orgRepo.GetByID(variableSet.OrganizationID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}}})
			return
		}
	}

	variable, err := h.variableSetVariableRepo.GetByID(variableID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable not found"}}})
		return
	}

	if variable.VariableSetID != variableSetID {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Variable not found"}}})
		return
	}

	if err := h.variableSetVariableRepo.Delete(variableID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to delete variable"}}})
		return
	}

	c.Status(http.StatusNoContent)
}
