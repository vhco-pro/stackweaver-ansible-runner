// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package terraform

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/logbuffer"
	"github.com/iac-platform/backend/internal/services/logparser"
	"github.com/iac-platform/backend/internal/services/rbac"
	"github.com/iac-platform/backend/internal/services/vcs"
	"github.com/iac-platform/backend/internal/storage"
	"github.com/michielvha/logger"
)

type RunHandlerV2 struct {
	runRepo           *repository.RunRepository
	workspaceRepo     *repository.WorkspaceRepository
	orgRepo           *repository.OrganizationRepository
	authService       *auth.Service
	storageClient     storage.Client
	configVersionRepo *repository.ConfigurationVersionRepository
	vcsConnectionRepo *repository.VCSConnectionRepository
	vcsRegistry       *vcs.ProviderRegistry     // Multi-provider VCS support
	logBufferService  *logbuffer.RedisLogBuffer // Optional: nil if Redis not available
	phaseStateRepo    *repository.RunPhaseStateRepository
	rbacService       *rbac.Service
	stateVersionRepo  *repository.StateVersionRepository
}

func NewRunHandlerV2(
	runRepo *repository.RunRepository,
	workspaceRepo *repository.WorkspaceRepository,
	orgRepo *repository.OrganizationRepository,
	authService *auth.Service,
	storageClient storage.Client,
	configVersionRepo *repository.ConfigurationVersionRepository,
	vcsConnectionRepo *repository.VCSConnectionRepository,
	vcsRegistry *vcs.ProviderRegistry,
	logBufferService *logbuffer.RedisLogBuffer,
	phaseStateRepo *repository.RunPhaseStateRepository,
	rbacService *rbac.Service,
	stateVersionRepo *repository.StateVersionRepository,
) *RunHandlerV2 {
	return &RunHandlerV2{
		runRepo:           runRepo,
		workspaceRepo:     workspaceRepo,
		orgRepo:           orgRepo,
		authService:       authService,
		storageClient:     storageClient,
		configVersionRepo: configVersionRepo,
		vcsConnectionRepo: vcsConnectionRepo,
		vcsRegistry:       vcsRegistry,
		logBufferService:  logBufferService,
		phaseStateRepo:    phaseStateRepo,
		rbacService:       rbacService,
		stateVersionRepo:  stateVersionRepo,
	}
}

type CreateRunRequestV2 struct {
	Data struct {
		Type       string `json:"type"`
		Attributes struct {
			IsDestroy          *bool  `json:"is-destroy,omitempty"`
			Message            string `json:"message,omitempty"`
			AutoApplyAfterPlan *bool  `json:"auto-apply-after-plan,omitempty"` // For UI "Plan and Apply" runs
		} `json:"attributes,omitempty"`
		Relationships struct {
			Workspace struct {
				Data struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"workspace,omitempty"`
			ConfigurationVersion struct {
				Data struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"configuration-version,omitempty"`
		} `json:"relationships,omitempty"`
	} `json:"data,omitempty"`
	// Legacy format support (for backward compatibility)
	WorkspaceID            string  `json:"workspace_id"`
	ConfigurationVersionID *string `json:"configuration_version_id,omitempty"` // Format: cv-{16-char-id}
	Operation              string  `json:"operation"`
	AutoApplyAfterPlan     *bool   `json:"auto_apply_after_plan,omitempty"` // Legacy format
}

// formatRunResponse formats a run in TFE-compatible JSON:API format
// Based on TFE API spec: https://developer.hashicorp.com/terraform/enterprise/api-docs/run
// c is optional - if provided, will use auth_method from context to determine source
// runRepo is optional - if provided, will check for existing apply runs to determine can-apply
func formatRunResponse(run *models.Run, c *gin.Context, configVersionRepo *repository.ConfigurationVersionRepository, runRepo *repository.RunRepository) gin.H {
	// Build status-timestamps (TFE requires these for Terraform CLI to recognize completion)
	statusTimestamps := gin.H{}

	// Set status-timestamps based on run operation type and status
	switch run.Operation {
	case models.RunOperationPlanAndApply:
		// Plan-and-apply runs: planning → planned → applying → applied
		// planning-at: when plan started
		if run.StartedAt != nil {
			statusTimestamps["planning-at"] = run.StartedAt.Format("2006-01-02T15:04:05Z")
		}
		// planned-at: when plan phase completed
		// Include even when status is 'failed' if PlanCompletedAt is set (plan completed before apply failed)
		if run.PlanCompletedAt != nil {
			statusTimestamps["planned-at"] = run.PlanCompletedAt.Format("2006-01-02T15:04:05Z")
		}
		// applying-at: when apply phase started
		// Include even when status is 'failed' if ApplyStartedAt is set (apply started before it failed)
		if run.ApplyStartedAt != nil {
			statusTimestamps["applying-at"] = run.ApplyStartedAt.Format("2006-01-02T15:04:05Z")
		}
		// applied-at: when apply phase completed (status is 'applied')
		if run.Status == models.RunStatusApplied && run.CompletedAt != nil {
			statusTimestamps["applied-at"] = run.CompletedAt.Format("2006-01-02T15:04:05Z")
		}

	case models.RunOperationPlanOnly:
		// Plan-only runs: planning → planned
		// planning-at: when plan started
		if run.StartedAt != nil {
			statusTimestamps["planning-at"] = run.StartedAt.Format("2006-01-02T15:04:05Z")
		}
		// planned-at: when plan completed (status is 'planned' or 'completed')
		if run.Status == models.RunStatusPlanned || run.Status == models.RunStatusCompleted {
			if run.PlanCompletedAt != nil {
				statusTimestamps["planned-at"] = run.PlanCompletedAt.Format("2006-01-02T15:04:05Z")
			} else if run.CompletedAt != nil {
				statusTimestamps["planned-at"] = run.CompletedAt.Format("2006-01-02T15:04:05Z")
			}
		}

	case models.RunOperationDestroy:
		// TFE-compatible: Destroy runs follow the same two-phase flow as plan-and-apply
		// planning-at: when destroy plan started
		if run.StartedAt != nil {
			statusTimestamps["planning-at"] = run.StartedAt.Format("2006-01-02T15:04:05Z")
		}
		// planned-at: when destroy plan completed
		if run.PlanCompletedAt != nil {
			statusTimestamps["planned-at"] = run.PlanCompletedAt.Format("2006-01-02T15:04:05Z")
		}
		// applying-at: when destroy execution started (apply phase)
		if run.ApplyStartedAt != nil {
			statusTimestamps["applying-at"] = run.ApplyStartedAt.Format("2006-01-02T15:04:05Z")
		}
		// applied-at: when destroy execution completed
		if run.Status == models.RunStatusApplied && run.CompletedAt != nil {
			statusTimestamps["applied-at"] = run.CompletedAt.Format("2006-01-02T15:04:05Z")
		}
	}

	// Legacy: Support old statuses for backward compatibility
	if run.Status == models.RunStatusRunning && run.StartedAt != nil {
		// Check operation to determine if it's planning or applying phase
		if run.Operation == models.RunOperationPlanAndApply {
			// For plan-and-apply, check if we're past the plan phase
			if len(run.PlanOutput) > 0 {
				// Plan completed, so this must be applying
				statusTimestamps["applying-at"] = run.StartedAt.Format("2006-01-02T15:04:05Z")
			} else {
				// Plan phase
				statusTimestamps["planning-at"] = run.StartedAt.Format("2006-01-02T15:04:05Z")
			}
		} else {
			// Plan-only or legacy plan
			statusTimestamps["planning-at"] = run.StartedAt.Format("2006-01-02T15:04:05Z")
		}
	}
	if (run.Status == models.RunStatusCompleted || run.Status == models.RunStatusPlanned) && run.CompletedAt != nil {
		// Check if this is a plan completion or apply completion
		if run.Operation == models.RunOperationPlanAndApply {
			// For plan-and-apply, check if we have apply logs (stored separately)
			// If plan output exists but status is completed, it might be apply completion
			// For now, assume it's plan completion if status is "planned"
			if run.Status == models.RunStatusPlanned {
				statusTimestamps["planned-at"] = run.CompletedAt.Format("2006-01-02T15:04:05Z")
			}
		} else {
			// Plan-only or legacy: set planned-at
			statusTimestamps["planned-at"] = run.CompletedAt.Format("2006-01-02T15:04:05Z")
		}
	}

	// Set plan-queued-at when run is pending (queued for planning)
	if run.Status == models.RunStatusPending {
		statusTimestamps["plan-queued-at"] = run.CreatedAt.Format("2006-01-02T15:04:05Z")
	}

	// TFE-compatible: Determine run source based on TFE spec
	// TFE only has 3 sources: tfe-ui, tfe-api, tfe-configuration-version
	// According to TFE spec: If run is queued from a Configuration Version, source should be "tfe-configuration-version"
	// Priority: 1) Configuration version (if exists) → "tfe-configuration-version", 2) Auth method from context
	runSource := "tfe-api" // Default fallback
	var configVersion *models.ConfigurationVersion
	if run.ConfigurationVersionID != nil && configVersionRepo != nil {
		// TFE-compatible: Runs created from configuration versions have source="tfe-configuration-version"
		// This applies regardless of how the configuration version was created (VCS, CLI, UI, etc.)
		// The configuration version's source tells us how the config was created, but the run source
		// indicates the run was queued from a configuration version
		runSource = "tfe-configuration-version"
		// Fetch configuration version to check its source for plan-only logic
		var err error
		configVersion, err = configVersionRepo.GetByID(*run.ConfigurationVersionID)
		if err != nil {
			configVersion = nil // Ignore error, will use defaults
		}
	}

	// If source not determined from config version, check auth method from context
	// Note: In TFE, CLI runs always create a configuration version first, so this is a fallback
	if runSource == "tfe-api" && c != nil {
		if authMethod, exists := c.Get("auth_method"); exists {
			switch authMethod {
			case "jwt":
				// JWT token = UI/web interface
				runSource = "tfe-ui"
			case "tfe_token", "api_key":
				// TFE token or API key = API access (not UI)
				// In TFE, CLI runs create configuration versions, so this is rare
				runSource = "tfe-api"
			}
		}
	}

	// TFE-compatible: Determine plan-only based on run operation type
	// - plan-only runs: Cannot be applied (CLI runs, UI "Plan only" runs)
	// - plan-and-apply runs: Can be applied after plan phase completes
	planOnly := false
	switch run.Operation {
	case models.RunOperationPlanOnly:
		// Plan-only runs are always plan-only (cannot be applied)
		planOnly = true
	case models.RunOperationPlanAndApply:
		// Plan-and-apply runs are NOT plan-only (can be applied after plan completes)
		planOnly = false
	case models.RunOperationDestroy:
		// Destroy runs are NOT plan-only (they can be applied)
		planOnly = false
	}

	// TFE-compatible: can-apply permission logic
	// For plan-and-apply and destroy runs: can-apply is true when plan phase is completed (status="planned")
	// For plan-only runs: can-apply is always false
	// Destroy runs follow the same two-phase flow: plan -destroy → confirm → apply
	var canApply bool
	switch run.Operation {
	case models.RunOperationPlanAndApply, models.RunOperationDestroy:
		// Plan-and-apply or destroy run: can apply when plan phase is completed (status="planned")
		canApply = run.Status == models.RunStatusPlanned
	case models.RunOperationPlanOnly:
		// Plan-only run: cannot be applied
		canApply = false
	}

	// TFE-compatible: Status in API response
	// According to TFE API spec and design doc: "Run status stays as 'completed' (not mapped to 'planned') - Terraform CLI checks plan status, not run status"
	// Terraform CLI expects "completed" status for completed plan runs
	// The "planned-at" timestamp in status-timestamps also signals completion
	// We keep status as-is from database (should be "completed" for completed plan runs, not "planned")
	apiStatus := string(run.Status)

	attributes := gin.H{
		"status":            apiStatus,
		"operation":         string(run.Operation), // TFE-compatible: Include operation in attributes
		"is-destroy":        run.Operation == models.RunOperationDestroy,
		"plan-only":         planOnly, // TFE-compatible: Indicates if run is plan-only (cannot be applied)
		"message":           "",
		"source":            runSource, // TFE-compatible: "tfe-api", "tfe-ui", "tfe-configuration-version" (TFE only has these 3 sources)
		"created-at":        run.CreatedAt.Format("2006-01-02T15:04:05Z"),
		"updated-at":        run.UpdatedAt.Format("2006-01-02T15:04:05Z"),
		"status-timestamps": statusTimestamps,
		"has-changes":       hasChanges(run), // Set based on plan output
		"actions": gin.H{
			"is-cancelable":       run.Status == models.RunStatusRunning || run.Status == models.RunStatusPending || run.Status == models.RunStatusPlanning || run.Status == models.RunStatusApplying,
			"is-confirmable":      false,
			"is-discardable":      run.Status == models.RunStatusPending,
			"is-force-cancelable": false,
		},
		"permissions": gin.H{
			"can-apply":         canApply, // TFE-compatible: Only true if run is completed, plan operation, not plan-only, and not auto-applied
			"can-cancel":        true,
			"can-discard":       true,
			"can-force-execute": false,
			"can-force-cancel":  false,
		},
	}

	// Include configuration version details for context-aware display
	// This allows frontend to show "Triggered via CLI", "Triggered via UI", or "Triggered via VCS" with commit info
	if configVersion != nil {
		attributes["configuration-version-source"] = configVersion.Source // "tfe-vcs", "tfe-cli", "tfe-ui", "tfe-api"
		if configVersion.CommitHash != "" {
			attributes["commit-hash"] = configVersion.CommitHash
		}
		if configVersion.Committer != "" {
			attributes["committer"] = configVersion.Committer
		}
		if configVersion.PRNumber > 0 {
			attributes["pr-number"] = configVersion.PRNumber
		}
		if configVersion.SourceBranch != "" {
			attributes["source-branch"] = configVersion.SourceBranch
		}
	}

	// Add optional timestamp fields
	if run.StartedAt != nil {
		attributes["started-at"] = run.StartedAt.Format("2006-01-02T15:04:05Z")
	}
	if run.CompletedAt != nil {
		attributes["completed-at"] = run.CompletedAt.Format("2006-01-02T15:04:05Z")
	}
	if run.ErrorMessage != "" {
		attributes["error-message"] = run.ErrorMessage
	}

	// Self-hosted runner info
	if run.AgentPoolID != nil {
		attributes["agent-pool-id"] = run.AgentPoolID.String()
		if run.AgentPool != nil {
			attributes["agent-pool-name"] = run.AgentPool.Name
		}
	}
	if run.RunnerID != nil {
		attributes["runner-id"] = run.RunnerID.String()
		if run.Runner != nil {
			attributes["runner-name"] = run.Runner.Name
		}
	}

	// TFE-compatible: Plan output is NOT included in run response
	// Frontend should fetch from /api/v2/runs/:id/plan endpoint instead
	// This improves scalability and matches TFE behavior

	// Build relationships
	relationships := gin.H{
		"workspace": gin.H{
			"data": gin.H{
				"id":   run.WorkspaceID,
				"type": "workspaces",
			},
		},
	}

	if run.ConfigurationVersionID != nil {
		relationships["configuration-version"] = gin.H{
			"data": gin.H{
				"id":   *run.ConfigurationVersionID,
				"type": "configuration-versions",
			},
		}
	}

	// TFE requires a "plan" relationship for runs
	// For plan operations, the plan ID is typically the same as the run ID
	// For plan-and-apply and plan-only runs, include plan relationship
	// For destroy runs, also include plan relationship (destroy uses plan phase)
	if run.Operation == models.RunOperationPlanAndApply || run.Operation == models.RunOperationPlanOnly || run.Operation == models.RunOperationDestroy {
		relationships["plan"] = gin.H{
			"data": gin.H{
				"id":   run.ID, // Plan ID = run ID
				"type": "plans",
			},
		}
	}

	// TFE requires an "apply" relationship for plan-and-apply runs that have started apply phase
	// Apply ID = Run ID (same pattern as Plan ID = Run ID)
	if run.Operation == models.RunOperationPlanAndApply && run.ApplyStartedAt != nil {
		relationships["apply"] = gin.H{
			"data": gin.H{
				"id":   run.ID, // Apply ID = run ID
				"type": "applies",
			},
		}
	}

	return gin.H{
		"id":            run.ID,
		"type":          "runs",
		"attributes":    attributes,
		"relationships": relationships,
	}
}

// hasChanges determines if the run has changes based on plan output
// Matches TFE behavior: checks the resource_changes array in Terraform plan JSON
func hasChanges(run *models.Run) bool {
	// Check if this is a plan operation that has completed
	if (run.Operation != models.RunOperationPlanOnly && run.Operation != models.RunOperationPlanAndApply) ||
		(run.Status != models.RunStatusPlanned && run.Status != models.RunStatusCompleted) {
		return false
	}

	// Check if plan output exists and has resource changes
	if run.PlanOutput != nil {
		// Terraform plan JSON format: resource_changes is an array, not a number
		// Check the resource_changes array for any non-no-op changes
		if resourceChanges, ok := run.PlanOutput["resource_changes"].([]interface{}); ok {
			for _, change := range resourceChanges {
				if changeMap, ok := change.(map[string]interface{}); ok {
					if changeData, ok := changeMap["change"].(map[string]interface{}); ok {
						if actions, ok := changeData["actions"].([]interface{}); ok {
							// Check if any action is not "no-op"
							for _, a := range actions {
								if actionStr, ok := a.(string); ok && actionStr != "no-op" {
									return true // Found a non-no-op change
								}
							}
						}
					}
				}
			}
		}

		// Fallback: Check for pre-computed counts (if stored separately)
		if addCount, ok := run.PlanOutput["resource_additions"].(float64); ok && addCount > 0 {
			return true
		}
		if destroyCount, ok := run.PlanOutput["resource_destructions"].(float64); ok && destroyCount > 0 {
			return true
		}
		// Also check for AddCount, ChangeCount, DestroyCount (from our plugin, if stored)
		if addCount, ok := run.PlanOutput["AddCount"].(float64); ok && addCount > 0 {
			return true
		}
		if changeCount, ok := run.PlanOutput["ChangeCount"].(float64); ok && changeCount > 0 {
			return true
		}
		if destroyCount, ok := run.PlanOutput["DestroyCount"].(float64); ok && destroyCount > 0 {
			return true
		}

		// Check for output changes (output-only changes should still allow apply)
		if outputChanges, ok := run.PlanOutput["output_changes"].(map[string]interface{}); ok {
			for _, change := range outputChanges {
				if changeMap, ok := change.(map[string]interface{}); ok {
					if actions, ok := changeMap["actions"].([]interface{}); ok {
						for _, a := range actions {
							if actionStr, ok := a.(string); ok && actionStr != "no-op" {
								return true
							}
						}
					}
				}
			}
		}
		if outputChangeCount, ok := run.PlanOutput["OutputChangeCount"].(float64); ok && outputChangeCount > 0 {
			return true
		}
	}

	return false
}

// Create creates a new run (TFE-compatible)
// POST /api/v2/runs
func (h *RunHandlerV2) Create(c *gin.Context) {
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{
					"status": "401",
					"title":  "Unauthorized",
					"detail": "Authentication required",
				},
			},
		})
		return
	}

	var req CreateRunRequestV2
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": err.Error(),
				},
			},
		})
		return
	}

	// Parse workspace ID (support both JSON:API format and legacy format)
	var workspaceID string
	switch {
	case req.Data.Relationships.Workspace.Data.ID != "":
		workspaceID = req.Data.Relationships.Workspace.Data.ID
	case req.WorkspaceID != "":
		workspaceID = req.WorkspaceID
	default:
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Workspace ID is required",
				},
			},
		})
		return
	}

	// Parse configuration version ID (optional)
	var configVersionID *string
	if req.Data.Relationships.ConfigurationVersion.Data.ID != "" {
		configVersionID = &req.Data.Relationships.ConfigurationVersion.Data.ID
	} else if req.ConfigurationVersionID != nil {
		configVersionID = req.ConfigurationVersionID
	}

	// Determine operation type based on request
	// TFE-compatible: Two run types - "plan-only" and "plan-and-apply"
	// - "plan-only": CLI runs and UI "Plan only" runs (cannot be applied)
	// - "plan-and-apply": UI "Plan and Apply" runs (goes through planning → planned → applying → applied)
	// - "destroy": Destroy runs (tear down infrastructure)
	var operation models.RunOperation
	switch {
	case req.Data.Attributes.IsDestroy != nil && *req.Data.Attributes.IsDestroy:
		operation = models.RunOperationDestroy
	case req.Operation == "plan-and-apply":
		// Explicit operation from frontend or API caller
		operation = models.RunOperationPlanAndApply
	case req.Operation == "plan-only":
		operation = models.RunOperationPlanOnly
	case req.Operation == "destroy":
		operation = models.RunOperationDestroy
	default:
		// Fallback: use autoApplyAfterPlan to determine if this is a plan-and-apply run
		// This maintains backward compatibility with go-tfe client
		autoApplyAfterPlan := false
		if req.Data.Attributes.AutoApplyAfterPlan != nil {
			autoApplyAfterPlan = *req.Data.Attributes.AutoApplyAfterPlan
		} else if req.AutoApplyAfterPlan != nil {
			autoApplyAfterPlan = *req.AutoApplyAfterPlan
		}

		// Reject legacy "plan" and "apply" operations
		if req.Operation == "plan" || req.Operation == "apply" {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "Legacy 'plan' and 'apply' operations are no longer supported. Use 'plan-only' or 'plan-and-apply' instead.",
					},
				},
			})
			return
		}

		if autoApplyAfterPlan {
			operation = models.RunOperationPlanAndApply
		} else {
			operation = models.RunOperationPlanOnly
		}
	}

	// Verify workspace exists and get it
	workspace, err := h.workspaceRepo.GetByID(workspaceID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Workspace not found",
				},
			},
		})
		return
	}

	// CRITICAL: Check if user has permission to create/plan runs
	// Viewers should NOT be able to create runs - they only have RunRead permission, not RunWrite
	// Creating a plan run requires both PermissionRuns (granular) AND PermissionWorkspaceWrite
	// Check granular runs permission first
	hasRunsPermission, err := h.rbacService.CheckResourcePermission(
		c.Request.Context(),
		user.ID,
		rbac.ResourceTypeTerraformWorkspace,
		workspaceID,
		rbac.PermissionRuns, // Granular permission for runs
		&workspace.ProjectID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to check permissions",
				},
			},
		})
		return
	}
	// Also check workspace write permission - viewers don't have this
	hasWorkspaceWrite, err := h.rbacService.CheckResourcePermission(
		c.Request.Context(),
		user.ID,
		rbac.ResourceTypeTerraformWorkspace,
		workspaceID,
		rbac.PermissionWorkspaceWrite, // Required to create/modify runs
		&workspace.ProjectID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to check permissions",
				},
			},
		})
		return
	}
	// Both permissions required - viewers have neither PermissionRuns nor PermissionWorkspaceWrite
	if !hasRunsPermission || !hasWorkspaceWrite {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "You do not have permission to create runs in this workspace. Viewers can only view runs, not create or plan them.",
				},
			},
		})
		return
	}

	// Check if workspace is manually locked
	// If locked, new runs cannot be created until user unlocks it
	if workspace.Locked {
		c.JSON(http.StatusConflict, gin.H{
			"errors": []gin.H{
				{
					"status": "409",
					"title":  "Conflict",
					"detail": "Workspace is locked. Unlock the workspace to create new runs.",
				},
			},
		})
		return
	}

	// TFE-compatible: If no configuration version provided and workspace has VCS configured,
	// automatically create a configuration version by cloning from VCS
	// This allows UI runs to work without requiring manual configuration version creation
	// For agent-mode workspaces, skip platform-side VCS clone — the self-hosted runner
	// will clone the repository itself using the VCS info from the job artifacts
	if configVersionID == nil && workspace.VCSConnectionID != nil && workspace.VCSRepository != "" && workspace.VCSBranch != "" && workspace.ExecutionMode != "agent" {
		logger.Infof("Run creation: No configuration version provided, but workspace has VCS configured. Creating configuration version from VCS.")

		// Get VCS connection
		vcsConn, err := h.vcsConnectionRepo.GetByID(*workspace.VCSConnectionID)
		if err != nil {
			logger.Infof("Run creation: Failed to get VCS connection: %v", err)
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "Failed to get VCS connection for workspace",
					},
				},
			})
			return
		}

		// Create configuration version from VCS
		createdConfigVersionID, err := h.createConfigurationVersionFromVCS(c.Request.Context(), workspace, vcsConn)
		if err != nil {
			logger.Infof("Run creation: Failed to create configuration version from VCS: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{
					{
						"status": "500",
						"title":  "Internal Server Error",
						"detail": fmt.Sprintf("Failed to create configuration version from VCS: %v", err),
					},
				},
			})
			return
		}
		configVersionID = &createdConfigVersionID
		logger.Infof("Run creation: Created configuration version %s from VCS", *configVersionID)
	}

	// TFE-compatible: Auto-cancel previous pending/running/planned runs based on operation type
	// This uses the centralized auto-cancel logic
	AutoCancelConflictingRuns(h.runRepo, workspaceID, operation)

	// Check for duplicate run created in the last 10 seconds (Terraform CLI retry protection)
	// This prevents duplicate runs when Terraform CLI retries due to network issues
	// Check by workspace and operation only (ignore config version to catch duplicates even if config changes)
	// IMPORTANT: Check BEFORE creating to avoid race conditions
	// Check for ANY recent run (pending, running, or just created) - not just pending
	recentRuns, _, err := h.runRepo.ListByWorkspace(workspaceID, 10, 0)
	if err == nil {
		now := time.Now()
		for _, existingRun := range recentRuns {
			// Check if there's a very recent run (within 10 seconds) with same operation
			// Ignore config version differences - if same workspace + operation within 10 seconds, it's likely a duplicate
			// Check for pending OR running status (not just pending) to catch duplicates even if first run started processing
			timeDiff := now.Sub(existingRun.CreatedAt)
			if timeDiff < 10*time.Second &&
				existingRun.Operation == operation &&
				(existingRun.Status == models.RunStatusPending || existingRun.Status == models.RunStatusRunning) {
				logger.Infof("Detected potential duplicate run creation for workspace %s, operation %s (existing run %s is %s). Returning existing run.", workspaceID, operation, existingRun.ID, existingRun.Status)
				c.JSON(http.StatusCreated, gin.H{
					"data": formatRunResponse(&existingRun, c, h.configVersionRepo, h.runRepo),
				})
				return
			}
		}
	}

	// TFE-compatible: Extract auto_apply_after_plan from request
	// This flag indicates if a UI "Plan and Apply" run should be applicable after completion
	// - false: "Plan only" run (plan-only, cannot be applied)
	// - true: "Plan and Apply" run (not plan-only, can be applied after completion)
	autoApplyAfterPlan := false
	switch {
	case req.Data.Attributes.AutoApplyAfterPlan != nil:
		autoApplyAfterPlan = *req.Data.Attributes.AutoApplyAfterPlan
		logger.Infof("Run creation: auto_apply_after_plan from JSON:API format = %v", autoApplyAfterPlan)
	case req.AutoApplyAfterPlan != nil:
		// Legacy format support
		autoApplyAfterPlan = *req.AutoApplyAfterPlan
		logger.Infof("Run creation: auto_apply_after_plan from legacy format = %v", autoApplyAfterPlan)
	default:
		logger.Infof("Run creation: auto_apply_after_plan not provided, defaulting to false")
	}

	// TFE-compatible: All runs follow 2-phase process (plan, then confirm apply)
	// UI "Plan and Apply" is just a regular plan run - user will see plan output and click "Apply Plan" button
	// Only VCS push events can auto-apply (if workspace.AutoApply is enabled)
	// CLI runs should NEVER auto-apply (they're just for preview)
	run := &models.Run{
		WorkspaceID:            workspaceID,
		ConfigurationVersionID: configVersionID,
		CreatedBy:              &user.ID,
		Status:                 models.RunStatusPending,
		Operation:              operation,
		AutoApplyAfterPlan:     autoApplyAfterPlan,    // TFE-compatible: Used to determine plan-only vs applicable
		AgentPoolID:            workspace.AgentPoolID, // Set from current workspace (remote => nil, agent => pool)
	}

	if err := h.runRepo.Create(run); err != nil {
		// If creation fails due to duplicate (race condition), try to get the existing run
		if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
			recentRuns, err2 := h.runRepo.ListByWorkspaceAndOperationAndConfigVersion(workspaceID, operation, configVersionID, 1)
			if err2 == nil && len(recentRuns) > 0 {
				latestRun := recentRuns[0]
				logger.Infof("Run creation failed due to duplicate, returning existing run %s.", latestRun.ID)
				c.JSON(http.StatusCreated, gin.H{
					"data": formatRunResponse(&latestRun, c, h.configVersionRepo, h.runRepo),
				})
				return
			}
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to create run",
				},
			},
		})
		return
	}

	// Reload run to ensure all fields are populated
	run, err = h.runRepo.GetByID(run.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve created run",
				},
			},
		})
		return
	}

	// TFE-compatible response format
	c.JSON(http.StatusCreated, gin.H{
		"data": formatRunResponse(run, c, h.configVersionRepo, h.runRepo),
	})
}

// Get returns a single run by ID (TFE-compatible)
// GET /api/v2/runs/:id
func (h *RunHandlerV2) Get(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid run ID",
				},
			},
		})
		return
	}

	run, err := h.runRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Run not found",
				},
			},
		})
		return
	}

	// TFE-compatible response format
	c.JSON(http.StatusOK, gin.H{
		"data": formatRunResponse(run, c, h.configVersionRepo, h.runRepo),
	})
}

// GetOutputs returns outputs from the state version created by this run (TFE-aligned).
// GET /api/v2/runs/:id/outputs
// Returns 404 when the run has no associated state version (e.g. not yet applied, or destroy).
func (h *RunHandlerV2) GetOutputs(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid run ID"},
			},
		})
		return
	}

	run, err := h.runRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "Run not found"},
			},
		})
		return
	}

	version, err := h.stateVersionRepo.GetByRunID(id)
	if err != nil || version == nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "No state version for this run"},
			},
		})
		return
	}

	workspace, err := h.workspaceRepo.GetByID(run.WorkspaceID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "Workspace not found"},
			},
		})
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

	ok, err := h.rbacService.CheckStateVersionPermission(c.Request.Context(), user.ID, version.WorkspaceID, workspace.ProjectID, "read-outputs")
	if err != nil || !ok {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "Insufficient permissions to view run outputs"},
			},
		})
		return
	}

	outputs := extractOutputsFromStateData(version, true)
	c.JSON(http.StatusOK, gin.H{"data": outputs})
}

// GetPlan returns the plan output for a run (TFE-compatible)
// GET /api/v2/runs/:id/plan
func (h *RunHandlerV2) GetPlan(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid run ID",
				},
			},
		})
		return
	}

	run, err := h.runRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Run not found",
				},
			},
		})
		return
	}

	// Calculate resource counts from plan output if available
	resourceAdditions := 0
	resourceChanges := 0
	resourceDestructions := 0
	resourceImports := 0

	if run.PlanOutput != nil {
		// Terraform plan JSON format: resource_changes is an array
		// Compute counts from the resource_changes array (matches TFE behavior)
		if resourceChangesArray, ok := run.PlanOutput["resource_changes"].([]interface{}); ok {
			for _, change := range resourceChangesArray {
				if changeMap, ok := change.(map[string]interface{}); ok {
					if changeData, ok := changeMap["change"].(map[string]interface{}); ok {
						if actions, ok := changeData["actions"].([]interface{}); ok {
							// Count each action type
							for _, a := range actions {
								if actionStr, ok := a.(string); ok {
									switch actionStr {
									case "create":
										resourceAdditions++
									case "update":
										resourceChanges++
									case "delete":
										resourceDestructions++
									case "read":
										// Read actions don't count as changes
									case "no-op":
										// No-op actions don't count as changes
									}
								}
							}
						}
					}
				}
			}
		}

		// Fallback: Check for pre-computed counts (if stored separately)
		if addCount, ok := run.PlanOutput["resource_additions"].(float64); ok && resourceAdditions == 0 {
			resourceAdditions = int(addCount)
		}
		if destroyCount, ok := run.PlanOutput["resource_destructions"].(float64); ok && resourceDestructions == 0 {
			resourceDestructions = int(destroyCount)
		}
		// Also check for AddCount, ChangeCount, DestroyCount (from our plugin, if stored)
		if addCount, ok := run.PlanOutput["AddCount"].(float64); ok && resourceAdditions == 0 {
			resourceAdditions = int(addCount)
		}
		if changeCount, ok := run.PlanOutput["ChangeCount"].(float64); ok && resourceChanges == 0 {
			resourceChanges = int(changeCount)
		}
		if destroyCount, ok := run.PlanOutput["DestroyCount"].(float64); ok && resourceDestructions == 0 {
			resourceDestructions = int(destroyCount)
		}
	}

	// Count output changes from the plan JSON
	outputChangeCount := 0
	if run.PlanOutput != nil {
		if outputChanges, ok := run.PlanOutput["output_changes"].(map[string]interface{}); ok {
			for _, change := range outputChanges {
				if changeMap, ok := change.(map[string]interface{}); ok {
					if actions, ok := changeMap["actions"].([]interface{}); ok {
						for _, a := range actions {
							if actionStr, ok := a.(string); ok && actionStr != "no-op" {
								outputChangeCount++
								break
							}
						}
					}
				}
			}
		}
		// Fallback to pre-computed count
		if occ, ok := run.PlanOutput["OutputChangeCount"].(float64); ok && outputChangeCount == 0 {
			outputChangeCount = int(occ)
		}
	}

	// Determine plan status according to TFE Plans API spec
	// Status values: pending, managed_queued/queued, running, errored, canceled, finished, unreachable
	planStatus := "pending"
	planStatusTimestamps := gin.H{}

	switch run.Status {
	case models.RunStatusPending:
		planStatus = "pending"
	case models.RunStatusPlanning:
		planStatus = "running"
		if run.StartedAt != nil {
			planStatusTimestamps["started-at"] = run.StartedAt.Format("2006-01-02T15:04:05Z")
		}
	case models.RunStatusPlanned:
		planStatus = "finished" // TFE uses "finished" for completed plans
		if run.PlanCompletedAt != nil {
			planStatusTimestamps["finished-at"] = run.PlanCompletedAt.Format("2006-01-02T15:04:05Z")
		}
		if run.StartedAt != nil {
			planStatusTimestamps["started-at"] = run.StartedAt.Format("2006-01-02T15:04:05Z")
		}
	case models.RunStatusApplying:
		planStatus = "running" // Apply phase is still running
		if run.ApplyStartedAt != nil {
			planStatusTimestamps["started-at"] = run.ApplyStartedAt.Format("2006-01-02T15:04:05Z")
		}
	case models.RunStatusApplied:
		planStatus = "finished"
		if run.CompletedAt != nil {
			planStatusTimestamps["finished-at"] = run.CompletedAt.Format("2006-01-02T15:04:05Z")
		}
		if run.ApplyStartedAt != nil {
			planStatusTimestamps["started-at"] = run.ApplyStartedAt.Format("2006-01-02T15:04:05Z")
		}
	case models.RunStatusFailed:
		planStatus = "errored"
	case models.RunStatusCancelled:
		planStatus = "canceled"
	case models.RunStatusRunning:
		planStatus = "running"
		if run.StartedAt != nil {
			planStatusTimestamps["started-at"] = run.StartedAt.Format("2006-01-02T15:04:05Z")
		}
	case models.RunStatusCompleted:
		planStatus = "finished" // TFE uses "finished" for completed plans, not "completed" or "planned"
		if run.CompletedAt != nil {
			planStatusTimestamps["finished-at"] = run.CompletedAt.Format("2006-01-02T15:04:05Z")
		}
		if run.StartedAt != nil {
			planStatusTimestamps["started-at"] = run.StartedAt.Format("2006-01-02T15:04:05Z")
		}
	}

	// Set queued-at and pending-at timestamps
	if run.Status == models.RunStatusPending {
		planStatusTimestamps["pending-at"] = run.CreatedAt.Format("2006-01-02T15:04:05Z")
		planStatusTimestamps["queued-at"] = run.CreatedAt.Format("2006-01-02T15:04:05Z")
	} else if run.StartedAt != nil {
		// If run has started, set queued-at to created-at (when it was queued)
		planStatusTimestamps["queued-at"] = run.CreatedAt.Format("2006-01-02T15:04:05Z")
		planStatusTimestamps["pending-at"] = run.CreatedAt.Format("2006-01-02T15:04:05Z")
	}

	// Determine has-changes based on resource and output counts
	hasChanges := resourceAdditions > 0 || resourceChanges > 0 || resourceDestructions > 0 || outputChangeCount > 0

	attributes := gin.H{
		"execution-details": gin.H{
			"mode": "remote", // TFE execution mode: remote, local, or agent
		},
		"generated-configuration": false,
		"has-changes":             hasChanges,
		"resource-additions":      resourceAdditions,
		"resource-changes":        resourceChanges,
		"resource-destructions":   resourceDestructions,
		"resource-imports":        resourceImports,
		"status":                  planStatus,
		"status-timestamps":       planStatusTimestamps,
	}

	// TFE-compatible: Include plan JSON output in attributes for frontend
	// The plan JSON contains resource_changes, planned_values, etc.
	if len(run.PlanOutput) > 0 {
		attributes["plan-json"] = run.PlanOutput
	}

	// TFE-compatible: log-read-url should be an absolute URL
	// TFE includes log-read-url for all runs (even pending/running), but the endpoint returns empty until logs are ready
	// Build absolute URL using request headers (respects X-Forwarded-Host, X-Forwarded-Proto)
	scheme := "http"
	if c.GetHeader("X-Forwarded-Proto") == "https" || c.Request.TLS != nil {
		scheme = "https"
	}

	host := c.GetHeader("X-Forwarded-Host")
	if host == "" {
		host = c.Request.Host
	}
	if host == "" {
		host = "localhost:8022"
	}

	// TFE uses absolute URLs for log-read-url - include it for all runs
	// The endpoint will return empty body (200 OK) if logs don't exist yet
	// According to TFE API docs, all endpoints use Authorization header for authentication
	// Terraform CLI will use the same Authorization header it uses for other API calls
	// However, TFE may include token in query parameter for log-read-url as a fallback
	// Extract token from request (if available) to include in log-read-url as fallback
	token := ""
	if authHeader := c.GetHeader("Authorization"); authHeader != "" {
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) == 2 && parts[0] == "Bearer" {
			token = parts[1]
		}
	}

	// Build log-read-url - TFE includes token in query parameter as fallback authentication
	// Terraform CLI should use Authorization header, but token in URL provides compatibility
	if token != "" {
		attributes["log-read-url"] = fmt.Sprintf("%s://%s/api/v2/runs/%s/logs?token=%s", scheme, host, run.ID, token)
	} else {
		attributes["log-read-url"] = fmt.Sprintf("%s://%s/api/v2/runs/%s/logs", scheme, host, run.ID)
	}

	// Build relationships and links according to TFE Plans API spec
	// TFE Plans API requires relationships.state-versions and links (self, json-output)
	relationships := gin.H{
		"state-versions": gin.H{
			"data": []gin.H{}, // Empty array - state versions are linked separately
		},
	}

	// Build absolute URLs for links
	planSelfURL := fmt.Sprintf("%s://%s/api/v2/plans/%s", scheme, host, run.ID)
	planJSONOutputURL := fmt.Sprintf("%s://%s/api/v2/plans/%s/json-output", scheme, host, run.ID)

	links := gin.H{
		"self":        planSelfURL,
		"json-output": planJSONOutputURL,
	}

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"id":            run.ID,
			"type":          "plans",
			"attributes":    attributes,
			"relationships": relationships,
			"links":         links,
		},
	})
}

// GetApply returns the apply output for a run (TFE-compatible)
// GET /api/v2/applies/:id
// Per TFE API docs: https://developer.hashicorp.com/terraform/enterprise/api-docs/applies
// Apply ID = Run ID (for plan-and-apply runs)
func (h *RunHandlerV2) GetApply(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid apply ID",
				},
			},
		})
		return
	}

	// Apply ID = Run ID (same pattern as Plan ID = Run ID)
	// Need PlanOutput for extracting planned resources for log parsing
	run, err := h.runRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Apply not found",
				},
			},
		})
		return
	}

	// Plan-and-apply and destroy runs have apply phases (destroy uses same two-phase flow)
	if run.Operation != models.RunOperationPlanAndApply && run.Operation != models.RunOperationDestroy {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Apply not found (only plan-and-apply and destroy runs have apply phases)",
				},
			},
		})
		return
	}

	// Calculate resource counts from stored phase state or plan output
	resourceAdditions := 0
	resourceChanges := 0
	resourceDestructions := 0
	resourceImports := 0
	var applyResources []models.ResourceState

	// Try to get stored phase state first
	phaseState, err := h.phaseStateRepo.GetByRunIDAndPhase(run.ID, "apply")
	if err == nil && phaseState != nil {
		// Use stored state
		applyResources = phaseState.Resources
		resourceAdditions = phaseState.Summary.Additions
		resourceChanges = phaseState.Summary.Changes
		resourceDestructions = phaseState.Summary.Destructions
	} else if h.storageClient != nil {
		// Fallback: parse apply logs from storage (e.g. self-hosted destroy runs) so apply-resources and status update
		logsKey := fmt.Sprintf("runs/%s/logs/apply.log", run.ID)
		if logsBytes, getErr := h.storageClient.Get(c.Request.Context(), logsKey); getErr == nil && len(logsBytes) > 0 {
			logsStr := string(logsBytes)
			var plannedResources []logparser.PlannedResource
			if run.PlanOutput != nil {
				plannedResources = logparser.ExtractPlannedResourcesFromPlanOutput(map[string]interface{}(run.PlanOutput))
			}
			if parseResult, parseErr := logparser.ParseApplyLogs(logsStr, plannedResources); parseErr == nil {
				applyResources = parseResult.Resources
				resourceAdditions = parseResult.Summary.Additions
				resourceChanges = parseResult.Summary.Changes
				resourceDestructions = parseResult.Summary.Destructions
			}
		}
	}
	if len(applyResources) == 0 && run.PlanOutput != nil {
		// Fallback: counts from plan output only
		if resourceChangesArray, ok := run.PlanOutput["resource_changes"].([]interface{}); ok {
			for _, change := range resourceChangesArray {
				if changeMap, ok := change.(map[string]interface{}); ok {
					if changeData, ok := changeMap["change"].(map[string]interface{}); ok {
						if actions, ok := changeData["actions"].([]interface{}); ok {
							for _, a := range actions {
								if actionStr, ok := a.(string); ok {
									switch actionStr {
									case "create":
										resourceAdditions++
									case "update":
										resourceChanges++
									case "delete":
										resourceDestructions++
									}
								}
							}
						}
					}
				}
			}
		}
	}

	// Determine apply status according to TFE Applies API spec
	// Status values: pending, queued, running, finished, errored, canceled, unreachable
	applyStatus := "pending"
	applyStatusTimestamps := gin.H{}

	switch run.Status {
	case models.RunStatusPending:
		applyStatus = "pending"
	case models.RunStatusPlanning:
		applyStatus = "pending" // Plan phase hasn't started apply yet
	case models.RunStatusPlanned:
		applyStatus = "pending" // Plan completed, waiting for apply
	case models.RunStatusRunning:
		applyStatus = "running"
		if run.ApplyStartedAt != nil {
			applyStatusTimestamps["started-at"] = run.ApplyStartedAt.Format("2006-01-02T15:04:05Z")
		} else if run.StartedAt != nil {
			applyStatusTimestamps["started-at"] = run.StartedAt.Format("2006-01-02T15:04:05Z")
		}
	case models.RunStatusCompleted:
		applyStatus = "finished"
		if run.CompletedAt != nil {
			applyStatusTimestamps["finished-at"] = run.CompletedAt.Format("2006-01-02T15:04:05Z")
		}
		if run.ApplyStartedAt != nil {
			applyStatusTimestamps["started-at"] = run.ApplyStartedAt.Format("2006-01-02T15:04:05Z")
		}
	case models.RunStatusFailed:
		applyStatus = "errored"
		if run.CompletedAt != nil {
			applyStatusTimestamps["finished-at"] = run.CompletedAt.Format("2006-01-02T15:04:05Z")
		}
	case models.RunStatusCancelled:
		applyStatus = "canceled"
		if run.CompletedAt != nil {
			applyStatusTimestamps["finished-at"] = run.CompletedAt.Format("2006-01-02T15:04:05Z")
		}
	case models.RunStatusApplying:
		applyStatus = "running"
		if run.ApplyStartedAt != nil {
			applyStatusTimestamps["started-at"] = run.ApplyStartedAt.Format("2006-01-02T15:04:05Z")
		}
	case models.RunStatusApplied:
		applyStatus = "finished"
		if run.CompletedAt != nil {
			applyStatusTimestamps["finished-at"] = run.CompletedAt.Format("2006-01-02T15:04:05Z")
		}
		if run.ApplyStartedAt != nil {
			applyStatusTimestamps["started-at"] = run.ApplyStartedAt.Format("2006-01-02T15:04:05Z")
		}
	}

	// Set queued-at timestamp
	if run.ApplyStartedAt != nil {
		// queued-at is when apply was queued (before started-at)
		// For plan-and-apply runs, this is when plan completed
		if run.PlanCompletedAt != nil {
			applyStatusTimestamps["queued-at"] = run.PlanCompletedAt.Format("2006-01-02T15:04:05Z")
		} else if run.ApplyStartedAt != nil {
			// Fallback to apply started time if plan completed time not available
			applyStatusTimestamps["queued-at"] = run.ApplyStartedAt.Format("2006-01-02T15:04:05Z")
		}
	}

	attributes := gin.H{
		"execution-details": gin.H{
			"mode": "remote", // TFE execution mode: remote, local, or agent
		},
		"status":                applyStatus,
		"status-timestamps":     applyStatusTimestamps,
		"resource-additions":    resourceAdditions,
		"resource-changes":      resourceChanges,
		"resource-destructions": resourceDestructions,
		"resource-imports":      resourceImports,
	}

	// Include apply-resources if available (from stored phase state)
	if len(applyResources) > 0 {
		// Convert ResourceState to JSON-compatible format
		applyResourcesJSON := make([]gin.H, len(applyResources))
		for i, res := range applyResources {
			resourceJSON := gin.H{
				"address": res.Address,
				"status":  res.Status,
				"action":  res.Action,
			}
			if res.ResourceID != "" {
				resourceJSON["resource_id"] = res.ResourceID
			}
			if res.CreatedAt != nil {
				resourceJSON["created_at"] = res.CreatedAt.Format("2006-01-02T15:04:05Z")
			}
			if res.ErrorMessage != "" {
				resourceJSON["error_message"] = res.ErrorMessage
			}
			if res.Details != "" {
				resourceJSON["details"] = res.Details
			}
			applyResourcesJSON[i] = resourceJSON
		}
		attributes["apply-resources"] = applyResourcesJSON
	}

	// Build log-read-url (TFE-compatible)
	scheme := "http"
	if c.GetHeader("X-Forwarded-Proto") == "https" || c.Request.TLS != nil {
		scheme = "https"
	}

	host := c.GetHeader("X-Forwarded-Host")
	if host == "" {
		host = c.Request.Host
	}
	if host == "" {
		host = "localhost:8022"
	}

	// Extract token from request for log-read-url
	token := ""
	if authHeader := c.GetHeader("Authorization"); authHeader != "" {
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) == 2 && parts[0] == "Bearer" {
			token = parts[1]
		}
	}

	// Build log-read-url with phase parameter for apply logs
	if token != "" {
		attributes["log-read-url"] = fmt.Sprintf("%s://%s/api/v2/runs/%s/logs?phase=apply&token=%s", scheme, host, run.ID, token)
	} else {
		attributes["log-read-url"] = fmt.Sprintf("%s://%s/api/v2/runs/%s/logs?phase=apply", scheme, host, run.ID)
	}

	// Build relationships according to TFE Applies API spec
	relationships := gin.H{
		"state-versions": gin.H{
			"data": []gin.H{}, // Empty array - state versions are linked separately
		},
	}

	// Build absolute URLs for links
	applySelfURL := fmt.Sprintf("%s://%s/api/v2/applies/%s", scheme, host, run.ID)

	links := gin.H{
		"self": applySelfURL,
	}

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"id":            run.ID, // Apply ID = Run ID
			"type":          "applies",
			"attributes":    attributes,
			"relationships": relationships,
			"links":         links,
		},
	})
}

// GetLogs returns the logs for a run (TFE-compatible)
// GET /api/v2/runs/:id/logs
// Supports all execution modes: remote, local, and agent
// TFE returns logs as plain text
// TFE supports token authentication via query parameter (for Terraform CLI)
func (h *RunHandlerV2) GetLogs(c *gin.Context) {
	logger.Infof("[LOGS DEBUG] GetLogs called for run ID: %s", c.Param("id"))
	// TFE-compatible: Support token in query parameter (for Terraform CLI log-read-url)
	// If token is in query, authenticate with it; otherwise rely on standard auth middleware
	tokenFromQuery := c.Query("token")
	if tokenFromQuery != "" {
		// Authenticate using token from query parameter (TFE-compatible)
		user, err := h.authService.GetUserFromToken(tokenFromQuery)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{
				"errors": []gin.H{
					{
						"status": "401",
						"title":  "Unauthorized",
						"detail": "Invalid token",
					},
				},
			})
			return
		}
		// Store user in context for potential future use
		c.Set("user_id", user.ID)
	}

	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid run ID",
				},
			},
		})
		return
	}

	run, err := h.runRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Run not found",
				},
			},
		})
		return
	}

	// Get workspace to check execution mode
	workspace, err := h.workspaceRepo.GetByID(run.WorkspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to get workspace",
				},
			},
		})
		return
	}

	// Determine phase for log retrieval
	// If phase query parameter is provided, use it (allows explicit phase selection)
	// Otherwise, determine phase based on run status (backward compatible)
	var phase string
	if phaseParam := c.Query("phase"); phaseParam != "" {
		// Explicit phase requested via query parameter
		// Validate that it's a valid phase for this operation
		// TFE-compatible: both plan-and-apply and destroy runs use "plan" and "apply" phases
		if run.Operation == models.RunOperationPlanAndApply || run.Operation == models.RunOperationDestroy {
			if phaseParam == "plan" || phaseParam == "apply" {
				phase = phaseParam
			} else {
				c.JSON(http.StatusBadRequest, gin.H{
					"errors": []gin.H{
						{
							"status": "400",
							"title":  "Bad Request",
							"detail": "Invalid phase parameter. Must be 'plan' or 'apply'",
						},
					},
				})
				return
			}
		} else {
			// For plan-only runs, phase must match operation
			if phaseParam == string(run.Operation) {
				phase = phaseParam
			} else {
				c.JSON(http.StatusBadRequest, gin.H{
					"errors": []gin.H{
						{
							"status": "400",
							"title":  "Bad Request",
							"detail": fmt.Sprintf("Invalid phase parameter. Must be '%s' for %s runs", run.Operation, run.Operation),
						},
					},
				})
				return
			}
		}
	} else {
		// No phase parameter - use backward compatible behavior (determine by status)
		// TFE-compatible: destroy runs follow the same two-phase flow as plan-and-apply
		if run.Operation == models.RunOperationPlanAndApply || run.Operation == models.RunOperationDestroy {
			switch run.Status {
			case models.RunStatusPending, models.RunStatusPlanning, models.RunStatusPlanned:
				phase = "plan"
			case models.RunStatusApplying, models.RunStatusApplied:
				phase = "apply"
			case models.RunStatusFailed, models.RunStatusCancelled, models.RunStatusRunning, models.RunStatusCompleted:
				// For other statuses, default to plan (logs may still be streaming)
				phase = "plan"
			}
		} else {
			// For other operations (plan-only), use operation name as phase
			phase = string(run.Operation)
		}
	}

	// Get offset and limit from query parameters (used for both Redis and MinIO)
	offset := 0
	if offsetStr := c.Query("offset"); offsetStr != "" {
		if parsedOffset, err := strconv.Atoi(offsetStr); err == nil && parsedOffset >= 0 {
			offset = parsedOffset
		}
	}
	limit := 0 // 0 means no limit
	if limitStr := c.Query("limit"); limitStr != "" {
		if parsedLimit, err := strconv.Atoi(limitStr); err == nil && parsedLimit > 0 {
			limit = parsedLimit
		}
	}

	// Handle different execution modes
	var logs []byte
	switch workspace.ExecutionMode {
	case "remote":
		// Remote execution: check Redis first (for active runs), then fall back to MinIO
		var logsStr string

		// Try Redis first if log buffer service is available
		if h.logBufferService != nil {
			ctx := context.Background()
			logsStr, err = h.logBufferService.Get(ctx, run.ID, phase, offset, limit)
			if err == nil && logsStr != "" {
				// Found logs in Redis, convert to bytes
				logs = []byte(logsStr)
			} else if err != nil {
				// Error accessing Redis, log but continue to MinIO fallback
				logger.Infof("[LOGS] Error getting logs from Redis for run %s phase %s: %v", run.ID, phase, err)
			}
			// If logsStr is empty, continue to MinIO fallback
		}

		// Fall back to MinIO if Redis didn't have logs
		if len(logs) == 0 {
			if h.storageClient == nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"errors": []gin.H{
						{
							"status": "503",
							"title":  "Service Unavailable",
							"detail": "Storage client not initialized",
						},
					},
				})
				return
			}

			// TFE-compatible log path: runs/{run_id}/logs/{phase}.log
			// Both plan-and-apply and destroy runs use plan/apply phases
			var logsKey string
			if run.Operation == models.RunOperationPlanAndApply || run.Operation == models.RunOperationDestroy {
				logsKey = fmt.Sprintf("runs/%s/logs/%s.log", run.ID, phase)
			} else {
				logsKey = fmt.Sprintf("runs/%s/logs/%s.log", run.ID, run.Operation)
			}
			logs, err = h.storageClient.Get(context.Background(), logsKey)
			if err != nil {
				// REMOVED: Fallback logic that returned plan logs when apply logs don't exist
				// This was causing plan logs to appear in apply phase terminal output
				// TFE returns 200 OK with empty body when logs don't exist (not 204 or 404)
				// This allows Terraform to successfully fetch the endpoint even if logs aren't ready yet
				// Log the error for debugging but don't return it to client
				logger.Infof("[LOGS] Logs not found for run %s at %s: %v", run.ID, logsKey, err)
				c.Data(http.StatusOK, "text/plain", []byte(""))
				return
			}

			// Apply offset/limit to MinIO logs (if they weren't applied by Redis)
			if len(logs) > 0 && (offset > 0 || limit > 0) {
				if offset >= len(logs) {
					logs = []byte("")
				} else {
					end := offset + limit
					if limit <= 0 || end > len(logs) {
						end = len(logs)
					}
					logs = logs[offset:end]
				}
			}
		}

	case "local":
		// Local execution: logs are generated on user's machine
		// For now, return 200 OK with empty body (local runs don't send logs to backend)
		// TODO: Implement log upload endpoint for local execution mode
		c.Data(http.StatusOK, "text/plain", []byte(""))
		return

	case "agent":
		// Agent execution: logs sent by agent to backend, stored in MinIO
		if h.storageClient == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"errors": []gin.H{
					{
						"status": "503",
						"title":  "Service Unavailable",
						"detail": "Storage client not initialized",
					},
				},
			})
			return
		}

		// Use the phase determined above (either from query parameter or status-based)
		logsKey := fmt.Sprintf("runs/%s/logs/%s.log", run.ID, phase)
		logs, err = h.storageClient.Get(context.Background(), logsKey)
		if err != nil {
			// TFE returns 200 OK with empty body when logs don't exist
			c.Data(http.StatusOK, "text/plain", []byte(""))
			return
		}

	default:
		// Unknown execution mode, try to get logs from storage anyway
		if h.storageClient != nil {
			logsKey := fmt.Sprintf("runs/%s/logs/%s.log", run.ID, run.Operation)
			logs, err = h.storageClient.Get(context.Background(), logsKey)
			if err != nil {
				// Return 200 OK with empty body when logs don't exist
				c.Data(http.StatusOK, "text/plain", []byte(""))
				return
			}
		} else {
			// No storage client available, return 200 OK with empty body
			c.Data(http.StatusOK, "text/plain", []byte(""))
			return
		}
	}

	// TFE behavior: Return empty when offset is beyond log length (signals end of stream)
	// Note: offset/limit already applied if logs came from Redis or MinIO
	if offset >= len(logs) {
		c.Data(http.StatusOK, "text/plain", []byte(""))
		return
	}

	// Return the logs (offset/limit already applied by Redis or MinIO handler)
	c.Data(http.StatusOK, "text/plain", logs)
}

// GetPlanLogs returns the plan logs for a run (explicit endpoint)
// GET /api/v2/runs/:id/logs/plan
// Always returns plan logs (or empty if not available)
// TFE-compatible: Supports token authentication via query parameter
func (h *RunHandlerV2) GetPlanLogs(c *gin.Context) {
	logger.Infof("[LOGS DEBUG] GetPlanLogs called for run ID: %s", c.Param("id"))
	// TFE-compatible: Support token in query parameter (for Terraform CLI log-read-url)
	tokenFromQuery := c.Query("token")
	if tokenFromQuery != "" {
		user, err := h.authService.GetUserFromToken(tokenFromQuery)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{
				"errors": []gin.H{
					{
						"status": "401",
						"title":  "Unauthorized",
						"detail": "Invalid token",
					},
				},
			})
			return
		}
		c.Set("user_id", user.ID)
	}

	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid run ID",
				},
			},
		})
		return
	}

	run, err := h.runRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Run not found",
				},
			},
		})
		return
	}

	// Get workspace to check execution mode
	workspace, err := h.workspaceRepo.GetByID(run.WorkspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to get workspace",
				},
			},
		})
		return
	}

	// Get offset and limit from query parameters
	offset := 0
	if offsetStr := c.Query("offset"); offsetStr != "" {
		if parsedOffset, err := strconv.Atoi(offsetStr); err == nil && parsedOffset >= 0 {
			offset = parsedOffset
		}
	}
	limit := 0 // 0 means no limit
	if limitStr := c.Query("limit"); limitStr != "" {
		if parsedLimit, err := strconv.Atoi(limitStr); err == nil && parsedLimit > 0 {
			limit = parsedLimit
		}
	}

	// Always use "plan" phase for this endpoint
	phase := "plan"

	// Handle different execution modes
	var logs []byte
	switch workspace.ExecutionMode {
	case "remote":
		// Remote execution: check Redis first (for active runs), then fall back to MinIO
		var logsStr string

		// Try Redis first if log buffer service is available
		if h.logBufferService != nil {
			ctx := context.Background()
			logsStr, err = h.logBufferService.Get(ctx, run.ID, phase, offset, limit)
			if err == nil && logsStr != "" {
				// Found logs in Redis, convert to bytes
				logs = []byte(logsStr)
			} else if err != nil {
				// Error accessing Redis, log but continue to MinIO fallback
				logger.Infof("[LOGS] Error getting logs from Redis for run %s phase %s: %v", run.ID, phase, err)
			}
		}

		// Fall back to MinIO if Redis didn't have logs
		if len(logs) == 0 {
			if h.storageClient == nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"errors": []gin.H{
						{
							"status": "503",
							"title":  "Service Unavailable",
							"detail": "Storage client not initialized",
						},
					},
				})
				return
			}

			// TFE-compatible log path: runs/{run_id}/logs/plan.log
			logsKey := fmt.Sprintf("runs/%s/logs/plan.log", run.ID)
			logs, err = h.storageClient.Get(context.Background(), logsKey)
			if err != nil {
				// TFE returns 200 OK with empty body when logs don't exist
				logger.Infof("[LOGS] Plan logs not found for run %s: %v", run.ID, err)
				c.Data(http.StatusOK, "text/plain", []byte(""))
				return
			}

			// Apply offset/limit to MinIO logs (if they weren't applied by Redis)
			if len(logs) > 0 && (offset > 0 || limit > 0) {
				if offset >= len(logs) {
					logs = []byte("")
				} else {
					end := offset + limit
					if limit <= 0 || end > len(logs) {
						end = len(logs)
					}
					logs = logs[offset:end]
				}
			}
		}

	case "local":
		// Local execution: logs are generated on user's machine
		c.Data(http.StatusOK, "text/plain", []byte(""))
		return

	case "agent":
		// Agent execution: logs sent by agent to backend, stored in MinIO
		if h.storageClient == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"errors": []gin.H{
					{
						"status": "503",
						"title":  "Service Unavailable",
						"detail": "Storage client not initialized",
					},
				},
			})
			return
		}

		logsKey := fmt.Sprintf("runs/%s/logs/plan.log", run.ID)
		logs, err = h.storageClient.Get(context.Background(), logsKey)
		if err != nil {
			c.Data(http.StatusOK, "text/plain", []byte(""))
			return
		}

	default:
		// Unknown execution mode, try to get logs from storage anyway
		if h.storageClient != nil {
			logsKey := fmt.Sprintf("runs/%s/logs/plan.log", run.ID)
			logs, err = h.storageClient.Get(context.Background(), logsKey)
			if err != nil {
				c.Data(http.StatusOK, "text/plain", []byte(""))
				return
			}
		} else {
			c.Data(http.StatusOK, "text/plain", []byte(""))
			return
		}
	}

	// TFE behavior: Return empty when offset is beyond log length
	if offset >= len(logs) {
		c.Data(http.StatusOK, "text/plain", []byte(""))
		return
	}

	// Return the logs
	c.Data(http.StatusOK, "text/plain", logs)
}

// GetApplyLogs returns the apply logs for a run (explicit endpoint)
// GET /api/v2/runs/:id/logs/apply
// Always returns apply logs (or empty if not available)
// TFE-compatible: Supports token authentication via query parameter
func (h *RunHandlerV2) GetApplyLogs(c *gin.Context) {
	logger.Infof("[LOGS DEBUG] GetApplyLogs called for run ID: %s", c.Param("id"))
	// TFE-compatible: Support token in query parameter (for Terraform CLI log-read-url)
	tokenFromQuery := c.Query("token")
	if tokenFromQuery != "" {
		user, err := h.authService.GetUserFromToken(tokenFromQuery)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{
				"errors": []gin.H{
					{
						"status": "401",
						"title":  "Unauthorized",
						"detail": "Invalid token",
					},
				},
			})
			return
		}
		c.Set("user_id", user.ID)
	}

	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid run ID",
				},
			},
		})
		return
	}

	run, err := h.runRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Run not found",
				},
			},
		})
		return
	}

	// Only plan-and-apply runs have apply logs
	if run.Operation != models.RunOperationPlanAndApply {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Apply logs only available for plan-and-apply runs",
				},
			},
		})
		return
	}

	// Get workspace to check execution mode
	workspace, err := h.workspaceRepo.GetByID(run.WorkspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to get workspace",
				},
			},
		})
		return
	}

	// Get offset and limit from query parameters
	offset := 0
	if offsetStr := c.Query("offset"); offsetStr != "" {
		if parsedOffset, err := strconv.Atoi(offsetStr); err == nil && parsedOffset >= 0 {
			offset = parsedOffset
		}
	}
	limit := 0 // 0 means no limit
	if limitStr := c.Query("limit"); limitStr != "" {
		if parsedLimit, err := strconv.Atoi(limitStr); err == nil && parsedLimit > 0 {
			limit = parsedLimit
		}
	}

	// Always use "apply" phase for this endpoint
	phase := "apply"

	// Handle different execution modes
	var logs []byte
	switch workspace.ExecutionMode {
	case "remote":
		// Remote execution: check Redis first (for active runs), then fall back to MinIO
		var logsStr string

		// Try Redis first if log buffer service is available
		if h.logBufferService != nil {
			ctx := context.Background()
			logsStr, err = h.logBufferService.Get(ctx, run.ID, phase, offset, limit)
			if err == nil && logsStr != "" {
				// Found logs in Redis, convert to bytes
				logs = []byte(logsStr)
			} else if err != nil {
				// Error accessing Redis, log but continue to MinIO fallback
				logger.Infof("[LOGS] Error getting logs from Redis for run %s phase %s: %v", run.ID, phase, err)
			}
		}

		// Fall back to MinIO if Redis didn't have logs
		if len(logs) == 0 {
			if h.storageClient == nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"errors": []gin.H{
						{
							"status": "503",
							"title":  "Service Unavailable",
							"detail": "Storage client not initialized",
						},
					},
				})
				return
			}

			// TFE-compatible log path: runs/{run_id}/logs/apply.log
			// NOTE: No fallback to plan logs - this endpoint only returns apply logs
			logsKey := fmt.Sprintf("runs/%s/logs/apply.log", run.ID)
			logs, err = h.storageClient.Get(context.Background(), logsKey)
			if err != nil {
				// TFE returns 200 OK with empty body when logs don't exist
				// This is correct behavior - apply logs simply don't exist yet or were cancelled
				logger.Infof("[LOGS] Apply logs not found for run %s: %v", run.ID, err)
				c.Data(http.StatusOK, "text/plain", []byte(""))
				return
			}

			// Apply offset/limit to MinIO logs (if they weren't applied by Redis)
			if len(logs) > 0 && (offset > 0 || limit > 0) {
				if offset >= len(logs) {
					logs = []byte("")
				} else {
					end := offset + limit
					if limit <= 0 || end > len(logs) {
						end = len(logs)
					}
					logs = logs[offset:end]
				}
			}
		}

	case "local":
		// Local execution: logs are generated on user's machine
		c.Data(http.StatusOK, "text/plain", []byte(""))
		return

	case "agent":
		// Agent execution: logs sent by agent to backend, stored in MinIO
		if h.storageClient == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"errors": []gin.H{
					{
						"status": "503",
						"title":  "Service Unavailable",
						"detail": "Storage client not initialized",
					},
				},
			})
			return
		}

		logsKey := fmt.Sprintf("runs/%s/logs/apply.log", run.ID)
		logs, err = h.storageClient.Get(context.Background(), logsKey)
		if err != nil {
			c.Data(http.StatusOK, "text/plain", []byte(""))
			return
		}

	default:
		// Unknown execution mode, try to get logs from storage anyway
		if h.storageClient != nil {
			logsKey := fmt.Sprintf("runs/%s/logs/apply.log", run.ID)
			logs, err = h.storageClient.Get(context.Background(), logsKey)
			if err != nil {
				c.Data(http.StatusOK, "text/plain", []byte(""))
				return
			}
		} else {
			c.Data(http.StatusOK, "text/plain", []byte(""))
			return
		}
	}

	// TFE behavior: Return empty when offset is beyond log length
	if offset >= len(logs) {
		c.Data(http.StatusOK, "text/plain", []byte(""))
		return
	}

	// Return the logs
	c.Data(http.StatusOK, "text/plain", logs)
}

// Apply applies a run (TFE-compatible)
// POST /api/v2/runs/:id/actions/apply
// TFE-compatible: Transitions a plan-and-apply run from "planned" to "applying" status
func (h *RunHandlerV2) Apply(c *gin.Context) {
	// Authenticate user (required for applying runs)
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{
					"status": "401",
					"title":  "Unauthorized",
					"detail": "Authentication required",
				},
			},
		})
		return
	}

	planRunID := c.Param("id")
	if planRunID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid run ID",
				},
			},
		})
		return
	}

	// Get the plan run
	planRun, err := h.runRepo.GetByID(planRunID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Run not found",
				},
			},
		})
		return
	}

	// Get workspace to check permissions
	workspace, err := h.workspaceRepo.GetByID(planRun.WorkspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve workspace",
				},
			},
		})
		return
	}

	// CRITICAL: Check if user has permission to apply runs
	// Viewers should NOT be able to apply runs - they only have RunRead permission
	// Applying requires both PermissionRuns (granular) AND PermissionWorkspaceWrite
	hasRunsPermission, err := h.rbacService.CheckResourcePermission(
		c.Request.Context(),
		user.ID,
		rbac.ResourceTypeTerraformWorkspace,
		workspace.ID,
		rbac.PermissionRuns,
		&workspace.ProjectID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to check permissions",
				},
			},
		})
		return
	}
	hasWorkspaceWrite, err := h.rbacService.CheckResourcePermission(
		c.Request.Context(),
		user.ID,
		rbac.ResourceTypeTerraformWorkspace,
		workspace.ID,
		rbac.PermissionWorkspaceWrite,
		&workspace.ProjectID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to check permissions",
				},
			},
		})
		return
	}
	// Both permissions required - viewers have neither
	if !hasRunsPermission || !hasWorkspaceWrite {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "You do not have permission to apply runs in this workspace. Viewers can only view runs, not apply them.",
				},
			},
		})
		return
	}

	// TFE-compatible: For plan-and-apply and destroy runs, transition from "planned" to "applying" status
	// Destroy runs follow the same two-phase flow: plan -destroy → confirm → apply
	if planRun.Operation == models.RunOperationPlanAndApply || planRun.Operation == models.RunOperationDestroy {
		// Plan-and-apply or destroy run: transition to applying phase
		if planRun.Status != models.RunStatusPlanned {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "Run must be in 'planned' status before applying",
					},
				},
			})
			return
		}

		// Transition run to applying phase
		now := time.Now()
		planRun.Status = models.RunStatusApplying
		planRun.ApplyStartedAt = &now // Track when apply phase started
		planRun.UpdatedAt = now
		if err := h.runRepo.Update(planRun); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{
					{
						"status": "500",
						"title":  "Internal Server Error",
						"detail": "Failed to transition run to applying phase",
					},
				},
			})
			return
		}

		// The orchestrator will automatically pick up runs in "applying" status and enqueue them
		// No manual enqueue needed - orchestrator polls for both "pending" and "applying" status runs

		// Reload run to ensure all fields are populated
		planRun, err = h.runRepo.GetByID(planRun.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{
					{
						"status": "500",
						"title":  "Internal Server Error",
						"detail": "Failed to retrieve updated run",
					},
				},
			})
			return
		}

		// TFE returns 202 Accepted with the updated run
		c.JSON(http.StatusAccepted, gin.H{
			"data": formatRunResponse(planRun, c, h.configVersionRepo, h.runRepo),
		})
		return
	}

	// Only plan-and-apply runs can be applied
	c.JSON(http.StatusBadRequest, gin.H{
		"errors": []gin.H{
			{
				"status": "400",
				"title":  "Bad Request",
				"detail": "Only plan-and-apply runs can be applied. Plan-only runs cannot be applied.",
			},
		},
	})
}

// ListByWorkspace lists runs for a workspace (by workspace ID)
// GET /api/v2/workspaces/:id/runs
func (h *RunHandlerV2) ListByWorkspace(c *gin.Context) {
	workspaceID := c.Param("id")
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid workspace ID",
				},
			},
		})
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	perPage, _ := strconv.Atoi(c.DefaultQuery("per_page", "20"))
	if perPage > 100 {
		perPage = 100
	}
	offset := (page - 1) * perPage

	runs, total, err := h.runRepo.ListByWorkspace(workspaceID, perPage, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to list runs",
				},
			},
		})
		return
	}

	// Format each run in TFE-compatible JSON:API format
	formattedRuns := make([]gin.H, len(runs))
	for i, run := range runs {
		formattedRuns[i] = formatRunResponse(&run, c, h.configVersionRepo, h.runRepo)
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formattedRuns,
		"meta": gin.H{
			"pagination": gin.H{
				"page":     page,
				"per_page": perPage,
				"total":    total,
			},
		},
	})
}

// Cancel cancels a run (TFE-compatible)
// POST /api/v2/runs/:id/actions/cancel
func (h *RunHandlerV2) Cancel(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid run ID",
				},
			},
		})
		return
	}

	run, err := h.runRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Run not found",
				},
			},
		})
		return
	}

	// Allow cancellation of pending, running, planning, and applying runs
	// Planning and applying runs can be cancelled to stop long-running operations
	cancellableStatuses := []models.RunStatus{
		models.RunStatusPending,
		models.RunStatusRunning,
		models.RunStatusPlanning,
		models.RunStatusApplying,
	}
	isCancellable := false
	for _, status := range cancellableStatuses {
		if run.Status == status {
			isCancellable = true
			break
		}
	}
	if !isCancellable {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Run cannot be cancelled in current state. Only pending, running, planning, or applying runs can be cancelled.",
				},
			},
		})
		return
	}

	// Update run status to cancelled
	now := time.Now()
	run.Status = models.RunStatusCancelled
	run.CompletedAt = &now

	if err := h.runRepo.Update(run); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to cancel run",
				},
			},
		})
		return
	}

	// TFE returns 202 Accepted for action endpoints (no body)
	// According to TFE API docs: https://developer.hashicorp.com/terraform/enterprise/api-docs/run
	c.Status(http.StatusAccepted)
}

// Discard discards a run (TFE-compatible)
// POST /api/v2/runs/:id/actions/discard
func (h *RunHandlerV2) Discard(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid run ID",
				},
			},
		})
		return
	}

	run, err := h.runRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Run not found",
				},
			},
		})
		return
	}

	// Can discard runs in pending state or planned state (plan completed, waiting for apply confirmation)
	// This allows users to discard unwanted plans before they are applied
	if run.Status != models.RunStatusPending && run.Status != models.RunStatusPlanned {
		c.JSON(http.StatusConflict, gin.H{
			"errors": []gin.H{
				{
					"status": "409",
					"title":  "Conflict",
					"detail": "Run cannot be discarded in current state. Only pending or planned runs can be discarded.",
				},
			},
		})
		return
	}

	// Update run status to cancelled (discarded runs are marked as cancelled)
	now := time.Now()
	run.Status = models.RunStatusCancelled
	run.CompletedAt = &now

	if err := h.runRepo.Update(run); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to discard run",
				},
			},
		})
		return
	}

	// TFE returns 202 Accepted for action endpoints (no body)
	c.Status(http.StatusAccepted)
}

// ForceCancel forcefully cancels a run (TFE-compatible)
// POST /api/v2/runs/:id/actions/force-cancel
func (h *RunHandlerV2) ForceCancel(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid run ID",
				},
			},
		})
		return
	}

	run, err := h.runRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Run not found",
				},
			},
		})
		return
	}

	// Force cancel can only be used on runs that are planning or applying
	// and have been canceled non-forcefully first
	if run.Status != models.RunStatusRunning {
		c.JSON(http.StatusConflict, gin.H{
			"errors": []gin.H{
				{
					"status": "409",
					"title":  "Conflict",
					"detail": "Run cannot be force-cancelled in current state",
				},
			},
		})
		return
	}

	// Force cancel immediately terminates the run
	now := time.Now()
	run.Status = models.RunStatusCancelled
	run.CompletedAt = &now

	if err := h.runRepo.Update(run); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to force-cancel run",
				},
			},
		})
		return
	}

	// TFE returns 202 Accepted for action endpoints (no body)
	c.Status(http.StatusAccepted)
}

// ForceExecute forcefully executes a run (TFE-compatible)
// POST /api/v2/runs/:id/actions/force-execute
func (h *RunHandlerV2) ForceExecute(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid run ID",
				},
			},
		})
		return
	}

	run, err := h.runRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Run not found",
				},
			},
		})
		return
	}

	// Force execute can only be used on pending runs when workspace is locked
	if run.Status != models.RunStatusPending {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Run is not pending",
				},
			},
		})
		return
	}

	// Check if workspace is locked by another run
	workspace, err := h.workspaceRepo.GetByID(run.WorkspaceID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Workspace not found",
				},
			},
		})
		return
	}

	if !workspace.Locked {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Workspace is not locked",
				},
			},
		})
		return
	}

	// TODO: Discard the run that's locking the workspace
	// For now, just unlock the workspace and start the run
	workspace.Locked = false
	workspace.LockedBy = nil
	workspace.LockedAt = nil
	if err := h.workspaceRepo.Update(workspace); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to unlock workspace",
				},
			},
		})
		return
	}

	// Start the run
	now := time.Now()
	run.Status = models.RunStatusRunning
	run.StartedAt = &now
	if err := h.runRepo.Update(run); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to start run",
				},
			},
		})
		return
	}

	// TFE returns 202 Accepted for action endpoints (no body)
	c.Status(http.StatusAccepted)
}

// ListByOrganization lists runs for an organization (TFE-compatible)
// GET /api/v2/organizations/:name/runs
func (h *RunHandlerV2) ListByOrganization(c *gin.Context) {
	orgName := c.Param("name")

	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Organization not found",
				},
			},
		})
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	perPage, _ := strconv.Atoi(c.DefaultQuery("per_page", "20"))
	if perPage > 100 {
		perPage = 100
	}
	offset := (page - 1) * perPage

	runs, total, err := h.runRepo.ListByOrganization(org.ID, perPage, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to list runs",
				},
			},
		})
		return
	}

	// Format each run in TFE-compatible JSON:API format
	formattedRuns := make([]gin.H, len(runs))
	for i, run := range runs {
		formattedRuns[i] = formatRunResponse(&run, c, h.configVersionRepo, h.runRepo)
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formattedRuns,
		"meta": gin.H{
			"pagination": gin.H{
				"page":     page,
				"per_page": perPage,
				"total":    total,
			},
		},
	})
}

// GetQueue returns the run queue for an organization (TFE-compatible)
// GET /api/v2/organizations/:name/runs/queue
func (h *RunHandlerV2) GetQueue(c *gin.Context) {
	orgName := c.Param("name")

	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Organization not found",
				},
			},
		})
		return
	}

	limit := 50 // Default limit for queue
	runs, err := h.runRepo.ListQueued(org.ID, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to get run queue",
				},
			},
		})
		return
	}

	// Format each run in TFE-compatible JSON:API format
	formattedRuns := make([]gin.H, len(runs))
	for i, run := range runs {
		formattedRuns[i] = formatRunResponse(&run, c, h.configVersionRepo, h.runRepo)
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formattedRuns,
	})
}

// createConfigurationVersionFromVCS creates a configuration version by cloning from VCS
// This is used when creating runs from UI without a configuration version
func (h *RunHandlerV2) createConfigurationVersionFromVCS(ctx context.Context, workspace *models.Workspace, vcsConn *models.VCSConnection) (string, error) {
	// Clone repository
	tempDir, err := os.MkdirTemp("", fmt.Sprintf("workspace-%s-*", workspace.ID))
	if err != nil {
		return "", fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			logger.Warnf("Failed to remove temp directory %s: %v", tempDir, err)
		}
	}()

	// Build authenticated clone URL via provider registry (supports GitHub, Azure DevOps, etc.)
	provider, providerErr := h.vcsRegistry.GetProvider(vcsConn)
	if providerErr != nil {
		return "", fmt.Errorf("failed to get VCS provider for connection %s: %w", vcsConn.ID, providerErr)
	}
	token, tokenErr := provider.GetFreshToken(ctx, vcsConn)
	if tokenErr != nil {
		return "", fmt.Errorf("failed to get fresh token for VCS connection %s: %w", vcsConn.ID, tokenErr)
	}
	cloneURL := provider.BuildCloneURL(vcsConn, token, workspace.VCSRepository)

	// Clone repository at branch
	branch := workspace.VCSBranch
	if branch == "" {
		branch = "main"
	}
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--branch", branch, cloneURL, tempDir) //nolint:gosec // intentional: executing git command
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to clone repository: %w", err)
	}

	// Create tarball from repository root to preserve full structure
	// This ensures relative module paths (e.g., ../module) work correctly
	// The runner will handle the working directory path within the extracted structure
	tarballPath := filepath.Join(os.TempDir(), fmt.Sprintf("workspace-%s-%s.tar.gz", workspace.ID, uuid.New().String()[:8]))
	defer func() {
		if err := os.Remove(tarballPath); err != nil {
			logger.Warnf("Failed to remove tarball %s: %v", tarballPath, err)
		}
	}()

	// Always create tarball from repository root to preserve directory structure
	// This allows relative module paths to work correctly
	if err := h.createTarball(tempDir, tarballPath); err != nil {
		return "", fmt.Errorf("failed to create tarball: %w", err)
	}

	// Create configuration version
	configVersion := &models.ConfigurationVersion{
		WorkspaceID:   workspace.ID,
		Status:        models.ConfigurationVersionStatusPending,
		Source:        "tfe-ui", // Mark as UI-triggered
		AutoQueueRuns: false,
		Speculative:   false,
	}

	if err := h.configVersionRepo.Create(configVersion); err != nil {
		return "", fmt.Errorf("failed to create configuration version: %w", err)
	}

	// Upload tarball to MinIO
	tarballFile, err := os.Open(tarballPath) //nolint:gosec // tarballPath is validated (in temp directory)
	if err != nil {
		return "", fmt.Errorf("failed to open tarball: %w", err)
	}
	defer func() {
		if err := tarballFile.Close(); err != nil {
			logger.Warnf("Failed to close tarball file: %v", err)
		}
	}()

	storageKey := fmt.Sprintf("configuration-versions/%s/config.tar.gz", configVersion.ID)
	if err := h.storageClient.PutStream(ctx, storageKey, tarballFile); err != nil {
		return "", fmt.Errorf("failed to upload configuration: %w", err)
	}

	// Update configuration version status to uploaded
	configVersion.Status = models.ConfigurationVersionStatusUploaded
	if err := h.configVersionRepo.Update(configVersion); err != nil {
		return "", fmt.Errorf("failed to update configuration version status: %w", err)
	}

	return configVersion.ID, nil
}

// createTarball creates a gzipped tarball from a directory
func (h *RunHandlerV2) createTarball(sourceDir, outputPath string) error {
	file, err := os.Create(outputPath) //nolint:gosec // outputPath is validated (in temp directory)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); err != nil {
			logger.Warnf("Failed to close file: %v", err)
		}
	}()

	gzipWriter := gzip.NewWriter(file)
	defer func() {
		if err := gzipWriter.Close(); err != nil {
			logger.Warnf("Failed to close gzip writer: %v", err)
		}
	}()

	tarWriter := tar.NewWriter(gzipWriter)
	defer func() {
		if err := tarWriter.Close(); err != nil {
			logger.Warnf("Failed to close tar writer: %v", err)
		}
	}()

	return filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip hidden files and directories
		if strings.HasPrefix(info.Name(), ".") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip .git, .terraform, *.tfstate, .terraform.lock.hcl
		if info.IsDir() && (info.Name() == ".git" || info.Name() == ".terraform") {
			return filepath.SkipDir
		}
		if !info.IsDir() && (strings.HasSuffix(path, ".tfstate") || strings.HasSuffix(path, ".terraform.lock.hcl")) {
			return nil
		}

		// Get relative path
		relPath, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}

		// Create tar header
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = relPath

		// Write header
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}

		// Write file content if not a directory
		if !info.IsDir() {
			data, err := os.Open(path) //nolint:gosec // path is from filepath.Walk, validated
			if err != nil {
				return err
			}
			defer func() {
				if err := data.Close(); err != nil {
					logger.Warnf("Failed to close data file: %v", err)
				}
			}()

			if _, err := io.Copy(tarWriter, data); err != nil {
				return err
			}
		}

		return nil
	})
}

// AutoCancelConflictingRuns cancels previous runs that conflict with a new run being created
// This is a centralized function that can be used by both UI-triggered and VCS-triggered run creation
// TFE-compatible: Auto-cancel previous pending/running/planned runs based on operation type
// Cancellation behavior:
//   - Plan-only runs: Cancel other plan-only runs (pending/running/planned) for better UX - users don't want to wait for old plan-only runs
//     Plan-only runs CANNOT alter state, so they can run completely independently from plan-and-apply runs
//     Multiple plan-only runs can run in parallel, but cancelling old ones when starting new ones improves UX
//   - Plan-and-apply runs: Cancel other plan-and-apply runs (pending/running/planned) to prevent state corruption
//     Plan-and-apply runs WILL alter state, so only one should run at a time
//     Plan-and-apply runs do NOT cancel plan-only runs (they're separate operations)
//   - Destroy runs: Cancel ALL other runs (plan-only, plan-and-apply, and other destroy runs) to prevent state conflicts
//     Destroy runs will remove infrastructure, so any other run that reads or modifies state should be cancelled
//     This prevents confusion and ensures destroy operations have exclusive access to the workspace state
//
// This ensures state integrity while allowing plan-only runs to run independently from plan-and-apply runs
func AutoCancelConflictingRuns(runRepo *repository.RunRepository, workspaceID string, operation models.RunOperation) {
	existingRuns, _, err := runRepo.ListByWorkspace(workspaceID, 100, 0)
	if err != nil {
		logger.Warnf("Failed to list existing runs for auto-cancel: %v", err)
		return
	}

	for _, existingRun := range existingRuns {
		// Skip if run is not in a cancellable state
		if existingRun.Status != models.RunStatusPending &&
			existingRun.Status != models.RunStatusRunning &&
			existingRun.Status != models.RunStatusPlanned {
			continue
		}

		// Determine if this existing run should be cancelled based on the new run's operation
		shouldCancel := false
		switch operation {
		case models.RunOperationPlanOnly:
			// Plan-only runs: Only cancel other plan-only runs (for UX - don't wait for old plan-only runs)
			// Do NOT cancel plan-and-apply runs - they're completely separate operations
			if existingRun.Operation == models.RunOperationPlanOnly {
				shouldCancel = true
				logger.Infof("Auto-cancelling previous plan-only run %s for workspace %s (new plan-only run being created)", existingRun.ID, workspaceID)
			}
		case models.RunOperationPlanAndApply:
			// Plan-and-apply runs: Only cancel other plan-and-apply runs (to prevent state corruption)
			// Do NOT cancel plan-only runs - they're separate operations that cannot alter state
			if existingRun.Operation == models.RunOperationPlanAndApply {
				shouldCancel = true
				logger.Infof("Auto-cancelling previous plan-and-apply run %s for workspace %s (new plan-and-apply run being created)", existingRun.ID, workspaceID)
			}
		case models.RunOperationDestroy:
			// Destroy runs: Cancel ALL other runs (plan-only, plan-and-apply, and other destroy runs)
			// Destroy operations will remove infrastructure, so any other run that reads or modifies state should be cancelled
			// This prevents state conflicts and ensures destroy operations have exclusive access to the workspace
			shouldCancel = true
			logger.Infof("Auto-cancelling run %s (operation: %s) for workspace %s (new destroy run being created)", existingRun.ID, existingRun.Operation, workspaceID)
		}

		if shouldCancel {
			now := time.Now()
			existingRun.Status = models.RunStatusCancelled
			existingRun.CompletedAt = &now
			if err := runRepo.Update(&existingRun); err != nil {
				logger.Warnf("Failed to cancel previous plan run %s: %v", existingRun.ID, err)
			}
		}
	}
}
