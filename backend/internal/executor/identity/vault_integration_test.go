// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/core/services/oidc"
)

// TestVaultLogin_RealVaultContainer is the real end-to-end runtime proof for
// tfe_vault_oidc_configuration: it stands up a throwaway `hashicorp/vault` dev container, configures
// its JWT auth method to trust a Stackweaver issuer (JWKS served from a live signing key), and proves
// our runner's vaultLogin exchanges a minted JWT for a genuine Vault client token - no mock, no cloud
// account, no manual staging gap.
//
// Gated behind VAULT_E2E=1 (needs Docker + Linux host networking), so normal `go test` / CI skip it.
// Run locally with:  VAULT_E2E=1 go test ./cmd/runner/ -run TestVaultLogin_RealVaultContainer -v
func TestVaultLogin_RealVaultContainer(t *testing.T) {
	if testing.Short() || os.Getenv("VAULT_E2E") != "1" {
		t.Skip("set VAULT_E2E=1 (Docker + Linux host networking required) to run the real-Vault test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}

	t.Setenv("DEV_INSECURE_KEY", "1") // AUD-121: NewSigningKey fails closed without a key/dev flag
	signingKey, err := oidc.NewSigningKey()
	if err != nil {
		t.Fatalf("NewSigningKey: %v", err)
	}
	ts := oidc.NewTokenService(signingKey, "https://stackweaver.example.com")

	// Serve the JWKS the Vault container will fetch to verify our tokens. --network host lets the
	// container reach this 127.0.0.1 listener.
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(signingKey.JWKS())
	}))
	defer jwks.Close()

	const (
		name      = "sw-vault-oidc-e2e"
		vaultAddr = "http://127.0.0.1:8200"
		rootToken = "root"
	)
	ctx := context.Background()
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", name).Run() // clean any stale container
	run := exec.CommandContext(ctx, "docker", "run", "-d", "--rm", "--network", "host", "--cap-add=IPC_LOCK",
		"-e", "VAULT_DEV_ROOT_TOKEN_ID="+rootToken,
		"-e", "VAULT_DEV_LISTEN_ADDRESS=127.0.0.1:8200",
		"--name", name, "hashicorp/vault:latest")
	if out, err := run.CombinedOutput(); err != nil {
		t.Skipf("could not start vault container (docker unavailable?): %v: %s", err, out)
	}
	defer func() { _ = exec.CommandContext(context.Background(), "docker", "rm", "-f", name).Run() }()

	// Wait for Vault to be ready.
	ready := false
	for i := 0; i < 40; i++ {
		resp, herr := http.Get(vaultAddr + "/v1/sys/health") //nolint:noctx,gosec // local test poll
		if herr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !ready {
		t.Fatal("vault did not become ready")
	}

	subject := "organization:dev-test:project:default:workspace:production:run_phase:plan"
	vaultDo := func(method, path string, body any) {
		var buf bytes.Buffer
		if body != nil {
			if encErr := json.NewEncoder(&buf).Encode(body); encErr != nil {
				t.Fatalf("encode vault request body: %v", encErr)
			}
		}
		req, _ := http.NewRequestWithContext(context.Background(), method, vaultAddr+path, &buf)
		req.Header.Set("X-Vault-Token", rootToken)
		resp, derr := http.DefaultClient.Do(req)
		if derr != nil {
			t.Fatalf("vault %s %s: %v", method, path, derr)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode >= 300 {
			t.Fatalf("vault %s %s -> HTTP %d", method, path, resp.StatusCode)
		}
	}

	// Enable + configure JWT auth trusting our JWKS, and a role bound to our audience + subject.
	vaultDo(http.MethodPost, "/v1/sys/auth/jwt", map[string]string{"type": "jwt"})
	vaultDo(http.MethodPost, "/v1/auth/jwt/config", map[string]string{"jwks_url": jwks.URL})
	vaultDo(http.MethodPost, "/v1/auth/jwt/role/stackweaver", map[string]any{
		"role_type":       "jwt",
		"bound_audiences": []string{oidc.VaultWorkloadIdentityAudience},
		"user_claim":      "sub",
		"bound_subject":   subject,
		"token_policies":  []string{"default"},
		"token_ttl":       "5m",
	})

	// Mint the token the runner would mint, and exercise the real login path.
	token, err := ts.GenerateToken(oidc.TokenRequest{Audience: oidc.VaultWorkloadIdentityAudience, OrganizationName: "dev-test", ProjectName: "default", WorkspaceName: "production", RunID: "run-e2e", RunPhase: "plan"})
	if err != nil {
		t.Fatalf("GenerateToken: %v", err)
	}
	clientToken, err := vaultLogin(context.Background(), vaultAddr, "jwt", "stackweaver", "", nil, token)
	if err != nil {
		t.Fatalf("vaultLogin against real Vault failed: %v", err)
	}
	if !strings.HasPrefix(clientToken, "hvs.") && !strings.HasPrefix(clientToken, "s.") {
		t.Fatalf("unexpected Vault client token format: %q", clientToken)
	}
	t.Logf("real Vault login succeeded, client token prefix ok (%.6s...)", clientToken)
}
