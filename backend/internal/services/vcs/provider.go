// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package vcs

import (
	"context"

	"github.com/iac-platform/backend/internal/models"
)

// Repository represents a VCS repository
type Repository struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Description   string `json:"description"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	URL           string `json:"url"`
	CloneURL      string `json:"clone_url"`
	SSHURL        string `json:"ssh_url"`
}

// Branch represents a VCS branch
type Branch struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
		URL string `json:"url"`
	} `json:"commit"`
	Protected bool `json:"protected"`
}

// WebhookPayload is a normalized representation of a push or PR event from any provider.
type WebhookPayload struct {
	EventType    string   `json:"event_type"`    // "push" or "pull_request"
	Repository   string   `json:"repository"`    // full name, e.g. "owner/repo" or "project/repo"
	Branch       string   `json:"branch"`        // branch name (without refs/heads/ prefix)
	Commit       string   `json:"commit"`        // commit SHA
	Committer    string   `json:"committer"`     // "Name <email>"
	HeadBranch   string   `json:"head_branch"`   // PR head branch (pull_request events only)
	BaseBranch   string   `json:"base_branch"`   // PR base branch (pull_request events only)
	PRNumber     int      `json:"pr_number"`     // PR number (pull_request events only)
	ChangedFiles []string `json:"changed_files"` // flat list of changed file paths
}

// ProviderService is the interface implemented by all VCS providers.
type ProviderService interface {
	// ListRepositories lists repositories accessible via this connection.
	ListRepositories(ctx context.Context, conn *models.VCSConnection, page, perPage int) ([]Repository, error)

	// ListBranches lists branches for a repository.
	// owner = organization/user/project; repo = repository name.
	ListBranches(ctx context.Context, conn *models.VCSConnection, owner, repo string, page, perPage int) ([]Branch, error)

	// GetFileContent retrieves the content of a file from a repository.
	GetFileContent(ctx context.Context, conn *models.VCSConnection, owner, repo, path, ref string) (string, error)

	// ListFiles lists files in a repository whose names match any of the given extensions.
	ListFiles(ctx context.Context, conn *models.VCSConnection, owner, repo, ref string, extensions []string) ([]string, error)

	// GetFreshToken returns a valid access token, refreshing via OAuth if necessary.
	GetFreshToken(ctx context.Context, conn *models.VCSConnection) (string, error)

	// BuildCloneURL returns a provider-specific HTTPS clone URL with embedded token authentication.
	// repoPath is the provider-specific repository path (e.g. "owner/repo" for GitHub,
	// "project/repo" for Azure DevOps).
	BuildCloneURL(conn *models.VCSConnection, token, repoPath string) string

	// ValidateWebhook validates a webhook signature. Returns nil if valid or if secret is empty.
	ValidateWebhook(payload []byte, signature, secret string) error

	// ParseWebhookPayload parses a provider-specific webhook payload into a normalized WebhookPayload.
	ParseWebhookPayload(payload []byte) (*WebhookPayload, error)

	// RegisterWebhooksForRepo registers webhook subscriptions for a specific repository so that
	// push and pull request events are automatically delivered to Stackweaver.
	// Called when a workspace is linked to a repository — NOT during the initial OAuth flow.
	// webhookBaseURL is the publicly accessible base URL of the Stackweaver API
	// (e.g. "https://stackweaver.example.com:8022"). The provider appends the appropriate path.
	// projectName and repoName identify the repository within the provider
	// (for Azure DevOps: ADO project name and repository name; unused for GitHub which manages
	// webhooks via the App installation).
	// Implementations must be idempotent — existing subscriptions for the same repo must not
	// be duplicated.
	// Returns nil on success or if webhook registration is not applicable for this provider.
	RegisterWebhooksForRepo(ctx context.Context, conn *models.VCSConnection, webhookBaseURL, projectName, repoName string) error
}
