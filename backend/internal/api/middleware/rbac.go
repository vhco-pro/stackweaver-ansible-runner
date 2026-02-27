// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/services/rbac"
)

func RBACMiddleware(rbacService *rbac.Service, permission rbac.Permission) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, exists := c.Get("user_id")
		if !exists {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			c.Abort()
			return
		}

		userIDUUID, ok := userID.(uuid.UUID)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid user ID"})
			c.Abort()
			return
		}

		// Extract organization ID from context or params
		orgIDStr := c.Param("organization_id")
		if orgIDStr == "" {
			orgIDStr = c.Query("organization_id")
		}

		if orgIDStr == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "organization_id required"})
			c.Abort()
			return
		}

		orgID, err := uuid.Parse(orgIDStr)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid organization_id"})
			c.Abort()
			return
		}

		hasPermission, err := rbacService.CheckPermission(c.Request.Context(), userIDUUID, orgID, permission)
		if err != nil || !hasPermission {
			c.JSON(http.StatusForbidden, gin.H{"error": "insufficient permissions"})
			c.Abort()
			return
		}

		c.Next()
	}
}
