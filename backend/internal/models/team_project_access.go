// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// TeamProjectAccess represents team permissions on a project (TFE-compatible)
// Must match TFE structure exactly for provider compatibility
// Note: Project access supports fixed access levels AND custom access with two permission blocks
// Fixed access levels: "admin", "maintain", "write", "read", "custom"
// When access = "custom", both project_access and workspace_access blocks are required
type TeamProjectAccess struct {
	ID        uuid.UUID `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	TeamID    uuid.UUID `gorm:"type:uuid;not null;uniqueIndex:idx_team_project" json:"team_id"`
	ProjectID uuid.UUID `gorm:"type:uuid;not null;uniqueIndex:idx_team_project" json:"project_id"`

	// Fixed access level (if using simple access)
	// One of: "admin", "maintain", "write", "read", "custom"
	// Nullable: if set to "custom", custom permission fields should be set
	Access *string `gorm:"type:varchar(50)" json:"access,omitempty"`

	// Custom project access permissions (if access = "custom")
	// Nullable: if any of these are set, Access should be "custom"
	ProjectSettings     *string `gorm:"type:varchar(50)" json:"project_settings,omitempty"`      // "read", "update", "delete"
	ProjectTeams        *string `gorm:"type:varchar(50)" json:"project_teams,omitempty"`         // "none", "read", "manage"
	ProjectVariableSets *string `gorm:"type:varchar(50)" json:"project_variable_sets,omitempty"` // "none", "read", "write"

	// Custom workspace access permissions (if access = "custom")
	// These apply to ALL workspaces within the project
	// Nullable: if any of these are set, Access should be "custom"
	WorkspaceRuns          *string `gorm:"type:varchar(50)" json:"workspace_runs,omitempty"`           // "read", "plan", "apply"
	WorkspaceSentinelMocks *string `gorm:"type:varchar(50)" json:"workspace_sentinel_mocks,omitempty"` // "none", "read"
	WorkspaceStateVersions *string `gorm:"type:varchar(50)" json:"workspace_state_versions,omitempty"` // "none", "read-outputs", "read", "write"
	WorkspaceVariables     *string `gorm:"type:varchar(50)" json:"workspace_variables,omitempty"`      // "none", "read", "write"
	WorkspaceCreate        *bool   `gorm:"type:boolean" json:"workspace_create,omitempty"`             // permission to create workspaces
	WorkspaceLocking       *bool   `gorm:"type:boolean" json:"workspace_locking,omitempty"`            // permission to lock/unlock workspaces
	WorkspaceMove          *bool   `gorm:"type:boolean" json:"workspace_move,omitempty"`               // permission to move workspaces
	WorkspaceDelete        *bool   `gorm:"type:boolean" json:"workspace_delete,omitempty"`             // permission to delete workspaces
	WorkspaceRunTasks      *bool   `gorm:"type:boolean" json:"workspace_run_tasks,omitempty"`          // permission to manage run tasks

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Relationships
	Team    Team    `gorm:"foreignKey:TeamID" json:"team,omitempty"`
	Project Project `gorm:"foreignKey:ProjectID" json:"project,omitempty"`
}

func (tpa *TeamProjectAccess) BeforeCreate(tx *gorm.DB) error {
	if tpa.ID == uuid.Nil {
		tpa.ID = uuid.New()
	}
	return nil
}
