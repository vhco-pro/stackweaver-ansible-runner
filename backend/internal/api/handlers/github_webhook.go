// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/michielvha/logger"
)

// PlaybookSyncQueuer is an interface for queuing playbook syncs
type PlaybookSyncQueuer interface {
	QueueSync(playbookID uuid.UUID) error
}

// GitHubWebhookHandler handles GitHub webhook events
type GitHubWebhookHandler struct {
	playbookRepo  *repository.AnsiblePlaybookRepository
	vcsRepo       *repository.VCSConnectionRepository
	syncQueuer    PlaybookSyncQueuer // Optional - if nil, syncs won't be queued
	webhookSecret string             // Optional webhook secret for validation
}

// NewGitHubWebhookHandler creates a new GitHub webhook handler
func NewGitHubWebhookHandler(
	playbookRepo *repository.AnsiblePlaybookRepository,
	vcsRepo *repository.VCSConnectionRepository,
	syncQueuer PlaybookSyncQueuer,
	webhookSecret string,
) *GitHubWebhookHandler {
	return &GitHubWebhookHandler{
		playbookRepo:  playbookRepo,
		vcsRepo:       vcsRepo,
		syncQueuer:    syncQueuer,
		webhookSecret: webhookSecret,
	}
}

// GitHubPushPayload represents a GitHub push webhook payload
type GitHubPushPayload struct {
	Ref        string `json:"ref"`
	Before     string `json:"before"`
	After      string `json:"after"`
	Created    bool   `json:"created"`
	Deleted    bool   `json:"deleted"`
	Forced     bool   `json:"forced"`
	BaseRef    string `json:"base_ref"`
	Compare    string `json:"compare"`
	Repository struct {
		ID       int64  `json:"id"`
		NodeID   string `json:"node_id"`
		Name     string `json:"name"`
		FullName string `json:"full_name"`
		Private  bool   `json:"private"`
		Owner    struct {
			Name  string `json:"name"`
			Login string `json:"login"`
		} `json:"owner"`
		HTMLURL       string `json:"html_url"`
		CloneURL      string `json:"clone_url"`
		SSHURL        string `json:"ssh_url"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
	Pusher struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"pusher"`
	Sender struct {
		Login     string `json:"login"`
		ID        int64  `json:"id"`
		AvatarURL string `json:"avatar_url"`
		Type      string `json:"type"`
	} `json:"sender"`
	HeadCommit struct {
		ID        string `json:"id"`
		TreeID    string `json:"tree_id"`
		Message   string `json:"message"`
		Timestamp string `json:"timestamp"`
		URL       string `json:"url"`
		Author    struct {
			Name     string `json:"name"`
			Email    string `json:"email"`
			Username string `json:"username"`
		} `json:"author"`
		Committer struct {
			Name     string `json:"name"`
			Email    string `json:"email"`
			Username string `json:"username"`
		} `json:"committer"`
		Added    []string `json:"added"`
		Removed  []string `json:"removed"`
		Modified []string `json:"modified"`
	} `json:"head_commit"`
	Commits []struct {
		ID        string   `json:"id"`
		Message   string   `json:"message"`
		Timestamp string   `json:"timestamp"`
		URL       string   `json:"url"`
		Added     []string `json:"added"`
		Removed   []string `json:"removed"`
		Modified  []string `json:"modified"`
	} `json:"commits"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

// HandleWebhook handles incoming GitHub webhooks
func (h *GitHubWebhookHandler) HandleWebhook(c *gin.Context) {
	// Read the request body
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read request body"})
		return
	}

	// Validate webhook signature if secret is configured
	if h.webhookSecret != "" {
		signature := c.GetHeader("X-Hub-Signature-256")
		if signature == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Missing signature header"})
			return
		}
		if !h.validateSignature(body, signature) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid signature"})
			return
		}
	}

	// Get the event type
	eventType := c.GetHeader("X-GitHub-Event")
	deliveryID := c.GetHeader("X-GitHub-Delivery")

	logger.Infof("Received GitHub webhook: event=%s, delivery=%s", eventType, deliveryID)

	switch eventType {
	case "push":
		h.handlePushEvent(c, body)
	case "ping":
		h.handlePingEvent(c, body)
	case "installation", "installation_repositories":
		h.handleInstallationEvent(c, eventType, body)
	default:
		logger.Infof("Ignoring GitHub webhook event: %s", eventType)
		c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("Event type %s ignored", eventType)})
	}
}

// validateSignature validates the GitHub webhook signature
func (h *GitHubWebhookHandler) validateSignature(body []byte, signature string) bool {
	// GitHub signature format: sha256=<hex-digest>
	if !strings.HasPrefix(signature, "sha256=") {
		return false
	}

	expectedSignature := signature[7:] // Remove "sha256=" prefix

	mac := hmac.New(sha256.New, []byte(h.webhookSecret))
	mac.Write(body)
	actualSignature := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(actualSignature), []byte(expectedSignature))
}

// handlePushEvent handles push events
func (h *GitHubWebhookHandler) handlePushEvent(c *gin.Context, body []byte) {
	var payload GitHubPushPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid push payload"})
		return
	}

	// Extract branch name from ref (refs/heads/main -> main)
	branch := strings.TrimPrefix(payload.Ref, "refs/heads/")

	logger.Infof("Push event: repo=%s, branch=%s, commit=%s",
		payload.Repository.FullName, branch, payload.After)

	// Skip if this is a delete event
	if payload.Deleted {
		logger.Infof("Ignoring branch delete event for %s", branch)
		c.JSON(http.StatusOK, gin.H{"message": "Branch delete event ignored"})
		return
	}

	// Find all playbooks that use this repository and branch
	playbooks, err := h.findAffectedPlaybooks(payload.Repository.FullName, branch)
	if err != nil {
		logger.Infof("Error finding affected playbooks: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to find affected playbooks"})
		return
	}

	if len(playbooks) == 0 {
		logger.Infof("No playbooks found for repo %s branch %s", payload.Repository.FullName, branch)
		c.JSON(http.StatusOK, gin.H{"message": "No playbooks affected"})
		return
	}

	// Check which playbooks have affected files
	syncCount := 0
	for _, playbook := range playbooks {
		// Check if any of the changed files are in the playbook's path
		if h.isPlaybookAffected(playbook, payload) {
			logger.Infof("Triggering sync for playbook: %s (ID: %s)", playbook.Name, playbook.ID)

			// Queue sync for this playbook
			if h.syncQueuer != nil {
				go func(p models.AnsiblePlaybook) {
					if err := h.syncQueuer.QueueSync(p.ID); err != nil {
						logger.Infof("Error queuing sync for playbook %s: %v", p.ID, err)
					}
				}(playbook)
			}
			syncCount++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"message":      fmt.Sprintf("Push event processed, %d playbooks queued for sync", syncCount),
		"synced_count": syncCount,
		"commit":       payload.After,
	})
}

// handlePingEvent handles ping events (sent when webhook is created)
func (h *GitHubWebhookHandler) handlePingEvent(c *gin.Context, body []byte) {
	var payload struct {
		Zen    string `json:"zen"`
		HookID int64  `json:"hook_id"`
		Hook   struct {
			Type   string   `json:"type"`
			Events []string `json:"events"`
			Active bool     `json:"active"`
		} `json:"hook"`
	}

	if err := json.Unmarshal(body, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid ping payload"})
		return
	}

	logger.Infof("Webhook ping received: zen='%s', hook_id=%d", payload.Zen, payload.HookID)
	c.JSON(http.StatusOK, gin.H{
		"message": "Pong!",
		"zen":     payload.Zen,
	})
}

// handleInstallationEvent handles GitHub App installation events
func (h *GitHubWebhookHandler) handleInstallationEvent(c *gin.Context, eventType string, body []byte) {
	var payload struct {
		Action       string `json:"action"`
		Installation struct {
			ID      int64 `json:"id"`
			Account struct {
				Login string `json:"login"`
				Type  string `json:"type"`
			} `json:"account"`
		} `json:"installation"`
		RepositoriesAdded []struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
		} `json:"repositories_added"`
		RepositoriesRemoved []struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
		} `json:"repositories_removed"`
	}

	if err := json.Unmarshal(body, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid installation payload"})
		return
	}

	logger.Infof("Installation event: action=%s, installation_id=%d, account=%s",
		payload.Action, payload.Installation.ID, payload.Installation.Account.Login)

	switch payload.Action {
	case "created":
		logger.Infof("GitHub App installed for account: %s", payload.Installation.Account.Login)
	case "deleted":
		logger.Infof("GitHub App uninstalled from account: %s", payload.Installation.Account.Login)
	case "added":
		for _, repo := range payload.RepositoriesAdded {
			logger.Infof("Repository added to installation: %s", repo.FullName)
		}
	case "removed":
		for _, repo := range payload.RepositoriesRemoved {
			logger.Infof("Repository removed from installation: %s", repo.FullName)
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("Installation event '%s' processed", payload.Action),
	})
}

// findAffectedPlaybooks finds all playbooks that use a specific repository and branch
func (h *GitHubWebhookHandler) findAffectedPlaybooks(repoFullName, branch string) ([]models.AnsiblePlaybook, error) {
	// Use the efficient repository method to query by repo and branch
	return h.playbookRepo.ListByVCSRepositoryAndBranch(repoFullName, branch)
}

// isPlaybookAffected checks if a playbook's files were changed in the push
func (h *GitHubWebhookHandler) isPlaybookAffected(playbook models.AnsiblePlaybook, payload GitHubPushPayload) bool {
	// If no specific path is set, any change to the repo affects the playbook
	if playbook.PlaybookPath == "" {
		return true
	}

	// Get the directory containing the playbook
	playbookDir := getDirectory(playbook.PlaybookPath)

	// Check if any changed files are in the playbook's directory
	for _, commit := range payload.Commits {
		for _, file := range append(append(commit.Added, commit.Modified...), commit.Removed...) {
			if strings.HasPrefix(file, playbookDir) || file == playbook.PlaybookPath {
				return true
			}
		}
	}

	// Also check head_commit
	headFiles := append(append(payload.HeadCommit.Added, payload.HeadCommit.Modified...), payload.HeadCommit.Removed...)
	for _, file := range headFiles {
		if strings.HasPrefix(file, playbookDir) || file == playbook.PlaybookPath {
			return true
		}
	}

	return false
}

// getDirectory returns the directory portion of a path
func getDirectory(path string) string {
	lastSlash := strings.LastIndex(path, "/")
	if lastSlash == -1 {
		return ""
	}
	return path[:lastSlash+1]
}

// WebhookResult represents the result of webhook processing
type WebhookResult struct {
	Event       string   `json:"event"`
	Repository  string   `json:"repository,omitempty"`
	Branch      string   `json:"branch,omitempty"`
	Commit      string   `json:"commit,omitempty"`
	PlaybookIDs []string `json:"playbook_ids,omitempty"`
	SyncCount   int      `json:"sync_count"`
	Message     string   `json:"message"`
}
