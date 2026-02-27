// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// AnsiblePlaybook represents an Ansible playbook configuration
// A playbook is linked to a project and contains the configuration for running Ansible
// VCS integration is handled exclusively via GitHub App (no legacy SCM credentials)
type AnsiblePlaybook struct {
	ID          uuid.UUID `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	ProjectID   uuid.UUID `gorm:"type:uuid;not null;index" json:"project_id"`
	Name        string    `gorm:"type:varchar(255);not null;uniqueIndex:idx_project_playbook" json:"name"`
	Description string    `gorm:"type:text" json:"description"`

	// VCS Connection (GitHub App integration - the only supported method)
	VCSConnectionID *uuid.UUID     `gorm:"type:uuid;index" json:"vcs_connection_id,omitempty"`
	VCSRepository   string         `gorm:"type:varchar(500)" json:"vcs_repository,omitempty"` // Repository full name (e.g., "owner/repo")
	VCSBranch       string         `gorm:"type:varchar(255);default:'main'" json:"vcs_branch"`
	PlaybookPath    string         `gorm:"type:varchar(500);not null;default:'site.yml'" json:"playbook_path"` // Path to playbook file within repo
	VCSConnection   *VCSConnection `gorm:"foreignKey:VCSConnectionID" json:"vcs_connection,omitempty"`

	// Sync Status
	LastSyncAt     *time.Time `json:"last_sync_at,omitempty"`
	LastSyncStatus string     `gorm:"type:varchar(50)" json:"last_sync_status,omitempty"`  // success, failed
	LastSyncCommit string     `gorm:"type:varchar(100)" json:"last_sync_commit,omitempty"` // Git commit SHA
	LastSyncError  string     `gorm:"type:text" json:"last_sync_error,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Relationships
	Project      Project              `gorm:"foreignKey:ProjectID" json:"project,omitempty"`
	Jobs         []AnsibleJob         `gorm:"foreignKey:PlaybookID" json:"jobs,omitempty"`
	JobTemplates []AnsibleJobTemplate `gorm:"foreignKey:PlaybookID" json:"job_templates,omitempty"`
}

func (p *AnsiblePlaybook) BeforeCreate(tx *gorm.DB) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	return nil
}

// AnsibleJobTemplate represents a reusable job configuration
// Templates allow users to pre-configure job settings for repeated execution
type AnsibleJobTemplate struct {
	ID          uuid.UUID `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	ProjectID   uuid.UUID `gorm:"type:uuid;not null;index" json:"project_id"`
	PlaybookID  uuid.UUID `gorm:"type:uuid;not null;index" json:"playbook_id"`
	InventoryID uuid.UUID `gorm:"type:uuid;not null;index" json:"inventory_id"`
	Name        string    `gorm:"type:varchar(255);not null;uniqueIndex:idx_project_template" json:"name"`
	Description string    `gorm:"type:text" json:"description"`

	// Job Configuration
	ExtraVars     InventoryVariables `gorm:"type:jsonb;default:'{}'" json:"extra_vars"` // Extra variables passed to playbook
	Limit         string             `gorm:"type:varchar(500)" json:"limit,omitempty"`  // Host pattern limit
	Tags          string             `gorm:"type:varchar(500)" json:"tags,omitempty"`   // Tags to run
	SkipTags      string             `gorm:"type:varchar(500)" json:"skip_tags,omitempty"`
	Verbosity     int                `gorm:"default:0" json:"verbosity"` // 0-4 (v, vv, vvv, vvvv)
	Forks         int                `gorm:"default:5" json:"forks"`     // Parallelism
	CredentialID  *uuid.UUID         `gorm:"type:uuid;index" json:"credential_id,omitempty"`
	BecomeEnabled bool               `gorm:"default:false" json:"become_enabled"` // sudo escalation
	DiffMode      bool               `gorm:"default:false" json:"diff_mode"`      // Show file changes

	// Galaxy Collections/Roles Requirements
	// Format: {"collections": [{"name": "amazon.aws", "version": ">=6.0.0"}], "roles": [...]}
	GalaxyRequirements InventoryVariables `gorm:"type:jsonb;default:'{}'" json:"galaxy_requirements"`

	// Self-hosted runner execution
	AgentPoolID *uuid.UUID `gorm:"type:uuid;index" json:"agent_pool_id,omitempty"` // Agent pool for job execution

	// Scheduling (Phase 2)
	ScheduleEnabled bool   `gorm:"default:false" json:"schedule_enabled"`
	ScheduleCron    string `gorm:"type:varchar(100)" json:"schedule_cron,omitempty"` // Cron expression

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Relationships
	Project    Project                      `gorm:"foreignKey:ProjectID" json:"project,omitempty"`
	Playbook   AnsiblePlaybook              `gorm:"foreignKey:PlaybookID" json:"playbook,omitempty"`
	Inventory  AnsibleInventory             `gorm:"foreignKey:InventoryID" json:"inventory,omitempty"`
	Credential *AnsibleCredential           `gorm:"foreignKey:CredentialID" json:"credential,omitempty"`
	AgentPool  *AgentPool                   `gorm:"foreignKey:AgentPoolID" json:"agent_pool,omitempty"`
	Variables  []AnsibleJobTemplateVariable `gorm:"foreignKey:JobTemplateID" json:"variables,omitempty"`
}

func (t *AnsibleJobTemplate) BeforeCreate(tx *gorm.DB) error {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	if t.ExtraVars == nil {
		t.ExtraVars = make(InventoryVariables)
	}
	return nil
}
