// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNewSigningKey(t *testing.T) {
	// Test auto-generated key (no env var set)
	sk, err := NewSigningKey()
	if err != nil {
		t.Fatalf("NewSigningKey() failed: %v", err)
	}

	if sk.PrivateKey() == nil {
		t.Fatal("PrivateKey() should not be nil")
	}

	if sk.KID() == "" {
		t.Fatal("KID() should not be empty")
	}
}

func TestNewSigningKeyFromBase64Env(t *testing.T) {
	// Generate a key, encode as base64 PEM, set env var, load it back
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey() failed: %v", err)
	}

	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	b64 := base64.StdEncoding.EncodeToString(pemBytes)

	// Set env var and restore after test
	t.Setenv("OIDC_SIGNING_KEY", b64)

	sk, err := NewSigningKey()
	if err != nil {
		t.Fatalf("NewSigningKey() with base64 env failed: %v", err)
	}

	// Verify the loaded key matches the original
	if sk.PrivateKey().N.Cmp(key.N) != 0 {
		t.Fatal("Loaded key modulus does not match original")
	}
	if sk.KID() == "" {
		t.Fatal("KID() should not be empty")
	}
}

func TestSigningKeyConsistency(t *testing.T) {
	// Verify that when API and runner load the same key from env, they get the same kid
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	b64 := base64.StdEncoding.EncodeToString(pemBytes)

	// Simulate two independent loads (api + runner)
	if err := os.Setenv("OIDC_SIGNING_KEY", b64); err != nil { //nolint:tenv // intentionally setting for both calls
		t.Fatalf("Setenv: %v", err)
	}
	sk1, _ := NewSigningKey()
	sk2, _ := NewSigningKey()
	if err := os.Unsetenv("OIDC_SIGNING_KEY"); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}

	if sk1.KID() != sk2.KID() {
		t.Fatalf("Two loads of the same key should produce same KID: %s vs %s", sk1.KID(), sk2.KID())
	}
}

func TestSigningKeyJWKS(t *testing.T) {
	sk, err := NewSigningKey()
	if err != nil {
		t.Fatalf("NewSigningKey() failed: %v", err)
	}

	jwks := sk.JWKS()

	keys, ok := jwks["keys"].([]map[string]interface{})
	if !ok || len(keys) == 0 {
		t.Fatal("JWKS should have at least one key")
	}

	key := keys[0]
	if key["kty"] != "RSA" {
		t.Errorf("Expected kty=RSA, got %v", key["kty"])
	}
	if key["alg"] != "RS256" {
		t.Errorf("Expected alg=RS256, got %v", key["alg"])
	}
	if key["use"] != "sig" {
		t.Errorf("Expected use=sig, got %v", key["use"])
	}
	if key["kid"] != sk.KID() {
		t.Errorf("JWKS kid should match SigningKey.KID()")
	}
	if key["n"] == nil || key["n"] == "" {
		t.Error("JWKS n (modulus) should not be empty")
	}
	if key["e"] == nil || key["e"] == "" {
		t.Error("JWKS e (exponent) should not be empty")
	}
}

func TestTokenServiceGenerateAndVerify(t *testing.T) {
	sk, err := NewSigningKey()
	if err != nil {
		t.Fatalf("NewSigningKey() failed: %v", err)
	}

	issuer := "https://app.stackweaver.io"
	ts := NewTokenService(sk, issuer)

	token, err := ts.GenerateToken(
		"api://AzureADTokenExchange",
		"my-org",
		"my-project",
		"my-workspace",
		"run-abc123",
		"plan",
	)
	if err != nil {
		t.Fatalf("GenerateToken() failed: %v", err)
	}

	// Token should be a 3-part JWT
	parts := strings.SplitN(token, ".", 3)
	if len(parts) != 3 {
		t.Fatalf("Expected 3-part JWT, got %d parts", len(parts))
	}

	// Verify the token
	claims, err := ts.VerifyToken(token)
	if err != nil {
		t.Fatalf("VerifyToken() failed: %v", err)
	}

	// Check claims
	if claims.Issuer != issuer {
		t.Errorf("Expected issuer=%s, got %s", issuer, claims.Issuer)
	}
	if claims.Audience != "api://AzureADTokenExchange" {
		t.Errorf("Expected audience=api://AzureADTokenExchange, got %s", claims.Audience)
	}

	expectedSub := "organization:my-org:project:my-project:workspace:my-workspace:run_phase:plan"
	if claims.Subject != expectedSub {
		t.Errorf("Expected subject=%s, got %s", expectedSub, claims.Subject)
	}

	if claims.TerraformOrganizationName != "my-org" {
		t.Errorf("Expected terraform_organization_name=my-org, got %s", claims.TerraformOrganizationName)
	}
	if claims.TerraformProjectName != "my-project" {
		t.Errorf("Expected terraform_project_name=my-project, got %s", claims.TerraformProjectName)
	}
	if claims.TerraformWorkspaceName != "my-workspace" {
		t.Errorf("Expected terraform_workspace_name=my-workspace, got %s", claims.TerraformWorkspaceName)
	}
	if claims.TerraformRunID != "run-abc123" {
		t.Errorf("Expected terraform_run_id=run-abc123, got %s", claims.TerraformRunID)
	}
	if claims.TerraformRunPhase != "plan" {
		t.Errorf("Expected terraform_run_phase=plan, got %s", claims.TerraformRunPhase)
	}

	// Check timing claims
	now := time.Now().Unix()
	if claims.IssuedAt > now || claims.IssuedAt < now-10 {
		t.Errorf("IssuedAt should be close to now, got %d (now=%d)", claims.IssuedAt, now)
	}
	if claims.Expiry <= now {
		t.Error("Expiry should be in the future")
	}
	if claims.NotBefore > now {
		t.Error("NotBefore should not be in the future")
	}
	if claims.JWTID == "" {
		t.Error("JWTID should not be empty")
	}
}

func TestTokenServiceVerifyInvalidSignature(t *testing.T) {
	sk1, _ := NewSigningKey()
	sk2, _ := NewSigningKey()

	ts1 := NewTokenService(sk1, "https://example.com")
	ts2 := NewTokenService(sk2, "https://example.com")

	token, err := ts1.GenerateToken("aud", "org", "proj", "ws", "run-1", "plan")
	if err != nil {
		t.Fatalf("GenerateToken() failed: %v", err)
	}

	// Verify with different key should fail
	_, err = ts2.VerifyToken(token)
	if err == nil {
		t.Fatal("VerifyToken should fail with wrong key")
	}
}

func TestGenerateWorkloadTokenInventory(t *testing.T) {
	sk, err := NewSigningKey()
	if err != nil {
		t.Fatalf("NewSigningKey() failed: %v", err)
	}

	issuer := "https://app.stackweaver.io"
	ts := NewTokenService(sk, issuer)

	token, err := ts.GenerateWorkloadToken(WorkloadTokenRequest{
		Audience:         "api://AzureADTokenExchange",
		OrganizationName: "my-org",
		ProjectName:      "my-project",
		ResourceType:     ResourceTypeInventory,
		ResourceName:     "azure-inventory",
		ActionKind:       ActionSync,
		ActionID:         "inv-abc123",
	})
	if err != nil {
		t.Fatalf("GenerateWorkloadToken() failed: %v", err)
	}

	claims, err := ts.VerifyToken(token)
	if err != nil {
		t.Fatalf("VerifyToken() failed: %v", err)
	}

	// StackWeaver subject format: no run_phase: prefix
	expectedSub := "organization:my-org:project:my-project:inventory:azure-inventory:sync"
	if claims.Subject != expectedSub {
		t.Errorf("Expected subject=%s, got %s", expectedSub, claims.Subject)
	}

	// StackWeaver-specific claims should be set
	if claims.StackweaverResourceType != string(ResourceTypeInventory) {
		t.Errorf("Expected stackweaver_resource_type=%s, got %s", ResourceTypeInventory, claims.StackweaverResourceType)
	}
	if claims.StackweaverResourceName != "azure-inventory" {
		t.Errorf("Expected stackweaver_resource_name=azure-inventory, got %s", claims.StackweaverResourceName)
	}
	if claims.StackweaverActionKind != string(ActionSync) {
		t.Errorf("Expected stackweaver_action_kind=sync, got %s", claims.StackweaverActionKind)
	}

	// TFC-compatible fields should NOT be populated for StackWeaver resources
	if claims.TerraformOrganizationName != "" {
		t.Errorf("Expected terraform_organization_name to be empty, got %s", claims.TerraformOrganizationName)
	}
	if claims.TerraformWorkspaceName != "" {
		t.Errorf("Expected terraform_workspace_name to be empty, got %s", claims.TerraformWorkspaceName)
	}
}

func TestGenerateWorkloadTokenJob(t *testing.T) {
	sk, err := NewSigningKey()
	if err != nil {
		t.Fatalf("NewSigningKey() failed: %v", err)
	}

	ts := NewTokenService(sk, "https://app.stackweaver.io")

	token, err := ts.GenerateWorkloadToken(WorkloadTokenRequest{
		Audience:         "api://AzureADTokenExchange",
		OrganizationName: "my-org",
		ProjectName:      "infra",
		ResourceType:     ResourceTypeJob,
		ResourceName:     "deploy-app",
		ActionKind:       ActionRun,
		ActionID:         "job-xyz789",
	})
	if err != nil {
		t.Fatalf("GenerateWorkloadToken() failed: %v", err)
	}

	claims, err := ts.VerifyToken(token)
	if err != nil {
		t.Fatalf("VerifyToken() failed: %v", err)
	}

	expectedSub := "organization:my-org:project:infra:job:deploy-app:run"
	if claims.Subject != expectedSub {
		t.Errorf("Expected subject=%s, got %s", expectedSub, claims.Subject)
	}

	if claims.StackweaverResourceType != string(ResourceTypeJob) {
		t.Errorf("Expected stackweaver_resource_type=%s, got %s", ResourceTypeJob, claims.StackweaverResourceType)
	}
	if claims.StackweaverActionKind != string(ActionRun) {
		t.Errorf("Expected stackweaver_action_kind=run, got %s", claims.StackweaverActionKind)
	}
}

func TestGenerateTokenTFCSubjectNotChanged(t *testing.T) {
	// Verify that GenerateToken produces TFE-compatible subjects with run_phase:
	// and does NOT include StackWeaver claims.
	sk, err := NewSigningKey()
	if err != nil {
		t.Fatalf("NewSigningKey() failed: %v", err)
	}

	ts := NewTokenService(sk, "https://app.stackweaver.io")

	token, err := ts.GenerateToken("api://AzureADTokenExchange", "main", "infra", "production", "run-123", "apply")
	if err != nil {
		t.Fatalf("GenerateToken() failed: %v", err)
	}

	claims, err := ts.VerifyToken(token)
	if err != nil {
		t.Fatalf("VerifyToken() failed: %v", err)
	}

	// TFE-compatible subject with run_phase:
	expectedSub := "organization:main:project:infra:workspace:production:run_phase:apply"
	if claims.Subject != expectedSub {
		t.Errorf("Expected subject=%s, got %s", expectedSub, claims.Subject)
	}

	// TFC claims must be populated
	if claims.TerraformOrganizationName != "main" {
		t.Errorf("Expected terraform_organization_name=main, got %s", claims.TerraformOrganizationName)
	}
	if claims.TerraformRunPhase != "apply" {
		t.Errorf("Expected terraform_run_phase=apply, got %s", claims.TerraformRunPhase)
	}

	// StackWeaver claims should NOT be populated for TFC tokens
	if claims.StackweaverResourceType != "" {
		t.Errorf("Expected stackweaver_resource_type to be empty, got %s", claims.StackweaverResourceType)
	}
	if claims.StackweaverResourceName != "" {
		t.Errorf("Expected stackweaver_resource_name to be empty, got %s", claims.StackweaverResourceName)
	}
}

func TestDefaultProjectFallback(t *testing.T) {
	// When an inventory has no project, callers pass "default" as project name.
	// Verify this produces the expected subject.
	sk, err := NewSigningKey()
	if err != nil {
		t.Fatalf("NewSigningKey() failed: %v", err)
	}

	ts := NewTokenService(sk, "https://app.stackweaver.io")

	token, err := ts.GenerateWorkloadToken(WorkloadTokenRequest{
		Audience:         "api://AzureADTokenExchange",
		OrganizationName: "my-org",
		ProjectName:      "default",
		ResourceType:     ResourceTypeInventory,
		ResourceName:     "azure-vms",
		ActionKind:       ActionSync,
		ActionID:         "inv-123",
	})
	if err != nil {
		t.Fatalf("GenerateWorkloadToken() failed: %v", err)
	}

	claims, err := ts.VerifyToken(token)
	if err != nil {
		t.Fatalf("VerifyToken() failed: %v", err)
	}

	expectedSub := "organization:my-org:project:default:inventory:azure-vms:sync"
	if claims.Subject != expectedSub {
		t.Errorf("Expected subject=%s, got %s", expectedSub, claims.Subject)
	}
}
