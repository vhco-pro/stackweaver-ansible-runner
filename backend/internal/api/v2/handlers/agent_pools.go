// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/rbac"
	"gorm.io/gorm"
)

// AgentPoolHandlerV2 handles TFE-compatible agent pool API.
type AgentPoolHandlerV2 struct {
	poolRepo    *repository.AgentPoolRepository
	runnerRepo  *repository.RunnerRepository
	orgRepo     *repository.OrganizationRepository
	authService *auth.Service
	rbacService *rbac.Service
}

// NewAgentPoolHandlerV2 creates an AgentPoolHandlerV2.
func NewAgentPoolHandlerV2(
	poolRepo *repository.AgentPoolRepository,
	runnerRepo *repository.RunnerRepository,
	orgRepo *repository.OrganizationRepository,
	authService *auth.Service,
	rbacService *rbac.Service,
) *AgentPoolHandlerV2 {
	return &AgentPoolHandlerV2{
		poolRepo:    poolRepo,
		runnerRepo:  runnerRepo,
		orgRepo:     orgRepo,
		authService: authService,
		rbacService: rbacService,
	}
}

// CreateAgentPoolRequestV2 uses JSON:API format (TFE-compatible).
type CreateAgentPoolRequestV2 struct {
	Data struct {
		Type       string `json:"type" binding:"required"`
		Attributes struct {
			Name               string `json:"name" binding:"required"`
			OrganizationScoped *bool  `json:"organization-scoped,omitempty"`
		} `json:"attributes" binding:"required"`
		Relationships *struct {
			AllowedWorkspaces  *jsonAPIRelationship `json:"allowed-workspaces,omitempty"`
			AllowedProjects    *jsonAPIRelationship `json:"allowed-projects,omitempty"`
			ExcludedWorkspaces *jsonAPIRelationship `json:"excluded-workspaces,omitempty"`
		} `json:"relationships,omitempty"`
	} `json:"data" binding:"required"`
}

// UpdateAgentPoolRequestV2 uses JSON:API format (TFE-compatible).
type UpdateAgentPoolRequestV2 struct {
	Data struct {
		Type       string `json:"type" binding:"required"`
		Attributes *struct {
			Name               *string `json:"name,omitempty"`
			OrganizationScoped *bool   `json:"organization-scoped,omitempty"`
		} `json:"attributes,omitempty"`
		Relationships *struct {
			AllowedWorkspaces  *jsonAPIRelationship `json:"allowed-workspaces,omitempty"`
			AllowedProjects    *jsonAPIRelationship `json:"allowed-projects,omitempty"`
			ExcludedWorkspaces *jsonAPIRelationship `json:"excluded-workspaces,omitempty"`
		} `json:"relationships,omitempty"`
	} `json:"data" binding:"required"`
}

// jsonAPIRelationship wraps relationship data in JSON:API format.
type jsonAPIRelationship struct {
	Data []jsonAPIRef `json:"data"`
}

type jsonAPIRef struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

func formatAgentPoolResponse(p *models.AgentPool, orgName string, agentCount int) gin.H {
	attrs := gin.H{
		"name":                p.Name,
		"agent-count":         agentCount,
		"organization-scoped": p.OrganizationScoped,
		"created-at":          p.CreatedAt.Format(time.RFC3339),
	}
	rel := gin.H{
		"organization": gin.H{
			"data": gin.H{"id": orgName, "type": "organizations"},
		},
	}
	if len(p.AllowedWorkspaces) > 0 {
		var refs []gin.H
		for _, w := range p.AllowedWorkspaces {
			refs = append(refs, gin.H{"id": w.ID, "type": "workspaces"})
		}
		rel["allowed-workspaces"] = gin.H{"data": refs}
	}
	if len(p.AllowedProjects) > 0 {
		var refs []gin.H
		for _, pr := range p.AllowedProjects {
			refs = append(refs, gin.H{"id": pr.ID.String(), "type": "projects"})
		}
		rel["allowed-projects"] = gin.H{"data": refs}
	}
	if len(p.ExcludedWorkspaces) > 0 {
		var refs []gin.H
		for _, w := range p.ExcludedWorkspaces {
			refs = append(refs, gin.H{"id": w.ID, "type": "workspaces"})
		}
		rel["excluded-workspaces"] = gin.H{"data": refs}
	}
	return gin.H{
		"id":            p.ID.String(),
		"type":          "agent-pools",
		"attributes":    attrs,
		"relationships": rel,
		"links":         gin.H{"self": "/api/v2/agent-pools/" + p.ID.String()},
	}
}

func (h *AgentPoolHandlerV2) requireManageAgentPools(c *gin.Context, orgID uuid.UUID) bool {
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}}})
		return false
	}
	ok, err := h.rbacService.CheckOrgManageAgentPools(c.Request.Context(), user.ID, orgID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to check permissions"}}})
		return false
	}
	if !ok {
		c.JSON(http.StatusForbidden, gin.H{"errors": []gin.H{{"status": "403", "title": "Forbidden", "detail": "You do not have permission to manage agent pools"}}})
		return false
	}
	return true
}

// List returns agent pools for an organization.
// GET /api/v2/organizations/:name/agent-pools
func (h *AgentPoolHandlerV2) List(c *gin.Context) {
	orgName := c.Param("name")
	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}}})
		return
	}
	if !h.requireManageAgentPools(c, org.ID) {
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page[number]", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page[size]", "20"))
	if pageSize > 100 {
		pageSize = 100
	}
	offset := (page - 1) * pageSize
	if offset < 0 {
		offset = 0
	}
	opts := repository.ListAgentPoolsOptions{
		Query:  c.Query("q"),
		Sort:   c.DefaultQuery("sort", "created-at"),
		Limit:  pageSize,
		Offset: offset,
	}

	pools, total, err := h.poolRepo.ListByOrganization(org.ID, opts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to list agent pools"}}})
		return
	}

	data := make([]gin.H, 0, len(pools))
	for i := range pools {
		agentCount := 0
		if h.runnerRepo != nil {
			if n, err := h.runnerRepo.CountByAgentPool(pools[i].ID); err == nil {
				agentCount = int(n)
			}
		}
		data = append(data, formatAgentPoolResponse(&pools[i], org.Name, agentCount))
	}

	c.JSON(http.StatusOK, gin.H{
		"data": data,
		"meta": gin.H{
			"pagination": gin.H{
				"current-page": page,
				"page-size":    pageSize,
				"total-count":  total,
			},
		},
	})
}

// Create creates an agent pool.
// POST /api/v2/organizations/:name/agent-pools
func (h *AgentPoolHandlerV2) Create(c *gin.Context) {
	orgName := c.Param("name")
	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}}})
		return
	}
	if !h.requireManageAgentPools(c, org.ID) {
		return
	}

	var req CreateAgentPoolRequestV2
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}}})
		return
	}
	if req.Data.Type != "agent-pools" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "data.type must be 'agent-pools'"}}})
		return
	}

	name := req.Data.Attributes.Name
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "name is required"}}})
		return
	}
	orgScoped := true
	if req.Data.Attributes.OrganizationScoped != nil {
		orgScoped = *req.Data.Attributes.OrganizationScoped
	}

	existing, _ := h.poolRepo.GetByOrganizationAndName(org.ID, name)
	if existing != nil {
		c.JSON(http.StatusConflict, gin.H{"errors": []gin.H{{"status": "409", "title": "Conflict", "detail": "Agent pool with this name already exists"}}})
		return
	}

	pool := &models.AgentPool{
		OrganizationID:     org.ID,
		Name:               name,
		OrganizationScoped: orgScoped,
	}
	if err := h.poolRepo.Create(pool); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to create agent pool"}}})
		return
	}

	// Apply relations if provided
	if req.Data.Relationships != nil {
		if req.Data.Relationships.AllowedWorkspaces != nil {
			ids := extractWorkspaceIDs(req.Data.Relationships.AllowedWorkspaces.Data)
			_ = h.poolRepo.ReplaceAllowedWorkspaces(pool.ID, ids)
		}
		if req.Data.Relationships.AllowedProjects != nil {
			ids := extractProjectIDs(req.Data.Relationships.AllowedProjects.Data)
			_ = h.poolRepo.ReplaceAllowedProjects(pool.ID, ids)
		}
		if req.Data.Relationships.ExcludedWorkspaces != nil {
			ids := extractWorkspaceIDs(req.Data.Relationships.ExcludedWorkspaces.Data)
			_ = h.poolRepo.ReplaceExcludedWorkspaces(pool.ID, ids)
		}
	}

	// Reload with relations
	updated, _ := h.poolRepo.GetByID(pool.ID, true)
	if updated != nil {
		pool = updated
	}
	agentCount := 0
	if h.runnerRepo != nil {
		if n, err := h.runnerRepo.CountByAgentPool(pool.ID); err == nil {
			agentCount = int(n)
		}
	}
	c.JSON(http.StatusCreated, gin.H{"data": formatAgentPoolResponse(pool, org.Name, agentCount)})
}

// GetByID returns a single agent pool by ID.
// GET /api/v2/agent-pools/:id
func (h *AgentPoolHandlerV2) GetByID(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "invalid agent pool id"}}})
		return
	}

	pool, err := h.poolRepo.GetByID(id, true)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Agent pool not found"}}})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to get agent pool"}}})
		return
	}

	org, _ := h.orgRepo.GetByID(pool.OrganizationID)
	orgName := ""
	if org != nil {
		orgName = org.Name
	}
	if !h.requireManageAgentPools(c, pool.OrganizationID) {
		return
	}

	agentCount := 0
	if h.runnerRepo != nil {
		if n, err := h.runnerRepo.CountByAgentPool(pool.ID); err == nil {
			agentCount = int(n)
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": formatAgentPoolResponse(pool, orgName, agentCount)})
}

// Update updates an agent pool (name, organization-scoped) or relation-only updates (allowed/excluded).
// PATCH /api/v2/agent-pools/:id
func (h *AgentPoolHandlerV2) Update(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "invalid agent pool id"}}})
		return
	}

	pool, err := h.poolRepo.GetByID(id, true)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Agent pool not found"}}})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to get agent pool"}}})
		return
	}
	org, _ := h.orgRepo.GetByID(pool.OrganizationID)
	orgName := ""
	if org != nil {
		orgName = org.Name
	}
	if !h.requireManageAgentPools(c, pool.OrganizationID) {
		return
	}

	var req UpdateAgentPoolRequestV2
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}}})
		return
	}
	if req.Data.Type != "agent-pools" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "data.type must be 'agent-pools'"}}})
		return
	}

	// Relation-only updates (TFE UpdateAllowedWorkspaces / UpdateAllowedProjects / UpdateExcludedWorkspaces)
	if req.Data.Relationships != nil {
		if req.Data.Relationships.AllowedWorkspaces != nil {
			ids := extractWorkspaceIDs(req.Data.Relationships.AllowedWorkspaces.Data)
			if err := h.poolRepo.ReplaceAllowedWorkspaces(id, ids); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to update allowed workspaces"}}})
				return
			}
		}
		if req.Data.Relationships.AllowedProjects != nil {
			ids := extractProjectIDs(req.Data.Relationships.AllowedProjects.Data)
			if err := h.poolRepo.ReplaceAllowedProjects(id, ids); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to update allowed projects"}}})
				return
			}
		}
		if req.Data.Relationships.ExcludedWorkspaces != nil {
			ids := extractWorkspaceIDs(req.Data.Relationships.ExcludedWorkspaces.Data)
			if err := h.poolRepo.ReplaceExcludedWorkspaces(id, ids); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to update excluded workspaces"}}})
				return
			}
		}
	}

	// Attribute updates
	if req.Data.Attributes != nil {
		if req.Data.Attributes.Name != nil && *req.Data.Attributes.Name != "" {
			pool.Name = *req.Data.Attributes.Name
		}
		if req.Data.Attributes.OrganizationScoped != nil {
			pool.OrganizationScoped = *req.Data.Attributes.OrganizationScoped
		}
		if err := h.poolRepo.Update(pool); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to update agent pool"}}})
			return
		}
	}

	updated, _ := h.poolRepo.GetByID(id, true)
	if updated != nil {
		pool = updated
	}
	agentCount := 0
	if h.runnerRepo != nil {
		if n, err := h.runnerRepo.CountByAgentPool(pool.ID); err == nil {
			agentCount = int(n)
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": formatAgentPoolResponse(pool, orgName, agentCount)})
}

// Delete deletes an agent pool.
// DELETE /api/v2/agent-pools/:id
func (h *AgentPoolHandlerV2) Delete(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "invalid agent pool id"}}})
		return
	}

	pool, err := h.poolRepo.GetByID(id, false)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Agent pool not found"}}})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to get agent pool"}}})
		return
	}
	if !h.requireManageAgentPools(c, pool.OrganizationID) {
		return
	}

	if err := h.poolRepo.Delete(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to delete agent pool"}}})
		return
	}
	c.Status(http.StatusNoContent)
}

// ListAgents returns agents (runners) in a pool. TFE-compatible; runners not implemented yet, returns empty list.
// GET /api/v2/agent-pools/:id/agents
func (h *AgentPoolHandlerV2) ListAgents(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "invalid agent pool id"}}})
		return
	}

	pool, err := h.poolRepo.GetByID(id, false)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Agent pool not found"}}})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to get agent pool"}}})
		return
	}
	if !h.requireManageAgentPools(c, pool.OrganizationID) {
		return
	}

	// Runners not implemented yet; return empty list (TFE Agent shape: id, name, ip-address, status, last-ping-at)
	c.JSON(http.StatusOK, gin.H{
		"data": []gin.H{},
		"meta": gin.H{"pagination": gin.H{"current-page": 1, "page-size": 20, "total-count": 0}},
	})
}

func extractWorkspaceIDs(refs []jsonAPIRef) []string {
	ids := make([]string, 0, len(refs))
	for _, r := range refs {
		if r.ID != "" {
			ids = append(ids, r.ID)
		}
	}
	return ids
}

func extractProjectIDs(refs []jsonAPIRef) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(refs))
	for _, r := range refs {
		if r.ID != "" {
			if u, err := uuid.Parse(r.ID); err == nil {
				ids = append(ids, u)
			}
		}
	}
	return ids
}
