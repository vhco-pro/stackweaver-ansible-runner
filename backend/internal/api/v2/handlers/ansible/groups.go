// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansible

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/ansible"
	"github.com/iac-platform/backend/internal/services/auth"
)

// GroupHandler handles Ansible inventory group API endpoints
type GroupHandler struct {
	inventoryService *ansible.InventoryService
	inventoryRepo    *repository.AnsibleInventoryRepository
	authService      *auth.Service
}

// NewGroupHandler creates a new group handler
func NewGroupHandler(
	inventoryService *ansible.InventoryService,
	inventoryRepo *repository.AnsibleInventoryRepository,
	authService *auth.Service,
) *GroupHandler {
	return &GroupHandler{
		inventoryService: inventoryService,
		inventoryRepo:    inventoryRepo,
		authService:      authService,
	}
}

// CreateGroupRequest represents the request to create a group
type CreateGroupRequest struct {
	Data struct {
		Type       string `json:"type"`
		Attributes struct {
			Name        string                    `json:"name" binding:"required"`
			Description string                    `json:"description"`
			Variables   models.InventoryVariables `json:"variables"`
		} `json:"attributes"`
		Relationships struct {
			Parent struct {
				Data *struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"parent,omitempty"`
		} `json:"relationships,omitempty"`
	} `json:"data"`
}

// UpdateGroupRequest represents the request to update a group
type UpdateGroupRequest struct {
	Data struct {
		Type       string `json:"type"`
		Attributes struct {
			Name        *string                    `json:"name"`
			Description *string                    `json:"description"`
			Variables   *models.InventoryVariables `json:"variables"`
		} `json:"attributes"`
		Relationships struct {
			Parent struct {
				Data *struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"parent,omitempty"`
		} `json:"relationships,omitempty"`
	} `json:"data"`
}

// List lists all groups in an inventory
// GET /api/v2/ansible/inventories/:id/groups
func (h *GroupHandler) List(c *gin.Context) {
	inventoryIDStr := c.Param("id")
	inventoryID, err := uuid.Parse(inventoryIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid inventory ID"},
			},
		})
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page[number]", "1"))
	perPage, _ := strconv.Atoi(c.DefaultQuery("page[size]", "20"))
	if perPage > 100 {
		perPage = 100
	}
	offset := (page - 1) * perPage

	groups, total, err := h.inventoryService.ListGroups(inventoryID, perPage, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to list groups"},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatGroupsResponse(groups),
		"meta": gin.H{
			"pagination": gin.H{
				"current-page": page,
				"page-size":    perPage,
				"total-count":  total,
				"total-pages":  (total + int64(perPage) - 1) / int64(perPage),
			},
		},
	})
}

// Create creates a new group in an inventory
// POST /api/v2/ansible/inventories/:id/groups
func (h *GroupHandler) Create(c *gin.Context) {
	inventoryIDStr := c.Param("id")
	inventoryID, err := uuid.Parse(inventoryIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid inventory ID"},
			},
		})
		return
	}

	var req CreateGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": err.Error()},
			},
		})
		return
	}

	var parentID *uuid.UUID
	if req.Data.Relationships.Parent.Data != nil {
		pid, err := uuid.Parse(req.Data.Relationships.Parent.Data.ID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{"status": "400", "title": "Bad Request", "detail": "Invalid parent group ID"},
				},
			})
			return
		}
		parentID = &pid
	}

	group, err := h.inventoryService.CreateGroup(
		inventoryID,
		req.Data.Attributes.Name,
		req.Data.Attributes.Description,
		req.Data.Attributes.Variables,
		parentID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": err.Error()},
			},
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"data": formatGroupResponse(group),
	})
}

// Get retrieves a group by ID
// GET /api/v2/ansible/groups/:id
func (h *GroupHandler) Get(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid group ID"},
			},
		})
		return
	}

	group, err := h.inventoryService.GetGroup(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "Group not found"},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatGroupResponse(group),
	})
}

// Update updates a group
// PATCH /api/v2/ansible/groups/:id
func (h *GroupHandler) Update(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid group ID"},
			},
		})
		return
	}

	var req UpdateGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": err.Error()},
			},
		})
		return
	}

	var parentID *uuid.UUID
	if req.Data.Relationships.Parent.Data != nil {
		pid, err := uuid.Parse(req.Data.Relationships.Parent.Data.ID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{"status": "400", "title": "Bad Request", "detail": "Invalid parent group ID"},
				},
			})
			return
		}
		parentID = &pid
	}

	group, err := h.inventoryService.UpdateGroup(
		id,
		req.Data.Attributes.Name,
		req.Data.Attributes.Description,
		req.Data.Attributes.Variables,
		parentID,
		false, // Don't clear parent unless explicitly requested
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": err.Error()},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatGroupResponse(group),
	})
}

// Delete deletes a group
// DELETE /api/v2/ansible/groups/:id
func (h *GroupHandler) Delete(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid group ID"},
			},
		})
		return
	}

	if err := h.inventoryService.DeleteGroup(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": err.Error()},
			},
		})
		return
	}

	c.Status(http.StatusNoContent)
}

// formatGroupResponse formats a group for JSON:API response
func formatGroupResponse(group *models.AnsibleInventoryGroup) gin.H {
	hosts := make([]gin.H, len(group.Hosts))
	for i, host := range group.Hosts {
		hosts[i] = gin.H{
			"id":   host.ID.String(),
			"type": "ansible-hosts",
		}
	}

	children := make([]gin.H, len(group.Children))
	for i, child := range group.Children {
		children[i] = gin.H{
			"id":   child.ID.String(),
			"type": "ansible-groups",
		}
	}

	relationships := gin.H{
		"inventory": gin.H{
			"data": gin.H{
				"id":   group.InventoryID.String(),
				"type": "ansible-inventories",
			},
		},
		"hosts": gin.H{
			"data": hosts,
		},
		"children": gin.H{
			"data": children,
		},
	}

	if group.ParentID != nil {
		relationships["parent"] = gin.H{
			"data": gin.H{
				"id":   group.ParentID.String(),
				"type": "ansible-groups",
			},
		}
	}

	return gin.H{
		"id":   group.ID.String(),
		"type": "ansible-groups",
		"attributes": gin.H{
			"name":        group.Name,
			"description": group.Description,
			"variables":   group.Variables,
			"created-at":  group.CreatedAt.Format("2006-01-02T15:04:05Z"),
			"updated-at":  group.UpdatedAt.Format("2006-01-02T15:04:05Z"),
		},
		"relationships": relationships,
	}
}

// formatGroupsResponse formats multiple groups for JSON:API response
func formatGroupsResponse(groups []models.AnsibleInventoryGroup) []gin.H {
	result := make([]gin.H, len(groups))
	for i, group := range groups {
		result[i] = formatGroupResponse(&group)
	}
	return result
}
