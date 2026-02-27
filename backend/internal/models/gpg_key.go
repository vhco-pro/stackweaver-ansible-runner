// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// GPGKey represents a GPG public key for signing provider binaries
type GPGKey struct {
	ID             uuid.UUID `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	OrganizationID uuid.UUID `gorm:"type:uuid;not null;index" json:"organization_id"`
	KeyID          string    `gorm:"type:varchar(16);not null;index" json:"key_id"` // Short key ID (last 8 chars)
	ASCIIArmor     string    `gorm:"type:text;not null" json:"ascii_armor"`         // Full GPG public key in ASCII armor format
	CreatedBy      uuid.UUID `gorm:"type:uuid;index" json:"created_by"`             // User who uploaded the key
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`

	// Relationships
	Organization Organization `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"`
}

func (g *GPGKey) BeforeCreate(tx *gorm.DB) error {
	if g.ID == uuid.Nil {
		g.ID = uuid.New()
	}
	return nil
}

// TableName specifies the table name for GORM
func (GPGKey) TableName() string {
	return "gpg_keys"
}
