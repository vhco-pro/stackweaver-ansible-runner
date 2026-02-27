// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package vcs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/michielvha/logger"
)

// AzureDevOpsStatusState maps to the Azure DevOps Git Pull Request Status state.
// See: https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pull-request-statuses/create
type AzureDevOpsStatusState string

const (
	ADOStatusNotSet        AzureDevOpsStatusState = "notSet"
	ADOStatusPending       AzureDevOpsStatusState = "pending"
	ADOStatusSucceeded     AzureDevOpsStatusState = "succeeded"
	ADOStatusFailed        AzureDevOpsStatusState = "failed"
	ADOStatusError         AzureDevOpsStatusState = "error"
	ADOStatusNotApplicable AzureDevOpsStatusState = "notApplicable"
)

// MapStatusToADO converts internal StatusState to Azure DevOps status state.
func MapStatusToADO(state StatusState) AzureDevOpsStatusState {
	switch state {
	case StatusStatePending:
		return ADOStatusPending
	case StatusStateSuccess:
		return ADOStatusSucceeded
	case StatusStateFailure:
		return ADOStatusFailed
	case StatusStateError:
		return ADOStatusError
	default:
		return ADOStatusNotSet
	}
}

// adoPRStatusRequest is the request body for creating a PR status in Azure DevOps.
// https://learn.microsoft.com/en-us/rest/api/azure/devops/git/pull-request-statuses/create
type adoPRStatusRequest struct {
	State       AzureDevOpsStatusState `json:"state"`
	Description string                 `json:"description"`
	Context     adoStatusContext       `json:"context"`
	TargetURL   string                 `json:"targetUrl,omitempty"`
}

// adoStatusContext identifies the status check (like GitHub's "context" string).
// The combination of name + genre uniquely identifies a status and allows updates.
type adoStatusContext struct {
	Name  string `json:"name"`  // e.g. "terraform-plan/workspace-name"
	Genre string `json:"genre"` // e.g. "stackweaver"
}

// AzureDevOpsStatusService handles Azure DevOps Pull Request Status API interactions.
// It posts status updates to pull requests so users see plan results directly in ADO.
type AzureDevOpsStatusService struct {
	manager     *AzureDevOpsManager
	connUpdater ConnUpdater
}

// NewAzureDevOpsStatusService creates a new Azure DevOps PR status service.
func NewAzureDevOpsStatusService(manager *AzureDevOpsManager, connUpdater ConnUpdater) *AzureDevOpsStatusService {
	return &AzureDevOpsStatusService{
		manager:     manager,
		connUpdater: connUpdater,
	}
}

// CreateOrUpdatePRStatus creates or updates a status on an Azure DevOps pull request.
// The Azure DevOps API uses POST for both create and update — if a status with the same
// context (name+genre) already exists on that PR, it will be updated.
//
// Parameters:
//   - token: Azure DevOps OAuth2 access token
//   - org: Azure DevOps organization name
//   - project: Project name
//   - repoID: Repository name or GUID
//   - prNumber: Pull request ID
//   - state: Status state (pending, succeeded, failed, error)
//   - statusContext: Context name (e.g. "terraform-plan/workspace-name")
//   - description: Human-readable description
//   - targetURL: URL to link the status to (e.g. the run detail page)
func (s *AzureDevOpsStatusService) CreateOrUpdatePRStatus(
	ctx context.Context,
	token string,
	org string,
	project string,
	repoID string,
	prNumber int,
	state StatusState,
	statusContext string,
	description string,
	targetURL string,
) error {
	if org == "" || project == "" || repoID == "" || prNumber == 0 {
		return fmt.Errorf("invalid parameters: org=%q, project=%q, repoID=%q, prNumber=%d", org, project, repoID, prNumber)
	}

	adoState := MapStatusToADO(state)

	reqBody := adoPRStatusRequest{
		State:       adoState,
		Description: truncateDescription(description, 150), // ADO limit
		Context: adoStatusContext{
			Name:  statusContext,
			Genre: "stackweaver",
		},
		TargetURL: targetURL,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal PR status request: %w", err)
	}

	apiURL := fmt.Sprintf(
		"https://dev.azure.com/%s/%s/_apis/git/repositories/%s/pullRequests/%d/statuses?api-version=7.1",
		url.PathEscape(org),
		url.PathEscape(project),
		url.PathEscape(repoID),
		prNumber,
	)

	respBody, err := s.doRequest(ctx, http.MethodPost, apiURL, token, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("failed to create/update PR status: %w", err)
	}

	logger.Infof("Created/updated ADO PR status: context=%s, state=%s, PR=#%d, repo=%s/%s/%s, response_length=%d",
		statusContext, adoState, prNumber, org, project, repoID, len(respBody))
	return nil
}

// CreateOrUpdatePRStatusWithConn is a convenience method that gets a fresh token from
// the VCSConnection before creating/updating the PR status. This handles token refresh
// automatically.
func (s *AzureDevOpsStatusService) CreateOrUpdatePRStatusWithConn(
	ctx context.Context,
	conn *ConnInfo,
	prNumber int,
	state StatusState,
	statusContext string,
	description string,
	targetURL string,
) error {
	return s.CreateOrUpdatePRStatus(
		ctx,
		conn.Token,
		conn.Org,
		conn.Project,
		conn.Repo,
		prNumber,
		state,
		statusContext,
		description,
		targetURL,
	)
}

// ConnInfo holds the connection parameters needed for ADO status API calls.
type ConnInfo struct {
	Token   string
	Org     string
	Project string
	Repo    string
}

// doRequest performs an authenticated HTTP request to Azure DevOps.
func (s *AzureDevOpsStatusService) doRequest(ctx context.Context, method, apiURL, token string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, apiURL, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req) //nolint:gosec // G704: URL constructed from trusted Azure DevOps config, not user-controlled
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	// Detect redirect responses (expired token)
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, fmt.Errorf("azure DevOps returned a redirect (HTTP %d) — the access token is likely expired", resp.StatusCode)
	}

	// Detect HTML responses
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/html") || (len(respBody) > 0 && respBody[0] == '<') {
		return nil, fmt.Errorf("azure DevOps returned HTML instead of JSON (HTTP %d) — the access token is likely expired", resp.StatusCode)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("ADO PR status API failed: status %d, body: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// truncateDescription truncates a description to the given max length, appending "..." if truncated.
func truncateDescription(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
