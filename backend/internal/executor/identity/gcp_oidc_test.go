// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package identity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildGCPOIDCEnv_WritesFilesAndEnv(t *testing.T) {
	dir := t.TempDir()
	token := "header.payload.signature"
	sa := "stackweaver@my-project.iam.gserviceaccount.com"
	projectNumber := "123456789012"
	provider := "projects/123456789012/locations/global/workloadIdentityPools/sw/providers/sw"

	env, err := BuildGCPOIDCEnv(sa, projectNumber, provider, token, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// GOOGLE_APPLICATION_CREDENTIALS must point at the credential-config JSON.
	credPath := env["GOOGLE_APPLICATION_CREDENTIALS"]
	if credPath != filepath.Join(dir, gcpOIDCCredConfigFile) {
		t.Fatalf("GOOGLE_APPLICATION_CREDENTIALS = %q", credPath)
	}
	if env["TFC_GCP_PROVIDER_AUTH"] != "true" {
		t.Errorf("TFC_GCP_PROVIDER_AUTH = %q, want true", env["TFC_GCP_PROVIDER_AUTH"])
	}
	if env["TFC_GCP_RUN_SERVICE_ACCOUNT_EMAIL"] != sa {
		t.Errorf("TFC_GCP_RUN_SERVICE_ACCOUNT_EMAIL = %q", env["TFC_GCP_RUN_SERVICE_ACCOUNT_EMAIL"])
	}
	if env["TFC_GCP_PROJECT_NUMBER"] != projectNumber {
		t.Errorf("TFC_GCP_PROJECT_NUMBER = %q", env["TFC_GCP_PROJECT_NUMBER"])
	}

	// The token file must exist, contain exactly the token, and be private (0600).
	tokenPath := filepath.Join(dir, gcpOIDCTokenFile)
	tokenData, readErr := os.ReadFile(tokenPath) //nolint:gosec // test-controlled path
	if readErr != nil {
		t.Fatalf("token file not written: %v", readErr)
	}
	if string(tokenData) != token {
		t.Errorf("token file contents = %q, want %q", string(tokenData), token)
	}
	if info, statErr := os.Stat(tokenPath); statErr != nil {
		t.Fatalf("stat token file: %v", statErr)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file perm = %o, want 600", perm)
	}

	// The credential-config JSON must be a valid external_account config with the WIF audience.
	credData, readErr := os.ReadFile(credPath) //nolint:gosec // test-controlled path
	if readErr != nil {
		t.Fatalf("credential config not written: %v", readErr)
	}
	if info, statErr := os.Stat(credPath); statErr != nil {
		t.Fatalf("stat credential config: %v", statErr)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credential config perm = %o, want 600", perm)
	}

	var cfg gcpExternalAccountConfig
	if err := json.Unmarshal(credData, &cfg); err != nil {
		t.Fatalf("credential config is not valid JSON: %v", err)
	}
	if cfg.Type != "external_account" {
		t.Errorf("type = %q, want external_account", cfg.Type)
	}
	if want := "//iam.googleapis.com/" + provider; cfg.Audience != want {
		t.Errorf("audience = %q, want %q", cfg.Audience, want)
	}
	if cfg.TokenURL != "https://sts.googleapis.com/v1/token" {
		t.Errorf("token_url = %q", cfg.TokenURL)
	}
	if cfg.CredentialSource.File != tokenPath {
		t.Errorf("credential_source.file = %q, want %q", cfg.CredentialSource.File, tokenPath)
	}
	if cfg.ServiceAccountImpersonationURL == "" {
		t.Error("service_account_impersonation_url must be set")
	}
}

func TestBuildGCPOIDCEnv_RejectsEmptyInputs(t *testing.T) {
	dir := t.TempDir()
	if _, err := BuildGCPOIDCEnv("sa@x.iam.gserviceaccount.com", "1", "p", "", dir); err == nil {
		t.Error("expected error for empty token")
	}
	if _, err := BuildGCPOIDCEnv("", "1", "p", "tok", dir); err == nil {
		t.Error("expected error for empty service account email")
	}
	if _, err := BuildGCPOIDCEnv("sa@x.iam.gserviceaccount.com", "1", "", "tok", dir); err == nil {
		t.Error("expected error for empty workload provider name")
	}
}

func TestMaterializeGCPWorkloadIdentity(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{
		"GCP_OIDC_RAW_TOKEN":              "header.payload.sig",
		"GCP_OIDC_SERVICE_ACCOUNT_EMAIL":  "stackweaver@my-project.iam.gserviceaccount.com",
		"GCP_OIDC_WORKLOAD_PROVIDER_NAME": "projects/1/locations/global/workloadIdentityPools/sw/providers/sw",
		"GCP_OIDC_PROJECT_NUMBER":         "1",
	}
	if err := MaterializeGCPWorkloadIdentity(env, dir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Raw passing keys must be dropped after materialization.
	for _, k := range []string{"GCP_OIDC_RAW_TOKEN", "GCP_OIDC_SERVICE_ACCOUNT_EMAIL", "GCP_OIDC_WORKLOAD_PROVIDER_NAME", "GCP_OIDC_PROJECT_NUMBER"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s should be removed from env after materialization", k)
		}
	}
	credPath := env["GOOGLE_APPLICATION_CREDENTIALS"]
	if credPath != filepath.Join(dir, gcpOIDCCredConfigFile) {
		t.Fatalf("GOOGLE_APPLICATION_CREDENTIALS = %q", credPath)
	}
	if env["TFC_GCP_PROVIDER_AUTH"] != "true" {
		t.Errorf("TFC_GCP_PROVIDER_AUTH = %q, want true", env["TFC_GCP_PROVIDER_AUTH"])
	}
	tokenData, err := os.ReadFile(filepath.Join(dir, gcpOIDCTokenFile)) //nolint:gosec // test-controlled path
	if err != nil || string(tokenData) != "header.payload.sig" {
		t.Fatalf("token file contents = %q (err=%v)", string(tokenData), err)
	}
}

func TestMaterializeGCPWorkloadIdentity_NoopWhenAbsent(t *testing.T) {
	env := map[string]string{"FOO": "bar"}
	if err := MaterializeGCPWorkloadIdentity(env, t.TempDir()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := env["GOOGLE_APPLICATION_CREDENTIALS"]; ok {
		t.Error("should not set GOOGLE_APPLICATION_CREDENTIALS when no token present")
	}
}
