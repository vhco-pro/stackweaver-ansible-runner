// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/rbac"
	"gorm.io/gorm"
)

// RunnerHandlerV2 handles runner management endpoints
type RunnerHandlerV2 struct {
	runnerRepo  *repository.RunnerRepository
	jobExecRepo *repository.RunnerJobExecutionRepository
	poolRepo    *repository.AgentPoolRepository
	orgRepo     *repository.OrganizationRepository
	rbacService *rbac.Service
}

// NewRunnerHandlerV2 creates a new runner handler
func NewRunnerHandlerV2(
	runnerRepo *repository.RunnerRepository,
	jobExecRepo *repository.RunnerJobExecutionRepository,
	poolRepo *repository.AgentPoolRepository,
	orgRepo *repository.OrganizationRepository,
	rbacService *rbac.Service,
) *RunnerHandlerV2 {
	return &RunnerHandlerV2{
		runnerRepo:  runnerRepo,
		jobExecRepo: jobExecRepo,
		poolRepo:    poolRepo,
		orgRepo:     orgRepo,
		rbacService: rbacService,
	}
}

// requireManageAgentPools checks org:manage-agent-pools permission
func (h *RunnerHandlerV2) requireManageAgentPools(c *gin.Context, orgID uuid.UUID) bool {
	userID, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized"}}})
		return false
	}
	uid := userID.(uuid.UUID)
	ok, err := h.rbacService.CheckOrgManageAgentPools(c.Request.Context(), uid, orgID)
	if err != nil || !ok {
		c.JSON(http.StatusForbidden, gin.H{"errors": []gin.H{{"status": "403", "title": "Forbidden", "detail": "You do not have permission to manage agent pools/runners for this organization"}}})
		return false
	}
	return true
}

// List lists runners for an organization
// GET /api/v2/organizations/:name/runners
func (h *RunnerHandlerV2) List(c *gin.Context) {
	orgName := c.Param("name")
	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Organization not found"}}})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		}
		return
	}

	if !h.requireManageAgentPools(c, org.ID) {
		return
	}

	// Parse query params
	opts := repository.ListRunnersOptions{
		Query: c.Query("q"),
		Sort:  c.Query("sort"),
	}

	if poolIDStr := c.Query("filter[agent_pool_id]"); poolIDStr != "" {
		poolID, err := uuid.Parse(poolIDStr)
		if err == nil {
			opts.AgentPoolID = &poolID
		}
	}
	if status := c.Query("filter[status]"); status != "" {
		opts.Status = status
	}
	if runnerType := c.Query("filter[runner_type]"); runnerType != "" {
		opts.RunnerType = runnerType
	}

	// Pagination
	pageSize := 20
	pageNum := 1
	if ps := c.Query("page[size]"); ps != "" {
		if n, err := strconv.Atoi(ps); err == nil && n > 0 && n <= 100 {
			pageSize = n
		}
	}
	if pn := c.Query("page[number]"); pn != "" {
		if n, err := strconv.Atoi(pn); err == nil && n > 0 {
			pageNum = n
		}
	}
	opts.Limit = pageSize
	opts.Offset = (pageNum - 1) * pageSize

	runners, total, err := h.runnerRepo.ListByOrganization(org.ID, opts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		return
	}

	// Build response
	data := make([]gin.H, 0, len(runners))
	for _, r := range runners {
		data = append(data, buildRunnerResponse(&r))
	}

	totalPages := (int(total) + pageSize - 1) / pageSize

	c.JSON(http.StatusOK, gin.H{
		"data": data,
		"meta": gin.H{
			"pagination": gin.H{
				"current-page": pageNum,
				"page-size":    pageSize,
				"total-count":  total,
				"total-pages":  totalPages,
			},
		},
	})
}

// GetByID returns a runner by ID
// GET /api/v2/runners/:id
func (h *RunnerHandlerV2) GetByID(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Invalid runner ID"}}})
		return
	}

	runner, err := h.runnerRepo.GetByID(id)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Runner not found"}}})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		}
		return
	}

	if !h.requireManageAgentPools(c, runner.OrganizationID) {
		return
	}

	// Get job history
	jobs, _ := h.jobExecRepo.ListByRunner(runner.ID, 10)
	currentJobs, _ := h.jobExecRepo.CountActiveByRunner(runner.ID)
	runner.CurrentJobs = int(currentJobs)

	response := buildRunnerResponse(runner)

	// Add job history to response
	jobHistory := make([]gin.H, 0, len(jobs))
	for _, j := range jobs {
		jobHistory = append(jobHistory, gin.H{
			"id":             j.ID.String(),
			"job_type":       j.JobType,
			"job_id":         j.JobID.String(),
			"workspace_id":   j.WorkspaceID,
			"workspace_name": j.WorkspaceName,
			"status":         j.Status,
			"started_at":     j.StartedAt,
			"finished_at":    j.FinishedAt,
			"duration_ms":    j.Duration().Milliseconds(),
		})
	}
	response["attributes"].(gin.H)["recent_jobs"] = jobHistory

	c.JSON(http.StatusOK, gin.H{"data": response})
}

// Update updates a runner (labels, description)
// PATCH /api/v2/runners/:id
func (h *RunnerHandlerV2) Update(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Invalid runner ID"}}})
		return
	}

	runner, err := h.runnerRepo.GetByID(id)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Runner not found"}}})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		}
		return
	}

	if !h.requireManageAgentPools(c, runner.OrganizationID) {
		return
	}

	var req struct {
		Data struct {
			Attributes struct {
				Description *string  `json:"description"`
				Labels      []string `json:"labels"`
			} `json:"attributes"`
		} `json:"data"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}}})
		return
	}

	if req.Data.Attributes.Description != nil {
		runner.Description = *req.Data.Attributes.Description
	}
	if req.Data.Attributes.Labels != nil {
		runner.Labels = models.RunnerLabels(req.Data.Attributes.Labels)
	}

	if err := h.runnerRepo.Update(runner); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": buildRunnerResponse(runner)})
}

// Delete deletes a runner
// DELETE /api/v2/runners/:id
func (h *RunnerHandlerV2) Delete(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Invalid runner ID"}}})
		return
	}

	runner, err := h.runnerRepo.GetByID(id)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Runner not found"}}})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		}
		return
	}

	if !h.requireManageAgentPools(c, runner.OrganizationID) {
		return
	}

	if err := h.runnerRepo.Delete(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		return
	}

	c.Status(http.StatusNoContent)
}

// GetStats returns runner statistics for an organization
// GET /api/v2/organizations/:name/runners/stats
func (h *RunnerHandlerV2) GetStats(c *gin.Context) {
	orgName := c.Param("name")
	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Organization not found"}}})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		}
		return
	}

	if !h.requireManageAgentPools(c, org.ID) {
		return
	}

	total, online, err := h.runnerRepo.CountByOrganization(org.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"type": "runner-stats",
			"attributes": gin.H{
				"total":   total,
				"online":  online,
				"offline": total - online,
			},
		},
	})
}

// buildRunnerResponse builds a JSON:API response for a runner
func buildRunnerResponse(r *models.Runner) gin.H {
	var lastHeartbeat *string
	if r.LastHeartbeatAt != nil {
		formatted := r.LastHeartbeatAt.Format("2006-01-02T15:04:05Z")
		lastHeartbeat = &formatted
	}

	attrs := gin.H{
		"name":                  r.Name,
		"description":           r.Description,
		"agent-pool-id":         r.AgentPoolID.String(),
		"runner-type":           r.RunnerType,
		"status":                r.Status,
		"hostname":              r.Hostname,
		"ip-address":            r.IPAddress,
		"os-type":               r.OSType,
		"os-version":            r.OSVersion,
		"agent-version":         r.AgentVersion,
		"labels":                r.Labels,
		"terraform-version":     r.TerraformVersion,
		"ansible-version":       r.AnsibleVersion,
		"available-collections": r.AvailableCollections,
		"max-concurrent-jobs":   r.MaxConcurrentJobs,
		"current-jobs":          r.CurrentJobs,
		"last-heartbeat-at":     lastHeartbeat,
		"registered-at":         r.RegisteredAt.Format("2006-01-02T15:04:05Z"),
	}

	// Include pool name if preloaded
	if r.AgentPool.ID != uuid.Nil {
		attrs["agent-pool-name"] = r.AgentPool.Name
	}

	return gin.H{
		"id":         r.ID.String(),
		"type":       "runners",
		"attributes": attrs,
		"relationships": gin.H{
			"organization": gin.H{
				"data": gin.H{"id": r.OrganizationID.String(), "type": "organizations"},
			},
			"agent-pool": gin.H{
				"data": gin.H{"id": r.AgentPoolID.String(), "type": "agent-pools"},
			},
		},
	}
}
