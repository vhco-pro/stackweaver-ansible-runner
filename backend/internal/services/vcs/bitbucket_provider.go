// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package vcs

import (
	"context"
	"fmt"

	"github.com/iac-platform/backend/internal/models"
)

// BitbucketProvider implements ProviderService for Bitbucket repositories.
// This is a stub implementation — full support is coming in a future release.
type BitbucketProvider struct{}

func (p *BitbucketProvider) GetFreshToken(_ context.Context, conn *models.VCSConnection) (string, error) {
	return conn.AccessToken, nil
}

func (p *BitbucketProvider) BuildCloneURL(_ *models.VCSConnection, token, repoPath string) string {
	if token != "" {
		return fmt.Sprintf("https://x-token-auth:%s@bitbucket.org/%s.git", token, repoPath)
	}
	return fmt.Sprintf("https://bitbucket.org/%s.git", repoPath)
}

func (p *BitbucketProvider) ListRepositories(_ context.Context, _ *models.VCSConnection, _, _ int) ([]Repository, error) {
	return nil, fmt.Errorf("not implemented for Bitbucket")
}

func (p *BitbucketProvider) ListBranches(_ context.Context, _ *models.VCSConnection, _, _ string, _, _ int) ([]Branch, error) {
	return nil, fmt.Errorf("not implemented for Bitbucket")
}

func (p *BitbucketProvider) GetFileContent(_ context.Context, _ *models.VCSConnection, _, _, _, _ string) (string, error) {
	return "", fmt.Errorf("not implemented for Bitbucket")
}

func (p *BitbucketProvider) ListFiles(_ context.Context, _ *models.VCSConnection, _, _, _ string, _ []string) ([]string, error) {
	return nil, fmt.Errorf("not implemented for Bitbucket")
}

func (p *BitbucketProvider) ValidateWebhook(_ []byte, _, _ string) error {
	return nil
}

func (p *BitbucketProvider) ParseWebhookPayload(_ []byte) (*WebhookPayload, error) {
	return nil, fmt.Errorf("not implemented for Bitbucket")
}

func (p *BitbucketProvider) RegisterWebhooksForRepo(_ context.Context, _ *models.VCSConnection, _, _, _ string) error {
	return nil
}
