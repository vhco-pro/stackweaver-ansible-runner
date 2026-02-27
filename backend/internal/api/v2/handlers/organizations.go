// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/api/helpers"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/activity"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/rbac"
	"github.com/michielvha/logger"
	"gorm.io/gorm"
)

type OrganizationHandlerV2 struct {
	orgRepo         *repository.OrganizationRepository
	teamRepo        *repository.TeamRepository
	projectRepo     *repository.ProjectRepository
	authService     *auth.Service
	activityService *activity.Service
	rbacService     *rbac.Service
}

func NewOrganizationHandlerV2(orgRepo *repository.OrganizationRepository, teamRepo *repository.TeamRepository, projectRepo *repository.ProjectRepository, authService *auth.Service, activityService *activity.Service, rbacService *rbac.Service) *OrganizationHandlerV2 {
	return &OrganizationHandlerV2{
		orgRepo:         orgRepo,
		teamRepo:        teamRepo,
		projectRepo:     projectRepo,
		authService:     authService,
		activityService: activityService,
		rbacService:     rbacService,
	}
}

// OrganizationAttributes contains TFE-compatible organization attributes
type OrganizationAttributes struct {
	Name                    string  `json:"name"`
	Email                   string  `json:"email"`
	Description             string  `json:"description"`
	CollaboratorAuthPolicy  string  `json:"collaborator-auth-policy"`  // password or two_factor_mandatory
	CostEstimationEnabled   *bool   `json:"cost-estimation-enabled"`   // pointer to distinguish unset from false
	DefaultTerraformVersion *string `json:"default-terraform-version"` // org-wide default terraform version
}

// CreateOrganizationRequestV2 supports both simple JSON and JSON:API format
// Simple: { "name": "...", "description": "..." }
// JSON:API: { "data": { "type": "organizations", "attributes": { "name": "...", "email": "..." } } }
type CreateOrganizationRequestV2 struct {
	// Simple format fields
	Name        string `json:"name"`
	Description string `json:"description"`

	// JSON:API format
	Data *struct {
		Type       string                 `json:"type"`
		Attributes OrganizationAttributes `json:"attributes"`
	} `json:"data"`
}

// UpdateOrganizationRequestV2 supports both simple JSON and JSON:API format
type UpdateOrganizationRequestV2 struct {
	// Simple format fields
	Name        string `json:"name"`
	Description string `json:"description"`

	// JSON:API format
	Data *struct {
		Type       string                 `json:"type"`
		Attributes OrganizationAttributes `json:"attributes"`
	} `json:"data"`
}

// buildTFEOrganizationResponse creates a TFE-compatible JSON:API response for an organization
func buildTFEOrganizationResponse(org *models.Organization) gin.H {
	// Use defaults if values are empty
	collaboratorAuthPolicy := org.CollaboratorAuthPolicy
	if collaboratorAuthPolicy == "" {
		collaboratorAuthPolicy = "password"
	}

	return gin.H{
		"id":   org.Name, // TFE uses name as ID for organizations
		"type": "organizations",
		"attributes": gin.H{
			"name":                                org.Name,
			"external-id":                         org.ID.String(),
			"created-at":                          org.CreatedAt.Format("2006-01-02T15:04:05Z"),
			"updated-at":                          org.UpdatedAt.Format("2006-01-02T15:04:05Z"),
			"email":                               org.Email,
			"session-timeout":                     nil,
			"session-remember":                    nil,
			"collaborator-auth-policy":            collaboratorAuthPolicy,
			"cost-estimation-enabled":             org.CostEstimationEnabled,
			"default-terraform-version":           org.DefaultTerraformVersion,
			"speculative-plan-management-enabled": true,
			"aggregated-commit-status-enabled":    false,
			"assessments-enforced":                false,
			"allow-force-delete-workspaces":       false,
			"send-passing-statuses-for-untriggered-speculative-plans": false,
			"permissions": gin.H{
				"can-update":                  true,
				"can-destroy":                 true,
				"can-access-via-teams":        true,
				"can-create-module":           true,
				"can-create-team":             true,
				"can-create-workspace":        true,
				"can-manage-users":            true,
				"can-manage-subscription":     false,
				"can-manage-sso":              false,
				"can-update-oauth":            true,
				"can-update-sentinel":         false,
				"can-update-ssh-keys":         true,
				"can-update-api-token":        true,
				"can-traverse":                true,
				"can-start-trial":             false,
				"can-update-agent-pools":      true,
				"can-manage-tags":             true,
				"can-manage-varsets":          true,
				"can-read-varsets":            true,
				"can-manage-public-modules":   true,
				"can-create-provider":         true,
				"can-manage-public-providers": false,
				"can-create-project":          true,
				"can-manage-assessments":      true,
				"can-read-assessments":        true,
				"can-view-explorer":           true,
				"can-deploy-no-code-modules":  false,
				"can-manage-policies":         true,
				"can-manage-policy-overrides": true,
				"can-manage-run-tasks":        true,
				"can-read-run-tasks":          true,
				"can-manage-projects":         true,
			},
		},
		"links": gin.H{
			"self": "/api/v2/organizations/" + org.Name,
		},
	}
}

// List returns all organizations that the user is a member of
// GET /api/v2/organizations
func (h *OrganizationHandlerV2) List(c *gin.Context) {
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

	// List organizations where the user has at least one team (team-based access; tenant isolation)
	orgs, err := h.orgRepo.ListByUser(user.ID)
	if err != nil {
		logger.Errorf("Failed to list organizations for user %s: %v", user.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to list organizations",
				},
			},
		})
		return
	}

	total := int64(len(orgs))

	// Apply pagination
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	perPage, _ := strconv.Atoi(c.DefaultQuery("per_page", "20"))
	if perPage > 100 {
		perPage = 100
	}
	offset := (page - 1) * perPage

	start := offset
	if start > len(orgs) {
		start = len(orgs)
	}
	end := start + perPage
	if end > len(orgs) {
		end = len(orgs)
	}

	var paginatedOrgs []models.Organization
	if start < len(orgs) {
		paginatedOrgs = orgs[start:end]
	}

	c.JSON(http.StatusOK, gin.H{
		"data": paginatedOrgs,
		"meta": gin.H{
			"pagination": gin.H{
				"page":     page,
				"per_page": perPage,
				"total":    total,
			},
		},
	})
}

// Get returns a single organization by name
// GET /api/v2/organizations/:name
// TFE-compatible JSON:API format
func (h *OrganizationHandlerV2) Get(c *gin.Context) {
	name := c.Param("name")

	org, err := h.orgRepo.GetByName(name)
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

	// TFE-compatible JSON:API response
	c.JSON(http.StatusOK, gin.H{
		"data": buildTFEOrganizationResponse(org),
	})
}

// Create creates a new organization
// POST /api/v2/organizations
func (h *OrganizationHandlerV2) Create(c *gin.Context) {
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

	var req CreateOrganizationRequestV2
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

	// Extract fields from either format (JSON:API or simple)
	name := req.Name
	description := req.Description
	email := ""
	collaboratorAuthPolicy := "password" // Default per TFE spec
	costEstimationEnabled := true        // Default per TFE spec

	if req.Data != nil {
		if req.Data.Attributes.Name != "" {
			name = req.Data.Attributes.Name
		}
		if req.Data.Attributes.Description != "" {
			description = req.Data.Attributes.Description
		}
		if req.Data.Attributes.Email != "" {
			email = req.Data.Attributes.Email
		}
		if req.Data.Attributes.CollaboratorAuthPolicy != "" {
			collaboratorAuthPolicy = req.Data.Attributes.CollaboratorAuthPolicy
		}
		if req.Data.Attributes.CostEstimationEnabled != nil {
			costEstimationEnabled = *req.Data.Attributes.CostEstimationEnabled
		}
	}

	// Validate collaborator auth policy
	if collaboratorAuthPolicy != "password" && collaboratorAuthPolicy != "two_factor_mandatory" {
		collaboratorAuthPolicy = "password"
	}

	// Validate name length
	if len(name) == 0 || len(name) > 200 {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Validation Error",
					"detail": "Name must be between 1 and 200 characters",
				},
			},
		})
		return
	}

	// Check for duplicate name
	existing, _ := h.orgRepo.GetByName(name)
	if existing != nil {
		c.JSON(http.StatusConflict, gin.H{
			"errors": []gin.H{
				{
					"status": "409",
					"title":  "Conflict",
					"detail": "Organization with this name already exists",
				},
			},
		})
		return
	}

	org := &models.Organization{
		Name:                   name,
		Description:            description,
		Email:                  email,
		CollaboratorAuthPolicy: collaboratorAuthPolicy,
		CostEstimationEnabled:  costEstimationEnabled,
	}

	if err := h.orgRepo.Create(org); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to create organization",
				},
			},
		})
		return
	}

	// Create default teams: "owners" and "viewers"
	if err := h.createDefaultTeams(org.ID); err != nil {
		logger.Errorf("Failed to create default teams for org %s: %v", org.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": fmt.Sprintf("Failed to create default teams: %v", err),
				},
			},
		})
		return
	}

	// Add creator to organization (no role - roles are deprecated)
	if err := h.orgRepo.AddMember(org.ID, user.ID); err != nil {
		logger.Errorf("Failed to add member %s to org %s: %v", user.ID, org.ID, err)

		// Check for duplicate key error (user already a member - shouldn't happen but handle gracefully)
		if err == gorm.ErrDuplicatedKey {
			// User is already a member (shouldn't happen, but handle gracefully)
			logger.Warnf("User %s is already a member of org %s", user.ID, org.ID)
			// Continue - user is already a member
			return
		}

		errStr := strings.ToLower(err.Error())
		// Check error string patterns (nolint: gocritic - error string checking requires if-else chain)
		if strings.Contains(errStr, "duplicate key") ||
			strings.Contains(errStr, "unique constraint") ||
			strings.Contains(errStr, "idx_org_user") {
			// User is already a member (shouldn't happen, but handle gracefully)
			logger.Warnf("User %s is already a member of org %s", user.ID, org.ID)
			// Continue - user is already a member
			return
		}

		if strings.Contains(errStr, "foreign key") ||
			strings.Contains(errStr, "violates foreign key constraint") {
			// Foreign key constraint error (user doesn't exist - shouldn't happen since user is authenticated)
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{
					{
						"status": "500",
						"title":  "Internal Server Error",
						"detail": "Failed to add user to organization: user record not found. Please contact support.",
					},
				},
			})
			return
		}

		// Other error
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": fmt.Sprintf("Failed to add member: %v", err),
				},
			},
		})
		return
	}

	// Add creator to "owners" team (replaces old admin role)
	ownersTeam, err := h.teamRepo.GetByName(org.ID, "owners")
	if err != nil {
		logger.Errorf("Failed to find owners team for org %s: %v", org.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": fmt.Sprintf("Failed to find owners team: %v", err),
				},
			},
		})
		return
	}
	if err := h.teamRepo.AddMember(ownersTeam.ID, user.ID); err != nil {
		logger.Errorf("Failed to add member %s to owners team %s: %v", user.ID, ownersTeam.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": fmt.Sprintf("Failed to add creator to owners team: %v", err),
				},
			},
		})
		return
	}

	// Create default project and grant owners team access
	// This ensures the creator has access to the default project via team project access
	if err := h.createDefaultProject(org.ID, ownersTeam.ID); err != nil {
		logger.Errorf("Failed to create default project for org %s: %v", org.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": fmt.Sprintf("Failed to create default project: %v", err),
				},
			},
		})
		return
	}

	// Log activity
	if h.activityService != nil {
		activityCtx := helpers.GetActivityContext(c)
		activityCtx.UserID = &user.ID
		activityCtx.OrganizationID = &org.ID
		_ = h.activityService.LogCreate(c.Request.Context(), "organization", org.ID.String(), org.Name, activityCtx)
	}

	// Return TFE-compatible JSON:API response
	c.JSON(http.StatusCreated, gin.H{
		"data": buildTFEOrganizationResponse(org),
	})
}

// Update updates an organization by name
// PATCH /api/v2/organizations/:name
func (h *OrganizationHandlerV2) Update(c *gin.Context) {
	name := c.Param("name")

	org, err := h.orgRepo.GetByName(name)
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

	var req UpdateOrganizationRequestV2
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

	// Extract fields from either format (JSON:API or simple)
	newName := req.Name
	newDescription := req.Description
	newEmail := ""
	newCollaboratorAuthPolicy := ""
	var newCostEstimationEnabled *bool
	var newDefaultTerraformVersion *string

	if req.Data != nil {
		if req.Data.Attributes.Name != "" {
			newName = req.Data.Attributes.Name
		}
		if req.Data.Attributes.Description != "" {
			newDescription = req.Data.Attributes.Description
		}
		if req.Data.Attributes.Email != "" {
			newEmail = req.Data.Attributes.Email
		}
		if req.Data.Attributes.CollaboratorAuthPolicy != "" {
			newCollaboratorAuthPolicy = req.Data.Attributes.CollaboratorAuthPolicy
		}
		newCostEstimationEnabled = req.Data.Attributes.CostEstimationEnabled
		newDefaultTerraformVersion = req.Data.Attributes.DefaultTerraformVersion
	}

	if newName != "" {
		// Check if new name conflicts with existing organization
		if newName != org.Name {
			existing, _ := h.orgRepo.GetByName(newName)
			if existing != nil {
				c.JSON(http.StatusConflict, gin.H{
					"errors": []gin.H{
						{
							"status": "409",
							"title":  "Conflict",
							"detail": "Organization with this name already exists",
						},
					},
				})
				return
			}
		}
		org.Name = newName
	}
	if newDescription != "" {
		org.Description = newDescription
	}
	if newEmail != "" {
		org.Email = newEmail
	}
	if newCollaboratorAuthPolicy != "" {
		// Validate collaborator auth policy
		if newCollaboratorAuthPolicy == "password" || newCollaboratorAuthPolicy == "two_factor_mandatory" {
			org.CollaboratorAuthPolicy = newCollaboratorAuthPolicy
		}
	}
	if newCostEstimationEnabled != nil {
		org.CostEstimationEnabled = *newCostEstimationEnabled
	}
	if newDefaultTerraformVersion != nil {
		org.DefaultTerraformVersion = *newDefaultTerraformVersion
	}

	if err := h.orgRepo.Update(org); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to update organization",
				},
			},
		})
		return
	}

	// Log activity
	if h.activityService != nil {
		user, _ := h.authService.GetUserFromContext(c)
		activityCtx := helpers.GetActivityContext(c)
		if user != nil {
			activityCtx.UserID = &user.ID
		}
		activityCtx.OrganizationID = &org.ID
		changes := map[string]interface{}{}
		if newName != "" && newName != org.Name {
			changes["name"] = newName
		}
		if newDescription != "" {
			changes["description"] = newDescription
		}
		_ = h.activityService.LogUpdate(c.Request.Context(), "organization", org.ID.String(), org.Name, changes, activityCtx)
	}

	// Return TFE-compatible JSON:API response
	c.JSON(http.StatusOK, gin.H{
		"data": buildTFEOrganizationResponse(org),
	})
}

// Delete deletes an organization by name
// DELETE /api/v2/organizations/:name
func (h *OrganizationHandlerV2) Delete(c *gin.Context) {
	name := c.Param("name")

	org, err := h.orgRepo.GetByName(name)
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

	// Check if user has permission to delete organization
	// Organization deletion requires user to be in "owners" team
	hasManageMembership, err := h.rbacService.CheckOrgManageMembership(c.Request.Context(), user.ID, org.ID)
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

	if !hasManageMembership {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "You do not have permission to delete this organization. Organization deletion requires membership in the 'owners' team.",
				},
			},
		})
		return
	}

	// Log activity before deletion
	if h.activityService != nil {
		activityCtx := helpers.GetActivityContext(c)
		activityCtx.UserID = &user.ID
		activityCtx.OrganizationID = &org.ID
		_ = h.activityService.LogDelete(c.Request.Context(), "organization", org.ID.String(), org.Name, activityCtx)
	}

	if err := h.orgRepo.Delete(org.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to delete organization",
				},
			},
		})
		return
	}

	c.Status(http.StatusNoContent)
}

// GetEntitlementSet returns the entitlement set for an organization
// GET /api/v2/organizations/:name/entitlement-set
// TFE-compatible endpoint - returns organization entitlements/features
func (h *OrganizationHandlerV2) GetEntitlementSet(c *gin.Context) {
	name := c.Param("name")

	// Verify organization exists
	org, err := h.orgRepo.GetByName(name)
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

	// TFE-compatible entitlement set response
	// This endpoint returns what features/entitlements the organization has access to
	// JSON:API format: id and type at top level, attributes contain the actual data
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"id":   org.ID.String(),
			"type": "entitlement-sets",
			"attributes": gin.H{
				"cost-estimation":         true,
				"configuration-design":    true,
				"operations":              true,
				"private-module-registry": true,
				"state-storage":           true,
				"teams":                   true,
				"vcs-integrations":        true,
				"usage-reporting":         true,
				"user-limit":              0, // 0 means unlimited
				"self-serve-billing":      false,
				"audit-logging":           true,
				"sso":                     false,
				"sentinel":                false,
				"agents":                  false,
				"policy-enforcement":      false,
			},
		},
	})
}

// createDefaultProject creates the default project and grants the owners team full access
func (h *OrganizationHandlerV2) createDefaultProject(orgID, ownersTeamID uuid.UUID) error {
	// Check if default project already exists
	existing, err := h.projectRepo.GetByOrganizationAndName(orgID, "default")
	if err == nil && existing != nil {
		// Project exists - ensure owners team has access
		_, err := h.teamRepo.GetProjectAccessByTeamAndProject(ownersTeamID, existing.ID)
		if err != nil {
			access := "admin"
			accessEntry := &models.TeamProjectAccess{
				TeamID:    ownersTeamID,
				ProjectID: existing.ID,
				Access:    &access,
			}
			if err := h.teamRepo.CreateProjectAccess(accessEntry); err != nil {
				return fmt.Errorf("failed to grant owners team access to default project: %w", err)
			}
		}
		return nil
	}

	// Create default project
	project := &models.Project{
		OrganizationID: orgID,
		Name:           "default",
		Description:    "Default project for your organization",
	}
	if err := h.projectRepo.Create(project); err != nil {
		return fmt.Errorf("failed to create default project: %w", err)
	}

	// Grant owners team full access to the default project
	access := "admin"
	accessEntry := &models.TeamProjectAccess{
		TeamID:    ownersTeamID,
		ProjectID: project.ID,
		Access:    &access,
	}
	if err := h.teamRepo.CreateProjectAccess(accessEntry); err != nil {
		return fmt.Errorf("failed to grant owners team access to default project: %w", err)
	}

	return nil
}

// createDefaultTeams creates the default "owners" and "viewers" teams for a new organization
// This function is idempotent - it will skip creating teams that already exist
func (h *OrganizationHandlerV2) createDefaultTeams(orgID uuid.UUID) error {
	// Check if "owners" team already exists
	ownersTeam, err := h.teamRepo.GetByName(orgID, "owners")
	if err != nil {
		if err != gorm.ErrRecordNotFound {
			return fmt.Errorf("failed to check for existing owners team: %w", err)
		}
		// Team doesn't exist, create it
		ownersTeam = &models.Team{
			OrganizationID:             orgID,
			Name:                       "owners",
			Description:                "Organization owners with full control",
			Visibility:                 "secret", // Only visible to owners and org creator
			AllowMemberTokenManagement: true,     // Explicitly set (has default but being explicit)
		}

		if err := h.teamRepo.Create(ownersTeam); err != nil {
			return fmt.Errorf("failed to create owners team: %w", err)
		}
	}

	// Check if organization access exists for owners team, create if not
	ownersOrgAccess, err := h.teamRepo.GetOrganizationAccess(ownersTeam.ID)
	if err != nil {
		if err != gorm.ErrRecordNotFound {
			return fmt.Errorf("failed to check for existing owners team organization access: %w", err)
		}
		// Organization access doesn't exist, create it
		ownersOrgAccess = &models.TeamOrganizationAccess{
			TeamID:                   ownersTeam.ID,
			ManagePolicies:           true,
			ManagePolicyOverrides:    true,
			ManageWorkspaces:         true,
			ManageVCSSettings:        true,
			ManageProviders:          true,
			ManageModules:            true,
			ManageRunTasks:           true,
			ManageProjects:           true,
			ReadWorkspaces:           true,
			ReadProjects:             true,
			ManageMembership:         true,
			ManageTeams:              true,
			ManageOrganizationAccess: true,
			AccessSecretTeams:        true,
			ManageAgentPools:         true,
		}
		if err := h.teamRepo.CreateOrganizationAccess(ownersOrgAccess); err != nil {
			return fmt.Errorf("failed to create owners team organization access: %w", err)
		}
	} else {
		// Update existing access to ensure all permissions are enabled (in case it was created with defaults)
		ownersOrgAccess.ManagePolicies = true
		ownersOrgAccess.ManagePolicyOverrides = true
		ownersOrgAccess.ManageWorkspaces = true
		ownersOrgAccess.ManageVCSSettings = true
		ownersOrgAccess.ManageProviders = true
		ownersOrgAccess.ManageModules = true
		ownersOrgAccess.ManageRunTasks = true
		ownersOrgAccess.ManageProjects = true
		ownersOrgAccess.ReadWorkspaces = true
		ownersOrgAccess.ReadProjects = true
		ownersOrgAccess.ManageMembership = true
		ownersOrgAccess.ManageTeams = true
		ownersOrgAccess.ManageOrganizationAccess = true
		ownersOrgAccess.AccessSecretTeams = true
		ownersOrgAccess.ManageAgentPools = true
		if err := h.teamRepo.UpdateOrganizationAccess(ownersOrgAccess); err != nil {
			return fmt.Errorf("failed to update owners team organization access: %w", err)
		}
	}

	// Check if "viewers" team already exists
	viewersTeam, err := h.teamRepo.GetByName(orgID, "viewers")
	if err != nil {
		if err != gorm.ErrRecordNotFound {
			return fmt.Errorf("failed to check for existing viewers team: %w", err)
		}
		// Team doesn't exist, create it
		viewersTeam = &models.Team{
			OrganizationID:             orgID,
			Name:                       "viewers",
			Description:                "Organization viewers with read-only access",
			Visibility:                 "organization", // Visible to everyone
			AllowMemberTokenManagement: false,          // Viewers don't need token management
		}

		if err := h.teamRepo.Create(viewersTeam); err != nil {
			return fmt.Errorf("failed to create viewers team: %w", err)
		}
	}

	// Check if organization access exists for viewers team, create if not
	viewersOrgAccess, err := h.teamRepo.GetOrganizationAccess(viewersTeam.ID)
	if err != nil {
		if err != gorm.ErrRecordNotFound {
			return fmt.Errorf("failed to check for existing viewers team organization access: %w", err)
		}
		// Organization access doesn't exist, create it
		viewersOrgAccess = &models.TeamOrganizationAccess{
			TeamID:                   viewersTeam.ID,
			ManagePolicies:           false,
			ManagePolicyOverrides:    false,
			ManageWorkspaces:         false,
			ManageVCSSettings:        false,
			ManageProviders:          false,
			ManageModules:            false,
			ManageRunTasks:           false,
			ManageProjects:           false,
			ReadWorkspaces:           true, // Can view workspaces
			ReadProjects:             true, // Can view projects
			ManageMembership:         false,
			ManageTeams:              false,
			ManageOrganizationAccess: false,
			AccessSecretTeams:        false,
			ManageAgentPools:         false,
		}
		if err := h.teamRepo.CreateOrganizationAccess(viewersOrgAccess); err != nil {
			return fmt.Errorf("failed to create viewers team organization access: %w", err)
		}
	} else {
		// Update existing access to ensure correct read-only permissions
		viewersOrgAccess.ManagePolicies = false
		viewersOrgAccess.ManagePolicyOverrides = false
		viewersOrgAccess.ManageWorkspaces = false
		viewersOrgAccess.ManageVCSSettings = false
		viewersOrgAccess.ManageProviders = false
		viewersOrgAccess.ManageModules = false
		viewersOrgAccess.ManageRunTasks = false
		viewersOrgAccess.ManageProjects = false
		viewersOrgAccess.ReadWorkspaces = true
		viewersOrgAccess.ReadProjects = true
		viewersOrgAccess.ManageMembership = false
		viewersOrgAccess.ManageTeams = false
		viewersOrgAccess.ManageOrganizationAccess = false
		viewersOrgAccess.AccessSecretTeams = false
		viewersOrgAccess.ManageAgentPools = false
		if err := h.teamRepo.UpdateOrganizationAccess(viewersOrgAccess); err != nil {
			return fmt.Errorf("failed to update viewers team organization access: %w", err)
		}
	}

	return nil
}
