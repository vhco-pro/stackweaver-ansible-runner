// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package auth

// Reference: https://zitadel.com/docs/guides/integrate/login/oidc/login-users
// Using Zitadel OIDC library for proper token verification: https://github.com/zitadel/oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-jose/go-jose/v4"
	"github.com/michielvha/logger"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"golang.org/x/oauth2"
)

// remoteKeySet implements oidc.KeySet for remote JWKS
type remoteKeySet struct {
	httpClient   *http.Client
	jwksURL      string
	hostOverride string // Host header override for in-cluster requests to Zitadel
	keys         []jose.JSONWebKey
}

func (r *remoteKeySet) VerifySignature(ctx context.Context, jws *jose.JSONWebSignature) ([]byte, error) {
	// Fetch keys if not cached
	if len(r.keys) == 0 {
		if err := r.fetchKeys(ctx); err != nil {
			return nil, fmt.Errorf("failed to fetch keys: %w", err)
		}
	}

	// Try each key until one works
	// jws.Verify() expects a single crypto key (not []JSONWebKey)
	// Extract the actual crypto key from each JSONWebKey.Key field
	var lastErr error
	for _, key := range r.keys {
		if key.Key == nil {
			continue
		}

		// jws.Verify() accepts the actual crypto key (e.g., *rsa.PublicKey, *ecdsa.PublicKey)
		payload, err := jws.Verify(key.Key)
		if err == nil {
			return payload, nil
		}
		lastErr = err
	}

	if lastErr != nil {
		return nil, fmt.Errorf("signature verification failed with all keys: %w", lastErr)
	}
	return nil, fmt.Errorf("no valid keys found in JWKS")
}

func (r *remoteKeySet) fetchKeys(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", r.jwksURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// When fetching JWKS via an internal K8s service address, Zitadel uses the
	// Host header to identify the instance. Override it with the external issuer
	// hostname so Zitadel can route to the correct tenant.
	if r.hostOverride != "" {
		req.Host = r.hostOverride
	}

	resp, err := r.httpClient.Do(req) //nolint:gosec // G704: URL is the operator-configured Zitadel JWKS endpoint, not user-controlled
	if err != nil {
		return fmt.Errorf("failed to fetch keys: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Warnf("Failed to close response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to fetch keys: status %d", resp.StatusCode)
	}

	// Use go-jose's built-in JWKS parsing
	var jwks jose.JSONWebKeySet
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return fmt.Errorf("failed to decode JWKS: %w", err)
	}

	if len(jwks.Keys) == 0 {
		return fmt.Errorf("no keys found in JWKS")
	}

	r.keys = jwks.Keys
	return nil
}

// ZitadelVerifier handles JWT token verification for Zitadel using the OIDC library
type ZitadelVerifier struct {
	issuer       string
	clientID     string
	clientSecret string
	httpClient   *http.Client
	keySet       oidc.KeySet
	internalAddr string // K8s-internal service address (e.g. "zitadel:8080"), bypasses TLS
	hostOverride string // External issuer hostname sent as Host header for tenant routing
}

// NewZitadelVerifier creates a new Zitadel JWT verifier using the OIDC library.
// If internalAddr is provided (e.g. "localhost:8080"), JWKS keys are fetched from
// http://{internalAddr}/oauth/v2/keys instead of the external issuer URL.
// This allows the API to validate JWTs issued with an external domain as issuer
// while fetching keys internally without going through the public internet.
func NewZitadelVerifier(issuer, clientID, clientSecret, internalAddr string) (*ZitadelVerifier, error) {
	// Create HTTP client
	httpClient := oauth2.NewClient(context.Background(), nil)

	// Use internal address for JWKS if provided, otherwise fall back to issuer.
	// When using an internal address (e.g. K8s service name), we must also send
	// the external issuer hostname as the Host header so Zitadel can identify
	// the correct instance (Zitadel routes tenants by the Host header).
	jwksURL := issuer + "/oauth/v2/keys"
	var hostOverride string
	if internalAddr != "" {
		jwksURL = "http://" + internalAddr + "/oauth/v2/keys"
		if u, err := url.Parse(issuer); err == nil {
			hostOverride = u.Host
		}
	}

	// Create remote key set that implements oidc.KeySet
	keySet := &remoteKeySet{
		httpClient:   httpClient,
		jwksURL:      jwksURL,
		hostOverride: hostOverride,
	}

	return &ZitadelVerifier{
		issuer:       issuer,
		clientID:     clientID,
		clientSecret: clientSecret,
		httpClient:   httpClient,
		keySet:       keySet,
		internalAddr: internalAddr,
		hostOverride: hostOverride,
	}, nil
}

// VerifyToken verifies a JWT token and returns the claims using the OIDC library
// Returns both the validated claims and the raw claims map (for custom Zitadel fields)
func (v *ZitadelVerifier) VerifyToken(ctx context.Context, tokenString string) (*oidc.AccessTokenClaims, map[string]interface{}, error) {
	// Verify that token is a JWT (has dots separating header.payload.signature)
	if !strings.Contains(tokenString, ".") {
		return nil, nil, fmt.Errorf("token is not a JWT (expected JWT format, got opaque token). Please configure Zitadel to issue JWT tokens")
	}

	// Parse JWS to get signature info - needed for CheckSignature
	supportedAlgs := []jose.SignatureAlgorithm{jose.RS256, jose.ES256, jose.PS256}
	jws, err := jose.ParseSigned(tokenString, supportedAlgs)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse JWS: %w", err)
	}

	// Verify signature using the KeySet to get the payload
	payload, err := v.keySet.VerifySignature(ctx, jws)
	if err != nil {
		return nil, nil, fmt.Errorf("signature verification failed: %w", err)
	}

	// Unmarshal payload into a map FIRST to preserve all claims (including custom Zitadel fields)
	var claimsMap map[string]interface{}
	if err := json.Unmarshal(payload, &claimsMap); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal claims map: %w", err)
	}

	// Create claims object and unmarshal payload into it for validation
	claims := &oidc.AccessTokenClaims{}
	if err := json.Unmarshal(payload, claims); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal claims: %w", err)
	}

	// Validate issuer
	if err := oidc.CheckIssuer(claims, v.issuer); err != nil {
		return nil, nil, fmt.Errorf("issuer validation failed: %w", err)
	}

	// Validate audience (lenient - access tokens might have project ID as audience)
	_ = oidc.CheckAudience(claims, v.clientID)

	// Validate expiration
	if err := oidc.CheckExpiration(claims, 0); err != nil {
		return nil, nil, fmt.Errorf("token expired: %w", err)
	}

	return claims, claimsMap, nil
}

// UserInfo represents user information extracted from token
type UserInfo struct {
	ID         string
	Email      string
	Name       string
	GivenName  string
	FamilyName string
	Subject    string
	Groups     []string // SSO group IDs from external IdP (via Zitadel Actions)
}

// ExtractUserInfo extracts user information from token claims
// Uses the raw claims map to extract custom Zitadel fields (email, name, etc.)
// If email is missing from claims, calls UserInfo endpoint (standard OIDC fallback).
// internalAddr and hostOverride are optional: when set, the UserInfo call is routed
// through the internal K8s service address (plain HTTP) instead of the external issuer
// URL, avoiding TLS certificate trust issues with corporate/private CAs.
func ExtractUserInfo(ctx context.Context, claims *oidc.AccessTokenClaims, claimsMap map[string]interface{}, issuer, tokenString string, httpClient *http.Client, internalAddr, hostOverride string) *UserInfo {
	info := &UserInfo{
		Subject: claims.Subject,
		ID:      claims.Subject,
	}

	if claimsMap == nil {
		return info
	}

	// Extract email
	var emailFound bool
	if email, ok := claimsMap["email"].(string); ok && email != "" {
		info.Email = email
		emailFound = true
	}

	// If email is missing, call UserInfo endpoint (standard OIDC practice)
	var userInfoData map[string]interface{}
	if !emailFound && issuer != "" && tokenString != "" && httpClient != nil {
		userInfoData = fetchUserInfoFromEndpoint(ctx, issuer, tokenString, httpClient, internalAddr, hostOverride)
		if userInfoData != nil {
			// Try email field
			if email, ok := userInfoData["email"].(string); ok && email != "" {
				info.Email = email
				emailFound = true
				logger.Debugf("ExtractUserInfo - Email retrieved from UserInfo endpoint for Subject=%s", claims.Subject)
			}
		}
		if !emailFound {
			// Debug: log available claims to diagnose
			keys := make([]string, 0, len(claimsMap))
			for k := range claimsMap {
				keys = append(keys, k)
			}
			logger.Debugf("ExtractUserInfo - Email not found in token claims or UserInfo. Subject=%s, Available keys: %v", claims.Subject, keys)
		}
	} else if !emailFound {
		// Debug: log available claims to diagnose
		keys := make([]string, 0, len(claimsMap))
		for k := range claimsMap {
			keys = append(keys, k)
		}
		logger.Debugf("ExtractUserInfo - Email not found in token claims. Subject=%s, Available keys: %v", claims.Subject, keys)
	}

	// Extract name (full name) - Zitadel uses "name" field
	if name, ok := claimsMap["name"].(string); ok && name != "" {
		info.Name = name
	}

	// Extract userName (Zitadel-specific attribute)
	if userName, ok := claimsMap["userName"].(string); ok && userName != "" {
		// Use userName if name is not set
		if info.Name == "" {
			info.Name = userName
		}
	}

	// Extract given_name
	if givenName, ok := claimsMap["given_name"].(string); ok && givenName != "" {
		info.GivenName = givenName
	}

	// Extract family_name
	if familyName, ok := claimsMap["family_name"].(string); ok && familyName != "" {
		info.FamilyName = familyName
	}

	// If name is not set but given_name and family_name are, construct name
	if info.Name == "" && info.GivenName != "" && info.FamilyName != "" {
		info.Name = info.GivenName + " " + info.FamilyName
	}

	// If name is still not set, try UserInfo if we already fetched it
	if info.Name == "" && userInfoData != nil {
		if nameStr, ok := userInfoData["name"].(string); ok && nameStr != "" {
			info.Name = nameStr
		}
	}

	// Extract SSO groups (set by Zitadel Actions from external IdP claims)
	// The "sso_groups" claim is a normalized array of group identifiers
	// populated by the complementTokenGroups Zitadel Action.
	if groups, ok := claimsMap["sso_groups"]; ok {
		logger.Infof("ExtractUserInfo - Found sso_groups claim (type %T) for Subject=%s", groups, claims.Subject)
		if groupArray, ok := groups.([]interface{}); ok {
			for _, g := range groupArray {
				if groupStr, ok := g.(string); ok && groupStr != "" {
					info.Groups = append(info.Groups, groupStr)
				}
			}
			if len(info.Groups) > 0 {
				logger.Infof("ExtractUserInfo - Extracted %d SSO groups for Subject=%s: %v", len(info.Groups), claims.Subject, info.Groups)
			}
		}
	} else {
		logger.Infof("ExtractUserInfo - No sso_groups claim found in JWT for Subject=%s (available claims: %v)", claims.Subject, getClaimKeys(claimsMap))
	}

	return info
}

// getClaimKeys returns the keys from a claims map for diagnostic logging.
func getClaimKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// fetchUserInfoFromEndpoint calls the OIDC UserInfo endpoint.
// When internalAddr is set (K8s deployments), the request is routed through the
// internal service address over plain HTTP, with the external issuer hostname sent
// as the Host header for Zitadel tenant routing.  This mirrors the JWKS fetch
// pattern and avoids TLS certificate trust issues with corporate/private CAs.
// Returns the parsed JSON response or nil on error.
func fetchUserInfoFromEndpoint(ctx context.Context, issuer, tokenString string, httpClient *http.Client, internalAddr, hostOverride string) map[string]interface{} {
	// Zitadel UserInfo endpoint: /oidc/v1/userinfo (from defaults.yaml)
	var userInfoURL string
	if internalAddr != "" {
		userInfoURL = "http://" + internalAddr + "/oidc/v1/userinfo"
	} else {
		userInfoURL = issuer + "/oidc/v1/userinfo"
	}

	req, err := http.NewRequestWithContext(ctx, "GET", userInfoURL, nil)
	if err != nil {
		logger.Warnf("Failed to create UserInfo request: %v", err)
		return nil
	}

	// When using an internal address, set the Host header so Zitadel can identify
	// the correct instance (Zitadel routes tenants by the Host header).
	if hostOverride != "" {
		req.Host = hostOverride
	}

	// Set Authorization header with Bearer token (OIDC standard)
	req.Header.Set("Authorization", "Bearer "+tokenString)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req) //nolint:gosec // G704: URL is the operator-configured Zitadel UserInfo endpoint, not user-controlled
	if err != nil {
		logger.Warnf("Failed to call UserInfo endpoint: %v", err)
		return nil
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.Warnf("Failed to close UserInfo response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		logger.Warnf("UserInfo endpoint returned status %d", resp.StatusCode)
		return nil
	}

	var userInfo map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&userInfo); err != nil {
		logger.Warnf("Failed to decode UserInfo response: %v", err)
		return nil
	}

	return userInfo
}
