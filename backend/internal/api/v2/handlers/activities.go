// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/activity"
	"github.com/iac-platform/backend/internal/services/auth"
)

type ActivityHandlerV2 struct {
	activityService *activity.Service
	authService     *auth.Service
}

func NewActivityHandlerV2(activityService *activity.Service, authService *auth.Service) *ActivityHandlerV2 {
	return &ActivityHandlerV2{
		activityService: activityService,
		authService:     authService,
	}
}

// ListActivities handles GET /api/v2/activities
func (h *ActivityHandlerV2) ListActivities(c *gin.Context) {
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}

	// Parse query parameters
	limitStr := c.DefaultQuery("limit", "50")
	offsetStr := c.DefaultQuery("offset", "0")
	limit, _ := strconv.Atoi(limitStr)
	offset, _ := strconv.Atoi(offsetStr)

	if limit > 100 {
		limit = 100
	}
	if limit < 1 {
		limit = 50
	}

	// Parse optional filters
	var userID *uuid.UUID
	if c.Query("user_id") != "" {
		if id, err := uuid.Parse(c.Query("user_id")); err == nil {
			userID = &id
		}
	}

	var orgID *uuid.UUID
	if c.Query("organization_id") != "" {
		if id, err := uuid.Parse(c.Query("organization_id")); err == nil {
			orgID = &id
		}
	}

	var workspaceID *string
	if c.Query("workspace_id") != "" {
		workspaceIDStr := c.Query("workspace_id")
		workspaceID = &workspaceIDStr
	}

	// Default to current user's activities if no filters
	if userID == nil && orgID == nil && workspaceID == nil {
		userID = &user.ID
	}

	filters := repository.AuditLogFilters{
		UserID:         userID,
		OrganizationID: orgID,
		WorkspaceID:    workspaceID,
		Action:         c.Query("action"),
		ResourceType:   c.Query("resource_type"),
	}

	activities, total, err := h.activityService.GetActivities(filters, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": err.Error()}}})
		return
	}

	// Format response
	activitiesData := make([]gin.H, len(activities))
	for i, act := range activities {
		attrs := gin.H{
			"action":        act.Action,
			"resource_type": act.ResourceType,
			"details":       act.Details,
			"created_at":    act.CreatedAt.Format("2006-01-02T15:04:05Z"),
		}

		// Convert UUID pointers to strings (or omit if nil)
		if act.ResourceID != nil {
			attrs["resource_id"] = act.ResourceID.String()
		}
		if act.UserID != nil {
			attrs["user_id"] = act.UserID.String()
		}
		if act.OrganizationID != nil {
			attrs["organization_id"] = act.OrganizationID.String()
		}
		if act.ProjectID != nil {
			attrs["project_id"] = act.ProjectID.String()
		}
		if act.WorkspaceID != nil {
			attrs["workspace_id"] = act.WorkspaceID.String()
		}

		activitiesData[i] = gin.H{
			"id":         act.ID.String(),
			"type":       "activity",
			"attributes": attrs,
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"data": activitiesData,
		"meta": gin.H{
			"pagination": gin.H{
				"total":  total,
				"limit":  limit,
				"offset": offset,
			},
		},
	})
}

// GetRecentActivities handles GET /api/v2/activities/recent
func (h *ActivityHandlerV2) GetRecentActivities(c *gin.Context) {
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return
	}

	limitStr := c.DefaultQuery("limit", "10")
	limit, _ := strconv.Atoi(limitStr)
	if limit > 50 {
		limit = 50
	}
	if limit < 1 {
		limit = 10
	}

	var orgID *uuid.UUID
	if c.Query("organization_id") != "" {
		if id, err := uuid.Parse(c.Query("organization_id")); err == nil {
			orgID = &id
		}
	}

	activities, err := h.activityService.GetRecentActivities(&user.ID, orgID, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": err.Error()}}})
		return
	}

	activitiesData := make([]gin.H, len(activities))
	for i, act := range activities {
		attrs := gin.H{
			"action":        act.Action,
			"resource_type": act.ResourceType,
			"details":       act.Details,
			"created_at":    act.CreatedAt.Format("2006-01-02T15:04:05Z"),
		}

		// Convert UUID pointers to strings (or omit if nil)
		if act.ResourceID != nil {
			attrs["resource_id"] = act.ResourceID.String()
		}
		if act.UserID != nil {
			attrs["user_id"] = act.UserID.String()
		}
		if act.OrganizationID != nil {
			attrs["organization_id"] = act.OrganizationID.String()
		}
		if act.ProjectID != nil {
			attrs["project_id"] = act.ProjectID.String()
		}
		if act.WorkspaceID != nil {
			attrs["workspace_id"] = act.WorkspaceID.String()
		}

		activitiesData[i] = gin.H{
			"id":         act.ID.String(),
			"type":       "activity",
			"attributes": attrs,
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"data": activitiesData,
	})
}
