// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package terraform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/api/helpers"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/activity"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/rbac"
	"github.com/iac-platform/backend/internal/services/vcs"
	"github.com/michielvha/logger"
	"gorm.io/gorm"
)

type WorkspaceHandlerV2 struct {
	workspaceRepo     *repository.WorkspaceRepository
	projectRepo       *repository.ProjectRepository
	orgRepo           *repository.OrganizationRepository
	vcsConnectionRepo *repository.VCSConnectionRepository
	teamRepo          *repository.TeamRepository
	authService       *auth.Service
	activityService   *activity.Service
	rbacService       *rbac.Service
	vcsRegistry       *vcs.ProviderRegistry
	db                *gorm.DB
}

func NewWorkspaceHandlerV2(
	workspaceRepo *repository.WorkspaceRepository,
	projectRepo *repository.ProjectRepository,
	orgRepo *repository.OrganizationRepository,
	vcsConnectionRepo *repository.VCSConnectionRepository,
	teamRepo *repository.TeamRepository,
	authService *auth.Service,
	activityService *activity.Service,
	rbacService *rbac.Service,
	vcsRegistry *vcs.ProviderRegistry,
	db *gorm.DB,
) *WorkspaceHandlerV2 {
	return &WorkspaceHandlerV2{
		workspaceRepo:     workspaceRepo,
		projectRepo:       projectRepo,
		orgRepo:           orgRepo,
		vcsConnectionRepo: vcsConnectionRepo,
		teamRepo:          teamRepo,
		authService:       authService,
		activityService:   activityService,
		rbacService:       rbacService,
		vcsRegistry:       vcsRegistry,
		db:                db,
	}
}

// maybeRegisterADOWebhook fires a background goroutine to register per-repository Service Hook
// subscriptions in Azure DevOps when a workspace is linked to an ADO repo.
// It is a no-op when the connection is not ADO, when STACKWEAVER_WEBHOOK_BASE_URL is unset,
// or when vcsRegistry is not configured.
func (h *WorkspaceHandlerV2) maybeRegisterADOWebhook(connID *uuid.UUID, repoPath string) {
	if connID == nil || repoPath == "" || h.vcsRegistry == nil {
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
			logger.Warnf("Failed to register ADO webhooks for repo %s/%s: %v", projectName, repoName, rErr)
		}
	}(*connID, parts[0], parts[1])
}

// VCSRepoRequest represents the TFE-compatible vcs-repo nested attribute
type VCSRepoRequest struct {
	Identifier        string `json:"identifier,omitempty"`
	Branch            string `json:"branch,omitempty"`
	OAuthTokenID      string `json:"oauth-token-id,omitempty"` //nolint:gosec // G117: reference ID, not a secret
	GHAInstallationID string `json:"github-app-installation-id,omitempty"`
	IngressSubmodules bool   `json:"ingress-submodules,omitempty"`
	TagsRegex         string `json:"tags-regex,omitempty"`
}

// CreateWorkspaceRequestV2 uses JSON:API format (TFE-compatible)
type CreateWorkspaceRequestV2 struct {
	Data struct {
		Type       string `json:"type"`
		Attributes struct {
			Name                       string          `json:"name"`
			Description                string          `json:"description"`
			ProjectID                  *uuid.UUID      `json:"project-id,omitempty"`
			VCSConnectionID            *uuid.UUID      `json:"vcs-connection-id,omitempty"`
			VCSRepository              string          `json:"vcs-repository,omitempty"`
			VCSBranch                  string          `json:"vcs-branch,omitempty"`
			VCSRepo                    *VCSRepoRequest `json:"vcs-repo,omitempty"`
			WorkingDirectory           string          `json:"working-directory,omitempty"`
			TerraformVersion           string          `json:"terraform-version,omitempty"`
			AutoQueueRuns              *bool           `json:"auto-queue-runs,omitempty"`
			AutoApply                  *bool           `json:"auto-apply,omitempty"`
			AutoApplyRunTrigger        *bool           `json:"auto-apply-run-trigger,omitempty"`
			AllowDestroyPlan           *bool           `json:"allow-destroy-plan,omitempty"`
			ExecutionMode              string          `json:"execution-mode,omitempty"`
			AgentPoolID                *string         `json:"agent-pool-id,omitempty"`
			QueueAllRuns               *bool           `json:"queue-all-runs,omitempty"`
			SpeculativeEnabled         *bool           `json:"speculative-enabled,omitempty"`
			FileTriggersEnabled        *bool           `json:"file-triggers-enabled,omitempty"`
			TriggerPrefixes            []string        `json:"trigger-prefixes,omitempty"`
			TriggerPatterns            []string        `json:"trigger-patterns,omitempty"`
			GlobalRemoteState          *bool           `json:"global-remote-state,omitempty"`
			StructuredRunOutputEnabled *bool           `json:"structured-run-output-enabled,omitempty"`
			AssessmentsEnabled         *bool           `json:"assessments-enabled,omitempty"`
			SourceName                 string          `json:"source-name,omitempty"`
			SourceURL                  string          `json:"source-url,omitempty"`
			TagNames                   []string        `json:"tag-names,omitempty"`
			RunTimeout                 *int            `json:"run-timeout,omitempty"`
			ForceDelete                *bool           `json:"force-delete,omitempty"`
		} `json:"attributes"`
		Relationships struct {
			Project *struct {
				Data struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"project,omitempty"`
		} `json:"relationships"`
	} `json:"data"`
}

// UpdateWorkspaceRequestV2 uses JSON:API format (TFE-compatible)
type UpdateWorkspaceRequestV2 struct {
	Data struct {
		Type       string `json:"type"`
		Attributes struct {
			Name                       string          `json:"name,omitempty"`
			Description                string          `json:"description,omitempty"`
			VCSConnectionID            *uuid.UUID      `json:"vcs-connection-id,omitempty"`
			VCSProvider                string          `json:"vcs-provider,omitempty"`
			VCSRepository              string          `json:"vcs-repository,omitempty"`
			VCSBranch                  string          `json:"vcs-branch,omitempty"`
			VCSRepo                    *VCSRepoRequest `json:"vcs-repo,omitempty"`
			TerraformVersion           string          `json:"terraform-version,omitempty"`
			WorkingDirectory           string          `json:"working-directory,omitempty"`
			AutoQueueRuns              *bool           `json:"auto-queue-runs,omitempty"`
			AutoApply                  *bool           `json:"auto-apply,omitempty"`
			AutoApplyRunTrigger        *bool           `json:"auto-apply-run-trigger,omitempty"`
			AllowDestroyPlan           *bool           `json:"allow-destroy-plan,omitempty"`
			ExecutionMode              string          `json:"execution-mode,omitempty"`
			AgentPoolID                *string         `json:"agent-pool-id,omitempty"`
			QueueAllRuns               *bool           `json:"queue-all-runs,omitempty"`
			SpeculativeEnabled         *bool           `json:"speculative-enabled,omitempty"`
			FileTriggersEnabled        *bool           `json:"file-triggers-enabled,omitempty"`
			TriggerPrefixes            []string        `json:"trigger-prefixes,omitempty"`
			TriggerPatterns            []string        `json:"trigger-patterns,omitempty"`
			GlobalRemoteState          *bool           `json:"global-remote-state,omitempty"`
			StructuredRunOutputEnabled *bool           `json:"structured-run-output-enabled,omitempty"`
			AssessmentsEnabled         *bool           `json:"assessments-enabled,omitempty"`
			TagNames                   []string        `json:"tag-names,omitempty"`
			RunTimeout                 *int            `json:"run-timeout,omitempty"`
			ForceDelete                *bool           `json:"force-delete,omitempty"`
		} `json:"attributes"`
	} `json:"data"`
}

// ListByOrganization lists workspaces by organization name (TFE-compatible)
// GET /api/v2/organizations/:name/workspaces
func (h *WorkspaceHandlerV2) ListByOrganization(c *gin.Context) {
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

	// Get user for permission checking
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

	// Check if user has organization-level read-workspaces permission
	hasOrgReadWorkspaces, err := h.rbacService.CheckOrgReadWorkspaces(c.Request.Context(), user.ID, org.ID)
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

	// Support TFE-style pagination: page[size] and page[number]
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page[size]", "20"))
	pageNumber, _ := strconv.Atoi(c.DefaultQuery("page[number]", "1"))
	if pageSize > 100 {
		pageSize = 100
	}
	offset := (pageNumber - 1) * pageSize

	var workspaces []models.Workspace
	var total int64

	if hasOrgReadWorkspaces {
		// User has organization-level read-workspaces permission - show all workspaces
		workspaces, total, err = h.workspaceRepo.ListByOrganization(orgName, pageSize, offset)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{
					{
						"status": "500",
						"title":  "Internal Server Error",
						"detail": "Failed to list workspaces",
					},
				},
			})
			return
		}
	} else {
		// User does NOT have organization-level read-workspaces permission
		// Filter workspaces to only those the user has team workspace access to
		// Get all teams user is member of
		teams, err := h.teamRepo.GetTeamsByUserID(user.ID, org.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{
					{
						"status": "500",
						"title":  "Internal Server Error",
						"detail": "Failed to get user teams",
					},
				},
			})
			return
		}

		// Collect all workspace IDs the user's teams have access to
		accessibleWorkspaceIDs := make(map[string]bool)
		for _, team := range teams {
			// Get team with workspace access preloaded
			teamWithAccess, err := h.teamRepo.GetByID(team.ID)
			if err != nil {
				// Log error but continue with other teams
				continue
			}
			// Collect workspace IDs from team's workspace access
			for _, access := range teamWithAccess.WorkspaceAccess {
				accessibleWorkspaceIDs[access.WorkspaceID] = true
			}
		}

		// If user has no team workspace access, return empty list
		if len(accessibleWorkspaceIDs) == 0 {
			workspaces = []models.Workspace{}
			total = 0
		} else {
			// Convert map keys to slice for SQL IN clause
			workspaceIDList := make([]string, 0, len(accessibleWorkspaceIDs))
			for id := range accessibleWorkspaceIDs {
				workspaceIDList = append(workspaceIDList, id)
			}

			// Query workspaces that are in the accessible list
			workspaces, total, err = h.workspaceRepo.ListByOrganizationAndIDs(orgName, workspaceIDList, pageSize, offset)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{
					"errors": []gin.H{
						{
							"status": "500",
							"title":  "Internal Server Error",
							"detail": "Failed to list workspaces",
						},
					},
				})
				return
			}
		}
	}

	// Check if frontend wants simple format
	if c.Query("format") == "simple" {
		workspacesData := make([]gin.H, len(workspaces))
		for i := range workspaces {
			workspacesData[i] = formatWorkspaceSimple(&workspaces[i])
		}
		c.JSON(http.StatusOK, gin.H{
			"data": workspacesData,
			"meta": gin.H{
				"pagination": gin.H{
					"page":     pageNumber,
					"per_page": pageSize,
					"total":    total,
				},
			},
		})
		return
	}

	// Default: Format workspaces in TFE-compatible JSON:API format
	workspacesData := make([]gin.H, len(workspaces))
	for i := range workspaces {
		workspacesData[i] = formatWorkspaceResponse(&workspaces[i], h.vcsConnectionRepo)
	}

	// TFE-compatible response format
	c.JSON(http.StatusOK, gin.H{
		"data": workspacesData,
		"meta": gin.H{
			"pagination": gin.H{
				"page":     pageNumber,
				"per_page": pageSize,
				"total":    total,
			},
		},
	})
}

// formatWorkspaceResponse formats a workspace model into TFE-compatible JSON:API format
// Based on: https://developer.hashicorp.com/terraform/enterprise/api-docs/workspaces
func formatWorkspaceResponse(workspace *models.Workspace, vcsConnRepo ...*repository.VCSConnectionRepository) gin.H {
	attributes := gin.H{
		"name":                workspace.Name,
		"terraform-version":   workspace.TerraformVersion,
		"working-directory":   workspace.WorkingDirectory,
		"auto-apply":          workspace.AutoApply,
		"auto-queue-runs":     workspace.AutoQueueRuns,
		"queue-all-runs":      workspace.QueueAllRuns,
		"speculative-enabled": workspace.SpeculativeEnabled,
		"allow-destroy-plan":  workspace.AllowDestroyPlan,
		"execution-mode":      workspace.ExecutionMode,
		"agent-pool-id":       workspace.AgentPoolID,
		"locked":              workspace.Locked,
		"created-at":          workspace.CreatedAt.Format("2006-01-02T15:04:05Z"),
		"updated-at":          workspace.UpdatedAt.Format("2006-01-02T15:04:05Z"),
	}

	// go-tfe Description is a plain string, not pointer — always include
	attributes["description"] = workspace.Description

	// TFE API: vcs-repo must be an object matching go-tfe VCSRepo struct exactly
	// All fields must be present (go-tfe uses plain types, not pointers)
	if workspace.VCSRepository != "" {
		branch := workspace.VCSBranch
		if branch == "" {
			branch = "main"
		}
		vcsRepo := gin.H{
			"identifier":          workspace.VCSRepository,
			"display-identifier":  workspace.VCSRepository,
			"branch":              branch,
			"ingress-submodules":  workspace.VCSIngressSubmodules,
			"service-provider":    "github",
			"tags-regex":          workspace.VCSTagsRegex, // Always include (go-tfe is plain string)
			"repository-http-url": "",
			"webhook-url":         "",
			"tags":                false,
		}
		// Include the VCS connection reference as github-app-installation-id or oauth-token-id,
		// and set service-provider to the correct TFE value based on the provider type.
		ghAppInstallID := ""
		oauthTokenID := ""
		if workspace.VCSConnectionID != nil {
			if len(vcsConnRepo) > 0 && vcsConnRepo[0] != nil {
				vcsConn, err := vcsConnRepo[0].GetByID(*workspace.VCSConnectionID)
				if err == nil {
					if vcsConn.InstallationID != "" {
						ghAppInstallID = vcsConn.InstallationID
					}
					// Map provider to TFE service-provider value
					switch vcsConn.Provider {
					case models.VCSProviderGitHub:
						vcsRepo["service-provider"] = "github"
					case models.VCSProviderAzureDevOps:
						vcsRepo["service-provider"] = "ado_services"
					case models.VCSProviderGitLab:
						vcsRepo["service-provider"] = "gitlab_hosted"
					case models.VCSProviderBitbucket:
						vcsRepo["service-provider"] = "bitbucket_hosted"
					default:
						vcsRepo["service-provider"] = "github"
					}
				}
			}
			// If no GitHub App, fall back to treating VCS connection ID as OAuth token
			if ghAppInstallID == "" {
				oauthTokenID = workspace.VCSConnectionID.String()
			}
		}
		vcsRepo["github-app-installation-id"] = ghAppInstallID
		vcsRepo["oauth-token-id"] = oauthTokenID
		attributes["vcs-repo"] = vcsRepo
	} else {
		attributes["vcs-repo"] = nil
	}

	// TFE API: Additional required/optional fields
	attributes["actions"] = gin.H{
		"is-destroyable": true,
	}
	attributes["auto-apply-run-trigger"] = workspace.AutoApplyRunTrigger
	attributes["assessments-enabled"] = workspace.AssessmentsEnabled
	attributes["force-delete"] = workspace.ForceDelete
	attributes["environment"] = "default" // TFE default
	attributes["file-triggers-enabled"] = workspace.FileTriggersEnabled
	attributes["global-remote-state"] = workspace.GlobalRemoteState
	attributes["resource-count"] = 0
	if workspace.SourceName != "" {
		attributes["source-name"] = workspace.SourceName
	}
	if workspace.SourceURL != "" {
		attributes["source-url"] = workspace.SourceURL
	}
	attributes["source"] = "tfe-api"
	attributes["structured-run-output-enabled"] = workspace.StructuredRunOutputEnabled

	// Parse trigger-prefixes from JSON or return empty array
	var triggerPrefixes []string
	if workspace.TriggerPrefixes != "" {
		_ = json.Unmarshal([]byte(workspace.TriggerPrefixes), &triggerPrefixes)
	}
	if triggerPrefixes == nil {
		triggerPrefixes = []string{}
	}
	attributes["trigger-prefixes"] = triggerPrefixes

	// Parse trigger-patterns from JSON or return empty array
	var triggerPatterns []string
	if workspace.TriggerPatterns != "" {
		_ = json.Unmarshal([]byte(workspace.TriggerPatterns), &triggerPatterns)
	}
	if triggerPatterns == nil {
		triggerPatterns = []string{}
	}
	attributes["trigger-patterns"] = triggerPatterns

	// Parse tag-names from JSON or return empty array
	var tagNames []string
	if workspace.TagNames != "" {
		_ = json.Unmarshal([]byte(workspace.TagNames), &tagNames)
	}
	if tagNames == nil {
		tagNames = []string{}
	}
	attributes["tag-names"] = tagNames

	attributes["latest-change-at"] = workspace.UpdatedAt.Format("2006-01-02T15:04:05Z")
	// TFE API: locked-reason is a string or null
	if workspace.LockedReason != "" {
		attributes["locked-reason"] = workspace.LockedReason
	} else {
		attributes["locked-reason"] = nil
	}
	attributes["operations"] = true // Indicates workspace is operational
	attributes["permissions"] = gin.H{
		"can-update":          true,
		"can-destroy":         true,
		"can-queue-destroy":   true,
		"can-queue-run":       true,
		"can-update-variable": true,
		"can-lock":            true,
		"can-unlock":          true,
		"can-force-unlock":    true,
		"can-read-settings":   true,
	}

	// Custom extension: run-timeout (TFE clients will ignore unknown attributes)
	// This is a StackWeaver-specific feature for preventing stuck applies
	if workspace.RunTimeout > 0 {
		attributes["run-timeout"] = workspace.RunTimeout
	}

	// TFE API: setting-overwrites indicates which settings the workspace defines itself
	// vs inheriting from org/project defaults. Since we always store explicit values, mark as overwritten.
	settingOverwrites := gin.H{}
	if workspace.ExecutionMode != "" && workspace.ExecutionMode != "remote" {
		settingOverwrites["execution-mode"] = true
		settingOverwrites["agent-pool"] = workspace.AgentPoolID != nil
	} else {
		settingOverwrites["execution-mode"] = false
		settingOverwrites["agent-pool"] = false
	}
	attributes["setting-overwrites"] = settingOverwrites

	// Build relationships
	relationships := gin.H{}

	// TFE API: organization relationship (required by tfe provider's workspace read)
	if workspace.Project.OrganizationID != uuid.Nil {
		orgName := workspace.Project.Organization.Name
		if orgName == "" {
			orgName = workspace.Project.OrganizationID.String()
		}
		relationships["organization"] = gin.H{
			"data": gin.H{
				"id":   orgName,
				"type": "organizations",
			},
		}
	}

	if workspace.ProjectID != uuid.Nil {
		relationships["project"] = gin.H{
			"data": gin.H{
				"id":   workspace.ProjectID.String(),
				"type": "projects",
			},
		}
	}

	// TFE API: agent-pool relationship (required by go-tfe client / tfe_workspace_settings)
	if workspace.AgentPoolID != nil {
		relationships["agent-pool"] = gin.H{
			"data": gin.H{
				"id":   workspace.AgentPoolID.String(),
				"type": "agent-pools",
			},
		}
	} else {
		relationships["agent-pool"] = gin.H{
			"data": nil,
		}
	}

	// TFE API: locked-by relationship when workspace is locked
	if workspace.Locked && workspace.LockedBy != nil {
		relationships["locked-by"] = gin.H{
			"data": gin.H{
				"id":   workspace.LockedBy.String(),
				"type": "users",
			},
			"links": gin.H{
				"related": "/api/v2/users/" + workspace.LockedBy.String(),
			},
		}
	}

	return gin.H{
		"id":            workspace.ID,
		"type":          "workspaces",
		"attributes":    attributes,
		"relationships": relationships,
	}
}

// formatWorkspaceSimple formats a workspace in simple format for frontend compatibility
func formatWorkspaceSimple(workspace *models.Workspace) gin.H {
	// Default branch to "main" when empty
	vcsBranch := workspace.VCSBranch
	if vcsBranch == "" && workspace.VCSRepository != "" {
		vcsBranch = "main"
	}

	// Derive VCS provider: prefer VCSConnection.Provider (source of truth), fall back to deprecated workspace field
	vcsProvider := workspace.VCSProvider
	if vcsProvider == "" && workspace.VCSConnection != nil {
		vcsProvider = string(workspace.VCSConnection.Provider)
	}

	result := gin.H{
		"id":                            workspace.ID,
		"project_id":                    workspace.ProjectID.String(),
		"name":                          workspace.Name,
		"description":                   workspace.Description,
		"vcs_connection_id":             nil, // Will be set below if workspace has VCS connection
		"vcs_provider":                  vcsProvider,
		"vcs_repository":                workspace.VCSRepository,
		"vcs_branch":                    vcsBranch,
		"terraform_version":             workspace.TerraformVersion,
		"working_directory":             workspace.WorkingDirectory,
		"auto_queue_runs":               workspace.AutoQueueRuns,
		"auto_apply":                    workspace.AutoApply,
		"auto_apply_run_trigger":        workspace.AutoApplyRunTrigger,
		"allow_destroy_plan":            workspace.AllowDestroyPlan,
		"queue_all_runs":                workspace.QueueAllRuns,
		"speculative_enabled":           workspace.SpeculativeEnabled,
		"file_triggers_enabled":         workspace.FileTriggersEnabled,
		"global_remote_state":           workspace.GlobalRemoteState,
		"structured_run_output_enabled": workspace.StructuredRunOutputEnabled,
		"assessments_enabled":           workspace.AssessmentsEnabled,
		"execution_mode":                workspace.ExecutionMode,
		"agent_pool_id":                 nil, // Will be set below if workspace has agent pool
		"force_delete":                  workspace.ForceDelete,
		"locked":                        workspace.Locked,
		"locked_reason":                 workspace.LockedReason,
		"run_timeout":                   workspace.RunTimeout,
		"created_at":                    workspace.CreatedAt.Format("2006-01-02T15:04:05Z"),
		"updated_at":                    workspace.UpdatedAt.Format("2006-01-02T15:04:05Z"),
	}

	// Include VCS connection ID and account name if workspace has one configured
	if workspace.VCSConnectionID != nil {
		result["vcs_connection_id"] = workspace.VCSConnectionID.String()
		if workspace.VCSConnection != nil {
			result["vcs_account_name"] = workspace.VCSConnection.AccountName
		}
	}

	// Include agent pool ID and name if workspace has one configured
	if workspace.AgentPoolID != nil {
		result["agent_pool_id"] = workspace.AgentPoolID.String()
		if workspace.AgentPool.Name != "" {
			result["agent_pool_name"] = workspace.AgentPool.Name
		}
	}

	// Include locked_by and locked_at if workspace is locked
	if workspace.LockedBy != nil {
		result["locked_by"] = workspace.LockedBy.String()
	}
	if workspace.LockedAt != nil {
		result["locked_at"] = workspace.LockedAt.Format("2006-01-02T15:04:05Z")
	}

	return result
}

// GetByOrganizationAndName gets a workspace by organization name and workspace name (TFE-compatible)
// GET /api/v2/organizations/:name/workspaces/:name
// Supports ?format=simple for frontend compatibility
func (h *WorkspaceHandlerV2) GetByOrganizationAndName(c *gin.Context) {
	orgName := c.Param("name")
	workspaceName := c.Param("workspace_name")

	workspace, err := h.workspaceRepo.GetByOrganizationAndName(orgName, workspaceName)
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

	// Check if frontend wants simple format
	if c.Query("format") == "simple" {
		c.JSON(http.StatusOK, gin.H{
			"data": formatWorkspaceSimple(workspace),
		})
		return
	}

	// Default: TFE-compatible JSON:API format
	c.JSON(http.StatusOK, gin.H{
		"data": formatWorkspaceResponse(workspace, h.vcsConnectionRepo),
	})
}

// Create creates a workspace in an organization (TFE-compatible)
// POST /api/v2/organizations/:name/workspaces
func (h *WorkspaceHandlerV2) Create(c *gin.Context) {
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

	// Check if user has permission to create workspaces
	// Admins have PermissionOrgManageWorkspaces, members have PermissionWorkspaceWrite
	// We allow both to create workspaces (members can do day-to-day tasks)
	hasManageWorkspaces, err := h.rbacService.CheckOrgManageWorkspaces(c.Request.Context(), user.ID, org.ID)
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

	// Note: Team-based permission model - CheckOrgManageWorkspaces already checks team memberships
	// If user doesn't have org-level manage permission, they cannot create workspaces
	// Workspace creation requires org-level manage permission (users get workspace access via project/workspace team access)
	if !hasManageWorkspaces {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "You do not have permission to create workspaces. Workspace creation requires organization-level manage-workspaces permission via team membership.",
				},
			},
		})
		return
	}

	var req CreateWorkspaceRequestV2
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

	// Extract from JSON:API format (TFE-compatible)
	if req.Data.Attributes.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Workspace name is required (provide 'data.attributes.name')",
				},
			},
		})
		return
	}

	workspaceName := req.Data.Attributes.Name
	description := req.Data.Attributes.Description
	projectID := req.Data.Attributes.ProjectID
	vcsConnectionID := req.Data.Attributes.VCSConnectionID
	vcsRepository := req.Data.Attributes.VCSRepository
	vcsBranch := req.Data.Attributes.VCSBranch
	workingDirectory := req.Data.Attributes.WorkingDirectory
	terraformVersion := req.Data.Attributes.TerraformVersion
	autoQueueRuns := req.Data.Attributes.AutoQueueRuns
	autoApply := req.Data.Attributes.AutoApply
	executionMode := req.Data.Attributes.ExecutionMode

	// TFE-compatible: Handle project from JSON:API relationship
	if projectID == nil && req.Data.Relationships.Project != nil && req.Data.Relationships.Project.Data.ID != "" {
		parsedID, err := uuid.Parse(req.Data.Relationships.Project.Data.ID)
		if err == nil {
			projectID = &parsedID
		}
	}

	// TFE-compatible: Handle vcs-repo nested attribute (go-tfe sends this)
	if req.Data.Attributes.VCSRepo != nil && req.Data.Attributes.VCSRepo.Identifier != "" {
		vcsRepository = req.Data.Attributes.VCSRepo.Identifier
		if req.Data.Attributes.VCSRepo.Branch != "" {
			vcsBranch = req.Data.Attributes.VCSRepo.Branch
		}
		// Resolve VCS connection from github-app-installation-id or oauth-token-id
		if vcsConnectionID == nil {
			switch {
			case req.Data.Attributes.VCSRepo.GHAInstallationID != "":
				conn, err := h.vcsConnectionRepo.GetByInstallationID(req.Data.Attributes.VCSRepo.GHAInstallationID)
				if err == nil {
					vcsConnectionID = &conn.ID
				}
			case req.Data.Attributes.VCSRepo.OAuthTokenID != "":
				parsedID, err := uuid.Parse(req.Data.Attributes.VCSRepo.OAuthTokenID)
				if err == nil {
					vcsConnectionID = &parsedID
				}
			default:
				// No explicit ID — use the org's VCS connection
				conns, connErr := h.vcsConnectionRepo.ListByOrganization(org.ID)
				if connErr == nil && len(conns) > 0 {
					vcsConnectionID = &conns[0].ID
				}
			}
		}
	}

	// Validate workspace name is not empty
	if workspaceName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Workspace name cannot be empty",
				},
			},
		})
		return
	}

	// Determine which project to use
	var finalProjectID uuid.UUID
	if projectID != nil {
		// Validate project exists and belongs to organization
		project, err := h.projectRepo.GetByID(*projectID)
		if err != nil || project.OrganizationID != org.ID {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "Invalid project ID or project does not belong to this organization",
					},
				},
			})
			return
		}
		finalProjectID = *projectID
	} else {
		// Try to find default project first, then fall back to first project
		projects, _, err := h.projectRepo.ListByOrganization(org.ID, 100, 0) // Get all projects to find "default"
		if err != nil || len(projects) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "Organization must have at least one project to create workspaces",
					},
				},
			})
			return
		}

		// Look for project named "default" first
		var defaultProject *models.Project
		for _, p := range projects {
			if p.Name == "default" {
				defaultProject = &p
				break
			}
		}

		if defaultProject != nil {
			finalProjectID = defaultProject.ID
		} else {
			// Fall back to first project if no "default" project exists
			finalProjectID = projects[0].ID
		}
	}

	// Validate VCS connection if provided
	if vcsConnectionID != nil {
		// Validate VCS connection exists
		_, err := h.vcsConnectionRepo.GetByID(*vcsConnectionID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "VCS connection not found",
					},
				},
			})
			return
		}

		// If VCS connection is provided, repository should also be provided
		if vcsRepository == "" {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "vcs_repository is required when vcs_connection_id is provided",
					},
				},
			})
			return
		}
	}

	// Set defaults
	if vcsBranch == "" {
		vcsBranch = "main"
	}

	if executionMode == "" {
		executionMode = "remote"
	}

	finalAutoQueueRuns := false
	if autoQueueRuns != nil {
		finalAutoQueueRuns = *autoQueueRuns
	}

	finalAutoApply := false
	if autoApply != nil {
		finalAutoApply = *autoApply
	}

	// Check for duplicate workspace name in project
	existing, _ := h.workspaceRepo.GetByProjectAndName(finalProjectID, workspaceName)
	if existing != nil {
		c.JSON(http.StatusConflict, gin.H{
			"errors": []gin.H{
				{
					"status": "409",
					"title":  "Conflict",
					"detail": "Workspace with this name already exists",
				},
			},
		})
		return
	}

	// Resolve terraform version: workspace setting -> org default
	if terraformVersion == "" {
		terraformVersion = org.DefaultTerraformVersion
	}

	// Validate terraform version exists and is enabled (like TFE)
	if terraformVersion != "" && h.db != nil {
		var tfVersion models.TerraformVersion
		if err := h.db.Where("version = ? AND enabled = ?", terraformVersion, true).First(&tfVersion).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				c.JSON(http.StatusUnprocessableEntity, gin.H{
					"errors": []gin.H{{
						"status": "422",
						"title":  "Invalid terraform version",
						"detail": fmt.Sprintf("Terraform version %s is not available. Use GET /api/v2/admin/terraform-versions to list available versions.", terraformVersion),
					}},
				})
				return
			}
		}
	}

	workspace := &models.Workspace{
		ProjectID:                  finalProjectID,
		Name:                       workspaceName,
		Description:                description,
		VCSConnectionID:            vcsConnectionID,
		VCSRepository:              vcsRepository,
		VCSBranch:                  vcsBranch,
		WorkingDirectory:           workingDirectory,
		TerraformVersion:           terraformVersion,
		AutoQueueRuns:              finalAutoQueueRuns,
		AutoApply:                  finalAutoApply,
		ExecutionMode:              executionMode,
		QueueAllRuns:               true,  // Default (TFE provider default)
		SpeculativeEnabled:         true,  // Default
		AllowDestroyPlan:           true,  // Default (TFE default)
		FileTriggersEnabled:        true,  // Default
		GlobalRemoteState:          false, // Default
		StructuredRunOutputEnabled: true,  // Default
		AssessmentsEnabled:         false, // Default
		RunTimeout:                 7200,  // Default: 2 hours
	}

	// Apply optional boolean fields from request (overriding defaults)
	attrs := req.Data.Attributes
	if attrs.AllowDestroyPlan != nil {
		workspace.AllowDestroyPlan = *attrs.AllowDestroyPlan
	}
	if attrs.AutoApplyRunTrigger != nil {
		workspace.AutoApplyRunTrigger = *attrs.AutoApplyRunTrigger
	}
	if attrs.QueueAllRuns != nil {
		workspace.QueueAllRuns = *attrs.QueueAllRuns
	}
	if attrs.SpeculativeEnabled != nil {
		workspace.SpeculativeEnabled = *attrs.SpeculativeEnabled
	}
	if attrs.FileTriggersEnabled != nil {
		workspace.FileTriggersEnabled = *attrs.FileTriggersEnabled
	}
	if attrs.GlobalRemoteState != nil {
		workspace.GlobalRemoteState = *attrs.GlobalRemoteState
	}
	if attrs.StructuredRunOutputEnabled != nil {
		workspace.StructuredRunOutputEnabled = *attrs.StructuredRunOutputEnabled
	}
	if attrs.AssessmentsEnabled != nil {
		workspace.AssessmentsEnabled = *attrs.AssessmentsEnabled
	}
	if attrs.ForceDelete != nil {
		workspace.ForceDelete = *attrs.ForceDelete
	}
	if attrs.SourceName != "" {
		workspace.SourceName = attrs.SourceName
	}
	if attrs.SourceURL != "" {
		workspace.SourceURL = attrs.SourceURL
	}

	// Handle trigger-prefixes / trigger-patterns (stored as JSON arrays)
	if len(attrs.TriggerPrefixes) > 0 {
		if data, err := json.Marshal(attrs.TriggerPrefixes); err == nil {
			workspace.TriggerPrefixes = string(data)
		}
	}
	if len(attrs.TriggerPatterns) > 0 {
		if data, err := json.Marshal(attrs.TriggerPatterns); err == nil {
			workspace.TriggerPatterns = string(data)
		}
	}
	if len(attrs.TagNames) > 0 {
		if data, err := json.Marshal(attrs.TagNames); err == nil {
			workspace.TagNames = string(data)
		}
	}

	// Set VCS-specific fields from vcs-repo
	if attrs.VCSRepo != nil {
		workspace.VCSIngressSubmodules = attrs.VCSRepo.IngressSubmodules
		workspace.VCSTagsRegex = attrs.VCSRepo.TagsRegex
	}

	// Set run timeout if provided (custom extension, not in TFE spec)
	if attrs.RunTimeout != nil && *attrs.RunTimeout > 0 {
		workspace.RunTimeout = *attrs.RunTimeout
	}

	// Set agent pool ID if execution mode is "agent" and pool ID is provided
	if executionMode == "agent" && attrs.AgentPoolID != nil && *attrs.AgentPoolID != "" {
		poolID, err := uuid.Parse(*attrs.AgentPoolID)
		if err == nil {
			workspace.AgentPoolID = &poolID
		}
	}

	if err := h.workspaceRepo.Create(workspace); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to create workspace",
				},
			},
		})
		return
	}

	// Register repository-scoped webhooks when the workspace is linked to an ADO repo.
	h.maybeRegisterADOWebhook(workspace.VCSConnectionID, workspace.VCSRepository)

	// Reload workspace from database to ensure all fields (CreatedAt, UpdatedAt, etc.) are populated
	createdWorkspace, err := h.workspaceRepo.GetByID(workspace.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to retrieve created workspace",
				},
			},
		})
		return
	}

	// Log activity (non-blocking)
	if h.activityService != nil {
		activityCtx := helpers.GetActivityContext(c)
		activityCtx.OrganizationID = &org.ID
		activityCtx.ProjectID = &createdWorkspace.ProjectID
		activityCtx.WorkspaceID = &createdWorkspace.ID
		_ = h.activityService.LogCreate(c.Request.Context(), "workspace", createdWorkspace.ID, createdWorkspace.Name, activityCtx)
	}

	// Check if frontend wants simple format
	if c.Query("format") == "simple" {
		c.JSON(http.StatusCreated, gin.H{
			"data": formatWorkspaceSimple(createdWorkspace),
		})
		return
	}

	// Default: Format in TFE-compatible JSON:API format
	c.JSON(http.StatusCreated, gin.H{
		"data": formatWorkspaceResponse(createdWorkspace, h.vcsConnectionRepo),
	})
}

// Update updates a workspace by organization name and workspace name (TFE-compatible)
// PATCH /api/v2/organizations/:name/workspaces/:name
func (h *WorkspaceHandlerV2) Update(c *gin.Context) {
	orgName := c.Param("name")
	workspaceName := c.Param("workspace_name")

	workspace, err := h.workspaceRepo.GetByOrganizationAndName(orgName, workspaceName)
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

	var req UpdateWorkspaceRequestV2
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

	// Get organization for validation
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

	// Verify user has permission to update workspace
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

	// Check if user has permission to update workspace
	// Check org-level permission first (admins have PermissionOrgManageWorkspaces)
	hasOrgManage, err := h.rbacService.CheckOrgManageWorkspaces(c.Request.Context(), user.ID, org.ID)
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

	// If no org-level permission, check workspace-level permission (members with workspace access)
	if !hasOrgManage {
		// workspace.ID is already a string, so use it directly
		hasWorkspaceWrite, err := h.rbacService.CheckWorkspacePermission(
			c.Request.Context(),
			user.ID,
			workspace.ID,
			rbac.PermissionWorkspaceWrite,
			workspace.ProjectID,
		)
		if err != nil || !hasWorkspaceWrite {
			c.JSON(http.StatusForbidden, gin.H{
				"errors": []gin.H{
					{
						"status": "403",
						"title":  "Forbidden",
						"detail": "Only organization admins and members with workspace access can update workspaces",
					},
				},
			})
			return
		}
	}

	// Update from JSON:API format (TFE-compatible)
	attrs := req.Data.Attributes

	// Track changes for activity logging
	changes := map[string]interface{}{}

	// Update name
	if attrs.Name != "" {
		// Check if new name conflicts with existing workspace in project
		if attrs.Name != workspace.Name {
			existing, _ := h.workspaceRepo.GetByProjectAndName(workspace.ProjectID, attrs.Name)
			if existing != nil {
				c.JSON(http.StatusConflict, gin.H{
					"errors": []gin.H{
						{
							"status": "409",
							"title":  "Conflict",
							"detail": "Workspace with this name already exists",
						},
					},
				})
				return
			}
			changes["name"] = attrs.Name
		}
		workspace.Name = attrs.Name
	}

	// Update description (allow empty string to clear)
	// Note: In JSON:API, if description is not provided, it will be empty string
	// We update if it's different from current value
	if workspace.Description != attrs.Description {
		changes["description"] = attrs.Description
		workspace.Description = attrs.Description
	}

	// TFE-compatible: Handle vcs-repo nested attribute (go-tfe sends this on update)
	if attrs.VCSRepo != nil && attrs.VCSRepo.Identifier != "" {
		if attrs.VCSRepository == "" {
			attrs.VCSRepository = attrs.VCSRepo.Identifier
		}
		if attrs.VCSBranch == "" && attrs.VCSRepo.Branch != "" {
			attrs.VCSBranch = attrs.VCSRepo.Branch
		}
		// Resolve VCS connection from github-app-installation-id or oauth-token-id
		if attrs.VCSConnectionID == nil {
			switch {
			case attrs.VCSRepo.GHAInstallationID != "":
				conn, err := h.vcsConnectionRepo.GetByInstallationID(attrs.VCSRepo.GHAInstallationID)
				if err == nil {
					attrs.VCSConnectionID = &conn.ID
				}
			case attrs.VCSRepo.OAuthTokenID != "":
				parsedID, err := uuid.Parse(attrs.VCSRepo.OAuthTokenID)
				if err == nil {
					attrs.VCSConnectionID = &parsedID
				}
			default:
				// If no specific connection ID, try to find the org's VCS connection
				conns, connErr := h.vcsConnectionRepo.ListByOrganization(org.ID)
				if connErr == nil && len(conns) > 0 {
					attrs.VCSConnectionID = &conns[0].ID
				}
			}
		}
	}

	// Update VCS connection (state-invalidating change - validate but allow)
	if attrs.VCSConnectionID != nil {
		// Validate VCS connection exists
		_, err := h.vcsConnectionRepo.GetByID(*attrs.VCSConnectionID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "VCS connection not found",
					},
				},
			})
			return
		}

		// Check if VCS connection is actually changing
		oldVCSConnectionID := workspace.VCSConnectionID
		if (oldVCSConnectionID == nil && attrs.VCSConnectionID != nil) ||
			(oldVCSConnectionID != nil && attrs.VCSConnectionID != nil && *oldVCSConnectionID != *attrs.VCSConnectionID) {
			changes["vcs_connection_id"] = attrs.VCSConnectionID.String()
		}
		workspace.VCSConnectionID = attrs.VCSConnectionID
	}

	// Update VCS provider (deprecated, but keep for backward compatibility)
	if attrs.VCSProvider != "" {
		if workspace.VCSProvider != attrs.VCSProvider {
			changes["vcs_provider"] = attrs.VCSProvider
		}
		workspace.VCSProvider = attrs.VCSProvider
	}

	// Update VCS repository (state-invalidating change - validate but allow)
	if attrs.VCSRepository != "" {
		// If VCS connection is set or being set, repository should be provided
		finalVCSConnectionID := workspace.VCSConnectionID
		if attrs.VCSConnectionID != nil {
			finalVCSConnectionID = attrs.VCSConnectionID
		}
		if finalVCSConnectionID != nil && attrs.VCSRepository == "" {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{
						"status": "400",
						"title":  "Bad Request",
						"detail": "vcs_repository is required when vcs_connection_id is set",
					},
				},
			})
			return
		}
		if workspace.VCSRepository != attrs.VCSRepository {
			changes["vcs_repository"] = attrs.VCSRepository
		}
		workspace.VCSRepository = attrs.VCSRepository
	}

	// Update VCS branch (state-invalidating change - validate but allow)
	if attrs.VCSBranch != "" {
		if workspace.VCSBranch != attrs.VCSBranch {
			changes["vcs_branch"] = attrs.VCSBranch
		}
		workspace.VCSBranch = attrs.VCSBranch
	}

	// Update Terraform version
	if attrs.TerraformVersion != "" {
		if workspace.TerraformVersion != attrs.TerraformVersion {
			changes["terraform_version"] = attrs.TerraformVersion
		}
		workspace.TerraformVersion = attrs.TerraformVersion
	}

	// Update working directory (allow empty string to clear)
	if workspace.WorkingDirectory != attrs.WorkingDirectory {
		changes["working_directory"] = attrs.WorkingDirectory
		workspace.WorkingDirectory = attrs.WorkingDirectory
	}

	// Update auto-queue-runs
	if attrs.AutoQueueRuns != nil {
		if workspace.AutoQueueRuns != *attrs.AutoQueueRuns {
			changes["auto_queue_runs"] = *attrs.AutoQueueRuns
		}
		workspace.AutoQueueRuns = *attrs.AutoQueueRuns
	}

	// Update auto-apply
	if attrs.AutoApply != nil {
		if workspace.AutoApply != *attrs.AutoApply {
			changes["auto_apply"] = *attrs.AutoApply
		}
		workspace.AutoApply = *attrs.AutoApply
	}

	// Update execution mode
	if attrs.ExecutionMode != "" {
		if workspace.ExecutionMode != attrs.ExecutionMode {
			changes["execution_mode"] = attrs.ExecutionMode
		}
		workspace.ExecutionMode = attrs.ExecutionMode
		// When switching to remote, clear agent_pool_id so workspace is not tied to an agent pool
		if attrs.ExecutionMode == "remote" && workspace.AgentPoolID != nil {
			changes["agent_pool_id"] = nil
			workspace.AgentPoolID = nil
		}
	}

	// Update agent pool ID (TFE-compatible: set when execution_mode=agent, clear otherwise)
	if attrs.AgentPoolID != nil {
		if *attrs.AgentPoolID == "" {
			// Clear agent pool; when switching away from agent, set execution_mode=remote
			// so the workspace is fully remote (not just "agent with no pool").
			if workspace.AgentPoolID != nil {
				changes["agent_pool_id"] = nil
			}
			workspace.AgentPoolID = nil
			if workspace.ExecutionMode == "agent" {
				changes["execution_mode"] = "remote"
				workspace.ExecutionMode = "remote"
			}
		} else {
			poolID, err := uuid.Parse(*attrs.AgentPoolID)
			if err == nil {
				if workspace.AgentPoolID == nil || *workspace.AgentPoolID != poolID {
					changes["agent_pool_id"] = poolID.String()
				}
				workspace.AgentPoolID = &poolID
			}
		}
	}

	// Update run timeout if provided (custom extension, not in TFE spec)
	if attrs.RunTimeout != nil && *attrs.RunTimeout > 0 {
		if workspace.RunTimeout != *attrs.RunTimeout {
			changes["run_timeout"] = *attrs.RunTimeout
		}
		workspace.RunTimeout = *attrs.RunTimeout
	}

	// Update auto-apply-run-trigger
	if attrs.AutoApplyRunTrigger != nil {
		if workspace.AutoApplyRunTrigger != *attrs.AutoApplyRunTrigger {
			changes["auto_apply_run_trigger"] = *attrs.AutoApplyRunTrigger
		}
		workspace.AutoApplyRunTrigger = *attrs.AutoApplyRunTrigger
	}

	// Update allow-destroy-plan
	if attrs.AllowDestroyPlan != nil {
		if workspace.AllowDestroyPlan != *attrs.AllowDestroyPlan {
			changes["allow_destroy_plan"] = *attrs.AllowDestroyPlan
		}
		workspace.AllowDestroyPlan = *attrs.AllowDestroyPlan
	}

	// Update queue-all-runs
	if attrs.QueueAllRuns != nil {
		if workspace.QueueAllRuns != *attrs.QueueAllRuns {
			changes["queue_all_runs"] = *attrs.QueueAllRuns
		}
		workspace.QueueAllRuns = *attrs.QueueAllRuns
	}

	// Update speculative-enabled
	if attrs.SpeculativeEnabled != nil {
		if workspace.SpeculativeEnabled != *attrs.SpeculativeEnabled {
			changes["speculative_enabled"] = *attrs.SpeculativeEnabled
		}
		workspace.SpeculativeEnabled = *attrs.SpeculativeEnabled
	}

	// Update file-triggers-enabled
	if attrs.FileTriggersEnabled != nil {
		if workspace.FileTriggersEnabled != *attrs.FileTriggersEnabled {
			changes["file_triggers_enabled"] = *attrs.FileTriggersEnabled
		}
		workspace.FileTriggersEnabled = *attrs.FileTriggersEnabled
	}

	// Update global-remote-state
	if attrs.GlobalRemoteState != nil {
		if workspace.GlobalRemoteState != *attrs.GlobalRemoteState {
			changes["global_remote_state"] = *attrs.GlobalRemoteState
		}
		workspace.GlobalRemoteState = *attrs.GlobalRemoteState
	}

	// Update structured-run-output-enabled
	if attrs.StructuredRunOutputEnabled != nil {
		if workspace.StructuredRunOutputEnabled != *attrs.StructuredRunOutputEnabled {
			changes["structured_run_output_enabled"] = *attrs.StructuredRunOutputEnabled
		}
		workspace.StructuredRunOutputEnabled = *attrs.StructuredRunOutputEnabled
	}

	// Update assessments-enabled
	if attrs.AssessmentsEnabled != nil {
		if workspace.AssessmentsEnabled != *attrs.AssessmentsEnabled {
			changes["assessments_enabled"] = *attrs.AssessmentsEnabled
		}
		workspace.AssessmentsEnabled = *attrs.AssessmentsEnabled
	}

	// Update force-delete
	if attrs.ForceDelete != nil {
		if workspace.ForceDelete != *attrs.ForceDelete {
			changes["force_delete"] = *attrs.ForceDelete
		}
		workspace.ForceDelete = *attrs.ForceDelete
	}

	// Update trigger-prefixes
	if len(attrs.TriggerPrefixes) > 0 {
		if data, err := json.Marshal(attrs.TriggerPrefixes); err == nil {
			workspace.TriggerPrefixes = string(data)
			changes["trigger_prefixes"] = attrs.TriggerPrefixes
		}
	}

	// Update trigger-patterns
	if len(attrs.TriggerPatterns) > 0 {
		if data, err := json.Marshal(attrs.TriggerPatterns); err == nil {
			workspace.TriggerPatterns = string(data)
			changes["trigger_patterns"] = attrs.TriggerPatterns
		}
	}

	// Update tag-names
	if len(attrs.TagNames) > 0 {
		if data, err := json.Marshal(attrs.TagNames); err == nil {
			workspace.TagNames = string(data)
			changes["tag_names"] = attrs.TagNames
		}
	}

	// Update VCS sub-fields from vcs-repo
	if attrs.VCSRepo != nil {
		workspace.VCSIngressSubmodules = attrs.VCSRepo.IngressSubmodules
		if attrs.VCSRepo.TagsRegex != "" {
			workspace.VCSTagsRegex = attrs.VCSRepo.TagsRegex
		}
	}

	if err := h.workspaceRepo.Update(workspace); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to update workspace",
				},
			},
		})
		return
	}

	// Register repository-scoped webhooks if VCS repo was set or changed.
	h.maybeRegisterADOWebhook(workspace.VCSConnectionID, workspace.VCSRepository)

	// Log activity (non-blocking)
	if h.activityService != nil && len(changes) > 0 {
		activityCtx := helpers.GetActivityContext(c)
		project, _ := h.projectRepo.GetByID(workspace.ProjectID)
		if project != nil {
			activityCtx.OrganizationID = &project.OrganizationID
		}
		activityCtx.ProjectID = &workspace.ProjectID
		workspaceIDStr := workspace.ID
		activityCtx.WorkspaceID = &workspaceIDStr
		_ = h.activityService.LogUpdate(c.Request.Context(), "workspace", workspace.ID, workspace.Name, changes, activityCtx)
	}

	// Check if frontend wants simple format
	if c.Query("format") == "simple" {
		c.JSON(http.StatusOK, gin.H{
			"data": formatWorkspaceSimple(workspace),
		})
		return
	}

	// Default: Format in TFE-compatible JSON:API format
	c.JSON(http.StatusOK, gin.H{
		"data": formatWorkspaceResponse(workspace, h.vcsConnectionRepo),
	})
}

// Delete deletes a workspace by organization name and workspace name (TFE-compatible)
// DELETE /api/v2/organizations/:name/workspaces/:name
func (h *WorkspaceHandlerV2) Delete(c *gin.Context) {
	orgName := c.Param("name")
	workspaceName := c.Param("workspace_name")

	workspace, err := h.workspaceRepo.GetByOrganizationAndName(orgName, workspaceName)
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

	// Get organization for validation and activity logging
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

	// Check if user has permission to delete workspace
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

	// Check org-level permission first (admins have PermissionOrgManageWorkspaces)
	hasOrgManage, err := h.rbacService.CheckOrgManageWorkspaces(c.Request.Context(), user.ID, org.ID)
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

	// If no org-level permission, check workspace-level permission (members with workspace access)
	if !hasOrgManage {
		hasWorkspaceWrite, err := h.rbacService.CheckWorkspacePermission(
			c.Request.Context(),
			user.ID,
			workspace.ID,
			rbac.PermissionWorkspaceWrite,
			workspace.ProjectID,
		)
		if err != nil || !hasWorkspaceWrite {
			c.JSON(http.StatusForbidden, gin.H{
				"errors": []gin.H{
					{
						"status": "403",
						"title":  "Forbidden",
						"detail": "Only organization admins and members with workspace access can delete workspaces",
					},
				},
			})
			return
		}
	}

	// Check if workspace has active infrastructure (applied runs but no successful destroy)
	// Skip this check if ?force=true query param OR workspace.ForceDelete attribute is set (TFE force_delete)
	forceDelete := c.Query("force") == "true" || workspace.ForceDelete
	if !forceDelete {
		hasActiveInfrastructure, err := h.workspaceRepo.HasActiveInfrastructure(workspace.ID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{
					{
						"status": "500",
						"title":  "Internal Server Error",
						"detail": "Failed to check workspace runs",
					},
				},
			})
			return
		}
		if hasActiveInfrastructure {
			c.JSON(http.StatusConflict, gin.H{
				"errors": []gin.H{
					{
						"status": "409",
						"title":  "Conflict",
						"detail": "Cannot delete workspace with active infrastructure. Please run a destroy operation first, or use force delete.",
					},
				},
			})
			return
		}
	}

	// Log activity before deletion (non-blocking)
	if h.activityService != nil && org != nil {
		activityCtx := helpers.GetActivityContext(c)
		activityCtx.OrganizationID = &org.ID
		activityCtx.ProjectID = &workspace.ProjectID
		workspaceIDStr := workspace.ID
		activityCtx.WorkspaceID = &workspaceIDStr
		_ = h.activityService.LogDelete(c.Request.Context(), "workspace", workspace.ID, workspace.Name, activityCtx)
	}

	if err := h.workspaceRepo.Delete(workspace.ID); err != nil {
		logger.Errorf("Failed to delete workspace %s: %v", workspace.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": fmt.Sprintf("Failed to delete workspace: %v", err),
				},
			},
		})
		return
	}

	c.Status(http.StatusNoContent)
}

// DeleteByID deletes a workspace by its ID (TFE-compatible force delete)
// DELETE /api/v2/workspaces/:id
func (h *WorkspaceHandlerV2) DeleteByID(c *gin.Context) {
	id := c.Param("id")
	workspace, err := h.workspaceRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Workspace not found"}},
		})
		return
	}

	// Force delete by ID (TFE behavior: DELETE /workspaces/:id is force delete)
	if err := h.workspaceRepo.Delete(workspace.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": fmt.Sprintf("Failed to delete workspace: %v", err)}},
		})
		return
	}

	c.Status(http.StatusNoContent)
}

// SafeDelete safely deletes a workspace by org+name (checks for active infrastructure)
// POST /api/v2/organizations/:name/workspaces/:workspace_name/actions/safe-delete
func (h *WorkspaceHandlerV2) SafeDelete(c *gin.Context) {
	// Reuse the Delete handler — it checks for active infrastructure by default
	h.Delete(c)
}

// SafeDeleteByID safely deletes a workspace by ID (checks for active infrastructure)
// POST /api/v2/workspaces/:id/actions/safe-delete
func (h *WorkspaceHandlerV2) SafeDeleteByID(c *gin.Context) {
	id := c.Param("id")
	workspace, err := h.workspaceRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Workspace not found"}},
		})
		return
	}

	// Safe delete: check for active infrastructure
	hasActiveInfrastructure, err := h.workspaceRepo.HasActiveInfrastructure(workspace.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to check workspace runs"}},
		})
		return
	}
	if hasActiveInfrastructure {
		c.JSON(http.StatusConflict, gin.H{
			"errors": []gin.H{{"status": "409", "title": "Conflict", "detail": "Cannot delete workspace with active infrastructure. Please run a destroy operation first."}},
		})
		return
	}

	if err := h.workspaceRepo.Delete(workspace.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": fmt.Sprintf("Failed to delete workspace: %v", err)}},
		})
		return
	}

	c.Status(http.StatusNoContent)
}

// LockRequest represents the request body for locking a workspace (TFE-compatible)
type LockRequest struct {
	Reason string `json:"reason"`
}

// Lock locks a workspace manually
// POST /api/v2/workspaces/:id/actions/lock
func (h *WorkspaceHandlerV2) Lock(c *gin.Context) {
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

	// Parse optional request body for reason (TFE-compatible)
	var lockReq LockRequest
	// Ignore errors - body is optional
	_ = c.ShouldBindJSON(&lockReq)

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

	if workspace.Locked {
		c.JSON(http.StatusConflict, gin.H{
			"errors": []gin.H{
				{
					"status": "409",
					"title":  "Conflict",
					"detail": "Workspace is already locked",
				},
			},
		})
		return
	}

	// Get current user for permission check and locking
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

	// Check workspace locking permission using RBAC service
	hasPermission, err := h.rbacService.CheckWorkspaceLockingPermission(c.Request.Context(), user.ID, workspaceID, workspace.ProjectID)
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
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Insufficient permissions to lock workspace",
				},
			},
		})
		return
	}

	lockedBy := &user.ID

	now := time.Now()
	workspace.Locked = true
	workspace.LockedBy = lockedBy
	workspace.LockedAt = &now
	workspace.LockedReason = lockReq.Reason

	if err := h.workspaceRepo.Update(workspace); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to lock workspace",
				},
			},
		})
		return
	}

	// Reload to get updated workspace
	workspace, _ = h.workspaceRepo.GetByID(workspaceID)

	// Check if frontend wants simple format
	if c.Query("format") == "simple" {
		c.JSON(http.StatusOK, gin.H{
			"data": formatWorkspaceSimple(workspace),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatWorkspaceResponse(workspace, h.vcsConnectionRepo),
	})
}

// Unlock unlocks a workspace manually
// POST /api/v2/workspaces/:id/actions/unlock
func (h *WorkspaceHandlerV2) Unlock(c *gin.Context) {
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

	if !workspace.Locked {
		c.JSON(http.StatusConflict, gin.H{
			"errors": []gin.H{
				{
					"status": "409",
					"title":  "Conflict",
					"detail": "Workspace is not locked",
				},
			},
		})
		return
	}

	// Get current user for permission check
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

	// Check workspace locking permission using RBAC service
	hasPermission, err := h.rbacService.CheckWorkspaceLockingPermission(c.Request.Context(), user.ID, workspaceID, workspace.ProjectID)
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
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Insufficient permissions to unlock workspace",
				},
			},
		})
		return
	}

	// TFE behavior: normal unlock can only be done by the user who locked it (unless they have admin permissions)
	// Check if current user matches the locker or has admin permissions
	if workspace.LockedBy != nil && *workspace.LockedBy != user.ID {
		// Check if user has admin permissions (org admin or workspace write)
		hasAdmin, err := h.rbacService.CheckWorkspacePermission(c.Request.Context(), user.ID, workspaceID, rbac.PermissionWorkspaceWrite, workspace.ProjectID)
		if err != nil || !hasAdmin {
			c.JSON(http.StatusConflict, gin.H{
				"errors": []gin.H{
					{
						"status": "409",
						"title":  "Conflict",
						"detail": "Workspace is locked by a different user",
					},
				},
			})
			return
		}
	}

	workspace.Locked = false
	workspace.LockedBy = nil
	workspace.LockedAt = nil
	workspace.LockedReason = ""

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

	// Reload to get updated workspace
	workspace, _ = h.workspaceRepo.GetByID(workspaceID)

	// Check if frontend wants simple format
	if c.Query("format") == "simple" {
		c.JSON(http.StatusOK, gin.H{
			"data": formatWorkspaceSimple(workspace),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatWorkspaceResponse(workspace, h.vcsConnectionRepo),
	})
}

// ForceUnlock force unlocks a workspace (admin only)
// POST /api/v2/workspaces/:id/actions/force-unlock
func (h *WorkspaceHandlerV2) ForceUnlock(c *gin.Context) {
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

	if !workspace.Locked {
		c.JSON(http.StatusConflict, gin.H{
			"errors": []gin.H{
				{
					"status": "409",
					"title":  "Conflict",
					"detail": "Workspace is already unlocked",
				},
			},
		})
		return
	}

	// Force unlock - no user check, requires admin permissions (handled by middleware/permissions)
	workspace.Locked = false
	workspace.LockedBy = nil
	workspace.LockedAt = nil
	workspace.LockedReason = ""

	if err := h.workspaceRepo.Update(workspace); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to force unlock workspace",
				},
			},
		})
		return
	}

	// Reload to get updated workspace
	workspace, _ = h.workspaceRepo.GetByID(workspaceID)

	// Check if frontend wants simple format
	if c.Query("format") == "simple" {
		c.JSON(http.StatusOK, gin.H{
			"data": formatWorkspaceSimple(workspace),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatWorkspaceResponse(workspace, h.vcsConnectionRepo),
	})
}

// GetByID gets a workspace by ID (internal API)
// GET /api/v2/terraform/workspaces/:id
func (h *WorkspaceHandlerV2) GetByID(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
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

	workspace, err := h.workspaceRepo.GetByID(id)
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

	// Check if frontend wants simple format
	if c.Query("format") == "simple" {
		c.JSON(http.StatusOK, gin.H{
			"data": formatWorkspaceSimple(workspace),
		})
		return
	}

	// Default: Format in TFE-compatible JSON:API format
	c.JSON(http.StatusOK, gin.H{
		"data": formatWorkspaceResponse(workspace, h.vcsConnectionRepo),
	})
}

// UpdateByID updates a workspace by its ID (TFE-compatible)
// PATCH /api/v2/workspaces/:id
// Used by go-tfe UpdateByID and tfe_workspace_settings resource
func (h *WorkspaceHandlerV2) UpdateByID(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid workspace ID"}},
		})
		return
	}

	workspace, err := h.workspaceRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Workspace not found"}},
		})
		return
	}

	var req UpdateWorkspaceRequestV2
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}},
		})
		return
	}

	attrs := req.Data.Attributes

	// Get the org for VCS connection resolution
	project, _ := h.projectRepo.GetByID(workspace.ProjectID)
	var org *models.Organization
	if project != nil {
		org, _ = h.orgRepo.GetByID(project.OrganizationID)
	}

	// TFE-compatible: Handle vcs-repo nested attribute (go-tfe sends this)
	if attrs.VCSRepo != nil && attrs.VCSRepo.Identifier != "" {
		if attrs.VCSRepository == "" {
			attrs.VCSRepository = attrs.VCSRepo.Identifier
		}
		if attrs.VCSBranch == "" && attrs.VCSRepo.Branch != "" {
			attrs.VCSBranch = attrs.VCSRepo.Branch
		}
		if attrs.VCSConnectionID == nil {
			switch {
			case attrs.VCSRepo.GHAInstallationID != "":
				conn, connErr := h.vcsConnectionRepo.GetByInstallationID(attrs.VCSRepo.GHAInstallationID)
				if connErr == nil {
					attrs.VCSConnectionID = &conn.ID
				}
			case attrs.VCSRepo.OAuthTokenID != "":
				parsedID, parseErr := uuid.Parse(attrs.VCSRepo.OAuthTokenID)
				if parseErr == nil {
					attrs.VCSConnectionID = &parsedID
				}
			case org != nil:
				conns, connErr := h.vcsConnectionRepo.ListByOrganization(org.ID)
				if connErr == nil && len(conns) > 0 {
					attrs.VCSConnectionID = &conns[0].ID
				}
			}
		}
	}

	// Update execution mode
	if attrs.ExecutionMode != "" {
		workspace.ExecutionMode = attrs.ExecutionMode
	}

	// Update agent pool ID
	if attrs.AgentPoolID != nil {
		if *attrs.AgentPoolID == "" {
			workspace.AgentPoolID = nil
		} else {
			poolID, parseErr := uuid.Parse(*attrs.AgentPoolID)
			if parseErr == nil {
				workspace.AgentPoolID = &poolID
			}
		}
	}

	// Update auto-apply
	if attrs.AutoApply != nil {
		workspace.AutoApply = *attrs.AutoApply
	}

	// Update description
	if attrs.Description != "" {
		workspace.Description = attrs.Description
	}

	// Update name
	if attrs.Name != "" && attrs.Name != workspace.Name {
		workspace.Name = attrs.Name
	}

	// Update Terraform version
	if attrs.TerraformVersion != "" {
		workspace.TerraformVersion = attrs.TerraformVersion
	}

	// Update working directory
	if attrs.WorkingDirectory != workspace.WorkingDirectory {
		workspace.WorkingDirectory = attrs.WorkingDirectory
	}

	// Update run timeout
	if attrs.RunTimeout != nil && *attrs.RunTimeout > 0 {
		workspace.RunTimeout = *attrs.RunTimeout
	}

	// Update VCS connection
	if attrs.VCSConnectionID != nil {
		workspace.VCSConnectionID = attrs.VCSConnectionID
	}

	// Update VCS repository
	if attrs.VCSRepository != "" {
		workspace.VCSRepository = attrs.VCSRepository
	}

	// Update VCS branch
	if attrs.VCSBranch != "" {
		workspace.VCSBranch = attrs.VCSBranch
	}

	// Update auto-apply-run-trigger
	if attrs.AutoApplyRunTrigger != nil {
		workspace.AutoApplyRunTrigger = *attrs.AutoApplyRunTrigger
	}

	// Update allow-destroy-plan
	if attrs.AllowDestroyPlan != nil {
		workspace.AllowDestroyPlan = *attrs.AllowDestroyPlan
	}

	// Update queue-all-runs
	if attrs.QueueAllRuns != nil {
		workspace.QueueAllRuns = *attrs.QueueAllRuns
	}

	// Update speculative-enabled
	if attrs.SpeculativeEnabled != nil {
		workspace.SpeculativeEnabled = *attrs.SpeculativeEnabled
	}

	// Update file-triggers-enabled
	if attrs.FileTriggersEnabled != nil {
		workspace.FileTriggersEnabled = *attrs.FileTriggersEnabled
	}

	// Update global-remote-state
	if attrs.GlobalRemoteState != nil {
		workspace.GlobalRemoteState = *attrs.GlobalRemoteState
	}

	// Update structured-run-output-enabled
	if attrs.StructuredRunOutputEnabled != nil {
		workspace.StructuredRunOutputEnabled = *attrs.StructuredRunOutputEnabled
	}

	// Update force-delete
	if attrs.ForceDelete != nil {
		workspace.ForceDelete = *attrs.ForceDelete
	}

	// Update assessments-enabled
	if attrs.AssessmentsEnabled != nil {
		workspace.AssessmentsEnabled = *attrs.AssessmentsEnabled
	}

	// Update trigger-prefixes
	if len(attrs.TriggerPrefixes) > 0 {
		if data, err := json.Marshal(attrs.TriggerPrefixes); err == nil {
			workspace.TriggerPrefixes = string(data)
		}
	}

	// Update trigger-patterns
	if len(attrs.TriggerPatterns) > 0 {
		if data, err := json.Marshal(attrs.TriggerPatterns); err == nil {
			workspace.TriggerPatterns = string(data)
		}
	}

	// Update tag-names
	if len(attrs.TagNames) > 0 {
		if data, err := json.Marshal(attrs.TagNames); err == nil {
			workspace.TagNames = string(data)
		}
	}

	// Update VCS sub-fields from vcs-repo
	if attrs.VCSRepo != nil {
		workspace.VCSIngressSubmodules = attrs.VCSRepo.IngressSubmodules
		if attrs.VCSRepo.TagsRegex != "" {
			workspace.VCSTagsRegex = attrs.VCSRepo.TagsRegex
		}
	}

	if err := h.workspaceRepo.Update(workspace); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to update workspace"}},
		})
		return
	}

	h.maybeRegisterADOWebhook(workspace.VCSConnectionID, workspace.VCSRepository)

	c.JSON(http.StatusOK, gin.H{
		"data": formatWorkspaceResponse(workspace, h.vcsConnectionRepo),
	})
}
