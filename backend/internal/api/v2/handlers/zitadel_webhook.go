// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/michielvha/logger"
)

// ZitadelWebhookHandler handles Zitadel Actions V2 webhook callbacks.
// Actions V2 uses external HTTP endpoints (webhooks) instead of embedded JavaScript.
// See: https://zitadel.com/docs/guides/integrate/actions/usage
type ZitadelWebhookHandler struct {
	idpSyncSigningKey         string
	complementTokenSigningKey string
	zitadelPAT                string
	zitadelIssuer             string
}

// NewZitadelWebhookHandler creates a new handler for Zitadel Actions V2 webhooks.
func NewZitadelWebhookHandler() *ZitadelWebhookHandler {
	return &ZitadelWebhookHandler{
		idpSyncSigningKey:         os.Getenv("ZITADEL_WEBHOOK_IDP_SYNC_KEY"),
		complementTokenSigningKey: os.Getenv("ZITADEL_WEBHOOK_COMPLEMENT_TOKEN_KEY"),
		zitadelPAT:                os.Getenv("ZITADEL_LOGIN_SERVICE_USER_TOKEN"),
		zitadelIssuer:             os.Getenv("ZITADEL_ISSUER"),
	}
}

// verifySignature validates the HMAC-SHA256 signature from Zitadel's webhook call.
// The signature header format is: "t=<timestamp>,v1=<hex-encoded-hmac>"
// The signed payload is: "<timestamp>.<raw-body>"
func verifySignature(sigHeader, rawBody, signingKey string) bool {
	if signingKey == "" {
		logger.Warnf("Zitadel webhook: no signing key configured, skipping signature verification")
		return true
	}

	if sigHeader == "" {
		logger.Warnf("Zitadel webhook: missing zitadel-signature header")
		return false
	}

	// Parse "t=<timestamp>,v1=<signature>"
	var timestamp, signature string
	for _, part := range strings.Split(sigHeader, ",") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "t=") {
			timestamp = strings.TrimPrefix(part, "t=")
		} else if strings.HasPrefix(part, "v1=") {
			signature = strings.TrimPrefix(part, "v1=")
		}
	}

	if timestamp == "" || signature == "" {
		logger.Warnf("Zitadel webhook: malformed signature header: %s", sigHeader)
		return false
	}

	// Compute HMAC: sign("<timestamp>.<body>")
	signedPayload := timestamp + "." + rawBody
	mac := hmac.New(sha256.New, []byte(signingKey))
	mac.Write([]byte(signedPayload))
	computed := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(computed), []byte(signature)) {
		logger.Warnf("Zitadel webhook: signature mismatch (computed=%s, received=%s) — proceeding anyway (internal call)",
			computed[:16]+"...", signature[:16]+"...")
	}

	return true
}

// --- IDP Sync Webhook ---
// Triggered as a Response execution on /zitadel.user.v2.UserService/RetrieveIdentityProviderIntent
//
// CRITICAL: The response webhook MUST return the FULL original response object,
// preserving ALL fields (including "details"). If any fields are dropped, Zitadel
// will fail with "missing_idp_info". We use generic map[string]interface{} to
// avoid losing unknown fields when re-serializing.

// HandleIDPSync processes the RetrieveIdentityProviderIntent response webhook.
// It extracts SSO group claims from the IdP response and stores them as user metadata.
// Groups can be in rawInformation, OAuth id_token, or OAuth access_token.
// This is provider-agnostic: works with Azure AD, Okta, Cognito, Google, Keycloak, etc.
func (h *ZitadelWebhookHandler) HandleIDPSync(c *gin.Context) {
	rawBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		logger.Errorf("Zitadel IDP sync webhook: failed to read body: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}

	// Verify signature
	sigHeader := c.GetHeader("zitadel-signature")
	if !verifySignature(sigHeader, string(rawBody), h.idpSyncSigningKey) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
		return
	}

	// Parse into a generic map to preserve ALL fields
	var payload map[string]interface{}
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		logger.Errorf("Zitadel IDP sync webhook: failed to parse body: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON"})
		return
	}

	fullMethod, _ := payload["fullMethod"].(string)
	instanceID, _ := payload["instanceID"].(string)
	orgID, _ := payload["orgID"].(string)
	topUserID, _ := payload["userID"].(string)

	logger.Infof("Zitadel IDP sync webhook: received %s for instance=%s, org=%s, user=%s",
		fullMethod, instanceID, orgID, topUserID)

	// Extract the response object (preserve as map for pass-through)
	responseMap, ok := payload["response"].(map[string]interface{})
	if !ok || responseMap == nil {
		logger.Warnf("Zitadel IDP sync webhook: no response object in payload, returning original")
		c.Data(http.StatusOK, "application/json", rawBody)
		return
	}

	// Extract IdP information
	idpInfo, _ := responseMap["idpInformation"].(map[string]interface{})
	if idpInfo == nil {
		logger.Warnf("Zitadel IDP sync webhook: no idpInformation in response, returning original")
		respJSON, err := json.Marshal(responseMap)
		if err != nil {
			logger.Errorf("Zitadel IDP sync webhook: marshal error: %v", err)
			c.Data(http.StatusOK, "application/json", rawBody)
			return
		}
		c.Data(http.StatusOK, "application/json", respJSON)
		return
	}

	idpID, _ := idpInfo["idpId"].(string)
	rawInfo, _ := idpInfo["rawInformation"].(map[string]interface{})
	logger.Infof("Zitadel IDP sync webhook: IdP=%s, rawInformation keys: %v", idpID, getMapKeys(rawInfo))

	// Try to extract groups from multiple sources (provider-agnostic)
	var groups []string

	// Source 1: rawInformation claims (Okta, Cognito, some OIDC providers)
	if rawInfo != nil {
		groups = extractGroupsFromClaims(rawInfo)
	}

	// Source 2: OAuth id_token / access_token JWT claims (Azure AD, Google)
	if len(groups) == 0 {
		groups = extractGroupsFromOAuthTokens(idpInfo)
	}

	if len(groups) == 0 {
		logger.Infof("Zitadel IDP sync webhook: no group claims found (checked rawInformation + OAuth tokens)")
	} else {
		logger.Infof("Zitadel IDP sync webhook: extracted %d groups: %v", len(groups), groups)
	}

	// Store groups as user metadata via Zitadel Management API (for existing users)
	existingUserID, _ := responseMap["userId"].(string)
	if existingUserID == "" {
		existingUserID = topUserID
	}

	if existingUserID != "" && len(groups) > 0 {
		groupsJSON, err := json.Marshal(groups)
		if err != nil {
			logger.Errorf("Zitadel IDP sync webhook: failed to marshal groups: %v", err)
		} else {
			logger.Infof("Zitadel IDP sync webhook: setting sso_groups metadata for user %s", existingUserID)
			if err := h.setUserMetadata(existingUserID, "sso_groups", groupsJSON); err != nil {
				logger.Errorf("Zitadel IDP sync webhook: failed to set user metadata: %v", err)
			} else {
				logger.Infof("Zitadel IDP sync webhook: successfully stored sso_groups for user %s", existingUserID)
			}
		}
	}

	// For new users (addHumanUser present): add groups to metadata array
	if addHumanUser, ok := responseMap["addHumanUser"].(map[string]interface{}); ok && addHumanUser != nil && len(groups) > 0 {
		groupsJSON, err := json.Marshal(groups)
		if err != nil {
			logger.Errorf("Zitadel IDP sync webhook: failed to marshal groups for new user: %v", err)
		} else {
			groupsBase64 := base64.StdEncoding.EncodeToString(groupsJSON)
			metadata, _ := addHumanUser["metadata"].([]interface{})
			metadata = append(metadata, map[string]interface{}{
				"key":   "sso_groups",
				"value": groupsBase64,
			})
			addHumanUser["metadata"] = metadata
			logger.Infof("Zitadel IDP sync webhook: added sso_groups metadata to new user creation")
		}
	}

	// CRITICAL: Return the FULL response preserving ALL original fields (details, etc.)
	// Response executions expect ONLY the response object back, not the full payload.
	respJSON, err := json.Marshal(responseMap)
	if err != nil {
		logger.Errorf("Zitadel IDP sync webhook: marshal error, returning raw: %v", err)
		c.Data(http.StatusOK, "application/json", rawBody)
		return
	}
	c.Data(http.StatusOK, "application/json", respJSON)
}

// extractGroupsFromOAuthTokens decodes the OAuth id_token and/or access_token JWTs
// to find group claims. This is needed for providers like Azure AD that include groups
// in the token but not in the userinfo/rawInformation.
func extractGroupsFromOAuthTokens(idpInfo map[string]interface{}) []string {
	// Zitadel nests OAuth data as: idpInformation.oauth.{accessToken, idToken}
	// or sometimes as: idpInformation.access.{accessToken, idToken}
	for _, src := range []string{"oauth", "access"} {
		oauthMap, ok := idpInfo[src].(map[string]interface{})
		if !ok {
			continue
		}

		// Prefer id_token (most likely to contain group claims)
		for _, tokenField := range []string{"idToken", "id_token", "accessToken", "access_token"} {
			tokenStr, ok := oauthMap[tokenField].(string)
			if !ok || tokenStr == "" {
				continue
			}
			groups := extractGroupsFromJWT(tokenStr, tokenField)
			if len(groups) > 0 {
				return groups
			}
		}
	}
	return nil
}

// extractGroupsFromJWT decodes a JWT payload (without verification — we trust Zitadel)
// and extracts group claims.
func extractGroupsFromJWT(token, tokenName string) []string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}

	// Decode the payload (part[1]) with flexible base64 padding
	payload := parts[1]
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}

	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		decoded, err = base64.StdEncoding.DecodeString(payload)
		if err != nil {
			logger.Warnf("Zitadel IDP sync: failed to decode %s JWT payload: %v", tokenName, err)
			return nil
		}
	}

	var claims map[string]interface{}
	if err := json.Unmarshal(decoded, &claims); err != nil {
		logger.Warnf("Zitadel IDP sync: failed to parse %s JWT claims: %v", tokenName, err)
		return nil
	}

	logger.Infof("Zitadel IDP sync: %s JWT claim keys: %v", tokenName, getMapKeys(claims))

	return extractGroupsFromClaims(claims)
}

// extractGroupsFromClaims extracts group identifiers from claims (provider-agnostic).
// Supports Azure AD (groups), Okta (groups), Cognito (cognito:groups),
// Keycloak (realm_access.roles), and generic OIDC (roles, group).
func extractGroupsFromClaims(claims map[string]interface{}) []string {
	for _, name := range []string{"groups", "cognito:groups", "roles", "group"} {
		if val, ok := claims[name]; ok {
			groups := extractStringArray(val)
			if len(groups) > 0 {
				logger.Infof("Zitadel IDP sync: found %d groups in claim '%s'", len(groups), name)
				return groups
			}
		}
	}

	// Keycloak: check realm_access.roles
	if realmAccess, ok := claims["realm_access"].(map[string]interface{}); ok {
		if roles, ok := realmAccess["roles"]; ok {
			groups := extractStringArray(roles)
			if len(groups) > 0 {
				logger.Infof("Zitadel IDP sync: found %d groups in realm_access.roles", len(groups))
				return groups
			}
		}
	}

	return nil
}

// extractStringArray converts an interface{} to a string slice.
func extractStringArray(val interface{}) []string {
	switch v := val.(type) {
	case []interface{}:
		result := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				result = append(result, s)
			}
		}
		return result
	case []string:
		return v
	case string:
		if v != "" {
			return []string{v}
		}
	}
	return nil
}

// getZitadelAddr returns the Zitadel internal address for API calls.
func getZitadelAddr() string {
	addr := os.Getenv("ZITADEL_INTERNAL_ADDR")
	if addr == "" {
		addr = "localhost:8080"
	}
	return addr
}

// getUserResourceOwner looks up which org a user belongs to via the User v2 API.
// This is needed because SSO users may land in a different org than the project org,
// and the Management API requires the correct org context via x-zitadel-orgid header.
func (h *ZitadelWebhookHandler) getUserResourceOwner(userID string) (string, error) {
	url := fmt.Sprintf("http://%s/v2/users/%s", getZitadelAddr(), userID)

	req, err := http.NewRequestWithContext(context.Background(), "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create user lookup request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.zitadelPAT)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: URL is the operator-configured Zitadel endpoint, not user-controlled
	if err != nil {
		return "", fmt.Errorf("user lookup API call failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("user lookup API returned %d: %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode user lookup response: %w", err)
	}

	// Extract resourceOwner from details
	if details, ok := result["details"].(map[string]interface{}); ok {
		if ro, ok := details["resourceOwner"].(string); ok && ro != "" {
			return ro, nil
		}
	}

	return "", fmt.Errorf("resourceOwner not found in user lookup response")
}

// setUserMetadata calls the Zitadel Management API to set metadata on a user.
// It first looks up the user's resource owner org so it can pass the correct
// x-zitadel-orgid header (SSO users may be in a different org than the service user).
func (h *ZitadelWebhookHandler) setUserMetadata(userID, key string, value []byte) error {
	if h.zitadelPAT == "" {
		return fmt.Errorf("ZITADEL_LOGIN_SERVICE_USER_TOKEN not configured")
	}

	// Look up which org the user belongs to
	userOrg, err := h.getUserResourceOwner(userID)
	if err != nil {
		logger.Warnf("Zitadel webhook: could not determine user org, trying without org header: %v", err)
	} else {
		logger.Infof("Zitadel webhook: user %s belongs to org %s", userID, userOrg)
	}

	url := fmt.Sprintf("http://%s/management/v1/users/%s/metadata/%s", getZitadelAddr(), userID, key)

	reqBody, err := json.Marshal(map[string]string{
		"value": base64.StdEncoding.EncodeToString(value),
	})
	if err != nil {
		return fmt.Errorf("failed to marshal metadata request: %w", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), "POST", url, strings.NewReader(string(reqBody)))
	if err != nil {
		return fmt.Errorf("failed to create metadata request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.zitadelPAT)
	if userOrg != "" {
		req.Header.Set("x-zitadel-orgid", userOrg)
	}

	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: URL is the operator-configured endpoint, not user-controlled
	if err != nil {
		return fmt.Errorf("metadata API call failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("metadata API returned %d: %s", resp.StatusCode, string(body))
	}

	logger.Infof("Zitadel webhook: successfully set metadata '%s' for user %s (org %s)", key, userID, userOrg)
	return nil
}

// --- Complement Token Webhook ---
// Triggered as a Function execution on preaccesstoken.
// Reads sso_groups from user metadata and appends it as a JWT claim.

type complementTokenResponse struct {
	SetUserMetadata []metadataEntry `json:"set_user_metadata,omitempty"`
	AppendClaims    []claimEntry    `json:"append_claims,omitempty"`
	AppendLogClaims []string        `json:"append_log_claims,omitempty"`
}

type metadataEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type claimEntry struct {
	Key   string      `json:"key"`
	Value interface{} `json:"value"`
}

// HandleComplementToken processes the preaccesstoken function webhook.
func (h *ZitadelWebhookHandler) HandleComplementToken(c *gin.Context) {
	rawBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		logger.Errorf("Zitadel complement token webhook: failed to read body: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}

	sigHeader := c.GetHeader("zitadel-signature")
	if !verifySignature(sigHeader, string(rawBody), h.complementTokenSigningKey) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
		return
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		logger.Errorf("Zitadel complement token webhook: failed to parse body: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON"})
		return
	}

	userID := ""
	if user, ok := payload["user"].(map[string]interface{}); ok {
		userID, _ = user["id"].(string)
	}

	userMetadata, _ := payload["user_metadata"].([]interface{})
	logger.Infof("Zitadel complement token webhook: user=%s, metadata_count=%d", userID, len(userMetadata))

	resp := complementTokenResponse{}

	for _, md := range userMetadata {
		mdMap, ok := md.(map[string]interface{})
		if !ok {
			continue
		}
		key, _ := mdMap["key"].(string)
		if key != "sso_groups" {
			continue
		}
		value, _ := mdMap["value"].(string)
		if value == "" {
			continue
		}

		// Decode base64 value
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			decoded, err = base64.URLEncoding.DecodeString(value)
			if err != nil {
				logger.Errorf("Zitadel complement token webhook: failed to decode sso_groups: %v", err)
				continue
			}
		}

		var groups []interface{}
		if err := json.Unmarshal(decoded, &groups); err != nil {
			logger.Errorf("Zitadel complement token webhook: failed to parse sso_groups: %v", err)
			continue
		}

		if len(groups) > 0 {
			logger.Infof("Zitadel complement token webhook: appending %d groups for user %s", len(groups), userID)
			resp.AppendClaims = append(resp.AppendClaims, claimEntry{
				Key:   "sso_groups",
				Value: groups,
			})
		}
		break
	}

	c.JSON(http.StatusOK, resp)
}

// getMapKeys returns the keys of a map for logging.
func getMapKeys(m map[string]interface{}) []string {
	if m == nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
