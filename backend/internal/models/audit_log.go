// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"database/sql/driver"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type AuditDetails map[string]interface{}

func (a AuditDetails) Value() (driver.Value, error) {
	return json.Marshal(a)
}

func (a *AuditDetails) Scan(value interface{}) error {
	if value == nil {
		*a = make(AuditDetails)
		return nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		return nil
	}
	return json.Unmarshal(bytes, a)
}

type AuditLog struct {
	ID             uuid.UUID    `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	UserID         *uuid.UUID   `gorm:"type:uuid;index" json:"user_id,omitempty"`
	OrganizationID *uuid.UUID   `gorm:"type:uuid;index" json:"organization_id,omitempty"`
	ProjectID      *uuid.UUID   `gorm:"type:uuid" json:"project_id,omitempty"`
	WorkspaceID    *uuid.UUID   `gorm:"type:uuid;index" json:"workspace_id,omitempty"`
	Action         string       `gorm:"type:varchar(100);not null" json:"action"`
	ResourceType   string       `gorm:"type:varchar(50);not null" json:"resource_type"`
	ResourceID     *uuid.UUID   `json:"resource_id,omitempty"`
	Details        AuditDetails `gorm:"type:jsonb" json:"details,omitempty"`
	IPAddress      string       `gorm:"type:varchar(45)" json:"ip_address,omitempty"`
	UserAgent      string       `gorm:"type:text" json:"user_agent,omitempty"`
	CreatedAt      time.Time    `gorm:"index" json:"created_at"`
}

func (a *AuditLog) BeforeCreate(tx *gorm.DB) error {
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	return nil
}
