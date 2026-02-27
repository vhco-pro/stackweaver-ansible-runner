// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package oidc

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ResourceType identifies what kind of StackWeaver resource is requesting the OIDC token.
// This appears as the resource segment in the JWT subject claim.
type ResourceType string

const (
	// ResourceTypeInventory is for Ansible dynamic inventory sync.
	ResourceTypeInventory ResourceType = "inventory"

	// ResourceTypeJob is for Ansible job/playbook execution.
	ResourceTypeJob ResourceType = "job"
)

// ActionKind identifies the type of action being performed.
type ActionKind string

const (
	ActionSync ActionKind = "sync"
	ActionRun  ActionKind = "run"
)

// WorkloadTokenRequest contains all the parameters needed to generate a StackWeaver
// workload identity token. The subject format is:
//
//	organization:<org>:project:<project>:<resource_type>:<resource_name>:<action_kind>
//
// Examples:
//
//	Inventory sync:   organization:main:project:infra:inventory:azure-vms:sync
//	Ansible job:      organization:main:project:infra:job:deploy-app:run
//
// This format is ONLY for StackWeaver-native resources. Terraform workspace runs use
// GenerateToken which produces TFE-compatible subjects that must not be changed.
type WorkloadTokenRequest struct {
	// Audience for the token (e.g., "api://AzureADTokenExchange" for Azure)
	Audience string

	// OrganizationName is the StackWeaver organization name
	OrganizationName string

	// ProjectName is the StackWeaver project name. For org-scoped resources without
	// a project, use "default".
	ProjectName string

	// ResourceType is the kind of resource (inventory, job)
	ResourceType ResourceType

	// ResourceName is the name of the resource (inventory name, job name, etc.)
	ResourceName string

	// ActionKind is the type of action (sync, run)
	ActionKind ActionKind

	// ActionID is a unique identifier for the specific action (job ID, inventory ID, etc.)
	ActionID string
}

// TokenClaims contains the claims for an OIDC workload identity token.
// Includes TFC-compatible fields for backward compatibility with terraform-provider-tfe,
// plus StackWeaver-specific fields for native resources.
type TokenClaims struct {
	// Standard OIDC claims
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	Audience  string `json:"aud"`
	IssuedAt  int64  `json:"iat"`
	Expiry    int64  `json:"exp"`
	NotBefore int64  `json:"nbf"`
	JWTID     string `json:"jti"`

	// TFC-compatible custom claims (populated by GenerateToken for Terraform runs)
	TerraformOrganizationName string `json:"terraform_organization_name,omitempty"`
	TerraformProjectName      string `json:"terraform_project_name,omitempty"`
	TerraformWorkspaceName    string `json:"terraform_workspace_name,omitempty"`
	TerraformRunID            string `json:"terraform_run_id,omitempty"`
	TerraformRunPhase         string `json:"terraform_run_phase,omitempty"`

	// StackWeaver-specific claims (populated by GenerateWorkloadToken for native resources)
	StackweaverResourceType string `json:"stackweaver_resource_type,omitempty"`
	StackweaverResourceName string `json:"stackweaver_resource_name,omitempty"`
	StackweaverActionKind   string `json:"stackweaver_action_kind,omitempty"`
}

// TokenService generates OIDC workload identity tokens signed with the platform's RSA key.
type TokenService struct {
	signingKey *SigningKey
	issuerURL  string
}

// NewTokenService creates a TokenService.
func NewTokenService(signingKey *SigningKey, issuerURL string) *TokenService {
	return &TokenService{
		signingKey: signingKey,
		issuerURL:  issuerURL,
	}
}

// GenerateWorkloadToken creates a signed JWT for StackWeaver-native workload identity operations.
// The subject format is: organization:<org>:project:<project>:<resource_type>:<resource_name>:<action_kind>
//
// This is for StackWeaver-specific resources (inventories, jobs, etc.) — NOT for Terraform
// workspace runs. Terraform uses GenerateToken which produces the TFE-compatible subject format.
func (ts *TokenService) GenerateWorkloadToken(req WorkloadTokenRequest) (string, error) {
	now := time.Now()

	// Generate a unique JWT ID
	jtiBytes := make([]byte, 16)
	if _, err := rand.Read(jtiBytes); err != nil {
		return "", fmt.Errorf("failed to generate JWT ID: %w", err)
	}
	jti := base64.RawURLEncoding.EncodeToString(jtiBytes)

	// Build StackWeaver subject (no run_phase: prefix — that's TFE-only)
	subject := fmt.Sprintf("organization:%s:project:%s:%s:%s:%s",
		req.OrganizationName, req.ProjectName, req.ResourceType, req.ResourceName, req.ActionKind)

	claims := TokenClaims{
		Issuer:    ts.issuerURL,
		Subject:   subject,
		Audience:  req.Audience,
		IssuedAt:  now.Unix(),
		Expiry:    now.Add(1 * time.Hour).Unix(),
		NotBefore: now.Unix(),
		JWTID:     jti,

		// StackWeaver-specific claims only (no TFC fields for native resources)
		StackweaverResourceType: string(req.ResourceType),
		StackweaverResourceName: req.ResourceName,
		StackweaverActionKind:   string(req.ActionKind),
	}

	return ts.signJWT(claims)
}

// GenerateToken creates a signed JWT with TFC-compatible workload identity claims.
// The subject format matches TFC exactly and MUST NOT be changed:
//
//	organization:<org>:project:<project>:workspace:<workspace>:run_phase:<phase>
//
// This is ONLY for Terraform workspace runs (plan/apply). For StackWeaver-native
// resources (inventories, jobs), use GenerateWorkloadToken instead.
func (ts *TokenService) GenerateToken(
	audience string,
	orgName string,
	projectName string,
	workspaceName string,
	runID string,
	runPhase string,
) (string, error) {
	now := time.Now()

	// Generate a unique JWT ID
	jtiBytes := make([]byte, 16)
	if _, err := rand.Read(jtiBytes); err != nil {
		return "", fmt.Errorf("failed to generate JWT ID: %w", err)
	}
	jti := base64.RawURLEncoding.EncodeToString(jtiBytes)

	// Build subject in TFC-compatible format (DO NOT CHANGE — must match terraform-provider-tfe)
	subject := fmt.Sprintf("organization:%s:project:%s:workspace:%s:run_phase:%s",
		orgName, projectName, workspaceName, runPhase)

	claims := TokenClaims{
		Issuer:    ts.issuerURL,
		Subject:   subject,
		Audience:  audience,
		IssuedAt:  now.Unix(),
		Expiry:    now.Add(1 * time.Hour).Unix(),
		NotBefore: now.Unix(),
		JWTID:     jti,

		// TFC-compatible claims only (no StackWeaver fields for TFE resources)
		TerraformOrganizationName: orgName,
		TerraformProjectName:      projectName,
		TerraformWorkspaceName:    workspaceName,
		TerraformRunID:            runID,
		TerraformRunPhase:         runPhase,
	}

	return ts.signJWT(claims)
}

func (ts *TokenService) signJWT(claims TokenClaims) (string, error) {
	// Header
	header := map[string]string{
		"alg": "RS256",
		"typ": "JWT",
		"kid": ts.signingKey.KID(),
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("failed to marshal JWT header: %w", err)
	}

	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("failed to marshal JWT claims: %w", err)
	}

	// Encode header and payload
	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	claimsB64 := base64.RawURLEncoding.EncodeToString(claimsJSON)

	// Create signing input
	signingInput := headerB64 + "." + claimsB64

	// Sign with RSA-SHA256
	hash := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, ts.signingKey.PrivateKey(), crypto.SHA256, hash[:])
	if err != nil {
		return "", fmt.Errorf("failed to sign JWT: %w", err)
	}

	signatureB64 := base64.RawURLEncoding.EncodeToString(signature)

	return signingInput + "." + signatureB64, nil
}

// VerifyToken parses and verifies a JWT token (useful for testing).
func (ts *TokenService) VerifyToken(tokenString string) (*TokenClaims, error) {
	parts := strings.SplitN(tokenString, ".", 3)
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid JWT format: expected 3 parts, got %d", len(parts))
	}

	// Verify signature
	signingInput := parts[0] + "." + parts[1]
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("failed to decode signature: %w", err)
	}

	hash := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(&ts.signingKey.PrivateKey().PublicKey, crypto.SHA256, hash[:], signature); err != nil {
		return nil, fmt.Errorf("invalid signature: %w", err)
	}

	// Decode claims
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("failed to decode claims: %w", err)
	}

	var claims TokenClaims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return nil, fmt.Errorf("failed to parse claims: %w", err)
	}

	return &claims, nil
}
