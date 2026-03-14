// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package vcs

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/go-github/v73/github"
	"github.com/michielvha/logger"
	"golang.org/x/oauth2"
)

// GitHubAppService handles GitHub App API interactions
type GitHubAppService struct {
	appID      string
	privateKey *rsa.PrivateKey
	httpClient *http.Client
}

// InstallationInfo represents a GitHub App installation
type InstallationInfo struct {
	ID          string
	AccountName string
	AccountType string // "user" or "organization"
}

// NewGitHubAppService creates a new GitHub App service
func NewGitHubAppService(appID string, privateKey *rsa.PrivateKey) *GitHubAppService {
	return &GitHubAppService{
		appID:      appID,
		privateKey: privateKey,
		httpClient: &http.Client{},
	}
}

// generateAppToken generates a JWT token for GitHub App authentication
// This token is used to authenticate as the app (not an installation)
func (s *GitHubAppService) generateAppToken() (string, error) {
	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iat": now.Add(-60 * time.Second).Unix(), // Issued at (allow 60s clock skew)
		"exp": now.Add(10 * time.Minute).Unix(),  // Expires in 10 minutes
		"iss": s.appID,                           // Issuer (App ID)
	})

	return token.SignedString(s.privateKey)
}

// GenerateInstallationToken generates an installation access token
// This token is used to make API calls on behalf of an installation
func (s *GitHubAppService) GenerateInstallationToken(ctx context.Context, installationID string) (string, error) {
	// First, get an app token
	appToken, err := s.generateAppToken()
	if err != nil {
		return "", fmt.Errorf("failed to generate app token: %w", err)
	}

	// Call GitHub API to create installation token
	url := fmt.Sprintf("https://api.github.com/app/installations/%s/access_tokens", installationID)
	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+appToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := s.httpClient.Do(req) //nolint:gosec // G704: URL targets GitHub API with validated installation ID
	if err != nil {
		return "", fmt.Errorf("failed to create installation token: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Warnf("Failed to close response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to create installation token: status %d, body: %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", fmt.Errorf("failed to decode token response: %w", err)
	}

	return tokenResp.Token, nil
}

// GetInstallation gets installation information
func (s *GitHubAppService) GetInstallation(ctx context.Context, installationID string) (*InstallationInfo, error) {
	// Generate app token
	appToken, err := s.generateAppToken()
	if err != nil {
		return nil, fmt.Errorf("failed to generate app token: %w", err)
	}

	// Call GitHub API
	url := fmt.Sprintf("https://api.github.com/app/installations/%s", installationID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+appToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := s.httpClient.Do(req) //nolint:gosec // G704: URL targets GitHub API with validated installation ID
	if err != nil {
		return nil, fmt.Errorf("failed to get installation: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Warnf("Failed to close response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to get installation: status %d, body: %s", resp.StatusCode, string(body))
	}

	var installation struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"account"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&installation); err != nil {
		return nil, fmt.Errorf("failed to decode installation: %w", err)
	}

	accountType := "user"
	if installation.Account.Type == "Organization" {
		accountType = "organization"
	}

	return &InstallationInfo{
		ID:          strconv.FormatInt(installation.ID, 10),
		AccountName: installation.Account.Login,
		AccountType: accountType,
	}, nil
}

// GetClientForInstallation returns a GitHub client authenticated with an installation token
func (s *GitHubAppService) GetClientForInstallation(ctx context.Context, installationID string) (*github.Client, error) {
	token, err := s.GenerateInstallationToken(ctx, installationID)
	if err != nil {
		return nil, fmt.Errorf("failed to generate installation token: %w", err)
	}

	// Create GitHub client with installation token
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)

	return client, nil
}

// ListRepositories lists repositories accessible to an installation
// Returns repositories as our Repository struct type
func (s *GitHubAppService) ListRepositories(ctx context.Context, installationID string, page, perPage int) ([]Repository, error) {
	client, err := s.GetClientForInstallation(ctx, installationID)
	if err != nil {
		return nil, fmt.Errorf("failed to get GitHub client: %w", err)
	}

	// List repositories accessible to the installation
	// ListRepos returns *github.ListRepositories which contains TotalCount and Repositories field
	listRepos, _, err := client.Apps.ListRepos(ctx, &github.ListOptions{
		Page:    page,
		PerPage: perPage,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list repositories: %w", err)
	}

	// Extract repositories from ListRepositories struct
	// ListRepositories has a Repositories field that is []*github.Repository
	var repos []*github.Repository
	if listRepos != nil {
		repos = listRepos.Repositories
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

// ListBranches lists branches for a repository using installation token
func (s *GitHubAppService) ListBranches(ctx context.Context, installationID, owner, repo string, page, perPage int) ([]Branch, error) {
	client, err := s.GetClientForInstallation(ctx, installationID)
	if err != nil {
		return nil, fmt.Errorf("failed to get GitHub client: %w", err)
	}

	branches, _, err := client.Repositories.ListBranches(ctx, owner, repo, &github.BranchListOptions{
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

// CreateWebhook creates a webhook for a repository using installation token
// This is called automatically when a workspace is created with a VCS connection
func (s *GitHubAppService) CreateWebhook(ctx context.Context, installationID, owner, repo, webhookURL, webhookSecret string) (int64, error) {
	client, err := s.GetClientForInstallation(ctx, installationID)
	if err != nil {
		return 0, fmt.Errorf("failed to get GitHub client: %w", err)
	}

	// Configure webhook
	hook := &github.Hook{
		Name: github.Ptr("web"),
		Config: &github.HookConfig{
			URL:         github.Ptr(webhookURL),
			ContentType: github.Ptr("json"),
			Secret:      github.Ptr(webhookSecret),
			InsecureSSL: github.Ptr("0"),
		},
		Events: []string{"push", "pull_request"},
		Active: github.Ptr(true),
	}

	createdHook, _, err := client.Repositories.CreateHook(ctx, owner, repo, hook)
	if err != nil {
		return 0, fmt.Errorf("failed to create webhook: %w", err)
	}

	return createdHook.GetID(), nil
}

// DeleteWebhook deletes a webhook by ID
func (s *GitHubAppService) DeleteWebhook(ctx context.Context, installationID, owner, repo string, hookID int64) error {
	client, err := s.GetClientForInstallation(ctx, installationID)
	if err != nil {
		return fmt.Errorf("failed to get GitHub client: %w", err)
	}

	_, err = client.Repositories.DeleteHook(ctx, owner, repo, hookID)
	if err != nil {
		return fmt.Errorf("failed to delete webhook: %w", err)
	}

	return nil
}

// ListTags lists tags for a repository and returns them sorted by creation date (newest first)
func (s *GitHubAppService) ListTags(ctx context.Context, installationID, owner, repo string, page, perPage int) ([]*github.RepositoryTag, error) {
	client, err := s.GetClientForInstallation(ctx, installationID)
	if err != nil {
		return nil, fmt.Errorf("failed to get GitHub client: %w", err)
	}

	tags, _, err := client.Repositories.ListTags(ctx, owner, repo, &github.ListOptions{
		Page:    page,
		PerPage: perPage,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list tags: %w", err)
	}

	return tags, nil
}

// GetLatestTag gets the latest tag from a repository (first page, sorted by GitHub)
func (s *GitHubAppService) GetLatestTag(ctx context.Context, installationID, owner, repo string) (*github.RepositoryTag, error) {
	tags, err := s.ListTags(ctx, installationID, owner, repo, 1, 1)
	if err != nil {
		return nil, err
	}

	if len(tags) == 0 {
		return nil, fmt.Errorf("no tags found in repository")
	}

	return tags[0], nil
}

// GetFileContent retrieves the content of a file from a repository
func (s *GitHubAppService) GetFileContent(ctx context.Context, installationID, owner, repo, path, ref string) (string, error) {
	client, err := s.GetClientForInstallation(ctx, installationID)
	if err != nil {
		return "", fmt.Errorf("failed to get GitHub client: %w", err)
	}

	opts := &github.RepositoryContentGetOptions{}
	if ref != "" {
		opts.Ref = ref
	}

	fileContent, _, _, err := client.Repositories.GetContents(ctx, owner, repo, path, opts)
	if err != nil {
		return "", fmt.Errorf("failed to get file content: %w", err)
	}

	if fileContent == nil {
		return "", fmt.Errorf("file not found: %s", path)
	}

	// Decode the base64 content
	content, err := fileContent.GetContent()
	if err != nil {
		return "", fmt.Errorf("failed to decode file content: %w", err)
	}

	return content, nil
}

// ListYamlFiles recursively lists all .yaml and .yml files in a repository
func (s *GitHubAppService) ListYamlFiles(ctx context.Context, installationID, owner, repo, ref string) ([]string, error) {
	client, err := s.GetClientForInstallation(ctx, installationID)
	if err != nil {
		return nil, fmt.Errorf("failed to get GitHub client: %w", err)
	}

	var yamlFiles []string
	opts := &github.RepositoryContentGetOptions{}
	if ref != "" {
		opts.Ref = ref
	}

	// Recursive function to traverse directories
	var listYamlFilesRecursive func(path string) error
	listYamlFilesRecursive = func(path string) error {
		// GetContents returns (fileContent *RepositoryContent, directoryContent []*RepositoryContent, ...)
		// One will be nil, the other will have data
		fileContent, directoryContent, _, err := client.Repositories.GetContents(ctx, owner, repo, path, opts)
		if err != nil {
			return fmt.Errorf("failed to get contents for path %s: %w", path, err)
		}

		// Handle directory (directoryContent will be non-nil)
		if directoryContent != nil {
			for _, file := range directoryContent {
				if file == nil {
					continue
				}
				if file.GetType() == "file" {
					name := file.GetName()
					if (len(name) >= 5 && name[len(name)-5:] == ".yaml") || (len(name) >= 4 && name[len(name)-4:] == ".yml") {
						yamlFiles = append(yamlFiles, file.GetPath())
					}
				} else if file.GetType() == "dir" {
					// Recursively search subdirectories
					if err := listYamlFilesRecursive(file.GetPath()); err != nil {
						// Log error but continue searching other directories
						logger.Warnf("vcs: failed to list files in directory %s: %v", file.GetPath(), err)
					}
				}
			}
		} else if fileContent != nil {
			// Single file
			if fileContent.GetType() == "file" {
				name := fileContent.GetName()
				if (len(name) >= 5 && name[len(name)-5:] == ".yaml") || (len(name) >= 4 && name[len(name)-4:] == ".yml") {
					yamlFiles = append(yamlFiles, fileContent.GetPath())
				}
			}
		}

		return nil
	}

	// Start from root
	if err := listYamlFilesRecursive(""); err != nil {
		return nil, err
	}

	return yamlFiles, nil
}

// ListInventoryFiles recursively lists all inventory files in a repository
// Supports .ini, .yaml, .yml, and .json file extensions (Ansible inventory formats)
func (s *GitHubAppService) ListInventoryFiles(ctx context.Context, installationID, owner, repo, ref string) ([]string, error) {
	client, err := s.GetClientForInstallation(ctx, installationID)
	if err != nil {
		return nil, fmt.Errorf("failed to get GitHub client: %w", err)
	}

	var inventoryFiles []string
	opts := &github.RepositoryContentGetOptions{}
	if ref != "" {
		opts.Ref = ref
	}

	// Helper function to check if a file is an inventory file
	isInventoryFile := func(name string) bool {
		ext := strings.ToLower(name)
		return strings.HasSuffix(ext, ".ini") ||
			strings.HasSuffix(ext, ".yaml") ||
			strings.HasSuffix(ext, ".yml") ||
			strings.HasSuffix(ext, ".json")
	}

	// Recursive function to traverse directories
	var listInventoryFilesRecursive func(path string) error
	listInventoryFilesRecursive = func(path string) error {
		// GetContents returns (fileContent *RepositoryContent, directoryContent []*RepositoryContent, ...)
		// One will be nil, the other will have data
		fileContent, directoryContent, _, err := client.Repositories.GetContents(ctx, owner, repo, path, opts)
		if err != nil {
			return fmt.Errorf("failed to get contents for path %s: %w", path, err)
		}

		// Handle directory (directoryContent will be non-nil)
		if directoryContent != nil {
			for _, file := range directoryContent {
				if file == nil {
					continue
				}
				if file.GetType() == "file" {
					name := file.GetName()
					if isInventoryFile(name) {
						inventoryFiles = append(inventoryFiles, file.GetPath())
					}
				} else if file.GetType() == "dir" {
					// Recursively search subdirectories
					if err := listInventoryFilesRecursive(file.GetPath()); err != nil {
						// Log error but continue searching other directories
						logger.Warnf("vcs: failed to list files in directory %s: %v", file.GetPath(), err)
					}
				}
			}
		} else if fileContent != nil {
			// Single file
			if fileContent.GetType() == "file" {
				name := fileContent.GetName()
				if isInventoryFile(name) {
					inventoryFiles = append(inventoryFiles, fileContent.GetPath())
				}
			}
		}

		return nil
	}

	// Start from root
	if err := listInventoryFilesRecursive(""); err != nil {
		return nil, err
	}

	return inventoryFiles, nil
}
