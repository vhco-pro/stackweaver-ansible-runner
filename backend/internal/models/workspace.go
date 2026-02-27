// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/pkg/id"
	"gorm.io/gorm"
)

type Workspace struct {
	ID          string    `gorm:"type:varchar(20);primary_key" json:"id"` // Format: ws-{16-char-id} = 19 chars, but varchar(20) for safety
	ProjectID   uuid.UUID `gorm:"type:uuid;not null;index" json:"project_id"`
	Name        string    `gorm:"type:varchar(255);not null;uniqueIndex:idx_project_workspace" json:"name"`
	Description string    `gorm:"type:text" json:"description"`

	// VCS Configuration
	VCSConnectionID      *uuid.UUID     `gorm:"type:uuid;index" json:"vcs_connection_id,omitempty"` // Link to VCSConnection
	VCSConnection        *VCSConnection `gorm:"foreignKey:VCSConnectionID" json:"vcs_connection,omitempty"`
	VCSProvider          string         `gorm:"type:varchar(50)" json:"vcs_provider"` // Deprecated: use VCSConnection
	VCSRepository        string         `gorm:"type:varchar(500)" json:"vcs_repository"`
	VCSBranch            string         `gorm:"type:varchar(255);default:'main'" json:"vcs_branch"`
	VCSWebhookSecret     string         `gorm:"type:varchar(255)" json:"-"`
	VCSIngressSubmodules bool           `gorm:"default:false" json:"vcs_ingress_submodules"`
	VCSTagsRegex         string         `gorm:"type:varchar(500)" json:"vcs_tags_regex,omitempty"`
	WorkingDirectory     string         `gorm:"type:varchar(500)" json:"working_directory"`

	// Terraform Configuration
	TerraformVersion string `gorm:"type:varchar(50)" json:"terraform_version"`

	// Auto-triggering
	AutoQueueRuns       bool   `gorm:"default:false" json:"auto_queue_runs"`
	TriggerPatterns     string `gorm:"type:text" json:"trigger_patterns,omitempty"` // JSON array
	TriggerPrefixes     string `gorm:"type:text" json:"trigger_prefixes,omitempty"` // JSON array
	FileTriggersEnabled bool   `gorm:"default:true" json:"file_triggers_enabled"`

	// Auto-apply
	AutoApply           bool   `gorm:"default:false" json:"auto_apply"`
	AutoApplyRunTrigger bool   `gorm:"default:false" json:"auto_apply_run_trigger"`
	AutoApplyBranch     string `gorm:"type:varchar(255)" json:"auto_apply_branch,omitempty"`

	// Execution Mode
	ExecutionMode string     `gorm:"type:varchar(50);default:'remote'" json:"execution_mode"` // remote, local, agent
	AgentPoolID   *uuid.UUID `gorm:"type:uuid" json:"agent_pool_id,omitempty"`

	// Run Settings
	QueueAllRuns               bool `gorm:"default:true" json:"queue_all_runs"`
	SpeculativeEnabled         bool `gorm:"default:true" json:"speculative_enabled"`
	AllowDestroyPlan           bool `gorm:"default:true" json:"allow_destroy_plan"`
	GlobalRemoteState          bool `gorm:"default:false" json:"global_remote_state"`
	StructuredRunOutputEnabled bool `gorm:"default:true" json:"structured_run_output_enabled"`
	AssessmentsEnabled         bool `gorm:"default:false" json:"assessments_enabled"`
	RunTimeout                 int  `gorm:"default:7200" json:"run_timeout"` // seconds (default: 2 hours)

	// Source metadata (set once at creation)
	SourceName string `gorm:"type:varchar(255)" json:"source_name,omitempty"`
	SourceURL  string `gorm:"type:text" json:"source_url,omitempty"`

	// Tags
	TagNames string `gorm:"type:text" json:"tag_names,omitempty"` // JSON array of strings

	// Deletion Settings
	ForceDelete bool `gorm:"default:false" json:"force_delete"` // TFE: force_delete attribute — allows deletion even with active infrastructure

	// Workspace State
	Locked       bool       `gorm:"default:false" json:"locked"`
	LockedBy     *uuid.UUID `gorm:"type:uuid" json:"locked_by,omitempty"`
	LockedAt     *time.Time `json:"locked_at,omitempty"`
	LockedReason string     `gorm:"type:text" json:"locked_reason,omitempty"`

	// Drift Detection
	DriftDetectionEnabled  bool       `gorm:"default:false" json:"drift_detection_enabled"`
	DriftDetectionSchedule string     `gorm:"type:varchar(100)" json:"drift_detection_schedule,omitempty"` // cron expression
	DriftDetectionTimezone string     `gorm:"type:varchar(50);default:'UTC'" json:"drift_detection_timezone"`
	NextDriftCheckAt       *time.Time `json:"next_drift_check_at,omitempty"`
	LastDriftCheckAt       *time.Time `json:"last_drift_check_at,omitempty"`

	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	Project   Project    `gorm:"foreignKey:ProjectID" json:"project,omitempty"`
	AgentPool AgentPool  `gorm:"foreignKey:AgentPoolID" json:"agent_pool,omitempty"`
	Runs      []Run      `gorm:"foreignKey:WorkspaceID" json:"runs,omitempty"`
	Variables []Variable `gorm:"foreignKey:WorkspaceID" json:"variables,omitempty"`
}

func (w *Workspace) BeforeCreate(tx *gorm.DB) error {
	if w.ID == "" {
		generatedID, err := id.GenerateWorkspaceID()
		if err != nil {
			return err
		}
		w.ID = generatedID
	}
	return nil
}
