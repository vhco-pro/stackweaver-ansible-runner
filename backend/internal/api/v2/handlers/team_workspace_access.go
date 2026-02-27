// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/rbac"
)

type TeamWorkspaceAccessHandlerV2 struct {
	teamRepo      *repository.TeamRepository
	workspaceRepo *repository.WorkspaceRepository
	projectRepo   *repository.ProjectRepository
	orgRepo       *repository.OrganizationRepository
	authService   *auth.Service
	rbacService   *rbac.Service
}

func NewTeamWorkspaceAccessHandlerV2(
	teamRepo *repository.TeamRepository,
	workspaceRepo *repository.WorkspaceRepository,
	projectRepo *repository.ProjectRepository,
	orgRepo *repository.OrganizationRepository,
	authService *auth.Service,
	rbacService *rbac.Service,
) *TeamWorkspaceAccessHandlerV2 {
	return &TeamWorkspaceAccessHandlerV2{
		teamRepo:      teamRepo,
		workspaceRepo: workspaceRepo,
		projectRepo:   projectRepo,
		orgRepo:       orgRepo,
		authService:   authService,
		rbacService:   rbacService,
	}
}

type CreateTeamWorkspaceAccessRequestV2 struct {
	Data struct {
		Type       string `json:"type" binding:"required"`
		Attributes struct {
			// Fixed access level (optional: "admin", "read", "plan", "write", "custom")
			Access *string `json:"access,omitempty"`

			// Custom permissions (top-level attributes in JSON:API, not nested)
			// These are sent as top-level attributes when using custom permissions
			Runs             *string `json:"runs,omitempty"`              // "read", "plan", "apply"
			Variables        *string `json:"variables,omitempty"`         // "none", "read", "write"
			StateVersions    *string `json:"state-versions,omitempty"`    // "none", "read", "read-outputs", "write"
			SentinelMocks    *string `json:"sentinel-mocks,omitempty"`    // "none", "read"
			WorkspaceLocking *bool   `json:"workspace-locking,omitempty"` // boolean
			RunTasks         *bool   `json:"run-tasks,omitempty"`         // boolean
		} `json:"attributes"`
		Relationships struct {
			Team struct {
				Data struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"team"`
			Workspace struct {
				Data struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"workspace,omitempty"` // TFE-compatible: workspace in relationships
		} `json:"relationships"`
	} `json:"data" binding:"required"`
}

type UpdateTeamWorkspaceAccessRequestV2 struct {
	Data struct {
		Type       string `json:"type" binding:"required"`
		Attributes struct {
			// Fixed access level (optional: "admin", "read", "plan", "write", "custom")
			Access *string `json:"access,omitempty"`

			// Custom permissions (top-level attributes in JSON:API, not nested)
			// These are sent as top-level attributes when using custom permissions
			Runs             *string `json:"runs,omitempty"`              // "read", "plan", "apply"
			Variables        *string `json:"variables,omitempty"`         // "none", "read", "write"
			StateVersions    *string `json:"state-versions,omitempty"`    // "none", "read", "read-outputs", "write"
			SentinelMocks    *string `json:"sentinel-mocks,omitempty"`    // "none", "read"
			WorkspaceLocking *bool   `json:"workspace-locking,omitempty"` // boolean
			RunTasks         *bool   `json:"run-tasks,omitempty"`         // boolean
		} `json:"attributes"`
	} `json:"data" binding:"required"`
}

// formatTeamWorkspaceAccessResponse formats a team workspace access in TFE-compatible JSON:API format
// TFE uses type "team-workspaces" (not "team-workspace-accesses")
func formatTeamWorkspaceAccessResponse(access *models.TeamWorkspaceAccess) gin.H {
	attributes := gin.H{}

	// Check if we have custom permissions (any permission field is set)
	hasCustomPermissions := access.Runs != nil || access.Variables != nil || access.StateVersions != nil ||
		access.SentinelMocks != nil || access.WorkspaceLocking != nil || access.RunTasks != nil

	// TFE behavior: If custom permissions are set, access should be "custom"
	// If fixed access level is set, use that
	if hasCustomPermissions {
		// Custom permissions: set access to "custom"
		attributes["access"] = "custom"

		// Add custom permissions block
		permissions := gin.H{}
		if access.Runs != nil {
			permissions["runs"] = *access.Runs
		} else {
			permissions["runs"] = "read" // Default when not specified
		}
		if access.Variables != nil {
			permissions["variables"] = *access.Variables
		} else {
			permissions["variables"] = "none" // Default when not specified
		}
		if access.StateVersions != nil {
			permissions["state-versions"] = *access.StateVersions
		} else {
			permissions["state-versions"] = "none" // Default when not specified
		}
		if access.SentinelMocks != nil {
			permissions["sentinel-mocks"] = *access.SentinelMocks
		} else {
			permissions["sentinel-mocks"] = "none" // Default when not specified
		}
		if access.WorkspaceLocking != nil {
			permissions["workspace-locking"] = *access.WorkspaceLocking
		} else {
			permissions["workspace-locking"] = false // Default when not specified
		}
		if access.RunTasks != nil {
			permissions["run-tasks"] = *access.RunTasks
		} else {
			permissions["run-tasks"] = false // Default when not specified
		}
		attributes["permissions"] = permissions
	} else if access.Access != nil {
		// Fixed access level: use the access value
		attributes["access"] = *access.Access
	}

	return gin.H{
		"id":         access.ID.String(),
		"type":       "team-workspaces", // TFE uses "team-workspaces" as the resource type
		"attributes": attributes,
		"relationships": gin.H{
			"team": gin.H{
				"data": gin.H{
					"id":   access.TeamID.String(),
					"type": "teams",
				},
			},
			"workspace": gin.H{
				"data": gin.H{
					"id":   access.WorkspaceID,
					"type": "workspaces",
				},
			},
		},
		"links": gin.H{
			"self": "/api/v2/team-workspaces/" + access.ID.String(), // TFE-compatible self link
		},
	}
}

// List lists team access for a workspace
// GET /api/v2/team-workspaces?filter[workspace][id]=ws-... (TFE-compatible)
// GET /api/v2/workspaces/:id/relationships/team-access (legacy)
func (h *TeamWorkspaceAccessHandlerV2) List(c *gin.Context) {
	// TFE-compatible: workspace ID comes from query param filter[workspace][id]
	workspaceID := c.Query("filter[workspace][id]")

	// Legacy: workspace ID comes from URL param
	if workspaceID == "" {
		workspaceID = c.Param("id")
	}

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

	// Get organization for authorization check
	project, err := h.projectRepo.GetByID(workspace.ProjectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve project",
				},
			},
		})
		return
	}

	org, err := h.orgRepo.GetByID(project.OrganizationID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve organization",
				},
			},
		})
		return
	}

	// Verify user has access to the organization (team-based)
	inOrg, err := h.orgRepo.UserInOrg(user.ID, org.ID)
	if err != nil || !inOrg {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "You must be a member of this organization (via team membership)",
				},
			},
		})
		return
	}

	// Get team access for workspace
	accessList, err := h.teamRepo.GetWorkspaceAccess(workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve team access",
				},
			},
		})
		return
	}

	// Format response
	data := make([]gin.H, len(accessList))
	for i := range accessList {
		data[i] = formatTeamWorkspaceAccessResponse(&accessList[i])
	}

	c.JSON(http.StatusOK, gin.H{
		"data": data,
	})
}

// Create creates team access for a workspace
// POST /api/v2/team-workspaces (TFE-compatible - team and workspace in relationships)
// POST /api/v2/workspaces/:id/relationships/team-access (legacy - workspace ID in URL)
func (h *TeamWorkspaceAccessHandlerV2) Create(c *gin.Context) {
	// Legacy: workspace ID comes from URL param
	// TFE-compatible: workspace ID comes from relationships
	workspaceID := c.Param("id") // May be empty for TFE route

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

	var req CreateTeamWorkspaceAccessRequestV2
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
	// TFE uses "team-workspaces" as the type, but we also accept "team-workspace-accesses" for backward compatibility
	if req.Data.Type != "team-workspaces" && req.Data.Type != "team-workspace-accesses" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "data.type must be 'team-workspaces' or 'team-workspace-accesses'",
				},
			},
		})
		return
	}

	// Get team ID from relationships
	// TFE-compatible: team and workspace are in relationships
	// Legacy: workspace ID is in URL param, only team is in relationships
	var teamIDStr string
	var workspaceIDFromReq string

	if req.Data.Relationships.Team.Data.ID != "" {
		teamIDStr = req.Data.Relationships.Team.Data.ID
	}

	// Check if workspace is in relationships (TFE-compatible format)
	if req.Data.Relationships.Workspace.Data.ID != "" {
		workspaceIDFromReq = req.Data.Relationships.Workspace.Data.ID
	}

	// If workspace is in relationships, use it; otherwise use URL param (legacy)
	if workspaceIDFromReq != "" {
		workspaceID = workspaceIDFromReq
	} else if workspaceID == "" {
		// Neither URL param nor relationships provided
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Workspace must be provided either in URL or relationships",
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

	// Get organization for authorization check
	project, err := h.projectRepo.GetByID(workspace.ProjectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve project",
				},
			},
		})
		return
	}

	org, err := h.orgRepo.GetByID(project.OrganizationID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve organization",
				},
			},
		})
		return
	}

	// Check if user has permission to manage teams (team workspace access requires team management permission)
	hasPermission, err := h.rbacService.CheckOrgManageTeams(c.Request.Context(), user.ID, org.ID)
	if err != nil || !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Only organization admins can manage team workspace access",
				},
			},
		})
		return
	}

	if teamIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "team relationship is required",
				},
			},
		})
		return
	}

	teamID, err := uuid.Parse(teamIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid team ID format",
				},
			},
		})
		return
	}

	// Verify team exists and belongs to same organization
	team, err := h.teamRepo.GetByID(teamID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Team not found",
				},
			},
		})
		return
	}

	if team.OrganizationID != org.ID {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Team must belong to the same organization as the workspace",
				},
			},
		})
		return
	}

	// Validate: either access OR custom permissions, not both (unless access is "custom")
	attrs := req.Data.Attributes
	hasAccess := attrs.Access != nil && *attrs.Access != ""

	// Check if custom permissions are provided (top-level attributes, not nested)
	hasCustomPermissions := attrs.Runs != nil || attrs.Variables != nil || attrs.StateVersions != nil ||
		attrs.SentinelMocks != nil || attrs.WorkspaceLocking != nil || attrs.RunTasks != nil

	// When using custom permissions, provider sends access="custom" AND permission attributes
	// This is valid - "custom" access means use the permission attributes
	isCustomAccess := hasAccess && *attrs.Access == "custom"

	if hasAccess && hasCustomPermissions && !isCustomAccess {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Cannot provide both 'access' and custom permission attributes unless access is 'custom'. Use either 'access' OR custom permission attributes",
				},
			},
		})
		return
	}

	if !hasAccess && !hasCustomPermissions {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Must provide either 'access' or custom permission attributes",
				},
			},
		})
		return
	}

	// Build TeamWorkspaceAccess model
	accessEntry := &models.TeamWorkspaceAccess{
		TeamID:      teamID,
		WorkspaceID: workspaceID,
	}

	// Set access level or custom permissions
	// If access is "custom", ignore it and use permissions block instead
	if hasAccess && !isCustomAccess {
		access := *attrs.Access
		// Validate access level (excluding "custom" which is handled separately)
		if access != "admin" && access != "read" && access != "plan" && access != "write" {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "access must be one of: admin, read, plan, write, custom",
					},
				},
			})
			return
		}
		accessEntry.Access = &access
	} else if hasCustomPermissions || isCustomAccess {
		// Custom permissions: access should be nil in database, permission attributes will be used
		// When access="custom" is sent, we ignore it and use the permission attributes
		// Parse custom permissions (all fields are required when using custom permissions)

		// Runs permission (required for custom)
		if attrs.Runs == nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "runs is required when using custom permissions",
					},
				},
			})
			return
		}
		runsVal := *attrs.Runs
		if runsVal != "read" && runsVal != "plan" && runsVal != "apply" {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "runs must be one of: read, plan, apply",
					},
				},
			})
			return
		}
		accessEntry.Runs = &runsVal

		// Variables permission (required for custom)
		if attrs.Variables == nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "variables is required when using custom permissions",
					},
				},
			})
			return
		}
		variablesVal := *attrs.Variables
		if variablesVal != "none" && variablesVal != "read" && variablesVal != "write" {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "variables must be one of: none, read, write",
					},
				},
			})
			return
		}
		accessEntry.Variables = &variablesVal

		// State versions permission (required for custom)
		if attrs.StateVersions == nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "state-versions is required when using custom permissions",
					},
				},
			})
			return
		}
		stateVersionsVal := *attrs.StateVersions
		if stateVersionsVal != "none" && stateVersionsVal != "read" && stateVersionsVal != "read-outputs" && stateVersionsVal != "write" {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "state-versions must be one of: none, read, read-outputs, write",
					},
				},
			})
			return
		}
		accessEntry.StateVersions = &stateVersionsVal

		// Sentinel mocks permission (required for custom)
		if attrs.SentinelMocks == nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "sentinel-mocks is required when using custom permissions",
					},
				},
			})
			return
		}
		sentinelMocksVal := *attrs.SentinelMocks
		if sentinelMocksVal != "none" && sentinelMocksVal != "read" {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "sentinel-mocks must be one of: none, read",
					},
				},
			})
			return
		}
		accessEntry.SentinelMocks = &sentinelMocksVal

		// Workspace locking (required for custom)
		if attrs.WorkspaceLocking == nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "workspace-locking is required when using custom permissions",
					},
				},
			})
			return
		}
		accessEntry.WorkspaceLocking = attrs.WorkspaceLocking

		// Run tasks (required for custom)
		if attrs.RunTasks == nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "run-tasks is required when using custom permissions",
					},
				},
			})
			return
		}
		accessEntry.RunTasks = attrs.RunTasks

		// When using custom permissions, Access field should be nil (not "custom")
		// The "custom" value is only used in API responses, not stored in DB
		accessEntry.Access = nil
	}

	// Check if access already exists
	existingAccess, _ := h.teamRepo.GetWorkspaceAccessByTeamAndWorkspace(teamID, workspaceID)
	if existingAccess != nil {
		c.JSON(http.StatusConflict, gin.H{
			"errors": []gin.H{
				{
					"status": "409",
					"title":  "Conflict",
					"detail": "Team access already exists for this workspace",
				},
			},
		})
		return
	}

	// Create access entry
	if err := h.teamRepo.CreateWorkspaceAccess(accessEntry); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to create team workspace access",
				},
			},
		})
		return
	}

	// Reload with relationships
	createdAccess, err := h.teamRepo.GetWorkspaceAccessByID(accessEntry.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve created access",
				},
			},
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"data": formatTeamWorkspaceAccessResponse(createdAccess),
	})
}

// Get retrieves a team workspace access by ID
// GET /api/v2/team-workspaces/:id (TFE-compatible)
// GET /api/v2/workspaces/:id/relationships/team-access/:access_id (legacy)
func (h *TeamWorkspaceAccessHandlerV2) Get(c *gin.Context) {
	// TFE-compatible: ID is directly in URL (/api/v2/team-workspaces/:id)
	// Legacy: workspace ID and access ID are both in URL (/api/v2/workspaces/:id/relationships/team-access/:access_id)
	accessIDStr := c.Param("id")
	accessIDFromSecondParam := c.Param("access_id")

	var workspaceID string
	if accessIDFromSecondParam != "" {
		// Legacy route: workspace ID is first param, access ID is second param
		workspaceID = accessIDStr
		accessIDStr = accessIDFromSecondParam
	}
	// TFE route: access ID is the only param, workspace ID will be retrieved from the access record

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

	accessID, err := uuid.Parse(accessIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid access ID format",
				},
			},
		})
		return
	}

	// Get access entry
	access, err := h.teamRepo.GetWorkspaceAccessByID(accessID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Team workspace access not found",
				},
			},
		})
		return
	}

	// For legacy route, verify workspace ID matches
	if workspaceID != "" && access.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Team workspace access not found",
				},
			},
		})
		return
	}

	// Use workspace ID from access record (for TFE route) or from URL param (for legacy route)
	actualWorkspaceID := access.WorkspaceID
	if workspaceID != "" {
		actualWorkspaceID = workspaceID
	}

	// Verify workspace exists and user has access
	workspace, err := h.workspaceRepo.GetByID(actualWorkspaceID)
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

	project, err := h.projectRepo.GetByID(workspace.ProjectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve project",
				},
			},
		})
		return
	}

	org, err := h.orgRepo.GetByID(project.OrganizationID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve organization",
				},
			},
		})
		return
	}

	inOrg, err := h.orgRepo.UserInOrg(user.ID, org.ID)
	if err != nil || !inOrg {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "You must be a member of this organization (via team membership)",
				},
			},
		})
		return
	}
	_ = workspace

	c.JSON(http.StatusOK, gin.H{
		"data": formatTeamWorkspaceAccessResponse(access),
	})
}

// Update updates team workspace access
// PATCH /api/v2/workspaces/:id/relationships/team-access/:access_id
func (h *TeamWorkspaceAccessHandlerV2) Update(c *gin.Context) {
	workspaceID := c.Param("id")
	accessIDStr := c.Param("access_id")

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

	accessID, err := uuid.Parse(accessIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid access ID format",
				},
			},
		})
		return
	}

	// Get access entry
	access, err := h.teamRepo.GetWorkspaceAccessByID(accessID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Team workspace access not found",
				},
			},
		})
		return
	}

	// Verify workspace ID matches
	if access.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Team workspace access not found",
				},
			},
		})
		return
	}

	// Get workspace to get project ID
	workspace, err := h.workspaceRepo.GetByID(access.WorkspaceID)
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

	// Get organization for authorization check
	project, err := h.projectRepo.GetByID(workspace.ProjectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve project",
				},
			},
		})
		return
	}

	org, err := h.orgRepo.GetByID(project.OrganizationID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve organization",
				},
			},
		})
		return
	}

	// Check if user has permission to manage teams (team workspace access requires team management permission)
	hasPermission, err := h.rbacService.CheckOrgManageTeams(c.Request.Context(), user.ID, org.ID)
	if err != nil || !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Only organization admins can update team workspace access",
				},
			},
		})
		return
	}

	var req UpdateTeamWorkspaceAccessRequestV2
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
	// TFE uses "team-workspaces" as the type, but we also accept "team-workspace-accesses" for backward compatibility
	if req.Data.Type != "team-workspaces" && req.Data.Type != "team-workspace-accesses" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "data.type must be 'team-workspaces' or 'team-workspace-accesses'",
				},
			},
		})
		return
	}

	// Validate: either access OR custom permissions, not both (unless access is "custom")
	attrs := req.Data.Attributes
	hasAccess := attrs.Access != nil && *attrs.Access != ""

	// Check if custom permissions are provided (top-level attributes, not nested)
	hasCustomPermissions := attrs.Runs != nil || attrs.Variables != nil || attrs.StateVersions != nil ||
		attrs.SentinelMocks != nil || attrs.WorkspaceLocking != nil || attrs.RunTasks != nil

	// When using custom permissions, provider sends access="custom" AND permission attributes
	isCustomAccess := hasAccess && *attrs.Access == "custom"

	if hasAccess && hasCustomPermissions && !isCustomAccess {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Cannot provide both 'access' and custom permission attributes unless access is 'custom'. Use either 'access' OR custom permission attributes",
				},
			},
		})
		return
	}

	// Update access level or custom permissions
	if hasAccess && !isCustomAccess {
		accessVal := *attrs.Access
		// Validate access level (excluding "custom" which is handled separately)
		if accessVal != "admin" && accessVal != "read" && accessVal != "plan" && accessVal != "write" {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "access must be one of: admin, read, plan, write, custom",
					},
				},
			})
			return
		}
		// Clear custom permissions and set access
		access.Access = &accessVal
		access.Runs = nil
		access.Variables = nil
		access.StateVersions = nil
		access.SentinelMocks = nil
		access.WorkspaceLocking = nil
		access.RunTasks = nil
	} else if hasCustomPermissions || isCustomAccess {
		// Parse custom permissions (top-level attributes)
		if attrs.Runs != nil {
			runsVal := *attrs.Runs
			if runsVal != "read" && runsVal != "plan" && runsVal != "apply" {
				c.JSON(http.StatusBadRequest, gin.H{
					"errors": []gin.H{
						{
							"status": "400",
							"title":  "Bad Request",
							"detail": "runs must be one of: read, plan, apply",
						},
					},
				})
				return
			}
			access.Runs = &runsVal
		}

		if attrs.Variables != nil {
			variablesVal := *attrs.Variables
			if variablesVal != "none" && variablesVal != "read" && variablesVal != "write" {
				c.JSON(http.StatusBadRequest, gin.H{
					"errors": []gin.H{
						{
							"status": "400",
							"title":  "Bad Request",
							"detail": "variables must be one of: none, read, write",
						},
					},
				})
				return
			}
			access.Variables = &variablesVal
		}

		if attrs.StateVersions != nil {
			stateVersionsVal := *attrs.StateVersions
			if stateVersionsVal != "none" && stateVersionsVal != "read" && stateVersionsVal != "read-outputs" && stateVersionsVal != "write" {
				c.JSON(http.StatusBadRequest, gin.H{
					"errors": []gin.H{
						{
							"status": "400",
							"title":  "Bad Request",
							"detail": "state-versions must be one of: none, read, read-outputs, write",
						},
					},
				})
				return
			}
			access.StateVersions = &stateVersionsVal
		}

		if attrs.SentinelMocks != nil {
			sentinelMocksVal := *attrs.SentinelMocks
			if sentinelMocksVal != "none" && sentinelMocksVal != "read" {
				c.JSON(http.StatusBadRequest, gin.H{
					"errors": []gin.H{
						{
							"status": "400",
							"title":  "Bad Request",
							"detail": "sentinel-mocks must be one of: none, read",
						},
					},
				})
				return
			}
			access.SentinelMocks = &sentinelMocksVal
		}

		if attrs.WorkspaceLocking != nil {
			access.WorkspaceLocking = attrs.WorkspaceLocking
		}

		if attrs.RunTasks != nil {
			access.RunTasks = attrs.RunTasks
		}

		// Clear access level when using custom permissions
		access.Access = nil
	}

	// Update access entry
	if err := h.teamRepo.UpdateWorkspaceAccess(access); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to update team workspace access",
				},
			},
		})
		return
	}

	// Reload with relationships
	updatedAccess, err := h.teamRepo.GetWorkspaceAccessByID(accessID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve updated access",
				},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatTeamWorkspaceAccessResponse(updatedAccess),
	})
}

// Delete deletes team workspace access
// DELETE /api/v2/team-workspaces/:id (TFE-compatible)
// DELETE /api/v2/workspaces/:id/relationships/team-access/:access_id (legacy)
func (h *TeamWorkspaceAccessHandlerV2) Delete(c *gin.Context) {
	// TFE-compatible: ID is directly in URL
	// Legacy: workspace ID and access ID are both in URL
	accessIDStr := c.Param("id")
	accessIDFromSecondParam := c.Param("access_id")

	var workspaceID string
	if accessIDFromSecondParam != "" {
		// Legacy route: workspace ID is first param, access ID is second param
		workspaceID = accessIDStr
		accessIDStr = accessIDFromSecondParam
	}

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

	accessID, err := uuid.Parse(accessIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid access ID format",
				},
			},
		})
		return
	}

	// Get access entry
	access, err := h.teamRepo.GetWorkspaceAccessByID(accessID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Team workspace access not found",
				},
			},
		})
		return
	}

	// Verify workspace ID matches
	if access.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Team workspace access not found",
				},
			},
		})
		return
	}

	// Get workspace to get project ID
	workspace, err := h.workspaceRepo.GetByID(access.WorkspaceID)
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

	// Get organization for authorization check
	project, err := h.projectRepo.GetByID(workspace.ProjectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve project",
				},
			},
		})
		return
	}

	org, err := h.orgRepo.GetByID(project.OrganizationID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve organization",
				},
			},
		})
		return
	}

	// Check if user has permission to manage teams (team workspace access requires team management permission)
	hasPermission, err := h.rbacService.CheckOrgManageTeams(c.Request.Context(), user.ID, org.ID)
	if err != nil || !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Only organization admins can delete team workspace access",
				},
			},
		})
		return
	}

	if err := h.teamRepo.DeleteWorkspaceAccess(accessID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to delete team workspace access",
				},
			},
		})
		return
	}

	c.Status(http.StatusNoContent)
}
