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
	"github.com/michielvha/logger"
)

type TeamProjectAccessHandlerV2 struct {
	teamRepo    *repository.TeamRepository
	projectRepo *repository.ProjectRepository
	orgRepo     *repository.OrganizationRepository
	authService *auth.Service
	rbacService *rbac.Service
}

func NewTeamProjectAccessHandlerV2(
	teamRepo *repository.TeamRepository,
	projectRepo *repository.ProjectRepository,
	orgRepo *repository.OrganizationRepository,
	authService *auth.Service,
	rbacService *rbac.Service,
) *TeamProjectAccessHandlerV2 {
	return &TeamProjectAccessHandlerV2{
		teamRepo:    teamRepo,
		projectRepo: projectRepo,
		orgRepo:     orgRepo,
		authService: authService,
		rbacService: rbacService,
	}
}

type CreateTeamProjectAccessRequestV2 struct {
	Data struct {
		Type       string `json:"type" binding:"required"`
		Attributes struct {
			// Fixed access level (required: "admin", "maintain", "write", "read", "custom")
			Access *string `json:"access,omitempty"`

			// Custom project access permissions (nested block in JSON:API)
			// TFE sends these as a nested "project-access" block when using custom permissions
			ProjectAccess struct {
				Settings     *string `json:"settings,omitempty"`      // "read", "update", "delete"
				Teams        *string `json:"teams,omitempty"`         // "none", "read", "manage"
				VariableSets *string `json:"variable-sets,omitempty"` // "none", "read", "write"
			} `json:"project-access,omitempty"`

			// Custom workspace access permissions (nested block in JSON:API)
			// TFE sends these as a nested "workspace-access" block when using custom permissions
			// These apply to ALL workspaces within the project
			WorkspaceAccess struct {
				Runs          *string `json:"runs,omitempty"`           // "read", "plan", "apply"
				SentinelMocks *string `json:"sentinel-mocks,omitempty"` // "none", "read"
				StateVersions *string `json:"state-versions,omitempty"` // "none", "read-outputs", "read", "write"
				Variables     *string `json:"variables,omitempty"`      // "none", "read", "write"
				Create        *bool   `json:"create,omitempty"`         // permission to create workspaces
				Locking       *bool   `json:"locking,omitempty"`        // permission to lock/unlock workspaces
				Move          *bool   `json:"move,omitempty"`           // permission to move workspaces
				Delete        *bool   `json:"delete,omitempty"`         // permission to delete workspaces
				RunTasks      *bool   `json:"run-tasks,omitempty"`      // permission to manage run tasks
			} `json:"workspace-access,omitempty"`
		} `json:"attributes"`
		Relationships struct {
			Team struct {
				Data struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"team"`
			Project struct {
				Data struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"project,omitempty"` // TFE-compatible: project in relationships
		} `json:"relationships"`
	} `json:"data" binding:"required"`
}

type UpdateTeamProjectAccessRequestV2 struct {
	Data struct {
		Type       string `json:"type" binding:"required"`
		Attributes struct {
			// Fixed access level (optional: "admin", "maintain", "write", "read", "custom")
			Access *string `json:"access,omitempty"`

			// Custom project access permissions (nested block in JSON:API)
			// TFE sends these as a nested "project-access" block when using custom permissions
			ProjectAccess struct {
				Settings     *string `json:"settings,omitempty"`      // "read", "update", "delete"
				Teams        *string `json:"teams,omitempty"`         // "none", "read", "manage"
				VariableSets *string `json:"variable-sets,omitempty"` // "none", "read", "write"
			} `json:"project-access,omitempty"`

			// Custom workspace access permissions (nested block in JSON:API)
			// TFE sends these as a nested "workspace-access" block when using custom permissions
			// These apply to ALL workspaces within the project
			WorkspaceAccess struct {
				Runs          *string `json:"runs,omitempty"`           // "read", "plan", "apply"
				SentinelMocks *string `json:"sentinel-mocks,omitempty"` // "none", "read"
				StateVersions *string `json:"state-versions,omitempty"` // "none", "read-outputs", "read", "write"
				Variables     *string `json:"variables,omitempty"`      // "none", "read", "write"
				Create        *bool   `json:"create,omitempty"`         // permission to create workspaces
				Locking       *bool   `json:"locking,omitempty"`        // permission to lock/unlock workspaces
				Move          *bool   `json:"move,omitempty"`           // permission to move workspaces
				Delete        *bool   `json:"delete,omitempty"`         // permission to delete workspaces
				RunTasks      *bool   `json:"run-tasks,omitempty"`      // permission to manage run tasks
			} `json:"workspace-access,omitempty"`
		} `json:"attributes"`
	} `json:"data" binding:"required"`
}

// formatTeamProjectAccessResponse formats a team project access in TFE-compatible JSON:API format
// TFE uses type "team-projects" (not "team-project-accesses")
func formatTeamProjectAccessResponse(access *models.TeamProjectAccess) gin.H {
	attributes := gin.H{}

	// Check if we have custom permissions (any permission field is set)
	hasCustomPermissions := access.ProjectSettings != nil || access.ProjectTeams != nil || access.ProjectVariableSets != nil ||
		access.WorkspaceRuns != nil || access.WorkspaceSentinelMocks != nil || access.WorkspaceStateVersions != nil ||
		access.WorkspaceVariables != nil || access.WorkspaceCreate != nil || access.WorkspaceLocking != nil ||
		access.WorkspaceMove != nil || access.WorkspaceDelete != nil || access.WorkspaceRunTasks != nil

	// TFE behavior: If custom permissions are set, access should be "custom"
	// If fixed access level is set, use that
	if hasCustomPermissions {
		// Custom permissions: set access to "custom"
		attributes["access"] = "custom"

		// Add custom project access block
		projectAccess := gin.H{}
		if access.ProjectSettings != nil {
			projectAccess["settings"] = *access.ProjectSettings
		} else {
			projectAccess["settings"] = "read" // Default when not specified
		}
		if access.ProjectTeams != nil {
			projectAccess["teams"] = *access.ProjectTeams
		} else {
			projectAccess["teams"] = "none" // Default when not specified
		}
		if access.ProjectVariableSets != nil {
			projectAccess["variable-sets"] = *access.ProjectVariableSets
		} else {
			projectAccess["variable-sets"] = "none" // Default when not specified
		}
		attributes["project-access"] = projectAccess

		// Add custom workspace access block
		workspaceAccess := gin.H{}
		if access.WorkspaceRuns != nil {
			workspaceAccess["runs"] = *access.WorkspaceRuns
		} else {
			workspaceAccess["runs"] = "read" // Default when not specified
		}
		if access.WorkspaceSentinelMocks != nil {
			workspaceAccess["sentinel-mocks"] = *access.WorkspaceSentinelMocks
		} else {
			workspaceAccess["sentinel-mocks"] = "none" // Default when not specified
		}
		if access.WorkspaceStateVersions != nil {
			workspaceAccess["state-versions"] = *access.WorkspaceStateVersions
		} else {
			workspaceAccess["state-versions"] = "none" // Default when not specified
		}
		if access.WorkspaceVariables != nil {
			workspaceAccess["variables"] = *access.WorkspaceVariables
		} else {
			workspaceAccess["variables"] = "none" // Default when not specified
		}
		if access.WorkspaceCreate != nil {
			workspaceAccess["create"] = *access.WorkspaceCreate
		} else {
			workspaceAccess["create"] = false // Default when not specified
		}
		if access.WorkspaceLocking != nil {
			workspaceAccess["locking"] = *access.WorkspaceLocking
		} else {
			workspaceAccess["locking"] = false // Default when not specified
		}
		if access.WorkspaceMove != nil {
			workspaceAccess["move"] = *access.WorkspaceMove
		} else {
			workspaceAccess["move"] = false // Default when not specified
		}
		if access.WorkspaceDelete != nil {
			workspaceAccess["delete"] = *access.WorkspaceDelete
		} else {
			workspaceAccess["delete"] = false // Default when not specified
		}
		if access.WorkspaceRunTasks != nil {
			workspaceAccess["run-tasks"] = *access.WorkspaceRunTasks
		} else {
			workspaceAccess["run-tasks"] = false // Default when not specified
		}
		attributes["workspace-access"] = workspaceAccess
	} else if access.Access != nil {
		// Fixed access level: use the access value
		attributes["access"] = *access.Access
	}

	return gin.H{
		"id":         access.ID.String(),
		"type":       "team-projects", // TFE uses "team-projects" as the resource type
		"attributes": attributes,
		"relationships": gin.H{
			"team": gin.H{
				"data": gin.H{
					"id":   access.TeamID.String(),
					"type": "teams",
				},
			},
			"project": gin.H{
				"data": gin.H{
					"id":   access.ProjectID.String(),
					"type": "projects",
				},
			},
		},
		"links": gin.H{
			"self": "/api/v2/team-projects/" + access.ID.String(),
		},
	}
}

// List lists all team project accesses for a project
// GET /api/v2/projects/:id/relationships/team-access
// GET /api/v2/team-projects?filter[project][id]=:project_id
func (h *TeamProjectAccessHandlerV2) List(c *gin.Context) {
	// Support both project-scoped route and TFE-compatible route
	projectIDStr := c.Param("id")
	if projectIDStr == "" {
		// TFE route: GET /api/v2/team-projects?filter[project][id]=:project_id
		projectIDStr = c.Query("filter[project][id]")
	}

	if projectIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Project ID is required",
				},
			},
		})
		return
	}

	projectID, err := uuid.Parse(projectIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid project ID format",
				},
			},
		})
		return
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

	// Verify project exists and user has access
	project, err := h.projectRepo.GetByID(projectID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Project not found",
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

	// Check if user has permission to manage teams (team project access requires team management permission)
	hasPermission, err := h.rbacService.CheckOrgManageTeams(c.Request.Context(), user.ID, org.ID)
	if err != nil || !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Only organization admins can manage team project access",
				},
			},
		})
		return
	}

	// Get all team project accesses for this project
	accesses, err := h.teamRepo.GetProjectAccess(projectID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve team project accesses",
				},
			},
		})
		return
	}

	// Format responses
	data := make([]gin.H, len(accesses))
	for i, access := range accesses {
		data[i] = formatTeamProjectAccessResponse(&access)
	}

	c.JSON(http.StatusOK, gin.H{
		"data": data,
	})
}

// Create creates a new team project access
// POST /api/v2/projects/:id/relationships/team-access
// POST /api/v2/team-projects
func (h *TeamProjectAccessHandlerV2) Create(c *gin.Context) {
	// Support both project-scoped route and TFE-compatible route
	projectIDStr := c.Param("id")

	var req CreateTeamProjectAccessRequestV2
	if err := c.ShouldBindJSON(&req); err != nil {
		// Log the actual error for debugging
		logger.Debugf("TeamProjectAccess Create: JSON binding error: %v", err)
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

	// Debug: Log what we received
	logger.Debugf("TeamProjectAccess Create: Received request - Type: %s, Access: %v, ProjectAccess: %+v, WorkspaceAccess: %+v",
		req.Data.Type, req.Data.Attributes.Access, req.Data.Attributes.ProjectAccess, req.Data.Attributes.WorkspaceAccess)

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

	// Validate JSON:API format
	// TFE uses "team-projects" as the type
	if req.Data.Type != "team-projects" && req.Data.Type != "team-project-accesses" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "data.type must be 'team-projects' or 'team-project-accesses'",
				},
			},
		})
		return
	}

	// Get team ID from relationships
	teamIDStr := req.Data.Relationships.Team.Data.ID
	if teamIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Team ID is required in relationships.team.data.id",
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

	// Get project ID from relationships (TFE route) or URL param (legacy route)
	if projectIDStr == "" {
		// TFE route: project in relationships
		projectIDStr = req.Data.Relationships.Project.Data.ID
	}

	if projectIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Project ID is required (either in URL or relationships.project.data.id)",
				},
			},
		})
		return
	}

	projectID, err := uuid.Parse(projectIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid project ID format",
				},
			},
		})
		return
	}

	// Verify team exists
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

	// Verify project exists
	project, err := h.projectRepo.GetByID(projectID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Project not found",
				},
			},
		})
		return
	}

	// Verify team and project belong to the same organization
	if team.OrganizationID != project.OrganizationID {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Team and project must belong to the same organization",
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

	// Check if user has permission to manage teams (team project access requires team management permission)
	hasPermission, err := h.rbacService.CheckOrgManageTeams(c.Request.Context(), user.ID, org.ID)
	if err != nil || !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Only organization admins can manage team project access",
				},
			},
		})
		return
	}

	// Validate: access is required (unlike workspace access where either access OR permissions is required)
	attrs := req.Data.Attributes
	hasAccess := attrs.Access != nil && *attrs.Access != ""

	// Check if custom permissions are provided (nested blocks: project-access and workspace-access)
	// TFE sends these as nested blocks in JSON:API format
	hasProjectAccessBlock := attrs.ProjectAccess.Settings != nil || attrs.ProjectAccess.Teams != nil || attrs.ProjectAccess.VariableSets != nil
	hasWorkspaceAccessBlock := attrs.WorkspaceAccess.Runs != nil || attrs.WorkspaceAccess.SentinelMocks != nil || attrs.WorkspaceAccess.StateVersions != nil ||
		attrs.WorkspaceAccess.Variables != nil || attrs.WorkspaceAccess.Create != nil || attrs.WorkspaceAccess.Locking != nil ||
		attrs.WorkspaceAccess.Move != nil || attrs.WorkspaceAccess.Delete != nil || attrs.WorkspaceAccess.RunTasks != nil
	hasCustomPermissions := hasProjectAccessBlock || hasWorkspaceAccessBlock

	// When using custom permissions, provider sends access="custom" AND nested permission blocks
	isCustomAccess := hasAccess && *attrs.Access == "custom"

	// Debug: Log validation state
	logger.Debugf("TeamProjectAccess Create: hasAccess=%v, access=%v, hasProjectAccessBlock=%v, hasWorkspaceAccessBlock=%v, hasCustomPermissions=%v, isCustomAccess=%v",
		hasAccess, attrs.Access, hasProjectAccessBlock, hasWorkspaceAccessBlock, hasCustomPermissions, isCustomAccess)

	if !hasAccess {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "access is required",
				},
			},
		})
		return
	}

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

	// If access is "custom", both project-access and workspace-access blocks must be provided
	if isCustomAccess && (!hasProjectAccessBlock || !hasWorkspaceAccessBlock) {
		missing := []string{}
		if !hasProjectAccessBlock {
			missing = append(missing, "project-access")
		}
		if !hasWorkspaceAccessBlock {
			missing = append(missing, "workspace-access")
		}
		missingStr := ""
		for i, m := range missing {
			if i > 0 {
				missingStr += ", "
			}
			missingStr += m
		}
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "When using access='custom', both 'project-access' and 'workspace-access' blocks are required. Missing: " + missingStr,
				},
			},
		})
		return
	}

	// Build TeamProjectAccess model
	accessEntry := &models.TeamProjectAccess{
		TeamID:    teamID,
		ProjectID: projectID,
	}

	// Set access level or custom permissions
	if hasAccess && !isCustomAccess {
		access := *attrs.Access
		// Validate access level using switch statement
		switch access {
		case "admin", "maintain", "write", "read":
			accessEntry.Access = &access
		default:
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "access must be one of: admin, maintain, write, read, custom",
					},
				},
			})
			return
		}
	} else if hasCustomPermissions || isCustomAccess {
		// Custom permissions: access should be "custom" in database
		// When access="custom" is sent, we use it and process the nested permission blocks
		customAccess := "custom"
		accessEntry.Access = &customAccess

		// Note: Validation for blocks already done above, so we can proceed here

		// Parse custom project access permissions (all fields are required when using custom permissions)
		if attrs.ProjectAccess.Settings == nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "project-access.settings is required when using custom permissions",
					},
				},
			})
			return
		}
		settingsVal := *attrs.ProjectAccess.Settings
		if settingsVal != "read" && settingsVal != "update" && settingsVal != "delete" {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "project-access.settings must be one of: read, update, delete",
					},
				},
			})
			return
		}
		accessEntry.ProjectSettings = &settingsVal

		if attrs.ProjectAccess.Teams == nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "project-access.teams is required when using custom permissions",
					},
				},
			})
			return
		}
		teamsVal := *attrs.ProjectAccess.Teams
		if teamsVal != "none" && teamsVal != "read" && teamsVal != "manage" {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "project-access.teams must be one of: none, read, manage",
					},
				},
			})
			return
		}
		accessEntry.ProjectTeams = &teamsVal

		if attrs.ProjectAccess.VariableSets == nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "project-access.variable-sets is required when using custom permissions",
					},
				},
			})
			return
		}
		variableSetsVal := *attrs.ProjectAccess.VariableSets
		if variableSetsVal != "none" && variableSetsVal != "read" && variableSetsVal != "write" {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "project-access.variable-sets must be one of: none, read, write",
					},
				},
			})
			return
		}
		accessEntry.ProjectVariableSets = &variableSetsVal

		// Parse custom workspace access permissions (all fields are required when using custom permissions)
		if attrs.WorkspaceAccess.Runs == nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "workspace-access.runs is required when using custom permissions",
					},
				},
			})
			return
		}
		runsVal := *attrs.WorkspaceAccess.Runs
		if runsVal != "read" && runsVal != "plan" && runsVal != "apply" {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "workspace-access.runs must be one of: read, plan, apply",
					},
				},
			})
			return
		}
		accessEntry.WorkspaceRuns = &runsVal

		if attrs.WorkspaceAccess.SentinelMocks == nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "workspace-access.sentinel-mocks is required when using custom permissions",
					},
				},
			})
			return
		}
		sentinelMocksVal := *attrs.WorkspaceAccess.SentinelMocks
		if sentinelMocksVal != "none" && sentinelMocksVal != "read" {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "workspace-access.sentinel-mocks must be one of: none, read",
					},
				},
			})
			return
		}
		accessEntry.WorkspaceSentinelMocks = &sentinelMocksVal

		if attrs.WorkspaceAccess.StateVersions == nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "workspace-access.state-versions is required when using custom permissions",
					},
				},
			})
			return
		}
		stateVersionsVal := *attrs.WorkspaceAccess.StateVersions
		if stateVersionsVal != "none" && stateVersionsVal != "read" && stateVersionsVal != "read-outputs" && stateVersionsVal != "write" {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "workspace-access.state-versions must be one of: none, read-outputs, read, write",
					},
				},
			})
			return
		}
		accessEntry.WorkspaceStateVersions = &stateVersionsVal

		if attrs.WorkspaceAccess.Variables == nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "workspace-access.variables is required when using custom permissions",
					},
				},
			})
			return
		}
		variablesVal := *attrs.WorkspaceAccess.Variables
		if variablesVal != "none" && variablesVal != "read" && variablesVal != "write" {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "workspace-access.variables must be one of: none, read, write",
					},
				},
			})
			return
		}
		accessEntry.WorkspaceVariables = &variablesVal

		// Boolean permissions (optional in request, default to false if not provided)
		// TFE allows these to be omitted, defaulting to false
		if attrs.WorkspaceAccess.Create != nil {
			accessEntry.WorkspaceCreate = attrs.WorkspaceAccess.Create
		} else {
			defaultFalse := false
			accessEntry.WorkspaceCreate = &defaultFalse
		}

		if attrs.WorkspaceAccess.Locking != nil {
			accessEntry.WorkspaceLocking = attrs.WorkspaceAccess.Locking
		} else {
			defaultFalse := false
			accessEntry.WorkspaceLocking = &defaultFalse
		}

		if attrs.WorkspaceAccess.Move != nil {
			accessEntry.WorkspaceMove = attrs.WorkspaceAccess.Move
		} else {
			defaultFalse := false
			accessEntry.WorkspaceMove = &defaultFalse
		}

		if attrs.WorkspaceAccess.Delete != nil {
			accessEntry.WorkspaceDelete = attrs.WorkspaceAccess.Delete
		} else {
			defaultFalse := false
			accessEntry.WorkspaceDelete = &defaultFalse
		}

		if attrs.WorkspaceAccess.RunTasks != nil {
			accessEntry.WorkspaceRunTasks = attrs.WorkspaceAccess.RunTasks
		} else {
			defaultFalse := false
			accessEntry.WorkspaceRunTasks = &defaultFalse
		}
	}

	// Check if access already exists
	existingAccesses, _ := h.teamRepo.GetProjectAccess(projectID)
	for _, existing := range existingAccesses {
		if existing.TeamID == teamID {
			c.JSON(http.StatusConflict, gin.H{
				"errors": []gin.H{
					{
						"status": "409",
						"title":  "Conflict",
						"detail": "Team already has access to this project",
					},
				},
			})
			return
		}
	}

	// Create access entry
	if err := h.teamRepo.CreateProjectAccess(accessEntry); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to create team project access",
				},
			},
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"data": formatTeamProjectAccessResponse(accessEntry),
	})
}

// Get retrieves a team project access by ID
// GET /api/v2/team-projects/:id
// Alias for GetByID to match route expectations
func (h *TeamProjectAccessHandlerV2) Get(c *gin.Context) {
	h.GetByID(c)
}

// GetByID retrieves a team project access by ID
// GET /api/v2/team-projects/:id
func (h *TeamProjectAccessHandlerV2) GetByID(c *gin.Context) {
	accessIDStr := c.Param("id")

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
	access, err := h.teamRepo.GetProjectAccessByID(accessID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Team project access not found",
				},
			},
		})
		return
	}

	// Verify project exists and user has access
	project, err := h.projectRepo.GetByID(access.ProjectID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Project not found",
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

	// Check if user has permission to manage teams (team project access requires team management permission)
	hasPermission, err := h.rbacService.CheckOrgManageTeams(c.Request.Context(), user.ID, org.ID)
	if err != nil || !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Only organization admins can manage team project access",
				},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatTeamProjectAccessResponse(access),
	})
}

// Update updates team project access
// PATCH /api/v2/team-projects/:id
// Alias for UpdateByID to match route expectations
func (h *TeamProjectAccessHandlerV2) Update(c *gin.Context) {
	h.UpdateByID(c)
}

// UpdateByID updates team project access
// PATCH /api/v2/projects/:id/relationships/team-access/:access_id
// PATCH /api/v2/team-projects/:id
func (h *TeamProjectAccessHandlerV2) UpdateByID(c *gin.Context) {
	accessIDStr := c.Param("id")

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

	// Get existing access
	access, err := h.teamRepo.GetProjectAccessByID(accessID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Team project access not found",
				},
			},
		})
		return
	}

	// Verify project exists and user has access
	project, err := h.projectRepo.GetByID(access.ProjectID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Project not found",
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

	// Check if user has permission to manage teams (team project access requires team management permission)
	hasPermission, err := h.rbacService.CheckOrgManageTeams(c.Request.Context(), user.ID, org.ID)
	if err != nil || !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Only organization admins can manage team project access",
				},
			},
		})
		return
	}

	var req UpdateTeamProjectAccessRequestV2
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
	if req.Data.Type != "team-projects" && req.Data.Type != "team-project-accesses" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "data.type must be 'team-projects' or 'team-project-accesses'",
				},
			},
		})
		return
	}

	// Validate: either access OR custom permissions, not both (unless access is "custom")
	attrs := req.Data.Attributes
	hasAccess := attrs.Access != nil && *attrs.Access != ""

	// Check if custom permissions are provided (nested blocks: project-access and workspace-access)
	// TFE sends these as nested blocks in JSON:API format
	hasProjectAccessBlock := attrs.ProjectAccess.Settings != nil || attrs.ProjectAccess.Teams != nil || attrs.ProjectAccess.VariableSets != nil
	hasWorkspaceAccessBlock := attrs.WorkspaceAccess.Runs != nil || attrs.WorkspaceAccess.SentinelMocks != nil || attrs.WorkspaceAccess.StateVersions != nil ||
		attrs.WorkspaceAccess.Variables != nil || attrs.WorkspaceAccess.Create != nil || attrs.WorkspaceAccess.Locking != nil ||
		attrs.WorkspaceAccess.Move != nil || attrs.WorkspaceAccess.Delete != nil || attrs.WorkspaceAccess.RunTasks != nil
	hasCustomPermissions := hasProjectAccessBlock || hasWorkspaceAccessBlock

	// When using custom permissions, provider sends access="custom" AND nested permission blocks
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
		// Validate access level using switch statement
		switch accessVal {
		case "admin", "maintain", "write", "read":
			// Clear custom permissions and set access
			access.Access = &accessVal
		default:
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "access must be one of: admin, maintain, write, read, custom",
					},
				},
			})
			return
		}
		access.ProjectSettings = nil
		access.ProjectTeams = nil
		access.ProjectVariableSets = nil
		access.WorkspaceRuns = nil
		access.WorkspaceSentinelMocks = nil
		access.WorkspaceStateVersions = nil
		access.WorkspaceVariables = nil
		access.WorkspaceCreate = nil
		access.WorkspaceLocking = nil
		access.WorkspaceMove = nil
		access.WorkspaceDelete = nil
		access.WorkspaceRunTasks = nil
	} else if hasCustomPermissions || isCustomAccess {
		// Custom permissions: access should be "custom" in database
		customAccess := "custom"
		access.Access = &customAccess

		// Validate that both project-access and workspace-access blocks are provided when using custom permissions
		if !hasProjectAccessBlock || !hasWorkspaceAccessBlock {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "Both 'project-access' and 'workspace-access' blocks are required when using custom permissions",
					},
				},
			})
			return
		}

		// Parse custom project access permissions (from nested project-access block)
		if attrs.ProjectAccess.Settings != nil {
			settingsVal := *attrs.ProjectAccess.Settings
			switch settingsVal {
			case "read", "update", "delete":
				access.ProjectSettings = &settingsVal
			default:
				c.JSON(http.StatusBadRequest, gin.H{
					"errors": []gin.H{
						{
							"status": "400",
							"title":  "Bad Request",
							"detail": "project-access.settings must be one of: read, update, delete",
						},
					},
				})
				return
			}
		}

		if attrs.ProjectAccess.Teams != nil {
			teamsVal := *attrs.ProjectAccess.Teams
			switch teamsVal {
			case "none", "read", "manage":
				access.ProjectTeams = &teamsVal
			default:
				c.JSON(http.StatusBadRequest, gin.H{
					"errors": []gin.H{
						{
							"status": "400",
							"title":  "Bad Request",
							"detail": "project-access.teams must be one of: none, read, manage",
						},
					},
				})
				return
			}
		}

		if attrs.ProjectAccess.VariableSets != nil {
			variableSetsVal := *attrs.ProjectAccess.VariableSets
			switch variableSetsVal {
			case "none", "read", "write":
				access.ProjectVariableSets = &variableSetsVal
			default:
				c.JSON(http.StatusBadRequest, gin.H{
					"errors": []gin.H{
						{
							"status": "400",
							"title":  "Bad Request",
							"detail": "project-access.variable-sets must be one of: none, read, write",
						},
					},
				})
				return
			}
		}

		// Parse custom workspace access permissions (from nested workspace-access block)
		if attrs.WorkspaceAccess.Runs != nil {
			runsVal := *attrs.WorkspaceAccess.Runs
			switch runsVal {
			case "read", "plan", "apply":
				access.WorkspaceRuns = &runsVal
			default:
				c.JSON(http.StatusBadRequest, gin.H{
					"errors": []gin.H{
						{
							"status": "400",
							"title":  "Bad Request",
							"detail": "workspace-access.runs must be one of: read, plan, apply",
						},
					},
				})
				return
			}
		}

		if attrs.WorkspaceAccess.SentinelMocks != nil {
			sentinelMocksVal := *attrs.WorkspaceAccess.SentinelMocks
			switch sentinelMocksVal {
			case "none", "read":
				access.WorkspaceSentinelMocks = &sentinelMocksVal
			default:
				c.JSON(http.StatusBadRequest, gin.H{
					"errors": []gin.H{
						{
							"status": "400",
							"title":  "Bad Request",
							"detail": "workspace-access.sentinel-mocks must be one of: none, read",
						},
					},
				})
				return
			}
		}

		if attrs.WorkspaceAccess.StateVersions != nil {
			stateVersionsVal := *attrs.WorkspaceAccess.StateVersions
			switch stateVersionsVal {
			case "none", "read", "read-outputs", "write":
				access.WorkspaceStateVersions = &stateVersionsVal
			default:
				c.JSON(http.StatusBadRequest, gin.H{
					"errors": []gin.H{
						{
							"status": "400",
							"title":  "Bad Request",
							"detail": "workspace-access.state-versions must be one of: none, read-outputs, read, write",
						},
					},
				})
				return
			}
		}

		if attrs.WorkspaceAccess.Variables != nil {
			variablesVal := *attrs.WorkspaceAccess.Variables
			switch variablesVal {
			case "none", "read", "write":
				access.WorkspaceVariables = &variablesVal
			default:
				c.JSON(http.StatusBadRequest, gin.H{
					"errors": []gin.H{
						{
							"status": "400",
							"title":  "Bad Request",
							"detail": "workspace-access.variables must be one of: none, read, write",
						},
					},
				})
				return
			}
		}

		if attrs.WorkspaceAccess.Create != nil {
			access.WorkspaceCreate = attrs.WorkspaceAccess.Create
		}

		if attrs.WorkspaceAccess.Locking != nil {
			access.WorkspaceLocking = attrs.WorkspaceAccess.Locking
		}

		if attrs.WorkspaceAccess.Move != nil {
			access.WorkspaceMove = attrs.WorkspaceAccess.Move
		}

		if attrs.WorkspaceAccess.Delete != nil {
			access.WorkspaceDelete = attrs.WorkspaceAccess.Delete
		}

		if attrs.WorkspaceAccess.RunTasks != nil {
			access.WorkspaceRunTasks = attrs.WorkspaceAccess.RunTasks
		}
	}

	// Update access entry
	if err := h.teamRepo.UpdateProjectAccess(access); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to update team project access",
				},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatTeamProjectAccessResponse(access),
	})
}

// Delete removes team project access
// DELETE /api/v2/team-projects/:id
// Alias for DeleteByID to match route expectations
func (h *TeamProjectAccessHandlerV2) Delete(c *gin.Context) {
	h.DeleteByID(c)
}

// DeleteByID removes team project access
// DELETE /api/v2/projects/:id/relationships/team-access/:access_id
// DELETE /api/v2/team-projects/:id
func (h *TeamProjectAccessHandlerV2) DeleteByID(c *gin.Context) {
	accessIDStr := c.Param("id")

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

	// Get existing access
	access, err := h.teamRepo.GetProjectAccessByID(accessID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Team project access not found",
				},
			},
		})
		return
	}

	// Verify project exists and user has access
	project, err := h.projectRepo.GetByID(access.ProjectID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Project not found",
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

	// Check if user has permission to manage teams (team project access requires team management permission)
	hasPermission, err := h.rbacService.CheckOrgManageTeams(c.Request.Context(), user.ID, org.ID)
	if err != nil || !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Only organization admins can manage team project access",
				},
			},
		})
		return
	}

	// Delete access entry
	if err := h.teamRepo.DeleteProjectAccess(accessID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to delete team project access",
				},
			},
		})
		return
	}

	c.Status(http.StatusNoContent)
}
