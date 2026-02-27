// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package vcs

import (
	"context"
)

type Provider interface {
	CloneRepository(ctx context.Context, repoURL, branch, destination string) error
	GetWebhookSecret() string
	ValidateWebhook(ctx context.Context, payload []byte, signature string) error
	GetRepositoryInfo(ctx context.Context, repoURL string) (*RepositoryInfo, error)
}

type RepositoryInfo struct {
	URL    string
	Branch string
	Commit string
}

type WebhookEvent struct {
	Type       string
	Repository string
	Branch     string
	Commit     string
}
