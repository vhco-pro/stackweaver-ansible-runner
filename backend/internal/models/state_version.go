// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"database/sql/driver"
	"encoding/json"
	"time"

	"github.com/iac-platform/backend/pkg/id"
	"gorm.io/gorm"
)

type StateData map[string]interface{}

func (s StateData) Value() (driver.Value, error) {
	return json.Marshal(s)
}

func (s *StateData) Scan(value interface{}) error {
	if value == nil {
		*s = make(StateData)
		return nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		return nil
	}
	return json.Unmarshal(bytes, s)
}

type StateVersion struct {
	ID          string    `gorm:"type:varchar(20);primary_key" json:"id"`                                                // Format: sv-{16-char-id}
	WorkspaceID string    `gorm:"type:varchar(20);not null;index;uniqueIndex:idx_workspace_version" json:"workspace_id"` // Format: ws-{16-char-id}
	RunID       *string   `gorm:"type:varchar(20);index" json:"run_id,omitempty"`                                        // Format: run-{16-char-id}
	Version     int       `gorm:"not null;uniqueIndex:idx_workspace_version" json:"version"`
	StateData   StateData `gorm:"type:jsonb" json:"state_data,omitempty"` // Optional - actual state stored in MinIO
	Serial      *int      `json:"serial,omitempty"`
	Lineage     string    `gorm:"type:varchar(255)" json:"lineage,omitempty"`
	CommitHash  string    `gorm:"type:varchar(255)" json:"commit_hash,omitempty"` // Git commit hash that triggered this state version
	Committer   string    `gorm:"type:varchar(255)" json:"committer,omitempty"`   // Committer email/name
	CreatedAt   time.Time `json:"created_at"`
	Workspace   Workspace `gorm:"foreignKey:WorkspaceID" json:"workspace,omitempty"`
	Run         *Run      `gorm:"foreignKey:RunID" json:"run,omitempty"`
}

func (sv *StateVersion) BeforeCreate(tx *gorm.DB) error {
	if sv.ID == "" {
		generatedID, err := id.GenerateStateVersionID()
		if err != nil {
			return err
		}
		sv.ID = generatedID
	}
	return nil
}
