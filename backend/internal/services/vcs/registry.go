// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package vcs

import (
	"fmt"

	"github.com/iac-platform/backend/internal/models"
)

// ConnUpdater is a callback that persists a VCSConnection to the database.
// Used by providers to save refreshed OAuth tokens after automatic refresh.
type ConnUpdater func(conn *models.VCSConnection) error

// ProviderRegistry maps a VCSConnection to the correct ProviderService implementation.
type ProviderRegistry struct {
	githubAppManager   *GitHubAppManager
	azureDevOpsManager *AzureDevOpsManager
	connUpdater        ConnUpdater
}

// NewProviderRegistry creates a new ProviderRegistry.
// connUpdater is an optional callback to persist VCSConnection changes (e.g. refreshed tokens)
// back to the database. Pass nil if persistence is not needed (e.g. in tests).
func NewProviderRegistry(ghManager *GitHubAppManager, adoManager *AzureDevOpsManager, connUpdater ConnUpdater) *ProviderRegistry {
	return &ProviderRegistry{
		githubAppManager:   ghManager,
		azureDevOpsManager: adoManager,
		connUpdater:        connUpdater,
	}
}

// GetConnUpdater returns the ConnUpdater callback, allowing other services
// to persist VCSConnection changes (e.g. refreshed tokens).
func (r *ProviderRegistry) GetConnUpdater() ConnUpdater {
	return r.connUpdater
}

// GetProvider returns the ProviderService for the given VCSConnection.
func (r *ProviderRegistry) GetProvider(conn *models.VCSConnection) (ProviderService, error) {
	switch conn.Provider {
	case models.VCSProviderGitHub:
		return &GitHubProvider{manager: r.githubAppManager}, nil
	case models.VCSProviderAzureDevOps:
		return &AzureDevOpsProvider{manager: r.azureDevOpsManager, connUpdater: r.connUpdater}, nil
	case models.VCSProviderGitLab:
		return &GitLabProvider{}, nil
	case models.VCSProviderBitbucket:
		return &BitbucketProvider{}, nil
	default:
		return nil, fmt.Errorf("unsupported VCS provider: %s", conn.Provider)
	}
}
