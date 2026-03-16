// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package vcs

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"regexp"
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

	key, parseErr := loadPrivateKeyFromString(string(data))
	if parseErr != nil {
		lineCount := strings.Count(string(data), "\n")
		return nil, fmt.Errorf("%w (hint: file=%s size=%d lines=%d)", parseErr, path, len(data), lineCount)
	}
	return key, nil
}

// loadPrivateKeyFromString loads a private key from a PEM string
func loadPrivateKeyFromString(keyData string) (*rsa.PrivateKey, error) {
	keyData = strings.TrimSpace(keyData)
	keyData = strings.ReplaceAll(keyData, "\\n", "\n")

	block, _ := pem.Decode([]byte(keyData))
	if block == nil {
		// PEM decode failed — attempt normalization for keys where newlines
		// were replaced by spaces (common with secret managers like 1Password,
		// HashiCorp Vault, or ExternalSecrets operator)
		normalized := normalizePEM(keyData)
		if normalized != "" {
			block, _ = pem.Decode([]byte(normalized))
		}
		if block == nil {
			return nil, fmt.Errorf("failed to decode PEM block")
		}
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

// pemHeaderRe matches PEM BEGIN/END markers to extract the type label and body.
var pemHeaderRe = regexp.MustCompile(`(?s)-----BEGIN ([A-Z0-9 ]+)-----(.+?)-----END ([A-Z0-9 ]+)-----`)

// normalizePEM reconstructs a properly formatted PEM block from input where
// newlines have been replaced by spaces (e.g. secret managers, 1Password).
func normalizePEM(data string) string {
	m := pemHeaderRe.FindStringSubmatch(data)
	if m == nil {
		return ""
	}

	pemType := m[1]
	body := m[2]

	// Strip all whitespace from the base64 body
	body = strings.NewReplacer(" ", "", "\t", "", "\n", "", "\r", "").Replace(body)
	if body == "" {
		return ""
	}

	// Re-wrap at 64 characters per line (standard PEM line length)
	var lines []string
	for len(body) > 64 {
		lines = append(lines, body[:64])
		body = body[64:]
	}
	if len(body) > 0 {
		lines = append(lines, body)
	}

	return fmt.Sprintf("-----BEGIN %s-----\n%s\n-----END %s-----\n", pemType, strings.Join(lines, "\n"), pemType)
}
