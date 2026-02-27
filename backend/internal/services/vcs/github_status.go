// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package vcs

import (
	"context"
	"fmt"

	"github.com/google/go-github/v73/github"
	"github.com/michielvha/logger"
)

// StatusState represents the state of a GitHub status check
type StatusState string

const (
	StatusStatePending StatusState = "pending"
	StatusStateSuccess StatusState = "success"
	StatusStateFailure StatusState = "failure"
	StatusStateError   StatusState = "error"
)

// GitHubStatusService handles GitHub Status Checks API interactions
type GitHubStatusService struct {
	appService *GitHubAppService
}

// NewGitHubStatusService creates a new GitHub status check service
func NewGitHubStatusService(appService *GitHubAppService) *GitHubStatusService {
	return &GitHubStatusService{
		appService: appService,
	}
}

// CreateOrUpdateStatusCheck creates or updates a GitHub status check for a commit
// The GitHub Status Checks API uses POST for both create and update operations
// If a status check with the same context already exists, it will be updated
func (s *GitHubStatusService) CreateOrUpdateStatusCheck(
	ctx context.Context,
	installationID string,
	owner string,
	repo string,
	sha string,
	context string,
	state StatusState,
	description string,
	targetURL string,
) error {
	client, err := s.appService.GetClientForInstallation(ctx, installationID)
	if err != nil {
		return fmt.Errorf("failed to get GitHub client: %w", err)
	}

	// Create status check using GitHub API
	status := &github.RepoStatus{
		State:       github.Ptr(string(state)),
		Context:     github.Ptr(context),
		Description: github.Ptr(description),
		TargetURL:   github.Ptr(targetURL),
	}

	_, _, err = client.Repositories.CreateStatus(ctx, owner, repo, sha, status)
	if err != nil {
		return fmt.Errorf("failed to create/update status check: %w", err)
	}

	logger.Infof("Created/updated status check: context=%s, state=%s, sha=%s, repo=%s/%s", context, state, sha, owner, repo)
	return nil
}

// CreateStatusCheck is an alias for CreateOrUpdateStatusCheck for clarity
func (s *GitHubStatusService) CreateStatusCheck(
	ctx context.Context,
	installationID string,
	owner string,
	repo string,
	sha string,
	context string,
	state StatusState,
	description string,
	targetURL string,
) error {
	return s.CreateOrUpdateStatusCheck(ctx, installationID, owner, repo, sha, context, state, description, targetURL)
}

// UpdateStatusCheck is an alias for CreateOrUpdateStatusCheck for clarity
func (s *GitHubStatusService) UpdateStatusCheck(
	ctx context.Context,
	installationID string,
	owner string,
	repo string,
	sha string,
	context string,
	state StatusState,
	description string,
	targetURL string,
) error {
	return s.CreateOrUpdateStatusCheck(ctx, installationID, owner, repo, sha, context, state, description, targetURL)
}
