// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type ProviderVersion struct {
	ID          uuid.UUID `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	ProviderID  uuid.UUID `gorm:"type:uuid;not null;index;uniqueIndex:idx_provider_version" json:"provider_id"`
	Version     string    `gorm:"type:varchar(50);not null;uniqueIndex:idx_provider_version" json:"version"` // Semantic version (strict, no pre-release)
	PublishedAt time.Time `json:"published_at"`
	Downloads   int       `gorm:"default:0" json:"downloads"`

	// Relationships
	Provider  Provider           `gorm:"foreignKey:ProviderID" json:"provider,omitempty"`
	Platforms []ProviderPlatform `gorm:"foreignKey:ProviderVersionID" json:"platforms,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (pv *ProviderVersion) BeforeCreate(tx *gorm.DB) error {
	if pv.ID == uuid.Nil {
		pv.ID = uuid.New()
	}
	if pv.PublishedAt.IsZero() {
		pv.PublishedAt = time.Now()
	}
	return nil
}

// TableName specifies the table name for GORM
func (ProviderVersion) TableName() string {
	return "provider_versions"
}
