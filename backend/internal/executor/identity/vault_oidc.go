// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package identity

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Vault OIDC (JWT auth) runtime wiring. Unlike the cloud providers - where we mint a token and let the
// provider perform the exchange - Vault requires an active login: the runner exchanges the signed JWT
// for a Vault client token via the JWT auth method, then exports VAULT_ADDR + VAULT_TOKEN so the run's
// `vault` provider works with no auth block (matching Terraform Cloud's Vault dynamic credentials).
const (
	// vaultCACertFile is the filename the runner writes a custom Vault CA cert to (0600) inside the run
	// working dir; VAULT_CACERT points at it.
	vaultCACertFile = ".stackweaver-vault-ca.pem"
	// defaultVaultAuthPath is the default JWT auth mount path when the config leaves auth_path empty.
	defaultVaultAuthPath = "jwt"
)

// decodeVaultCACert accepts the config's encoded-cacert value, which may be either a raw PEM string or
// a base64-encoded PEM blob, and returns the PEM bytes (nil when empty).
func decodeVaultCACert(encoded string) []byte {
	if encoded == "" {
		return nil
	}
	if strings.Contains(encoded, "-----BEGIN") {
		return []byte(encoded)
	}
	if decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded)); err == nil && bytes.Contains(decoded, []byte("-----BEGIN")) {
		return decoded
	}
	return []byte(encoded)
}

// vaultLoginResponse is the subset of Vault's auth login response we consume.
type vaultLoginResponse struct {
	Auth struct {
		ClientToken string `json:"client_token"`
	} `json:"auth"`
}

// vaultLogin exchanges a signed OIDC JWT for a Vault client token via the JWT auth method:
// POST {address}/v1/auth/{authPath}/login  {"role": role, "jwt": token}. It honours the optional
// namespace (X-Vault-Namespace header) and a custom CA cert for Vault's TLS.
func vaultLogin(ctx context.Context, address, authPath, role, namespace string, caPEM []byte, token string) (string, error) {
	if address == "" {
		return "", fmt.Errorf("vault oidc: address is empty")
	}
	if role == "" {
		return "", fmt.Errorf("vault oidc: role is empty")
	}
	if token == "" {
		return "", fmt.Errorf("vault oidc: token is empty")
	}
	if authPath == "" {
		authPath = defaultVaultAuthPath
	}

	transport := &http.Transport{}
	if len(caPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return "", fmt.Errorf("vault oidc: failed to parse encoded-cacert")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: transport}

	body, err := json.Marshal(map[string]string{"role": role, "jwt": token})
	if err != nil {
		return "", fmt.Errorf("vault oidc: marshal login body: %w", err)
	}
	url := fmt.Sprintf("%s/v1/auth/%s/login", strings.TrimRight(address, "/"), authPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("vault oidc: build login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if namespace != "" {
		req.Header.Set("X-Vault-Namespace", namespace)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("vault oidc: login request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("vault oidc: login rejected (HTTP %d)", resp.StatusCode)
	}
	var parsed vaultLoginResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("vault oidc: decode login response: %w", err)
	}
	if parsed.Auth.ClientToken == "" {
		return "", fmt.Errorf("vault oidc: login response contained no client token")
	}
	return parsed.Auth.ClientToken, nil
}

// vaultRunEnv assembles the environment a Terraform run needs to talk to Vault after a successful
// login. It writes the CA cert (if any) to a 0600 file in dir and points VAULT_CACERT at it.
func vaultRunEnv(address, namespace, authPath, clientToken, role string, caPEM []byte, dir string) (map[string]string, error) {
	env := map[string]string{
		"VAULT_ADDR":              address,
		"VAULT_TOKEN":             clientToken,
		"TFC_VAULT_PROVIDER_AUTH": "true",
		"TFC_VAULT_ADDR":          address,
		"TFC_VAULT_RUN_ROLE":      role,
	}
	if authPath == "" {
		authPath = defaultVaultAuthPath
	}
	env["TFC_VAULT_AUTH_PATH"] = authPath
	if namespace != "" {
		env["VAULT_NAMESPACE"] = namespace
		env["TFC_VAULT_NAMESPACE"] = namespace
	}
	if len(caPEM) > 0 {
		caPath := filepath.Join(dir, vaultCACertFile)
		//nolint:gosec // G306: a CA certificate is public material; 0600 is already conservative
		if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
			return nil, fmt.Errorf("vault oidc: write CA cert file: %w", err)
		}
		env["VAULT_CACERT"] = caPath
	}
	return env, nil
}

// BuildVaultOIDCEnv is the platform-hosted-runner path: perform the Vault JWT login with the minted
// token and return the run environment (VAULT_ADDR + VAULT_TOKEN + metadata).
func BuildVaultOIDCEnv(ctx context.Context, address, role, namespace, authPath, encodedCACert, token, dir string) (map[string]string, error) {
	caPEM := decodeVaultCACert(encodedCACert)
	clientToken, err := vaultLogin(ctx, address, authPath, role, namespace, caPEM, token)
	if err != nil {
		return nil, err
	}
	return vaultRunEnv(address, namespace, authPath, clientToken, role, caPEM, dir)
}

// MaterializeVaultToken is the self-hosted-agent counterpart. The API handler injects the raw token +
// Vault config attrs (VAULT_OIDC_*) because only the agent's host can reach the customer's Vault. Here
// the agent performs the login, sets VAULT_ADDR + VAULT_TOKEN (+ namespace/CA/metadata), and drops the
// raw VAULT_OIDC_* keys. No-op when the raw token is absent.
func MaterializeVaultToken(ctx context.Context, env map[string]string, dir string) error {
	token := env["VAULT_OIDC_RAW_TOKEN"]
	if token == "" {
		return nil
	}
	address := env["VAULT_OIDC_ADDRESS"]
	role := env["VAULT_OIDC_ROLE"]
	namespace := env["VAULT_OIDC_NAMESPACE"]
	authPath := env["VAULT_OIDC_AUTH_PATH"]
	encodedCACert := env["VAULT_OIDC_ENCODED_CACERT"]

	runEnv, err := BuildVaultOIDCEnv(ctx, address, role, namespace, authPath, encodedCACert, token, dir)
	if err != nil {
		return err
	}
	for k, v := range runEnv {
		env[k] = v
	}
	delete(env, "VAULT_OIDC_RAW_TOKEN")
	delete(env, "VAULT_OIDC_ADDRESS")
	delete(env, "VAULT_OIDC_ROLE")
	delete(env, "VAULT_OIDC_NAMESPACE")
	delete(env, "VAULT_OIDC_AUTH_PATH")
	delete(env, "VAULT_OIDC_ENCODED_CACERT")
	return nil
}
