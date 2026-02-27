// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package vcs

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
)

// GitHubAppManager manages GitHub App configuration and provides services
// This is loaded once at startup and reused for all requests (like Terraform Enterprise)
type GitHubAppManager struct {
	appID      string
	appName    string
	privateKey *rsa.PrivateKey
	enabled    bool
}

// NewGitHubAppManager creates a new GitHub App manager from environment variables
// This should be called once at startup
func NewGitHubAppManager() (*GitHubAppManager, error) {
	appID := os.Getenv("GITHUB_APP_ID")
	appName := os.Getenv("GITHUB_APP_NAME")

	// If not configured, return a disabled manager
	if appID == "" || appName == "" {
		return &GitHubAppManager{
			enabled: false,
		}, nil
	}

	// Load private key
	privateKeyPath := os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH")
	privateKeyStr := os.Getenv("GITHUB_APP_PRIVATE_KEY")

	var privateKey *rsa.PrivateKey
	var err error

	switch {
	case privateKeyPath != "":
		privateKey, err = loadPrivateKeyFromFile(privateKeyPath)
	case privateKeyStr != "":
		privateKey, err = loadPrivateKeyFromString(privateKeyStr)
	default:
		return &GitHubAppManager{
			enabled: false,
		}, fmt.Errorf("GitHub App private key not configured (set GITHUB_APP_PRIVATE_KEY_PATH or GITHUB_APP_PRIVATE_KEY)")
	}

	if err != nil {
		return nil, fmt.Errorf("failed to load GitHub App private key: %w", err)
	}

	return &GitHubAppManager{
		appID:      appID,
		appName:    appName,
		privateKey: privateKey,
		enabled:    true,
	}, nil
}

// IsEnabled returns whether the GitHub App is configured
func (m *GitHubAppManager) IsEnabled() bool {
	return m.enabled
}

// GetAppID returns the GitHub App ID
func (m *GitHubAppManager) GetAppID() string {
	return m.appID
}

// GetAppName returns the GitHub App name (slug)
func (m *GitHubAppManager) GetAppName() string {
	return m.appName
}

// GetPrivateKey returns the GitHub App private key
func (m *GitHubAppManager) GetPrivateKey() *rsa.PrivateKey {
	return m.privateKey
}

// GetService returns a GitHubAppService instance for making API calls
func (m *GitHubAppManager) GetService() *GitHubAppService {
	if !m.enabled {
		return nil
	}
	return NewGitHubAppService(m.appID, m.privateKey)
}

// loadPrivateKeyFromFile loads a private key from a file path
func loadPrivateKeyFromFile(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is validated (from trusted config)
	if err != nil {
		return nil, fmt.Errorf("failed to read private key file: %w", err)
	}
	return loadPrivateKeyFromString(string(data))
}

// loadPrivateKeyFromString loads a private key from a PEM string
func loadPrivateKeyFromString(keyData string) (*rsa.PrivateKey, error) {
	// Remove any whitespace and newlines that might be in environment variable
	keyData = strings.TrimSpace(keyData)
	keyData = strings.ReplaceAll(keyData, "\\n", "\n")

	block, _ := pem.Decode([]byte(keyData))
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block")
	}

	privateKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		// Try PKCS8 format
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("failed to parse private key: %w", err)
		}
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("key is not an RSA private key")
		}
		return rsaKey, nil
	}

	return privateKey, nil
}
