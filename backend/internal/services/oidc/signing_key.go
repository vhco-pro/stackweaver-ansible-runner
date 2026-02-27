// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"strings"
	"sync"

	"github.com/michielvha/logger"
)

// SigningKey manages the RSA key pair used to sign OIDC workload identity tokens.
// The private key signs JWTs; the public key is exposed via the JWKS endpoint
// so Azure AD (or other IdPs) can verify them.
type SigningKey struct {
	privateKey *rsa.PrivateKey
	kid        string // Key ID (thumbprint of the public key)
	mu         sync.RWMutex
}

// NewSigningKey initializes the signing key from the OIDC_SIGNING_KEY env var.
// The value can be either:
//   - Raw PEM (multi-line, works in docker-compose environment: but NOT in env_file:)
//   - Base64-encoded PEM (single-line, works everywhere including docker env_file)
//
// If the env var is not set, it auto-generates an RSA-2048 key pair (dev mode).
// WARNING: Auto-generated keys differ between containers (API vs runner), causing
// Azure OIDC validation failures. Always set OIDC_SIGNING_KEY in production.
func NewSigningKey() (*SigningKey, error) {
	sk := &SigningKey{}

	rawData := os.Getenv("OIDC_SIGNING_KEY")
	if rawData != "" {
		// Determine if the value is base64-encoded or raw PEM
		var pemBytes []byte
		if strings.HasPrefix(strings.TrimSpace(rawData), "-----BEGIN") {
			// Raw PEM format (works when set via docker-compose environment: block)
			pemBytes = []byte(rawData)
		} else {
			// Base64-encoded PEM (required for docker env_file: which can't handle multi-line)
			decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rawData))
			if err != nil {
				return nil, fmt.Errorf("OIDC_SIGNING_KEY is neither valid PEM nor valid base64: %w", err)
			}
			pemBytes = decoded
		}

		block, _ := pem.Decode(pemBytes)
		if block == nil {
			return nil, fmt.Errorf("failed to decode OIDC_SIGNING_KEY PEM block")
		}

		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			// Try PKCS8 format
			parsedKey, pkcs8Err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if pkcs8Err != nil {
				return nil, fmt.Errorf("failed to parse OIDC_SIGNING_KEY: PKCS1 error: %w, PKCS8 error: %v", err, pkcs8Err)
			}
			var ok bool
			key, ok = parsedKey.(*rsa.PrivateKey)
			if !ok {
				return nil, fmt.Errorf("OIDC_SIGNING_KEY is not an RSA private key")
			}
		}

		sk.privateKey = key
		logger.Info("OIDC signing key loaded from OIDC_SIGNING_KEY environment variable")
	} else {
		// Auto-generate RSA-2048 key pair for development
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, fmt.Errorf("failed to generate OIDC signing key: %w", err)
		}
		sk.privateKey = key
		logger.Warn("OIDC_SIGNING_KEY not set — auto-generated RSA key pair (dev mode, tokens won't persist across restarts)")
	}

	// Compute key ID (kid) from public key thumbprint
	pubKeyDER, err := x509.MarshalPKIXPublicKey(&sk.privateKey.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal public key for thumbprint: %w", err)
	}
	thumbprint := sha256.Sum256(pubKeyDER)
	sk.kid = base64.RawURLEncoding.EncodeToString(thumbprint[:])

	return sk, nil
}

// PrivateKey returns the RSA private key for signing JWTs.
func (sk *SigningKey) PrivateKey() *rsa.PrivateKey {
	sk.mu.RLock()
	defer sk.mu.RUnlock()
	return sk.privateKey
}

// KID returns the key ID for the JWKS endpoint.
func (sk *SigningKey) KID() string {
	sk.mu.RLock()
	defer sk.mu.RUnlock()
	return sk.kid
}

// JWKS returns the JSON Web Key Set representation of the public key.
// This is served at /.well-known/jwks for Azure AD to verify tokens.
func (sk *SigningKey) JWKS() map[string]interface{} {
	sk.mu.RLock()
	defer sk.mu.RUnlock()

	pub := &sk.privateKey.PublicKey
	return map[string]interface{}{
		"keys": []map[string]interface{}{
			{
				"kty": "RSA",
				"alg": "RS256",
				"use": "sig",
				"kid": sk.kid,
				"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			},
		},
	}
}
