// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package identity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuildAWSOIDCEnv_WritesTokenFileAndEnv(t *testing.T) {
	dir := t.TempDir()
	token := "header.payload.signature"
	roleARN := "arn:aws:iam::123456789012:role/stackweaver-oidc"

	env, err := BuildAWSOIDCEnv(roleARN, token, dir, "stackweaver-run-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if env["AWS_ROLE_ARN"] != roleARN {
		t.Errorf("AWS_ROLE_ARN = %q, want %q", env["AWS_ROLE_ARN"], roleARN)
	}
	if env["AWS_ROLE_SESSION_NAME"] != "stackweaver-run-abc" {
		t.Errorf("AWS_ROLE_SESSION_NAME = %q", env["AWS_ROLE_SESSION_NAME"])
	}

	wantPath := filepath.Join(dir, awsWebIdentityTokenFile)
	if env["AWS_WEB_IDENTITY_TOKEN_FILE"] != wantPath {
		t.Errorf("AWS_WEB_IDENTITY_TOKEN_FILE = %q, want %q", env["AWS_WEB_IDENTITY_TOKEN_FILE"], wantPath)
	}

	// The token file must exist, contain exactly the token, and be private (0600).
	data, readErr := os.ReadFile(wantPath) //nolint:gosec // test-controlled path
	if readErr != nil {
		t.Fatalf("token file not written: %v", readErr)
	}
	if string(data) != token {
		t.Errorf("token file contents = %q, want %q", string(data), token)
	}
	info, statErr := os.Stat(wantPath)
	if statErr != nil {
		t.Fatalf("stat token file: %v", statErr)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file perm = %o, want 600", perm)
	}
}

func TestBuildAWSOIDCEnv_RejectsEmptyInputs(t *testing.T) {
	dir := t.TempDir()
	if _, err := BuildAWSOIDCEnv("", "tok", dir, "s"); err == nil {
		t.Error("expected error for empty role ARN")
	}
	if _, err := BuildAWSOIDCEnv("arn:aws:iam::1:role/r", "", dir, "s"); err == nil {
		t.Error("expected error for empty token")
	}
}

func TestMaterializeAWSWebIdentityToken(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{
		"AWS_ROLE_ARN":           "arn:aws:iam::123456789012:role/r",
		"AWS_WEB_IDENTITY_TOKEN": "header.payload.sig",
	}
	if err := MaterializeAWSWebIdentityToken(env, dir); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := env["AWS_WEB_IDENTITY_TOKEN"]; ok {
		t.Error("raw AWS_WEB_IDENTITY_TOKEN should be removed from env after materialization")
	}
	tokenPath := env["AWS_WEB_IDENTITY_TOKEN_FILE"]
	if tokenPath != filepath.Join(dir, awsWebIdentityTokenFile) {
		t.Fatalf("AWS_WEB_IDENTITY_TOKEN_FILE = %q", tokenPath)
	}
	data, err := os.ReadFile(tokenPath) //nolint:gosec // test-controlled path
	if err != nil || string(data) != "header.payload.sig" {
		t.Fatalf("token file contents = %q (err=%v)", string(data), err)
	}
}

func TestMaterializeAWSWebIdentityToken_NoopWhenAbsent(t *testing.T) {
	env := map[string]string{"FOO": "bar"}
	if err := MaterializeAWSWebIdentityToken(env, t.TempDir()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := env["AWS_WEB_IDENTITY_TOKEN_FILE"]; ok {
		t.Error("should not set AWS_WEB_IDENTITY_TOKEN_FILE when no token present")
	}
}
