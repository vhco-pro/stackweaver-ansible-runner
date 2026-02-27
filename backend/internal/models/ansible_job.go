// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"database/sql/driver"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// AnsibleJobStatus defines the status of an Ansible job
type AnsibleJobStatus string

const (
	AnsibleJobStatusPending    AnsibleJobStatus = "pending"    // Job is queued
	AnsibleJobStatusRunning    AnsibleJobStatus = "running"    // Job is executing
	AnsibleJobStatusSuccessful AnsibleJobStatus = "successful" // Job completed successfully
	AnsibleJobStatusFailed     AnsibleJobStatus = "failed"     // Job failed
	AnsibleJobStatusCanceled   AnsibleJobStatus = "canceled"   // Job was canceled
	AnsibleJobStatusError      AnsibleJobStatus = "error"      // Job had an error (different from playbook failure)
)

// AnsibleJobType defines the type of job execution
type AnsibleJobType string

const (
	AnsibleJobTypeRun    AnsibleJobType = "run"    // Normal playbook execution
	AnsibleJobTypeCheck  AnsibleJobType = "check"  // Dry-run mode (--check)
	AnsibleJobTypeSyntax AnsibleJobType = "syntax" // Syntax check only (--syntax-check)
)

// JobExtraVars is a map for storing extra variables as JSONB
type JobExtraVars map[string]interface{}

func (v JobExtraVars) Value() (driver.Value, error) {
	if v == nil {
		return json.Marshal(map[string]interface{}{})
	}
	return json.Marshal(v)
}

func (v *JobExtraVars) Scan(value interface{}) error {
	if value == nil {
		*v = make(JobExtraVars)
		return nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		return nil
	}
	return json.Unmarshal(bytes, v)
}

// AnsibleJob represents an Ansible job execution
type AnsibleJob struct {
	ID          uuid.UUID        `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	ProjectID   uuid.UUID        `gorm:"type:uuid;not null;index" json:"project_id"`
	PlaybookID  uuid.UUID        `gorm:"type:uuid;not null;index" json:"playbook_id"`
	InventoryID uuid.UUID        `gorm:"type:uuid;not null;index" json:"inventory_id"`
	TemplateID  *uuid.UUID       `gorm:"type:uuid;index" json:"template_id,omitempty"` // Optional: launched from template
	Name        string           `gorm:"type:varchar(255)" json:"name"`                // Job name/description
	JobType     AnsibleJobType   `gorm:"type:varchar(50);not null;default:'run'" json:"job_type"`
	Status      AnsibleJobStatus `gorm:"type:varchar(50);not null;default:'pending';index" json:"status"`

	// Execution Configuration
	ExtraVars     JobExtraVars `gorm:"type:jsonb;default:'{}'" json:"extra_vars"`
	Limit         string       `gorm:"type:varchar(500)" json:"limit,omitempty"`
	Tags          string       `gorm:"type:varchar(500)" json:"tags,omitempty"`
	SkipTags      string       `gorm:"type:varchar(500)" json:"skip_tags,omitempty"`
	Verbosity     int          `gorm:"default:0" json:"verbosity"` // 0-4
	Forks         int          `gorm:"default:5" json:"forks"`
	CredentialID  *uuid.UUID   `gorm:"type:uuid;index" json:"credential_id,omitempty"`
	BecomeEnabled bool         `gorm:"default:false" json:"become_enabled"`
	DiffMode      bool         `gorm:"default:false" json:"diff_mode"`

	// Ansible Version (for multi-version support)
	AnsibleVersion string `gorm:"type:varchar(50)" json:"ansible_version,omitempty"` // e.g., "2.15", "2.16"

	// Execution Results
	ExitCode     *int       `json:"exit_code,omitempty"`
	ErrorMessage string     `gorm:"type:text" json:"error_message,omitempty"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`

	// Statistics (populated after execution)
	HostsTotal       int  `gorm:"default:0" json:"hosts_total"`
	HostsOk          int  `gorm:"default:0" json:"hosts_ok"`
	HostsChanged     int  `gorm:"default:0" json:"hosts_changed"`
	HostsFailed      int  `gorm:"default:0" json:"hosts_failed"`
	HostsUnreachable int  `gorm:"default:0" json:"hosts_unreachable"`
	HostsSkipped     int  `gorm:"default:0" json:"hosts_skipped"`
	HostsRescued     int  `gorm:"default:0" json:"hosts_rescued"`
	HostsIgnored     int  `gorm:"default:0" json:"hosts_ignored"`
	HasWarnings      bool `gorm:"default:false" json:"has_warnings"`
	WarningsCount    int  `gorm:"default:0" json:"warnings_count"`

	// Self-hosted runner execution (Phase 3: Job Routing)
	AgentPoolID *uuid.UUID `gorm:"type:uuid;index" json:"agent_pool_id,omitempty"` // Agent pool used for execution
	RunnerID    *uuid.UUID `gorm:"type:uuid;index" json:"runner_id,omitempty"`     // Specific runner that executed the job

	// User tracking
	CreatedBy *uuid.UUID `gorm:"type:uuid;index" json:"created_by,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`

	// Relationships
	Project    Project             `gorm:"foreignKey:ProjectID" json:"project,omitempty"`
	Playbook   AnsiblePlaybook     `gorm:"foreignKey:PlaybookID" json:"playbook,omitempty"`
	Inventory  AnsibleInventory    `gorm:"foreignKey:InventoryID" json:"inventory,omitempty"`
	Template   *AnsibleJobTemplate `gorm:"foreignKey:TemplateID" json:"template,omitempty"`
	Credential *AnsibleCredential  `gorm:"foreignKey:CredentialID" json:"credential,omitempty"`
	Events     []AnsibleJobEvent   `gorm:"foreignKey:JobID" json:"events,omitempty"`
	AgentPool  *AgentPool          `gorm:"foreignKey:AgentPoolID" json:"agent_pool,omitempty"`
	Runner     *Runner             `gorm:"foreignKey:RunnerID" json:"runner,omitempty"`
}

func (j *AnsibleJob) BeforeCreate(tx *gorm.DB) error {
	if j.ID == uuid.Nil {
		j.ID = uuid.New()
	}
	if j.ExtraVars == nil {
		j.ExtraVars = make(JobExtraVars)
	}
	return nil
}

// AnsibleJobEvent represents an event that occurred during job execution
// Events are parsed from Ansible's JSON callback output
type AnsibleJobEvent struct {
	ID        uuid.UUID    `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	JobID     uuid.UUID    `gorm:"type:uuid;not null;index" json:"job_id"`
	Counter   int          `gorm:"not null;index:idx_job_counter" json:"counter"` // Sequence number
	Event     string       `gorm:"type:varchar(100);not null;index" json:"event"` // Event type (e.g., "playbook_on_start", "runner_on_ok")
	EventData JobExtraVars `gorm:"type:jsonb;default:'{}'" json:"event_data"`     // Event-specific data
	Host      string       `gorm:"type:varchar(255);index" json:"host,omitempty"` // Host name if applicable
	Task      string       `gorm:"type:varchar(500)" json:"task,omitempty"`       // Task name if applicable
	Play      string       `gorm:"type:varchar(500)" json:"play,omitempty"`       // Play name if applicable
	Role      string       `gorm:"type:varchar(255)" json:"role,omitempty"`       // Role name if applicable
	Stdout    string       `gorm:"type:text" json:"stdout,omitempty"`             // Standard output
	Stderr    string       `gorm:"type:text" json:"stderr,omitempty"`             // Standard error
	Changed   bool         `gorm:"default:false" json:"changed"`
	Failed    bool         `gorm:"default:false" json:"failed"`
	Skipped   bool         `gorm:"default:false" json:"skipped"`
	Timestamp time.Time    `json:"timestamp"`
	CreatedAt time.Time    `json:"created_at"`

	// Relationships
	Job AnsibleJob `gorm:"foreignKey:JobID" json:"job,omitempty"`
}

func (e *AnsibleJobEvent) BeforeCreate(tx *gorm.DB) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.EventData == nil {
		e.EventData = make(JobExtraVars)
	}
	return nil
}
