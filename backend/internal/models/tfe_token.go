// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"crypto/rand"
	"encoding/base64"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type TFEToken struct {
	ID          uuid.UUID  `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	UserID      uuid.UUID  `gorm:"type:uuid;not null;index" json:"user_id"`
	Token       string     `gorm:"type:varchar(255);uniqueIndex;not null" json:"token"` // Hashed token
	Description string     `gorm:"type:varchar(255)" json:"description"`
	LastUsedAt  *time.Time `gorm:"type:timestamp" json:"last_used_at,omitempty"`
	ExpiresAt   *time.Time `gorm:"type:timestamp" json:"expires_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	User        User       `gorm:"foreignKey:UserID" json:"user,omitempty"`
}

func (t *TFEToken) BeforeCreate(tx *gorm.DB) error {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	return nil
}

// GenerateToken generates a secure random token string
func GenerateTFEToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	// Base64 encode and format like TFE tokens (prefix with "tfe-" for identification)
	return "tfe-" + base64.URLEncoding.EncodeToString(bytes), nil
}
