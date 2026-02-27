// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// TeamWorkspaceAccess represents team permissions on a workspace (TFE-compatible)
// Must match TFE structure exactly for provider compatibility
// Note: Workspace uses string IDs (format: ws-{16-char-id}), not UUIDs
// Supports both fixed access levels (admin, read, plan, write) and custom permissions block
type TeamWorkspaceAccess struct {
	ID          uuid.UUID `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	TeamID      uuid.UUID `gorm:"type:uuid;not null;uniqueIndex:idx_team_workspace" json:"team_id"`
	WorkspaceID string    `gorm:"type:varchar(20);not null;uniqueIndex:idx_team_workspace" json:"workspace_id"` // Workspace uses string IDs

	// Fixed access level (if using simple access)
	// One of: "admin", "read", "plan", "write"
	// Nullable: if set, custom permission fields should be null
	Access *string `gorm:"type:varchar(50)" json:"access,omitempty"`

	// Custom permissions (if using custom permissions block)
	// Nullable: if any of these are set, Access should be null
	Runs             *string `gorm:"type:varchar(50)" json:"runs,omitempty"`           // "read", "plan", or "apply"
	Variables        *string `gorm:"type:varchar(50)" json:"variables,omitempty"`      // "none", "read", or "write"
	StateVersions    *string `gorm:"type:varchar(50)" json:"state_versions,omitempty"` // "none", "read", "read-outputs", or "write"
	SentinelMocks    *string `gorm:"type:varchar(50)" json:"sentinel_mocks,omitempty"` // "none" or "read"
	WorkspaceLocking *bool   `gorm:"type:boolean" json:"workspace_locking,omitempty"`
	RunTasks         *bool   `gorm:"type:boolean" json:"run_tasks,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Relationships
	Team      Team      `gorm:"foreignKey:TeamID" json:"team,omitempty"`
	Workspace Workspace `gorm:"foreignKey:WorkspaceID" json:"workspace,omitempty"`
}

func (twa *TeamWorkspaceAccess) BeforeCreate(tx *gorm.DB) error {
	if twa.ID == uuid.Nil {
		twa.ID = uuid.New()
	}
	return nil
}
