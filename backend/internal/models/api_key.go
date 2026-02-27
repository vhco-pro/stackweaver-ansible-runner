// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"database/sql/driver"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// StringArray is a custom type for storing string arrays in PostgreSQL
type StringArray []string

// Value implements the driver.Valuer interface
func (a StringArray) Value() (driver.Value, error) {
	if len(a) == 0 {
		return "[]", nil
	}
	return json.Marshal(a)
}

// Scan implements the sql.Scanner interface
func (a *StringArray) Scan(value interface{}) error {
	if value == nil {
		*a = StringArray{}
		return nil
	}

	var bytes []byte
	switch v := value.(type) {
	case []byte:
		bytes = v
	case string:
		bytes = []byte(v)
	default:
		return nil
	}

	return json.Unmarshal(bytes, a)
}

// APIKey represents an API key for programmatic access
type APIKey struct {
	ID     uuid.UUID `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	UserID uuid.UUID `gorm:"type:uuid;not null;index" json:"user_id"`
	User   User      `gorm:"foreignKey:UserID" json:"user,omitempty"`

	// Key information
	Name      string `gorm:"type:varchar(255);not null" json:"name"`
	KeyHash   string `gorm:"type:varchar(255);not null;index" json:"-"`   // Hashed key (bcrypt)
	KeyPrefix string `gorm:"type:varchar(20);not null" json:"key_prefix"` // First 8 chars for display

	// Scopes - defines what the API key can access
	// Format examples:
	//   - "*" - all permissions
	//   - "org:<org_id>:read" - organization-scoped read access
	//   - "project:<project_id>:write" - project-scoped write access
	//   - "team:<team_id>:read" - team-scoped read access
	//   - "user:read" - user-scoped read access
	//   - "read" - legacy format, treated as user scope
	Scopes StringArray `gorm:"type:jsonb;default:'[]'" json:"scopes"`

	// Optional scope restrictions - if set, the key is limited to these resources
	// These are derived from the scopes for efficient querying
	OrganizationID *uuid.UUID `gorm:"type:uuid;index" json:"organization_id,omitempty"`
	ProjectID      *uuid.UUID `gorm:"type:uuid;index" json:"project_id,omitempty"`
	TeamID         *uuid.UUID `gorm:"type:uuid;index" json:"team_id,omitempty"`

	// Expiration
	ExpiresAt *time.Time `gorm:"type:timestamp" json:"expires_at,omitempty"`

	// Usage tracking
	LastUsedAt *time.Time `gorm:"type:timestamp" json:"last_used_at,omitempty"`

	// Timestamps
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (k *APIKey) BeforeCreate(tx *gorm.DB) error {
	if k.ID == uuid.Nil {
		k.ID = uuid.New()
	}
	return nil
}

// IsExpired checks if the API key has expired
func (k *APIKey) IsExpired() bool {
	if k.ExpiresAt == nil {
		return false
	}
	return k.ExpiresAt.Before(time.Now())
}

// IsOrganizationScoped returns true if the API key is scoped to a specific organization
func (k *APIKey) IsOrganizationScoped() bool {
	return k.OrganizationID != nil
}

// IsProjectScoped returns true if the API key is scoped to a specific project
func (k *APIKey) IsProjectScoped() bool {
	return k.ProjectID != nil
}

// IsTeamScoped returns true if the API key is scoped to a specific team
func (k *APIKey) IsTeamScoped() bool {
	return k.TeamID != nil
}
