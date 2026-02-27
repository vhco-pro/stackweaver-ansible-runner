// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"database/sql/driver"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ScheduleType defines what type of job the schedule triggers
type ScheduleType string

const (
	ScheduleTypeJobTemplate     ScheduleType = "job_template"     // Ansible job template
	ScheduleTypeInventorySource ScheduleType = "inventory_source" // Inventory sync
	ScheduleTypePlaybookSync    ScheduleType = "playbook_sync"    // Playbook VCS sync
)

// ScheduleStatus defines whether a schedule is active
type ScheduleStatus string

const (
	ScheduleStatusEnabled  ScheduleStatus = "enabled"
	ScheduleStatusDisabled ScheduleStatus = "disabled"
)

// ScheduleConfig stores additional schedule configuration
type ScheduleConfig map[string]interface{}

func (c ScheduleConfig) Value() (driver.Value, error) {
	if c == nil {
		return json.Marshal(map[string]interface{}{})
	}
	return json.Marshal(c)
}

func (c *ScheduleConfig) Scan(value interface{}) error {
	if value == nil {
		*c = make(ScheduleConfig)
		return nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		return nil
	}
	return json.Unmarshal(bytes, c)
}

// AnsibleSchedule represents a scheduled execution of a job template or inventory sync
type AnsibleSchedule struct {
	ID             uuid.UUID      `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	OrganizationID uuid.UUID      `gorm:"type:uuid;not null;index" json:"organization_id"`
	Name           string         `gorm:"type:varchar(255);not null" json:"name"`
	Description    string         `gorm:"type:text" json:"description"`
	Type           ScheduleType   `gorm:"type:varchar(50);not null" json:"type"`
	Status         ScheduleStatus `gorm:"type:varchar(50);default:'enabled'" json:"status"`

	// Target - what this schedule triggers
	// Only one of these should be set based on Type
	JobTemplateID     *uuid.UUID `gorm:"type:uuid;index" json:"job_template_id,omitempty"`
	InventorySourceID *uuid.UUID `gorm:"type:uuid;index" json:"inventory_source_id,omitempty"`
	PlaybookID        *uuid.UUID `gorm:"type:uuid;index" json:"playbook_id,omitempty"`

	// Cron expression (standard 5-field cron: minute hour day month weekday)
	// Examples:
	//   "0 2 * * *"     - Every day at 2:00 AM
	//   "*/15 * * * *"  - Every 15 minutes
	//   "0 9 * * 1-5"   - Weekdays at 9:00 AM
	//   "0 0 1 * *"     - First day of each month at midnight
	CronExpression string `gorm:"type:varchar(100);not null" json:"cron_expression"`

	// Timezone for the cron expression (e.g., "America/New_York", "Europe/London", "UTC")
	Timezone string `gorm:"type:varchar(100);default:'UTC'" json:"timezone"`

	// Optional date range constraints
	StartDateTime *time.Time `json:"start_date_time,omitempty"` // Schedule becomes active after this
	EndDateTime   *time.Time `json:"end_date_time,omitempty"`   // Schedule stops after this

	// Extra configuration (e.g., extra_vars override for job templates)
	Config ScheduleConfig `gorm:"type:jsonb;default:'{}'" json:"config,omitempty"`

	// Execution tracking
	NextRunAt     *time.Time `json:"next_run_at,omitempty"`                             // Next calculated run time
	LastRunAt     *time.Time `json:"last_run_at,omitempty"`                             // Last execution time
	LastJobID     *uuid.UUID `gorm:"type:uuid" json:"last_job_id,omitempty"`            // Last job created
	LastRunStatus string     `gorm:"type:varchar(50)" json:"last_run_status,omitempty"` // Last job status
	RunCount      int        `gorm:"default:0" json:"run_count"`                        // Total executions

	// User tracking
	CreatedBy *uuid.UUID `gorm:"type:uuid;index" json:"created_by,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`

	// Relationships
	Organization    Organization            `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"`
	JobTemplate     *AnsibleJobTemplate     `gorm:"foreignKey:JobTemplateID" json:"job_template,omitempty"`
	InventorySource *AnsibleInventorySource `gorm:"foreignKey:InventorySourceID" json:"inventory_source,omitempty"`
	Playbook        *AnsiblePlaybook        `gorm:"foreignKey:PlaybookID" json:"playbook,omitempty"`
}

func (s *AnsibleSchedule) BeforeCreate(tx *gorm.DB) error {
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	if s.Config == nil {
		s.Config = make(ScheduleConfig)
	}
	return nil
}

// Common cron expression presets for UI convenience
var CronPresets = map[string]string{
	"every_15_minutes":       "*/15 * * * *",
	"every_30_minutes":       "*/30 * * * *",
	"every_hour":             "0 * * * *",
	"every_2_hours":          "0 */2 * * *",
	"every_6_hours":          "0 */6 * * *",
	"every_12_hours":         "0 */12 * * *",
	"daily_midnight":         "0 0 * * *",
	"daily_noon":             "0 12 * * *",
	"daily_6am":              "0 6 * * *",
	"weekdays_9am":           "0 9 * * 1-5",
	"weekends_10am":          "0 10 * * 0,6",
	"weekly_monday_6am":      "0 6 * * 1",
	"weekly_sunday_midnight": "0 0 * * 0",
	"monthly_first_day":      "0 0 1 * *",
	"monthly_15th":           "0 0 15 * *",
}
