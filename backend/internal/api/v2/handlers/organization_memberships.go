// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"bytes"
	"fmt"
	"io"
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
	"gorm.io/gorm"
)

type OrganizationMembershipHandlerV2 struct {
	orgRepo     *repository.OrganizationRepository
	userRepo    *repository.UserRepository
	teamRepo    *repository.TeamRepository
	authService *auth.Service
	rbacService *rbac.Service
}

func NewOrganizationMembershipHandlerV2(
	orgRepo *repository.OrganizationRepository,
	userRepo *repository.UserRepository,
	teamRepo *repository.TeamRepository,
	authService *auth.Service,
	rbacService *rbac.Service,
) *OrganizationMembershipHandlerV2 {
	return &OrganizationMembershipHandlerV2{
		orgRepo:     orgRepo,
		userRepo:    userRepo,
		teamRepo:    teamRepo,
		authService: authService,
		rbacService: rbacService,
	}
}

type CreateOrganizationMembershipRequestV2 struct {
	Data struct {
		Type       string `json:"type" binding:"required"`
		Attributes struct {
			Email string `json:"email" binding:"required"`
		} `json:"attributes"`
		Relationships struct {
			Teams struct {
				Data []struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"teams,omitempty"`
		} `json:"relationships,omitempty"`
	} `json:"data" binding:"required"`
}

// List lists all organization memberships for an organization
// GET /api/v2/organizations/:name/organization-memberships
func (h *OrganizationMembershipHandlerV2) List(c *gin.Context) {
	organizationName := c.Param("name")
	logger.Debugf("OrganizationMembershipHandlerV2.List - Request for organization: %s", organizationName)

	// Get organization
	org, err := h.orgRepo.GetByName(organizationName)
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

	// Parse pagination options
	page, _ := strconv.Atoi(c.DefaultQuery("page[number]", "1"))
	perPage, _ := strconv.Atoi(c.DefaultQuery("page[size]", "20"))
	if perPage > 100 {
		perPage = 100
	}
	if perPage < 1 {
		perPage = 20
	}
	offset := (page - 1) * perPage

	// Parse filter options
	var emails []string
	if emailFilter := c.QueryArray("filter[email]"); len(emailFilter) > 0 {
		emails = emailFilter
	}

	var status string
	if statusFilter := c.Query("filter[status]"); statusFilter != "" {
		status = statusFilter
	}

	// Parse search query
	query := c.Query("q")

	// Check if user has permission to list organization memberships (requires manage-membership permission)
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

	hasPermission, err := h.rbacService.CheckOrgManageMembership(c.Request.Context(), user.ID, org.ID)
	if err != nil || !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "You do not have permission to list organization memberships. This requires manage-membership permission via team membership (e.g., being in the 'owners' team).",
				},
			},
		})
		return
	}

	// Note: Teams are always included in the response (JSON:API pattern)
	// List members
	members, total, err := h.orgRepo.ListMembers(org.ID, perPage, offset, emails, status, query)
	if err != nil {
		logger.Errorf("OrganizationMembershipHandlerV2.List - Failed to list members: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to list organization memberships",
				},
			},
		})
		return
	}
	logger.Debugf("OrganizationMembershipHandlerV2.List - Found %d members (total: %d)", len(members), total)

	// Always return JSON:API format (no simple format handling)
	// Always include user data in included array for frontend
	data := make([]gin.H, 0, len(members))
	included := make([]gin.H, 0)
	seenUserIDs := make(map[uuid.UUID]bool)
	seenTeamIDs := make(map[uuid.UUID]bool)

	for _, member := range members {
		membershipData := formatOrganizationMembershipResponse(&member, org.Name)

		// Always include user data in included array (JSON:API pattern for frontend)
		// Include user data even if email/name are empty (for admin users created before auth)
		if member.User.ID != uuid.Nil && !seenUserIDs[member.User.ID] {
			seenUserIDs[member.User.ID] = true
			userData := formatOrganizationMembershipUserResponse(&member.User)
			included = append(included, userData)
		}

		// Fetch teams for this user in this organization
		teams, err := h.teamRepo.GetTeamsByUserID(member.User.ID, org.ID)
		if err != nil {
			logger.Warnf("OrganizationMembershipHandlerV2.List - Failed to fetch teams for user %s: %v", member.User.ID, err)
			// Continue with empty teams array
			teams = []models.Team{}
		}

		// Build teams relationship data
		teamsData := make([]gin.H, 0, len(teams))
		for _, team := range teams {
			teamsData = append(teamsData, gin.H{
				"id":   team.ID.String(),
				"type": "teams",
			})

			// Include team data in included array if not already included
			if !seenTeamIDs[team.ID] {
				seenTeamIDs[team.ID] = true
				teamData := gin.H{
					"id":   team.ID.String(),
					"type": "teams",
					"attributes": gin.H{
						"name":        team.Name,
						"description": team.Description,
						"visibility":  team.Visibility,
					},
				}
				included = append(included, teamData)
			}
		}

		// Update teams relationship with actual data
		membershipData["relationships"].(gin.H)["teams"] = gin.H{
			"data": teamsData,
		}

		data = append(data, membershipData)
	}

	response := gin.H{
		"data": data,
		"meta": gin.H{
			"pagination": gin.H{
				"current-page": page,
				"page-size":    perPage,
				"prev-page":    nil,
				"next-page":    nil,
				"total-pages":  (int(total) + perPage - 1) / perPage,
				"total-count":  total,
			},
		},
	}

	if len(included) > 0 {
		response["included"] = included
	}

	// Set prev-page and next-page
	if page > 1 {
		response["meta"].(gin.H)["pagination"].(gin.H)["prev-page"] = page - 1
	}
	if offset+perPage < int(total) {
		response["meta"].(gin.H)["pagination"].(gin.H)["next-page"] = page + 1
	}

	logger.Debugf("OrganizationMembershipHandlerV2.List - Returning %d memberships with %d included users", len(data), len(included))
	c.JSON(http.StatusOK, response)
}

// Create creates a new organization membership
// POST /api/v2/organizations/:name/organization-memberships
func (h *OrganizationMembershipHandlerV2) Create(c *gin.Context) {
	logger.Debugf("OrganizationMembership Create - Handler called, path: %s, method: %s", c.Request.URL.Path, c.Request.Method)
	organizationName := c.Param("name")
	logger.Debugf("OrganizationMembership Create - Organization name from param: %s", organizationName)

	// Get organization
	org, err := h.orgRepo.GetByName(organizationName)
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

	// Get current user
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

	// Check if user has permission to manage organization memberships
	hasPermission, err := h.rbacService.CheckOrgManageMembership(c.Request.Context(), user.ID, org.ID)
	if err != nil || !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Only organization admins can create organization memberships",
				},
			},
		})
		return
	}

	// Debug: Log the raw request body
	bodyBytes, _ := io.ReadAll(c.Request.Body)
	c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
	logger.Debugf("OrganizationMembership Create - Raw request body: %s", string(bodyBytes))

	var req CreateOrganizationMembershipRequestV2
	if err := c.ShouldBindJSON(&req); err != nil {
		logger.Debugf("OrganizationMembership Create - JSON binding error: %v", err)
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

	logger.Debugf("OrganizationMembership Create - Parsed request: type=%s, email=%s", req.Data.Type, req.Data.Attributes.Email)

	// Validate type
	if req.Data.Type != "organization-memberships" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid type, expected 'organization-memberships'",
				},
			},
		})
		return
	}

	// Validate email
	email := req.Data.Attributes.Email
	if email == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Email is required",
				},
			},
		})
		return
	}

	// Check for duplicate email in organization (case-insensitive) before creating user
	// This prevents creating duplicate placeholder users for the same email
	allMembers, _, err := h.orgRepo.ListMembers(org.ID, 1000, 0, nil, "", "")
	if err == nil {
		for _, member := range allMembers {
			if member.User.Email != "" && strings.EqualFold(member.User.Email, email) {
				// Found existing membership with this email (case-insensitive match)
				c.JSON(http.StatusConflict, gin.H{
					"errors": []gin.H{
						{
							"status": "409",
							"title":  "Conflict",
							"detail": fmt.Sprintf("User with email '%s' is already a member of this organization", member.User.Email),
						},
					},
				})
				return
			}
		}
	}

	// Get user by email - try exact match first, then case-insensitive
	targetUser, err := h.userRepo.GetByEmail(email)
	switch err {
	case nil:
		logger.Debugf("OrganizationMembership Create - Found user with exact email match: %s", email)
	case gorm.ErrRecordNotFound:
		// Try case-insensitive lookup as fallback
		targetUser, err = h.userRepo.GetByEmailCaseInsensitive(email)
		switch err {
		case nil:
			logger.Debugf("OrganizationMembership Create - Found user with case-insensitive email match: %s", email)
		case gorm.ErrRecordNotFound:
			// TFE behavior: Create a placeholder user for invited memberships
			// The user will be properly populated when they log in via Zitadel
			// Note: We already checked for duplicate email in organization above, so it's safe to create
			logger.Debugf("OrganizationMembership Create - User not found, creating placeholder user for email: %s", email)

			// Create a placeholder user with just the email
			// Generate a temporary ZitadelSubject that we can identify as "invited"
			// Format: "invited-{uuid}" - this will be replaced when they log in
			tempSubject := fmt.Sprintf("invited-%s", uuid.New().String())
			placeholderUser := &models.User{
				Email:          email,
				ZitadelSubject: tempSubject, // Temporary subject, will be updated on first login
			}

			if err := h.userRepo.Create(placeholderUser); err != nil {
				logger.Debugf("OrganizationMembership Create - Failed to create placeholder user: %v", err)
				// Check if error is due to duplicate email (unique constraint violation)
				// This can happen in a race condition where another request creates the user at the same time
				if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique constraint") || strings.Contains(err.Error(), "UNIQUE constraint") {
					// Email already exists (race condition), try to find it again with case-insensitive lookup
					targetUser, err = h.userRepo.GetByEmailCaseInsensitive(email)
					if err != nil {
						c.JSON(http.StatusConflict, gin.H{
							"errors": []gin.H{
								{
									"status": "409",
									"title":  "Conflict",
									"detail": fmt.Sprintf("User with email '%s' already exists. Please try again.", email),
								},
							},
						})
						return
					}
					logger.Debugf("OrganizationMembership Create - Found existing user after duplicate error (race condition): %s", email)
				} else {
					c.JSON(http.StatusInternalServerError, gin.H{
						"errors": []gin.H{
							{
								"status": "500",
								"title":  "Internal Server Error",
								"detail": fmt.Sprintf("Failed to create user for email '%s'", email),
							},
						},
					})
					return
				}
			} else {
				targetUser = placeholderUser
				logger.Debugf("OrganizationMembership Create - Created placeholder user with ID: %s", targetUser.ID.String())
			}
		default:
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{
					{
						"status": "500",
						"title":  "Internal Server Error",
						"detail": "Failed to find or create user",
					},
				},
			})
			return
		}
	default:
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to find or create user",
				},
			},
		})
		return
	}

	// Check if membership already exists
	existingMember, _ := h.orgRepo.GetMember(org.ID, targetUser.ID)
	if existingMember != nil {
		c.JSON(http.StatusConflict, gin.H{
			"errors": []gin.H{
				{
					"status": "409",
					"title":  "Conflict",
					"detail": "User is already a member of this organization",
				},
			},
		})
		return
	}

	// Create membership (no role - roles are deprecated, permissions come from team memberships)
	if err := h.orgRepo.AddMember(org.ID, targetUser.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to create organization membership",
				},
			},
		})
		return
	}

	// Get the created membership
	createdMember, err := h.orgRepo.GetMember(org.ID, targetUser.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve created membership",
				},
			},
		})
		return
	}

	// Load user and organization for response
	createdMember.User = *targetUser
	createdMember.Organization = *org

	// Handle team assignments if provided
	// StackWeaver doesn't support team memberships via organization membership creation yet,
	// but we'll handle the request gracefully. Teams can be managed separately via team members API.
	_ = len(req.Data.Relationships.Teams.Data) // Explicitly ignore for now

	c.JSON(http.StatusCreated, gin.H{
		"data": formatOrganizationMembershipResponse(createdMember, org.Name),
	})
}

// GetByID retrieves an organization membership by ID
// GET /api/v2/organization-memberships/:id
func (h *OrganizationMembershipHandlerV2) GetByID(c *gin.Context) {
	memberIDStr := c.Param("id")

	memberID, err := uuid.Parse(memberIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid membership ID format",
				},
			},
		})
		return
	}

	// Note: Teams are always included in the response (JSON:API pattern)
	// Get membership
	member, err := h.orgRepo.GetMemberByID(memberID)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"errors": []gin.H{
					{
						"status": "404",
						"title":  "Not Found",
						"detail": "Organization membership not found",
					},
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve organization membership",
				},
			},
		})
		return
	}

	// Get organization name for response formatting
	org, err := h.orgRepo.GetByID(member.OrganizationID)
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

	// Format response - always include user data in included array (JSON:API pattern)
	membershipData := formatOrganizationMembershipResponse(member, org.Name)
	included := make([]gin.H, 0)

	if member.User.ID != uuid.Nil {
		userData := formatOrganizationMembershipUserResponse(&member.User)
		included = append(included, userData)
	}

	// Fetch teams for this user in this organization
	teams, err := h.teamRepo.GetTeamsByUserID(member.User.ID, org.ID)
	if err != nil {
		logger.Warnf("OrganizationMembershipHandlerV2.GetByID - Failed to fetch teams for user %s: %v", member.User.ID, err)
		// Continue with empty teams array
		teams = []models.Team{}
	}

	// Build teams relationship data
	teamsData := make([]gin.H, 0, len(teams))
	seenTeamIDs := make(map[uuid.UUID]bool)
	for _, team := range teams {
		teamsData = append(teamsData, gin.H{
			"id":   team.ID.String(),
			"type": "teams",
		})

		// Include team data in included array if not already included
		if !seenTeamIDs[team.ID] {
			seenTeamIDs[team.ID] = true
			teamData := gin.H{
				"id":   team.ID.String(),
				"type": "teams",
				"attributes": gin.H{
					"name":        team.Name,
					"description": team.Description,
					"visibility":  team.Visibility,
				},
			}
			included = append(included, teamData)
		}
	}

	// Update teams relationship with actual data
	membershipData["relationships"].(gin.H)["teams"] = gin.H{
		"data": teamsData,
	}

	response := gin.H{
		"data": membershipData,
	}

	if len(included) > 0 {
		response["included"] = included
	}

	c.JSON(http.StatusOK, response)
}

// Update updates an organization membership role by ID
// PATCH /api/v2/organization-memberships/:id
func (h *OrganizationMembershipHandlerV2) Update(c *gin.Context) {
	memberIDStr := c.Param("id")

	memberID, err := uuid.Parse(memberIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid membership ID format",
				},
			},
		})
		return
	}

	// Parse JSON:API request
	var req struct {
		Data struct {
			Type       string `json:"type" binding:"required"`
			ID         string `json:"id" binding:"required"`
			Attributes struct {
				Role string `json:"role"`
			} `json:"attributes"`
		} `json:"data" binding:"required"`
	}

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

	// Validate type
	if req.Data.Type != "organization-memberships" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid type, expected 'organization-memberships'",
				},
			},
		})
		return
	}

	// Validate ID matches
	if req.Data.ID != memberIDStr {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "ID in request body does not match URL parameter",
				},
			},
		})
		return
	}

	// Check if membership exists
	membershipToUpdate, err := h.orgRepo.GetMemberByID(memberID)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"errors": []gin.H{
					{
						"status": "404",
						"title":  "Not Found",
						"detail": "Organization membership not found",
					},
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve organization membership",
				},
			},
		})
		return
	}

	// Get current user
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

	// Get organization to check permissions
	org, err := h.orgRepo.GetByID(membershipToUpdate.OrganizationID)
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

	// Check if user has permission to manage organization memberships (must be in "owners" team)
	hasPermission, err := h.rbacService.CheckOrgManageMembership(c.Request.Context(), user.ID, org.ID)
	if err != nil || !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Only members of the 'owners' team can manage organization memberships",
				},
			},
		})
		return
	}

	// Roles are deprecated - organization membership updates are no-ops
	// Permissions are now managed via team memberships instead
	// For TFE compatibility, we accept the request but don't update roles
	// If role is provided in request, we ignore it (roles are deprecated)
	if req.Data.Attributes.Role != "" {
		// Log deprecation warning (roles are ignored)
		// In production, you might want to log this to monitoring system
		_ = req.Data.Attributes.Role // Explicitly ignore role field
	}

	// No role update needed - roles are deprecated
	// Just return the existing membership (membership is unchanged, permissions come from teams)
	// Get organization name for response formatting
	org, err = h.orgRepo.GetByID(membershipToUpdate.OrganizationID)
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

	// Reload membership with user data for response
	updatedMember, err := h.orgRepo.GetMemberByID(memberID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve organization membership",
				},
			},
		})
		return
	}

	// Format response - always include user data in included array (JSON:API pattern)
	membershipData := formatOrganizationMembershipResponse(updatedMember, org.Name)
	included := make([]gin.H, 0)

	if updatedMember.User.ID != uuid.Nil {
		userData := formatOrganizationMembershipUserResponse(&updatedMember.User)
		included = append(included, userData)
	}

	response := gin.H{
		"data": membershipData,
	}

	if len(included) > 0 {
		response["included"] = included
	}

	c.JSON(http.StatusOK, response)
}

// Delete deletes an organization membership by ID
// DELETE /api/v2/organization-memberships/:id
func (h *OrganizationMembershipHandlerV2) Delete(c *gin.Context) {
	memberIDStr := c.Param("id")

	memberID, err := uuid.Parse(memberIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid membership ID format",
				},
			},
		})
		return
	}

	// Check if membership exists
	membershipToDelete, err := h.orgRepo.GetMemberByID(memberID)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{
				"errors": []gin.H{
					{
						"status": "404",
						"title":  "Not Found",
						"detail": "Organization membership not found",
					},
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve organization membership",
				},
			},
		})
		return
	}

	// Get current user
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

	// Get organization to check permissions
	org, err := h.orgRepo.GetByID(membershipToDelete.OrganizationID)
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

	// Check if user has permission to manage organization memberships
	hasPermission, err := h.rbacService.CheckOrgManageMembership(c.Request.Context(), user.ID, org.ID)
	if err != nil || !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Only organization admins can delete organization memberships",
				},
			},
		})
		return
	}

	// Delete membership
	if err := h.orgRepo.DeleteMemberByID(memberID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to delete organization membership",
				},
			},
		})
		return
	}

	c.Status(http.StatusNoContent)
}

// formatOrganizationMembershipResponse formats an OrganizationMember as TFE OrganizationMembership
// orgName is the organization name (TFE uses names, not UUIDs, in relationships)
func formatOrganizationMembershipResponse(member *models.OrganizationMember, orgName string) gin.H {
	status := "active"
	email := "N/A"
	username := "N/A"
	name := "-"
	userID := uuid.Nil.String()

	if member.User.ID != uuid.Nil {
		userID = member.User.ID.String()
		if member.User.ZitadelSubject != "" && strings.HasPrefix(member.User.ZitadelSubject, "invited-") {
			status = "invited"
		}
		if member.User.Email != "" {
			email = member.User.Email
		}
		if member.User.Username != "" {
			username = member.User.Username
		}
		if member.User.Name != "" {
			name = member.User.Name
		}
	}

	return gin.H{
		"id":   member.ID.String(),
		"type": "organization-memberships",
		"attributes": gin.H{
			"email":      email,
			"status":     status,
			"role":       nil, // Deprecated: Roles are deprecated, permissions come from team memberships
			"created-at": member.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
			"username":   username, // Added for completeness
			"name":       name,     // Added for completeness
		},
		"relationships": gin.H{
			"organization": gin.H{
				"data": gin.H{
					"id":   orgName, // TFE uses organization name, not UUID
					"type": "organizations",
				},
			},
			"user": gin.H{
				"data": gin.H{
					"id":   userID,
					"type": "users",
				},
			},
			"teams": gin.H{
				"data": []gin.H{},
			},
		},
	}
}

// formatOrganizationMembershipUserResponse formats a User for inclusion in organization membership responses
// Handles cases where user email/name might be empty (e.g., admin users created before auth)
func formatOrganizationMembershipUserResponse(user *models.User) gin.H {
	// Ensure we always return a valid user object, even if email/name are empty
	// Frontend will handle empty strings by showing "N/A" or "-"
	return gin.H{
		"id":   user.ID.String(),
		"type": "users",
		"attributes": gin.H{
			"username": user.Username,
			"email":    user.Email, // May be empty - frontend handles this
			"name":     user.Name,  // May be empty - frontend handles this
		},
	}
}
