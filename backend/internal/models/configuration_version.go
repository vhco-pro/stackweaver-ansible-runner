// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"crypto/rand"
	"encoding/base64"
	"time"

	"github.com/iac-platform/backend/pkg/id"
	"gorm.io/gorm"
)

type ConfigurationVersionStatus string

const (
	ConfigurationVersionStatusPending  ConfigurationVersionStatus = "pending"
	ConfigurationVersionStatusUploaded ConfigurationVersionStatus = "uploaded"
	ConfigurationVersionStatusErrored  ConfigurationVersionStatus = "errored"
)

type ConfigurationVersion struct {
	ID            string                     `gorm:"type:varchar(20);primary_key" json:"id"`              // Format: cv-{16-char-id}
	WorkspaceID   string                     `gorm:"type:varchar(20);not null;index" json:"workspace_id"` // Format: ws-{16-char-id}
	Status        ConfigurationVersionStatus `gorm:"type:varchar(50);not null;default:'pending';index" json:"status"`
	UploadURL     string                     `gorm:"type:text" json:"upload_url,omitempty"`
	UploadToken   string                     `gorm:"type:varchar(255);index" json:"-"`                 // Temporary token for upload (not exposed in JSON)
	Source        string                     `gorm:"type:varchar(50);default:'tfe-api'" json:"source"` // "tfe-api", "tfe-ui", "tfe-cli", "tfe-vcs" (indicates how config version was created)
	AutoQueueRuns bool                       `gorm:"default:false" json:"auto_queue_runs"`
	Speculative   bool                       `gorm:"default:false" json:"speculative"`
	CommitHash    string                     `gorm:"type:varchar(255)" json:"commit_hash,omitempty"`   // Git commit hash (for VCS-triggered runs)
	Committer     string                     `gorm:"type:varchar(255)" json:"committer,omitempty"`     // Committer email/name (for VCS-triggered runs)
	PRNumber      int                        `gorm:"default:0" json:"pr_number,omitempty"`             // Pull request number (for PR-triggered speculative runs)
	SourceBranch  string                     `gorm:"type:varchar(255)" json:"source_branch,omitempty"` // Source/head branch of the PR (for PR-triggered speculative runs)
	ErrorMessage  string                     `gorm:"type:text" json:"error_message,omitempty"`
	CreatedAt     time.Time                  `json:"created_at"`
	UpdatedAt     time.Time                  `json:"updated_at"`
	Workspace     Workspace                  `gorm:"foreignKey:WorkspaceID" json:"workspace,omitempty"`
}

func (cv *ConfigurationVersion) BeforeCreate(tx *gorm.DB) error {
	if cv.ID == "" {
		generatedID, err := id.GenerateConfigurationVersionID()
		if err != nil {
			return err
		}
		cv.ID = generatedID
	}
	// Generate upload token if not set
	if cv.UploadToken == "" {
		token, err := generateUploadToken()
		if err != nil {
			return err
		}
		cv.UploadToken = token
	}
	return nil
}

// generateUploadToken generates a secure random token for upload authentication
func generateUploadToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(bytes), nil
}
