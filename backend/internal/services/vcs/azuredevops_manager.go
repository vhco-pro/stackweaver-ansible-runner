// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package vcs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/michielvha/logger"
)

// Azure DevOps resource identifier used in Entra ID OAuth2 scopes.
const adoResourceID = "499b84ac-1321-427f-aa17-267ca6975798"

// adoScopes are the delegated permission scopes requested from the user.
// vso.code        — read source code, commits, branches; also inherits vso.hooks_write (service hooks)
// vso.code_status — read and write commit/pull request status (required for PR status checks)
// vso.project     — read projects and teams (required to list repos across projects)
// offline_access  — enables refresh tokens so the user does not need to re-authorize
//
// Note: vso.hooks and vso.hooks_write are no longer public scopes in Entra ID — they cannot be
// requested explicitly and will cause AADSTS650053. Service hook access is already included
// because vso.code inherits from vso.hooks_write per the Azure DevOps scope hierarchy.
const adoScopes = adoResourceID + "/vso.code " +
	adoResourceID + "/vso.code_status " +
	adoResourceID + "/vso.project " +
	"offline_access"

// AzureDevOpsManager manages Azure DevOps OAuth2 configuration via Microsoft Entra ID.
// Loaded once at startup, similar to GitHubAppManager.
//
// Registration: Azure Portal → Microsoft Entra ID → App registrations (portal.azure.com)
// Auth endpoints: login.microsoftonline.com/{tenant}/oauth2/v2.0/...
type AzureDevOpsManager struct {
	clientID     string
	clientSecret string //nolint:gosec // G117: config field, not a hardcoded secret
	redirectURI  string
	tenantID     string // Entra tenant ID; use "common" for multi-tenant (any org)
	enabled      bool
}

// OAuthTokenResult holds the result of an Azure DevOps OAuth token exchange or refresh.
type OAuthTokenResult struct {
	AccessToken  string    `json:"access_token"`  //nolint:gosec // G117: token field
	RefreshToken string    `json:"refresh_token"` //nolint:gosec // G117: token field
	ExpiresIn    int       `json:"expires_in"`
	TokenType    string    `json:"token_type"`
	ExpiresAt    time.Time `json:"-"`
}

// NewAzureDevOpsManager creates a new AzureDevOpsManager from environment variables.
// If AZURE_DEVOPS_CLIENT_ID or AZURE_DEVOPS_CLIENT_SECRET are not set, returns a disabled manager.
//
// Required env vars:
//   - AZURE_DEVOPS_CLIENT_ID    — Application (client) ID from Azure Portal App registration
//   - AZURE_DEVOPS_CLIENT_SECRET — Client secret value (Certificates & secrets)
//   - AZURE_DEVOPS_REDIRECT_URI  — Must match exactly the redirect URI in the app registration
//
// Optional:
//   - AZURE_DEVOPS_TENANT_ID    — Entra tenant ID (default: "common" for multi-tenant)
func NewAzureDevOpsManager() (*AzureDevOpsManager, error) {
	clientID := os.Getenv("AZURE_DEVOPS_CLIENT_ID")
	clientSecret := os.Getenv("AZURE_DEVOPS_CLIENT_SECRET")
	redirectURI := os.Getenv("AZURE_DEVOPS_REDIRECT_URI")
	tenantID := os.Getenv("AZURE_DEVOPS_TENANT_ID")

	if clientID == "" || clientSecret == "" {
		return &AzureDevOpsManager{enabled: false}, nil
	}

	if tenantID == "" {
		tenantID = "common" // supports any organizational account (Entra ID)
	}

	// Entra ID requires HTTPS for all redirect URIs except http://localhost and http://127.0.0.1.
	// Auto-upgrade http:// → https:// for non-localhost URIs so that misconfigured env vars
	// (e.g. http://example.com/...) don't silently produce an AADSTS50011 redirect_uri error.
	if upgraded := ensureHTTPS(redirectURI); upgraded != redirectURI {
		logger.Warnf("AZURE_DEVOPS_REDIRECT_URI uses http:// for a non-localhost host; auto-upgraded to https://. Update deploy/vcs.env to suppress this warning.")
		redirectURI = upgraded
	}

	return &AzureDevOpsManager{
		clientID:     clientID,
		clientSecret: clientSecret,
		redirectURI:  redirectURI,
		tenantID:     tenantID,
		enabled:      true,
	}, nil
}

// ensureHTTPS upgrades http:// to https:// unless the host is localhost or 127.0.0.1,
// which Entra ID allows over plain HTTP for local development.
func ensureHTTPS(uri string) string {
	if !strings.HasPrefix(uri, "http://") {
		return uri
	}
	rest := strings.TrimPrefix(uri, "http://")
	host, _, _ := strings.Cut(rest, "/")
	// Strip port for the localhost check
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return uri // keep http:// for local dev
	}
	return "https://" + rest
}

// IsEnabled reports whether Azure DevOps OAuth2 is configured.
func (m *AzureDevOpsManager) IsEnabled() bool {
	return m.enabled
}

// GetClientID returns the Azure DevOps application client ID.
func (m *AzureDevOpsManager) GetClientID() string {
	return m.clientID
}

// GetAuthorizationURL returns the Microsoft Entra ID OAuth2 authorization URL.
// The state parameter should be in the format "stackweaverOrg|adoOrg|returnPath|uuid".
func (m *AzureDevOpsManager) GetAuthorizationURL(state string) string {
	params := url.Values{}
	params.Set("client_id", m.clientID)
	params.Set("response_type", "code")
	params.Set("state", state)
	params.Set("scope", adoScopes)
	params.Set("redirect_uri", m.redirectURI)
	return fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/authorize?%s", m.tenantID, params.Encode())
}

// ExchangeCode exchanges an OAuth2 authorization code for access and refresh tokens.
func (m *AzureDevOpsManager) ExchangeCode(ctx context.Context, code string) (*OAuthTokenResult, error) {
	return m.tokenRequest(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {m.clientID},
		"client_secret": {m.clientSecret},
		"code":          {code},
		"redirect_uri":  {m.redirectURI},
		"scope":         {adoScopes},
	})
}

// RefreshToken exchanges a refresh token for a new access token.
func (m *AzureDevOpsManager) RefreshToken(ctx context.Context, refreshToken string) (*OAuthTokenResult, error) {
	return m.tokenRequest(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {m.clientID},
		"client_secret": {m.clientSecret},
		"refresh_token": {refreshToken},
		"scope":         {adoScopes},
	})
}

func (m *AzureDevOpsManager) tokenRequest(ctx context.Context, params url.Values) (*OAuthTokenResult, error) {
	tokenURL := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", m.tenantID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL,
		strings.NewReader(params.Encode()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{}
	resp, err := client.Do(req) //nolint:gosec // G704: URL is hardcoded to Microsoft's OAuth endpoint, not user-controlled
	if err != nil {
		return nil, fmt.Errorf("token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token request failed: status %d, body: %s", resp.StatusCode, string(body))
	}

	var raw struct {
		AccessToken  string `json:"access_token"`  //nolint:gosec // G117: field names are required by the OAuth token response schema
		RefreshToken string `json:"refresh_token"` //nolint:gosec // G117: field names are required by the OAuth token response schema
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", err)
	}

	result := &OAuthTokenResult{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		ExpiresIn:    raw.ExpiresIn,
		TokenType:    raw.TokenType,
	}
	if raw.ExpiresIn > 0 {
		result.ExpiresAt = time.Now().Add(time.Duration(raw.ExpiresIn) * time.Second)
	}
	return result, nil
}
