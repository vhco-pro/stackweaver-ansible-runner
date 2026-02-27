// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"

	"github.com/iac-platform/backend/internal/vcs"
)

type Client struct {
	webhookSecret string
	httpClient    *http.Client
}

func NewClient(webhookSecret string) *Client {
	return &Client{
		webhookSecret: webhookSecret,
		httpClient:    &http.Client{},
	}
}

func (c *Client) CloneRepository(ctx context.Context, repoURL, branch, destination string) error {
	cmd := exec.CommandContext(ctx, "git", "clone", "--branch", branch, "--depth", "1", repoURL, destination) //nolint:gosec // G204: git clone with validated parameters
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed: %s: %w", string(output), err)
	}
	return nil
}

func (c *Client) GetWebhookSecret() string {
	return c.webhookSecret
}

func (c *Client) ValidateWebhook(ctx context.Context, payload []byte, signature string) error {
	if c.webhookSecret == "" {
		return fmt.Errorf("webhook secret not configured")
	}

	expectedSignature := c.computeSignature(payload)
	if !hmac.Equal([]byte(signature), []byte(expectedSignature)) {
		return fmt.Errorf("invalid webhook signature")
	}

	return nil
}

func (c *Client) computeSignature(payload []byte) string {
	mac := hmac.New(sha256.New, []byte(c.webhookSecret))
	mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func (c *Client) GetRepositoryInfo(ctx context.Context, repoURL string) (*vcs.RepositoryInfo, error) {
	// Extract owner and repo from URL
	parts := strings.Split(strings.TrimSuffix(repoURL, ".git"), "/")
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid repository URL")
	}

	// For now, return basic info
	// In production, this would call GitHub API
	return &vcs.RepositoryInfo{
		URL:    repoURL,
		Branch: "main",
		Commit: "",
	}, nil
}

func (c *Client) ParseWebhookEvent(payload io.Reader) (*vcs.WebhookEvent, error) {
	// Basic implementation - would parse GitHub webhook payload
	// For now, return a placeholder
	return &vcs.WebhookEvent{
		Type:       "push",
		Repository: "",
		Branch:     "",
		Commit:     "",
	}, nil
}
