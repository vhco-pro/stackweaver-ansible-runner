// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package identity

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michielvha/stackweaver/core/services/oidc"
)

// TestVaultLoginAgainstMockVault is the runtime verification for tfe_vault_oidc_configuration without a
// real Vault: it proves our runner exchanges a Stackweaver-minted JWT for a Vault client token via the
// JWT auth method. A mock Vault validates the presented JWT exactly as real Vault would (signature
// against the issuer's JWKS, audience, subject) before returning a client token.
func TestVaultLoginAgainstMockVault(t *testing.T) {
	t.Setenv("DEV_INSECURE_KEY", "1") // AUD-121: NewSigningKey fails closed without a key/dev flag
	signingKey, err := oidc.NewSigningKey()
	if err != nil {
		t.Fatalf("NewSigningKey: %v", err)
	}
	ts := oidc.NewTokenService(signingKey, "https://stackweaver.example.com")

	token, err := ts.GenerateToken(oidc.TokenRequest{
		Audience:         oidc.VaultWorkloadIdentityAudience,
		OrganizationName: "dev-test", ProjectName: "default", WorkspaceName: "production", RunID: "run-abc123", RunPhase: "plan",
	})
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}

	var gotRole string
	mockVault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Vault JWT auth login: POST /v1/auth/jwt/login  {"role":..., "jwt":...}
		if !strings.HasSuffix(r.URL.Path, "/v1/auth/jwt/login") {
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct{ Role, JWT string }
		_ = json.Unmarshal(body, &req)
		gotRole = req.Role

		claims, verr := ts.VerifyToken(req.JWT) // real signature check against the issuer's key
		if verr != nil {
			http.Error(w, "invalid jwt: "+verr.Error(), http.StatusBadRequest)
			return
		}
		if claims.Audience != oidc.VaultWorkloadIdentityAudience {
			http.Error(w, "wrong audience: "+claims.Audience, http.StatusForbidden)
			return
		}
		if !strings.HasPrefix(claims.Subject, "organization:dev-test:") {
			http.Error(w, "unexpected subject: "+claims.Subject, http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		//nolint:gosec,errchkjson // G101: mock Vault response token in a test, not a real credential
		_ = json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{"client_token": "hvs.mock-vault-token"},
		})
	}))
	defer mockVault.Close()

	dir := t.TempDir()
	env, err := BuildVaultOIDCEnv(context.Background(), mockVault.URL, "stackweaver", "", "jwt", "", token, dir)
	if err != nil {
		t.Fatalf("BuildVaultOIDCEnv: %v", err)
	}
	if gotRole != "stackweaver" {
		t.Errorf("mock Vault saw role %q, want stackweaver", gotRole)
	}
	if env["VAULT_TOKEN"] != "hvs.mock-vault-token" {
		t.Errorf("VAULT_TOKEN = %q, want the client token from Vault", env["VAULT_TOKEN"])
	}
	if env["VAULT_ADDR"] != mockVault.URL {
		t.Errorf("VAULT_ADDR = %q, want %q", env["VAULT_ADDR"], mockVault.URL)
	}
	if env["TFC_VAULT_PROVIDER_AUTH"] != "true" {
		t.Errorf("TFC_VAULT_PROVIDER_AUTH = %q, want true", env["TFC_VAULT_PROVIDER_AUTH"])
	}
	if env["TFC_VAULT_RUN_ROLE"] != "stackweaver" {
		t.Errorf("TFC_VAULT_RUN_ROLE = %q", env["TFC_VAULT_RUN_ROLE"])
	}
}

func TestVaultLogin_RejectsEmptyInputs(t *testing.T) {
	ctx := context.Background()
	if _, err := vaultLogin(ctx, "", "jwt", "role", "", nil, "tok"); err == nil {
		t.Error("expected error for empty address")
	}
	if _, err := vaultLogin(ctx, "https://v", "jwt", "", "", nil, "tok"); err == nil {
		t.Error("expected error for empty role")
	}
	if _, err := vaultLogin(ctx, "https://v", "jwt", "role", "", nil, ""); err == nil {
		t.Error("expected error for empty token")
	}
}

func TestVaultRunEnv_WritesCACertAndNamespace(t *testing.T) {
	dir := t.TempDir()
	caPEM := []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
	env, err := vaultRunEnv("https://vault.example.com", "admin/team-a", "kubernetes", "hvs.tok", "role1", caPEM, dir)
	if err != nil {
		t.Fatalf("vaultRunEnv: %v", err)
	}
	if env["VAULT_NAMESPACE"] != "admin/team-a" {
		t.Errorf("VAULT_NAMESPACE = %q", env["VAULT_NAMESPACE"])
	}
	if env["TFC_VAULT_AUTH_PATH"] != "kubernetes" {
		t.Errorf("TFC_VAULT_AUTH_PATH = %q", env["TFC_VAULT_AUTH_PATH"])
	}
	caPath := env["VAULT_CACERT"]
	if caPath != filepath.Join(dir, vaultCACertFile) {
		t.Fatalf("VAULT_CACERT = %q", caPath)
	}
	data, readErr := os.ReadFile(caPath) //nolint:gosec // test-controlled path
	if readErr != nil || !strings.Contains(string(data), "BEGIN CERTIFICATE") {
		t.Fatalf("CA file contents = %q (err=%v)", string(data), readErr)
	}
}

func TestDecodeVaultCACert(t *testing.T) {
	rawPEM := "-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----"
	if got := string(decodeVaultCACert(rawPEM)); got != rawPEM {
		t.Errorf("raw PEM should pass through, got %q", got)
	}
	// base64 of the PEM should decode back to it.
	b64 := "LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCmFiYwotLS0tLUVORCBDRVJUSUZJQ0FURS0tLS0t"
	if got := string(decodeVaultCACert(b64)); !strings.Contains(got, "BEGIN CERTIFICATE") {
		t.Errorf("base64 PEM should decode to PEM, got %q", got)
	}
	if decodeVaultCACert("") != nil {
		t.Error("empty input should return nil")
	}
}

func TestMaterializeVaultToken_NoopWhenAbsent(t *testing.T) {
	env := map[string]string{"FOO": "bar"}
	if err := MaterializeVaultToken(context.Background(), env, t.TempDir()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := env["VAULT_TOKEN"]; ok {
		t.Error("should not set VAULT_TOKEN when no raw token present")
	}
}
