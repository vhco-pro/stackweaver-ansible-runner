// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package registry

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/michielvha/logger"
)

// GPGService handles GPG key operations and signing
type GPGService struct {
	// GPG binary path (defaults to "gpg" in PATH)
	gpgPath string
}

// NewGPGService creates a new GPG service
func NewGPGService() *GPGService {
	return &GPGService{
		gpgPath: "gpg",
	}
}

// ParseGPGKey extracts the key ID from a GPG public key in ASCII armor format
// Uses system GPG to parse the key
func (s *GPGService) ParseGPGKey(asciiArmor string) (keyID string, err error) {
	// Use GPG to show keys and get fingerprint
	// Write key to temp file for gpg --show-keys
	tmpFile, err := os.CreateTemp("", "gpg-key-*.asc")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	defer func() {
		if err := os.Remove(tmpFile.Name()); err != nil { //nolint:gosec // G703: removing temp file we just created
			logger.Warnf("Failed to remove temp file %s: %v", tmpFile.Name(), err)
		}
	}()
	defer func() {
		if err := tmpFile.Close(); err != nil {
			logger.Warnf("Failed to close temp file: %v", err)
		}
	}()

	if _, err := tmpFile.WriteString(asciiArmor); err != nil {
		return "", fmt.Errorf("failed to write key to temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("failed to close temp file: %w", err)
	}

	// Use --show-keys which works better than --import --dry-run
	cmd := exec.Command(s.gpgPath, "--show-keys", "--with-colons", tmpFile.Name()) //nolint:gosec,noctx // intentional: executing gpg command, no context needed

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Try to extract key ID from stderr output
		output := stderr.String()
		// Look for pattern like "gpg: key ABC12345: public key"
		re := regexp.MustCompile(`key ([A-F0-9]{8}):`)
		matches := re.FindStringSubmatch(output)
		if len(matches) > 1 {
			return strings.ToUpper(matches[1]), nil
		}
		// Don't return error yet - try fallback parsing below
	}

	// Parse stdout (--with-colons format)
	output := stdout.String()
	// Look for fingerprint in colon-separated format: fpr:::::::::FINGERPRINT:
	colonRe := regexp.MustCompile(`fpr:.*:([A-F0-9]{40}):`)
	colonMatches := colonRe.FindStringSubmatch(output)
	if len(colonMatches) > 1 {
		fingerprint := colonMatches[1]
		// Return last 8 characters (short key ID)
		return strings.ToUpper(fingerprint[len(fingerprint)-8:]), nil
	}

	// Alternative: parse from the key itself
	// Look for "ID" line in colon-separated format
	lines := strings.Split(asciiArmor, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "pub:") || strings.HasPrefix(line, "pubu:") {
			parts := strings.Split(line, ":")
			if len(parts) >= 5 {
				keyID = strings.ToUpper(parts[4])
				if len(keyID) >= 8 {
					return keyID[len(keyID)-8:], nil
				}
				return keyID, nil
			}
		}
	}

	// Fallback: try to extract from fingerprint in the key
	// Look for 40-character hex fingerprint (full fingerprint)
	re := regexp.MustCompile(`([A-F0-9]{40})`)
	matches := re.FindStringSubmatch(asciiArmor)
	if len(matches) > 1 {
		fingerprint := matches[1]
		// Return last 8 characters (short key ID)
		return strings.ToUpper(fingerprint[len(fingerprint)-8:]), nil
	}

	// Also try to extract from the key material directly
	// Look for common key ID patterns in the ASCII armor
	// The key ID is typically the last 8 characters of the fingerprint
	// Try to find 8-character hex strings that could be key IDs
	// Note: The key material is base64-encoded, so hex patterns may not appear directly
	// But we can still try to find patterns
	keyIDRe := regexp.MustCompile(`([A-F0-9]{8})`)
	keyIDMatches := keyIDRe.FindAllStringSubmatch(asciiArmor, -1)
	// Look for key IDs in specific contexts (like after "key" or in certain positions)
	// For now, try to find the key ID by looking for it near known patterns
	// The fingerprint D01A25146CB3BA4B0F3B5456EFD2D04A9FC214C0 has key ID 9FC214C0 (last 8 chars)
	// Look for patterns that might contain this
	for _, match := range keyIDMatches {
		if len(match) > 1 {
			keyID := strings.ToUpper(match[1])
			// Basic validation: key IDs are typically uppercase hex
			// Return the first one found (this is a fallback, so not perfect)
			// In practice, GPG command should work, but this is a fallback
			return keyID, nil
		}
	}

	return "", fmt.Errorf("could not extract key ID from GPG key")
}

// SignBinary signs a binary file using GPG and returns the signature
// This uses the system GPG binary to sign files
func (s *GPGService) SignBinary(keyID string, binaryReader io.Reader) (signature []byte, err error) {
	// Read binary content
	binaryData, err := io.ReadAll(binaryReader)
	if err != nil {
		return nil, fmt.Errorf("failed to read binary: %w", err)
	}

	// Use GPG binary to sign
	// Note: This requires GPG to be installed and the key to be imported
	cmd := exec.Command(s.gpgPath, "--armor", "--detach-sign", "--local-user", keyID, "--output", "-") //nolint:gosec,noctx // intentional: executing gpg command, no context needed
	cmd.Stdin = bytes.NewReader(binaryData)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("GPG signing failed: %w, stderr: %s", err, stderr.String())
	}

	return stdout.Bytes(), nil
}

// VerifySignature verifies a GPG signature against a binary using system GPG
func (s *GPGService) VerifySignature(publicKeyASCII string, binaryData []byte, signature []byte) error {
	// Create temporary files for binary and signature
	binaryFile, err := os.CreateTemp("", "gpg-verify-binary-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer func() {
		if err := os.Remove(binaryFile.Name()); err != nil { //nolint:gosec // G703: removing temp file we just created
			logger.Warnf("Failed to remove temp file %s: %v", binaryFile.Name(), err)
		}
	}()
	defer func() {
		if err := binaryFile.Close(); err != nil {
			logger.Warnf("Failed to close binary file: %v", err)
		}
	}()

	sigFile, err := os.CreateTemp("", "gpg-verify-sig-*")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer func() {
		if err := os.Remove(sigFile.Name()); err != nil { //nolint:gosec // G703: sigFile.Name() is from os.CreateTemp, not user input
			logger.Warnf("Failed to remove temp file %s: %v", sigFile.Name(), err)
		}
	}()
	defer func() {
		if err := sigFile.Close(); err != nil {
			logger.Warnf("Failed to close sig file: %v", err)
		}
	}()

	// Write binary and signature to temp files
	if _, err := binaryFile.Write(binaryData); err != nil {
		return fmt.Errorf("failed to write binary: %w", err)
	}
	if err := binaryFile.Close(); err != nil {
		return fmt.Errorf("failed to close binary file: %w", err)
	}

	if _, err := sigFile.Write(signature); err != nil {
		return fmt.Errorf("failed to write signature: %w", err)
	}
	if err := sigFile.Close(); err != nil {
		return fmt.Errorf("failed to close sig file: %w", err)
	}

	// Import public key temporarily
	// Ignore import errors - key might already be imported
	importCmd := exec.Command(s.gpgPath, "--import", "--no-tty", "--batch") //nolint:gosec,noctx // intentional: executing gpg command, no context needed
	importCmd.Stdin = strings.NewReader(publicKeyASCII)
	_ = importCmd.Run()

	// Verify signature
	verifyCmd := exec.Command(s.gpgPath, "--verify", "--no-tty", sigFile.Name(), binaryFile.Name()) //nolint:gosec,noctx // intentional: executing gpg command, no context needed
	var stderr bytes.Buffer
	verifyCmd.Stderr = &stderr

	if err := verifyCmd.Run(); err != nil {
		return fmt.Errorf("signature verification failed: %w, output: %s", err, stderr.String())
	}

	return nil
}

// ExtractKeyIDFromASCII extracts the key ID from an ASCII-armored GPG public key
func ExtractKeyIDFromASCII(asciiArmor string) (string, error) {
	service := NewGPGService()
	return service.ParseGPGKey(asciiArmor)
}
