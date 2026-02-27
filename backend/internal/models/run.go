// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"database/sql/driver"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/pkg/id"
	"gorm.io/gorm"
)

type RunStatus string

const (
	// TFE-compatible run statuses (per https://developer.hashicorp.com/terraform/enterprise/api-docs/run)
	RunStatusPending   RunStatus = "pending"  // Initial status after creation
	RunStatusPlanning  RunStatus = "planning" // Planning phase in progress
	RunStatusPlanned   RunStatus = "planned"  // Planning phase completed (plan-and-apply runs wait here for apply)
	RunStatusApplying  RunStatus = "applying" // Applying phase in progress (plan-and-apply runs)
	RunStatusApplied   RunStatus = "applied"  // Applying phase completed (plan-and-apply runs)
	RunStatusFailed    RunStatus = "failed"   // Run failed at any phase
	RunStatusCancelled RunStatus = "canceled" // TFE uses "canceled" (American spelling)
	// Legacy statuses (for backward compatibility during migration)
	RunStatusRunning   RunStatus = "running"   // Generic running status (maps to planning or applying based on phase)
	RunStatusCompleted RunStatus = "completed" // Generic completed status (maps to planned or applied based on phase)
)

type RunOperation string

const (
	// TFE-compatible: Three run operation types
	RunOperationPlanOnly     RunOperation = "plan-only"      // Plan-only run (cannot be applied, CLI runs)
	RunOperationPlanAndApply RunOperation = "plan-and-apply" // Plan-and-apply run (goes through planning → planned → applying → applied)
	RunOperationDestroy      RunOperation = "destroy"        // Destroy run (tear down infrastructure)
)

type PlanOutput map[string]interface{}

func (p PlanOutput) Value() (driver.Value, error) {
	return json.Marshal(p)
}

func (p *PlanOutput) Scan(value interface{}) error {
	if value == nil {
		*p = make(PlanOutput)
		return nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		return nil
	}
	return json.Unmarshal(bytes, p)
}

type Run struct {
	ID                     string       `gorm:"type:varchar(20);primary_key" json:"id"`                           // Format: run-{16-char-id}
	WorkspaceID            string       `gorm:"type:varchar(20);not null;index" json:"workspace_id"`              // Format: ws-{16-char-id}
	ConfigurationVersionID *string      `gorm:"type:varchar(20);index" json:"configuration_version_id,omitempty"` // Format: cv-{16-char-id}
	CreatedBy              *uuid.UUID   `gorm:"type:uuid;index" json:"created_by"`
	Status                 RunStatus    `gorm:"type:varchar(50);not null;default:'pending';index" json:"status"`
	Operation              RunOperation `gorm:"type:varchar(50);not null" json:"operation"`
	PlanOutput             PlanOutput   `gorm:"type:jsonb" json:"plan_output,omitempty"`
	ErrorMessage           string       `gorm:"type:text" json:"error_message,omitempty"`
	AutoApplyAfterPlan     bool         `gorm:"default:false" json:"auto_apply_after_plan"` // For UI "Plan and Apply" runs
	StartedAt              *time.Time   `json:"started_at,omitempty"`                       // When plan phase started
	PlanCompletedAt        *time.Time   `json:"plan_completed_at,omitempty"`                // When plan phase completed (for plan-and-apply runs)
	ApplyStartedAt         *time.Time   `json:"apply_started_at,omitempty"`                 // When apply phase started (for plan-and-apply runs)
	CompletedAt            *time.Time   `json:"completed_at,omitempty"`                     // When apply phase completed (for plan-and-apply runs) or plan completed (for plan-only runs)
	CreatedAt              time.Time    `json:"created_at"`
	UpdatedAt              time.Time    `json:"updated_at"`

	// Self-hosted runner execution (Phase 3: Job Routing)
	AgentPoolID *uuid.UUID `gorm:"type:uuid;index" json:"agent_pool_id,omitempty"` // Agent pool used for execution
	RunnerID    *uuid.UUID `gorm:"type:uuid;index" json:"runner_id,omitempty"`     // Specific runner that executed the run

	// Relationships
	Workspace            Workspace             `gorm:"foreignKey:WorkspaceID" json:"workspace,omitempty"`
	ConfigurationVersion *ConfigurationVersion `gorm:"foreignKey:ConfigurationVersionID" json:"configuration_version,omitempty"`
	AgentPool            *AgentPool            `gorm:"foreignKey:AgentPoolID" json:"agent_pool,omitempty"`
	Runner               *Runner               `gorm:"foreignKey:RunnerID" json:"runner,omitempty"`
}

func (r *Run) BeforeCreate(tx *gorm.DB) error {
	if r.ID == "" {
		generatedID, err := id.GenerateRunID()
		if err != nil {
			return err
		}
		r.ID = generatedID
	}
	return nil
}
