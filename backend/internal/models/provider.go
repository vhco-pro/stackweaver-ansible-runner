// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Provider struct {
	ID             uuid.UUID `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	OrganizationID uuid.UUID `gorm:"type:uuid;not null;index" json:"organization_id"`
	Name           string    `gorm:"type:varchar(255);not null" json:"name"`
	Description    string    `gorm:"type:text" json:"description"`
	Verified       bool      `gorm:"default:false" json:"verified"`       // Verified providers (like TFE)
	PublishedBy    uuid.UUID `gorm:"type:uuid;index" json:"published_by"` // User who published

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Relationships
	Organization Organization      `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"`
	Versions     []ProviderVersion `gorm:"foreignKey:ProviderID" json:"versions,omitempty"`
}

func (p *Provider) BeforeCreate(tx *gorm.DB) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	return nil
}

// TableName specifies the table name for GORM
func (Provider) TableName() string {
	return "providers"
}
