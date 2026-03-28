// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/rbac"
	"github.com/michielvha/logger"
)

type TeamHandlerV2 struct {
	teamRepo    *repository.TeamRepository
	orgRepo     *repository.OrganizationRepository
	authService *auth.Service
	rbacService *rbac.Service
}

func NewTeamHandlerV2(teamRepo *repository.TeamRepository, orgRepo *repository.OrganizationRepository, authService *auth.Service, rbacService *rbac.Service) *TeamHandlerV2 {
	return &TeamHandlerV2{
		teamRepo:    teamRepo,
		orgRepo:     orgRepo,
		authService: authService,
		rbacService: rbacService,
	}
}

type CreateTeamRequestV2 struct {
	Data struct {
		Type       string `json:"type" binding:"required"`
		Attributes struct {
			Name                       string                 `json:"name" binding:"required"`
			Description                string                 `json:"description"`
			Visibility                 string                 `json:"visibility"`
			AllowMemberTokenManagement *bool                  `json:"allow-member-token-management"`
			SSOTeamID                  *string                `json:"sso-team-id"`
			OrganizationAccess         map[string]interface{} `json:"organization-access"`
		} `json:"attributes" binding:"required"`
		Relationships struct {
			Organization struct {
				Data struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"organization"`
		} `json:"relationships,omitempty"`
	} `json:"data" binding:"required"`
}

type UpdateTeamRequestV2 struct {
	Data struct {
		Type       string `json:"type" binding:"required"`
		Attributes struct {
			Name                       string                 `json:"name"`
			Description                string                 `json:"description"`
			Visibility                 string                 `json:"visibility"`
			AllowMemberTokenManagement *bool                  `json:"allow-member-token-management"`
			SSOTeamID                  *string                `json:"sso-team-id"`
			OrganizationAccess         map[string]interface{} `json:"organization-access"`
		} `json:"attributes"`
	} `json:"data" binding:"required"`
}

// updateOrganizationAccessFromRequest updates organization access from request map,
// handling mutual exclusivity for radio button groups
func (h *TeamHandlerV2) updateOrganizationAccessFromRequest(orgAccess *models.TeamOrganizationAccess, reqAccess map[string]interface{}) {
	// Update organization access permissions from request
	if v, ok := reqAccess["manage-policies"].(bool); ok {
		orgAccess.ManagePolicies = v
	}
	if v, ok := reqAccess["manage-policy-overrides"].(bool); ok {
		orgAccess.ManagePolicyOverrides = v
	}
	if v, ok := reqAccess["manage-vcs-settings"].(bool); ok {
		orgAccess.ManageVCSSettings = v
	}
	if v, ok := reqAccess["manage-providers"].(bool); ok {
		orgAccess.ManageProviders = v
	}
	if v, ok := reqAccess["manage-modules"].(bool); ok {
		orgAccess.ManageModules = v
	}
	if v, ok := reqAccess["manage-run-tasks"].(bool); ok {
		orgAccess.ManageRunTasks = v
	}
	if v, ok := reqAccess["access-secret-teams"].(bool); ok {
		orgAccess.AccessSecretTeams = v
	}
	if v, ok := reqAccess["manage-agent-pools"].(bool); ok {
		orgAccess.ManageAgentPools = v
	}

	// Ansible permissions: manage-ansible and read-ansible are mutually exclusive parent toggles
	if v, ok := reqAccess["manage-ansible"].(bool); ok {
		orgAccess.ManageAnsible = v
		if v {
			orgAccess.ReadAnsible = false
			// Parent toggle sets all sub-permissions
			orgAccess.ManageAnsiblePlaybooks = true
			orgAccess.ReadAnsiblePlaybooks = true
			orgAccess.ManageAnsibleInventories = true
			orgAccess.ReadAnsibleInventories = true
			orgAccess.ManageAnsibleCredentials = true
			orgAccess.ReadAnsibleCredentials = true
			orgAccess.ManageAnsibleJobTemplates = true
			orgAccess.ReadAnsibleJobTemplates = true
			orgAccess.ManageAnsibleJobs = true
			orgAccess.ReadAnsibleJobs = true
			orgAccess.ManageAnsibleSchedules = true
			orgAccess.ReadAnsibleSchedules = true
		}
	}
	if v, ok := reqAccess["read-ansible"].(bool); ok {
		orgAccess.ReadAnsible = v
		if v {
			orgAccess.ManageAnsible = false
			// Parent toggle sets all read sub-permissions, clears manage
			orgAccess.ManageAnsiblePlaybooks = false
			orgAccess.ReadAnsiblePlaybooks = true
			orgAccess.ManageAnsibleInventories = false
			orgAccess.ReadAnsibleInventories = true
			orgAccess.ManageAnsibleCredentials = false
			orgAccess.ReadAnsibleCredentials = true
			orgAccess.ManageAnsibleJobTemplates = false
			orgAccess.ReadAnsibleJobTemplates = true
			orgAccess.ManageAnsibleJobs = false
			orgAccess.ReadAnsibleJobs = true
			orgAccess.ManageAnsibleSchedules = false
			orgAccess.ReadAnsibleSchedules = true
		}
	}

	// Fine-grained per-resource Ansible permissions
	// Each manage/read pair is mutually exclusive per resource type
	if v, ok := reqAccess["manage-ansible-playbooks"].(bool); ok {
		orgAccess.ManageAnsiblePlaybooks = v
		if v {
			orgAccess.ReadAnsiblePlaybooks = false
		}
	}
	if v, ok := reqAccess["read-ansible-playbooks"].(bool); ok {
		orgAccess.ReadAnsiblePlaybooks = v
		if v {
			orgAccess.ManageAnsiblePlaybooks = false
		}
	}
	if v, ok := reqAccess["manage-ansible-inventories"].(bool); ok {
		orgAccess.ManageAnsibleInventories = v
		if v {
			orgAccess.ReadAnsibleInventories = false
		}
	}
	if v, ok := reqAccess["read-ansible-inventories"].(bool); ok {
		orgAccess.ReadAnsibleInventories = v
		if v {
			orgAccess.ManageAnsibleInventories = false
		}
	}
	if v, ok := reqAccess["manage-ansible-credentials"].(bool); ok {
		orgAccess.ManageAnsibleCredentials = v
		if v {
			orgAccess.ReadAnsibleCredentials = false
		}
	}
	if v, ok := reqAccess["read-ansible-credentials"].(bool); ok {
		orgAccess.ReadAnsibleCredentials = v
		if v {
			orgAccess.ManageAnsibleCredentials = false
		}
	}
	if v, ok := reqAccess["manage-ansible-job-templates"].(bool); ok {
		orgAccess.ManageAnsibleJobTemplates = v
		if v {
			orgAccess.ReadAnsibleJobTemplates = false
		}
	}
	if v, ok := reqAccess["read-ansible-job-templates"].(bool); ok {
		orgAccess.ReadAnsibleJobTemplates = v
		if v {
			orgAccess.ManageAnsibleJobTemplates = false
		}
	}
	if v, ok := reqAccess["manage-ansible-jobs"].(bool); ok {
		orgAccess.ManageAnsibleJobs = v
		if v {
			orgAccess.ReadAnsibleJobs = false
		}
	}
	if v, ok := reqAccess["read-ansible-jobs"].(bool); ok {
		orgAccess.ReadAnsibleJobs = v
		if v {
			orgAccess.ManageAnsibleJobs = false
		}
	}
	if v, ok := reqAccess["manage-ansible-schedules"].(bool); ok {
		orgAccess.ManageAnsibleSchedules = v
		if v {
			orgAccess.ReadAnsibleSchedules = false
		}
	}
	if v, ok := reqAccess["read-ansible-schedules"].(bool); ok {
		orgAccess.ReadAnsibleSchedules = v
		if v {
			orgAccess.ManageAnsibleSchedules = false
		}
	}

	// Project permissions: manage-projects and read-projects are mutually exclusive
	if v, ok := reqAccess["manage-projects"].(bool); ok {
		orgAccess.ManageProjects = v
		if v {
			// If setting manage-projects to true, clear read-projects
			orgAccess.ReadProjects = false
		}
	}
	if v, ok := reqAccess["read-projects"].(bool); ok {
		orgAccess.ReadProjects = v
		if v {
			// If setting read-projects to true, clear manage-projects
			orgAccess.ManageProjects = false
		}
	}

	// Workspace permissions: manage-workspaces and read-workspaces are mutually exclusive
	if v, ok := reqAccess["manage-workspaces"].(bool); ok {
		orgAccess.ManageWorkspaces = v
		if v {
			// If setting manage-workspaces to true, clear read-workspaces
			orgAccess.ReadWorkspaces = false
		}
	}
	if v, ok := reqAccess["read-workspaces"].(bool); ok {
		orgAccess.ReadWorkspaces = v
		if v {
			// If setting read-workspaces to true, clear manage-workspaces
			orgAccess.ManageWorkspaces = false
		}
	}

	// Team permissions: manage-organization-access, manage-teams, and manage-membership are mutually exclusive (in order of precedence)
	if v, ok := reqAccess["manage-organization-access"].(bool); ok {
		orgAccess.ManageOrganizationAccess = v
		if v {
			// If setting manage-organization-access to true, clear others
			orgAccess.ManageTeams = false
			orgAccess.ManageMembership = false
		}
	}
	if v, ok := reqAccess["manage-teams"].(bool); ok {
		orgAccess.ManageTeams = v
		if v {
			// If setting manage-teams to true, clear manage-organization-access (but not manage-membership - it's lower precedence)
			orgAccess.ManageOrganizationAccess = false
		}
	}
	if v, ok := reqAccess["manage-membership"].(bool); ok {
		orgAccess.ManageMembership = v
		if v {
			// If setting manage-membership to true, clear higher precedence permissions
			orgAccess.ManageOrganizationAccess = false
			orgAccess.ManageTeams = false
		}
	}
}

// formatTeamResponse formats a team in TFE-compatible JSON:API format
// userID is optional - if provided, permissions will be calculated based on user's role
func formatTeamResponse(team *models.Team, orgName string, userID ...uuid.UUID) gin.H {
	visibility := team.Visibility
	if visibility == "" {
		visibility = "secret" // TFE default is "secret", not "organization"
	}

	// Format organization access (always include, even if nil)
	orgAccess := gin.H{
		"manage-policies":              false,
		"manage-policy-overrides":      false,
		"manage-workspaces":            false,
		"manage-vcs-settings":          false,
		"manage-providers":             false,
		"manage-modules":               false,
		"manage-run-tasks":             false,
		"manage-projects":              false,
		"read-workspaces":              false,
		"read-projects":                false,
		"manage-membership":            false,
		"manage-teams":                 false,
		"manage-organization-access":   false,
		"access-secret-teams":          false,
		"manage-agent-pools":           false,
		"manage-ansible":               false,
		"read-ansible":                 false,
		"manage-ansible-playbooks":     false,
		"read-ansible-playbooks":       false,
		"manage-ansible-inventories":   false,
		"read-ansible-inventories":     false,
		"manage-ansible-credentials":   false,
		"read-ansible-credentials":     false,
		"manage-ansible-job-templates": false,
		"read-ansible-job-templates":   false,
		"manage-ansible-jobs":          false,
		"read-ansible-jobs":            false,
		"manage-ansible-schedules":     false,
		"read-ansible-schedules":       false,
	}

	if team.OrganizationAccess != nil {
		orgAccess = gin.H{
			"manage-policies":              team.OrganizationAccess.ManagePolicies,
			"manage-policy-overrides":      team.OrganizationAccess.ManagePolicyOverrides,
			"manage-workspaces":            team.OrganizationAccess.ManageWorkspaces,
			"manage-vcs-settings":          team.OrganizationAccess.ManageVCSSettings,
			"manage-providers":             team.OrganizationAccess.ManageProviders,
			"manage-modules":               team.OrganizationAccess.ManageModules,
			"manage-run-tasks":             team.OrganizationAccess.ManageRunTasks,
			"manage-projects":              team.OrganizationAccess.ManageProjects,
			"read-workspaces":              team.OrganizationAccess.ReadWorkspaces,
			"read-projects":                team.OrganizationAccess.ReadProjects,
			"manage-membership":            team.OrganizationAccess.ManageMembership,
			"manage-teams":                 team.OrganizationAccess.ManageTeams,
			"manage-organization-access":   team.OrganizationAccess.ManageOrganizationAccess,
			"access-secret-teams":          team.OrganizationAccess.AccessSecretTeams,
			"manage-agent-pools":           team.OrganizationAccess.ManageAgentPools,
			"manage-ansible":               team.OrganizationAccess.ManageAnsible,
			"read-ansible":                 team.OrganizationAccess.ReadAnsible,
			"manage-ansible-playbooks":     team.OrganizationAccess.ManageAnsiblePlaybooks,
			"read-ansible-playbooks":       team.OrganizationAccess.ReadAnsiblePlaybooks,
			"manage-ansible-inventories":   team.OrganizationAccess.ManageAnsibleInventories,
			"read-ansible-inventories":     team.OrganizationAccess.ReadAnsibleInventories,
			"manage-ansible-credentials":   team.OrganizationAccess.ManageAnsibleCredentials,
			"read-ansible-credentials":     team.OrganizationAccess.ReadAnsibleCredentials,
			"manage-ansible-job-templates": team.OrganizationAccess.ManageAnsibleJobTemplates,
			"read-ansible-job-templates":   team.OrganizationAccess.ReadAnsibleJobTemplates,
			"manage-ansible-jobs":          team.OrganizationAccess.ManageAnsibleJobs,
			"read-ansible-jobs":            team.OrganizationAccess.ReadAnsibleJobs,
			"manage-ansible-schedules":     team.OrganizationAccess.ManageAnsibleSchedules,
			"read-ansible-schedules":       team.OrganizationAccess.ReadAnsibleSchedules,
		}
	}

	// Format SSO team ID (must be present, even if null)
	ssoTeamID := interface{}(nil)
	if team.SSOTeamID != nil {
		ssoTeamID = *team.SSOTeamID
	}

	// Format allow member token management (default to true if not set)
	allowTokenMgmt := team.AllowMemberTokenManagement

	// Format users relationship (TFE-compatible)
	usersData := make([]gin.H, len(team.Members))
	for i, member := range team.Members {
		usersData[i] = gin.H{
			"id":   member.UserID.String(),
			"type": "users",
		}
	}

	teamID := team.ID.String()

	// Permissions will be calculated by the handler and passed in
	// Default to no permissions (handler will override)
	permissions := gin.H{
		"can-update-membership":          false,
		"can-destroy":                    false,
		"can-update-organization-access": false,
		"can-update-api-token":           false,
		"can-update-visibility":          false,
	}

	// Format organization-memberships relationship (TFE-compatible)
	// This will be populated by the handler when include=organization-memberships is requested
	orgMembershipsData := make([]gin.H, 0)

	return gin.H{
		"id":   teamID,
		"type": "teams",
		"attributes": gin.H{
			"name":                          team.Name,
			"visibility":                    visibility,
			"users-count":                   len(team.Members),
			"allow-member-token-management": allowTokenMgmt,
			"organization-access":           orgAccess,
			"sso-team-id":                   ssoTeamID,
			"permissions":                   permissions,
		},
		"relationships": gin.H{
			"users": gin.H{
				"data": usersData,
			},
			"organization-memberships": gin.H{
				"data": orgMembershipsData,
			},
			"authentication-token": gin.H{
				"meta": gin.H{},
			},
		},
		"links": gin.H{
			"self": "/api/v2/teams/" + teamID,
		},
	}
}

// calculateTeamPermissions calculates team permissions based on user's team memberships in organization
// Roles are deprecated - all permissions now come from team memberships
func (h *TeamHandlerV2) calculateTeamPermissions(ctx context.Context, userID, orgID uuid.UUID) gin.H {
	// Default: no permissions
	permissions := gin.H{
		"can-update-membership":          false,
		"can-destroy":                    false,
		"can-update-organization-access": false,
		"can-update-api-token":           false,
		"can-update-visibility":          false,
	}

	// Check if user has permission to manage teams (using team-based permissions)
	hasPermission, err := h.rbacService.CheckOrgManageTeams(ctx, userID, orgID)
	if err == nil && hasPermission {
		permissions = gin.H{
			"can-update-membership":          true,
			"can-destroy":                    true,
			"can-update-organization-access": true,
			"can-update-api-token":           true,
			"can-update-visibility":          true,
		}
	}

	return permissions
}

// List lists teams for an organization
// GET /api/v2/organizations/:name/teams
func (h *TeamHandlerV2) List(c *gin.Context) {
	orgName := c.Param("name")

	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Organization not found",
				},
			},
		})
		return
	}

	// Parse pagination
	page, _ := strconv.Atoi(c.DefaultQuery("page[number]", "1"))
	perPage, _ := strconv.Atoi(c.DefaultQuery("page[size]", "20"))
	if perPage > 100 {
		perPage = 100
	}
	offset := (page - 1) * perPage

	teams, total, err := h.teamRepo.List(org.ID, perPage, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to list teams",
				},
			},
		})
		return
	}

	// Get current user for permission calculation
	user, err := h.authService.GetUserFromContext(c)
	var userID uuid.UUID
	if err == nil {
		userID = user.ID
	}

	// Format response
	data := make([]gin.H, len(teams))
	for i := range teams {
		// Calculate permissions for this team
		permissions := h.calculateTeamPermissions(c.Request.Context(), userID, org.ID)
		teamResp := formatTeamResponse(&teams[i], orgName)
		// Override permissions in response
		teamResp["attributes"].(gin.H)["permissions"] = permissions
		data[i] = teamResp
	}

	c.JSON(http.StatusOK, gin.H{
		"data": data,
		"meta": gin.H{
			"pagination": gin.H{
				"current-page": page,
				"page-size":    perPage,
				"prev-page":    nil,
				"next-page":    nil,
				"total-count":  total,
				"total-pages":  (int(total) + perPage - 1) / perPage,
			},
		},
	})
}

// Get returns a single team by name within an organization
// GET /api/v2/organizations/:name/teams/:name
func (h *TeamHandlerV2) Get(c *gin.Context) {
	orgName := c.Param("name")
	teamName := c.Param("teamName")

	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Organization not found",
				},
			},
		})
		return
	}

	team, err := h.teamRepo.GetByName(org.ID, teamName)
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

	// Get current user for permission calculation
	user, err := h.authService.GetUserFromContext(c)
	var userID uuid.UUID
	if err == nil {
		userID = user.ID
	}

	// Calculate permissions
	permissions := h.calculateTeamPermissions(c.Request.Context(), userID, org.ID)
	teamResp := formatTeamResponse(team, orgName)
	teamResp["attributes"].(gin.H)["permissions"] = permissions

	c.JSON(http.StatusOK, gin.H{
		"data": teamResp,
	})
}

// Create creates a new team
// POST /api/v2/organizations/:name/teams
func (h *TeamHandlerV2) Create(c *gin.Context) {
	orgName := c.Param("name")

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

	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Organization not found",
				},
			},
		})
		return
	}

	// Check if user has permission to manage teams
	hasPermission, err := h.rbacService.CheckOrgManageTeams(c.Request.Context(), user.ID, org.ID)
	if err != nil || !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Only organization admins can create teams",
				},
			},
		})
		return
	}

	var req CreateTeamRequestV2
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
	if req.Data.Type != "teams" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "data.type must be 'teams'",
				},
			},
		})
		return
	}

	attrs := req.Data.Attributes

	// Validate name
	if len(attrs.Name) == 0 || len(attrs.Name) > 255 {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Validation Error",
					"detail": "Name must be between 1 and 255 characters",
				},
			},
		})
		return
	}

	// Validate visibility (TFE default is "secret")
	visibility := attrs.Visibility
	if visibility == "" {
		visibility = "secret"
	}
	if visibility != "organization" && visibility != "secret" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Validation Error",
					"detail": "Visibility must be 'organization' or 'secret'",
				},
			},
		})
		return
	}

	// Check for duplicate name
	existing, _ := h.teamRepo.GetByName(org.ID, attrs.Name)
	if existing != nil {
		c.JSON(http.StatusConflict, gin.H{
			"errors": []gin.H{
				{
					"status": "409",
					"title":  "Conflict",
					"detail": "Team with this name already exists in this organization",
				},
			},
		})
		return
	}

	// Set allow_member_token_management (default to true)
	allowTokenMgmt := true
	if attrs.AllowMemberTokenManagement != nil {
		allowTokenMgmt = *attrs.AllowMemberTokenManagement
	}

	team := &models.Team{
		OrganizationID:             org.ID,
		Name:                       attrs.Name,
		Description:                attrs.Description,
		Visibility:                 visibility,
		AllowMemberTokenManagement: allowTokenMgmt,
		SSOTeamID:                  attrs.SSOTeamID,
	}

	if err := h.teamRepo.Create(team); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to create team",
				},
			},
		})
		return
	}

	// Prevent creating an "owners" team manually - it's created automatically by the system
	if attrs.Name == "owners" {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "The 'owners' team is created automatically by the system and cannot be created manually.",
				},
			},
		})
		return
	}

	// Create or update organization access
	if attrs.OrganizationAccess != nil {
		orgAccess, err := h.teamRepo.GetOrCreateOrganizationAccess(team.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{
					{
						"status": "500",
						"title":  "Internal Server Error",
						"detail": "Failed to create organization access",
					},
				},
			})
			return
		}

		// Update organization access permissions from request (handles mutual exclusivity)
		h.updateOrganizationAccessFromRequest(orgAccess, attrs.OrganizationAccess)

		if err := h.teamRepo.UpdateOrganizationAccess(orgAccess); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{
					{
						"status": "500",
						"title":  "Internal Server Error",
						"detail": "Failed to update organization access",
					},
				},
			})
			return
		}
	} else {
		// Create default organization access (all false)
		_, err := h.teamRepo.GetOrCreateOrganizationAccess(team.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{
					{
						"status": "500",
						"title":  "Internal Server Error",
						"detail": "Failed to create organization access",
					},
				},
			})
			return
		}
	}

	// Load team with relationships
	team, err = h.teamRepo.GetByID(team.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve created team",
				},
			},
		})
		return
	}

	// Calculate permissions (user is admin since they created the team)
	permissions := h.calculateTeamPermissions(c.Request.Context(), user.ID, org.ID)
	teamResp := formatTeamResponse(team, orgName)
	teamResp["attributes"].(gin.H)["permissions"] = permissions

	c.JSON(http.StatusCreated, gin.H{
		"data": teamResp,
	})
}

// Update updates a team
// PATCH /api/v2/organizations/:name/teams/:name
func (h *TeamHandlerV2) Update(c *gin.Context) {
	orgName := c.Param("name")
	teamName := c.Param("teamName")

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

	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Organization not found",
				},
			},
		})
		return
	}

	team, err := h.teamRepo.GetByName(org.ID, teamName)
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

	// Check if user has permission to manage teams
	hasPermission, err := h.rbacService.CheckOrgManageTeams(c.Request.Context(), user.ID, org.ID)
	if err != nil || !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Only organization admins can update teams",
				},
			},
		})
		return
	}

	var req UpdateTeamRequestV2
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
	if req.Data.Type != "teams" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "data.type must be 'teams'",
				},
			},
		})
		return
	}

	attrs := req.Data.Attributes

	// Update fields if provided
	if attrs.Name != "" {
		// Check for duplicate name if name is changing
		if attrs.Name != team.Name {
			existing, _ := h.teamRepo.GetByName(org.ID, attrs.Name)
			if existing != nil {
				c.JSON(http.StatusConflict, gin.H{
					"errors": []gin.H{
						{
							"status": "409",
							"title":  "Conflict",
							"detail": "Team with this name already exists in this organization",
						},
					},
				})
				return
			}
		}
		team.Name = attrs.Name
	}
	if attrs.Description != "" {
		team.Description = attrs.Description
	}
	if attrs.Visibility != "" {
		if attrs.Visibility != "organization" && attrs.Visibility != "secret" {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Validation Error",
						"detail": "Visibility must be 'organization' or 'secret'",
					},
				},
			})
			return
		}
		team.Visibility = attrs.Visibility
	}

	// Update allow_member_token_management if provided
	if attrs.AllowMemberTokenManagement != nil {
		team.AllowMemberTokenManagement = *attrs.AllowMemberTokenManagement
	}

	// Update sso_team_id if provided
	if attrs.SSOTeamID != nil {
		team.SSOTeamID = attrs.SSOTeamID
	}

	// Update organization access if provided
	if attrs.OrganizationAccess != nil {
		// Prevent modification of "owners" team permissions - it must always have full permissions
		if team.Name == "owners" {
			c.JSON(http.StatusForbidden, gin.H{
				"errors": []gin.H{
					{
						"status": "403",
						"title":  "Forbidden",
						"detail": "The 'owners' team permissions cannot be modified. The owners team must always have full permissions.",
					},
				},
			})
			return
		}

		orgAccess, err := h.teamRepo.GetOrCreateOrganizationAccess(team.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{
					{
						"status": "500",
						"title":  "Internal Server Error",
						"detail": "Failed to get organization access",
					},
				},
			})
			return
		}

		// Update organization access permissions from request (handles mutual exclusivity)
		h.updateOrganizationAccessFromRequest(orgAccess, attrs.OrganizationAccess)

		if err := h.teamRepo.UpdateOrganizationAccess(orgAccess); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{
					{
						"status": "500",
						"title":  "Internal Server Error",
						"detail": "Failed to update organization access",
					},
				},
			})
			return
		}
	}

	if err := h.teamRepo.Update(team); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to update team",
				},
			},
		})
		return
	}

	// Load team with relationships
	team, err = h.teamRepo.GetByID(team.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve updated team",
				},
			},
		})
		return
	}

	// Calculate permissions
	permissions := h.calculateTeamPermissions(c.Request.Context(), user.ID, org.ID)
	teamResp := formatTeamResponse(team, orgName)
	teamResp["attributes"].(gin.H)["permissions"] = permissions

	c.JSON(http.StatusOK, gin.H{
		"data": teamResp,
	})
}

// Delete deletes a team
// DELETE /api/v2/organizations/:name/teams/:name
func (h *TeamHandlerV2) Delete(c *gin.Context) {
	orgName := c.Param("name")
	teamName := c.Param("teamName")

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

	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Organization not found",
				},
			},
		})
		return
	}

	team, err := h.teamRepo.GetByName(org.ID, teamName)
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

	// Prevent deletion of "owners" and "viewers" teams - they are required system teams
	if team.Name == "owners" || team.Name == "viewers" {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": fmt.Sprintf("The '%s' team is a required system team and cannot be deleted.", team.Name),
				},
			},
		})
		return
	}

	// Check if user has permission to manage teams
	hasPermission, err := h.rbacService.CheckOrgManageTeams(c.Request.Context(), user.ID, org.ID)
	if err != nil || !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Only organization admins can delete teams",
				},
			},
		})
		return
	}

	if err := h.teamRepo.Delete(team.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to delete team",
				},
			},
		})
		return
	}

	c.Status(http.StatusNoContent)
}

// GetByID returns a single team by ID (TFE-compatible)
// GET /api/v2/teams/:id
func (h *TeamHandlerV2) GetByID(c *gin.Context) {
	teamIDStr := c.Param("id")
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

	// Get organization name for response formatting
	org, err := h.orgRepo.GetByID(team.OrganizationID)
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

	// Get current user for permission calculation
	user, err := h.authService.GetUserFromContext(c)
	var userID uuid.UUID
	if err == nil {
		userID = user.ID
	}

	// Parse include options
	includeParam := c.Query("include")
	includeUsers := false
	includeOrgMemberships := false
	if includeParam != "" {
		includes := strings.Split(includeParam, ",")
		for _, inc := range includes {
			inc = strings.TrimSpace(inc)
			switch inc {
			case "users":
				includeUsers = true
			case "organization-memberships":
				includeOrgMemberships = true
			}
		}
	}

	// Calculate permissions
	permissions := h.calculateTeamPermissions(c.Request.Context(), userID, org.ID)
	teamResp := formatTeamResponse(team, org.Name)
	teamResp["attributes"].(gin.H)["permissions"] = permissions

	// Build included resources and relationships
	included := make([]gin.H, 0)
	orgMembershipsData := make([]gin.H, 0)

	// Always get organization memberships for all team members (TFE always includes this relationship)
	// Query organization memberships directly for all team member user IDs
	// This ensures consistent ordering regardless of team member order
	userIDs := make([]uuid.UUID, 0, len(team.Members))
	for _, member := range team.Members {
		userIDs = append(userIDs, member.UserID)
	}

	var orgMemberships []models.OrganizationMember
	if len(userIDs) > 0 {
		// Get all organization memberships in one query, ordered by ID
		// This ensures consistent ordering and prevents drift
		var err error
		orgMemberships, err = h.orgRepo.GetMembersByUserIDs(team.OrganizationID, userIDs)
		if err != nil {
			// Log error but continue - relationship will be empty
			logger.Errorf("Failed to get organization memberships for team %s: %v", teamID, err)
		} else {
			// Debug: Log if we found fewer memberships than team members (data inconsistency)
			if len(orgMemberships) != len(userIDs) {
				// Find which user IDs don't have memberships
				foundUserIDs := make(map[uuid.UUID]bool)
				for _, om := range orgMemberships {
					foundUserIDs[om.UserID] = true
				}
				missingUserIDs := make([]uuid.UUID, 0)
				for _, userID := range userIDs {
					if !foundUserIDs[userID] {
						missingUserIDs = append(missingUserIDs, userID)
					}
				}
				logger.Warnf("Team %s has %d members but only %d organization memberships found. Missing memberships for user IDs: %v",
					teamID, len(userIDs), len(orgMemberships), missingUserIDs)
			}
			// Log membership IDs being returned (for debugging drift)
			membershipIDs := make([]string, len(orgMemberships))
			for i, om := range orgMemberships {
				membershipIDs[i] = om.ID.String()
			}
			logger.Debugf("Team %s returning organization membership IDs: %v", teamID, membershipIDs)
		}
	}

	// Build relationships and included in sorted order (already sorted by ID from query)
	// CRITICAL: Only include memberships that actually exist - this prevents drift
	for _, orgMember := range orgMemberships {
		// Add to relationships data (always include in relationships)
		orgMembershipsData = append(orgMembershipsData, gin.H{
			"id":   orgMember.ID.String(),
			"type": "organization-memberships",
		})
		// Add to included resources only if requested
		if includeOrgMemberships {
			membershipData := formatOrganizationMembershipResponse(&orgMember, org.Name)
			included = append(included, membershipData)
		}
	}

	// Always update relationships (TFE always includes this relationship)
	teamResp["relationships"].(gin.H)["organization-memberships"] = gin.H{
		"data": orgMembershipsData,
	}

	// If users are requested, add them to included
	if includeUsers {
		for _, teamMember := range team.Members {
			if teamMember.User.ID != uuid.Nil {
				userData := gin.H{
					"id":   teamMember.User.ID.String(),
					"type": "users",
					"attributes": gin.H{
						"username": teamMember.User.Username,
						"email":    teamMember.User.Email,
						"name":     teamMember.User.Name,
					},
				}
				included = append(included, userData)
			}
		}
	}

	response := gin.H{
		"data": teamResp,
	}

	if len(included) > 0 {
		response["included"] = included
	}

	c.JSON(http.StatusOK, response)
}

// UpdateByID updates a team by ID (TFE-compatible)
// PATCH /api/v2/teams/:id
func (h *TeamHandlerV2) UpdateByID(c *gin.Context) {
	teamIDStr := c.Param("id")
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

	// Get organization for authorization check
	org, err := h.orgRepo.GetByID(team.OrganizationID)
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

	// Check if user has permission to manage teams
	hasPermission, err := h.rbacService.CheckOrgManageTeams(c.Request.Context(), user.ID, org.ID)
	if err != nil || !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Only organization admins can update teams",
				},
			},
		})
		return
	}

	var req UpdateTeamRequestV2
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
	if req.Data.Type != "teams" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "data.type must be 'teams'",
				},
			},
		})
		return
	}

	attrs := req.Data.Attributes

	// Update fields if provided
	if attrs.Name != "" {
		// Check for duplicate name if name is changing
		if attrs.Name != team.Name {
			existing, _ := h.teamRepo.GetByName(org.ID, attrs.Name)
			if existing != nil {
				c.JSON(http.StatusConflict, gin.H{
					"errors": []gin.H{
						{
							"status": "409",
							"title":  "Conflict",
							"detail": "Team with this name already exists in this organization",
						},
					},
				})
				return
			}
		}
		team.Name = attrs.Name
	}
	if attrs.Description != "" {
		team.Description = attrs.Description
	}
	if attrs.Visibility != "" {
		if attrs.Visibility != "organization" && attrs.Visibility != "secret" {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Validation Error",
						"detail": "Visibility must be 'organization' or 'secret'",
					},
				},
			})
			return
		}
		team.Visibility = attrs.Visibility
	}

	// Update allow_member_token_management if provided
	if attrs.AllowMemberTokenManagement != nil {
		team.AllowMemberTokenManagement = *attrs.AllowMemberTokenManagement
	}

	// Update sso_team_id if provided
	if attrs.SSOTeamID != nil {
		team.SSOTeamID = attrs.SSOTeamID
	}

	// Update organization access if provided
	if attrs.OrganizationAccess != nil {
		// Prevent modification of "owners" team permissions - it must always have full permissions
		if team.Name == "owners" {
			c.JSON(http.StatusForbidden, gin.H{
				"errors": []gin.H{
					{
						"status": "403",
						"title":  "Forbidden",
						"detail": "The 'owners' team permissions cannot be modified. The owners team must always have full permissions.",
					},
				},
			})
			return
		}

		orgAccess, err := h.teamRepo.GetOrCreateOrganizationAccess(team.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{
					{
						"status": "500",
						"title":  "Internal Server Error",
						"detail": "Failed to get organization access",
					},
				},
			})
			return
		}

		// Update organization access permissions from request (handles mutual exclusivity)
		h.updateOrganizationAccessFromRequest(orgAccess, attrs.OrganizationAccess)

		if err := h.teamRepo.UpdateOrganizationAccess(orgAccess); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{
					{
						"status": "500",
						"title":  "Internal Server Error",
						"detail": "Failed to update organization access",
					},
				},
			})
			return
		}
	}

	if err := h.teamRepo.Update(team); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to update team",
				},
			},
		})
		return
	}

	// Load team with relationships
	team, err = h.teamRepo.GetByID(team.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve updated team",
				},
			},
		})
		return
	}

	// Calculate permissions
	permissions := h.calculateTeamPermissions(c.Request.Context(), user.ID, org.ID)
	teamResp := formatTeamResponse(team, org.Name)
	teamResp["attributes"].(gin.H)["permissions"] = permissions

	c.JSON(http.StatusOK, gin.H{
		"data": teamResp,
	})
}

// DeleteByID deletes a team by ID (TFE-compatible)
// DELETE /api/v2/teams/:id
func (h *TeamHandlerV2) DeleteByID(c *gin.Context) {
	teamIDStr := c.Param("id")
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

	// Get organization for authorization check
	org, err := h.orgRepo.GetByID(team.OrganizationID)
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

	// Prevent deletion of "owners" and "viewers" teams - they are required system teams
	if team.Name == "owners" || team.Name == "viewers" {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": fmt.Sprintf("The '%s' team is a required system team and cannot be deleted.", team.Name),
				},
			},
		})
		return
	}

	// Check if user has permission to manage teams
	hasPermission, err := h.rbacService.CheckOrgManageTeams(c.Request.Context(), user.ID, org.ID)
	if err != nil || !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Only organization admins can delete teams",
				},
			},
		})
		return
	}

	if err := h.teamRepo.Delete(team.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to delete team",
				},
			},
		})
		return
	}

	c.Status(http.StatusNoContent)
}
