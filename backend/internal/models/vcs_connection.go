// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type VCSProvider string

const (
	VCSProviderGitHub      VCSProvider = "github"
	VCSProviderGitLab      VCSProvider = "gitlab"
	VCSProviderBitbucket   VCSProvider = "bitbucket"
	VCSProviderAzureDevOps VCSProvider = "azure_devops"
)

type VCSConnection struct {
	ID             uuid.UUID    `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	OrganizationID uuid.UUID    `gorm:"type:uuid;not null;index" json:"organization_id"`
	Provider       VCSProvider  `gorm:"type:varchar(50);not null" json:"provider"`
	InstallationID string       `gorm:"type:varchar(255)" json:"installation_id,omitempty"` // GitHub App installation ID
	AccessToken    string       `gorm:"type:text" json:"-"`                                 // Encrypted access token (never expose)
	RefreshToken   string       `gorm:"type:text" json:"-"`                                 // Encrypted refresh token (never expose)
	TokenExpiresAt *time.Time   `gorm:"type:timestamp" json:"token_expires_at,omitempty"`
	AccountName    string       `gorm:"type:varchar(255)" json:"account_name"`          // GitHub org/user name
	AccountType    string       `gorm:"type:varchar(50)" json:"account_type"`           // "organization" or "user"
	WebhookURL     string       `gorm:"type:varchar(500)" json:"webhook_url,omitempty"` // Webhook endpoint URL
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
	Organization   Organization `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"`
}

func (v *VCSConnection) BeforeCreate(tx *gorm.DB) error {
	if v.ID == uuid.Nil {
		v.ID = uuid.New()
	}
	return nil
}

// IsExpired checks if the access token is expired
func (v *VCSConnection) IsExpired() bool {
	if v.TokenExpiresAt == nil {
		return false // No expiration set
	}
	return v.TokenExpiresAt.Before(time.Now())
}

// NeedsRefresh checks if the token needs to be refreshed
func (v *VCSConnection) NeedsRefresh() bool {
	if v.TokenExpiresAt == nil {
		return false
	}
	// Refresh if token expires within 5 minutes
	return v.TokenExpiresAt.Before(time.Now().Add(5 * time.Minute))
}
