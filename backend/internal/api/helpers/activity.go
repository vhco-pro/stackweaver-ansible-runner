// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package helpers

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/services/activity"
)

// GetActivityContext extracts activity context from Gin context
func GetActivityContext(c *gin.Context) activity.ActivityContext {
	ctx := activity.ActivityContext{
		IPAddress: c.ClientIP(),
		UserAgent: c.GetHeader("User-Agent"),
	}

	// Get user ID if available
	if userID, exists := c.Get("user_id"); exists {
		if id, ok := userID.(uuid.UUID); ok {
			ctx.UserID = &id
		}
	}

	// Get organization ID from params if available
	if orgIDStr := c.Param("organization_id"); orgIDStr != "" {
		if id, err := uuid.Parse(orgIDStr); err == nil {
			ctx.OrganizationID = &id
		}
	}

	// Get project ID from params if available
	if projectIDStr := c.Param("project_id"); projectIDStr != "" {
		if id, err := uuid.Parse(projectIDStr); err == nil {
			ctx.ProjectID = &id
		}
	}

	// Get workspace ID from params if available (now uses prefixed string IDs)
	if workspaceIDStr := c.Param("workspace_id"); workspaceIDStr != "" {
		ctx.WorkspaceID = &workspaceIDStr
	}

	return ctx
}
