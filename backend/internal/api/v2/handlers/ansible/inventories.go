// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansible

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/queue"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/ansible"
	"github.com/iac-platform/backend/internal/services/auth"
	vcs "github.com/iac-platform/backend/internal/services/vcs"
	"github.com/michielvha/logger"
)

// InventorySyncMessage represents a request to sync an inventory from VCS
type InventorySyncMessage struct {
	InventoryID uuid.UUID `json:"inventory_id"`
}

// InventoryHandler handles Ansible inventory API endpoints
type InventoryHandler struct {
	inventoryService  *ansible.InventoryService
	inventoryRepo     *repository.AnsibleInventoryRepository
	orgRepo           *repository.OrganizationRepository
	projectRepo       *repository.ProjectRepository
	authService       *auth.Service
	queue             queue.Queue
	vcsRegistry       *vcs.ProviderRegistry
	vcsConnectionRepo *repository.VCSConnectionRepository
}

// NewInventoryHandler creates a new inventory handler
func NewInventoryHandler(
	inventoryService *ansible.InventoryService,
	inventoryRepo *repository.AnsibleInventoryRepository,
	orgRepo *repository.OrganizationRepository,
	projectRepo *repository.ProjectRepository,
	authService *auth.Service,
	redisQueue queue.Queue,
	vcsRegistry *vcs.ProviderRegistry,
	vcsConnectionRepo *repository.VCSConnectionRepository,
) *InventoryHandler {
	return &InventoryHandler{
		inventoryService:  inventoryService,
		inventoryRepo:     inventoryRepo,
		orgRepo:           orgRepo,
		projectRepo:       projectRepo,
		authService:       authService,
		queue:             redisQueue,
		vcsRegistry:       vcsRegistry,
		vcsConnectionRepo: vcsConnectionRepo,
	}
}

// maybeRegisterADOWebhook registers Azure DevOps service hook subscriptions for a specific repo
// in a background goroutine. Silently skips if not ADO, no webhook base URL, or wrong repo format.
func (h *InventoryHandler) maybeRegisterADOWebhook(connID *uuid.UUID, repoPath string) {
	if connID == nil || repoPath == "" || h.vcsRegistry == nil || h.vcsConnectionRepo == nil {
		return
	}
	webhookBaseURL := os.Getenv("STACKWEAVER_WEBHOOK_BASE_URL")
	if webhookBaseURL == "" {
		return
	}
	parts := strings.SplitN(repoPath, "/", 2)
	if len(parts) != 2 {
		return
	}
	go func(id uuid.UUID, projectName, repoName string) {
		conn, err := h.vcsConnectionRepo.GetByID(id)
		if err != nil || conn.Provider != models.VCSProviderAzureDevOps {
			return
		}
		provider, err := h.vcsRegistry.GetProvider(conn)
		if err != nil {
			return
		}
		bgCtx := context.Background()
		if rErr := provider.RegisterWebhooksForRepo(bgCtx, conn, webhookBaseURL, projectName, repoName); rErr != nil {
			logger.Warnf("Failed to register ADO webhooks for ansible inventory repo %s/%s: %v", projectName, repoName, rErr)
		}
	}(*connID, parts[0], parts[1])
}

// CreateInventoryRequest represents the request to create an inventory
type CreateInventoryRequest struct {
	Data struct {
		Type       string `json:"type"`
		Attributes struct {
			Name          string                    `json:"name" binding:"required"`
			Description   string                    `json:"description"`
			Type          string                    `json:"inventory-type"`
			Source        string                    `json:"source"`
			Variables     models.InventoryVariables `json:"variables"`
			VCSRepository string                    `json:"vcs_repository"`
			VCSBranch     string                    `json:"vcs_branch"`
			InventoryPath string                    `json:"inventory_path"`
		} `json:"attributes"`
		Relationships struct {
			VCSConnection struct {
				Data *struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"vcs_connection"`
			Project struct {
				Data *struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"project"`
		} `json:"relationships"`
	} `json:"data"`
}

// UpdateInventoryRequest represents the request to update an inventory
type UpdateInventoryRequest struct {
	Data struct {
		Type       string `json:"type"`
		Attributes struct {
			Name          *string                    `json:"name"`
			Description   *string                    `json:"description"`
			Source        *string                    `json:"source"`
			Variables     *models.InventoryVariables `json:"variables"`
			VCSRepository *string                    `json:"vcs_repository"`
			VCSBranch     *string                    `json:"vcs_branch"`
			InventoryPath *string                    `json:"inventory_path"`
		} `json:"attributes"`
		Relationships struct {
			VCSConnection struct {
				Data *struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"vcs_connection"`
			Project struct {
				Data *struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"project"`
		} `json:"relationships"`
	} `json:"data"`
}

// List lists all inventories for an organization
// GET /api/v2/organizations/:name/ansible/inventories
func (h *InventoryHandler) List(c *gin.Context) {
	orgName := c.Param("name")
	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "Organization not found"},
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

	inventories, total, err := h.inventoryService.ListInventories(org.ID, perPage, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to list inventories"},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatInventoriesResponse(inventories),
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

// getOrCreateDefaultProject gets or creates the default project for an organization
func (h *InventoryHandler) getOrCreateDefaultProject(orgID uuid.UUID) (*uuid.UUID, error) {
	// Try to get existing default project
	defaultProject, err := h.projectRepo.GetByOrganizationAndName(orgID, "default")
	if err == nil && defaultProject != nil {
		return &defaultProject.ID, nil
	}

	// Create default project if it doesn't exist
	defaultProject = &models.Project{
		OrganizationID: orgID,
		Name:           "default",
		Description:    "Default project for your organization",
	}
	if err := h.projectRepo.Create(defaultProject); err != nil {
		return nil, fmt.Errorf("failed to create default project: %w", err)
	}
	return &defaultProject.ID, nil
}

// Create creates a new inventory
// POST /api/v2/organizations/:name/ansible/inventories
func (h *InventoryHandler) Create(c *gin.Context) {
	orgName := c.Param("name")
	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "Organization not found"},
			},
		})
		return
	}

	var req CreateInventoryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": err.Error()},
			},
		})
		return
	}

	// Parse inventory type
	invType := models.InventoryTypeStatic
	switch req.Data.Attributes.Type {
	case "static":
		invType = models.InventoryTypeStatic
	case "dynamic":
		invType = models.InventoryTypeDynamic
	case "vcs":
		invType = models.InventoryTypeVCS
	}

	// Parse VCS connection ID if provided
	var vcsConnectionID *uuid.UUID
	if req.Data.Relationships.VCSConnection.Data != nil {
		vid, err := uuid.Parse(req.Data.Relationships.VCSConnection.Data.ID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{"status": "400", "title": "Bad Request", "detail": "Invalid VCS connection ID"},
				},
			})
			return
		}
		vcsConnectionID = &vid
	}

	// Parse project ID if provided, otherwise use default project
	var projectID *uuid.UUID
	if req.Data.Relationships.Project.Data != nil {
		pid, err := uuid.Parse(req.Data.Relationships.Project.Data.ID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{"status": "400", "title": "Bad Request", "detail": "Invalid project ID"},
				},
			})
			return
		}
		// Verify project belongs to organization
		project, err := h.projectRepo.GetByID(pid)
		if err != nil || project.OrganizationID != org.ID {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{"status": "400", "title": "Bad Request", "detail": "Project not found or does not belong to organization"},
				},
			})
			return
		}
		projectID = &pid
	} else {
		// Use default project
		defaultProjectID, err := h.getOrCreateDefaultProject(org.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{
					{"status": "500", "title": "Internal Server Error", "detail": "Failed to get default project"},
				},
			})
			return
		}
		projectID = defaultProjectID
	}

	inventory, err := h.inventoryService.CreateInventory(
		org.ID,
		projectID,
		req.Data.Attributes.Name,
		req.Data.Attributes.Description,
		invType,
		req.Data.Attributes.Source,
		req.Data.Attributes.Variables,
		vcsConnectionID,
		req.Data.Attributes.VCSRepository,
		req.Data.Attributes.VCSBranch,
		req.Data.Attributes.InventoryPath,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": err.Error()},
			},
		})
		return
	}

	// Register ADO webhooks if this inventory is linked to an Azure DevOps repository
	h.maybeRegisterADOWebhook(inventory.VCSConnectionID, inventory.VCSRepository)

	// Auto-trigger sync for VCS-backed inventories
	if inventory.VCSConnectionID != nil && inventory.VCSRepository != "" && inventory.InventoryPath != "" && h.queue != nil {
		inventory.LastSyncStatus = "syncing"
		if err := h.inventoryRepo.Update(inventory); err != nil {
			logger.Warnf("Failed to update inventory sync status: %v", err)
		}

		syncMsg := InventorySyncMessage{
			InventoryID: inventory.ID,
		}
		if err := h.queue.Enqueue(context.Background(), "ansible_sync", syncMsg); err != nil {
			// Log error but don't fail the create request
			inventory.LastSyncStatus = "pending"
			inventory.LastSyncError = "Auto-sync failed to queue: " + err.Error()
			if updateErr := h.inventoryRepo.Update(inventory); updateErr != nil {
				logger.Warnf("Failed to update inventory after sync queue error: %v", updateErr)
			}
		}
	}

	c.JSON(http.StatusCreated, gin.H{
		"data": formatInventoryResponse(inventory),
	})
}

// Get retrieves an inventory by ID
// GET /api/v2/ansible/inventories/:id
func (h *InventoryHandler) Get(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid inventory ID"},
			},
		})
		return
	}

	inventory, err := h.inventoryService.GetInventory(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "Inventory not found"},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatInventoryResponse(inventory),
	})
}

// Update updates an inventory
// PATCH /api/v2/ansible/inventories/:id
func (h *InventoryHandler) Update(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid inventory ID"},
			},
		})
		return
	}

	var req UpdateInventoryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": err.Error()},
			},
		})
		return
	}

	// Parse VCS connection ID if provided
	var vcsConnectionID *uuid.UUID
	if req.Data.Relationships.VCSConnection.Data != nil {
		vid, err := uuid.Parse(req.Data.Relationships.VCSConnection.Data.ID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{"status": "400", "title": "Bad Request", "detail": "Invalid VCS connection ID"},
				},
			})
			return
		}
		vcsConnectionID = &vid
	}

	// Parse project ID if provided
	var projectID *uuid.UUID
	if req.Data.Relationships.Project.Data != nil {
		pid, err := uuid.Parse(req.Data.Relationships.Project.Data.ID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{"status": "400", "title": "Bad Request", "detail": "Invalid project ID"},
				},
			})
			return
		}
		// Get existing inventory to verify organization
		existingInventory, err := h.inventoryRepo.GetByID(id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{
				"errors": []gin.H{
					{"status": "404", "title": "Not Found", "detail": "Inventory not found"},
				},
			})
			return
		}
		// Verify project belongs to same organization
		project, err := h.projectRepo.GetByID(pid)
		if err != nil || project.OrganizationID != existingInventory.OrganizationID {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{"status": "400", "title": "Bad Request", "detail": "Project not found or does not belong to organization"},
				},
			})
			return
		}
		projectID = &pid
	}

	inventory, err := h.inventoryService.UpdateInventory(
		id,
		projectID,
		req.Data.Attributes.Name,
		req.Data.Attributes.Description,
		req.Data.Attributes.Source,
		req.Data.Attributes.Variables,
		vcsConnectionID,
		req.Data.Attributes.VCSRepository,
		req.Data.Attributes.VCSBranch,
		req.Data.Attributes.InventoryPath,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": err.Error()},
			},
		})
		return
	}

	// Register ADO webhooks if this inventory is linked to an Azure DevOps repository
	h.maybeRegisterADOWebhook(inventory.VCSConnectionID, inventory.VCSRepository)

	c.JSON(http.StatusOK, gin.H{
		"data": formatInventoryResponse(inventory),
	})
}

// Delete deletes an inventory
// DELETE /api/v2/ansible/inventories/:id
func (h *InventoryHandler) Delete(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid inventory ID"},
			},
		})
		return
	}

	if err := h.inventoryService.DeleteInventory(id); err != nil {
		// Check if it's a dependency error (contains "cannot delete" or "referenced")
		errStr := err.Error()
		if strings.Contains(errStr, "cannot delete") || strings.Contains(errStr, "referenced") {
			c.JSON(http.StatusConflict, gin.H{
				"errors": []gin.H{
					{"status": "409", "title": "Conflict", "detail": err.Error()},
				},
			})
			return
		}

		// Check for foreign key constraint violation (fallback)
		if strings.Contains(errStr, "violates foreign key constraint") {
			c.JSON(http.StatusConflict, gin.H{
				"errors": []gin.H{
					{"status": "409", "title": "Conflict", "detail": "Cannot delete inventory: it is referenced by one or more job templates, jobs, or inventory sources. Remove the inventory from those resources first."},
				},
			})
			return
		}

		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": err.Error()},
			},
		})
		return
	}

	c.Status(http.StatusNoContent)
}

// GetInventoryINI returns the inventory in INI format
// GET /api/v2/ansible/inventories/:id/ini
func (h *InventoryHandler) GetInventoryINI(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid inventory ID"},
			},
		})
		return
	}

	content, err := h.inventoryService.GenerateInventoryINI(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": err.Error()},
			},
		})
		return
	}

	c.Header("Content-Type", "text/plain")
	c.String(http.StatusOK, content)
}

// GetInventoryJSON returns the inventory in JSON format
// GET /api/v2/ansible/inventories/:id/json
func (h *InventoryHandler) GetInventoryJSON(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid inventory ID"},
			},
		})
		return
	}

	content, err := h.inventoryService.GenerateInventoryJSON(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": err.Error()},
			},
		})
		return
	}

	c.Header("Content-Type", "application/json")
	c.String(http.StatusOK, content)
}

// SyncInventory syncs a VCS inventory from repository
// POST /api/v2/ansible/inventories/:id/actions/sync
func (h *InventoryHandler) SyncInventory(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid inventory ID"},
			},
		})
		return
	}

	inventory, err := h.inventoryRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "Inventory not found"},
			},
		})
		return
	}

	// Check if inventory has VCS configuration
	if inventory.VCSConnectionID == nil || inventory.VCSRepository == "" || inventory.InventoryPath == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Inventory has no VCS connection configured"},
			},
		})
		return
	}

	// Update sync status to syncing
	inventory.LastSyncStatus = "syncing"
	inventory.LastSyncError = ""
	if err := h.inventoryRepo.Update(inventory); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to update sync status"},
			},
		})
		return
	}

	// Queue sync job
	if h.queue != nil {
		syncMsg := InventorySyncMessage{
			InventoryID: inventory.ID,
		}
		if err := h.queue.Enqueue(context.Background(), "ansible_sync", syncMsg); err != nil {
			// Revert status on queue failure
			inventory.LastSyncStatus = "failed"
			inventory.LastSyncError = "Failed to queue sync job: " + err.Error()
			if updateErr := h.inventoryRepo.Update(inventory); updateErr != nil {
				logger.Warnf("Failed to update inventory after sync queue error: %v", updateErr)
			}

			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{
					{"status": "500", "title": "Internal Server Error", "detail": "Failed to queue sync job"},
				},
			})
			return
		}
	}

	c.JSON(http.StatusAccepted, gin.H{
		"data": formatInventoryResponse(inventory),
	})
}

// formatInventoryResponse formats an inventory for JSON:API response
func formatInventoryResponse(inv *models.AnsibleInventory) gin.H {
	attributes := gin.H{
		"name":                       inv.Name,
		"description":                inv.Description,
		"inventory-type":             inv.Type,
		"source":                     inv.Source,
		"variables":                  inv.Variables,
		"last-sync-at":               inv.LastSyncAt,
		"last-sync-status":           inv.LastSyncStatus,
		"last-sync-error":            inv.LastSyncError,
		"last-sync-hosts-discovered": inv.LastSyncHostsDiscovered,
		"last-sync-log":              inv.LastSyncLog,
		"created-at":                 inv.CreatedAt.Format("2006-01-02T15:04:05Z"),
		"updated-at":                 inv.UpdatedAt.Format("2006-01-02T15:04:05Z"),
	}

	// Add VCS fields if present
	if inv.VCSConnectionID != nil {
		attributes["vcs_connection_id"] = inv.VCSConnectionID.String()
	}
	if inv.VCSRepository != "" {
		attributes["vcs_repository"] = inv.VCSRepository
	}
	if inv.VCSBranch != "" {
		attributes["vcs_branch"] = inv.VCSBranch
	}
	if inv.InventoryPath != "" {
		attributes["inventory_path"] = inv.InventoryPath
	}

	relationships := gin.H{
		"organization": gin.H{
			"data": gin.H{
				"id":   inv.OrganizationID.String(),
				"type": "organizations",
			},
		},
	}

	// Add VCS connection relationship if present
	if inv.VCSConnectionID != nil {
		relationships["vcs_connection"] = gin.H{
			"data": gin.H{
				"id":   inv.VCSConnectionID.String(),
				"type": "vcs-connections",
			},
		}
	}

	return gin.H{
		"id":            inv.ID.String(),
		"type":          "ansible-inventories",
		"attributes":    attributes,
		"relationships": relationships,
	}
}

// formatInventoriesResponse formats multiple inventories for JSON:API response
func formatInventoriesResponse(inventories []models.AnsibleInventory) []gin.H {
	result := make([]gin.H, len(inventories))
	for i, inv := range inventories {
		result[i] = formatInventoryResponse(&inv)
	}
	return result
}
