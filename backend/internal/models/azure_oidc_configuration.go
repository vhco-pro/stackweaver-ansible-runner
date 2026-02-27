// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/pkg/id"
	"gorm.io/gorm"
)

// AzureOIDCConfiguration represents an Azure OIDC configuration for keyless authentication.
// TFE-compatible: matches go-tfe/azure_oidc_configuration.go
// JSON:API type: "azure-oidc-configurations"
// ID format: azoidc-{16-char-alphanumeric}
type AzureOIDCConfiguration struct {
	ID             string        `gorm:"type:varchar(24);primary_key" json:"id"`                  // Format: azoidc-{16-char-id}
	ClientID       string        `gorm:"type:varchar(255);not null" json:"client_id"`             // Azure Entra ID application/client ID
	SubscriptionID string        `gorm:"type:varchar(255);not null" json:"subscription_id"`       // Azure subscription ID
	TenantID       string        `gorm:"type:varchar(255);not null" json:"tenant_id"`             // Azure Entra ID tenant/directory ID
	OrganizationID uuid.UUID     `gorm:"type:uuid;not null;index" json:"organization_id"`         // Foreign key to organizations
	Organization   *Organization `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"` // GORM relationship for preloading
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
}

// BeforeCreate auto-generates the azoidc- prefixed ID if not already set.
func (a *AzureOIDCConfiguration) BeforeCreate(tx *gorm.DB) error {
	if a.ID == "" {
		generatedID, err := id.GenerateAzureOIDCConfigID()
		if err != nil {
			return err
		}
		a.ID = generatedID
	}
	return nil
}
