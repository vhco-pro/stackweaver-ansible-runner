// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package identity

import (
	"fmt"
	"os"
	"path/filepath"
)

// awsWebIdentityTokenFile is the filename the runner writes the OIDC token to inside the run's
// working directory. The AWS SDK / terraform-provider-aws read the token from a file path
// (AWS_WEB_IDENTITY_TOKEN_FILE), unlike Azure which accepts the token value directly in an env var.
const awsWebIdentityTokenFile = ".stackweaver-aws-oidc-token"

// BuildAWSOIDCEnv materializes an AWS keyless-auth environment for a Terraform run: it writes the
// signed workload-identity token to a 0600 file inside dir and returns the env vars the aws provider
// needs to perform AssumeRoleWithWebIdentity. This mirrors how Terraform Cloud injects AWS dynamic
// credentials (AWS_ROLE_ARN + AWS_WEB_IDENTITY_TOKEN_FILE).
func BuildAWSOIDCEnv(roleARN, token, dir, sessionName string) (map[string]string, error) {
	if roleARN == "" {
		return nil, fmt.Errorf("aws oidc: role ARN is empty")
	}
	if token == "" {
		return nil, fmt.Errorf("aws oidc: token is empty")
	}
	tokenPath := filepath.Join(dir, awsWebIdentityTokenFile)
	//nolint:gosec // G703: tokenPath is <run working dir>/<constant filename>; dir is runner-controlled
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		return nil, fmt.Errorf("aws oidc: failed to write web identity token file: %w", err)
	}
	return map[string]string{
		"AWS_ROLE_ARN":                roleARN,
		"AWS_WEB_IDENTITY_TOKEN_FILE": tokenPath,
		"AWS_ROLE_SESSION_NAME":       sessionName,
	}, nil
}

// MaterializeAWSWebIdentityToken is the self-hosted-agent counterpart to BuildAWSOIDCEnv. The API
// handler that serves work to external agents cannot write files on the agent's host, so it passes
// the raw token as AWS_WEB_IDENTITY_TOKEN in the job env. Here the agent writes it to a 0600 file in
// the run working directory, points AWS_WEB_IDENTITY_TOKEN_FILE at it (what terraform-provider-aws
// reads), and drops the raw token from the env. No-op when the key is absent.
func MaterializeAWSWebIdentityToken(env map[string]string, dir string) error {
	token := env["AWS_WEB_IDENTITY_TOKEN"]
	if token == "" {
		return nil
	}
	tokenPath := filepath.Join(dir, awsWebIdentityTokenFile)
	//nolint:gosec // G703: tokenPath is <run working dir>/<constant filename>; dir is runner-controlled
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		return fmt.Errorf("aws oidc: failed to write web identity token file: %w", err)
	}
	env["AWS_WEB_IDENTITY_TOKEN_FILE"] = tokenPath
	delete(env, "AWS_WEB_IDENTITY_TOKEN")
	return nil
}
