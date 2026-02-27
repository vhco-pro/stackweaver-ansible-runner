// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// AnsibleConfig stores ansible.cfg configuration at different scopes.
// Priority: Workspace > Project > Organization (most specific wins).
type AnsibleConfig struct {
	ID             uuid.UUID  `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	OrganizationID *uuid.UUID `gorm:"type:uuid;uniqueIndex:idx_ansible_config_org;index" json:"organization_id,omitempty"`
	ProjectID      *uuid.UUID `gorm:"type:uuid;uniqueIndex:idx_ansible_config_project;index" json:"project_id,omitempty"`
	WorkspaceID    *string    `gorm:"type:varchar(20);uniqueIndex:idx_ansible_config_workspace;index" json:"workspace_id,omitempty"`

	// The ansible.cfg content
	ConfigContent string `gorm:"type:text;not null" json:"config_content"`

	// Audit fields
	CreatedByID uuid.UUID `gorm:"type:uuid;not null" json:"created_by_id"`
	UpdatedByID uuid.UUID `gorm:"type:uuid" json:"updated_by_id"`

	// Timestamps
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Relations
	Organization *Organization `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"`
	Project      *Project      `gorm:"foreignKey:ProjectID" json:"project,omitempty"`
	CreatedBy    User          `gorm:"foreignKey:CreatedByID" json:"created_by,omitempty"`
	UpdatedBy    *User         `gorm:"foreignKey:UpdatedByID" json:"updated_by,omitempty"`
}

// TableName overrides default table name
func (AnsibleConfig) TableName() string {
	return "ansible_configs"
}

// BeforeCreate sets defaults
func (c *AnsibleConfig) BeforeCreate(tx *gorm.DB) error {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	return nil
}

// Scope returns the scope level of this config
func (c *AnsibleConfig) Scope() string {
	if c.WorkspaceID != nil {
		return "workspace"
	}
	if c.ProjectID != nil {
		return "project"
	}
	if c.OrganizationID != nil {
		return "organization"
	}
	return "unknown"
}

// Priority returns the priority level (higher = more specific)
func (c *AnsibleConfig) Priority() int {
	if c.WorkspaceID != nil {
		return 3 // Highest priority
	}
	if c.ProjectID != nil {
		return 2
	}
	if c.OrganizationID != nil {
		return 1
	}
	return 0
}
