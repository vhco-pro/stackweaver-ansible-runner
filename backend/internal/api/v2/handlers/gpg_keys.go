// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/registry"
)

// GPGKeyHandler handles GPG key management operations
type GPGKeyHandler struct {
	gpgKeyRepo  *repository.GPGKeyRepository
	orgRepo     *repository.OrganizationRepository
	authService *auth.Service
	gpgService  *registry.GPGService
}

func NewGPGKeyHandler(
	gpgKeyRepo *repository.GPGKeyRepository,
	orgRepo *repository.OrganizationRepository,
	authService *auth.Service,
) *GPGKeyHandler {
	return &GPGKeyHandler{
		gpgKeyRepo:  gpgKeyRepo,
		orgRepo:     orgRepo,
		authService: authService,
		gpgService:  registry.NewGPGService(),
	}
}

// CreateGPGKey handles POST /api/v2/organizations/:name/registry/gpg-keys
func (h *GPGKeyHandler) CreateGPGKey(c *gin.Context) {
	orgName := c.Param("name")

	// Get authenticated user
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}},
		})
		return
	}

	// Get organization
	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}},
		})
		return
	}

	// Parse request body
	var req struct {
		ASCIIArmor string `json:"ascii_armor" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}},
		})
		return
	}

	// Extract key ID from ASCII armor
	keyID, err := h.gpgService.ParseGPGKey(req.ASCIIArmor)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": fmt.Sprintf("Invalid GPG key: %v", err)}},
		})
		return
	}

	// Check if key already exists
	existing, err := h.gpgKeyRepo.GetByKeyID(org.ID, keyID)
	if err == nil && existing != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": fmt.Sprintf("GPG key %s already exists", keyID)}},
		})
		return
	}

	// Create GPG key
	gpgKey := &models.GPGKey{
		OrganizationID: org.ID,
		KeyID:          keyID,
		ASCIIArmor:     req.ASCIIArmor,
		CreatedBy:      user.ID,
	}

	if err := h.gpgKeyRepo.Create(gpgKey); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": err.Error()}},
		})
		return
	}

	// Format response (TFE-compatible)
	c.JSON(http.StatusCreated, gin.H{
		"data": gin.H{
			"id":   gpgKey.ID.String(),
			"type": "gpg-keys",
			"attributes": gin.H{
				"key_id":      gpgKey.KeyID,
				"ascii_armor": gpgKey.ASCIIArmor,
			},
		},
	})
}

// ListGPGKeys handles GET /api/v2/organizations/:name/registry/gpg-keys
func (h *GPGKeyHandler) ListGPGKeys(c *gin.Context) {
	orgName := c.Param("name")

	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}},
		})
		return
	}

	keys, err := h.gpgKeyRepo.GetByOrganization(org.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": err.Error()}},
		})
		return
	}

	// Format response
	data := make([]gin.H, len(keys))
	for i, key := range keys {
		data[i] = gin.H{
			"id":   key.ID.String(),
			"type": "gpg-keys",
			"attributes": gin.H{
				"key_id":      key.KeyID,
				"ascii_armor": key.ASCIIArmor,
				"created_at":  key.CreatedAt.Format("2006-01-02T15:04:05Z"),
			},
		}
	}

	c.JSON(http.StatusOK, gin.H{"data": data})
}

// DeleteGPGKey handles DELETE /api/v2/organizations/:name/registry/gpg-keys/:key_id
func (h *GPGKeyHandler) DeleteGPGKey(c *gin.Context) {
	orgName := c.Param("name")
	keyIDParam := c.Param("key_id")

	// Get authenticated user
	_, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}},
		})
		return
	}

	// Get organization
	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}},
		})
		return
	}

	// Get GPG key by key ID
	key, err := h.gpgKeyRepo.GetByKeyID(org.ID, keyIDParam)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "GPG key not found"}},
		})
		return
	}

	// Delete key
	if err := h.gpgKeyRepo.Delete(key.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": err.Error()}},
		})
		return
	}

	c.JSON(http.StatusNoContent, nil)
}
