// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/profile"
)

type ProfileHandler struct {
	profileService *profile.Service
	authService    *auth.Service
	userRepo       *repository.UserRepository
}

func NewProfileHandler(profileService *profile.Service, authService *auth.Service, userRepo *repository.UserRepository) *ProfileHandler {
	return &ProfileHandler{
		profileService: profileService,
		authService:    authService,
		userRepo:       userRepo,
	}
}

// GetProfile gets the current user's profile
// GET /api/v2/settings/profile
func (h *ProfileHandler) GetProfile(c *gin.Context) {
	// Get local user from context
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	// Get profile from Zitadel if service is available
	var zitadelProfile *profile.UserProfile
	if h.profileService != nil {
		userSubject, err := h.authService.GetUserSubject(c)
		if err == nil {
			zitadelProfile, _ = h.profileService.GetUserProfile(userSubject)
		}
	}

	// Merge Zitadel profile with local user data
	response := gin.H{
		"id":         user.ID.String(),
		"email":      user.Email,
		"name":       user.Name,
		"username":   user.Username,
		"bio":        user.Bio,
		"company":    user.Company,
		"location":   user.Location,
		"created_at": user.CreatedAt,
		"updated_at": user.UpdatedAt,
	}

	// Override with Zitadel data if available (Zitadel is source of truth for name/email)
	if zitadelProfile != nil {
		if zitadelProfile.Email != "" {
			response["email"] = zitadelProfile.Email
		}
		if zitadelProfile.Name != "" {
			response["name"] = zitadelProfile.Name
		}
	}

	c.JSON(http.StatusOK, response)
}

// UpdateProfile updates the current user's profile
// PATCH /api/v2/settings/profile
type UpdateProfileRequest struct {
	Name     *string `json:"name,omitempty"`
	Email    *string `json:"email,omitempty"`
	Username *string `json:"username,omitempty"`
	Bio      *string `json:"bio,omitempty"`
	Company  *string `json:"company,omitempty"`
	Location *string `json:"location,omitempty"`
}

func (h *ProfileHandler) UpdateProfile(c *gin.Context) {
	// Read raw JSON to check which fields are present (including empty strings)
	var jsonData map[string]interface{}
	if err := c.ShouldBindJSON(&jsonData); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Build request struct from JSON data, handling empty strings properly
	var req UpdateProfileRequest
	if val, ok := jsonData["name"]; ok {
		if str, ok := val.(string); ok {
			req.Name = &str // Empty string is valid to clear the field
		}
	}
	if val, ok := jsonData["email"]; ok {
		if str, ok := val.(string); ok {
			req.Email = &str
		}
	}
	if val, ok := jsonData["username"]; ok {
		if str, ok := val.(string); ok {
			req.Username = &str
		}
	}
	if val, ok := jsonData["bio"]; ok {
		if str, ok := val.(string); ok {
			req.Bio = &str
		}
	}
	if val, ok := jsonData["company"]; ok {
		if str, ok := val.(string); ok {
			req.Company = &str
		}
	}
	if val, ok := jsonData["location"]; ok {
		if str, ok := val.(string); ok {
			req.Location = &str
		}
	}

	// Get local user from context
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	// Update in Zitadel if name or email changed
	// Note: Zitadel doesn't allow empty names/emails, so we only update if values are non-empty
	if h.profileService != nil {
		userSubject, err := h.authService.GetUserSubject(c)
		if err == nil {
			updateReq := &profile.UpdateProfileRequest{}
			shouldUpdateZitadel := false

			// Only update name in Zitadel if it's not empty (Zitadel requires non-empty names)
			if req.Name != nil && *req.Name != "" {
				updateReq.Name = *req.Name
				shouldUpdateZitadel = true
			}

			// Only update email in Zitadel if it's not empty (Zitadel requires non-empty emails)
			if req.Email != nil && *req.Email != "" {
				updateReq.Email = *req.Email
				shouldUpdateZitadel = true
			}

			if shouldUpdateZitadel {
				if err := h.profileService.UpdateUserProfile(userSubject, updateReq); err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update profile in Zitadel", "details": err.Error()})
					return
				}
			}
		}
	}

	// Update local user record (only fields that are provided)
	// Empty strings are allowed to clear fields
	if req.Name != nil {
		user.Name = *req.Name
	}
	if req.Email != nil {
		user.Email = *req.Email
	}
	if req.Username != nil {
		user.Username = *req.Username
	}
	if req.Bio != nil {
		user.Bio = *req.Bio
	}
	if req.Company != nil {
		user.Company = *req.Company
	}
	if req.Location != nil {
		user.Location = *req.Location
	}

	if err := h.userRepo.Update(user); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update profile", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Profile updated successfully",
		"profile": gin.H{
			"id":       user.ID.String(),
			"email":    user.Email,
			"name":     user.Name,
			"username": user.Username,
			"bio":      user.Bio,
			"company":  user.Company,
			"location": user.Location,
		},
	})
}
