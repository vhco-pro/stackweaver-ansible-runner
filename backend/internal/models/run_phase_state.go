// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"database/sql/driver"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ResourceState represents the state of a single resource during a phase
type ResourceState struct {
	Address      string     `json:"address"`                 // Resource address (e.g., "proxmox_vm.example")
	Status       string     `json:"status"`                  // pending, applying, completed, failed, cancelled
	ResourceID   string     `json:"resource_id,omitempty"`   // Resource ID if available
	CreatedAt    *time.Time `json:"created_at,omitempty"`    // When resource was created
	Action       string     `json:"action"`                  // create, update, delete, replace
	ErrorMessage string     `json:"error_message,omitempty"` // Error message if failed
	Details      string     `json:"details,omitempty"`       // Additional details
}

// ResourceStates is a slice of ResourceState for JSONB storage
type ResourceStates []ResourceState

func (r ResourceStates) Value() (driver.Value, error) {
	return json.Marshal(r)
}

func (r *ResourceStates) Scan(value interface{}) error {
	if value == nil {
		*r = make(ResourceStates, 0)
		return nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		return nil
	}
	return json.Unmarshal(bytes, r)
}

// PhaseSummary contains summary counts for a phase
type PhaseSummary struct {
	Additions    int `json:"additions"`
	Changes      int `json:"changes"`
	Destructions int `json:"destructions"`
	Replace      int `json:"replace"`
	Failed       int `json:"failed"`
	Total        int `json:"total"`
}

func (p PhaseSummary) Value() (driver.Value, error) {
	return json.Marshal(p)
}

func (p *PhaseSummary) Scan(value interface{}) error {
	if value == nil {
		*p = PhaseSummary{}
		return nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		return nil
	}
	return json.Unmarshal(bytes, p)
}

// RunPhaseState stores parsed state for a completed phase (plan or apply)
// This enables state persistence across page reloads without re-parsing logs
type RunPhaseState struct {
	ID        uuid.UUID      `gorm:"type:uuid;primary_key;default:gen_random_uuid()" json:"id"`
	RunID     string         `gorm:"type:varchar(20);not null;index;uniqueIndex:idx_run_phase" json:"run_id"` // Format: run-{16-char-id}
	Phase     string         `gorm:"type:varchar(20);not null;uniqueIndex:idx_run_phase" json:"phase"`        // 'plan' or 'apply'
	Resources ResourceStates `gorm:"type:jsonb;not null" json:"resources"`                                    // Array of resource states
	Summary   PhaseSummary   `gorm:"type:jsonb" json:"summary"`                                               // Summary counts
	ParsedAt  time.Time      `gorm:"default:now()" json:"parsed_at"`                                          // When state was parsed
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`

	// Relationships
	Run Run `gorm:"foreignKey:RunID" json:"run,omitempty"`
}

func (rps *RunPhaseState) BeforeCreate(tx *gorm.DB) error {
	if rps.ID == uuid.Nil {
		rps.ID = uuid.New()
	}
	if rps.ParsedAt.IsZero() {
		rps.ParsedAt = time.Now()
	}
	return nil
}

// TableName specifies the table name for GORM
func (RunPhaseState) TableName() string {
	return "run_phase_states"
}
