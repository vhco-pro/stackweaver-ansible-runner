// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Team represents a team within an organization
// Must match TFE structure exactly for provider compatibility
type Team struct {
	ID             uuid.UUID `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	OrganizationID uuid.UUID `gorm:"type:uuid;not null;index" json:"organization_id"`
	Name           string    `gorm:"type:varchar(255);not null;uniqueIndex:idx_org_team" json:"name"`
	Description    string    `gorm:"type:text" json:"description"`
	Visibility     string    `gorm:"type:varchar(50);default:'secret'" json:"visibility"` // 'organization' or 'secret' (TFE default is 'secret')

	// TFE-compatible fields
	AllowMemberTokenManagement bool    `gorm:"default:true" json:"allow_member_token_management"`           // Controls whether team members can manage team tokens
	SSOTeamID                  *string `gorm:"type:varchar(255);unique;index" json:"sso_team_id,omitempty"` // SSO team ID for OIDC/SAML integration (nullable)

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Relationships
	Organization       Organization            `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"`
	Members            []TeamMember            `gorm:"foreignKey:TeamID" json:"members,omitempty"`
	ProjectAccess      []TeamProjectAccess     `gorm:"foreignKey:TeamID" json:"project_access,omitempty"`
	WorkspaceAccess    []TeamWorkspaceAccess   `gorm:"foreignKey:TeamID" json:"workspace_access,omitempty"`
	OrganizationAccess *TeamOrganizationAccess `gorm:"foreignKey:TeamID" json:"organization_access,omitempty"`
}

func (t *Team) BeforeCreate(tx *gorm.DB) error {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	return nil
}
