// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"database/sql/driver"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// RunnerType defines what kind of jobs the runner can execute
type RunnerType string

const (
	RunnerTypeTerraform RunnerType = "terraform"
	RunnerTypeAnsible   RunnerType = "ansible"
	RunnerTypeCombined  RunnerType = "combined"
)

// RunnerStatus represents the current state of the runner
type RunnerStatus string

const (
	RunnerStatusOnline  RunnerStatus = "online"
	RunnerStatusOffline RunnerStatus = "offline"
	RunnerStatusBusy    RunnerStatus = "busy"
	RunnerStatusError   RunnerStatus = "error"
)

// RunnerLabels is a custom type for storing labels as JSON
type RunnerLabels []string

// Value implements the driver.Valuer interface
func (l RunnerLabels) Value() (driver.Value, error) {
	if len(l) == 0 {
		return "[]", nil
	}
	return json.Marshal(l)
}

// Scan implements the sql.Scanner interface
func (l *RunnerLabels) Scan(value interface{}) error {
	if value == nil {
		*l = RunnerLabels{}
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
	return json.Unmarshal(bytes, l)
}

// RunnerCollections stores available Ansible Galaxy collections
type RunnerCollections []string

// Value implements the driver.Valuer interface
func (c RunnerCollections) Value() (driver.Value, error) {
	if len(c) == 0 {
		return "[]", nil
	}
	return json.Marshal(c)
}

// Scan implements the sql.Scanner interface
func (c *RunnerCollections) Scan(value interface{}) error {
	if value == nil {
		*c = RunnerCollections{}
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
	return json.Unmarshal(bytes, c)
}

// Runner represents a self-hosted runner registered with StackWeaver.
// Runners belong to an agent pool and can execute Terraform and/or Ansible jobs.
type Runner struct {
	ID             uuid.UUID    `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	OrganizationID uuid.UUID    `gorm:"type:uuid;not null;index" json:"organization_id"`
	AgentPoolID    uuid.UUID    `gorm:"type:uuid;not null;index" json:"agent_pool_id"`
	Name           string       `gorm:"type:varchar(255);not null;uniqueIndex:idx_runner_org_name" json:"name"`
	Description    string       `gorm:"type:text" json:"description"`
	RunnerType     RunnerType   `gorm:"type:varchar(50);not null;default:'combined'" json:"runner_type"`
	Status         RunnerStatus `gorm:"type:varchar(50);not null;default:'offline'" json:"status"`

	// Runner metadata (reported by agent)
	Hostname     string       `gorm:"type:varchar(255)" json:"hostname"`
	IPAddress    string       `gorm:"type:varchar(45)" json:"ip_address"` // IPv4 or IPv6
	OSType       string       `gorm:"type:varchar(100)" json:"os_type"`
	OSVersion    string       `gorm:"type:varchar(100)" json:"os_version"`
	AgentVersion string       `gorm:"type:varchar(50)" json:"agent_version"`
	Labels       RunnerLabels `gorm:"type:jsonb;default:'[]'" json:"labels"`

	// Capabilities (reported by agent)
	TerraformVersion     string            `gorm:"type:varchar(50)" json:"terraform_version"`
	AnsibleVersion       string            `gorm:"type:varchar(50)" json:"ansible_version"`
	AvailableCollections RunnerCollections `gorm:"type:jsonb;default:'[]'" json:"available_collections"`
	MaxConcurrentJobs    int               `gorm:"default:1" json:"max_concurrent_jobs"`
	CurrentJobs          int               `gorm:"-" json:"current_jobs"` // Computed, not stored

	// Heartbeat & health
	LastHeartbeatAt *time.Time `gorm:"type:timestamp" json:"last_heartbeat_at"`
	RegisteredAt    time.Time  `gorm:"default:now()" json:"registered_at"`

	// API key used to register this runner
	RegisteredWithAPIKeyID *uuid.UUID `gorm:"type:uuid;index" json:"registered_with_api_key_id,omitempty"`

	// Runner-specific API key (generated after registration)
	RunnerAPIKeyID *uuid.UUID `gorm:"type:uuid;index" json:"runner_api_key_id,omitempty"`

	// Timestamps
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Relations
	Organization Organization `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"`
	AgentPool    AgentPool    `gorm:"foreignKey:AgentPoolID" json:"agent_pool,omitempty"`
}

// TableName overrides default table name
func (Runner) TableName() string {
	return "runners"
}

// BeforeCreate sets defaults before creating a runner
func (r *Runner) BeforeCreate(tx *gorm.DB) error {
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	if r.RunnerType == "" {
		r.RunnerType = RunnerTypeCombined
	}
	if r.Status == "" {
		r.Status = RunnerStatusOffline
	}
	if r.MaxConcurrentJobs == 0 {
		r.MaxConcurrentJobs = 1
	}
	return nil
}

// IsOnline returns true if the runner is online
func (r *Runner) IsOnline() bool {
	return r.Status == RunnerStatusOnline || r.Status == RunnerStatusBusy
}

// IsAvailable returns true if the runner can accept new jobs
func (r *Runner) IsAvailable() bool {
	return r.Status == RunnerStatusOnline && r.CurrentJobs < r.MaxConcurrentJobs
}

// CanExecuteTerraform returns true if the runner can execute Terraform jobs
func (r *Runner) CanExecuteTerraform() bool {
	return r.RunnerType == RunnerTypeTerraform || r.RunnerType == RunnerTypeCombined
}

// CanExecuteAnsible returns true if the runner can execute Ansible jobs
func (r *Runner) CanExecuteAnsible() bool {
	return r.RunnerType == RunnerTypeAnsible || r.RunnerType == RunnerTypeCombined
}

// HasLabel returns true if the runner has the specified label
func (r *Runner) HasLabel(label string) bool {
	for _, l := range r.Labels {
		if l == label {
			return true
		}
	}
	return false
}

// HasAllLabels returns true if the runner has all specified labels
func (r *Runner) HasAllLabels(labels []string) bool {
	for _, required := range labels {
		if !r.HasLabel(required) {
			return false
		}
	}
	return true
}
