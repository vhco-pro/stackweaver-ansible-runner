// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AzureTokenResponse represents an Azure AD OAuth2 token response.
type AzureTokenResponse struct {
	// AccessToken is the OAuth2 access token returned by Azure AD (standard JSON key).
	AccessToken string `json:"access_token"` //nolint:gosec // G117: OAuth response DTO, not a hardcoded secret
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// ExchangeOIDCForAzureToken exchanges an OIDC JWT (federated token) for an Azure AD access token.
// This uses the OAuth2 client credentials flow with a federated identity credential assertion.
//
// Flow:
// 1. We have a self-signed OIDC JWT (the "federated token")
// 2. POST to Azure AD token endpoint with client_credentials grant + jwt-bearer assertion
// 3. Azure AD validates the JWT against our JWKS endpoint (configured in the federated credential)
// 4. Azure AD returns a real Azure access token
//
// Parameters:
//   - tenantID: Azure AD tenant ID
//   - clientID: Azure AD application (client) ID
//   - federatedToken: The self-signed OIDC JWT to exchange
//   - scope: The Azure resource scope (typically "https://management.azure.com/.default")
func ExchangeOIDCForAzureToken(ctx context.Context, tenantID, clientID, federatedToken, scope string) (*AzureTokenResponse, error) {
	tokenURL := fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", tenantID)

	data := url.Values{
		"grant_type":            {"client_credentials"},
		"client_id":             {clientID},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {federatedToken},
		"scope":                 {scope},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to build Azure token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req) //nolint:gosec // G704: URL is from trusted Azure tenant ID (org config), not user input
	if err != nil {
		return nil, fmt.Errorf("failed to request Azure token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read Azure token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("azure token exchange failed (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var tokenResp AzureTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse Azure token response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return nil, fmt.Errorf("azure token response has empty access_token")
	}

	return &tokenResp, nil
}
