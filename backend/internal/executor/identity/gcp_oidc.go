// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package identity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// GCP Workload Identity Federation runtime wiring. Unlike Azure (env-value token) and AWS (bare token
// file), the google provider authenticates via an external-account *credential-config JSON* pointed at
// by GOOGLE_APPLICATION_CREDENTIALS; that JSON references a token file plus the STS token-exchange and
// service-account-impersonation URLs. So a GCP run needs two files written into the run working dir.
const (
	gcpOIDCTokenFile      = ".stackweaver-gcp-oidc-token"            //nolint:gosec // G101: run-workdir filename, not a credential
	gcpOIDCCredConfigFile = ".stackweaver-gcp-oidc-credentials.json" //nolint:gosec // G101: run-workdir filename, not a credential
)

// gcpExternalAccountConfig is the external_account credential-config JSON the google provider /
// google-cloud SDKs read (via GOOGLE_APPLICATION_CREDENTIALS) to perform Workload Identity Federation.
type gcpExternalAccountConfig struct {
	Type                           string                     `json:"type"`
	Audience                       string                     `json:"audience"`
	SubjectTokenType               string                     `json:"subject_token_type"`
	TokenURL                       string                     `json:"token_url"`
	ServiceAccountImpersonationURL string                     `json:"service_account_impersonation_url"`
	CredentialSource               gcpCredentialSourceFileCfg `json:"credential_source"`
}

type gcpCredentialSourceFileCfg struct {
	File   string                 `json:"file"`
	Format gcpCredentialSourceFmt `json:"format"`
}

type gcpCredentialSourceFmt struct {
	Type string `json:"type"`
}

// writeGCPCredentialFiles writes the OIDC token file and the external-account credential-config JSON
// into dir (both 0600) and returns the path to the credential-config JSON (what
// GOOGLE_APPLICATION_CREDENTIALS points at). Shared by the platform-hosted runner and the self-hosted
// agent so both produce byte-identical credential files.
func writeGCPCredentialFiles(dir, token, serviceAccountEmail, workloadProviderName string) (string, error) {
	if token == "" {
		return "", fmt.Errorf("gcp oidc: token is empty")
	}
	if serviceAccountEmail == "" {
		return "", fmt.Errorf("gcp oidc: service account email is empty")
	}
	if workloadProviderName == "" {
		return "", fmt.Errorf("gcp oidc: workload provider name is empty")
	}

	tokenPath := filepath.Join(dir, gcpOIDCTokenFile)
	//nolint:gosec // G703: tokenPath is <run working dir>/<constant filename>; dir is runner-controlled
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		return "", fmt.Errorf("gcp oidc: failed to write web identity token file: %w", err)
	}

	cfg := gcpExternalAccountConfig{ //nolint:gosec // G101: OIDC/STS endpoint URLs + token-type URNs, not hardcoded credentials
		Type:                           "external_account",
		Audience:                       "//iam.googleapis.com/" + workloadProviderName,
		SubjectTokenType:               "urn:ietf:params:oauth:token-type:jwt",
		TokenURL:                       "https://sts.googleapis.com/v1/token",
		ServiceAccountImpersonationURL: fmt.Sprintf("https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/%s:generateAccessToken", serviceAccountEmail),
		CredentialSource: gcpCredentialSourceFileCfg{
			File:   tokenPath,
			Format: gcpCredentialSourceFmt{Type: "text"},
		},
	}
	credJSON, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", fmt.Errorf("gcp oidc: failed to marshal credential config: %w", err)
	}
	credPath := filepath.Join(dir, gcpOIDCCredConfigFile)
	//nolint:gosec // G703: credPath is <run working dir>/<constant filename>; dir is runner-controlled
	if err := os.WriteFile(credPath, credJSON, 0o600); err != nil {
		return "", fmt.Errorf("gcp oidc: failed to write credential config file: %w", err)
	}
	return credPath, nil
}

// BuildGCPOIDCEnv materializes a GCP keyless-auth environment for a Terraform run: it writes the token
// + credential-config files into dir and returns the env vars the google provider needs to perform
// Workload Identity Federation. Mirrors how Terraform Cloud injects GCP dynamic credentials
// (GOOGLE_APPLICATION_CREDENTIALS + TFC_GCP_* metadata).
func BuildGCPOIDCEnv(serviceAccountEmail, projectNumber, workloadProviderName, token, dir string) (map[string]string, error) {
	credPath, err := writeGCPCredentialFiles(dir, token, serviceAccountEmail, workloadProviderName)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"GOOGLE_APPLICATION_CREDENTIALS":    credPath,
		"TFC_GCP_PROVIDER_AUTH":             "true",
		"TFC_GCP_RUN_SERVICE_ACCOUNT_EMAIL": serviceAccountEmail,
		"TFC_GCP_WORKLOAD_PROVIDER_NAME":    workloadProviderName,
		"TFC_GCP_PROJECT_NUMBER":            projectNumber,
	}, nil
}

// MaterializeGCPWorkloadIdentity is the self-hosted-agent counterpart to BuildGCPOIDCEnv. The API
// handler that serves work to external agents cannot write files on the agent's host, so it passes the
// raw token + config attrs (GCP_OIDC_*). Here the agent writes the token + credential-config files to
// dir, sets GOOGLE_APPLICATION_CREDENTIALS + the TFC_GCP_* metadata, and drops the raw GCP_OIDC_* keys.
// No-op when the raw token is absent.
func MaterializeGCPWorkloadIdentity(env map[string]string, dir string) error {
	token := env["GCP_OIDC_RAW_TOKEN"]
	if token == "" {
		return nil
	}
	serviceAccountEmail := env["GCP_OIDC_SERVICE_ACCOUNT_EMAIL"]
	workloadProviderName := env["GCP_OIDC_WORKLOAD_PROVIDER_NAME"]
	projectNumber := env["GCP_OIDC_PROJECT_NUMBER"]

	credPath, err := writeGCPCredentialFiles(dir, token, serviceAccountEmail, workloadProviderName)
	if err != nil {
		return err
	}
	env["GOOGLE_APPLICATION_CREDENTIALS"] = credPath
	env["TFC_GCP_PROVIDER_AUTH"] = "true"
	env["TFC_GCP_RUN_SERVICE_ACCOUNT_EMAIL"] = serviceAccountEmail
	env["TFC_GCP_WORKLOAD_PROVIDER_NAME"] = workloadProviderName
	env["TFC_GCP_PROJECT_NUMBER"] = projectNumber

	delete(env, "GCP_OIDC_RAW_TOKEN")
	delete(env, "GCP_OIDC_SERVICE_ACCOUNT_EMAIL")
	delete(env, "GCP_OIDC_WORKLOAD_PROVIDER_NAME")
	delete(env, "GCP_OIDC_PROJECT_NUMBER")
	return nil
}
