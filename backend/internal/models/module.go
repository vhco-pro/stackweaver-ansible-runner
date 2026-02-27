// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"database/sql/driver"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// JSONB type for storing JSON data in PostgreSQL
type JSONB map[string]interface{}

func (j JSONB) Value() (driver.Value, error) {
	return json.Marshal(j)
}

func (j *JSONB) Scan(value interface{}) error {
	if value == nil {
		*j = make(JSONB)
		return nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		return nil
	}
	return json.Unmarshal(bytes, j)
}

type Module struct {
	ID             uuid.UUID `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	OrganizationID uuid.UUID `gorm:"type:uuid;not null;index" json:"organization_id"`
	Name           string    `gorm:"type:varchar(255);not null" json:"name"`
	Provider       string    `gorm:"type:varchar(50);not null" json:"provider"`
	Description    string    `gorm:"type:text" json:"description"`
	Source         string    `gorm:"type:varchar(500)" json:"source"` // Git URL (for VCS-connected modules)
	Verified       bool      `gorm:"default:false" json:"verified"`   // Partner/verified modules
	PublishedBy    uuid.UUID `gorm:"type:uuid;index" json:"published_by"`

	// VCS Integration (like Workspace)
	VCSConnectionID  *uuid.UUID     `gorm:"type:uuid;index" json:"vcs_connection_id,omitempty"`
	VCSConnection    *VCSConnection `gorm:"foreignKey:VCSConnectionID" json:"vcs_connection,omitempty"`
	VCSRepository    string         `gorm:"type:varchar(500)" json:"vcs_repository"` // Full repo path (owner/repo)
	VCSWebhookSecret string         `gorm:"type:varchar(255)" json:"-"`              // Webhook secret for tag push events
	AutoPublishTags  bool           `gorm:"default:true" json:"auto_publish_tags"`   // Auto-publish versions from Git tags

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Relationships
	Organization Organization    `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"`
	Versions     []ModuleVersion `gorm:"foreignKey:ModuleID" json:"versions,omitempty"`
}

func (m *Module) BeforeCreate(tx *gorm.DB) error {
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	return nil
}

// TableName specifies the table name for GORM
func (Module) TableName() string {
	return "modules"
}
