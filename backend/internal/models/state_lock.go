// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type StateLock struct {
	ID          uuid.UUID `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	WorkspaceID string    `gorm:"type:varchar(20);not null;index;uniqueIndex:idx_workspace_lock" json:"workspace_id"` // Format: ws-{16-char-id}
	LockID      string    `gorm:"type:varchar(255);not null;uniqueIndex:idx_workspace_lock" json:"lock_id"`
	Operation   string    `gorm:"type:varchar(50);not null" json:"operation"`
	LockedBy    *string   `gorm:"type:varchar(20)" json:"locked_by,omitempty"` // Run ID: run-{16-char-id}
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `gorm:"not null" json:"expires_at"`
	Workspace   Workspace `gorm:"foreignKey:WorkspaceID" json:"workspace,omitempty"`
}

func (sl *StateLock) BeforeCreate(tx *gorm.DB) error {
	if sl.ID == uuid.Nil {
		sl.ID = uuid.New()
	}
	return nil
}

func (sl *StateLock) IsExpired() bool {
	return time.Now().After(sl.ExpiresAt)
}
