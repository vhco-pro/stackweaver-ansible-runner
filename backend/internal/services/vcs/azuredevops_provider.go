// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package vcs

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // G401: Azure DevOps Service Hooks use HMAC-SHA1 for webhook signatures
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/iac-platform/backend/internal/models"
	"github.com/michielvha/logger"
)

// adoRepoInfo holds the GUIDs for a repository and its parent project,
// as returned by the Azure DevOps Git Repositories API.
type adoRepoInfo struct {
	ID      string `json:"id"`
	Project struct {
		ID string `json:"id"`
	} `json:"project"`
}

// AzureDevOpsProvider implements ProviderService for Azure DevOps repositories.
type AzureDevOpsProvider struct {
	manager     *AzureDevOpsManager
	connUpdater ConnUpdater
}

// NewAzureDevOpsProvider creates a new AzureDevOpsProvider with the given manager.
func NewAzureDevOpsProvider(manager *AzureDevOpsManager) *AzureDevOpsProvider {
	return &AzureDevOpsProvider{manager: manager}
}

// GetFreshToken returns a valid Azure DevOps access token.
// If the token needs refreshing and a refresh token is available, it refreshes automatically.
// Refreshed tokens are persisted to the database via the connUpdater callback if available.
// Falls back to the stored access token if refresh fails or manager is not configured.
func (p *AzureDevOpsProvider) GetFreshToken(ctx context.Context, conn *models.VCSConnection) (string, error) {
	if p.manager != nil && p.manager.IsEnabled() && conn.NeedsRefresh() && conn.RefreshToken != "" {
		result, err := p.manager.RefreshToken(ctx, conn.RefreshToken)
		if err != nil {
			logger.Warnf("Azure DevOps token refresh failed for connection %s: %v (falling back to stored token)", conn.ID, err)
		} else {
			// Update the connection in-memory
			conn.AccessToken = result.AccessToken
			if result.RefreshToken != "" {
				conn.RefreshToken = result.RefreshToken
			}
			if !result.ExpiresAt.IsZero() {
				conn.TokenExpiresAt = &result.ExpiresAt
			}
			// Persist to database so subsequent calls use the fresh token
			if p.connUpdater != nil {
				if updErr := p.connUpdater(conn); updErr != nil {
					logger.Warnf("Failed to persist refreshed Azure DevOps token for connection %s: %v", conn.ID, updErr)
				}
			}
			return result.AccessToken, nil
		}
		// Fall through to stored token on refresh failure
	}
	return conn.AccessToken, nil
}

// BuildCloneURL returns an Azure DevOps HTTPS clone URL with OAuth2 token authentication.
// repoPath must be in "project/repo" format; conn.AccountName is the Azure DevOps org name.
func (p *AzureDevOpsProvider) BuildCloneURL(conn *models.VCSConnection, token, repoPath string) string {
	org := conn.AccountName
	parts := strings.SplitN(repoPath, "/", 2)
	if len(parts) != 2 {
		// Best-effort fallback: treat as a single repo name under the org
		if token != "" {
			return fmt.Sprintf("https://oauth2:%s@dev.azure.com/%s/%s", token, org, repoPath)
		}
		return fmt.Sprintf("https://dev.azure.com/%s/%s", org, repoPath)
	}
	project, repo := parts[0], parts[1]
	if token != "" {
		return fmt.Sprintf("https://oauth2:%s@dev.azure.com/%s/%s/_git/%s", token, org, project, repo)
	}
	return fmt.Sprintf("https://dev.azure.com/%s/%s/_git/%s", org, project, repo)
}

// ListRepositories lists all repositories across all projects in the Azure DevOps organization.
func (p *AzureDevOpsProvider) ListRepositories(ctx context.Context, conn *models.VCSConnection, page, perPage int) ([]Repository, error) {
	token, err := p.GetFreshToken(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("failed to get token: %w", err)
	}

	org := conn.AccountName
	apiURL := fmt.Sprintf("https://dev.azure.com/%s/_apis/git/repositories?api-version=7.1", org)

	body, err := p.doRequest(ctx, http.MethodGet, apiURL, token)
	if err != nil {
		return nil, fmt.Errorf("failed to list repositories: %w", err)
	}

	var response struct {
		Value []struct {
			Name          string `json:"name"`
			RemoteURL     string `json:"remoteUrl"`
			DefaultBranch string `json:"defaultBranch"`
			Project       struct {
				Name string `json:"name"`
			} `json:"project"`
		} `json:"value"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("failed to parse repositories: %w", err)
	}

	// Manual pagination (ADO returns all repos at once)
	start := (page - 1) * perPage
	if start >= len(response.Value) {
		return []Repository{}, nil
	}
	end := min(start+perPage, len(response.Value))

	result := make([]Repository, 0, end-start)
	for _, r := range response.Value[start:end] {
		defaultBranch := strings.TrimPrefix(r.DefaultBranch, "refs/heads/")
		if defaultBranch == "" {
			defaultBranch = "main"
		}
		result = append(result, Repository{
			Name:          r.Name,
			FullName:      r.Project.Name + "/" + r.Name,
			DefaultBranch: defaultBranch,
			URL:           r.RemoteURL,
			CloneURL:      r.RemoteURL,
		})
	}
	return result, nil
}

// ListBranches lists branches for a repository.
// owner = project name, repo = repository name.
func (p *AzureDevOpsProvider) ListBranches(ctx context.Context, conn *models.VCSConnection, owner, repo string, page, perPage int) ([]Branch, error) {
	token, err := p.GetFreshToken(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("failed to get token: %w", err)
	}

	org := conn.AccountName
	apiURL := fmt.Sprintf(
		"https://dev.azure.com/%s/%s/_apis/git/repositories/%s/refs?filter=heads/&api-version=7.1",
		org, owner, repo,
	)

	body, err := p.doRequest(ctx, http.MethodGet, apiURL, token)
	if err != nil {
		return nil, fmt.Errorf("failed to list branches: %w", err)
	}

	var response struct {
		Value []struct {
			Name     string `json:"name"`
			ObjectID string `json:"objectId"`
		} `json:"value"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("failed to parse branches: %w", err)
	}

	result := make([]Branch, 0, len(response.Value))
	for _, ref := range response.Value {
		branchName := strings.TrimPrefix(ref.Name, "refs/heads/")
		b := Branch{Name: branchName}
		b.Commit.SHA = ref.ObjectID
		result = append(result, b)
	}
	return result, nil
}

// GetFileContent retrieves the raw content of a file from a repository.
// owner = project name, repo = repository name.
func (p *AzureDevOpsProvider) GetFileContent(ctx context.Context, conn *models.VCSConnection, owner, repo, path, ref string) (string, error) {
	token, err := p.GetFreshToken(ctx, conn)
	if err != nil {
		return "", fmt.Errorf("failed to get token: %w", err)
	}

	org := conn.AccountName
	apiURL := fmt.Sprintf(
		"https://dev.azure.com/%s/%s/_apis/git/repositories/%s/items?path=%s&$format=text&api-version=7.1",
		org, owner, repo, path,
	)
	if ref != "" {
		apiURL += "&versionDescriptor.version=" + ref
	}

	body, err := p.doRequest(ctx, http.MethodGet, apiURL, token)
	if err != nil {
		return "", fmt.Errorf("failed to get file content: %w", err)
	}
	return string(body), nil
}

// ListFiles lists all files in a repository matching the given extensions.
// owner = project name, repo = repository name.
func (p *AzureDevOpsProvider) ListFiles(ctx context.Context, conn *models.VCSConnection, owner, repo, ref string, extensions []string) ([]string, error) {
	token, err := p.GetFreshToken(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("failed to get token: %w", err)
	}

	org := conn.AccountName
	apiURL := fmt.Sprintf(
		"https://dev.azure.com/%s/%s/_apis/git/repositories/%s/items?recursionLevel=Full&api-version=7.1",
		org, owner, repo,
	)
	if ref != "" {
		apiURL += "&versionDescriptor.version=" + ref
	}

	body, err := p.doRequest(ctx, http.MethodGet, apiURL, token)
	if err != nil {
		return nil, fmt.Errorf("failed to list files: %w", err)
	}

	var response struct {
		Value []struct {
			Path     string `json:"path"`
			IsFolder bool   `json:"isFolder"`
		} `json:"value"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("failed to parse file listing: %w", err)
	}

	extSet := make(map[string]bool, len(extensions))
	for _, ext := range extensions {
		extSet[strings.ToLower(ext)] = true
	}

	var result []string
	for _, item := range response.Value {
		if item.IsFolder {
			continue
		}
		// Azure DevOps returns paths with a leading "/" (e.g. "/ansible-examples/playbooks/site.yml").
		// Strip it to normalize with GitHub paths (which have no leading slash).
		normalized := strings.TrimPrefix(item.Path, "/")
		lower := strings.ToLower(normalized)
		for ext := range extSet {
			if strings.HasSuffix(lower, ext) {
				result = append(result, normalized)
				break
			}
		}
	}
	return result, nil
}

// ValidateWebhook validates an Azure DevOps Service Hook webhook signature (HMAC-SHA1).
func (p *AzureDevOpsProvider) ValidateWebhook(payload []byte, signature, secret string) error {
	if secret == "" {
		return nil
	}
	mac := hmac.New(sha1.New, []byte(secret)) //nolint:gosec // G401: Azure DevOps uses SHA1
	mac.Write(payload)
	expected := "sha1=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(signature), []byte(expected)) {
		return fmt.Errorf("webhook signature mismatch")
	}
	return nil
}

// ParseWebhookPayload parses an Azure DevOps Service Hook event into a normalized WebhookPayload.
// Supports git.push, git.pullrequest.created, and git.pullrequest.updated event types.
func (p *AzureDevOpsProvider) ParseWebhookPayload(payload []byte) (*WebhookPayload, error) {
	// First, determine the event type
	var header struct {
		EventType string `json:"eventType"`
	}
	if err := json.Unmarshal(payload, &header); err != nil {
		return nil, fmt.Errorf("failed to parse Azure DevOps webhook payload: %w", err)
	}

	switch {
	case header.EventType == "git.push":
		return p.parsePushPayload(payload)
	case strings.HasPrefix(header.EventType, "git.pullrequest."):
		return p.parsePullRequestPayload(payload, header.EventType)
	default:
		return &WebhookPayload{
			EventType: header.EventType,
		}, nil
	}
}

// parsePushPayload parses an Azure DevOps git.push Service Hook event.
func (p *AzureDevOpsProvider) parsePushPayload(payload []byte) (*WebhookPayload, error) {
	var event struct {
		EventType string `json:"eventType"`
		Resource  struct {
			RefUpdates []struct {
				Name        string `json:"name"`
				NewObjectID string `json:"newObjectId"`
			} `json:"refUpdates"`
			Commits []struct {
				CommitID string `json:"commitId"`
				Author   struct {
					Name  string `json:"name"`
					Email string `json:"email"`
				} `json:"author"`
				Changes []struct {
					Item struct {
						Path string `json:"path"`
					} `json:"item"`
				} `json:"changes"`
			} `json:"commits"`
			Repository struct {
				Name    string `json:"name"`
				Project struct {
					Name string `json:"name"`
				} `json:"project"`
			} `json:"repository"`
		} `json:"resource"`
	}

	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, fmt.Errorf("failed to parse Azure DevOps push payload: %w", err)
	}

	wp := &WebhookPayload{
		EventType:  "push",
		Repository: event.Resource.Repository.Project.Name + "/" + event.Resource.Repository.Name,
	}

	if len(event.Resource.RefUpdates) > 0 {
		wp.Branch = strings.TrimPrefix(event.Resource.RefUpdates[0].Name, "refs/heads/")
		wp.Commit = event.Resource.RefUpdates[0].NewObjectID
	}

	if len(event.Resource.Commits) > 0 {
		first := event.Resource.Commits[0]
		if wp.Commit == "" {
			wp.Commit = first.CommitID
		}
		wp.Committer = fmt.Sprintf("%s <%s>", first.Author.Name, first.Author.Email)

		var changedFiles []string
		for _, commit := range event.Resource.Commits {
			for _, change := range commit.Changes {
				if change.Item.Path != "" {
					changedFiles = append(changedFiles, change.Item.Path)
				}
			}
		}
		wp.ChangedFiles = changedFiles
	}

	return wp, nil
}

// parsePullRequestPayload parses an Azure DevOps git.pullrequest.* Service Hook event.
func (p *AzureDevOpsProvider) parsePullRequestPayload(payload []byte, eventType string) (*WebhookPayload, error) {
	var event struct {
		Resource struct {
			PullRequestID         int    `json:"pullRequestId"`
			Status                string `json:"status"` // "active", "completed", "abandoned"
			SourceRefName         string `json:"sourceRefName"`
			TargetRefName         string `json:"targetRefName"`
			LastMergeSourceCommit struct {
				CommitID string `json:"commitId"`
			} `json:"lastMergeSourceCommit"`
			CreatedBy struct {
				DisplayName string `json:"displayName"`
				UniqueName  string `json:"uniqueName"` // email
			} `json:"createdBy"`
			Repository struct {
				Name    string `json:"name"`
				Project struct {
					Name string `json:"name"`
				} `json:"project"`
			} `json:"repository"`
		} `json:"resource"`
	}

	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, fmt.Errorf("failed to parse Azure DevOps pull request payload: %w", err)
	}

	// Only process active PRs (created, updated). Skip completed/abandoned.
	if event.Resource.Status != "active" && eventType != "git.pullrequest.created" {
		return &WebhookPayload{
			EventType: eventType, // Return as-is so handlers can decide to ignore
		}, nil
	}

	wp := &WebhookPayload{
		EventType:  "pull_request",
		Repository: event.Resource.Repository.Project.Name + "/" + event.Resource.Repository.Name,
		PRNumber:   event.Resource.PullRequestID,
		HeadBranch: strings.TrimPrefix(event.Resource.SourceRefName, "refs/heads/"),
		BaseBranch: strings.TrimPrefix(event.Resource.TargetRefName, "refs/heads/"),
		Commit:     event.Resource.LastMergeSourceCommit.CommitID,
		Committer:  fmt.Sprintf("%s <%s>", event.Resource.CreatedBy.DisplayName, event.Resource.CreatedBy.UniqueName),
	}

	return wp, nil
}

// RegisterWebhooksForRepo registers Service Hook subscriptions for a specific Azure DevOps
// repository so that push and pull request events are delivered to Stackweaver.
// It is called when a workspace is linked to a repository — not during the initial OAuth flow.
// The registration is idempotent: existing subscriptions for the same repo+event are skipped.
// If webhookBaseURL is empty, registration is skipped silently.
func (p *AzureDevOpsProvider) RegisterWebhooksForRepo(ctx context.Context, conn *models.VCSConnection, webhookBaseURL, projectName, repoName string) error {
	if webhookBaseURL == "" {
		return nil
	}

	token, err := p.GetFreshToken(ctx, conn)
	if err != nil {
		return fmt.Errorf("failed to get token for webhook registration: %w", err)
	}

	org := conn.AccountName
	webhookURL := strings.TrimRight(webhookBaseURL, "/") + "/api/v2/vcs-connections/azure-devops/webhook"

	// Resolve project and repository names to GUIDs — the Service Hooks API requires GUIDs.
	repoInfo, err := p.getRepoInfo(ctx, org, token, projectName, repoName)
	if err != nil {
		return fmt.Errorf("failed to resolve repository %s/%s: %w", projectName, repoName, err)
	}

	existing, err := p.listWebhookSubscriptionKeys(ctx, org, token, webhookURL)
	if err != nil {
		return fmt.Errorf("failed to list existing subscriptions: %w", err)
	}

	eventTypes := []string{"git.push", "git.pullrequest.created", "git.pullrequest.updated"}
	for _, eventType := range eventTypes {
		if existing[repoInfo.ID+"/"+eventType] {
			continue
		}
		if err := p.createServiceHook(ctx, org, token, repoInfo.Project.ID, repoInfo.ID, eventType, webhookURL); err != nil {
			return fmt.Errorf("failed to create %s subscription for %s/%s: %w", eventType, projectName, repoName, err)
		}
	}
	return nil
}

// getRepoInfo resolves project and repository names to their GUIDs using the ADO Git API.
// The returned adoRepoInfo contains both the repository GUID and its parent project GUID,
// which are required by the Service Hooks API.
func (p *AzureDevOpsProvider) getRepoInfo(ctx context.Context, org, token, projectName, repoName string) (*adoRepoInfo, error) {
	apiURL := fmt.Sprintf(
		"https://dev.azure.com/%s/%s/_apis/git/repositories/%s?api-version=7.1",
		url.PathEscape(org), url.PathEscape(projectName), url.PathEscape(repoName),
	)
	body, err := p.doRequest(ctx, http.MethodGet, apiURL, token)
	if err != nil {
		return nil, err
	}

	var info adoRepoInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("failed to parse repository response: %w", err)
	}
	if info.ID == "" {
		return nil, fmt.Errorf("repository not found: %s/%s", projectName, repoName)
	}
	return &info, nil
}

// listWebhookSubscriptionKeys returns a set of "repoGUID/eventType" keys for existing
// Service Hook subscriptions that already point at the given webhook URL.
// The repoGUID is taken from publisherInputs.repository, which is a GUID for targeted
// subscriptions or an empty string for broad (all-repo) subscriptions.
func (p *AzureDevOpsProvider) listWebhookSubscriptionKeys(ctx context.Context, org, token, webhookURL string) (map[string]bool, error) {
	apiURL := fmt.Sprintf("https://dev.azure.com/%s/_apis/hooks/subscriptions?api-version=7.1", org)
	body, err := p.doRequest(ctx, http.MethodGet, apiURL, token)
	if err != nil {
		return nil, err
	}

	var response struct {
		Value []struct {
			EventType       string `json:"eventType"`
			PublisherInputs struct {
				Repository string `json:"repository"` // repo GUID; empty = all repos
			} `json:"publisherInputs"`
			ConsumerInputs struct {
				URL string `json:"url"`
			} `json:"consumerInputs"`
		} `json:"value"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("failed to parse subscriptions response: %w", err)
	}

	existing := make(map[string]bool)
	for _, sub := range response.Value {
		if sub.ConsumerInputs.URL == webhookURL {
			existing[sub.PublisherInputs.Repository+"/"+sub.EventType] = true
		}
	}
	return existing, nil
}

// createServiceHook creates a single Azure DevOps Service Hook subscription scoped to a
// specific repository (identified by repoGUID) within a project (projectGUID).
func (p *AzureDevOpsProvider) createServiceHook(ctx context.Context, org, token, projectGUID, repoGUID, eventType, webhookURL string) error {
	type publisherInputs struct {
		ProjectID  string `json:"projectId"`
		Repository string `json:"repository"`
	}
	type consumerInputs struct {
		URL                    string `json:"url"`
		ResourceDetailsToSend  string `json:"resourceDetailsToSend"`
		MessagesToSend         string `json:"messagesToSend"`
		DetailedMessagesToSend string `json:"detailedMessagesToSend"`
	}
	type subscription struct {
		PublisherID      string          `json:"publisherId"`
		EventType        string          `json:"eventType"`
		PublisherInputs  publisherInputs `json:"publisherInputs"`
		ConsumerID       string          `json:"consumerId"`
		ConsumerActionID string          `json:"consumerActionId"`
		ConsumerInputs   consumerInputs  `json:"consumerInputs"`
	}

	sub := subscription{
		PublisherID:      "tfs",
		EventType:        eventType,
		PublisherInputs:  publisherInputs{ProjectID: projectGUID, Repository: repoGUID},
		ConsumerID:       "webHooks",
		ConsumerActionID: "httpRequest",
		ConsumerInputs: consumerInputs{
			URL:                    webhookURL,
			ResourceDetailsToSend:  "all",
			MessagesToSend:         "none",
			DetailedMessagesToSend: "none",
		},
	}

	bodyBytes, err := json.Marshal(sub)
	if err != nil {
		return fmt.Errorf("failed to marshal subscription: %w", err)
	}

	apiURL := fmt.Sprintf("https://dev.azure.com/%s/_apis/hooks/subscriptions?api-version=7.1", org)
	_, err = p.doRequestWithBody(ctx, http.MethodPost, apiURL, token, bytes.NewReader(bodyBytes))
	return err
}

// doRequest performs an authenticated GET/DELETE HTTP request to the Azure DevOps REST API.
func (p *AzureDevOpsProvider) doRequest(ctx context.Context, method, apiURL, token string) ([]byte, error) {
	return p.doRequestWithBody(ctx, method, apiURL, token, nil)
}

// doRequestWithBody performs an authenticated HTTP request with an optional body.
func (p *AzureDevOpsProvider) doRequestWithBody(ctx context.Context, method, apiURL, token string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, apiURL, body) //nolint:gosec // G107: URL constructed from trusted config
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// Don't follow redirects — API calls should never redirect.
	// Azure DevOps redirects to the sign-in page when the token is expired,
	// which returns a 200 with HTML that breaks JSON parsing.
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

	// Detect redirect responses (expired token → sign-in redirect)
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, fmt.Errorf("azure DevOps returned a redirect (HTTP %d) — the access token is likely expired or invalid; please reconnect your Azure DevOps VCS connection", resp.StatusCode)
	}

	// Detect HTML responses that slipped through (e.g. sign-in pages)
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/html") || (len(respBody) > 0 && respBody[0] == '<') {
		return nil, fmt.Errorf("azure DevOps returned HTML instead of JSON (HTTP %d) — the access token is likely expired or invalid; please reconnect your Azure DevOps VCS connection", resp.StatusCode)
	}

	if resp.StatusCode >= 400 {
		bodyStr := string(respBody)
		if resp.StatusCode == http.StatusForbidden && (strings.Contains(bodyStr, "AadUserStateException") || strings.Contains(bodyStr, "not been materialized")) {
			return nil, fmt.Errorf("azure_devops_identity_not_materialized: your Azure DevOps identity has not been activated in this organization. " +
				"Please sign in to https://dev.azure.com/ in a browser using the same Microsoft account you authorized with, then retry. " +
				"If you used a work/school account for the OAuth flow, make sure you sign in to Azure DevOps with that same work/school account — not a personal Microsoft account")
		}
		return nil, fmt.Errorf("API request failed: status %d, body: %s", resp.StatusCode, bodyStr)
	}
	return respBody, nil
}

// ValidateTokenAndOrg validates the access token by calling the global VSSPS profile API
// and then the org-specific connection data endpoint. Calling the profile API first can
// trigger Entra ID identity materialization in Azure DevOps.
// Returns nil if both calls succeed. Returns a descriptive error otherwise.
func (p *AzureDevOpsProvider) ValidateTokenAndOrg(ctx context.Context, token, org string) error {
	// Step 1: Call the global profile endpoint. This validates the token is for a real
	// Azure DevOps user and may trigger identity materialization for the user's account.
	profileURL := "https://app.vssps.visualstudio.com/_apis/profile/profiles/me?api-version=7.1"
	_, err := p.doRequest(ctx, http.MethodGet, profileURL, token)
	if err != nil {
		return fmt.Errorf("token validation failed against Azure DevOps profile API: %w", err)
	}

	// Step 2: Verify we can actually reach the specified organization.
	// Use _apis/connectionData which is a lightweight endpoint (no repo listing).
	if org != "" {
		connDataURL := fmt.Sprintf("https://dev.azure.com/%s/_apis/connectionData?connectOptions=0&api-version=7.1-preview", org)
		_, err = p.doRequest(ctx, http.MethodGet, connDataURL, token)
		if err != nil {
			return err
		}
	}
	return nil
}
