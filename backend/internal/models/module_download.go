// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type ModuleDownload struct {
	ID              uuid.UUID `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	ModuleVersionID uuid.UUID `gorm:"type:uuid;not null;index" json:"module_version_id"`
	DownloadedAt    time.Time `gorm:"index" json:"downloaded_at"`
	IPAddress       string    `gorm:"type:varchar(45)" json:"ip_address"` // IPv4 or IPv6
	UserAgent       string    `gorm:"type:text" json:"user_agent"`

	// Relationships
	ModuleVersion ModuleVersion `gorm:"foreignKey:ModuleVersionID" json:"module_version,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

func (md *ModuleDownload) BeforeCreate(tx *gorm.DB) error {
	if md.ID == uuid.Nil {
		md.ID = uuid.New()
	}
	if md.DownloadedAt.IsZero() {
		md.DownloadedAt = time.Now()
	}
	return nil
}

// TableName specifies the table name for GORM
func (ModuleDownload) TableName() string {
	return "module_downloads"
}
