// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// JobType defines the type of job being executed
type JobType string

const (
	JobTypeTerraformRun JobType = "terraform_run"
	JobTypeAnsibleJob   JobType = "ansible_job"
)

// JobExecutionStatus represents the status of a job execution
type JobExecutionStatus string

const (
	JobExecutionStatusPending   JobExecutionStatus = "pending"
	JobExecutionStatusRunning   JobExecutionStatus = "running"
	JobExecutionStatusCompleted JobExecutionStatus = "completed"
	JobExecutionStatusFailed    JobExecutionStatus = "failed"
	JobExecutionStatusCanceled  JobExecutionStatus = "canceled"
)

// RunnerJobExecution tracks job executions on runners
type RunnerJobExecution struct {
	ID       uuid.UUID `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	RunnerID uuid.UUID `gorm:"type:uuid;not null;index" json:"runner_id"`
	JobType  JobType   `gorm:"type:varchar(50);not null" json:"job_type"`
	JobID    uuid.UUID `gorm:"type:uuid;not null;index" json:"job_id"` // References terraform_runs or ansible_jobs

	// Job details (denormalized for quick access)
	WorkspaceID   string `gorm:"type:varchar(20)" json:"workspace_id,omitempty"`
	WorkspaceName string `gorm:"type:varchar(255)" json:"workspace_name,omitempty"`

	// Execution status
	Status       JobExecutionStatus `gorm:"type:varchar(50);not null;default:'pending'" json:"status"`
	StartedAt    *time.Time         `gorm:"type:timestamp" json:"started_at,omitempty"`
	FinishedAt   *time.Time         `gorm:"type:timestamp" json:"finished_at,omitempty"`
	ErrorMessage string             `gorm:"type:text" json:"error_message,omitempty"`

	// Timestamps
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Relations
	Runner Runner `gorm:"foreignKey:RunnerID" json:"runner,omitempty"`
}

// TableName overrides default table name
func (RunnerJobExecution) TableName() string {
	return "runner_job_executions"
}

// BeforeCreate sets defaults
func (e *RunnerJobExecution) BeforeCreate(tx *gorm.DB) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.Status == "" {
		e.Status = JobExecutionStatusPending
	}
	return nil
}

// Duration returns the duration of the job execution
func (e *RunnerJobExecution) Duration() time.Duration {
	if e.StartedAt == nil {
		return 0
	}
	end := time.Now()
	if e.FinishedAt != nil {
		end = *e.FinishedAt
	}
	return end.Sub(*e.StartedAt)
}

// IsComplete returns true if the job has finished (success or failure)
func (e *RunnerJobExecution) IsComplete() bool {
	return e.Status == JobExecutionStatusCompleted ||
		e.Status == JobExecutionStatusFailed ||
		e.Status == JobExecutionStatusCanceled
}
