// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/iac-platform/backend/internal/repository"
)

// WebhookEventHandlerV2 handles webhook event listing for debugging.
type WebhookEventHandlerV2 struct {
	eventRepo *repository.WebhookEventRepository
	orgRepo   *repository.OrganizationRepository
}

func NewWebhookEventHandlerV2(eventRepo *repository.WebhookEventRepository, orgRepo *repository.OrganizationRepository) *WebhookEventHandlerV2 {
	return &WebhookEventHandlerV2{
		eventRepo: eventRepo,
		orgRepo:   orgRepo,
	}
}

// List lists recent webhook events for an organization.
// GET /api/v2/organizations/:name/webhook-events
func (h *WebhookEventHandlerV2) List(c *gin.Context) {
	orgName := c.Param("name")

	// Get organization
	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}},
		})
		return
	}

	// Parse pagination
	limit := 50
	offset := 0
	if l, err := strconv.Atoi(c.DefaultQuery("page[size]", "50")); err == nil && l > 0 && l <= 100 {
		limit = l
	}
	if p, err := strconv.Atoi(c.DefaultQuery("page[number]", "1")); err == nil && p > 0 {
		offset = (p - 1) * limit
	}

	events, total, err := h.eventRepo.ListByOrganization(org.ID, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to list webhook events"}},
		})
		return
	}

	// Check format parameter
	format := c.DefaultQuery("format", "")

	if format == "simple" {
		// Simple JSON format for frontend
		data := make([]gin.H, len(events))
		for i, event := range events {
			data[i] = gin.H{
				"id":            event.ID.String(),
				"event_type":    event.EventType,
				"provider":      event.Provider,
				"repository":    event.Repository,
				"branch":        event.Branch,
				"commit":        event.Commit,
				"status":        event.Status,
				"response_code": event.ResponseCode,
				"message":       event.Message,
				"delivered_at":  event.DeliveredAt,
				"processed_at":  event.ProcessedAt,
			}
		}
		c.JSON(http.StatusOK, gin.H{
			"data": data,
			"meta": gin.H{
				"total":       total,
				"page_size":   limit,
				"page_number": (offset / limit) + 1,
			},
		})
		return
	}

	// JSON:API format
	data := make([]gin.H, len(events))
	for i, event := range events {
		data[i] = gin.H{
			"id":   event.ID.String(),
			"type": "webhook-events",
			"attributes": gin.H{
				"event-type":    event.EventType,
				"provider":      event.Provider,
				"repository":    event.Repository,
				"branch":        event.Branch,
				"commit":        event.Commit,
				"status":        event.Status,
				"response-code": event.ResponseCode,
				"message":       event.Message,
				"delivered-at":  event.DeliveredAt,
				"processed-at":  event.ProcessedAt,
			},
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"data": data,
		"meta": gin.H{
			"pagination": gin.H{
				"current-page": (offset / limit) + 1,
				"page-size":    limit,
				"total-count":  total,
			},
		},
	})
}
