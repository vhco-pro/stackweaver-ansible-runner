// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package vcs

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/iac-platform/backend/internal/models"
)

// GitHubProvider implements ProviderService for GitHub repositories.
// It supports both GitHub App installations (with InstallationID) and PAT-based connections.
type GitHubProvider struct {
	manager *GitHubAppManager
}

// GetFreshToken returns a valid GitHub access token.
// For GitHub App connections, it generates a short-lived installation token.
// For PAT connections, it returns the stored access token.
func (p *GitHubProvider) GetFreshToken(ctx context.Context, conn *models.VCSConnection) (string, error) {
	if conn.InstallationID != "" {
		if p.manager == nil || !p.manager.IsEnabled() {
			return "", fmt.Errorf("GitHub App is not configured")
		}
		svc := p.manager.GetService()
		return svc.GenerateInstallationToken(ctx, conn.InstallationID)
	}
	return conn.AccessToken, nil
}

// BuildCloneURL returns a GitHub HTTPS clone URL with x-access-token authentication.
func (p *GitHubProvider) BuildCloneURL(_ *models.VCSConnection, token, repoPath string) string {
	if token != "" {
		return fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", token, repoPath)
	}
	return fmt.Sprintf("https://github.com/%s.git", repoPath)
}

// ListRepositories lists repositories accessible to the GitHub App installation or via PAT.
func (p *GitHubProvider) ListRepositories(ctx context.Context, conn *models.VCSConnection, page, perPage int) ([]Repository, error) {
	if conn.InstallationID != "" {
		if p.manager == nil || !p.manager.IsEnabled() {
			return nil, fmt.Errorf("GitHub App is not configured")
		}
		svc := p.manager.GetService()
		return svc.ListRepositories(ctx, conn.InstallationID, page, perPage)
	}
	// PAT-based connection
	svc := NewGitHubService(conn.AccessToken) //nolint:contextcheck
	return svc.ListRepositories(ctx, conn.AccountType, conn.AccountName, page, perPage)
}

// ListBranches lists branches for a repository.
func (p *GitHubProvider) ListBranches(ctx context.Context, conn *models.VCSConnection, owner, repo string, page, perPage int) ([]Branch, error) {
	if conn.InstallationID != "" {
		if p.manager == nil || !p.manager.IsEnabled() {
			return nil, fmt.Errorf("GitHub App is not configured")
		}
		svc := p.manager.GetService()
		return svc.ListBranches(ctx, conn.InstallationID, owner, repo, page, perPage)
	}
	svc := NewGitHubService(conn.AccessToken) //nolint:contextcheck
	return svc.ListBranches(ctx, owner, repo, page, perPage)
}

// GetFileContent retrieves the content of a file from a GitHub repository.
func (p *GitHubProvider) GetFileContent(ctx context.Context, conn *models.VCSConnection, owner, repo, path, ref string) (string, error) {
	if conn.InstallationID == "" {
		return "", fmt.Errorf("file content retrieval requires a GitHub App installation")
	}
	if p.manager == nil || !p.manager.IsEnabled() {
		return "", fmt.Errorf("GitHub App is not configured")
	}
	svc := p.manager.GetService()
	return svc.GetFileContent(ctx, conn.InstallationID, owner, repo, path, ref)
}

// ListFiles lists files in a repository matching the given extensions.
// Routes to ListYamlFiles or ListInventoryFiles based on the requested extensions.
func (p *GitHubProvider) ListFiles(ctx context.Context, conn *models.VCSConnection, owner, repo, ref string, extensions []string) ([]string, error) {
	if conn.InstallationID == "" {
		return nil, fmt.Errorf("file listing requires a GitHub App installation")
	}
	if p.manager == nil || !p.manager.IsEnabled() {
		return nil, fmt.Errorf("GitHub App is not configured")
	}
	svc := p.manager.GetService()

	// Check which set of extensions is requested
	hasInventoryExt := false
	for _, ext := range extensions {
		if ext == ".ini" || ext == ".json" {
			hasInventoryExt = true
			break
		}
	}
	if hasInventoryExt {
		return svc.ListInventoryFiles(ctx, conn.InstallationID, owner, repo, ref)
	}
	return svc.ListYamlFiles(ctx, conn.InstallationID, owner, repo, ref)
}

// ValidateWebhook validates a GitHub webhook HMAC-SHA256 signature.
func (p *GitHubProvider) ValidateWebhook(payload []byte, signature, secret string) error {
	if secret == "" {
		return nil
	}
	expected := "sha256=" + computeHMACSHA256(payload, secret)
	if !hmac.Equal([]byte(signature), []byte(expected)) {
		return fmt.Errorf("webhook signature mismatch")
	}
	return nil
}

// ParseWebhookPayload parses a GitHub push/PR webhook payload into a normalized WebhookPayload.
func (p *GitHubProvider) ParseWebhookPayload(payload []byte) (*WebhookPayload, error) {
	var event struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Commits []struct {
			ID       string   `json:"id"`
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
		PullRequest struct {
			Number int `json:"number"`
			Head   struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			} `json:"head"`
			Base struct {
				Ref string `json:"ref"`
			} `json:"base"`
		} `json:"pull_request"`
	}

	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, fmt.Errorf("failed to parse GitHub webhook payload: %w", err)
	}

	wp := &WebhookPayload{
		Repository: event.Repository.FullName,
	}

	if event.Ref != "" {
		wp.EventType = "push"
		wp.Branch = strings.TrimPrefix(event.Ref, "refs/heads/")
		wp.Commit = event.After

		var changedFiles []string
		for _, commit := range event.Commits {
			changedFiles = append(changedFiles, commit.Added...)
			changedFiles = append(changedFiles, commit.Modified...)
			changedFiles = append(changedFiles, commit.Removed...)
		}
		wp.ChangedFiles = changedFiles

		if len(event.Commits) > 0 {
			last := event.Commits[len(event.Commits)-1]
			wp.Committer = fmt.Sprintf("%s <%s>", last.Author.Name, last.Author.Email)
		} else if event.Pusher.Email != "" {
			wp.Committer = fmt.Sprintf("%s <%s>", event.Pusher.Name, event.Pusher.Email)
		}
	} else if event.PullRequest.Number > 0 {
		wp.EventType = "pull_request"
		wp.PRNumber = event.PullRequest.Number
		wp.HeadBranch = event.PullRequest.Head.Ref
		wp.BaseBranch = event.PullRequest.Base.Ref
		wp.Commit = event.PullRequest.Head.SHA
	}

	return wp, nil
}

// RegisterWebhooksForRepo is a no-op for GitHub — webhook subscriptions are managed via the
// GitHub App installation flow and do not need to be registered per-repository.
func (p *GitHubProvider) RegisterWebhooksForRepo(_ context.Context, _ *models.VCSConnection, _, _, _ string) error {
	return nil
}

// computeHMACSHA256 returns the hex-encoded HMAC-SHA256 of payload with secret.
func computeHMACSHA256(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}
