// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package vcs

import (
	"context"
	"fmt"
	"time"

	"github.com/google/go-github/v73/github"
	"golang.org/x/oauth2"
)

type GitHubService struct {
	client *github.Client
}

func NewGitHubService(accessToken string) *GitHubService {
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: accessToken},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)

	return &GitHubService{
		client: client,
	}
}

// ListRepositories lists repositories for the authenticated user or organization
func (s *GitHubService) ListRepositories(ctx context.Context, accountType, accountName string, page, perPage int) ([]Repository, error) {
	var repos []*github.Repository
	var err error

	if accountType == "organization" {
		repos, _, err = s.client.Repositories.ListByOrg(ctx, accountName, &github.RepositoryListByOrgOptions{
			Type:        "all", // all, public, private, forks, sources, member
			ListOptions: github.ListOptions{Page: page, PerPage: perPage},
		})
	} else {
		// User repositories
		repos, _, err = s.client.Repositories.ListByUser(ctx, accountName, &github.RepositoryListByUserOptions{
			Type:        "all",
			ListOptions: github.ListOptions{Page: page, PerPage: perPage},
		})
	}

	if err != nil {
		return nil, fmt.Errorf("failed to list repositories: %w", err)
	}

	result := make([]Repository, 0, len(repos))
	for _, repo := range repos {
		defaultBranch := "main"
		if repo.DefaultBranch != nil {
			defaultBranch = *repo.DefaultBranch
		}

		result = append(result, Repository{
			ID:            repo.GetID(),
			Name:          repo.GetName(),
			FullName:      repo.GetFullName(),
			Description:   repo.GetDescription(),
			Private:       repo.GetPrivate(),
			DefaultBranch: defaultBranch,
			URL:           repo.GetHTMLURL(),
			CloneURL:      repo.GetCloneURL(),
			SSHURL:        repo.GetSSHURL(),
		})
	}

	return result, nil
}

// ListBranches lists branches for a repository
func (s *GitHubService) ListBranches(ctx context.Context, owner, repo string, page, perPage int) ([]Branch, error) {
	branches, _, err := s.client.Repositories.ListBranches(ctx, owner, repo, &github.BranchListOptions{
		ListOptions: github.ListOptions{
			Page:    page,
			PerPage: perPage,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list branches: %w", err)
	}

	result := make([]Branch, 0, len(branches))
	for _, branch := range branches {
		b := Branch{
			Name:      branch.GetName(),
			Protected: branch.GetProtected(),
		}
		if branch.Commit != nil {
			b.Commit.SHA = branch.Commit.GetSHA()
			b.Commit.URL = branch.Commit.GetURL()
		}
		result = append(result, b)
	}

	return result, nil
}

// GetRepository gets a single repository
func (s *GitHubService) GetRepository(ctx context.Context, owner, repo string) (*Repository, error) {
	repository, _, err := s.client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("failed to get repository: %w", err)
	}

	defaultBranch := "main"
	if repository.DefaultBranch != nil {
		defaultBranch = *repository.DefaultBranch
	}

	return &Repository{
		ID:            repository.GetID(),
		Name:          repository.GetName(),
		FullName:      repository.GetFullName(),
		Description:   repository.GetDescription(),
		Private:       repository.GetPrivate(),
		DefaultBranch: defaultBranch,
		URL:           repository.GetHTMLURL(),
		CloneURL:      repository.GetCloneURL(),
		SSHURL:        repository.GetSSHURL(),
	}, nil
}

// VerifyAccess verifies that the access token has access to the repository
func (s *GitHubService) VerifyAccess(ctx context.Context, owner, repo string) error {
	_, _, err := s.client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("no access to repository %s/%s: %w", owner, repo, err)
	}
	return nil
}

// CreateWebhook creates a webhook for a repository
func (s *GitHubService) CreateWebhook(ctx context.Context, owner, repo, webhookURL, secret string) (int64, error) {
	hook := &github.Hook{
		Name: github.Ptr("web"),
		Config: &github.HookConfig{
			URL:         github.Ptr(webhookURL),
			ContentType: github.Ptr("json"),
			Secret:      github.Ptr(secret),
			InsecureSSL: github.Ptr("0"),
		},
		Events: []string{"push", "pull_request"},
		Active: github.Ptr(true),
	}

	createdHook, _, err := s.client.Repositories.CreateHook(ctx, owner, repo, hook)
	if err != nil {
		return 0, fmt.Errorf("failed to create webhook: %w", err)
	}

	return createdHook.GetID(), nil
}

// DeleteWebhook deletes a webhook
func (s *GitHubService) DeleteWebhook(ctx context.Context, owner, repo string, hookID int64) error {
	_, err := s.client.Repositories.DeleteHook(ctx, owner, repo, hookID)
	if err != nil {
		return fmt.Errorf("failed to delete webhook: %w", err)
	}
	return nil
}

// ListWebhooks lists webhooks for a repository
func (s *GitHubService) ListWebhooks(ctx context.Context, owner, repo string) ([]*github.Hook, error) {
	hooks, _, err := s.client.Repositories.ListHooks(ctx, owner, repo, &github.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list webhooks: %w", err)
	}
	return hooks, nil
}

// GetAuthenticatedUser gets the authenticated user info
func (s *GitHubService) GetAuthenticatedUser(ctx context.Context) (*github.User, error) {
	user, _, err := s.client.Users.Get(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("failed to get authenticated user: %w", err)
	}
	return user, nil
}

// ListOrganizations lists organizations for the authenticated user
func (s *GitHubService) ListOrganizations(ctx context.Context, page, perPage int) ([]*github.Organization, error) {
	orgs, _, err := s.client.Organizations.List(ctx, "", &github.ListOptions{
		Page:    page,
		PerPage: perPage,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list organizations: %w", err)
	}
	return orgs, nil
}

// RefreshToken refreshes an expired access token (if refresh token is available)
func (s *GitHubService) RefreshToken(ctx context.Context, refreshToken string) (string, *time.Time, error) {
	// GitHub OAuth Apps don't support refresh tokens in the same way
	// This would need to be implemented based on the OAuth flow used
	// For now, return error indicating token needs to be re-authorized
	return "", nil, fmt.Errorf("token refresh not implemented - user needs to re-authorize")
}
