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

// HostHandler handles Ansible inventory host API endpoints
type HostHandler struct {
	inventoryService *ansible.InventoryService
	inventoryRepo    *repository.AnsibleInventoryRepository
	authService      *auth.Service
}

// NewHostHandler creates a new host handler
func NewHostHandler(
	inventoryService *ansible.InventoryService,
	inventoryRepo *repository.AnsibleInventoryRepository,
	authService *auth.Service,
) *HostHandler {
	return &HostHandler{
		inventoryService: inventoryService,
		inventoryRepo:    inventoryRepo,
		authService:      authService,
	}
}

// CreateHostRequest represents the request to create a host
type CreateHostRequest struct {
	Data struct {
		Type       string `json:"type"`
		Attributes struct {
			Name        string                    `json:"name" binding:"required"`
			Description string                    `json:"description"`
			Hostname    string                    `json:"hostname"`
			Port        int                       `json:"port"`
			Variables   models.InventoryVariables `json:"variables"`
			Enabled     *bool                     `json:"enabled"`
		} `json:"attributes"`
	} `json:"data"`
}

// UpdateHostRequest represents the request to update a host
type UpdateHostRequest struct {
	Data struct {
		Type       string `json:"type"`
		Attributes struct {
			Name        *string                    `json:"name"`
			Description *string                    `json:"description"`
			Hostname    *string                    `json:"hostname"`
			Port        *int                       `json:"port"`
			Variables   *models.InventoryVariables `json:"variables"`
			Enabled     *bool                      `json:"enabled"`
		} `json:"attributes"`
	} `json:"data"`
}

// List lists all hosts in an inventory
// GET /api/v2/ansible/inventories/:id/hosts
func (h *HostHandler) List(c *gin.Context) {
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

	hosts, total, err := h.inventoryService.ListHosts(inventoryID, perPage, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to list hosts"},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatHostsResponse(hosts),
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

// Create creates a new host in an inventory
// POST /api/v2/ansible/inventories/:id/hosts
func (h *HostHandler) Create(c *gin.Context) {
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

	var req CreateHostRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": err.Error()},
			},
		})
		return
	}

	enabled := true
	if req.Data.Attributes.Enabled != nil {
		enabled = *req.Data.Attributes.Enabled
	}

	port := req.Data.Attributes.Port
	if port == 0 {
		port = 22
	}

	host, err := h.inventoryService.CreateHost(
		inventoryID,
		req.Data.Attributes.Name,
		req.Data.Attributes.Description,
		req.Data.Attributes.Hostname,
		port,
		req.Data.Attributes.Variables,
		enabled,
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
		"data": formatHostResponse(host),
	})
}

// Get retrieves a host by ID
// GET /api/v2/ansible/hosts/:id
func (h *HostHandler) Get(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid host ID"},
			},
		})
		return
	}

	host, err := h.inventoryService.GetHost(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "Host not found"},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatHostResponse(host),
	})
}

// Update updates a host
// PATCH /api/v2/ansible/hosts/:id
func (h *HostHandler) Update(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid host ID"},
			},
		})
		return
	}

	var req UpdateHostRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": err.Error()},
			},
		})
		return
	}

	host, err := h.inventoryService.UpdateHost(
		id,
		req.Data.Attributes.Name,
		req.Data.Attributes.Description,
		req.Data.Attributes.Hostname,
		req.Data.Attributes.Port,
		req.Data.Attributes.Variables,
		req.Data.Attributes.Enabled,
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
		"data": formatHostResponse(host),
	})
}

// Delete deletes a host
// DELETE /api/v2/ansible/hosts/:id
func (h *HostHandler) Delete(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid host ID"},
			},
		})
		return
	}

	if err := h.inventoryService.DeleteHost(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": err.Error()},
			},
		})
		return
	}

	c.Status(http.StatusNoContent)
}

// AddToGroup adds a host to a group
// POST /api/v2/ansible/hosts/:id/groups/:group_id
func (h *HostHandler) AddToGroup(c *gin.Context) {
	hostIDStr := c.Param("id")
	hostID, err := uuid.Parse(hostIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid host ID"},
			},
		})
		return
	}

	groupIDStr := c.Param("group_id")
	groupID, err := uuid.Parse(groupIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid group ID"},
			},
		})
		return
	}

	if err := h.inventoryService.AddHostToGroup(hostID, groupID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": err.Error()},
			},
		})
		return
	}

	c.Status(http.StatusNoContent)
}

// RemoveFromGroup removes a host from a group
// DELETE /api/v2/ansible/hosts/:id/groups/:group_id
func (h *HostHandler) RemoveFromGroup(c *gin.Context) {
	hostIDStr := c.Param("id")
	hostID, err := uuid.Parse(hostIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid host ID"},
			},
		})
		return
	}

	groupIDStr := c.Param("group_id")
	groupID, err := uuid.Parse(groupIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid group ID"},
			},
		})
		return
	}

	if err := h.inventoryService.RemoveHostFromGroup(hostID, groupID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": err.Error()},
			},
		})
		return
	}

	c.Status(http.StatusNoContent)
}

// formatHostResponse formats a host for JSON:API response
func formatHostResponse(host *models.AnsibleInventoryHost) gin.H {
	groups := make([]gin.H, len(host.Groups))
	for i, group := range host.Groups {
		groups[i] = gin.H{
			"id":   group.ID.String(),
			"type": "ansible-groups",
		}
	}

	return gin.H{
		"id":   host.ID.String(),
		"type": "ansible-hosts",
		"attributes": gin.H{
			"name":        host.Name,
			"description": host.Description,
			"hostname":    host.Hostname,
			"port":        host.Port,
			"variables":   host.Variables,
			"enabled":     host.Enabled,
			"created-at":  host.CreatedAt.Format("2006-01-02T15:04:05Z"),
			"updated-at":  host.UpdatedAt.Format("2006-01-02T15:04:05Z"),
		},
		"relationships": gin.H{
			"inventory": gin.H{
				"data": gin.H{
					"id":   host.InventoryID.String(),
					"type": "ansible-inventories",
				},
			},
			"groups": gin.H{
				"data": groups,
			},
		},
	}
}

// formatHostsResponse formats multiple hosts for JSON:API response
func formatHostsResponse(hosts []models.AnsibleInventoryHost) []gin.H {
	result := make([]gin.H, len(hosts))
	for i, host := range hosts {
		result[i] = formatHostResponse(&host)
	}
	return result
}
