// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package vcs

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
)

// generateTestPEM creates a real RSA PEM key for testing.
func generateTestPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return string(pemBytes)
}

func TestNormalizePEM(t *testing.T) {
	validPEM := generateTestPEM(t)
	pemHeader := "-----BEGIN RSA PRIVATE KEY-----" //nolint:gosec // test constant, not a credential

	tests := []struct {
		name    string
		input   string
		wantOK  bool
		wantHdr string
	}{
		{
			name:    "already formatted PEM passes through",
			input:   validPEM,
			wantOK:  true,
			wantHdr: pemHeader,
		},
		{
			name:    "single-line PEM with spaces instead of newlines",
			input:   strings.ReplaceAll(validPEM, "\n", " "),
			wantOK:  true,
			wantHdr: pemHeader,
		},
		{
			name:    "completely mangled non-PEM content",
			input:   "this is not a PEM at all",
			wantOK:  false,
			wantHdr: "",
		},
		{
			name:    "empty input",
			input:   "",
			wantOK:  false,
			wantHdr: "",
		},
		{
			name:    "PEM with escaped newlines (env var style)",
			input:   strings.ReplaceAll(validPEM, "\n", "\\n"),
			wantOK:  true,
			wantHdr: pemHeader,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := normalizePEM(tt.input)
			if tt.wantOK && result == "" {
				t.Fatalf("normalizePEM() returned empty string, expected valid PEM")
			}
			if !tt.wantOK && result != "" {
				t.Fatalf("normalizePEM() returned non-empty string for invalid input")
			}
			if tt.wantOK {
				lines := strings.Split(result, "\n")
				if lines[0] != tt.wantHdr {
					t.Errorf("first line = %q, want %q", lines[0], tt.wantHdr)
				}
				// Verify all body lines are ≤ 64 characters
				for i, line := range lines[1 : len(lines)-2] {
					if len(line) > 64 {
						t.Errorf("line %d has %d chars (max 64): %q", i+2, len(line), line)
					}
				}
			}
		})
	}
}

func TestLoadPrivateKeyFromString_NormalizesSpaceSeparatedPEM(t *testing.T) {
	validPEM := generateTestPEM(t)

	// Replace newlines with spaces to simulate 1Password/ExternalSecrets corruption
	singleLine := strings.ReplaceAll(validPEM, "\n", " ")

	key, err := loadPrivateKeyFromString(singleLine)
	if err != nil {
		t.Fatalf("loadPrivateKeyFromString() with space-separated PEM: %v", err)
	}
	if key == nil {
		t.Fatal("expected non-nil key")
	}
}

func TestLoadPrivateKeyFromString_EscapedNewlines(t *testing.T) {
	validPEM := generateTestPEM(t)

	// Replace newlines with literal \n to simulate env var encoding
	escaped := strings.ReplaceAll(validPEM, "\n", "\\n")

	key, err := loadPrivateKeyFromString(escaped)
	if err != nil {
		t.Fatalf("loadPrivateKeyFromString() with escaped newlines: %v", err)
	}
	if key == nil {
		t.Fatal("expected non-nil key")
	}
}

func TestLoadPrivateKeyFromString_ValidPEM(t *testing.T) {
	validPEM := generateTestPEM(t)

	key, err := loadPrivateKeyFromString(validPEM)
	if err != nil {
		t.Fatalf("loadPrivateKeyFromString() with valid PEM: %v", err)
	}
	if key == nil {
		t.Fatal("expected non-nil key")
	}
}

func TestLoadPrivateKeyFromString_InvalidContent(t *testing.T) {
	_, err := loadPrivateKeyFromString("not a pem at all")
	if err == nil {
		t.Fatal("expected error for invalid content")
	}
	if !strings.Contains(err.Error(), "failed to decode PEM block") {
		t.Errorf("unexpected error: %v", err)
	}
}
