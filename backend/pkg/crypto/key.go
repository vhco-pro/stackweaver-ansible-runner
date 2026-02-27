// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

func GenerateEncryptionKey() ([]byte, error) {
	key := make([]byte, 32) // AES-256
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("failed to generate key: %w", err)
	}
	return key, nil
}

func GenerateEncryptionKeyBase64() (string, error) {
	key, err := GenerateEncryptionKey()
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}
