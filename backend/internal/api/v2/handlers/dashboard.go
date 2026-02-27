// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/auth"
)

type DashboardHandler struct {
	orgRepo             *repository.OrganizationRepository
	projectRepo         *repository.ProjectRepository
	workspaceRepo       *repository.WorkspaceRepository
	runRepo             *repository.RunRepository
	ansibleJobRepo      *repository.AnsibleJobRepository
	ansiblePlaybookRepo *repository.AnsiblePlaybookRepository
	authService         *auth.Service
}

func NewDashboardHandler(
	orgRepo *repository.OrganizationRepository,
	projectRepo *repository.ProjectRepository,
	workspaceRepo *repository.WorkspaceRepository,
	runRepo *repository.RunRepository,
	ansibleJobRepo *repository.AnsibleJobRepository,
	ansiblePlaybookRepo *repository.AnsiblePlaybookRepository,
	authService *auth.Service,
) *DashboardHandler {
	return &DashboardHandler{
		orgRepo:             orgRepo,
		projectRepo:         projectRepo,
		workspaceRepo:       workspaceRepo,
		runRepo:             runRepo,
		ansibleJobRepo:      ansibleJobRepo,
		ansiblePlaybookRepo: ansiblePlaybookRepo,
		authService:         authService,
	}
}

// GetStats returns aggregated dashboard statistics for the authenticated user
// GET /api/v2/dashboard/stats
func (h *DashboardHandler) GetStats(c *gin.Context) {
	// Get authenticated user from context
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{
					"status": "401",
					"title":  "Unauthorized",
					"detail": "User not authenticated",
				},
			},
		})
		return
	}

	// Get all organizations the user belongs to
	orgs, err := h.orgRepo.ListByUser(user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to load organizations",
				},
			},
		})
		return
	}

	// Initialize aggregate stats
	var totalProjects int64
	var totalWorkspaces int64
	var totalAnsiblePlaybooks int64
	var activeTerraformRuns int64
	var activeAnsibleJobs int64
	var completedTerraformRunsThisMonth int64
	var completedAnsibleJobsThisMonth int64

	// Calculate first day of current month for "this month" filtering
	now := time.Now()
	firstDayOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	// Organization-level stats
	orgStats := make([]gin.H, 0, len(orgs))

	for _, org := range orgs {
		// Count projects
		projects, _, err := h.projectRepo.ListByOrganization(org.ID, 1000, 0)
		if err != nil {
			// Log error but continue with other orgs
			continue
		}
		projectCount := int64(len(projects))
		totalProjects += projectCount

		// Count workspaces
		var workspaceCount int64
		for _, project := range projects {
			workspaces, _, err := h.workspaceRepo.ListByProject(project.ID, 1000, 0)
			if err != nil {
				continue
			}
			workspaceCount += int64(len(workspaces))
		}
		totalWorkspaces += workspaceCount

		// Count Ansible playbooks
		playbooks, _, err := h.ansiblePlaybookRepo.ListByOrganization(org.ID, 1000, 0)
		if err != nil {
			continue
		}
		playbookCount := int64(len(playbooks))
		totalAnsiblePlaybooks += playbookCount

		// Get user's runs for this organization
		orgRuns, _, err := h.runRepo.ListByOrganizationAndUser(org.ID, user.ID, 10000, 0)
		if err != nil {
			continue
		}

		// Count active Terraform runs (running, planning, applying)
		var orgActiveRuns int64
		var orgCompletedRunsThisMonth int64
		for _, run := range orgRuns {
			status := run.Status
			if status == models.RunStatusRunning || status == models.RunStatusPlanning || status == models.RunStatusApplying {
				orgActiveRuns++
			}
			// Count completed runs this month
			if (status == models.RunStatusApplied || status == models.RunStatusCompleted) && run.CompletedAt != nil {
				if run.CompletedAt.After(firstDayOfMonth) || run.CompletedAt.Equal(firstDayOfMonth) {
					orgCompletedRunsThisMonth++
				}
			}
		}
		activeTerraformRuns += orgActiveRuns
		completedTerraformRunsThisMonth += orgCompletedRunsThisMonth

		// Get user's Ansible jobs for this organization
		orgJobs, _, err := h.ansibleJobRepo.ListByOrganizationAndUser(org.ID, user.ID, 10000, 0)
		if err != nil {
			continue
		}

		// Count active Ansible jobs (running, pending)
		var orgActiveJobs int64
		var orgCompletedJobsThisMonth int64
		for _, job := range orgJobs {
			status := job.Status
			if status == models.AnsibleJobStatusRunning || status == models.AnsibleJobStatusPending {
				orgActiveJobs++
			}
			// Count successful jobs this month
			if status == models.AnsibleJobStatusSuccessful && job.FinishedAt != nil {
				if job.FinishedAt.After(firstDayOfMonth) || job.FinishedAt.Equal(firstDayOfMonth) {
					orgCompletedJobsThisMonth++
				}
			}
		}
		activeAnsibleJobs += orgActiveJobs
		completedAnsibleJobsThisMonth += orgCompletedJobsThisMonth

		// Add organization stats
		orgStats = append(orgStats, gin.H{
			"id":                                  org.ID.String(),
			"name":                                org.Name,
			"description":                         org.Description,
			"projects":                            projectCount,
			"terraform_workspaces":                workspaceCount,
			"ansible_playbooks":                   playbookCount,
			"active_terraform_runs":               orgActiveRuns,
			"active_ansible_jobs":                 orgActiveJobs,
			"completed_terraform_runs_this_month": orgCompletedRunsThisMonth,
			"completed_ansible_jobs_this_month":   orgCompletedJobsThisMonth,
		})
	}

	// Return JSON:API compatible response
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"type": "dashboard-stats",
			"attributes": gin.H{
				"projects":                            totalProjects,
				"terraform_workspaces":                totalWorkspaces,
				"ansible_playbooks":                   totalAnsiblePlaybooks,
				"active_terraform_runs":               activeTerraformRuns,
				"active_ansible_jobs":                 activeAnsibleJobs,
				"completed_terraform_runs_this_month": completedTerraformRunsThisMonth,
				"completed_ansible_jobs_this_month":   completedAnsibleJobsThisMonth,
				"organizations":                       orgStats,
			},
		},
	})
}
