// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Project struct {
	ID             uuid.UUID            `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	OrganizationID uuid.UUID            `gorm:"type:uuid;not null;index;uniqueIndex:idx_org_project" json:"organization_id"`
	Name           string               `gorm:"type:varchar(255);not null;uniqueIndex:idx_org_project" json:"name"`
	Description    string               `gorm:"type:text" json:"description"`
	CreatedAt      time.Time            `json:"created_at"`
	UpdatedAt      time.Time            `json:"updated_at"`
	Organization   Organization         `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"`
	Workspaces     []Workspace          `gorm:"foreignKey:ProjectID" json:"workspaces,omitempty"`
	Inventories    []AnsibleInventory   `gorm:"foreignKey:ProjectID" json:"inventories,omitempty"`
	Playbooks      []AnsiblePlaybook    `gorm:"foreignKey:ProjectID" json:"playbooks,omitempty"`
	JobTemplates   []AnsibleJobTemplate `gorm:"foreignKey:ProjectID" json:"job_templates,omitempty"`
	Workflows      []AnsibleWorkflow    `gorm:"foreignKey:ProjectID" json:"workflows,omitempty"`
	Credentials    []AnsibleCredential  `gorm:"foreignKey:ProjectID" json:"credentials,omitempty"`
}

func (p *Project) BeforeCreate(tx *gorm.DB) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	return nil
}
