// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// AgentPool groups self-hosted runners and scopes which workspaces/projects can use them.
// TFE-compatible: see go-tfe/agent_pool.go, terraform-provider-tfe agent_pool resources.
type AgentPool struct {
	ID                 uuid.UUID `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	OrganizationID     uuid.UUID `gorm:"type:uuid;not null;uniqueIndex:idx_agent_pool_org_name" json:"organization_id"`
	Name               string    `gorm:"type:varchar(255);not null;uniqueIndex:idx_agent_pool_org_name" json:"name"`
	OrganizationScoped bool      `gorm:"default:true;not null" json:"organization_scoped"`
	CreatedAt          time.Time `json:"created_at"`

	Organization       Organization `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"`
	AllowedWorkspaces  []Workspace  `gorm:"many2many:agent_pool_allowed_workspaces;" json:"allowed_workspaces,omitempty"`
	AllowedProjects    []Project    `gorm:"many2many:agent_pool_allowed_projects;" json:"allowed_projects,omitempty"`
	ExcludedWorkspaces []Workspace  `gorm:"many2many:agent_pool_excluded_workspaces;" json:"excluded_workspaces,omitempty"`
}

// TableName overrides default table name.
func (AgentPool) TableName() string {
	return "agent_pools"
}

func (p *AgentPool) BeforeCreate(tx *gorm.DB) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	return nil
}
