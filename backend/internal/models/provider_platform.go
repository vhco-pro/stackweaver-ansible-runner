// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type ProviderPlatform struct {
	ID                uuid.UUID `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	ProviderVersionID uuid.UUID `gorm:"type:uuid;not null;index;uniqueIndex:idx_provider_version_platform" json:"provider_version_id"`
	OS                string    `gorm:"type:varchar(50);not null;uniqueIndex:idx_provider_version_platform" json:"os"`   // linux, darwin, windows
	Arch              string    `gorm:"type:varchar(50);not null;uniqueIndex:idx_provider_version_platform" json:"arch"` // amd64, arm64, 386
	Filename          string    `gorm:"type:varchar(255);not null" json:"filename"`
	Shasum            string    `gorm:"type:varchar(64);not null" json:"shasum"`     // SHA256 checksum
	BinaryPath        string    `gorm:"type:varchar(500)" json:"binary_path"`        // MinIO path
	BinarySize        int64     `json:"binary_size"`                                 // Size in bytes
	GPGSignaturePath  string    `gorm:"type:varchar(500)" json:"gpg_signature_path"` // Path to GPG signature file (.sig)
	GPGKeyID          string    `gorm:"type:varchar(16)" json:"gpg_key_id"`          // GPG key ID used for signing

	// Relationships
	ProviderVersion ProviderVersion `gorm:"foreignKey:ProviderVersionID" json:"provider_version,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (pp *ProviderPlatform) BeforeCreate(tx *gorm.DB) error {
	if pp.ID == uuid.Nil {
		pp.ID = uuid.New()
	}
	return nil
}

// TableName specifies the table name for GORM
func (ProviderPlatform) TableName() string {
	return "provider_platforms"
}
