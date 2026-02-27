// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type TFETokenRepository struct {
	db *gorm.DB
}

func NewTFETokenRepository(db *gorm.DB) *TFETokenRepository {
	return &TFETokenRepository{db: db}
}

// HashToken hashes a token for storage (we store hashed tokens, not plaintext)
func HashToken(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}

// Create creates a new TFE token
func (r *TFETokenRepository) Create(token *models.TFEToken) error {
	// Hash the token before storing
	token.Token = HashToken(token.Token)
	return r.db.Create(token).Error
}

// GetByToken finds a token by its plaintext value (hashes it for lookup)
func (r *TFETokenRepository) GetByToken(tokenString string) (*models.TFEToken, error) {
	hashedToken := HashToken(tokenString)
	var tfeToken models.TFEToken
	err := r.db.Preload("User").Where("token = ?", hashedToken).First(&tfeToken).Error
	if err != nil {
		return nil, err
	}
	// Check if token is expired
	if tfeToken.ExpiresAt != nil && tfeToken.ExpiresAt.Before(time.Now()) {
		return nil, gorm.ErrRecordNotFound
	}
	return &tfeToken, nil
}

// GetByID gets a token by its ID
func (r *TFETokenRepository) GetByID(id uuid.UUID) (*models.TFEToken, error) {
	var token models.TFEToken
	err := r.db.Preload("User").First(&token, "id = ?", id).Error
	return &token, err
}

// ListByUser lists all tokens for a user
func (r *TFETokenRepository) ListByUser(userID uuid.UUID) ([]models.TFEToken, error) {
	var tokens []models.TFEToken
	err := r.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&tokens).Error
	return tokens, err
}

// UpdateLastUsed updates the last used timestamp
func (r *TFETokenRepository) UpdateLastUsed(id uuid.UUID) error {
	now := time.Now()
	return r.db.Model(&models.TFEToken{}).Where("id = ?", id).Update("last_used_at", now).Error
}

// Delete deletes a token
func (r *TFETokenRepository) Delete(id uuid.UUID) error {
	return r.db.Delete(&models.TFEToken{}, "id = ?", id).Error
}

// DeleteByUser deletes all tokens for a user
func (r *TFETokenRepository) DeleteByUser(userID uuid.UUID) error {
	return r.db.Delete(&models.TFEToken{}, "user_id = ?", userID).Error
}
