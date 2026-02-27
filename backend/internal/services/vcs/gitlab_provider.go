// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package vcs

import (
	"context"
	"fmt"

	"github.com/iac-platform/backend/internal/models"
)

// GitLabProvider implements ProviderService for GitLab repositories.
// This is a stub implementation — full support is coming in a future release.
type GitLabProvider struct{}

func (p *GitLabProvider) GetFreshToken(_ context.Context, conn *models.VCSConnection) (string, error) {
	return conn.AccessToken, nil
}

func (p *GitLabProvider) BuildCloneURL(_ *models.VCSConnection, token, repoPath string) string {
	if token != "" {
		return fmt.Sprintf("https://oauth2:%s@gitlab.com/%s.git", token, repoPath)
	}
	return fmt.Sprintf("https://gitlab.com/%s.git", repoPath)
}

func (p *GitLabProvider) ListRepositories(_ context.Context, _ *models.VCSConnection, _, _ int) ([]Repository, error) {
	return nil, fmt.Errorf("not implemented for GitLab")
}

func (p *GitLabProvider) ListBranches(_ context.Context, _ *models.VCSConnection, _, _ string, _, _ int) ([]Branch, error) {
	return nil, fmt.Errorf("not implemented for GitLab")
}

func (p *GitLabProvider) GetFileContent(_ context.Context, _ *models.VCSConnection, _, _, _, _ string) (string, error) {
	return "", fmt.Errorf("not implemented for GitLab")
}

func (p *GitLabProvider) ListFiles(_ context.Context, _ *models.VCSConnection, _, _, _ string, _ []string) ([]string, error) {
	return nil, fmt.Errorf("not implemented for GitLab")
}

func (p *GitLabProvider) ValidateWebhook(_ []byte, _, _ string) error {
	return nil
}

func (p *GitLabProvider) ParseWebhookPayload(_ []byte) (*WebhookPayload, error) {
	return nil, fmt.Errorf("not implemented for GitLab")
}

func (p *GitLabProvider) RegisterWebhooksForRepo(_ context.Context, _ *models.VCSConnection, _, _, _ string) error {
	return nil
}
