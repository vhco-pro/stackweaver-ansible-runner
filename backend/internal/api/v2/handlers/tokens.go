// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/auth"
)

type TokenHandlerV2 struct {
	tfeTokenRepo *repository.TFETokenRepository
	authService  *auth.Service
}

func NewTokenHandlerV2(
	tfeTokenRepo *repository.TFETokenRepository,
	authService *auth.Service,
) *TokenHandlerV2 {
	return &TokenHandlerV2{
		tfeTokenRepo: tfeTokenRepo,
		authService:  authService,
	}
}

type CreateTokenRequestV2 struct {
	Description string     `json:"description"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

type TokenResponseV2 struct {
	ID          uuid.UUID  `json:"id"`
	Token       string     `json:"token"` // Only returned on creation
	Description string     `json:"description"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// Create creates a new TFE token for the authenticated user
// POST /api/v2/tokens
func (h *TokenHandlerV2) Create(c *gin.Context) {
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{
					"status": "401",
					"title":  "Unauthorized",
					"detail": "User not authenticated",
				},
			},
		})
		return
	}

	var req CreateTokenRequestV2
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

	// Generate token
	tokenString, err := models.GenerateTFEToken()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to generate token",
				},
			},
		})
		return
	}

	// Create token record (token will be hashed in repository)
	tfeToken := &models.TFEToken{
		UserID:      user.ID,
		Token:       tokenString, // Will be hashed in Create()
		Description: req.Description,
		ExpiresAt:   req.ExpiresAt,
	}

	if err := h.tfeTokenRepo.Create(tfeToken); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to create token",
				},
			},
		})
		return
	}

	// Return token with plaintext token (only time it's shown)
	// TFE-compatible response format
	c.JSON(http.StatusCreated, gin.H{
		"data": gin.H{
			"id":   tfeToken.ID,
			"type": "tokens",
			"attributes": gin.H{
				"token":       tokenString, // Plaintext token (only shown once)
				"description": tfeToken.Description,
				"expires_at":  tfeToken.ExpiresAt,
				"created_at":  tfeToken.CreatedAt,
			},
		},
	})
}

// List lists all TFE tokens for the authenticated user
// GET /api/v2/tokens
func (h *TokenHandlerV2) List(c *gin.Context) {
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{
					"status": "401",
					"title":  "Unauthorized",
					"detail": "User not authenticated",
				},
			},
		})
		return
	}

	tokens, err := h.tfeTokenRepo.ListByUser(user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to list tokens",
				},
			},
		})
		return
	}

	// Convert to response format (without plaintext tokens)
	responseData := make([]gin.H, 0, len(tokens))
	for _, token := range tokens {
		responseData = append(responseData, gin.H{
			"id":   token.ID,
			"type": "tokens",
			"attributes": gin.H{
				"description":  token.Description,
				"last_used_at": token.LastUsedAt,
				"expires_at":   token.ExpiresAt,
				"created_at":   token.CreatedAt,
			},
		})
	}

	// TFE-compatible response format
	c.JSON(http.StatusOK, gin.H{
		"data": responseData,
	})
}

// Delete deletes a TFE token
// DELETE /api/v2/tokens/:id
func (h *TokenHandlerV2) Delete(c *gin.Context) {
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{
					"status": "401",
					"title":  "Unauthorized",
					"detail": "User not authenticated",
				},
			},
		})
		return
	}

	tokenID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid token ID",
				},
			},
		})
		return
	}

	// Verify token belongs to user
	token, err := h.tfeTokenRepo.GetByID(tokenID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Token not found",
				},
			},
		})
		return
	}

	if token.UserID != user.ID {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Token does not belong to user",
				},
			},
		})
		return
	}

	if err := h.tfeTokenRepo.Delete(tokenID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to delete token",
				},
			},
		})
		return
	}

	c.Status(http.StatusNoContent)
}
