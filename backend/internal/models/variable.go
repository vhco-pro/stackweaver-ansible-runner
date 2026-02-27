// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/iac-platform/backend/pkg/id"
	"gorm.io/gorm"
)

type Variable struct {
	ID          string    `gorm:"type:varchar(20);primary_key" json:"id"`                                            // Format: var-{16-char-id}
	WorkspaceID string    `gorm:"type:varchar(20);not null;index;uniqueIndex:idx_workspace_key" json:"workspace_id"` // Format: ws-{16-char-id}
	Key         string    `gorm:"type:varchar(255);not null;uniqueIndex:idx_workspace_key" json:"key"`
	Value       string    `gorm:"type:text;not null" json:"value"`
	Description string    `gorm:"type:text" json:"description"`                         // TFE-compatible: description field
	Category    string    `gorm:"type:varchar(50);default:'terraform'" json:"category"` // TFE-compatible: "terraform" or "env"
	HCL         bool      `gorm:"default:false" json:"hcl"`                             // TFE-compatible: whether value is HCL code
	Encrypted   bool      `gorm:"default:false" json:"encrypted"`
	Sensitive   bool      `gorm:"default:false" json:"sensitive"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Workspace   Workspace `gorm:"foreignKey:WorkspaceID" json:"workspace,omitempty"`
}

func (v *Variable) BeforeCreate(tx *gorm.DB) error {
	if v.ID == "" {
		generatedID, err := id.GenerateVariableID()
		if err != nil {
			return err
		}
		v.ID = generatedID
	}
	return nil
}
