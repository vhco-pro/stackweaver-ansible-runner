// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/pkg/id"
	"gorm.io/gorm"
)

// AnsibleJobTemplateVariable represents a variable for an Ansible job template
// Similar to workspace variables, but scoped to job templates
type AnsibleJobTemplateVariable struct {
	ID            string             `gorm:"type:varchar(20);primary_key" json:"id"` // Format: var-{16-char-id}
	JobTemplateID uuid.UUID          `gorm:"type:uuid;not null;index;uniqueIndex:idx_job_template_key" json:"job_template_id"`
	Key           string             `gorm:"type:varchar(255);not null;uniqueIndex:idx_job_template_key" json:"key"`
	Value         string             `gorm:"type:text;not null" json:"value"`
	Description   string             `gorm:"type:text" json:"description"`                   // TFE-compatible: description field
	Category      string             `gorm:"type:varchar(50);default:'env'" json:"category"` // TFE-compatible: "terraform" or "env" (default: "env" for Ansible)
	HCL           bool               `gorm:"default:false" json:"hcl"`                       // TFE-compatible: whether value is HCL code (not used for Ansible)
	Encrypted     bool               `gorm:"default:false" json:"encrypted"`
	Sensitive     bool               `gorm:"default:false" json:"sensitive"`
	CreatedAt     time.Time          `json:"created_at"`
	UpdatedAt     time.Time          `json:"updated_at"`
	JobTemplate   AnsibleJobTemplate `gorm:"foreignKey:JobTemplateID" json:"job_template,omitempty"`
}

func (v *AnsibleJobTemplateVariable) BeforeCreate(tx *gorm.DB) error {
	if v.ID == "" {
		generatedID, err := id.GenerateVariableID()
		if err != nil {
			return err
		}
		v.ID = generatedID
	}
	return nil
}
