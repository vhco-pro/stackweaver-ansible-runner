// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansible

import (
	"context"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/ansible"
	"github.com/iac-platform/backend/internal/services/auth"
)

// JobHandler handles Ansible job API endpoints
type JobHandler struct {
	jobService  *ansible.JobService
	projectRepo *repository.ProjectRepository
	orgRepo     *repository.OrganizationRepository
	authService *auth.Service
}

// NewJobHandler creates a new job handler
func NewJobHandler(
	jobService *ansible.JobService,
	projectRepo *repository.ProjectRepository,
	orgRepo *repository.OrganizationRepository,
	authService *auth.Service,
) *JobHandler {
	return &JobHandler{
		jobService:  jobService,
		projectRepo: projectRepo,
		orgRepo:     orgRepo,
		authService: authService,
	}
}

// LaunchJobRequest represents the request to launch a job
type LaunchJobRequest struct {
	Data struct {
		Type       string `json:"type"`
		Attributes struct {
			Name           string              `json:"name"`
			JobType        string              `json:"job-type"`
			ExtraVars      models.JobExtraVars `json:"extra-vars"`
			Limit          string              `json:"limit"`
			Tags           string              `json:"tags"`
			SkipTags       string              `json:"skip-tags"`
			Verbosity      int                 `json:"verbosity"`
			Forks          int                 `json:"forks"`
			BecomeEnabled  bool                `json:"become-enabled"`
			DiffMode       bool                `json:"diff-mode"`
			AnsibleVersion string              `json:"ansible-version"`
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
		} `json:"relationships"`
	} `json:"data"`
}

// LaunchFromTemplateRequest represents the request to launch a job from a template
type LaunchFromTemplateRequest struct {
	Data struct {
		Type       string `json:"type"`
		Attributes struct {
			ExtraVars models.JobExtraVars `json:"extra-vars"`
		} `json:"attributes"`
	} `json:"data"`
}

// ListByProject lists all jobs for a project
// GET /api/v2/projects/:id/ansible/jobs
func (h *JobHandler) ListByProject(c *gin.Context) {
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

	jobs, total, err := h.jobService.ListJobsByProject(projectID, perPage, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to list jobs"},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatJobsResponse(jobs),
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

// ListByOrganization lists all jobs for an organization
// GET /api/v2/organizations/:name/ansible/jobs
func (h *JobHandler) ListByOrganization(c *gin.Context) {
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

	jobs, total, err := h.jobService.ListJobsByOrganization(org.ID, perPage, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to list jobs"},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatJobsResponse(jobs),
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

// GetQueue gets the job queue for an organization
// GET /api/v2/organizations/:name/ansible/jobs/queue
func (h *JobHandler) GetQueue(c *gin.Context) {
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

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if limit > 100 {
		limit = 100
	}

	jobs, err := h.jobService.GetJobQueue(org.ID, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to get job queue"},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatJobsResponse(jobs),
	})
}

// Launch launches a new job
// POST /api/v2/projects/:id/ansible/jobs
func (h *JobHandler) Launch(c *gin.Context) {
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

	var req LaunchJobRequest
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

	// Parse job type
	jobType := models.AnsibleJobTypeRun
	switch req.Data.Attributes.JobType {
	case "run":
		jobType = models.AnsibleJobTypeRun
	case "check":
		jobType = models.AnsibleJobTypeCheck
	case "syntax":
		jobType = models.AnsibleJobTypeSyntax
	}

	// Get user ID
	var createdBy *uuid.UUID
	if user, err := h.authService.GetUserFromContext(c); err == nil {
		createdBy = &user.ID
	}

	input := ansible.LaunchJobInput{
		ProjectID:      projectID,
		PlaybookID:     playbookID,
		InventoryID:    inventoryID,
		Name:           req.Data.Attributes.Name,
		JobType:        jobType,
		ExtraVars:      req.Data.Attributes.ExtraVars,
		Limit:          req.Data.Attributes.Limit,
		Tags:           req.Data.Attributes.Tags,
		SkipTags:       req.Data.Attributes.SkipTags,
		Verbosity:      req.Data.Attributes.Verbosity,
		Forks:          req.Data.Attributes.Forks,
		CredentialID:   credentialID,
		BecomeEnabled:  req.Data.Attributes.BecomeEnabled,
		DiffMode:       req.Data.Attributes.DiffMode,
		AnsibleVersion: req.Data.Attributes.AnsibleVersion,
		CreatedBy:      createdBy,
	}

	job, err := h.jobService.LaunchJob(context.Background(), input)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": err.Error()},
			},
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"data": formatJobResponse(job),
	})
}

// LaunchByOrganization launches a new job (org-scoped, TFE-compatible pattern)
// POST /api/v2/organizations/:name/ansible/jobs
func (h *JobHandler) LaunchByOrganization(c *gin.Context) {
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

	var req LaunchJobRequest
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
					{"status": "400", "title": "Bad Request", "detail": "Organization must have at least one project to launch jobs"},
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

	// Parse job type
	jobType := models.AnsibleJobTypeRun
	switch req.Data.Attributes.JobType {
	case "run":
		jobType = models.AnsibleJobTypeRun
	case "check":
		jobType = models.AnsibleJobTypeCheck
	case "syntax":
		jobType = models.AnsibleJobTypeSyntax
	}

	// Get user ID
	var createdBy *uuid.UUID
	if user, err := h.authService.GetUserFromContext(c); err == nil {
		createdBy = &user.ID
	}

	input := ansible.LaunchJobInput{
		ProjectID:      projectID,
		PlaybookID:     playbookID,
		InventoryID:    inventoryID,
		Name:           req.Data.Attributes.Name,
		JobType:        jobType,
		ExtraVars:      req.Data.Attributes.ExtraVars,
		Limit:          req.Data.Attributes.Limit,
		Tags:           req.Data.Attributes.Tags,
		SkipTags:       req.Data.Attributes.SkipTags,
		Verbosity:      req.Data.Attributes.Verbosity,
		Forks:          req.Data.Attributes.Forks,
		CredentialID:   credentialID,
		BecomeEnabled:  req.Data.Attributes.BecomeEnabled,
		DiffMode:       req.Data.Attributes.DiffMode,
		AnsibleVersion: req.Data.Attributes.AnsibleVersion,
		CreatedBy:      createdBy,
	}

	job, err := h.jobService.LaunchJob(context.Background(), input)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": err.Error()},
			},
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"data": formatJobResponse(job),
	})
}

// Get retrieves a job by ID
// GET /api/v2/ansible/jobs/:id
func (h *JobHandler) Get(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid job ID"},
			},
		})
		return
	}

	job, err := h.jobService.GetJob(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "Job not found"},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatJobResponse(job),
	})
}

// Cancel cancels a job
// POST /api/v2/ansible/jobs/:id/actions/cancel
func (h *JobHandler) Cancel(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid job ID"},
			},
		})
		return
	}

	job, err := h.jobService.CancelJob(id)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": err.Error()},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatJobResponse(job),
	})
}

// Relaunch relaunches a job
// POST /api/v2/ansible/jobs/:id/actions/relaunch
func (h *JobHandler) Relaunch(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid job ID"},
			},
		})
		return
	}

	// Get user ID
	var createdBy *uuid.UUID
	if user, err := h.authService.GetUserFromContext(c); err == nil {
		createdBy = &user.ID
	}

	job, err := h.jobService.RelaunchJob(context.Background(), id, createdBy)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": err.Error()},
			},
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"data": formatJobResponse(job),
	})
}

// Delete deletes a job
// DELETE /api/v2/ansible/jobs/:id
func (h *JobHandler) Delete(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid job ID"},
			},
		})
		return
	}

	err = h.jobService.DeleteJob(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": err.Error()},
			},
		})
		return
	}

	c.Status(http.StatusNoContent)
}

// GetEvents retrieves events for a job
// GET /api/v2/ansible/jobs/:id/events
func (h *JobHandler) GetEvents(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid job ID"},
			},
		})
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page[number]", "1"))
	perPage, _ := strconv.Atoi(c.DefaultQuery("page[size]", "100"))
	if perPage > 500 {
		perPage = 500
	}
	offset := (page - 1) * perPage

	events, total, err := h.jobService.GetJobEvents(id, perPage, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to get job events"},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatEventsResponse(events),
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

// GetOutput retrieves the combined output for a job
// GET /api/v2/ansible/jobs/:id/output
func (h *JobHandler) GetOutput(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid job ID"},
			},
		})
		return
	}

	output, err := h.jobService.GetJobOutput(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to get job output"},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": output,
	})
}

// LaunchFromTemplate launches a job from a template
// POST /api/v2/ansible/job-templates/:id/launch
func (h *JobHandler) LaunchFromTemplate(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid template ID"},
			},
		})
		return
	}

	var req LaunchFromTemplateRequest
	// Optional body for extra vars
	// If JSON binding fails, continue with empty request (backward compatibility)
	// The request body is optional for this endpoint
	_ = c.ShouldBindJSON(&req)

	// Get user ID
	var createdBy *uuid.UUID
	if user, err := h.authService.GetUserFromContext(c); err == nil {
		createdBy = &user.ID
	}

	job, err := h.jobService.LaunchFromTemplate(context.Background(), id, req.Data.Attributes.ExtraVars, createdBy)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": err.Error()},
			},
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"data": formatJobResponse(job),
	})
}

// formatJobResponse formats a job for JSON:API response
func formatJobResponse(job *models.AnsibleJob) gin.H {
	attributes := gin.H{
		"name":              job.Name,
		"job-type":          job.JobType,
		"status":            job.Status,
		"extra-vars":        job.ExtraVars,
		"limit":             job.Limit,
		"tags":              job.Tags,
		"skip-tags":         job.SkipTags,
		"verbosity":         job.Verbosity,
		"forks":             job.Forks,
		"become-enabled":    job.BecomeEnabled,
		"diff-mode":         job.DiffMode,
		"ansible-version":   job.AnsibleVersion,
		"exit-code":         job.ExitCode,
		"error-message":     job.ErrorMessage,
		"started-at":        nil,
		"finished-at":       nil,
		"hosts-total":       job.HostsTotal,
		"hosts-ok":          job.HostsOk,
		"hosts-changed":     job.HostsChanged,
		"hosts-failed":      job.HostsFailed,
		"hosts-unreachable": job.HostsUnreachable,
		"hosts-skipped":     job.HostsSkipped,
		"hosts-rescued":     job.HostsRescued,
		"hosts-ignored":     job.HostsIgnored,
		"has-warnings":      job.HasWarnings,
		"warnings-count":    job.WarningsCount,
		"created-at":        job.CreatedAt.Format("2006-01-02T15:04:05Z"),
		"updated-at":        job.UpdatedAt.Format("2006-01-02T15:04:05Z"),
	}

	if job.StartedAt != nil {
		attributes["started-at"] = job.StartedAt.Format("2006-01-02T15:04:05Z")
	}
	if job.FinishedAt != nil {
		attributes["finished-at"] = job.FinishedAt.Format("2006-01-02T15:04:05Z")
	}

	relationships := gin.H{
		"project": gin.H{
			"data": gin.H{
				"id":   job.ProjectID.String(),
				"type": "projects",
			},
		},
		"playbook": gin.H{
			"data": gin.H{
				"id":   job.PlaybookID.String(),
				"type": "ansible-playbooks",
			},
		},
		"inventory": gin.H{
			"data": gin.H{
				"id":   job.InventoryID.String(),
				"type": "ansible-inventories",
			},
		},
	}

	if job.TemplateID != nil {
		relationships["template"] = gin.H{
			"data": gin.H{
				"id":   job.TemplateID.String(),
				"type": "ansible-job-templates",
			},
		}
	}

	if job.CredentialID != nil {
		relationships["credential"] = gin.H{
			"data": gin.H{
				"id":   job.CredentialID.String(),
				"type": "ansible-credentials",
			},
		}
	}

	if job.CreatedBy != nil {
		relationships["created-by"] = gin.H{
			"data": gin.H{
				"id":   job.CreatedBy.String(),
				"type": "users",
			},
		}
	}

	if job.AgentPoolID != nil {
		relationships["agent-pool"] = gin.H{
			"data": gin.H{
				"id":   job.AgentPoolID.String(),
				"type": "agent-pools",
			},
		}
		if job.AgentPool != nil {
			attributes["agent-pool-name"] = job.AgentPool.Name
		}
	}

	if job.RunnerID != nil {
		relationships["runner"] = gin.H{
			"data": gin.H{
				"id":   job.RunnerID.String(),
				"type": "runners",
			},
		}
		if job.Runner != nil {
			attributes["runner-name"] = job.Runner.Name
		}
	}

	return gin.H{
		"id":            job.ID.String(),
		"type":          "ansible-jobs",
		"attributes":    attributes,
		"relationships": relationships,
	}
}

// formatJobsResponse formats multiple jobs for JSON:API response
func formatJobsResponse(jobs []models.AnsibleJob) []gin.H {
	result := make([]gin.H, len(jobs))
	for i, job := range jobs {
		result[i] = formatJobResponse(&job)
	}
	return result
}

// formatEventResponse formats an event for JSON:API response
func formatEventResponse(event *models.AnsibleJobEvent) gin.H {
	return gin.H{
		"id":   event.ID.String(),
		"type": "ansible-job-events",
		"attributes": gin.H{
			"counter":    event.Counter,
			"event-type": event.Event,
			"event-data": event.EventData,
			"host":       event.Host,
			"task":       event.Task,
			"play":       event.Play,
			"role":       event.Role,
			"stdout":     event.Stdout,
			"stderr":     event.Stderr,
			"changed":    event.Changed,
			"failed":     event.Failed,
			"skipped":    event.Skipped,
			"timestamp":  event.Timestamp.Format("2006-01-02T15:04:05Z"),
			"created-at": event.CreatedAt.Format("2006-01-02T15:04:05Z"),
		},
		"relationships": gin.H{
			"job": gin.H{
				"data": gin.H{
					"id":   event.JobID.String(),
					"type": "ansible-jobs",
				},
			},
		},
	}
}

// formatEventsResponse formats multiple events for JSON:API response
func formatEventsResponse(events []models.AnsibleJobEvent) []gin.H {
	result := make([]gin.H, len(events))
	for i, event := range events {
		result[i] = formatEventResponse(&event)
	}
	return result
}
