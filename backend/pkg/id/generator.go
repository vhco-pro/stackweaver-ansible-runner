// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package id

import (
	"crypto/rand"
	"fmt"
)

const (
	// IDLength is the length of the random part (16 characters, matching TFE)
	IDLength = 16
	// Alphanumeric character set: A-Z, a-z, 0-9 (62 characters total)
	alphanumericChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
)

// Generate generates a 16-character alphanumeric ID with prefix
// Format: {prefix}-{16-char-random}
// Uses only alphanumeric characters (A-Z, a-z, 0-9) to match TFE's ID format
// TFE workspace IDs must be alphanumeric only (no underscores or hyphens in the random part)
func Generate(prefix string) (string, error) {
	// Generate 16 random alphanumeric characters
	// We need enough entropy: log2(62^16) ≈ 95.3 bits
	// Generate 12 random bytes (96 bits) and map to alphanumeric characters
	bytes := make([]byte, 12)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}

	// Map bytes to alphanumeric characters
	// Each byte (0-255) is mapped to one of 62 characters using modulo
	result := make([]byte, IDLength)
	alphanumericLen := len(alphanumericChars)
	for i := 0; i < IDLength; i++ {
		// Use byte at position (i % 12) to cycle through our 12 random bytes
		// This gives us 16 characters from 12 bytes
		byteIndex := i % 12
		// Security: Bounds check to prevent slice index out of range
		if byteIndex >= len(bytes) {
			return "", fmt.Errorf("internal error: byte index out of range")
		}
		charIndex := int(bytes[byteIndex]) % alphanumericLen
		if charIndex < 0 || charIndex >= alphanumericLen {
			return "", fmt.Errorf("internal error: character index out of range")
		}
		result[i] = alphanumericChars[charIndex] //nolint:gosec // charIndex is validated above
	}

	// Return with prefix: e.g., "ws-abc123...", "run-xyz789..."
	return fmt.Sprintf("%s-%s", prefix, string(result)), nil
}

// GenerateWorkspaceID generates a workspace ID with "ws-" prefix
func GenerateWorkspaceID() (string, error) {
	return Generate("ws")
}

// GenerateRunID generates a run ID with "run-" prefix
func GenerateRunID() (string, error) {
	return Generate("run")
}

// GenerateStateVersionID generates a state version ID with "sv-" prefix
func GenerateStateVersionID() (string, error) {
	return Generate("sv")
}

// GenerateVariableID generates a variable ID with "var-" prefix
func GenerateVariableID() (string, error) {
	return Generate("var")
}

// GenerateVariableSetID generates a variable set ID with "varset-" prefix
func GenerateVariableSetID() (string, error) {
	return Generate("varset")
}

// GenerateConfigurationVersionID generates a configuration version ID with "cv-" prefix
func GenerateConfigurationVersionID() (string, error) {
	return Generate("cv")
}

// GenerateAzureOIDCConfigID generates an Azure OIDC configuration ID with "azoidc-" prefix
func GenerateAzureOIDCConfigID() (string, error) {
	return Generate("azoidc")
}
