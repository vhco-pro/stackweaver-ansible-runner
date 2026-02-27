// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/vcs"
	"github.com/michielvha/logger"
)

// updatePRStatusCheck updates the VCS status check for PR-triggered runs.
// Dispatches to GitHub or Azure DevOps based on the VCS connection provider.
func updatePRStatusCheck(
	ctx context.Context,
	run *models.Run,
	workspaceRepo *repository.WorkspaceRepository,
	configVersionRepo *repository.ConfigurationVersionRepository,
	vcsConnectionRepo *repository.VCSConnectionRepository,
	statusService *vcs.GitHubStatusService,
	adoStatusService *vcs.AzureDevOpsStatusService,
	vcsRegistry *vcs.ProviderRegistry,
) {
	logger.Infof("[STATUS_CHECK] updatePRStatusCheck called for run %s, status=%s", run.ID, run.Status)

	// Only update status checks for speculative (PR) runs with commit hash
	if run.ConfigurationVersionID == nil {
		logger.Infof("[STATUS_CHECK] Run %s has no configuration version ID, skipping", run.ID)
		return
	}

	configVersion, err := configVersionRepo.GetByID(*run.ConfigurationVersionID)
	if err != nil || configVersion == nil {
		logger.Infof("[STATUS_CHECK] Failed to get config version for run %s: %v", run.ID, err)
		return
	}

	// Only update status checks for speculative plans with commit hash (PR runs)
	if !configVersion.Speculative {
		logger.Infof("[STATUS_CHECK] Run %s config version is not speculative, skipping (speculative=%v)", run.ID, configVersion.Speculative)
		return
	}
	if configVersion.CommitHash == "" {
		logger.Infof("[STATUS_CHECK] Run %s config version has no commit hash, skipping", run.ID)
		return
	}

	logger.Infof("[STATUS_CHECK] Run %s is a PR run (speculative=true, commit=%s), proceeding with status check update", run.ID, configVersion.CommitHash)

	// Get workspace to find VCS connection (preload Project and Organization for URL generation)
	workspace, err := workspaceRepo.GetByID(run.WorkspaceID)
	if err != nil {
		logger.Warnf("[STATUS_CHECK] Failed to get workspace %s for run %s: %v", run.WorkspaceID, run.ID, err)
		return
	}
	if workspace.VCSConnectionID == nil {
		logger.Infof("[STATUS_CHECK] Workspace %s has no VCS connection, skipping status check update", workspace.ID)
		return
	}

	// Get VCS connection
	vcsConn, err := vcsConnectionRepo.GetByID(*workspace.VCSConnectionID)
	if err != nil || vcsConn == nil {
		logger.Warnf("[STATUS_CHECK] Failed to get VCS connection %s for workspace %s: %v", *workspace.VCSConnectionID, workspace.ID, err)
		return
	}

	// Extract owner and repo from workspace VCS repository
	if workspace.VCSRepository == "" {
		logger.Infof("[STATUS_CHECK] Workspace %s has no VCS repository set, skipping", workspace.ID)
		return
	}
	parts := strings.SplitN(workspace.VCSRepository, "/", 2)
	if len(parts) != 2 {
		logger.Warnf("[STATUS_CHECK] Invalid VCS repository format '%s' for workspace %s, skipping", workspace.VCSRepository, workspace.ID)
		return
	}
	owner := parts[0]
	repo := parts[1]

	// Determine status check state and description based on run status
	state, description, ok := mapRunStatusToCheckState(run)
	if !ok {
		return
	}

	// Generate target URL
	targetURL := buildTargetURL(workspace, run.ID)

	// Status check context: terraform-plan/<workspace-name>
	statusContext := fmt.Sprintf("terraform-plan/%s", workspace.Name)

	// Dispatch to the correct provider
	switch vcsConn.Provider {
	case models.VCSProviderGitHub:
		updateGitHubStatusCheck(ctx, run, vcsConn, owner, repo, configVersion.CommitHash, statusContext, state, description, targetURL, statusService)
	case models.VCSProviderAzureDevOps:
		updateADOStatusCheck(ctx, run, vcsConn, configVersion, owner, repo, statusContext, state, description, targetURL, adoStatusService, vcsRegistry)
	case models.VCSProviderGitLab, models.VCSProviderBitbucket:
		logger.Infof("[STATUS_CHECK] Provider %s does not support PR status checks yet, skipping for run %s", vcsConn.Provider, run.ID)
	default:
		logger.Infof("[STATUS_CHECK] Provider %s does not support PR status checks, skipping for run %s", vcsConn.Provider, run.ID)
	}
}

// updateGitHubStatusCheck posts a commit status via the GitHub Status API.
func updateGitHubStatusCheck(
	ctx context.Context,
	run *models.Run,
	vcsConn *models.VCSConnection,
	owner, repo, commitHash, statusContext string,
	state vcs.StatusState,
	description, targetURL string,
	statusService *vcs.GitHubStatusService,
) {
	if statusService == nil {
		logger.Infof("[STATUS_CHECK] GitHub status service is nil, skipping for run %s", run.ID)
		return
	}
	if vcsConn.InstallationID == "" {
		logger.Infof("[STATUS_CHECK] VCS connection %s has no installation ID, skipping GitHub status check", vcsConn.ID)
		return
	}

	logger.Infof("[STATUS_CHECK] Updating GitHub status check for run %s: state=%s, context=%s, sha=%s, repo=%s/%s", run.ID, state, statusContext, commitHash, owner, repo)
	err := statusService.UpdateStatusCheck(ctx, vcsConn.InstallationID, owner, repo, commitHash, statusContext, state, description, targetURL)
	if err != nil {
		logger.Warnf("[STATUS_CHECK] ERROR - Failed to update GitHub status check for run %s: %v", run.ID, err)
	} else {
		logger.Infof("[STATUS_CHECK] SUCCESS - Updated GitHub status check for run %s: state=%s, description=%s", run.ID, state, description)
	}
}

// updateADOStatusCheck posts a PR status via the Azure DevOps Pull Request Status API.
func updateADOStatusCheck(
	ctx context.Context,
	run *models.Run,
	vcsConn *models.VCSConnection,
	configVersion *models.ConfigurationVersion,
	project, repo, statusContext string,
	state vcs.StatusState,
	description, targetURL string,
	adoStatusService *vcs.AzureDevOpsStatusService,
	vcsRegistry *vcs.ProviderRegistry,
) {
	if adoStatusService == nil {
		logger.Infof("[STATUS_CHECK] Azure DevOps status service is nil, skipping for run %s", run.ID)
		return
	}

	// Get PR number from the dedicated field
	prNumber := configVersion.PRNumber
	if prNumber == 0 {
		// Fallback: try to extract from committer field for backward compatibility (format: "PR #N")
		prNumber = extractPRNumber(configVersion.Committer)
	}
	if prNumber == 0 {
		logger.Infof("[STATUS_CHECK] No PR number found for run %s (committer=%q, pr_number=%d), skipping ADO status", run.ID, configVersion.Committer, configVersion.PRNumber)
		return
	}

	// Get a fresh token for the ADO API call
	var token string
	if vcsRegistry != nil {
		if provider, pErr := vcsRegistry.GetProvider(vcsConn); pErr == nil {
			freshToken, tErr := provider.GetFreshToken(ctx, vcsConn)
			if tErr == nil {
				token = freshToken
			} else {
				logger.Warnf("[STATUS_CHECK] Failed to get fresh token for ADO connection %s: %v", vcsConn.ID, tErr)
			}
		}
	}
	if token == "" {
		token = vcsConn.AccessToken
	}

	logger.Infof("[STATUS_CHECK] Updating ADO PR status for run %s: state=%s, context=%s, PR=#%d, repo=%s/%s/%s",
		run.ID, state, statusContext, prNumber, vcsConn.AccountName, project, repo)

	err := adoStatusService.CreateOrUpdatePRStatus(ctx, token, vcsConn.AccountName, project, repo, prNumber, state, statusContext, description, targetURL)
	if err != nil {
		logger.Warnf("[STATUS_CHECK] ERROR - Failed to update ADO PR status for run %s: %v", run.ID, err)
	} else {
		logger.Infof("[STATUS_CHECK] SUCCESS - Updated ADO PR status for run %s: state=%s, description=%s", run.ID, state, description)
	}
}

// extractPRNumber parses a PR number from the committer field (format: "PR #N").
func extractPRNumber(committer string) int {
	if !strings.HasPrefix(committer, "PR #") {
		return 0
	}
	numStr := strings.TrimPrefix(committer, "PR #")
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return 0
	}
	return n
}

// mapRunStatusToCheckState maps a run status to a VCS status check state and description.
// Returns false if the status should not trigger a status check update.
func mapRunStatusToCheckState(run *models.Run) (vcs.StatusState, string, bool) {
	//nolint:exhaustive // Only plan-only run statuses are valid for PR status checks (speculative runs)
	switch run.Status {
	case models.RunStatusPending:
		return vcs.StatusStatePending, "Terraform plan is queued", true
	case models.RunStatusPlanning:
		return vcs.StatusStatePending, "Terraform plan is running", true
	case models.RunStatusPlanned:
		return vcs.StatusStateSuccess, buildPlannedDescription(run), true
	case models.RunStatusFailed:
		desc := "Terraform plan failed"
		if run.ErrorMessage != "" {
			desc = fmt.Sprintf("Terraform plan failed: %s", run.ErrorMessage)
			if len(desc) > 140 {
				desc = desc[:137] + "..."
			}
		}
		return vcs.StatusStateFailure, desc, true
	case models.RunStatusCancelled:
		return vcs.StatusStateError, "Terraform plan was cancelled", true
	default:
		return "", "", false
	}
}

// buildPlannedDescription formats plan impact counts into a description string.
func buildPlannedDescription(run *models.Run) string {
	addCount := 0
	changeCount := 0
	destroyCount := 0
	if run.PlanOutput != nil {
		if add, ok := run.PlanOutput["AddCount"].(float64); ok {
			addCount = int(add)
		}
		if change, ok := run.PlanOutput["ChangeCount"].(float64); ok {
			changeCount = int(change)
		}
		if destroy, ok := run.PlanOutput["DestroyCount"].(float64); ok {
			destroyCount = int(destroy)
		}
	}
	parts := make([]string, 0)
	if addCount > 0 {
		parts = append(parts, fmt.Sprintf("+%d", addCount))
	}
	if changeCount > 0 {
		parts = append(parts, fmt.Sprintf("~%d", changeCount))
	}
	if destroyCount > 0 {
		parts = append(parts, fmt.Sprintf("-%d", destroyCount))
	}
	if len(parts) > 0 {
		return "planned: " + strings.Join(parts, ", ")
	}
	return "planned: no changes"
}

// buildTargetURL generates the run detail URL for status check links.
func buildTargetURL(workspace *models.Workspace, runID string) string {
	baseURL := os.Getenv("STACKWEAVER_BASE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:5173"
	}
	if workspace.Project.ID != (uuid.UUID{}) && workspace.Project.Organization.ID != (uuid.UUID{}) && workspace.Project.Organization.Name != "" {
		return fmt.Sprintf("%s/app/%s/workspaces/%s/runs/%s", baseURL, workspace.Project.Organization.Name, workspace.Name, runID)
	}
	logger.Warnf("[STATUS_CHECK] Workspace %s missing org/project info, using fallback URL with workspace ID", workspace.ID)
	return fmt.Sprintf("%s/workspaces/%s/runs/%s", baseURL, workspace.ID, runID)
}
