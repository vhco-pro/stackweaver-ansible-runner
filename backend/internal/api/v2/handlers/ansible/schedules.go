// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansible

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/api/v2/response"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/ansible"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/rbac"
)

// ScheduleHandler handles schedule API requests
type ScheduleHandler struct {
	schedulerService *ansible.SchedulerService
	orgRepo          *repository.OrganizationRepository
	authService      *auth.Service
	rbacService      *rbac.Service
}

// NewScheduleHandler creates a new schedule handler
func NewScheduleHandler(schedulerService *ansible.SchedulerService, orgRepo *repository.OrganizationRepository, authService *auth.Service, rbacService *rbac.Service) *ScheduleHandler {
	return &ScheduleHandler{
		schedulerService: schedulerService,
		orgRepo:          orgRepo,
		authService:      authService,
		rbacService:      rbacService,
	}
}

// CreateScheduleRequest represents the request to create a schedule (JSON:API format)
type CreateScheduleRequest struct {
	Data struct {
		Type       string `json:"type" binding:"required"` // Must be "schedules"
		Attributes struct {
			Name              string                `json:"name" binding:"required,min=1,max=255"`
			Description       string                `json:"description"`
			ScheduleType      models.ScheduleType   `json:"schedule-type" binding:"required,oneof=job_template inventory_source playbook_sync"`
			JobTemplateID     string                `json:"job-template-id"`
			InventorySourceID string                `json:"inventory-source-id"`
			PlaybookID        string                `json:"playbook-id"`
			CronExpression    string                `json:"cron-expression" binding:"required"`
			Timezone          string                `json:"timezone" binding:"required"`
			StartDateTime     string                `json:"start-date-time"`
			EndDateTime       string                `json:"end-date-time"`
			Config            models.ScheduleConfig `json:"config"`
		} `json:"attributes" binding:"required"`
	} `json:"data" binding:"required"`
}

// UpdateScheduleRequest represents the request to update a schedule
type UpdateScheduleRequest struct {
	Name           *string                `json:"name"`
	Description    *string                `json:"description"`
	CronExpression *string                `json:"cron_expression"`
	Timezone       *string                `json:"timezone"`
	Config         *models.ScheduleConfig `json:"config"`
}

// Create creates a new schedule
// @Summary Create schedule
// @Description Create a new schedule
// @Tags Ansible Schedules
// @Accept json
// @Produce json
// @Param request body CreateScheduleRequest true "Schedule details"
// @Success 201 {object} models.AnsibleSchedule
// @Failure 400 {object} response.ErrorResponse
// @Failure 500 {object} response.ErrorResponse
// @Router /api/v2/ansible/schedules [post]
func (h *ScheduleHandler) Create(c *gin.Context) {
	var req CreateScheduleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	// Log request for debugging (remove in production)
	// logger.Debugf("CreateSchedule: Type=%s, Name=%s, ScheduleType=%s", req.Data.Type, req.Data.Attributes.Name, req.Data.Attributes.ScheduleType)

	// Validate JSON:API type
	if req.Data.Type != "schedules" {
		response.BadRequest(c, "data.type must be 'schedules'")
		return
	}

	// Get organization by name from URL param
	orgName := c.Param("name")
	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		response.NotFound(c, "Organization not found")
		return
	}
	orgID := org.ID

	// RBAC: check org-level write permission
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{"status": "401", "title": "Unauthorized", "detail": "Authentication required"},
			},
		})
		return
	}
	hasPermission, err := h.rbacService.CheckOrgManageAnsible(c.Request.Context(), user.ID, org.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to check permissions"},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "You do not have permission to create schedules in this organization"},
			},
		})
		return
	}

	// Parse target IDs based on type
	var jobTemplateID, inventorySourceID, playbookID *uuid.UUID
	attrs := req.Data.Attributes

	switch attrs.ScheduleType {
	case models.ScheduleTypeJobTemplate:
		if attrs.JobTemplateID == "" {
			response.BadRequest(c, "job-template-id is required for job_template schedules")
			return
		}
		id, err := uuid.Parse(attrs.JobTemplateID)
		if err != nil {
			response.BadRequest(c, "Invalid job-template-id")
			return
		}
		jobTemplateID = &id

	case models.ScheduleTypeInventorySource:
		if attrs.InventorySourceID == "" {
			response.BadRequest(c, "inventory-source-id is required for inventory_source schedules")
			return
		}
		id, err := uuid.Parse(attrs.InventorySourceID)
		if err != nil {
			response.BadRequest(c, "Invalid inventory-source-id")
			return
		}
		inventorySourceID = &id

	case models.ScheduleTypePlaybookSync:
		if attrs.PlaybookID == "" {
			response.BadRequest(c, "playbook-id is required for playbook_sync schedules")
			return
		}
		id, err := uuid.Parse(attrs.PlaybookID)
		if err != nil {
			response.BadRequest(c, "Invalid playbook-id")
			return
		}
		playbookID = &id
	}

	// Parse optional date times
	var startDateTime, endDateTime *time.Time
	if attrs.StartDateTime != "" {
		startDT, err := time.Parse(time.RFC3339, attrs.StartDateTime)
		if err != nil {
			response.BadRequest(c, "Invalid start-date-time format, must be RFC3339")
			return
		}
		startDateTime = &startDT
	}
	if attrs.EndDateTime != "" {
		endDT, err := time.Parse(time.RFC3339, attrs.EndDateTime)
		if err != nil {
			response.BadRequest(c, "Invalid end-date-time format, must be RFC3339")
			return
		}
		endDateTime = &endDT
	}

	// Get user ID from context
	var createdBy *uuid.UUID
	if userIDStr, exists := c.Get("user_id"); exists {
		if id, err := uuid.Parse(userIDStr.(string)); err == nil {
			createdBy = &id
		}
	}

	// Ensure Config is not nil (default to empty map)
	config := attrs.Config
	if config == nil {
		config = make(models.ScheduleConfig)
	}

	schedule, err := h.schedulerService.CreateSchedule(
		orgID,
		attrs.Name,
		attrs.Description,
		attrs.ScheduleType,
		attrs.CronExpression,
		attrs.Timezone,
		jobTemplateID,
		inventorySourceID,
		playbookID,
		config,
		createdBy,
		startDateTime,
		endDateTime,
	)
	if err != nil {
		response.InternalError(c, err.Error())
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"data": formatScheduleResponse(schedule),
	})
}

// formatScheduleResponse formats a schedule for JSON:API response
func formatScheduleResponse(schedule *models.AnsibleSchedule) gin.H {
	// Ensure config is not nil for response
	config := schedule.Config
	if config == nil {
		config = make(models.ScheduleConfig)
	}

	attributes := gin.H{
		"name":            schedule.Name,
		"description":     schedule.Description,
		"schedule-type":   schedule.Type,
		"status":          schedule.Status,
		"cron-expression": schedule.CronExpression,
		"timezone":        schedule.Timezone,
		"config":          config,
		"created-at":      schedule.CreatedAt.Format("2006-01-02T15:04:05Z"),
		"updated-at":      schedule.UpdatedAt.Format("2006-01-02T15:04:05Z"),
	}

	if schedule.StartDateTime != nil {
		attributes["start-date-time"] = schedule.StartDateTime.Format(time.RFC3339)
	}
	if schedule.EndDateTime != nil {
		attributes["end-date-time"] = schedule.EndDateTime.Format(time.RFC3339)
	}
	if schedule.NextRunAt != nil {
		attributes["next-run-at"] = schedule.NextRunAt.Format(time.RFC3339)
	}
	if schedule.LastRunAt != nil {
		attributes["last-run-at"] = schedule.LastRunAt.Format(time.RFC3339)
	}
	if schedule.LastRunStatus != "" {
		attributes["last-run-status"] = schedule.LastRunStatus
	}
	if schedule.RunCount > 0 {
		attributes["run-count"] = schedule.RunCount
	}

	relationships := gin.H{
		"organization": gin.H{
			"data": gin.H{
				"id":   schedule.OrganizationID.String(),
				"type": "organizations",
			},
		},
	}

	if schedule.JobTemplateID != nil {
		relationships["job-template"] = gin.H{
			"data": gin.H{
				"id":   schedule.JobTemplateID.String(),
				"type": "ansible-job-templates",
			},
		}
	}
	if schedule.InventorySourceID != nil {
		relationships["inventory-source"] = gin.H{
			"data": gin.H{
				"id":   schedule.InventorySourceID.String(),
				"type": "ansible-inventory-sources",
			},
		}
	}
	if schedule.PlaybookID != nil {
		relationships["playbook"] = gin.H{
			"data": gin.H{
				"id":   schedule.PlaybookID.String(),
				"type": "ansible-playbooks",
			},
		}
	}
	if schedule.LastJobID != nil {
		relationships["last-job"] = gin.H{
			"data": gin.H{
				"id":   schedule.LastJobID.String(),
				"type": "ansible-jobs",
			},
		}
	}
	if schedule.CreatedBy != nil {
		relationships["created-by"] = gin.H{
			"data": gin.H{
				"id":   schedule.CreatedBy.String(),
				"type": "users",
			},
		}
	}

	return gin.H{
		"id":            schedule.ID.String(),
		"type":          "schedules",
		"attributes":    attributes,
		"relationships": relationships,
	}
}

// Get retrieves a schedule by ID
// @Summary Get schedule
// @Description Get a schedule by ID
// @Tags Ansible Schedules
// @Produce json
// @Param id path string true "Schedule ID"
// @Success 200 {object} models.AnsibleSchedule
// @Failure 400 {object} response.ErrorResponse
// @Failure 404 {object} response.ErrorResponse
// @Router /api/v2/ansible/schedules/{id} [get]
func (h *ScheduleHandler) Get(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.BadRequest(c, "Invalid schedule ID")
		return
	}

	schedule, err := h.schedulerService.GetSchedule(id)
	if err != nil {
		response.NotFound(c, "Schedule not found")
		return
	}

	// RBAC: check org-level read permission
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{"status": "401", "title": "Unauthorized", "detail": "Authentication required"},
			},
		})
		return
	}
	hasPermission, err := h.rbacService.CheckOrgReadAnsible(c.Request.Context(), user.ID, schedule.OrganizationID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to check permissions"},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "You do not have permission to view this schedule"},
			},
		})
		return
	}

	c.JSON(http.StatusOK, schedule)
}

// List lists schedules for an organization
// @Summary List schedules
// @Description List all schedules for the current organization
// @Tags Ansible Schedules
// @Produce json
// @Param limit query int false "Limit" default(20)
// @Param offset query int false "Offset" default(0)
// @Success 200 {object} response.PaginatedResponse
// @Failure 400 {object} response.ErrorResponse
// @Router /api/v2/ansible/schedules [get]
func (h *ScheduleHandler) List(c *gin.Context) {
	// Get organization ID from context
	orgIDStr, exists := c.Get("organization_id")
	if !exists {
		response.BadRequest(c, "Organization ID not found")
		return
	}
	orgID, err := uuid.Parse(orgIDStr.(string))
	if err != nil {
		response.BadRequest(c, "Invalid organization ID")
		return
	}

	// RBAC: check org-level read permission
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{"status": "401", "title": "Unauthorized", "detail": "Authentication required"},
			},
		})
		return
	}
	hasPermission, err := h.rbacService.CheckOrgReadAnsible(c.Request.Context(), user.ID, orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to check permissions"},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "You do not have permission to list schedules in this organization"},
			},
		})
		return
	}

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))

	schedules, total, err := h.schedulerService.ListSchedules(orgID, limit, offset)
	if err != nil {
		response.InternalError(c, err.Error())
		return
	}

	response.Paginated(c, schedules, total, limit, offset)
}

// Update updates a schedule
// @Summary Update schedule
// @Description Update a schedule
// @Tags Ansible Schedules
// @Accept json
// @Produce json
// @Param id path string true "Schedule ID"
// @Param request body UpdateScheduleRequest true "Update details"
// @Success 200 {object} models.AnsibleSchedule
// @Failure 400 {object} response.ErrorResponse
// @Failure 404 {object} response.ErrorResponse
// @Router /api/v2/ansible/schedules/{id} [patch]
func (h *ScheduleHandler) Update(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.BadRequest(c, "Invalid schedule ID")
		return
	}

	// RBAC: fetch schedule and check org-level write permission
	existingSchedule, err := h.schedulerService.GetSchedule(id)
	if err != nil {
		response.NotFound(c, "Schedule not found")
		return
	}
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{"status": "401", "title": "Unauthorized", "detail": "Authentication required"},
			},
		})
		return
	}
	hasPermission, err := h.rbacService.CheckOrgManageAnsible(c.Request.Context(), user.ID, existingSchedule.OrganizationID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to check permissions"},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "You do not have permission to update this schedule"},
			},
		})
		return
	}

	var req UpdateScheduleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	schedule, err := h.schedulerService.UpdateSchedule(id, req.Name, req.Description, req.CronExpression, req.Timezone, req.Config)
	if err != nil {
		response.InternalError(c, err.Error())
		return
	}

	c.JSON(http.StatusOK, schedule)
}

// Delete deletes a schedule
// @Summary Delete schedule
// @Description Delete a schedule
// @Tags Ansible Schedules
// @Param id path string true "Schedule ID"
// @Success 204
// @Failure 400 {object} response.ErrorResponse
// @Failure 500 {object} response.ErrorResponse
// @Router /api/v2/ansible/schedules/{id} [delete]
func (h *ScheduleHandler) Delete(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.BadRequest(c, "Invalid schedule ID")
		return
	}

	// RBAC: fetch schedule and check org-level write permission
	schedule, err := h.schedulerService.GetSchedule(id)
	if err != nil {
		response.NotFound(c, "Schedule not found")
		return
	}
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{"status": "401", "title": "Unauthorized", "detail": "Authentication required"},
			},
		})
		return
	}
	hasPermission, err := h.rbacService.CheckOrgManageAnsible(c.Request.Context(), user.ID, schedule.OrganizationID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to check permissions"},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "You do not have permission to delete this schedule"},
			},
		})
		return
	}

	if err := h.schedulerService.DeleteSchedule(id); err != nil {
		response.InternalError(c, err.Error())
		return
	}

	c.Status(http.StatusNoContent)
}

// Enable enables a schedule
// @Summary Enable schedule
// @Description Enable a schedule
// @Tags Ansible Schedules
// @Param id path string true "Schedule ID"
// @Success 200 {object} gin.H
// @Failure 400 {object} response.ErrorResponse
// @Failure 500 {object} response.ErrorResponse
// @Router /api/v2/ansible/schedules/{id}/enable [post]
func (h *ScheduleHandler) Enable(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.BadRequest(c, "Invalid schedule ID")
		return
	}

	// RBAC: fetch schedule and check org-level write permission
	schedule, err := h.schedulerService.GetSchedule(id)
	if err != nil {
		response.NotFound(c, "Schedule not found")
		return
	}
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{"status": "401", "title": "Unauthorized", "detail": "Authentication required"},
			},
		})
		return
	}
	hasPermission, err := h.rbacService.CheckOrgManageAnsible(c.Request.Context(), user.ID, schedule.OrganizationID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to check permissions"},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "You do not have permission to enable this schedule"},
			},
		})
		return
	}

	if err := h.schedulerService.EnableSchedule(id); err != nil {
		response.InternalError(c, err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Schedule enabled"})
}

// Disable disables a schedule
// @Summary Disable schedule
// @Description Disable a schedule
// @Tags Ansible Schedules
// @Param id path string true "Schedule ID"
// @Success 200 {object} gin.H
// @Failure 400 {object} response.ErrorResponse
// @Failure 500 {object} response.ErrorResponse
// @Router /api/v2/ansible/schedules/{id}/disable [post]
func (h *ScheduleHandler) Disable(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.BadRequest(c, "Invalid schedule ID")
		return
	}

	// RBAC: fetch schedule and check org-level write permission
	schedule, err := h.schedulerService.GetSchedule(id)
	if err != nil {
		response.NotFound(c, "Schedule not found")
		return
	}
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{"status": "401", "title": "Unauthorized", "detail": "Authentication required"},
			},
		})
		return
	}
	hasPermission, err := h.rbacService.CheckOrgManageAnsible(c.Request.Context(), user.ID, schedule.OrganizationID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to check permissions"},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "You do not have permission to disable this schedule"},
			},
		})
		return
	}

	if err := h.schedulerService.DisableSchedule(id); err != nil {
		response.InternalError(c, err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Schedule disabled"})
}

// ValidateCron validates a cron expression and returns the next run time
// @Summary Validate cron expression
// @Description Validate a cron expression and return the next run time
// @Tags Ansible Schedules
// @Accept json
// @Produce json
// @Param request body ValidateCronRequest true "Cron expression"
// @Success 200 {object} ValidateCronResponse
// @Failure 400 {object} response.ErrorResponse
// @Router /api/v2/ansible/schedules/validate-cron [post]
func (h *ScheduleHandler) ValidateCron(c *gin.Context) {
	var req ValidateCronRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	if err := h.schedulerService.ValidateCronExpression(req.CronExpression); err != nil {
		response.BadRequest(c, "Invalid cron expression: "+err.Error())
		return
	}

	timezone := req.Timezone
	if timezone == "" {
		timezone = "UTC"
	}

	nextRun, err := h.schedulerService.GetNextRunTime(req.CronExpression, timezone)
	if err != nil {
		response.BadRequest(c, "Error calculating next run time: "+err.Error())
		return
	}

	c.JSON(http.StatusOK, ValidateCronResponse{
		Valid:          true,
		CronExpression: req.CronExpression,
		Timezone:       timezone,
		NextRunAt:      nextRun.Format("2006-01-02T15:04:05Z07:00"),
	})
}

// ValidateCronRequest represents the request to validate a cron expression
type ValidateCronRequest struct {
	CronExpression string `json:"cron_expression" binding:"required"`
	Timezone       string `json:"timezone"`
}

// ValidateCronResponse represents the response for cron validation
type ValidateCronResponse struct {
	Valid          bool   `json:"valid"`
	CronExpression string `json:"cron_expression"`
	Timezone       string `json:"timezone"`
	NextRunAt      string `json:"next_run_at"`
}

// GetCronPresets returns the available cron presets
// @Summary Get cron presets
// @Description Get available cron expression presets
// @Tags Ansible Schedules
// @Produce json
// @Success 200 {object} map[string]string
// @Router /api/v2/ansible/schedules/cron-presets [get]
func (h *ScheduleHandler) GetCronPresets(c *gin.Context) {
	c.JSON(http.StatusOK, models.CronPresets)
}

// ListByOrganization lists schedules for an organization by name
// @Summary List schedules by organization
// @Description List all schedules for an organization by name
// @Tags Ansible Schedules
// @Produce json
// @Param name path string true "Organization name"
// @Param limit query int false "Limit" default(20)
// @Param offset query int false "Offset" default(0)
// @Success 200 {object} response.PaginatedResponse
// @Failure 400 {object} response.ErrorResponse
// @Router /api/v2/organizations/{name}/ansible/schedules [get]
func (h *ScheduleHandler) ListByOrganization(c *gin.Context) {
	// Get organization by name from URL param
	orgName := c.Param("name")
	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		response.NotFound(c, "Organization not found")
		return
	}
	orgID := org.ID

	// RBAC: check org-level read permission
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{"status": "401", "title": "Unauthorized", "detail": "Authentication required"},
			},
		})
		return
	}
	hasPermission, err := h.rbacService.CheckOrgReadAnsible(c.Request.Context(), user.ID, org.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to check permissions"},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "You do not have permission to list schedules in this organization"},
			},
		})
		return
	}

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))

	schedules, total, err := h.schedulerService.ListSchedules(orgID, limit, offset)
	if err != nil {
		response.InternalError(c, err.Error())
		return
	}

	response.Paginated(c, schedules, total, limit, offset)
}

// RunNow triggers immediate execution of a schedule
// @Summary Run schedule now
// @Description Trigger immediate execution of a schedule
// @Tags Ansible Schedules
// @Param id path string true "Schedule ID"
// @Success 200 {object} gin.H
// @Failure 400 {object} response.ErrorResponse
// @Failure 500 {object} response.ErrorResponse
// @Router /api/v2/ansible/schedules/{id}/actions/run-now [post]
func (h *ScheduleHandler) RunNow(c *gin.Context) {
	id, err := uuid.Parse(c.Param("schedule_id"))
	if err != nil {
		response.BadRequest(c, "Invalid schedule ID")
		return
	}

	// RBAC: fetch schedule and check org-level write permission
	schedule, err := h.schedulerService.GetSchedule(id)
	if err != nil {
		response.NotFound(c, "Schedule not found")
		return
	}
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{"status": "401", "title": "Unauthorized", "detail": "Authentication required"},
			},
		})
		return
	}
	hasPermission, err := h.rbacService.CheckOrgManageAnsible(c.Request.Context(), user.ID, schedule.OrganizationID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to check permissions"},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "You do not have permission to run this schedule"},
			},
		})
		return
	}

	if err := h.schedulerService.RunScheduleNow(id); err != nil {
		response.InternalError(c, err.Error())
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Schedule triggered"})
}
