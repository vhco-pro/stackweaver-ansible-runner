// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type ProviderDownload struct {
	ID                 uuid.UUID `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	ProviderPlatformID uuid.UUID `gorm:"type:uuid;not null;index" json:"provider_platform_id"`
	DownloadedAt       time.Time `gorm:"index" json:"downloaded_at"`
	IPAddress          string    `gorm:"type:varchar(45)" json:"ip_address"` // IPv4 or IPv6
	UserAgent          string    `gorm:"type:text" json:"user_agent"`

	// Relationships
	ProviderPlatform ProviderPlatform `gorm:"foreignKey:ProviderPlatformID" json:"provider_platform,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

func (pd *ProviderDownload) BeforeCreate(tx *gorm.DB) error {
	if pd.ID == uuid.Nil {
		pd.ID = uuid.New()
	}
	if pd.DownloadedAt.IsZero() {
		pd.DownloadedAt = time.Now()
	}
	return nil
}

// TableName specifies the table name for GORM
func (ProviderDownload) TableName() string {
	return "provider_downloads"
}
