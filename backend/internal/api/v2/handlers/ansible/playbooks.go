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
	"github.com/iac-platform/backend/internal/services/auth"
	vcs "github.com/iac-platform/backend/internal/services/vcs"
	"github.com/michielvha/logger"
)

// PlaybookSyncMessage represents a request to sync a playbook from VCS
type PlaybookSyncMessage struct {
	PlaybookID uuid.UUID `json:"playbook_id"`
}

// PlaybookHandler handles Ansible playbook API endpoints
type PlaybookHandler struct {
	playbookRepo      *repository.AnsiblePlaybookRepository
	templateRepo      *repository.AnsibleJobTemplateRepository
	jobRepo           *repository.AnsibleJobRepository
	scheduleRepo      *repository.AnsibleScheduleRepository
	projectRepo       *repository.ProjectRepository
	orgRepo           *repository.OrganizationRepository
	authService       *auth.Service
	queue             queue.Queue
	vcsRegistry       *vcs.ProviderRegistry
	vcsConnectionRepo *repository.VCSConnectionRepository
}

// NewPlaybookHandler creates a new playbook handler
func NewPlaybookHandler(
	playbookRepo *repository.AnsiblePlaybookRepository,
	templateRepo *repository.AnsibleJobTemplateRepository,
	jobRepo *repository.AnsibleJobRepository,
	scheduleRepo *repository.AnsibleScheduleRepository,
	projectRepo *repository.ProjectRepository,
	orgRepo *repository.OrganizationRepository,
	authService *auth.Service,
	redisQueue queue.Queue,
	vcsRegistry *vcs.ProviderRegistry,
	vcsConnectionRepo *repository.VCSConnectionRepository,
) *PlaybookHandler {
	return &PlaybookHandler{
		playbookRepo:      playbookRepo,
		templateRepo:      templateRepo,
		jobRepo:           jobRepo,
		scheduleRepo:      scheduleRepo,
		projectRepo:       projectRepo,
		orgRepo:           orgRepo,
		authService:       authService,
		queue:             redisQueue,
		vcsRegistry:       vcsRegistry,
		vcsConnectionRepo: vcsConnectionRepo,
	}
}

// maybeRegisterADOWebhook registers Azure DevOps service hook subscriptions for a specific repo
// in a background goroutine. Silently skips if not ADO, no webhook base URL, or wrong repo format.
func (h *PlaybookHandler) maybeRegisterADOWebhook(connID *uuid.UUID, repoPath string) {
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
			logger.Warnf("Failed to register ADO webhooks for ansible playbook repo %s/%s: %v", projectName, repoName, rErr)
		}
	}(*connID, parts[0], parts[1])
}

// CreatePlaybookRequest represents the request to create a playbook
type CreatePlaybookRequest struct {
	Data struct {
		Type       string `json:"type"`
		Attributes struct {
			Name          string `json:"name"`
			Description   string `json:"description"`
			VCSRepository string `json:"vcs-repository"` // Repository full name (e.g., "owner/repo")
			VCSBranch     string `json:"vcs-branch"`     // Branch to use (defaults to "main")
			PlaybookPath  string `json:"playbook-path"`  // Path to playbook file in repo
		} `json:"attributes"`
		Relationships struct {
			Project struct {
				Data *struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"project,omitempty"`
			VCSConnection struct {
				Data *struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"vcs-connection,omitempty"`
		} `json:"relationships"`
	} `json:"data"`
}

// UpdatePlaybookRequest represents the request to update a playbook
type UpdatePlaybookRequest struct {
	Data struct {
		Type       string `json:"type"`
		ID         string `json:"id"`
		Attributes struct {
			Name          *string `json:"name,omitempty"`
			Description   *string `json:"description,omitempty"`
			VCSRepository *string `json:"vcs-repository,omitempty"`
			VCSBranch     *string `json:"vcs-branch,omitempty"`
			PlaybookPath  *string `json:"playbook-path,omitempty"`
		} `json:"attributes"`
		Relationships struct {
			VCSConnection struct {
				Data *struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"vcs-connection,omitempty"`
		} `json:"relationships,omitempty"`
	} `json:"data"`
}

// CreateJobTemplateRequest represents the request to create a job template
type CreateJobTemplateRequest struct {
	Data struct {
		Type       string `json:"type"`
		Attributes struct {
			Name            string                    `json:"name"`
			Description     string                    `json:"description"`
			ExtraVars       models.InventoryVariables `json:"extra-vars"`
			Limit           string                    `json:"limit"`
			Tags            string                    `json:"tags"`
			SkipTags        string                    `json:"skip-tags"`
			Verbosity       int                       `json:"verbosity"`
			Forks           int                       `json:"forks"`
			BecomeEnabled   bool                      `json:"become-enabled"`
			DiffMode        bool                      `json:"diff-mode"`
			ScheduleEnabled bool                      `json:"schedule-enabled"`
			ScheduleCron    string                    `json:"schedule-cron"`
		} `json:"attributes"`
		Relationships struct {
			Project struct {
				Data *struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"project,omitempty"`
			Playbook struct {
				Data struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"playbook"`
			Inventory struct {
				Data struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"inventory"`
			Credential struct {
				Data *struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"credential,omitempty"`
			AgentPool struct {
				Data *struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"agent-pool,omitempty"`
		} `json:"relationships"`
	} `json:"data"`
}

// UpdateJobTemplateRequest represents the request to update a job template
type UpdateJobTemplateRequest struct {
	Data struct {
		Type       string `json:"type"`
		ID         string `json:"id"`
		Attributes struct {
			Name            *string                    `json:"name,omitempty"`
			Description     *string                    `json:"description,omitempty"`
			ExtraVars       *models.InventoryVariables `json:"extra-vars,omitempty"`
			Limit           *string                    `json:"limit,omitempty"`
			Tags            *string                    `json:"tags,omitempty"`
			SkipTags        *string                    `json:"skip-tags,omitempty"`
			Verbosity       *int                       `json:"verbosity,omitempty"`
			Forks           *int                       `json:"forks,omitempty"`
			BecomeEnabled   *bool                      `json:"become-enabled,omitempty"`
			DiffMode        *bool                      `json:"diff-mode,omitempty"`
			ScheduleEnabled *bool                      `json:"schedule-enabled,omitempty"`
			ScheduleCron    *string                    `json:"schedule-cron,omitempty"`
		} `json:"attributes"`
		Relationships struct {
			Inventory struct {
				Data *struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"inventory,omitempty"`
			Credential struct {
				Data *struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"credential,omitempty"`
			AgentPool struct {
				Data *struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"agent-pool,omitempty"`
		} `json:"relationships,omitempty"`
	} `json:"data"`
}

// ListPlaybooks lists all playbooks for a project
// GET /api/v2/projects/:id/ansible/playbooks
func (h *PlaybookHandler) ListPlaybooks(c *gin.Context) {
	projectIDStr := c.Param("id")
	projectID, err := uuid.Parse(projectIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid project ID"},
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

	playbooks, total, err := h.playbookRepo.ListByProject(projectID, perPage, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to list playbooks"},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatPlaybooksResponse(playbooks),
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

// ListPlaybooksByOrganization lists all playbooks for an organization
// GET /api/v2/organizations/:name/ansible/playbooks
func (h *PlaybookHandler) ListPlaybooksByOrganization(c *gin.Context) {
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

	playbooks, total, err := h.playbookRepo.ListByOrganization(org.ID, perPage, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to list playbooks"},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatPlaybooksResponse(playbooks),
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

// GetPlaybook retrieves a playbook by ID
// GET /api/v2/ansible/playbooks/:id
func (h *PlaybookHandler) GetPlaybook(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid playbook ID"},
			},
		})
		return
	}

	playbook, err := h.playbookRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "Playbook not found"},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatPlaybookResponse(playbook),
	})
}

// CreatePlaybook creates a new playbook
// POST /api/v2/projects/:id/ansible/playbooks
func (h *PlaybookHandler) CreatePlaybook(c *gin.Context) {
	projectIDStr := c.Param("id")
	projectID, err := uuid.Parse(projectIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid project ID"},
			},
		})
		return
	}

	var req CreatePlaybookRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": err.Error()},
			},
		})
		return
	}

	playbook := &models.AnsiblePlaybook{
		ProjectID:     projectID,
		Name:          req.Data.Attributes.Name,
		Description:   req.Data.Attributes.Description,
		VCSRepository: req.Data.Attributes.VCSRepository,
		VCSBranch:     req.Data.Attributes.VCSBranch,
		PlaybookPath:  req.Data.Attributes.PlaybookPath,
	}

	if playbook.PlaybookPath == "" {
		playbook.PlaybookPath = "site.yml"
	}
	if playbook.VCSBranch == "" {
		playbook.VCSBranch = "main"
	}

	// Parse VCS connection ID (required for playbook creation)
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
		playbook.VCSConnectionID = &vid
	}

	if err := h.playbookRepo.Create(playbook); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to create playbook"},
			},
		})
		return
	}

	// Register ADO webhooks if this playbook is linked to an Azure DevOps repository
	h.maybeRegisterADOWebhook(playbook.VCSConnectionID, playbook.VCSRepository)

	c.JSON(http.StatusCreated, gin.H{
		"data": formatPlaybookResponse(playbook),
	})
}

// CreatePlaybookByOrganization creates a new playbook (org-scoped, TFE-compatible pattern)
// POST /api/v2/organizations/:name/ansible/playbooks
func (h *PlaybookHandler) CreatePlaybookByOrganization(c *gin.Context) {
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

	var req CreatePlaybookRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": err.Error()},
			},
		})
		return
	}

	// Determine project ID - from request body or default to first project in org
	var projectID uuid.UUID
	if req.Data.Relationships.Project.Data != nil && req.Data.Relationships.Project.Data.ID != "" {
		pid, err := uuid.Parse(req.Data.Relationships.Project.Data.ID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{"status": "400", "title": "Bad Request", "detail": "Invalid project ID"},
				},
			})
			return
		}
		// Validate project belongs to organization
		project, err := h.projectRepo.GetByID(pid)
		if err != nil || project.OrganizationID != org.ID {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{"status": "400", "title": "Bad Request", "detail": "Project not found or does not belong to this organization"},
				},
			})
			return
		}
		projectID = pid
	} else {
		// Use first project in organization (TFE-compatible behavior)
		projects, _, err := h.projectRepo.ListByOrganization(org.ID, 1, 0)
		if err != nil || len(projects) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{"status": "400", "title": "Bad Request", "detail": "Organization must have at least one project to create playbooks"},
				},
			})
			return
		}
		projectID = projects[0].ID
	}

	playbook := &models.AnsiblePlaybook{
		ProjectID:     projectID,
		Name:          req.Data.Attributes.Name,
		Description:   req.Data.Attributes.Description,
		VCSRepository: req.Data.Attributes.VCSRepository,
		VCSBranch:     req.Data.Attributes.VCSBranch,
		PlaybookPath:  req.Data.Attributes.PlaybookPath,
	}

	if playbook.PlaybookPath == "" {
		playbook.PlaybookPath = "site.yml"
	}
	if playbook.VCSBranch == "" {
		playbook.VCSBranch = "main"
	}

	// Parse VCS connection ID (required for playbook creation)
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
		playbook.VCSConnectionID = &vid
	}

	if err := h.playbookRepo.Create(playbook); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to create playbook"},
			},
		})
		return
	}

	// Register ADO webhooks if this playbook is linked to an Azure DevOps repository
	h.maybeRegisterADOWebhook(playbook.VCSConnectionID, playbook.VCSRepository)

	// Auto-trigger sync for VCS-backed playbooks
	if playbook.VCSConnectionID != nil && playbook.VCSRepository != "" && h.queue != nil {
		playbook.LastSyncStatus = "syncing"
		if err := h.playbookRepo.Update(playbook); err != nil {
			logger.Warnf("Failed to update playbook sync status: %v", err)
		}

		syncMsg := PlaybookSyncMessage{
			PlaybookID: playbook.ID,
		}
		if err := h.queue.Enqueue(context.Background(), "ansible_sync", syncMsg); err != nil {
			// Log error but don't fail the create request
			playbook.LastSyncStatus = "pending"
			playbook.LastSyncError = "Auto-sync failed to queue: " + err.Error()
			if updateErr := h.playbookRepo.Update(playbook); updateErr != nil {
				logger.Warnf("Failed to update playbook after sync queue error: %v", updateErr)
			}
		}
	}

	c.JSON(http.StatusCreated, gin.H{
		"data": formatPlaybookResponse(playbook),
	})
}

// UpdatePlaybook updates a playbook
// PATCH /api/v2/ansible/playbooks/:id
func (h *PlaybookHandler) UpdatePlaybook(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid playbook ID"},
			},
		})
		return
	}

	playbook, err := h.playbookRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "Playbook not found"},
			},
		})
		return
	}

	var req UpdatePlaybookRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": err.Error()},
			},
		})
		return
	}

	if req.Data.Attributes.Name != nil {
		playbook.Name = *req.Data.Attributes.Name
	}
	if req.Data.Attributes.Description != nil {
		playbook.Description = *req.Data.Attributes.Description
	}
	if req.Data.Attributes.VCSRepository != nil {
		playbook.VCSRepository = *req.Data.Attributes.VCSRepository
	}
	if req.Data.Attributes.VCSBranch != nil {
		playbook.VCSBranch = *req.Data.Attributes.VCSBranch
	}
	if req.Data.Attributes.PlaybookPath != nil {
		playbook.PlaybookPath = *req.Data.Attributes.PlaybookPath
	}

	// Handle VCS connection relationship update
	if req.Data.Relationships.VCSConnection.Data != nil {
		if req.Data.Relationships.VCSConnection.Data.ID == "" {
			// Explicitly setting to null
			playbook.VCSConnectionID = nil
		} else {
			vid, err := uuid.Parse(req.Data.Relationships.VCSConnection.Data.ID)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{
					"errors": []gin.H{
						{"status": "400", "title": "Bad Request", "detail": "Invalid VCS connection ID"},
					},
				})
				return
			}
			playbook.VCSConnectionID = &vid
		}
	}

	if err := h.playbookRepo.Update(playbook); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to update playbook"},
			},
		})
		return
	}

	// Register ADO webhooks if this playbook is linked to an Azure DevOps repository
	h.maybeRegisterADOWebhook(playbook.VCSConnectionID, playbook.VCSRepository)

	c.JSON(http.StatusOK, gin.H{
		"data": formatPlaybookResponse(playbook),
	})
}

// DeletePlaybook deletes a playbook
// DELETE /api/v2/ansible/playbooks/:id
func (h *PlaybookHandler) DeletePlaybook(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid playbook ID"},
			},
		})
		return
	}

	// Check for dependencies before deleting
	templateCount, err := h.playbookRepo.CountJobTemplatesByPlaybook(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": fmt.Sprintf("Failed to check dependencies: %v", err)},
			},
		})
		return
	}

	jobCount, err := h.playbookRepo.CountJobsByPlaybook(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": fmt.Sprintf("Failed to check dependencies: %v", err)},
			},
		})
		return
	}

	if templateCount > 0 || jobCount > 0 {
		var deps []string
		if templateCount > 0 {
			deps = append(deps, fmt.Sprintf("%d job template(s)", templateCount))
		}
		if jobCount > 0 {
			deps = append(deps, fmt.Sprintf("%d job(s)", jobCount))
		}
		c.JSON(http.StatusConflict, gin.H{
			"errors": []gin.H{
				{"status": "409", "title": "Conflict", "detail": fmt.Sprintf("Cannot delete playbook: it is referenced by %s. Remove the playbook from those resources first", strings.Join(deps, ", "))},
			},
		})
		return
	}

	if err := h.playbookRepo.Delete(id); err != nil {
		// Check for foreign key constraint violation (fallback)
		errStr := err.Error()
		if strings.Contains(errStr, "violates foreign key constraint") {
			c.JSON(http.StatusConflict, gin.H{
				"errors": []gin.H{
					{"status": "409", "title": "Conflict", "detail": "Cannot delete playbook: it is referenced by one or more job templates or jobs. Remove the playbook from those resources first."},
				},
			})
			return
		}

		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to delete playbook"},
			},
		})
		return
	}

	c.Status(http.StatusNoContent)
}

// SyncPlaybook syncs a playbook from VCS
// POST /api/v2/ansible/playbooks/:id/actions/sync
func (h *PlaybookHandler) SyncPlaybook(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid playbook ID"},
			},
		})
		return
	}

	playbook, err := h.playbookRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "Playbook not found"},
			},
		})
		return
	}

	// Check if playbook has VCS configuration
	if playbook.VCSConnectionID == nil || playbook.VCSRepository == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Playbook has no VCS connection configured"},
			},
		})
		return
	}

	// Update sync status to syncing
	playbook.LastSyncStatus = "syncing"
	playbook.LastSyncError = ""
	if err := h.playbookRepo.Update(playbook); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to update sync status"},
			},
		})
		return
	}

	// Queue sync job
	if h.queue != nil {
		syncMsg := PlaybookSyncMessage{
			PlaybookID: playbook.ID,
		}
		if err := h.queue.Enqueue(context.Background(), "ansible_sync", syncMsg); err != nil {
			// Revert status on queue failure
			playbook.LastSyncStatus = "failed"
			playbook.LastSyncError = "Failed to queue sync job: " + err.Error()
			if updateErr := h.playbookRepo.Update(playbook); updateErr != nil {
				logger.Warnf("Failed to update playbook after sync queue error: %v", updateErr)
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
		"data": formatPlaybookResponse(playbook),
	})
}

// ListTemplates lists all job templates for a project
// GET /api/v2/projects/:id/ansible/job-templates
func (h *PlaybookHandler) ListTemplates(c *gin.Context) {
	projectIDStr := c.Param("id")
	projectID, err := uuid.Parse(projectIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid project ID"},
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

	templates, total, err := h.templateRepo.ListByProject(projectID, perPage, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to list job templates"},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatJobTemplatesResponse(templates),
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

// ListTemplatesByOrganization lists all job templates for an organization
// GET /api/v2/organizations/:name/ansible/job-templates
func (h *PlaybookHandler) ListTemplatesByOrganization(c *gin.Context) {
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

	templates, total, err := h.templateRepo.ListByOrganization(org.ID, perPage, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to list job templates"},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatJobTemplatesResponse(templates),
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

// GetTemplate retrieves a job template by ID
// GET /api/v2/ansible/job-templates/:id
func (h *PlaybookHandler) GetTemplate(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid job template ID"},
			},
		})
		return
	}

	template, err := h.templateRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "Job template not found"},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatJobTemplateResponse(template),
	})
}

// CreateTemplate creates a new job template
// POST /api/v2/projects/:id/ansible/job-templates
func (h *PlaybookHandler) CreateTemplate(c *gin.Context) {
	projectIDStr := c.Param("id")
	projectID, err := uuid.Parse(projectIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid project ID"},
			},
		})
		return
	}

	var req CreateJobTemplateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": err.Error()},
			},
		})
		return
	}

	// Parse playbook ID
	playbookID, err := uuid.Parse(req.Data.Relationships.Playbook.Data.ID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid playbook ID"},
			},
		})
		return
	}

	// Parse inventory ID
	inventoryID, err := uuid.Parse(req.Data.Relationships.Inventory.Data.ID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid inventory ID"},
			},
		})
		return
	}

	// Parse credential ID (optional)
	var credentialID *uuid.UUID
	if req.Data.Relationships.Credential.Data != nil {
		cid, err := uuid.Parse(req.Data.Relationships.Credential.Data.ID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{"status": "400", "title": "Bad Request", "detail": "Invalid credential ID"},
				},
			})
			return
		}
		credentialID = &cid
	}

	// Parse agent pool ID (optional)
	var agentPoolID *uuid.UUID
	if req.Data.Relationships.AgentPool.Data != nil {
		apid, err := uuid.Parse(req.Data.Relationships.AgentPool.Data.ID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{"status": "400", "title": "Bad Request", "detail": "Invalid agent pool ID"},
				},
			})
			return
		}
		agentPoolID = &apid
	}

	template := &models.AnsibleJobTemplate{
		ProjectID:       projectID,
		PlaybookID:      playbookID,
		InventoryID:     inventoryID,
		CredentialID:    credentialID,
		AgentPoolID:     agentPoolID,
		Name:            req.Data.Attributes.Name,
		Description:     req.Data.Attributes.Description,
		ExtraVars:       req.Data.Attributes.ExtraVars,
		Limit:           req.Data.Attributes.Limit,
		Tags:            req.Data.Attributes.Tags,
		SkipTags:        req.Data.Attributes.SkipTags,
		Verbosity:       req.Data.Attributes.Verbosity,
		Forks:           req.Data.Attributes.Forks,
		BecomeEnabled:   req.Data.Attributes.BecomeEnabled,
		DiffMode:        req.Data.Attributes.DiffMode,
		ScheduleEnabled: req.Data.Attributes.ScheduleEnabled,
		ScheduleCron:    req.Data.Attributes.ScheduleCron,
	}

	if template.Forks == 0 {
		template.Forks = 5
	}

	if err := h.templateRepo.Create(template); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to create job template"},
			},
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"data": formatJobTemplateResponse(template),
	})
}

// CreateTemplateByOrganization creates a new job template (org-scoped, TFE-compatible pattern)
// POST /api/v2/organizations/:name/ansible/job-templates
func (h *PlaybookHandler) CreateTemplateByOrganization(c *gin.Context) {
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

	var req CreateJobTemplateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": err.Error()},
			},
		})
		return
	}

	// Determine project ID - from request body or default to first project in org
	var projectID uuid.UUID
	if req.Data.Relationships.Project.Data != nil && req.Data.Relationships.Project.Data.ID != "" {
		pid, err := uuid.Parse(req.Data.Relationships.Project.Data.ID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{"status": "400", "title": "Bad Request", "detail": "Invalid project ID"},
				},
			})
			return
		}
		// Validate project belongs to organization
		project, err := h.projectRepo.GetByID(pid)
		if err != nil || project.OrganizationID != org.ID {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{"status": "400", "title": "Bad Request", "detail": "Project not found or does not belong to this organization"},
				},
			})
			return
		}
		projectID = pid
	} else {
		// Use first project in organization (TFE-compatible behavior)
		projects, _, err := h.projectRepo.ListByOrganization(org.ID, 1, 0)
		if err != nil || len(projects) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{"status": "400", "title": "Bad Request", "detail": "Organization must have at least one project to create job templates"},
				},
			})
			return
		}
		projectID = projects[0].ID
	}

	// Parse playbook ID
	playbookID, err := uuid.Parse(req.Data.Relationships.Playbook.Data.ID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid playbook ID"},
			},
		})
		return
	}

	// Parse inventory ID
	inventoryID, err := uuid.Parse(req.Data.Relationships.Inventory.Data.ID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid inventory ID"},
			},
		})
		return
	}

	// Parse credential ID (optional)
	var credentialID *uuid.UUID
	if req.Data.Relationships.Credential.Data != nil {
		cid, err := uuid.Parse(req.Data.Relationships.Credential.Data.ID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{"status": "400", "title": "Bad Request", "detail": "Invalid credential ID"},
				},
			})
			return
		}
		credentialID = &cid
	}

	// Parse agent pool ID (optional)
	var agentPoolID *uuid.UUID
	if req.Data.Relationships.AgentPool.Data != nil {
		apid, err := uuid.Parse(req.Data.Relationships.AgentPool.Data.ID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{"status": "400", "title": "Bad Request", "detail": "Invalid agent pool ID"},
				},
			})
			return
		}
		agentPoolID = &apid
	}

	template := &models.AnsibleJobTemplate{
		ProjectID:       projectID,
		PlaybookID:      playbookID,
		InventoryID:     inventoryID,
		CredentialID:    credentialID,
		AgentPoolID:     agentPoolID,
		Name:            req.Data.Attributes.Name,
		Description:     req.Data.Attributes.Description,
		ExtraVars:       req.Data.Attributes.ExtraVars,
		Limit:           req.Data.Attributes.Limit,
		Tags:            req.Data.Attributes.Tags,
		SkipTags:        req.Data.Attributes.SkipTags,
		Verbosity:       req.Data.Attributes.Verbosity,
		Forks:           req.Data.Attributes.Forks,
		BecomeEnabled:   req.Data.Attributes.BecomeEnabled,
		DiffMode:        req.Data.Attributes.DiffMode,
		ScheduleEnabled: req.Data.Attributes.ScheduleEnabled,
		ScheduleCron:    req.Data.Attributes.ScheduleCron,
	}

	if template.Forks == 0 {
		template.Forks = 5
	}

	if err := h.templateRepo.Create(template); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to create job template"},
			},
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"data": formatJobTemplateResponse(template),
	})
}

// UpdateTemplate updates a job template
// PATCH /api/v2/ansible/job-templates/:id
func (h *PlaybookHandler) UpdateTemplate(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid job template ID"},
			},
		})
		return
	}

	template, err := h.templateRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "Job template not found"},
			},
		})
		return
	}

	var req UpdateJobTemplateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": err.Error()},
			},
		})
		return
	}

	// Debug logging: Check what relationships were received
	logger.Debugf("UpdateTemplate: Received request - Inventory.Data: %v, Credential.Data: %v",
		req.Data.Relationships.Inventory.Data != nil, req.Data.Relationships.Credential.Data != nil)
	if req.Data.Relationships.Inventory.Data != nil {
		logger.Debugf("UpdateTemplate: Inventory ID in request: %s", req.Data.Relationships.Inventory.Data.ID)
	}
	if req.Data.Relationships.Credential.Data != nil {
		logger.Debugf("UpdateTemplate: Credential ID in request: %s", req.Data.Relationships.Credential.Data.ID)
	}

	if req.Data.Attributes.Name != nil {
		template.Name = *req.Data.Attributes.Name
	}
	if req.Data.Attributes.Description != nil {
		template.Description = *req.Data.Attributes.Description
	}
	if req.Data.Attributes.ExtraVars != nil {
		template.ExtraVars = *req.Data.Attributes.ExtraVars
	}
	if req.Data.Attributes.Limit != nil {
		template.Limit = *req.Data.Attributes.Limit
	}
	if req.Data.Attributes.Tags != nil {
		template.Tags = *req.Data.Attributes.Tags
	}
	if req.Data.Attributes.SkipTags != nil {
		template.SkipTags = *req.Data.Attributes.SkipTags
	}
	if req.Data.Attributes.Verbosity != nil {
		template.Verbosity = *req.Data.Attributes.Verbosity
	}
	if req.Data.Attributes.Forks != nil {
		template.Forks = *req.Data.Attributes.Forks
	}
	if req.Data.Attributes.BecomeEnabled != nil {
		template.BecomeEnabled = *req.Data.Attributes.BecomeEnabled
	}
	if req.Data.Attributes.DiffMode != nil {
		template.DiffMode = *req.Data.Attributes.DiffMode
	}
	if req.Data.Attributes.ScheduleEnabled != nil {
		template.ScheduleEnabled = *req.Data.Attributes.ScheduleEnabled
	}
	if req.Data.Attributes.ScheduleCron != nil {
		template.ScheduleCron = *req.Data.Attributes.ScheduleCron
	}

	// Handle inventory relationship update
	if req.Data.Relationships.Inventory.Data != nil {
		iid, err := uuid.Parse(req.Data.Relationships.Inventory.Data.ID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{"status": "400", "title": "Bad Request", "detail": "Invalid inventory ID"},
				},
			})
			return
		}
		logger.Debugf("UpdateTemplate: Setting InventoryID from %s to %s", template.InventoryID.String(), iid.String())
		template.InventoryID = iid
	} else {
		logger.Debugf("UpdateTemplate: Inventory relationship not provided in request (Data is nil)")
	}

	// Handle agent pool relationship update
	if req.Data.Relationships.AgentPool.Data != nil {
		if req.Data.Relationships.AgentPool.Data.ID == "" {
			// Explicitly setting to null (remove agent pool assignment)
			template.AgentPoolID = nil
		} else {
			apid, err := uuid.Parse(req.Data.Relationships.AgentPool.Data.ID)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{
					"errors": []gin.H{
						{"status": "400", "title": "Bad Request", "detail": "Invalid agent pool ID"},
					},
				})
				return
			}
			template.AgentPoolID = &apid
		}
	}

	// Handle credential relationship update
	if req.Data.Relationships.Credential.Data != nil {
		if req.Data.Relationships.Credential.Data.ID == "" {
			// Explicitly setting to null (remove credential assignment)
			logger.Debugf("UpdateTemplate: Clearing CredentialID (was %v)", template.CredentialID)
			template.CredentialID = nil
		} else {
			cid, err := uuid.Parse(req.Data.Relationships.Credential.Data.ID)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{
					"errors": []gin.H{
						{"status": "400", "title": "Bad Request", "detail": "Invalid credential ID"},
					},
				})
				return
			}
			oldCredID := "nil"
			if template.CredentialID != nil {
				oldCredID = template.CredentialID.String()
			}
			logger.Debugf("UpdateTemplate: Setting CredentialID from %s to %s", oldCredID, cid.String())
			template.CredentialID = &cid
		}
	}

	logger.Debugf("UpdateTemplate: Before Save - InventoryID: %s, CredentialID: %v", template.InventoryID.String(), template.CredentialID)
	if err := h.templateRepo.Update(template); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to update job template"},
			},
		})
		return
	}

	// Reload the template from database to get updated relationships
	updatedTemplate, err := h.templateRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to reload updated job template"},
			},
		})
		return
	}

	logger.Debugf("UpdateTemplate: After reload - InventoryID: %s, CredentialID: %v",
		updatedTemplate.InventoryID.String(), updatedTemplate.CredentialID)

	c.JSON(http.StatusOK, gin.H{
		"data": formatJobTemplateResponse(updatedTemplate),
	})
}

// DeleteTemplate deletes a job template
// DELETE /api/v2/ansible/job-templates/:id
func (h *PlaybookHandler) DeleteTemplate(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid job template ID"},
			},
		})
		return
	}

	// Cascade delete: first delete all schedules that reference this template
	if err := h.scheduleRepo.DeleteByJobTemplate(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to delete associated schedules"},
			},
		})
		return
	}

	// Then delete all jobs that were created from this template
	if err := h.jobRepo.DeleteByTemplateID(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to delete associated jobs"},
			},
		})
		return
	}

	// Finally delete the template itself
	if err := h.templateRepo.Delete(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to delete job template"},
			},
		})
		return
	}

	c.Status(http.StatusNoContent)
}

// formatPlaybookResponse formats a playbook for JSON:API response
func formatPlaybookResponse(playbook *models.AnsiblePlaybook) gin.H {
	// Derive VCS provider and account name from the preloaded VCSConnection
	vcsProvider := ""
	vcsAccountName := ""
	if playbook.VCSConnection != nil {
		vcsProvider = string(playbook.VCSConnection.Provider)
		vcsAccountName = playbook.VCSConnection.AccountName
	}

	attributes := gin.H{
		"name":             playbook.Name,
		"description":      playbook.Description,
		"vcs-repository":   playbook.VCSRepository,
		"vcs-branch":       playbook.VCSBranch,
		"vcs-provider":     vcsProvider,
		"vcs-account-name": vcsAccountName,
		"playbook-path":    playbook.PlaybookPath,
		"last-sync-at":     nil,
		"last-sync-status": playbook.LastSyncStatus,
		"last-sync-commit": playbook.LastSyncCommit,
		"last-sync-error":  playbook.LastSyncError,
		"created-at":       playbook.CreatedAt.Format("2006-01-02T15:04:05Z"),
		"updated-at":       playbook.UpdatedAt.Format("2006-01-02T15:04:05Z"),
	}

	if playbook.LastSyncAt != nil {
		attributes["last-sync-at"] = playbook.LastSyncAt.Format("2006-01-02T15:04:05Z")
	}

	relationships := gin.H{
		"project": gin.H{
			"data": gin.H{
				"id":   playbook.ProjectID.String(),
				"type": "projects",
			},
		},
	}

	if playbook.VCSConnectionID != nil {
		relationships["vcs-connection"] = gin.H{
			"data": gin.H{
				"id":   playbook.VCSConnectionID.String(),
				"type": "vcs-connections",
			},
		}
	}

	return gin.H{
		"id":            playbook.ID.String(),
		"type":          "ansible-playbooks",
		"attributes":    attributes,
		"relationships": relationships,
	}
}

// formatPlaybooksResponse formats multiple playbooks for JSON:API response
func formatPlaybooksResponse(playbooks []models.AnsiblePlaybook) []gin.H {
	result := make([]gin.H, len(playbooks))
	for i, playbook := range playbooks {
		result[i] = formatPlaybookResponse(&playbook)
	}
	return result
}

// formatJobTemplateResponse formats a job template for JSON:API response
func formatJobTemplateResponse(template *models.AnsibleJobTemplate) gin.H {
	relationships := gin.H{
		"project": gin.H{
			"data": gin.H{
				"id":   template.ProjectID.String(),
				"type": "projects",
			},
		},
		"playbook": gin.H{
			"data": gin.H{
				"id":   template.PlaybookID.String(),
				"type": "ansible-playbooks",
			},
		},
		"inventory": gin.H{
			"data": gin.H{
				"id":   template.InventoryID.String(),
				"type": "ansible-inventories",
			},
		},
	}

	if template.CredentialID != nil {
		relationships["credential"] = gin.H{
			"data": gin.H{
				"id":   template.CredentialID.String(),
				"type": "ansible-credentials",
			},
		}
	}

	if template.AgentPoolID != nil {
		relationships["agent-pool"] = gin.H{
			"data": gin.H{
				"id":   template.AgentPoolID.String(),
				"type": "agent-pools",
			},
		}
	}

	return gin.H{
		"id":   template.ID.String(),
		"type": "ansible-job-templates",
		"attributes": gin.H{
			"name":             template.Name,
			"description":      template.Description,
			"extra-vars":       template.ExtraVars,
			"limit":            template.Limit,
			"tags":             template.Tags,
			"skip-tags":        template.SkipTags,
			"verbosity":        template.Verbosity,
			"forks":            template.Forks,
			"become-enabled":   template.BecomeEnabled,
			"diff-mode":        template.DiffMode,
			"schedule-enabled": template.ScheduleEnabled,
			"schedule-cron":    template.ScheduleCron,
			"created-at":       template.CreatedAt.Format("2006-01-02T15:04:05Z"),
			"updated-at":       template.UpdatedAt.Format("2006-01-02T15:04:05Z"),
		},
		"relationships": relationships,
	}
}

// formatJobTemplatesResponse formats multiple job templates for JSON:API response
func formatJobTemplatesResponse(templates []models.AnsibleJobTemplate) []gin.H {
	result := make([]gin.H, len(templates))
	for i, template := range templates {
		result[i] = formatJobTemplateResponse(&template)
	}
	return result
}
