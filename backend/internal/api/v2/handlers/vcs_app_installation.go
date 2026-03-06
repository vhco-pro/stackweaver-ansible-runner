// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	terraform "github.com/iac-platform/backend/internal/api/v2/handlers/terraform"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/queue"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/vcs"
	"github.com/iac-platform/backend/internal/storage"
	"github.com/michielvha/logger"
)

// VCSAppInstallationHandlerV2 handles VCS App installation flow (GitHub, Azure DevOps)
type VCSAppInstallationHandlerV2 struct {
	vcsConnectionRepo  *repository.VCSConnectionRepository
	orgRepo            *repository.OrganizationRepository
	moduleRepo         *repository.ModuleRepository
	workspaceRepo      *repository.WorkspaceRepository
	runRepo            *repository.RunRepository
	configVersionRepo  *repository.ConfigurationVersionRepository
	inventoryRepo      *repository.AnsibleInventoryRepository
	playbookRepo       *repository.AnsiblePlaybookRepository
	webhookEventRepo   *repository.WebhookEventRepository // For recording webhook deliveries
	authService        *auth.Service
	githubAppManager   *vcs.GitHubAppManager
	azureDevOpsManager *vcs.AzureDevOpsManager
	vcsRegistry        *vcs.ProviderRegistry
	statusService      *vcs.GitHubStatusService      // For GitHub status checks
	adoStatusService   *vcs.AzureDevOpsStatusService // For Azure DevOps PR status checks
	registryPublisher  *RegistryPublishingHandler    // For tag push event handling
	storageClient      storage.Client
	ansibleQueue       queue.Queue // For inventory and playbook sync
}

func NewVCSAppInstallationHandlerV2(
	vcsConnectionRepo *repository.VCSConnectionRepository,
	orgRepo *repository.OrganizationRepository,
	moduleRepo *repository.ModuleRepository,
	workspaceRepo *repository.WorkspaceRepository,
	runRepo *repository.RunRepository,
	configVersionRepo *repository.ConfigurationVersionRepository,
	inventoryRepo *repository.AnsibleInventoryRepository,
	playbookRepo *repository.AnsiblePlaybookRepository,
	webhookEventRepo *repository.WebhookEventRepository,
	authService *auth.Service,
	githubAppManager *vcs.GitHubAppManager,
	azureDevOpsManager *vcs.AzureDevOpsManager,
	vcsRegistry *vcs.ProviderRegistry,
	registryPublisher *RegistryPublishingHandler,
	storageClient storage.Client,
	ansibleQueue queue.Queue,
) *VCSAppInstallationHandlerV2 {
	// Create status service from app manager if available
	var statusService *vcs.GitHubStatusService
	if githubAppManager != nil && githubAppManager.IsEnabled() {
		appService := githubAppManager.GetService()
		if appService != nil {
			statusService = vcs.NewGitHubStatusService(appService)
		}
	}

	// Create Azure DevOps status service if available
	var adoStatusService *vcs.AzureDevOpsStatusService
	if azureDevOpsManager != nil && azureDevOpsManager.IsEnabled() && vcsRegistry != nil {
		adoStatusService = vcs.NewAzureDevOpsStatusService(azureDevOpsManager, vcsRegistry.GetConnUpdater())
	}

	return &VCSAppInstallationHandlerV2{
		vcsConnectionRepo:  vcsConnectionRepo,
		orgRepo:            orgRepo,
		moduleRepo:         moduleRepo,
		workspaceRepo:      workspaceRepo,
		runRepo:            runRepo,
		configVersionRepo:  configVersionRepo,
		inventoryRepo:      inventoryRepo,
		playbookRepo:       playbookRepo,
		webhookEventRepo:   webhookEventRepo,
		authService:        authService,
		githubAppManager:   githubAppManager,
		azureDevOpsManager: azureDevOpsManager,
		vcsRegistry:        vcsRegistry,
		statusService:      statusService,
		adoStatusService:   adoStatusService,
		registryPublisher:  registryPublisher,
		storageClient:      storageClient,
		ansibleQueue:       ansibleQueue,
	}
}

// InitiateInstallation initiates the GitHub App installation flow
// Returns the installation URL for the frontend to redirect to
// GET /api/v2/organizations/:name/vcs-connections/github/install
func (h *VCSAppInstallationHandlerV2) InitiateInstallation(c *gin.Context) {
	orgName := c.Param("name")
	redirect := c.Query("redirect")
	returnPath := c.Query("return")

	// Verify organization exists
	_, err := h.orgRepo.GetByName(orgName)
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

	if h.githubAppManager == nil || !h.githubAppManager.IsEnabled() {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Configuration Error",
					"detail": "GitHub App is not configured. Please set GITHUB_APP_ID, GITHUB_APP_NAME, and GITHUB_APP_PRIVATE_KEY environment variables.",
				},
			},
		})
		return
	}

	// Generate state token to prevent CSRF
	// State format: "orgName|returnPath|uuid"
	escapedReturn := url.QueryEscape(returnPath)
	state := fmt.Sprintf("%s|%s|%s", orgName, escapedReturn, uuid.New().String())

	// GitHub App installation URL
	// Users will be redirected here to install the app on their organization
	installURL := fmt.Sprintf("https://github.com/apps/%s/installations/new?state=%s", h.githubAppManager.GetAppName(), state)
	// If a redirect URL is provided, include it so GitHub sends users back to our app
	if redirect != "" {
		installURL = installURL + "&redirect_url=" + redirect
	}

	// Return the installation URL as JSON (frontend will handle redirect)
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"install_url": installURL,
		},
	})
}

// GitHubSetupCallback is the GitHub App "Setup URL" endpoint.
// GitHub redirects here after a user installs or updates the GitHub App
// (including permission upgrades). This handler proxies the redirect to
// the correct frontend URL, which varies between Docker Compose (localhost:5173)
// and Kubernetes (the public ingress domain).
//
// Set STACKWEAVER_APP_URL to your public frontend origin (e.g. https://app.example.com).
// Defaults to http://localhost:5173 for Docker Compose.
//
// GitHub App Setup URL should be: <your-api-url>/api/v2/vcs-connections/github/setup
// GET /api/v2/vcs-connections/github/setup
func (h *VCSAppInstallationHandlerV2) GitHubSetupCallback(c *gin.Context) {
	appURL := os.Getenv("STACKWEAVER_APP_URL")
	if appURL == "" {
		appURL = "http://localhost:5173"
	}

	// Forward all query params GitHub sends (installation_id, setup_action, state, etc.)
	target := appURL + "/vcs/github/installed"
	if q := c.Request.URL.RawQuery; q != "" {
		target = target + "?" + q
	}

	c.Redirect(http.StatusFound, target)
}

// InitiateAzureDevOpsInstallation initiates the Azure DevOps OAuth2 installation flow
// Returns the authorization URL for the frontend to redirect to
// GET /api/v2/organizations/:name/vcs-connections/azure-devops/install
func (h *VCSAppInstallationHandlerV2) InitiateAzureDevOpsInstallation(c *gin.Context) {
	orgName := c.Param("name")
	adoOrg := c.Query("ado_org")    // Azure DevOps organization name
	returnPath := c.Query("return") // Where to send user after auth

	_, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}},
		})
		return
	}

	if h.azureDevOpsManager == nil || !h.azureDevOpsManager.IsEnabled() {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{{
				"status": "500",
				"title":  "Configuration Error",
				"detail": "Azure DevOps OAuth is not configured. Please set AZURE_DEVOPS_CLIENT_ID, AZURE_DEVOPS_CLIENT_SECRET, and AZURE_DEVOPS_REDIRECT_URI.",
			}},
		})
		return
	}

	// State format: "stackweaverOrg|adoOrg|returnPath|uuid"
	escapedReturn := url.QueryEscape(returnPath)
	state := fmt.Sprintf("%s|%s|%s|%s", orgName, adoOrg, escapedReturn, uuid.New().String())

	authURL := h.azureDevOpsManager.GetAuthorizationURL(state)
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{"auth_url": authURL},
	})
}

// CompleteAzureDevOpsInstallation handles the Azure DevOps OAuth2 callback
// POST /api/v2/vcs-connections/azure-devops/callback
func (h *VCSAppInstallationHandlerV2) CompleteAzureDevOpsInstallation(c *gin.Context) {
	code := c.Query("code")
	state := c.Query("state")

	if code == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Missing code parameter"}},
		})
		return
	}

	if h.azureDevOpsManager == nil || !h.azureDevOpsManager.IsEnabled() {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{{"status": "500", "title": "Configuration Error", "detail": "Azure DevOps OAuth is not configured"}},
		})
		return
	}

	// Decode state: "stackweaverOrg|adoOrg|returnPath|uuid"
	stateParts := strings.SplitN(state, "|", 4)
	if len(stateParts) < 2 {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid state parameter"}},
		})
		return
	}
	orgName := stateParts[0]
	adoOrgName := stateParts[1]

	if orgName == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid state: missing org name"}},
		})
		return
	}

	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}},
		})
		return
	}

	ctx := c.Request.Context()
	tokenResult, err := h.azureDevOpsManager.ExchangeCode(ctx, code)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": fmt.Sprintf("Failed to exchange authorization code: %v", err)}},
		})
		return
	}

	// Validate the token against Azure DevOps before saving the connection.
	// This call hits the global VSSPS profile endpoint first (which can trigger identity
	// materialization), then verifies access to the specified organization.
	adoProvider := vcs.NewAzureDevOpsProvider(h.azureDevOpsManager)
	if validationErr := adoProvider.ValidateTokenAndOrg(ctx, tokenResult.AccessToken, adoOrgName); validationErr != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"errors": []gin.H{{"status": "422", "title": "Azure DevOps Access Error", "detail": fmt.Sprintf("Token validated but org access failed: %v", validationErr)}},
		})
		return
	}

	var tokenExpiresAt *time.Time
	if tokenResult.ExpiresIn > 0 {
		t := time.Now().Add(time.Duration(tokenResult.ExpiresIn) * time.Second)
		tokenExpiresAt = &t
	}

	// Update existing ADO connection if one exists for this org
	existing, getErr := h.vcsConnectionRepo.GetByOrganizationAndProvider(org.ID, models.VCSProviderAzureDevOps)

	if getErr == nil && existing != nil && existing.ID != uuid.Nil {
		existing.AccessToken = tokenResult.AccessToken
		existing.RefreshToken = tokenResult.RefreshToken
		existing.TokenExpiresAt = tokenExpiresAt
		if adoOrgName != "" {
			existing.AccountName = adoOrgName
		}
		if err := h.vcsConnectionRepo.Update(existing); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to update VCS connection"}},
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"data": gin.H{
				"id":         existing.ID,
				"type":       "vcs-connections",
				"attributes": gin.H{"provider": existing.Provider, "account_name": existing.AccountName},
			},
		})
		return
	}

	connection := &models.VCSConnection{
		OrganizationID: org.ID,
		Provider:       models.VCSProviderAzureDevOps,
		AccessToken:    tokenResult.AccessToken,
		RefreshToken:   tokenResult.RefreshToken,
		TokenExpiresAt: tokenExpiresAt,
		AccountName:    adoOrgName,
		AccountType:    "organization",
	}
	if err := h.vcsConnectionRepo.Create(connection); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to create VCS connection"}},
		})
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"data": gin.H{
			"id":         connection.ID,
			"type":       "vcs-connections",
			"attributes": gin.H{"provider": connection.Provider, "account_name": connection.AccountName},
		},
	})
}

// HandleAzureDevOpsWebhook handles Azure DevOps Service Hook events (push and pull request)
// POST /api/v2/vcs-connections/azure-devops/webhook
func (h *VCSAppInstallationHandlerV2) HandleAzureDevOpsWebhook(c *gin.Context) {
	payload, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Failed to read webhook payload"}},
		})
		return
	}

	adoProvider := vcs.NewAzureDevOpsProvider(h.azureDevOpsManager)
	wp, err := adoProvider.ParseWebhookPayload(payload)
	if err != nil {
		logger.Errorf("Failed to parse Azure DevOps webhook payload: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Failed to parse webhook payload"}},
		})
		return
	}

	logger.Infof("Azure DevOps webhook - eventType=%s, repository=%s, branch=%s, commit=%s, PR=%d",
		wp.EventType, wp.Repository, wp.Branch, wp.Commit, wp.PRNumber)

	switch wp.EventType {
	case "push":
		h.handleAzureDevOpsPushEvent(c, wp, payload)
	case "pull_request":
		h.handleAzureDevOpsPullRequestEvent(c, wp, payload)
	default:
		h.recordWebhookEvent(wp.EventType, "azure_devops", wp.Repository, wp.Branch, wp.Commit, "ignored",
			fmt.Sprintf("Event type %q not handled", wp.EventType), http.StatusOK, string(payload))
		c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("Event type %q ignored", wp.EventType)})
	}
}

// handleAzureDevOpsPushEvent handles Azure DevOps git.push events — triggers plan-and-apply runs.
func (h *VCSAppInstallationHandlerV2) handleAzureDevOpsPushEvent(c *gin.Context, wp *vcs.WebhookPayload, payload []byte) {
	workspaces, err := h.workspaceRepo.FindByVCSRepositoryAndBranch(wp.Repository, wp.Branch)
	if err != nil {
		logger.Errorf("Error finding workspaces for %s/%s: %v", wp.Repository, wp.Branch, err)
		c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("No workspaces found: %v", err)})
		return
	}

	if len(workspaces) == 0 {
		h.recordWebhookEvent("push", "azure_devops", wp.Repository, wp.Branch, wp.Commit, "ignored",
			"No workspaces with AutoQueueRuns enabled", http.StatusOK, string(payload))
		c.JSON(http.StatusOK, gin.H{"message": "No workspaces found for this repository and branch"})
		return
	}

	filteredWorkspaces := make([]models.Workspace, 0, len(workspaces))
	for _, workspace := range workspaces {
		if len(wp.ChangedFiles) == 0 || h.isWorkspaceAffectedByFiles(workspace, wp.ChangedFiles) {
			filteredWorkspaces = append(filteredWorkspaces, workspace)
		} else {
			logger.Infof("Workspace %s skipped (no changed files in its path)", workspace.ID)
		}
	}

	if len(filteredWorkspaces) == 0 {
		h.recordWebhookEvent("push", "azure_devops", wp.Repository, wp.Branch, wp.Commit, "ignored",
			fmt.Sprintf("No workspaces match changed files (%d exist)", len(workspaces)), http.StatusOK, string(payload))
		c.JSON(http.StatusOK, gin.H{"message": "No workspaces match the changed files"})
		return
	}

	commitHash := wp.Commit
	branchName := wp.Branch
	committer := wp.Committer

	for _, workspace := range filteredWorkspaces {
		go func(ws models.Workspace) {
			defer func() {
				if r := recover(); r != nil {
					logger.Errorf("PANIC in ADO webhook goroutine for workspace %s: %v", ws.ID, r)
				}
			}()

			if commitHash == "" || len(commitHash) < 7 {
				logger.Warnf("Invalid commit hash for workspace %s, skipping", ws.ID)
				return
			}

			ctx := context.Background()

			var vcsConn *models.VCSConnection
			if ws.VCSConnectionID != nil {
				var connErr error
				vcsConn, connErr = h.vcsConnectionRepo.GetByID(*ws.VCSConnectionID)
				if connErr != nil {
					logger.Errorf("Failed to get VCS connection for workspace %s: %v", ws.ID, connErr)
					return
				}
			}

			var cloneURL string
			if vcsConn != nil && h.vcsRegistry != nil {
				if provider, provErr := h.vcsRegistry.GetProvider(vcsConn); provErr == nil {
					freshToken, _ := provider.GetFreshToken(ctx, vcsConn)
					cloneURL = provider.BuildCloneURL(vcsConn, freshToken, ws.VCSRepository)
				} else {
					logger.Warnf("Failed to get provider for workspace %s: %v", ws.ID, provErr)
				}
			}
			if cloneURL == "" {
				logger.Errorf("Cannot build clone URL for workspace %s", ws.ID)
				return
			}

			tempDir, tempErr := os.MkdirTemp("", fmt.Sprintf("workspace-ado-%s-*", ws.ID))
			if tempErr != nil {
				logger.Errorf("Failed to create temp dir for workspace %s: %v", ws.ID, tempErr)
				return
			}
			defer func() { _ = os.RemoveAll(tempDir) }()

			cmd := exec.CommandContext(ctx, "git", "clone", cloneURL, tempDir) //nolint:gosec // intentional
			if err := cmd.Run(); err != nil {
				logger.Errorf("Failed to clone repository for workspace %s: %v", ws.ID, err)
				return
			}
			cmd = exec.CommandContext(ctx, "git", "checkout", commitHash) //nolint:gosec // intentional
			cmd.Dir = tempDir
			if err := cmd.Run(); err != nil {
				logger.Errorf("Failed to checkout commit %s for workspace %s: %v", commitHash, ws.ID, err)
				return
			}

			commitShort := commitHash[:7]
			tarballPath := filepath.Join(os.TempDir(), fmt.Sprintf("workspace-ado-%s-%s.tar.gz", ws.ID, commitShort))
			defer func() {
				if err := os.Remove(tarballPath); err != nil && !os.IsNotExist(err) {
					logger.Warnf("Failed to remove tarball %s: %v", tarballPath, err)
				}
			}()

			if err := h.createTarball(tempDir, tarballPath); err != nil {
				logger.Errorf("Failed to create tarball for workspace %s: %v", ws.ID, err)
				return
			}

			existingCVs, checkErr := h.configVersionRepo.GetByWorkspaceID(ws.ID)
			if checkErr == nil {
				for _, cv := range existingCVs {
					if cv.CommitHash == commitHash && cv.Status != models.ConfigurationVersionStatusErrored {
						logger.Infof("Config version for commit %s already exists, skipping workspace %s", commitHash, ws.ID)
						return
					}
				}
			}

			configVersion := &models.ConfigurationVersion{
				WorkspaceID:   ws.ID,
				Status:        models.ConfigurationVersionStatusPending,
				Source:        "tfe-vcs",
				AutoQueueRuns: true,
				Speculative:   false,
				CommitHash:    commitHash,
				Committer:     committer,
			}
			if err := h.configVersionRepo.Create(configVersion); err != nil {
				errStr := strings.ToLower(err.Error())
				if strings.Contains(errStr, "duplicate") || strings.Contains(errStr, "unique constraint") {
					logger.Infof("Config version for commit %s already exists (duplicate), skipping", commitHash)
					return
				}
				logger.Errorf("Failed to create config version for workspace %s: %v", ws.ID, err)
				return
			}

			tarballFile, fileErr := os.Open(tarballPath) //nolint:gosec // validated
			if fileErr != nil {
				logger.Errorf("Failed to open tarball for workspace %s: %v", ws.ID, fileErr)
				return
			}
			defer func() { _ = tarballFile.Close() }()

			if h.storageClient == nil {
				configVersion.Status = models.ConfigurationVersionStatusErrored
				configVersion.ErrorMessage = "Storage client not initialized"
				_ = h.configVersionRepo.Update(configVersion)
				return
			}

			storageKey := fmt.Sprintf("configuration-versions/%s/config.tar.gz", configVersion.ID)
			if err := h.storageClient.PutStream(ctx, storageKey, tarballFile); err != nil {
				logger.Errorf("Failed to upload config for workspace %s: %v", ws.ID, err)
				configVersion.Status = models.ConfigurationVersionStatusErrored
				configVersion.ErrorMessage = fmt.Sprintf("Failed to upload: %v", err)
				_ = h.configVersionRepo.Update(configVersion)
				return
			}

			configVersion.Status = models.ConfigurationVersionStatusUploaded
			if err := h.configVersionRepo.Update(configVersion); err != nil {
				logger.Errorf("Failed to update config version status for workspace %s: %v", ws.ID, err)
				return
			}

			terraform.AutoCancelConflictingRuns(h.runRepo, ws.ID, models.RunOperationPlanAndApply)

			run := &models.Run{
				WorkspaceID:            ws.ID,
				ConfigurationVersionID: &configVersion.ID,
				Status:                 models.RunStatusPending,
				Operation:              models.RunOperationPlanAndApply,
				AutoApplyAfterPlan:     true,
			}
			if err := h.runRepo.Create(run); err != nil {
				logger.Errorf("Failed to create run for workspace %s: %v", ws.ID, err)
				return
			}
			logger.Infof("Created run %s for workspace %s (ADO push, commit %s)", run.ID, ws.ID, commitHash)
		}(workspace)
	}

	// Sync inventories and playbooks for matching repo+branch
	inventories, _ := h.inventoryRepo.FindByVCSRepositoryAndBranch(wp.Repository, branchName)
	for _, inventory := range inventories {
		inv := inventory
		if h.ansibleQueue != nil {
			go func() {
				syncMsg := map[string]any{"inventory_id": inv.ID.String()}
				if err := h.ansibleQueue.Enqueue(context.Background(), "ansible_sync", syncMsg); err != nil {
					logger.Errorf("Error queuing sync for inventory %s: %v", inv.ID, err)
				}
			}()
		}
	}
	if h.playbookRepo != nil {
		playbooks, _ := h.playbookRepo.ListByVCSRepositoryAndBranch(wp.Repository, branchName)
		for _, playbook := range playbooks {
			pb := playbook
			if h.ansibleQueue != nil {
				go func() {
					syncMsg := map[string]any{"playbook_id": pb.ID.String()}
					if err := h.ansibleQueue.Enqueue(context.Background(), "ansible_sync", syncMsg); err != nil {
						logger.Errorf("Error queuing sync for playbook %s: %v", pb.ID, err)
					}
				}()
			}
		}
	}

	var triggeredNames []string
	for _, ws := range filteredWorkspaces {
		triggeredNames = append(triggeredNames, ws.Name)
	}
	h.recordWebhookEvent("push", "azure_devops", wp.Repository, branchName, commitHash, "success",
		fmt.Sprintf("%d workspace(s) triggered: %s", len(filteredWorkspaces), strings.Join(triggeredNames, ", ")),
		http.StatusOK, string(payload))

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("Azure DevOps push event processed: %d workspace(s) queued", len(filteredWorkspaces)),
	})
}

// handleAzureDevOpsPullRequestEvent handles Azure DevOps git.pullrequest.* events — triggers speculative plan runs.
func (h *VCSAppInstallationHandlerV2) handleAzureDevOpsPullRequestEvent(c *gin.Context, wp *vcs.WebhookPayload, payload []byte) {
	if wp.BaseBranch == "" || wp.HeadBranch == "" {
		logger.Infof("Azure DevOps PR event missing base/head branch, ignoring")
		c.JSON(http.StatusOK, gin.H{"message": "PR event missing branch info, ignored"})
		return
	}

	logger.Infof("Azure DevOps PR #%d: head=%s -> base=%s, commit=%s, repo=%s",
		wp.PRNumber, wp.HeadBranch, wp.BaseBranch, wp.Commit, wp.Repository)

	// Find workspaces that match the target (base) branch with speculative plans enabled
	workspaces, err := h.workspaceRepo.FindByVCSRepositoryAndBranch(wp.Repository, wp.BaseBranch)
	if err != nil {
		logger.Errorf("Error finding workspaces for %s/%s: %v", wp.Repository, wp.BaseBranch, err)
		c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("No workspaces found: %v", err)})
		return
	}

	// Filter to workspaces with speculative plans enabled
	filteredWorkspaces := make([]models.Workspace, 0, len(workspaces))
	for _, ws := range workspaces {
		if ws.SpeculativeEnabled {
			filteredWorkspaces = append(filteredWorkspaces, ws)
		}
	}

	if len(filteredWorkspaces) == 0 {
		h.recordWebhookEvent("pull_request", "azure_devops", wp.Repository, wp.BaseBranch, wp.Commit, "ignored",
			"No workspaces with speculative plans enabled", http.StatusOK, string(payload))
		c.JSON(http.StatusOK, gin.H{"message": "No workspaces with speculative plans enabled"})
		return
	}

	// Path-based filtering using changed files (if available)
	if len(wp.ChangedFiles) > 0 {
		pathFiltered := make([]models.Workspace, 0, len(filteredWorkspaces))
		for _, ws := range filteredWorkspaces {
			if h.isWorkspaceAffectedByFiles(ws, wp.ChangedFiles) {
				pathFiltered = append(pathFiltered, ws)
			}
		}
		if len(pathFiltered) == 0 {
			h.recordWebhookEvent("pull_request", "azure_devops", wp.Repository, wp.BaseBranch, wp.Commit, "ignored",
				fmt.Sprintf("No workspaces match changed files (%d with speculative enabled)", len(filteredWorkspaces)),
				http.StatusOK, string(payload))
			c.JSON(http.StatusOK, gin.H{"message": "No workspaces match the changed files"})
			return
		}
		filteredWorkspaces = pathFiltered
	}

	logger.Infof("Azure DevOps PR #%d: triggering speculative plans for %d workspace(s)", wp.PRNumber, len(filteredWorkspaces))

	commitHash := wp.Commit

	for _, workspace := range filteredWorkspaces {
		go func(ws models.Workspace) {
			defer func() {
				if r := recover(); r != nil {
					logger.Errorf("PANIC in ADO PR webhook goroutine for workspace %s: %v", ws.ID, r)
				}
			}()

			if commitHash == "" || len(commitHash) < 7 {
				logger.Warnf("Invalid commit hash for workspace %s, skipping", ws.ID)
				return
			}

			ctx := context.Background()

			var vcsConn *models.VCSConnection
			if ws.VCSConnectionID != nil {
				var connErr error
				vcsConn, connErr = h.vcsConnectionRepo.GetByID(*ws.VCSConnectionID)
				if connErr != nil {
					logger.Errorf("Failed to get VCS connection for workspace %s: %v", ws.ID, connErr)
					return
				}
			}

			var cloneURL string
			if vcsConn != nil && h.vcsRegistry != nil {
				if provider, provErr := h.vcsRegistry.GetProvider(vcsConn); provErr == nil {
					freshToken, _ := provider.GetFreshToken(ctx, vcsConn)
					cloneURL = provider.BuildCloneURL(vcsConn, freshToken, ws.VCSRepository)
				} else {
					logger.Warnf("Failed to get provider for workspace %s: %v", ws.ID, provErr)
				}
			}
			if cloneURL == "" {
				logger.Errorf("Cannot build clone URL for workspace %s", ws.ID)
				return
			}

			tempDir, tempErr := os.MkdirTemp("", fmt.Sprintf("workspace-ado-pr-%s-*", ws.ID))
			if tempErr != nil {
				logger.Errorf("Failed to create temp dir for workspace %s: %v", ws.ID, tempErr)
				return
			}
			defer func() { _ = os.RemoveAll(tempDir) }()

			// Clone and checkout the PR head commit
			cmd := exec.CommandContext(ctx, "git", "clone", cloneURL, tempDir) //nolint:gosec // intentional
			if err := cmd.Run(); err != nil {
				logger.Errorf("Failed to clone repository for workspace %s: %v", ws.ID, err)
				return
			}
			cmd = exec.CommandContext(ctx, "git", "checkout", commitHash) //nolint:gosec // intentional
			cmd.Dir = tempDir
			if err := cmd.Run(); err != nil {
				logger.Errorf("Failed to checkout commit %s for workspace %s: %v", commitHash, ws.ID, err)
				return
			}

			commitShort := commitHash[:7]
			tarballPath := filepath.Join(os.TempDir(), fmt.Sprintf("workspace-ado-pr-%s-%s.tar.gz", ws.ID, commitShort))
			defer func() {
				if err := os.Remove(tarballPath); err != nil && !os.IsNotExist(err) {
					logger.Warnf("Failed to remove tarball %s: %v", tarballPath, err)
				}
			}()

			if err := h.createTarball(tempDir, tarballPath); err != nil {
				logger.Errorf("Failed to create tarball for workspace %s: %v", ws.ID, err)
				return
			}

			// Create speculative configuration version
			configVersion := &models.ConfigurationVersion{
				WorkspaceID:   ws.ID,
				Status:        models.ConfigurationVersionStatusPending,
				Source:        "tfe-vcs",
				AutoQueueRuns: true,
				Speculative:   true,
				CommitHash:    commitHash,
				Committer:     wp.Committer,
				PRNumber:      wp.PRNumber,
				SourceBranch:  wp.HeadBranch,
			}
			if err := h.configVersionRepo.Create(configVersion); err != nil {
				errStr := strings.ToLower(err.Error())
				if strings.Contains(errStr, "duplicate") || strings.Contains(errStr, "unique constraint") {
					logger.Infof("Config version for commit %s already exists (duplicate), skipping", commitHash)
					return
				}
				logger.Errorf("Failed to create config version for workspace %s: %v", ws.ID, err)
				return
			}

			tarballFile, fileErr := os.Open(tarballPath) //nolint:gosec // validated
			if fileErr != nil {
				logger.Errorf("Failed to open tarball for workspace %s: %v", ws.ID, fileErr)
				return
			}
			defer func() { _ = tarballFile.Close() }()

			if h.storageClient == nil {
				configVersion.Status = models.ConfigurationVersionStatusErrored
				configVersion.ErrorMessage = "Storage client not initialized"
				_ = h.configVersionRepo.Update(configVersion)
				return
			}

			storageKey := fmt.Sprintf("configuration-versions/%s/config.tar.gz", configVersion.ID)
			if err := h.storageClient.PutStream(ctx, storageKey, tarballFile); err != nil {
				logger.Errorf("Failed to upload config for workspace %s: %v", ws.ID, err)
				configVersion.Status = models.ConfigurationVersionStatusErrored
				configVersion.ErrorMessage = fmt.Sprintf("Failed to upload: %v", err)
				_ = h.configVersionRepo.Update(configVersion)
				return
			}

			configVersion.Status = models.ConfigurationVersionStatusUploaded
			if err := h.configVersionRepo.Update(configVersion); err != nil {
				logger.Errorf("Failed to update config version status for workspace %s: %v", ws.ID, err)
				return
			}

			// Create plan-only (speculative) run
			run := &models.Run{
				WorkspaceID:            ws.ID,
				ConfigurationVersionID: &configVersion.ID,
				Status:                 models.RunStatusPending,
				Operation:              models.RunOperationPlanOnly,
				AutoApplyAfterPlan:     false,
			}
			if err := h.runRepo.Create(run); err != nil {
				logger.Errorf("Failed to create speculative run for workspace %s: %v", ws.ID, err)
				return
			}
			logger.Infof("Created speculative run %s for workspace %s (ADO PR #%d, commit %s)",
				run.ID, ws.ID, wp.PRNumber, commitHash)

			// Create Azure DevOps PR status check (pending)
			if h.adoStatusService != nil && vcsConn != nil && wp.PRNumber > 0 {
				repoPath := ws.VCSRepository
				if repoPath == "" {
					repoPath = wp.Repository
				}
				repoParts := strings.SplitN(repoPath, "/", 2)
				if len(repoParts) == 2 {
					// Generate target URL for the status check
					baseURL := os.Getenv("STACKWEAVER_BASE_URL")
					if baseURL == "" {
						baseURL = "http://localhost:5173"
					}
					workspaceForURL, urlErr := h.workspaceRepo.GetByID(ws.ID)
					var targetURL string
					if urlErr == nil && workspaceForURL.Project.Organization.Name != "" {
						targetURL = fmt.Sprintf("%s/app/%s/workspaces/%s/runs/%s", baseURL, workspaceForURL.Project.Organization.Name, workspaceForURL.Name, run.ID)
					} else {
						targetURL = fmt.Sprintf("%s/workspaces/%s/runs/%s", baseURL, ws.ID, run.ID)
					}

					statusContext := fmt.Sprintf("terraform-plan/%s", ws.Name)
					token, tokenErr := func() (string, error) {
						if h.vcsRegistry != nil {
							if provider, pErr := h.vcsRegistry.GetProvider(vcsConn); pErr == nil {
								return provider.GetFreshToken(ctx, vcsConn)
							}
						}
						return vcsConn.AccessToken, nil
					}()
					if tokenErr == nil {
						err = h.adoStatusService.CreateOrUpdatePRStatus(
							ctx,
							token,
							vcsConn.AccountName,
							repoParts[0],
							repoParts[1],
							wp.PRNumber,
							vcs.StatusStatePending,
							statusContext,
							"Terraform plan is queued",
							targetURL,
						)
						if err != nil {
							logger.Errorf("Failed to create ADO PR status for run %s: %v", run.ID, err)
						} else {
							logger.Infof("Created ADO PR status for run %s (PR #%d, commit %s)", run.ID, wp.PRNumber, commitHash)
						}
					}
				}
			}
		}(workspace)
	}

	var triggeredNames []string
	for _, ws := range filteredWorkspaces {
		triggeredNames = append(triggeredNames, ws.Name)
	}
	h.recordWebhookEvent("pull_request", "azure_devops", wp.Repository, wp.BaseBranch, commitHash, "success",
		fmt.Sprintf("PR #%d: %d workspace(s) triggered: %s", wp.PRNumber, len(filteredWorkspaces), strings.Join(triggeredNames, ", ")),
		http.StatusOK, string(payload))

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("Azure DevOps PR event processed: %d workspace(s) queued for speculative plans", len(filteredWorkspaces)),
	})
}

// recordWebhookEvent records a webhook event for debugging and auditing
func (h *VCSAppInstallationHandlerV2) recordWebhookEvent(eventType, provider, repoFullName, branch, commit, status, message string, responseCode int, payload string) {
	if h.webhookEventRepo == nil {
		return
	}

	// Try to determine organization from repository
	var orgID *uuid.UUID
	if repoFullName != "" {
		// Try to find any workspace matching this repository (regardless of branch or auto_queue_runs)
		workspaces, err := h.workspaceRepo.FindByVCSRepository(repoFullName)
		if err == nil && len(workspaces) > 0 {
			// Get organization from the first workspace's project
			if workspaces[0].Project.Organization.ID != uuid.Nil {
				id := workspaces[0].Project.Organization.ID
				orgID = &id
			}
		}
		// If no workspace found, try modules
		if orgID == nil && h.moduleRepo != nil {
			modules, err := h.moduleRepo.FindByVCSRepository(repoFullName)
			if err == nil && len(modules) > 0 && modules[0].OrganizationID != uuid.Nil {
				id := modules[0].OrganizationID
				orgID = &id
			}
		}
	}

	// If we couldn't find an org, just log - event will be recorded without org
	if orgID == nil {
		logger.Infof("Could not determine organization for repository %s, recording without org", repoFullName)
	}

	event := &models.WebhookEvent{
		OrganizationID: orgID,
		EventType:      eventType,
		Provider:       provider,
		Repository:     repoFullName,
		Branch:         branch,
		Commit:         commit,
		Status:         status,
		ResponseCode:   responseCode,
		Message:        message,
		Payload:        payload,
		DeliveredAt:    time.Now(),
	}

	if err := h.webhookEventRepo.Create(event); err != nil {
		logger.Errorf("Failed to record webhook event: %v", err)
	}
}

// HandleInstallationWebhook handles GitHub App installation webhook events
// POST /api/v2/vcs-connections/github/webhook
func (h *VCSAppInstallationHandlerV2) HandleInstallationWebhook(c *gin.Context) {
	logger.Infof("Received webhook request - Method: %s, Path: %s", c.Request.Method, c.Request.URL.Path)

	// Verify webhook signature
	payload, err := c.GetRawData()
	if err != nil {
		logger.Errorf("Failed to read webhook payload: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Failed to read webhook payload",
				},
			},
		})
		return
	}

	logger.Infof("Received payload (size: %d bytes)", len(payload))

	// TODO: Verify GitHub webhook signature
	// signature := c.GetHeader("X-Hub-Signature-256")
	// if !verifyWebhookSignature(payload, signature, h.githubWebhookSecret) {
	// 	c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid signature"})
	// 	return
	// }

	// Parse webhook event
	eventType := c.GetHeader("X-GitHub-Event")
	logger.Infof("Event type: %s", eventType)

	// Handle push events (both tag pushes for modules and branch pushes for workspaces)
	if eventType == "push" {
		// First check if it's a tag push (for module publishing)
		var pushEvent struct {
			Ref string `json:"ref"`
		}
		if err := json.Unmarshal(payload, &pushEvent); err == nil {
			if strings.HasPrefix(pushEvent.Ref, "refs/tags/") {
				// Tag push - handle module publishing
				h.handlePushEvent(c, payload)
				return
			} else if strings.HasPrefix(pushEvent.Ref, "refs/heads/") {
				// Branch push - handle workspace runs
				h.handleBranchPushEvent(c, payload)
				return
			}
		}
		// Fallback to old handler for tag pushes
		h.handlePushEvent(c, payload)
		return
	}

	// Handle pull request events (for speculative plans and status checks)
	if eventType == "pull_request" {
		h.handlePullRequestEvent(c, payload)
		return
	}

	if eventType != "installation" && eventType != "installation_repositories" {
		// Ignore other event types
		c.JSON(http.StatusOK, gin.H{"message": "Event ignored"})
		return
	}

	var installationEvent struct {
		Action       string `json:"action"`
		Installation struct {
			ID      int64 `json:"id"`
			Account struct {
				Login string `json:"login"`
				Type  string `json:"type"` // "User" or "Organization"
			} `json:"account"`
			RepositorySelection string `json:"repository_selection"` // "all" or "selected"
			Repositories        []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"repositories,omitempty"`
		} `json:"installation"`
		RepositoriesAdded []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"repositories_added,omitempty"`
		RepositoriesRemoved []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"repositories_removed,omitempty"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
	}

	if err := json.Unmarshal(payload, &installationEvent); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Failed to parse webhook payload",
				},
			},
		})
		return
	}

	installationID := strconv.FormatInt(installationEvent.Installation.ID, 10)
	accountName := installationEvent.Installation.Account.Login
	accountType := "user"
	if installationEvent.Installation.Account.Type == "Organization" {
		accountType = "organization"
	}

	// Handle different installation events
	switch installationEvent.Action {
	case "created":
		// AUTOMATIC: Create VCS connection when installation is created (like Terraform Enterprise)
		// Try to find organization by account name (GitHub org/user name matches our org name)
		org, err := h.orgRepo.GetByName(accountName)
		if err != nil {
			// Organization not found - this is OK, user can manually connect later
			// But we still return success so GitHub knows we processed the webhook
			c.Status(http.StatusOK)
			return
		}

		// Check if connection already exists
		existing, _ := h.vcsConnectionRepo.GetByOrganizationAndProvider(org.ID, models.VCSProviderGitHub)
		if existing != nil {
			// Update existing connection with new installation ID
			existing.InstallationID = installationID
			existing.AccountName = accountName
			existing.AccountType = accountType
			if err := h.vcsConnectionRepo.Update(existing); err != nil {
				c.Status(http.StatusOK) // Still return OK to GitHub
				return
			}
			c.Status(http.StatusOK)
			return
		}

		// Create new VCS connection automatically
		connection := &models.VCSConnection{
			OrganizationID: org.ID,
			Provider:       models.VCSProviderGitHub,
			InstallationID: installationID,
			AccountName:    accountName,
			AccountType:    accountType,
		}

		if err := h.vcsConnectionRepo.Create(connection); err != nil {
			c.Status(http.StatusOK) // Still return OK to GitHub
			return
		}

		c.Status(http.StatusOK)

	case "deleted":
		// Installation removed - delete VCS connection
		connections, _ := h.vcsConnectionRepo.ListByProvider(models.VCSProviderGitHub)
		for _, conn := range connections {
			if conn.InstallationID == installationID {
				_ = h.vcsConnectionRepo.Delete(conn.ID)
				break
			}
		}
		c.JSON(http.StatusOK, gin.H{"message": "Installation deleted"})

	case "suspend":
		// Installation suspended - mark as inactive
		connections, _ := h.vcsConnectionRepo.ListByProvider(models.VCSProviderGitHub)
		for _, conn := range connections {
			if conn.InstallationID == installationID {
				// TODO: Add status field to mark as suspended
				break
			}
		}
		c.JSON(http.StatusOK, gin.H{"message": "Installation suspended"})

	case "unsuspend":
		// Installation unsuspended - mark as active
		connections, _ := h.vcsConnectionRepo.ListByProvider(models.VCSProviderGitHub)
		for _, conn := range connections {
			if conn.InstallationID == installationID {
				// TODO: Add status field to mark as active
				break
			}
		}
		c.JSON(http.StatusOK, gin.H{"message": "Installation unsuspended"})

	default:
		c.JSON(http.StatusOK, gin.H{"message": "Event processed"})
	}
}

// CreateConnectionFromInstallation creates a VCS connection from an installation
// This is called after user installs the app and we receive the webhook
// POST /api/v2/organizations/:name/vcs-connections/github/installations/:installation_id
func (h *VCSAppInstallationHandlerV2) CreateConnectionFromInstallation(c *gin.Context) {
	orgName := c.Param("name")
	installationID := c.Param("installation_id")

	// Get organization
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

	if h.githubAppManager == nil || !h.githubAppManager.IsEnabled() {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Configuration Error",
					"detail": "GitHub App is not configured.",
				},
			},
		})
		return
	}

	// Get installation info from GitHub
	ctx := context.Background()
	githubService := h.githubAppManager.GetService()
	installation, err := githubService.GetInstallation(ctx, installationID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": fmt.Sprintf("Failed to get installation info: %v", err),
				},
			},
		})
		return
	}

	// Check if connection already exists
	existing, getErr := h.vcsConnectionRepo.GetByOrganizationAndProvider(org.ID, models.VCSProviderGitHub)
	if getErr == nil && existing != nil && existing.ID != uuid.Nil {
		// Update existing connection
		existing.InstallationID = installationID
		existing.AccountName = installation.AccountName
		existing.AccountType = installation.AccountType
		if err := h.vcsConnectionRepo.Update(existing); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{
					{
						"status": "500",
						"title":  "Internal Server Error",
						"detail": "Failed to update VCS connection",
					},
				},
			})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"data": gin.H{
				"id":   existing.ID,
				"type": "vcs-connections",
				"attributes": gin.H{
					"provider":     existing.Provider,
					"account_name": existing.AccountName,
					"account_type": existing.AccountType,
				},
			},
		})
		return
	}

	// Create new connection
	connection := &models.VCSConnection{
		OrganizationID: org.ID,
		Provider:       models.VCSProviderGitHub,
		InstallationID: installationID,
		AccountName:    installation.AccountName,
		AccountType:    installation.AccountType,
	}

	if err := h.vcsConnectionRepo.Create(connection); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to create VCS connection",
				},
			},
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"data": gin.H{
			"id":   connection.ID,
			"type": "vcs-connections",
			"attributes": gin.H{
				"provider":     connection.Provider,
				"account_name": connection.AccountName,
				"account_type": connection.AccountType,
			},
		},
	})
}

// handlePushEvent handles GitHub push events for tag-based module publishing
func (h *VCSAppInstallationHandlerV2) handlePushEvent(c *gin.Context, payload []byte) {
	if h.registryPublisher == nil || h.moduleRepo == nil {
		logger.Infof("Registry publishing not configured (registryPublisher=%v, moduleRepo=%v)", h.registryPublisher != nil, h.moduleRepo != nil)
		c.JSON(http.StatusOK, gin.H{"message": "Registry publishing not configured"})
		return
	}

	var pushEvent struct {
		Ref        string `json:"ref"`
		RefType    string `json:"ref_type"`
		Repository struct {
			FullName string `json:"full_name"`
			ID       int64  `json:"id"`
		} `json:"repository"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}

	if err := json.Unmarshal(payload, &pushEvent); err != nil {
		logger.Errorf("Failed to parse push event: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Failed to parse push event"}},
		})
		return
	}

	logger.Infof("Received push event - ref=%s, repository=%s", pushEvent.Ref, pushEvent.Repository.FullName)

	// Only process tag push events
	if !strings.HasPrefix(pushEvent.Ref, "refs/tags/") {
		logger.Infof("Not a tag push event (ref=%s)", pushEvent.Ref)
		c.JSON(http.StatusOK, gin.H{"message": "Not a tag push event"})
		return
	}

	// Extract tag name
	tagName := strings.TrimPrefix(pushEvent.Ref, "refs/tags/")
	repositoryFullName := pushEvent.Repository.FullName

	logger.Infof("Processing tag push - tag=%s, repository=%s", tagName, repositoryFullName)

	// Find modules connected to this repository with auto-publish enabled
	modules, err := h.moduleRepo.FindByVCSRepository(repositoryFullName)
	if err != nil {
		logger.Errorf("Error finding modules for repository %s: %v", repositoryFullName, err)
		c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("No modules found for repository: %v", err)})
		return
	}

	if len(modules) == 0 {
		logger.Infof("No modules found for repository %s (auto-publish enabled)", repositoryFullName)
		c.JSON(http.StatusOK, gin.H{"message": "No modules found for repository"})
		return
	}

	logger.Infof("Found %d module(s) for repository %s", len(modules), repositoryFullName)

	// Process each module
	successCount := 0
	for _, module := range modules {
		if !module.AutoPublishTags {
			logger.Infof("Module %s/%s/%s has auto-publish disabled, skipping", module.Organization.Name, module.Name, module.Provider)
			continue
		}

		logger.Infof("Publishing module %s/%s/%s version from tag %s", module.Organization.Name, module.Name, module.Provider, tagName)

		// Publish version from Git tag
		if err := h.registryPublisher.PublishFromGitTag(
			c.Request.Context(),
			module.ID,
			tagName,
			repositoryFullName,
		); err != nil {
			// Log error but continue processing other modules
			logger.Errorf("Failed to publish module %s/%s/%s from tag %s: %v", module.Organization.Name, module.Name, module.Provider, tagName, err)
			continue
		}

		successCount++
		logger.Infof("Successfully published module %s/%s/%s version from tag %s", module.Organization.Name, module.Name, module.Provider, tagName)
	}

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("Tag push event processed: %d module(s) published", successCount),
	})
}

// handleBranchPushEvent handles GitHub branch push events for workspace runs
// TFE-compatible: Creates plan runs (and apply runs if auto-apply enabled) when code is pushed to the default branch
func (h *VCSAppInstallationHandlerV2) handleBranchPushEvent(c *gin.Context, payload []byte) {
	var pushEvent struct {
		Ref        string `json:"ref"`
		After      string `json:"after"` // Commit SHA
		Repository struct {
			FullName string `json:"full_name"`
			ID       int64  `json:"id"`
		} `json:"repository"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
		Commits []struct {
			ID       string   `json:"id"`
			Message  string   `json:"message"`
			Added    []string `json:"added"`
			Removed  []string `json:"removed"`
			Modified []string `json:"modified"`
			Author   struct {
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"author"`
		} `json:"commits"`
		Pusher struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"pusher"`
	}

	if err := json.Unmarshal(payload, &pushEvent); err != nil {
		logger.Errorf("Failed to parse branch push event: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Failed to parse push event",
				},
			},
		})
		return
	}

	// Extract branch name from ref (e.g., "refs/heads/main" -> "main")
	branchName := strings.TrimPrefix(pushEvent.Ref, "refs/heads/")
	repositoryFullName := pushEvent.Repository.FullName
	commitHash := pushEvent.After
	committer := ""
	if len(pushEvent.Commits) > 0 {
		// Use the last commit's author as committer
		lastCommit := pushEvent.Commits[len(pushEvent.Commits)-1]
		committer = fmt.Sprintf("%s <%s>", lastCommit.Author.Name, lastCommit.Author.Email)
	} else if pushEvent.Pusher.Email != "" {
		committer = fmt.Sprintf("%s <%s>", pushEvent.Pusher.Name, pushEvent.Pusher.Email)
	}

	logger.Infof("Received branch push event - ref=%s, branch=%s, repository=%s, commit=%s", pushEvent.Ref, branchName, repositoryFullName, commitHash)

	// Find workspaces that match this repository and branch with AutoQueueRuns enabled
	workspaces, err := h.workspaceRepo.FindByVCSRepositoryAndBranch(repositoryFullName, branchName)
	if err != nil {
		logger.Errorf("Error finding workspaces for repository %s, branch %s: %v", repositoryFullName, branchName, err)
		c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("No workspaces found or error: %v", err)})
		return
	}

	if len(workspaces) == 0 {
		logger.Infof("No workspaces found for repository %s, branch %s with AutoQueueRuns enabled", repositoryFullName, branchName)
		h.recordWebhookEvent("push", "github", repositoryFullName, branchName, commitHash, "ignored",
			"No workspaces with AutoQueueRuns enabled", http.StatusOK, string(payload))
		c.JSON(http.StatusOK, gin.H{"message": "No workspaces found for this repository and branch"})
		return
	}

	logger.Infof("Found %d workspace(s) for repository %s, branch %s", len(workspaces), repositoryFullName, branchName)
	for _, ws := range workspaces {
		logger.Infof("Workspace %s - working_directory=%q, vcs_repository=%q, vcs_branch=%q",
			ws.ID, ws.WorkingDirectory, ws.VCSRepository, ws.VCSBranch)
	}

	// Collect all changed files from commits for path-based filtering
	commitsForCheck := make([]struct {
		Added    []string `json:"added"`
		Removed  []string `json:"removed"`
		Modified []string `json:"modified"`
	}, len(pushEvent.Commits))
	allChangedFilesList := make([]string, 0)
	for i, commit := range pushEvent.Commits {
		commitsForCheck[i] = struct {
			Added    []string `json:"added"`
			Removed  []string `json:"removed"`
			Modified []string `json:"modified"`
		}{
			Added:    commit.Added,
			Removed:  commit.Removed,
			Modified: commit.Modified,
		}
		allChangedFilesList = append(allChangedFilesList, commit.Added...)
		allChangedFilesList = append(allChangedFilesList, commit.Modified...)
		allChangedFilesList = append(allChangedFilesList, commit.Removed...)
	}
	logger.Infof("Changed files in push: %v", allChangedFilesList)

	// Filter workspaces based on changed files and their WorkingDirectory paths (GitOps-style filtering)
	filteredWorkspaces := make([]models.Workspace, 0)
	for _, workspace := range workspaces {
		if h.isWorkspaceAffected(workspace, commitsForCheck) {
			filteredWorkspaces = append(filteredWorkspaces, workspace)
			logger.Infof("Workspace %s (path: %q) will be triggered - files in its path were changed", workspace.ID, workspace.WorkingDirectory)
		} else {
			logger.Infof("Workspace %s (path: %q) skipped - no files in its path were changed", workspace.ID, workspace.WorkingDirectory)
		}
	}

	if len(filteredWorkspaces) == 0 {
		logger.Infof("No workspaces match the changed files for repository %s, branch %s", repositoryFullName, branchName)
		h.recordWebhookEvent("push", "github", repositoryFullName, branchName, commitHash, "ignored",
			fmt.Sprintf("No workspaces match changed files (%d workspace(s) exist but none match path)", len(workspaces)), http.StatusOK, string(payload))
		c.JSON(http.StatusOK, gin.H{
			"message": fmt.Sprintf("No workspaces match the changed files (found %d workspace(s) but none match path filters)", len(workspaces)),
		})
		return
	}

	logger.Infof("Filtered to %d workspace(s) that match changed files (from %d total)", len(filteredWorkspaces), len(workspaces))
	if len(filteredWorkspaces) != len(workspaces) {
		logger.Infof("Summary - %d workspace(s) filtered out due to path mismatch", len(workspaces)-len(filteredWorkspaces))
	}

	// Process each filtered workspace in background goroutines
	for _, workspace := range filteredWorkspaces {
		// Process in background goroutine to avoid blocking webhook response
		go func(ws models.Workspace) {
			// Recover from any panics to prevent crashing the backend
			defer func() {
				if r := recover(); r != nil {
					logger.Errorf("PANIC in goroutine for workspace %s: %v", ws.ID, r)
					// Log stack trace if available
					if err, ok := r.(error); ok {
						logger.Errorf("Panic error details: %v", err)
					}
				}
			}()

			ctx := context.Background()

			// Validate commit hash before using it
			if commitHash == "" || len(commitHash) < 7 {
				logger.Warnf("Invalid commit hash '%s' for workspace %s, skipping", commitHash, ws.ID)
				return
			}

			// Get VCS connection for authentication
			var vcsConn *models.VCSConnection
			if ws.VCSConnectionID != nil {
				var err error
				vcsConn, err = h.vcsConnectionRepo.GetByID(*ws.VCSConnectionID)
				if err != nil {
					logger.Errorf("Failed to get VCS connection for workspace %s: %v", ws.ID, err)
					return
				}
			}

			// Clone repository at commit
			tempDir, err := os.MkdirTemp("", fmt.Sprintf("workspace-%s-*", ws.ID))
			if err != nil {
				logger.Errorf("Failed to create temp directory for workspace %s: %v", ws.ID, err)
				return
			}
			defer func() {
				if err := os.RemoveAll(tempDir); err != nil {
					logger.Warnf("Failed to remove temp directory %s: %v", tempDir, err)
				}
			}()

			// Clone using git command with provider-based auth token
			var cloneURL string
			if vcsConn != nil && h.vcsRegistry != nil {
				if provider, provErr := h.vcsRegistry.GetProvider(vcsConn); provErr == nil {
					freshToken, _ := provider.GetFreshToken(ctx, vcsConn)
					cloneURL = provider.BuildCloneURL(vcsConn, freshToken, ws.VCSRepository)
				} else {
					logger.Warnf("Failed to get VCS provider for workspace %s: %v", ws.ID, provErr)
				}
			}
			if cloneURL == "" {
				// Fallback to unauthenticated GitHub clone
				cloneURL = fmt.Sprintf("https://github.com/%s.git", repositoryFullName)
			}

			// Clone repository - use unshallow clone first, then checkout specific commit
			// We need full history to checkout arbitrary commits
			cmd := exec.CommandContext(ctx, "git", "clone", cloneURL, tempDir) //nolint:gosec // intentional: executing git command
			if err := cmd.Run(); err != nil {
				logger.Errorf("Failed to clone repository for workspace %s: %v", ws.ID, err)
				return
			}

			// Checkout specific commit
			cmd = exec.CommandContext(ctx, "git", "checkout", commitHash) //nolint:gosec // intentional: executing git command
			cmd.Dir = tempDir
			if err := cmd.Run(); err != nil {
				logger.Errorf("Failed to checkout commit %s for workspace %s: %v", commitHash, ws.ID, err)
				// Try to fetch the commit if checkout failed
				cmd = exec.CommandContext(ctx, "git", "fetch", "origin", commitHash) //nolint:gosec // intentional: executing git command
				cmd.Dir = tempDir
				if fetchErr := cmd.Run(); fetchErr != nil {
					logger.Errorf("Failed to fetch commit %s for workspace %s: %v", commitHash, ws.ID, fetchErr)
					return
				}
				// Try checkout again after fetch
				cmd = exec.CommandContext(ctx, "git", "checkout", commitHash) //nolint:gosec // intentional: executing git command
				cmd.Dir = tempDir
				if err := cmd.Run(); err != nil {
					logger.Errorf("Failed to checkout commit %s for workspace %s after fetch: %v", commitHash, ws.ID, err)
					return
				}
			}

			// Create tarball from repository root to preserve full structure
			// This ensures relative module paths (e.g., ../module) work correctly
			// The runner will handle the working directory path within the extracted structure
			// Use first 7 chars of commit hash (safe since we validated length above)
			commitShort := commitHash
			if len(commitHash) >= 7 {
				commitShort = commitHash[:7]
			}
			tarballPath := filepath.Join(os.TempDir(), fmt.Sprintf("workspace-%s-%s.tar.gz", ws.ID, commitShort))
			defer func() {
				if err := os.Remove(tarballPath); err != nil && !os.IsNotExist(err) {
					logger.Warnf("Failed to remove tarball %s: %v", tarballPath, err)
				}
			}()

			// Always create tarball from repository root to preserve directory structure
			// This allows relative module paths to work correctly
			if err := h.createTarball(tempDir, tarballPath); err != nil {
				logger.Errorf("Failed to create tarball for workspace %s: %v", ws.ID, err)
				return
			}

			// Check if configuration version for this commit already exists
			// This prevents duplicate processing if webhook is called multiple times for same commit
			existingCVs, checkErr := h.configVersionRepo.GetByWorkspaceID(ws.ID)
			if checkErr == nil {
				for _, cv := range existingCVs {
					if cv.CommitHash == commitHash && cv.Status != models.ConfigurationVersionStatusErrored {
						logger.Infof("Configuration version for commit %s already exists for workspace %s, skipping", commitHash, ws.ID)
						return
					}
				}
			}

			// Create configuration version with commit info
			configVersion := &models.ConfigurationVersion{
				WorkspaceID:   ws.ID,
				Status:        models.ConfigurationVersionStatusPending,
				Source:        "tfe-vcs", // Mark as VCS-triggered
				AutoQueueRuns: true,
				Speculative:   false,
				CommitHash:    commitHash,
				Committer:     committer,
			}

			if err := h.configVersionRepo.Create(configVersion); err != nil {
				logger.Errorf("Failed to create configuration version for workspace %s: %v", ws.ID, err)
				// Check if it's a unique constraint violation (duplicate)
				errStr := strings.ToLower(err.Error())
				if strings.Contains(errStr, "duplicate") || strings.Contains(errStr, "unique constraint") || strings.Contains(errStr, "already exists") {
					logger.Infof("Configuration version for commit %s already exists (duplicate), skipping", commitHash)
					return
				}
				// For other errors, log and return (don't crash)
				return
			}

			// Upload tarball to MinIO
			tarballFile, err := os.Open(tarballPath) //nolint:gosec // tarballPath is validated (in temp directory)
			if err != nil {
				logger.Errorf("Failed to open tarball for workspace %s: %v", ws.ID, err)
				return
			}
			defer func() {
				if err := tarballFile.Close(); err != nil {
					logger.Warnf("Failed to close tarball file: %v", err)
				}
			}()

			storageKey := fmt.Sprintf("configuration-versions/%s/config.tar.gz", configVersion.ID)
			if h.storageClient == nil {
				logger.Errorf("storageClient is nil, cannot upload configuration for workspace %s", ws.ID)
				// Mark configuration version as errored since we can't upload
				configVersion.Status = models.ConfigurationVersionStatusErrored
				configVersion.ErrorMessage = "Storage client not initialized"
				if updateErr := h.configVersionRepo.Update(configVersion); updateErr != nil {
					logger.Errorf("Failed to update configuration version error status: %v", updateErr)
				}
				return
			}

			logger.Infof("Uploading tarball to MinIO: %s for workspace %s", storageKey, ws.ID)
			if err := h.storageClient.PutStream(ctx, storageKey, tarballFile); err != nil {
				logger.Errorf("Failed to upload configuration for workspace %s: %v", ws.ID, err)
				// Mark configuration version as errored since upload failed
				configVersion.Status = models.ConfigurationVersionStatusErrored
				configVersion.ErrorMessage = fmt.Sprintf("Failed to upload configuration: %v", err)
				if updateErr := h.configVersionRepo.Update(configVersion); updateErr != nil {
					logger.Errorf("Failed to update configuration version error status: %v", updateErr)
				}
				return
			}

			logger.Infof("Successfully uploaded tarball to MinIO: %s for workspace %s", storageKey, ws.ID)

			// Update configuration version status to uploaded (only after successful upload)
			configVersion.Status = models.ConfigurationVersionStatusUploaded
			if err := h.configVersionRepo.Update(configVersion); err != nil {
				logger.Errorf("Failed to update configuration version status for workspace %s: %v", ws.ID, err)
				return
			}
			logger.Infof("Configuration version %s marked as uploaded for workspace %s", configVersion.ID, ws.ID)

			// TFE-compatible: Auto-cancel previous plan-and-apply runs (pending/running/planned) before creating new one
			// This uses the centralized auto-cancel logic (same as UI-triggered runs)
			// VCS-triggered runs are always plan-and-apply, so cancel other plan-and-apply runs
			terraform.AutoCancelConflictingRuns(h.runRepo, ws.ID, models.RunOperationPlanAndApply)

			// Create plan-and-apply run (TFE-compatible: VCS-triggered runs are always plan-and-apply)
			// This is a single run that goes through both phases: planning → planned → applying → applied
			planAndApplyRun := &models.Run{
				WorkspaceID:            ws.ID,
				ConfigurationVersionID: &configVersion.ID,
				CreatedBy:              nil, // VCS-triggered, no user
				Status:                 models.RunStatusPending,
				Operation:              models.RunOperationPlanAndApply, // VCS-triggered runs are always plan-and-apply
				AutoApplyAfterPlan:     true,                            // VCS-triggered runs should auto-apply if workspace.AutoApply is enabled
			}

			if err := h.runRepo.Create(planAndApplyRun); err != nil {
				logger.Errorf("Failed to create plan-and-apply run for workspace %s: %v", ws.ID, err)
				// Check if it's a constraint violation (e.g., duplicate run)
				if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique constraint") || strings.Contains(err.Error(), "foreign key") {
					logger.Infof("Run creation failed due to constraint violation, skipping")
					return
				}
				// For other errors, log and return (don't crash)
				return
			}

			logger.Infof("Created plan-and-apply run %s for workspace %s from commit %s", planAndApplyRun.ID, ws.ID, commitHash)

			// TFE-compatible: VCS-triggered runs are plan-and-apply runs
			// The runner will automatically transition to applying phase after plan completes successfully
			// if AutoApply is enabled (which is the default expectation for VCS pushes)
			if ws.AutoApply && branchName == ws.VCSBranch {
				logger.Infof("Auto-apply enabled for workspace %s, run will automatically transition to applying phase after plan completes", ws.ID)
			}
		}(workspace)
	}

	// Also sync VCS inventories that match this repository and branch
	inventories, err := h.inventoryRepo.FindByVCSRepositoryAndBranch(repositoryFullName, branchName)
	if err != nil {
		logger.Errorf("Error finding inventories for repository %s, branch %s: %v", repositoryFullName, branchName, err)
	} else if len(inventories) > 0 {
		logger.Infof("Found %d inventory(ies) for repository %s, branch %s", len(inventories), repositoryFullName, branchName)
		// Map commits to the format expected by isInventoryAffected
		commitsForCheck := make([]struct {
			Added    []string `json:"added"`
			Removed  []string `json:"removed"`
			Modified []string `json:"modified"`
		}, len(pushEvent.Commits))
		for i, commit := range pushEvent.Commits {
			commitsForCheck[i] = struct {
				Added    []string `json:"added"`
				Removed  []string `json:"removed"`
				Modified []string `json:"modified"`
			}{
				Added:    commit.Added,
				Removed:  commit.Removed,
				Modified: commit.Modified,
			}
		}
		for _, inventory := range inventories {
			// Check if inventory path was affected by this push
			if h.isInventoryAffected(inventory, commitsForCheck) {
				logger.Infof("Triggering sync for inventory: %s (ID: %s)", inventory.Name, inventory.ID)
				if h.ansibleQueue != nil {
					go func(inv models.AnsibleInventory) {
						// Use JSON marshaling to create the sync message (same format as ansible-runner expects)
						syncMsg := map[string]interface{}{
							"inventory_id": inv.ID.String(),
						}
						if err := h.ansibleQueue.Enqueue(context.Background(), "ansible_sync", syncMsg); err != nil {
							logger.Errorf("Error queuing sync for inventory %s: %v", inv.ID, err)
						}
					}(inventory)
				}
			}
		}
	}

	// Also sync VCS playbooks that match this repository and branch
	if h.playbookRepo != nil {
		playbooks, err := h.playbookRepo.ListByVCSRepositoryAndBranch(repositoryFullName, branchName)
		if err != nil {
			logger.Errorf("Error finding playbooks for repository %s, branch %s: %v", repositoryFullName, branchName, err)
		} else if len(playbooks) > 0 {
			logger.Infof("Found %d playbook(s) for repository %s, branch %s", len(playbooks), repositoryFullName, branchName)
			// Map commits to the format expected by isPlaybookAffected
			commitsForCheck := make([]struct {
				Added    []string `json:"added"`
				Removed  []string `json:"removed"`
				Modified []string `json:"modified"`
			}, len(pushEvent.Commits))
			for i, commit := range pushEvent.Commits {
				commitsForCheck[i] = struct {
					Added    []string `json:"added"`
					Removed  []string `json:"removed"`
					Modified []string `json:"modified"`
				}{
					Added:    commit.Added,
					Removed:  commit.Removed,
					Modified: commit.Modified,
				}
			}
			for _, playbook := range playbooks {
				// Check if playbook path was affected by this push
				if h.isPlaybookAffected(playbook, commitsForCheck) {
					logger.Infof("Triggering sync for playbook: %s (ID: %s)", playbook.Name, playbook.ID)
					if h.ansibleQueue != nil {
						go func(pb models.AnsiblePlaybook) {
							// Use JSON marshaling to create the sync message (same format as ansible-runner expects)
							syncMsg := map[string]interface{}{
								"playbook_id": pb.ID.String(),
							}
							if err := h.ansibleQueue.Enqueue(context.Background(), "ansible_sync", syncMsg); err != nil {
								logger.Errorf("Error queuing sync for playbook %s: %v", pb.ID, err)
							}
						}(playbook)
					}
				}
			}
		}
	}

	// Build list of triggered workspace names for the webhook event message
	var triggeredNames []string
	for _, ws := range filteredWorkspaces {
		triggeredNames = append(triggeredNames, ws.Name)
	}
	eventMessage := fmt.Sprintf("%d workspace(s) triggered", len(filteredWorkspaces))
	if len(triggeredNames) > 0 {
		eventMessage = fmt.Sprintf("%d workspace(s) triggered: %s", len(filteredWorkspaces), strings.Join(triggeredNames, ", "))
	}

	// Record webhook event
	h.recordWebhookEvent("push", "github", repositoryFullName, branchName, commitHash, "success",
		eventMessage, http.StatusOK, string(payload))

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("Branch push event processed: %d workspace(s) queued (filtered from %d total)", len(filteredWorkspaces), len(workspaces)),
	})
}

// handlePullRequestEvent handles GitHub pull request events for speculative plans
// TFE-compatible: Creates speculative plan-only runs when PRs are opened or updated
func (h *VCSAppInstallationHandlerV2) handlePullRequestEvent(c *gin.Context, payload []byte) {
	var prEvent struct {
		Action      string `json:"action"` // "opened", "synchronize", "reopened", etc.
		Number      int    `json:"number"` // PR number
		PullRequest struct {
			Head struct {
				Ref string `json:"ref"` // Source branch
				SHA string `json:"sha"` // Head commit SHA
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"` // Target branch (typically "main")
			} `json:"base"`
			User struct {
				Login string `json:"login"` // PR author username
			} `json:"user"`
		} `json:"pull_request"`
		Repository struct {
			FullName string `json:"full_name"`
			ID       int64  `json:"id"`
		} `json:"repository"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}

	if err := json.Unmarshal(payload, &prEvent); err != nil {
		logger.Errorf("Failed to parse pull request event: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Failed to parse pull request event",
				},
			},
		})
		return
	}

	// Only process opened, synchronize (new commits), and reopened PRs
	if prEvent.Action != "opened" && prEvent.Action != "synchronize" && prEvent.Action != "reopened" {
		logger.Infof("Ignoring PR event action: %s", prEvent.Action)
		c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("PR action %s ignored", prEvent.Action)})
		return
	}

	baseBranch := prEvent.PullRequest.Base.Ref
	headBranch := prEvent.PullRequest.Head.Ref
	headSHA := prEvent.PullRequest.Head.SHA
	repositoryFullName := prEvent.Repository.FullName
	prNumber := prEvent.Number

	logger.Infof("Received pull request event - action=%s, PR=%d, base=%s, head=%s, headSHA=%s, repository=%s",
		prEvent.Action, prNumber, baseBranch, headBranch, headSHA, repositoryFullName)

	// Find workspaces that match this repository and base branch (target branch)
	// Only trigger speculative plans for workspaces connected to the target branch
	workspaces, err := h.workspaceRepo.FindByVCSRepositoryAndBranch(repositoryFullName, baseBranch)
	if err != nil {
		logger.Errorf("Error finding workspaces for repository %s, branch %s: %v", repositoryFullName, baseBranch, err)
		c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("No workspaces found or error: %v", err)})
		return
	}

	if len(workspaces) == 0 {
		logger.Infof("No workspaces found for repository %s, branch %s", repositoryFullName, baseBranch)
		c.JSON(http.StatusOK, gin.H{"message": "No workspaces found for this repository and branch"})
		return
	}

	// Filter workspaces that have speculative plans enabled
	filteredWorkspaces := make([]models.Workspace, 0)
	for _, ws := range workspaces {
		if ws.SpeculativeEnabled {
			filteredWorkspaces = append(filteredWorkspaces, ws)
		}
	}

	if len(filteredWorkspaces) == 0 {
		logger.Infof("No workspaces with speculative plans enabled for repository %s, branch %s", repositoryFullName, baseBranch)
		c.JSON(http.StatusOK, gin.H{"message": "No workspaces with speculative plans enabled"})
		return
	}

	logger.Infof("Found %d workspace(s) for PR #%d (filtered from %d total)", len(filteredWorkspaces), prNumber, len(workspaces))

	// Get changed files using git diff between base and head branches for path-based filtering
	// This allows us to only trigger workspaces whose paths are actually affected
	changedFiles := h.getPRChangedFiles(repositoryFullName, baseBranch, headBranch, headSHA)

	// Filter workspaces based on changed files and their WorkingDirectory paths (GitOps-style filtering)
	// This prevents unnecessary plan runs for workspaces whose code wasn't changed
	finalFilteredWorkspaces := make([]models.Workspace, 0)
	if len(changedFiles) > 0 {
		logger.Infof("PR #%d changed files: %v", prNumber, changedFiles)
		for _, workspace := range filteredWorkspaces {
			if h.isWorkspaceAffectedByFiles(workspace, changedFiles) {
				finalFilteredWorkspaces = append(finalFilteredWorkspaces, workspace)
				logger.Infof("Workspace %s (path: %q) will be triggered - files in its path were changed", workspace.ID, workspace.WorkingDirectory)
			} else {
				logger.Infof("Workspace %s (path: %q) skipped - no files in its path were changed", workspace.ID, workspace.WorkingDirectory)
			}
		}
		if len(finalFilteredWorkspaces) == 0 {
			logger.Infof("No workspaces match the changed files for PR #%d (found %d workspace(s) but none match path filters)", prNumber, len(filteredWorkspaces))
			c.JSON(http.StatusOK, gin.H{
				"message": fmt.Sprintf("No workspaces match the changed files (found %d workspace(s) but none match path filters)", len(filteredWorkspaces)),
			})
			return
		}
		logger.Infof("Filtered to %d workspace(s) that match changed files (from %d total with speculative enabled)", len(finalFilteredWorkspaces), len(filteredWorkspaces))
		if len(finalFilteredWorkspaces) != len(filteredWorkspaces) {
			logger.Infof("Summary - %d workspace(s) filtered out due to path mismatch", len(filteredWorkspaces)-len(finalFilteredWorkspaces))
		}
		filteredWorkspaces = finalFilteredWorkspaces
	} else {
		logger.Infof("Could not determine changed files for PR #%d, triggering all %d workspaces with speculative enabled", prNumber, len(filteredWorkspaces))
	}

	// Process each workspace in background goroutines
	for _, workspace := range filteredWorkspaces {
		go func(ws models.Workspace) {
			// Recover from any panics to prevent crashing the backend
			defer func() {
				if r := recover(); r != nil {
					logger.Errorf("PANIC in goroutine for workspace %s: %v", ws.ID, r)
				}
			}()

			ctx := context.Background()

			// Validate commit hash (GitHub accepts 7+ character SHAs, but full 40-char is preferred)
			if headSHA == "" || len(headSHA) < 7 {
				logger.Warnf("Invalid commit hash '%s' (length: %d) for workspace %s, skipping", headSHA, len(headSHA), ws.ID)
				return
			}

			// Get VCS connection for authentication
			var vcsConn *models.VCSConnection
			if ws.VCSConnectionID != nil {
				var err error
				vcsConn, err = h.vcsConnectionRepo.GetByID(*ws.VCSConnectionID)
				if err != nil {
					logger.Errorf("Failed to get VCS connection for workspace %s: %v", ws.ID, err)
					return
				}
			}

			// Clone repository at PR head commit
			tempDir, err := os.MkdirTemp("", fmt.Sprintf("workspace-pr-%s-*", ws.ID))
			if err != nil {
				logger.Errorf("Failed to create temp directory for workspace %s: %v", ws.ID, err)
				return
			}
			defer func() {
				if err := os.RemoveAll(tempDir); err != nil {
					logger.Warnf("Failed to remove temp directory %s: %v", tempDir, err)
				}
			}()

			// Clone using git command with installation token if available
			var cloneURL string
			if vcsConn != nil && vcsConn.InstallationID != "" && h.githubAppManager != nil && h.githubAppManager.IsEnabled() {
				githubService := h.githubAppManager.GetService()
				installToken, err := githubService.GenerateInstallationToken(ctx, vcsConn.InstallationID)
				if err == nil {
					cloneURL = fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", installToken, repositoryFullName)
				} else {
					logger.Warnf("Failed to generate installation token for workspace %s, using public clone: %v", ws.ID, err)
					cloneURL = fmt.Sprintf("https://github.com/%s.git", repositoryFullName)
				}
			} else {
				cloneURL = fmt.Sprintf("https://github.com/%s.git", repositoryFullName)
			}

			// Clone repository
			cmd := exec.CommandContext(ctx, "git", "clone", cloneURL, tempDir) //nolint:gosec // intentional: executing git command
			if err := cmd.Run(); err != nil {
				logger.Errorf("Failed to clone repository for workspace %s: %v", ws.ID, err)
				return
			}

			// Checkout PR head branch
			cmd = exec.CommandContext(ctx, "git", "checkout", headBranch) //nolint:gosec // intentional: executing git command
			cmd.Dir = tempDir
			if err := cmd.Run(); err != nil {
				logger.Errorf("Failed to checkout branch %s for workspace %s: %v", headBranch, ws.ID, err)
				// Try to fetch the branch if checkout failed
				cmd = exec.CommandContext(ctx, "git", "fetch", "origin", headBranch) //nolint:gosec // intentional: executing git command
				cmd.Dir = tempDir
				if fetchErr := cmd.Run(); fetchErr != nil {
					logger.Errorf("Failed to fetch branch %s for workspace %s: %v", headBranch, ws.ID, fetchErr)
					return
				}
				// Try checkout again after fetch
				cmd = exec.CommandContext(ctx, "git", "checkout", headBranch) //nolint:gosec // intentional: executing git command
				cmd.Dir = tempDir
				if err := cmd.Run(); err != nil {
					logger.Errorf("Failed to checkout branch %s for workspace %s after fetch: %v", headBranch, ws.ID, err)
					return
				}
			}

			// Create tarball from repository root
			commitShort := headSHA
			if len(headSHA) >= 7 {
				commitShort = headSHA[:7]
			}
			tarballPath := filepath.Join(os.TempDir(), fmt.Sprintf("workspace-pr-%s-%s.tar.gz", ws.ID, commitShort))
			defer func() {
				if err := os.Remove(tarballPath); err != nil && !os.IsNotExist(err) {
					logger.Warnf("Failed to remove tarball %s: %v", tarballPath, err)
				}
			}()

			if err := h.createTarball(tempDir, tarballPath); err != nil {
				logger.Errorf("Failed to create tarball for workspace %s: %v", ws.ID, err)
				return
			}

			// Check if configuration version for this commit already exists
			existingCVs, checkErr := h.configVersionRepo.GetByWorkspaceID(ws.ID)
			if checkErr == nil {
				for _, cv := range existingCVs {
					if cv.CommitHash == headSHA && cv.Speculative && cv.Status != models.ConfigurationVersionStatusErrored {
						logger.Infof("Configuration version for PR commit %s already exists for workspace %s, skipping", headSHA, ws.ID)
						return
					}
				}
			}

			// Create speculative configuration version
			configVersion := &models.ConfigurationVersion{
				WorkspaceID:   ws.ID,
				Status:        models.ConfigurationVersionStatusPending,
				Source:        "tfe-vcs", // Mark as VCS-triggered
				AutoQueueRuns: true,
				Speculative:   true, // Mark as speculative (plan-only)
				CommitHash:    headSHA,
				Committer:     prEvent.PullRequest.User.Login,
				PRNumber:      prNumber,
				SourceBranch:  headBranch,
			}

			if err := h.configVersionRepo.Create(configVersion); err != nil {
				logger.Errorf("Failed to create configuration version for workspace %s: %v", ws.ID, err)
				errStr := strings.ToLower(err.Error())
				if strings.Contains(errStr, "duplicate") || strings.Contains(errStr, "unique constraint") || strings.Contains(errStr, "already exists") {
					logger.Infof("Configuration version for commit %s already exists (duplicate), skipping", headSHA)
					return
				}
				return
			}

			// Upload tarball to MinIO
			tarballFile, err := os.Open(tarballPath) //nolint:gosec // tarballPath is validated (in temp directory)
			if err != nil {
				logger.Errorf("Failed to open tarball for workspace %s: %v", ws.ID, err)
				return
			}
			defer func() {
				if err := tarballFile.Close(); err != nil {
					logger.Warnf("Failed to close tarball file: %v", err)
				}
			}()

			storageKey := fmt.Sprintf("configuration-versions/%s/config.tar.gz", configVersion.ID)
			if h.storageClient == nil {
				logger.Errorf("storageClient is nil, cannot upload configuration for workspace %s", ws.ID)
				configVersion.Status = models.ConfigurationVersionStatusErrored
				configVersion.ErrorMessage = "Storage client not initialized"
				if updateErr := h.configVersionRepo.Update(configVersion); updateErr != nil {
					logger.Errorf("Failed to update configuration version error status: %v", updateErr)
				}
				return
			}

			logger.Infof("Uploading tarball to MinIO: %s for workspace %s", storageKey, ws.ID)
			if err := h.storageClient.PutStream(ctx, storageKey, tarballFile); err != nil {
				logger.Errorf("Failed to upload configuration for workspace %s: %v", ws.ID, err)
				configVersion.Status = models.ConfigurationVersionStatusErrored
				configVersion.ErrorMessage = fmt.Sprintf("Failed to upload configuration: %v", err)
				if updateErr := h.configVersionRepo.Update(configVersion); updateErr != nil {
					logger.Errorf("Failed to update configuration version error status: %v", updateErr)
				}
				return
			}

			// Mark configuration version as uploaded
			configVersion.Status = models.ConfigurationVersionStatusUploaded
			if err := h.configVersionRepo.Update(configVersion); err != nil {
				logger.Errorf("Failed to update configuration version status: %v", err)
				return
			}

			// Create plan-only run (speculative)
			run := &models.Run{
				WorkspaceID:            ws.ID,
				ConfigurationVersionID: &configVersion.ID,
				CreatedBy:              nil, // System-triggered
				Status:                 models.RunStatusPending,
				Operation:              models.RunOperationPlanOnly, // Plan-only for speculative runs
			}

			if err := h.runRepo.Create(run); err != nil {
				logger.Errorf("Failed to create run for workspace %s: %v", ws.ID, err)
				return
			}

			logger.Infof("Created speculative plan-only run %s for workspace %s from PR #%d commit %s", run.ID, ws.ID, prNumber, headSHA)

			// Create GitHub status check
			if h.statusService == nil {
				logger.Warnf("Status service is nil, cannot create status check for run %s", run.ID)
				return
			}
			if vcsConn == nil {
				logger.Warnf("VCS connection is nil for workspace %s, cannot create status check for run %s", ws.ID, run.ID)
				return
			}
			if vcsConn.InstallationID == "" {
				logger.Warnf("VCS connection has no installation ID for workspace %s, cannot create status check for run %s", ws.ID, run.ID)
				return
			}
			// Use workspace VCS repository if available, otherwise use PR event repository
			repoFullName := repositoryFullName
			if ws.VCSRepository != "" {
				repoFullName = ws.VCSRepository
				logger.Infof("Using workspace VCS repository %s for status check (PR event had %s)", repoFullName, repositoryFullName)
			}

			// Extract owner and repo from full name (e.g., "owner/repo")
			parts := strings.Split(repoFullName, "/")
			if len(parts) != 2 {
				logger.Warnf("Invalid repository full name format '%s' for workspace %s, cannot create status check", repoFullName, ws.ID)
			} else {
				owner := parts[0]
				repo := parts[1]

				// Generate target URL for status check
				// Format: /app/:orgName/workspaces/:workspaceName/runs/:runId
				// Use request host if available, otherwise construct from config
				host := c.GetHeader("Host")
				if host == "" {
					host = c.Request.Host
				}
				if host == "" {
					host = "localhost:5173" // Fallback to frontend default
				}
				scheme := "https"
				if c.GetHeader("X-Forwarded-Proto") == "http" || c.Request.TLS == nil {
					scheme = "http"
				}

				// Get organization name from workspace (need to fetch with project preloaded)
				workspaceForURL, urlErr := h.workspaceRepo.GetByID(ws.ID)
				var targetURL string
				if urlErr == nil && workspaceForURL.Project.ID != (uuid.UUID{}) && workspaceForURL.Project.Organization.ID != (uuid.UUID{}) && workspaceForURL.Project.Organization.Name != "" {
					// Use organization name and workspace name for proper routing
					targetURL = fmt.Sprintf("%s://%s/app/%s/workspaces/%s/runs/%s", scheme, host, workspaceForURL.Project.Organization.Name, workspaceForURL.Name, run.ID)
				} else {
					// Fallback to workspace ID if org info not available
					logger.Warnf("Could not get org info for workspace %s, using fallback URL with workspace ID", ws.ID)
					targetURL = fmt.Sprintf("%s://%s/workspaces/%s/runs/%s", scheme, host, ws.ID, run.ID)
				}

				// Status check context: terraform-plan/<workspace-name>
				statusContext := fmt.Sprintf("terraform-plan/%s", ws.Name)

				logger.Infof("Creating status check for run %s - installationID=%s, owner=%s, repo=%s, sha=%s, context=%s",
					run.ID, vcsConn.InstallationID, owner, repo, headSHA, statusContext)

				// Create pending status check
				err = h.statusService.CreateStatusCheck(
					ctx,
					vcsConn.InstallationID,
					owner,
					repo,
					headSHA,
					statusContext,
					vcs.StatusStatePending,
					"Terraform plan is queued",
					targetURL,
				)
				if err != nil {
					// Log error but don't fail the run creation
					logger.Errorf("Failed to create status check for run %s: %v", run.ID, err)
				} else {
					logger.Infof("Created status check for run %s (PR #%d, commit %s)", run.ID, prNumber, headSHA)
				}
			}
		}(workspace)
	}

	// Build list of triggered workspace names for the webhook event message
	var prTriggeredNames []string
	for _, ws := range filteredWorkspaces {
		prTriggeredNames = append(prTriggeredNames, ws.Name)
	}
	prEventMessage := fmt.Sprintf("PR #%d: %d workspace(s) triggered for speculative plans", prNumber, len(filteredWorkspaces))
	if len(prTriggeredNames) > 0 {
		prEventMessage = fmt.Sprintf("PR #%d: %d workspace(s) triggered: %s", prNumber, len(filteredWorkspaces), strings.Join(prTriggeredNames, ", "))
	}

	// Record webhook event
	h.recordWebhookEvent("pull_request", "github", repositoryFullName, headBranch, headSHA, "success",
		prEventMessage, http.StatusOK, string(payload))

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("Pull request event processed: %d workspace(s) queued for speculative plans", len(filteredWorkspaces)),
	})
}

// isWorkspaceAffected checks if a workspace is affected by changed files based on its WorkingDirectory path
// Implements GitOps-style path-based filtering: only triggers workspace if files in its path are changed
func (h *VCSAppInstallationHandlerV2) isWorkspaceAffected(workspace models.Workspace, commits []struct {
	Added    []string `json:"added"`
	Removed  []string `json:"removed"`
	Modified []string `json:"modified"`
},
) bool {
	// If WorkingDirectory is empty or root, match all changes (root-level workspace)
	workingDir := strings.TrimSpace(workspace.WorkingDirectory)
	if workingDir == "" || workingDir == "/" {
		logger.Infof("Workspace %s - working directory is empty/root, matching all changes", workspace.ID)
		return true
	}

	// Normalize the working directory path (remove leading/trailing slashes, ensure it ends with / for prefix matching)
	originalWorkingDir := workingDir
	workingDir = strings.TrimPrefix(workingDir, "/")
	workingDir = strings.TrimSuffix(workingDir, "/")
	if workingDir == "" {
		// After normalization, if empty, it's root-level
		logger.Infof("Workspace %s - working directory normalized to empty, matching all changes", workspace.ID)
		return true
	}
	// Add trailing slash for proper prefix matching
	workingDirPrefix := workingDir + "/"

	// Collect all changed files from all commits
	allChangedFiles := make(map[string]bool)
	for _, commit := range commits {
		for _, file := range commit.Added {
			allChangedFiles[file] = true
		}
		for _, file := range commit.Modified {
			allChangedFiles[file] = true
		}
		for _, file := range commit.Removed {
			allChangedFiles[file] = true
		}
	}

	logger.Infof("Workspace %s - checking path filter: workingDir=%q (normalized=%q, prefix=%q), changedFiles=%v",
		workspace.ID, originalWorkingDir, workingDir, workingDirPrefix, getKeys(allChangedFiles))

	// Check if any changed file is within the workspace's working directory
	// A workspace with working_directory="proxmox" should match files in "proxmox/" and all subdirectories
	// A workspace with working_directory="proxmox/passwd" should only match files in "proxmox/passwd/"
	for file := range allChangedFiles {
		// Normalize file path (remove leading slash)
		normalizedFile := strings.TrimPrefix(file, "/")

		// Check if file is exactly in the working directory or in a subdirectory
		// This correctly handles:
		// - workingDir="proxmox" matches "proxmox/main.tf" and "proxmox/passwd/main.tf"
		// - workingDir="proxmox/passwd" matches "proxmox/passwd/main.tf" but NOT "proxmox/api/main.tf"
		matches := normalizedFile == workingDir || strings.HasPrefix(normalizedFile, workingDirPrefix)
		if matches {
			logger.Infof("Workspace %s - file %q matches working directory %q (normalized: %q, prefix: %q)",
				workspace.ID, file, workingDir, normalizedFile, workingDirPrefix)
			return true
		}
	}

	logger.Infof("Workspace %s - no files match working directory %q", workspace.ID, workingDir)
	return false
}

// isWorkspaceAffectedByFiles checks if a workspace is affected by a list of changed files
// This is a simpler version that works with a flat list of file paths
func (h *VCSAppInstallationHandlerV2) isWorkspaceAffectedByFiles(workspace models.Workspace, changedFiles []string) bool {
	workingDir := strings.TrimSpace(workspace.WorkingDirectory)

	// Normalize the working directory path
	workingDir = strings.TrimPrefix(workingDir, "/")
	workingDir = strings.TrimSuffix(workingDir, "/")

	// If WorkingDirectory is empty (root-level workspace), only match files at repository root
	// Root-level files have no directory separator (no "/" in the path)
	if workingDir == "" {
		for _, file := range changedFiles {
			// Normalize file path (remove leading slash)
			normalizedFile := strings.TrimPrefix(file, "/")
			// Check if file is at root level (no directory separator)
			if normalizedFile != "" && !strings.Contains(normalizedFile, "/") {
				return true
			}
		}
		return false
	}

	// Add trailing slash for proper prefix matching
	workingDirPrefix := workingDir + "/"

	// Check if any changed file is within the workspace's working directory
	for _, file := range changedFiles {
		// Normalize file path (remove leading slash)
		normalizedFile := strings.TrimPrefix(file, "/")
		// Check if file is in the workspace's directory
		if normalizedFile == workingDir || strings.HasPrefix(normalizedFile, workingDirPrefix) {
			return true
		}
	}

	return false
}

// getPRChangedFiles uses git diff to get the list of files changed between base and head branches
// Returns empty slice if unable to determine (e.g., no access to repo), which will trigger all workspaces
func (h *VCSAppInstallationHandlerV2) getPRChangedFiles(repositoryFullName, baseBranch, headBranch, headSHA string) []string {
	// Create a temporary directory for git operations
	tempDir, err := os.MkdirTemp("", "pr-diff-*")
	if err != nil {
		logger.Errorf("Failed to create temp directory for PR diff: %v", err)
		return nil
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			logger.Warnf("Failed to remove temp directory %s: %v", tempDir, err)
		}
	}()

	ctx := context.Background()

	// Clone repository (shallow clone is faster)
	cloneURL := "https://github.com/" + repositoryFullName + ".git"
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "50", cloneURL, tempDir) //nolint:gosec // intentional: executing git command
	if err := cmd.Run(); err != nil {
		logger.Errorf("Failed to clone repository for PR diff: %v", err)
		return nil
	}

	// Unshallow to allow fetching arbitrary branches
	cmd = exec.CommandContext(ctx, "git", "fetch", "--unshallow")
	cmd.Dir = tempDir
	_ = cmd.Run() // Ignore error - repo might already be unshallow or unshallow might fail

	// Fetch both base and head branches from origin using refspec format
	// This ensures remote tracking refs are set up properly (required for git diff after shallow clone)
	// Format: refs/heads/<branch>:refs/remotes/origin/<branch>
	baseRefspec := fmt.Sprintf("refs/heads/%s:refs/remotes/origin/%s", baseBranch, baseBranch)
	headRefspec := fmt.Sprintf("refs/heads/%s:refs/remotes/origin/%s", headBranch, headBranch)
	cmd = exec.CommandContext(ctx, "git", "fetch", "origin", baseRefspec, headRefspec) //nolint:gosec // intentional: executing git command
	cmd.Dir = tempDir
	if err := cmd.Run(); err != nil {
		logger.Errorf("Failed to fetch branches for PR diff: %v", err)
		return nil
	}

	// Get changed files using git diff between origin/baseBranch and origin/headBranch
	// Format: --name-only gives just the file names
	var output []byte
	cmd = exec.CommandContext(ctx, "git", "diff", "--name-only", "origin/"+baseBranch, "origin/"+headBranch) //nolint:gosec // intentional: executing git command with branch names
	cmd.Dir = tempDir
	output, err = cmd.Output()
	if err != nil {
		// Get stderr to see the actual error
		if exitError, ok := err.(*exec.ExitError); ok {
			stderr := string(exitError.Stderr)
			logger.Errorf("Failed to get PR diff (exit status %d): %s", exitError.ExitCode(), stderr)
		} else {
			logger.Errorf("Failed to get PR diff: %v", err)
		}
		return nil
	}

	// Parse output (one file per line)
	changedFiles := make([]string, 0)
	for _, line := range strings.Split(string(output), "\n") {
		file := strings.TrimSpace(line)
		if file != "" {
			changedFiles = append(changedFiles, file)
		}
	}

	return changedFiles
}

// Helper function to get keys from map for logging
func getKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// isInventoryAffected checks if an inventory file was affected by a push event
// Helper function to check if inventory path is in the changed files
func (h *VCSAppInstallationHandlerV2) isInventoryAffected(inventory models.AnsibleInventory, commits []struct {
	Added    []string `json:"added"`
	Removed  []string `json:"removed"`
	Modified []string `json:"modified"`
},
) bool {
	// If inventory path is empty, assume it's affected
	if inventory.InventoryPath == "" {
		return true
	}

	// Check if the inventory file was in any of the commits
	for _, commit := range commits {
		for _, file := range commit.Added {
			if file == inventory.InventoryPath || strings.HasSuffix(file, inventory.InventoryPath) {
				return true
			}
		}
		for _, file := range commit.Modified {
			if file == inventory.InventoryPath || strings.HasSuffix(file, inventory.InventoryPath) {
				return true
			}
		}
		for _, file := range commit.Removed {
			if file == inventory.InventoryPath || strings.HasSuffix(file, inventory.InventoryPath) {
				return true
			}
		}
	}

	return false
}

// isPlaybookAffected checks if a playbook's files were affected by a push event
// Helper function to check if playbook path or files in the playbook's directory were changed
func (h *VCSAppInstallationHandlerV2) isPlaybookAffected(playbook models.AnsiblePlaybook, commits []struct {
	Added    []string `json:"added"`
	Removed  []string `json:"removed"`
	Modified []string `json:"modified"`
},
) bool {
	// If no specific path is set, any change to the repo affects the playbook
	if playbook.PlaybookPath == "" {
		return true
	}

	// Get the directory containing the playbook
	playbookDir := getDirectory(playbook.PlaybookPath)

	// Check if any changed files are in the playbook's directory
	for _, commit := range commits {
		for _, file := range commit.Added {
			if strings.HasPrefix(file, playbookDir) || file == playbook.PlaybookPath {
				return true
			}
		}
		for _, file := range commit.Modified {
			if strings.HasPrefix(file, playbookDir) || file == playbook.PlaybookPath {
				return true
			}
		}
		for _, file := range commit.Removed {
			if strings.HasPrefix(file, playbookDir) || file == playbook.PlaybookPath {
				return true
			}
		}
	}

	return false
}

// getDirectory returns the directory portion of a path
func getDirectory(path string) string {
	lastSlash := strings.LastIndex(path, "/")
	if lastSlash == -1 {
		return ""
	}
	return path[:lastSlash+1]
}

// createTarball creates a gzipped tarball from a directory (similar to module publisher)
func (h *VCSAppInstallationHandlerV2) createTarball(sourceDir, outputPath string) error {
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
