// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/iac-platform/backend/pkg/id"
	"gorm.io/gorm"
)

// TerraformVersion represents an available Terraform version in the platform.
// This mirrors the TFE Admin API: /api/v2/admin/terraform-versions
type TerraformVersion struct {
	ID               string    `gorm:"type:varchar(30);primary_key" json:"id"`
	Version          string    `gorm:"type:varchar(50);not null;uniqueIndex" json:"version"`
	URL              string    `gorm:"type:text" json:"url,omitempty"`
	Sha              string    `gorm:"type:varchar(128)" json:"sha,omitempty"`
	Deprecated       bool      `gorm:"default:false" json:"deprecated"`
	DeprecatedReason *string   `gorm:"type:text" json:"deprecated_reason,omitempty"`
	Official         bool      `gorm:"default:false" json:"official"`
	Enabled          bool      `gorm:"default:true" json:"enabled"`
	Beta             bool      `gorm:"default:false" json:"beta"`
	Usage            int       `gorm:"default:0" json:"usage"`
	ArchsJSON        *string   `gorm:"type:text" json:"-"` // Stored archs from user; nil = not user-specified
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func (tv *TerraformVersion) BeforeCreate(tx *gorm.DB) error {
	if tv.ID == "" {
		generatedID, err := id.Generate("tool")
		if err != nil {
			return err
		}
		tv.ID = generatedID
	}
	return nil
}

// OfficialTerraformVersions is the list of well-known Terraform versions
// that are auto-seeded when the platform starts. This is equivalent to TFE's
// built-in version catalog.
var OfficialTerraformVersions = []string{
	"1.13.0",
	"1.12.2", "1.12.1", "1.12.0",
	"1.11.4", "1.11.3", "1.11.2", "1.11.1", "1.11.0",
	"1.10.5", "1.10.4", "1.10.3", "1.10.2", "1.10.1", "1.10.0",
	"1.9.8", "1.9.7", "1.9.6", "1.9.5", "1.9.4", "1.9.3", "1.9.2", "1.9.1", "1.9.0",
	"1.8.5", "1.8.4", "1.8.3", "1.8.2", "1.8.1", "1.8.0",
	"1.7.5", "1.7.4", "1.7.3", "1.7.2", "1.7.1", "1.7.0",
	"1.6.6", "1.6.5", "1.6.4", "1.6.3", "1.6.2", "1.6.1", "1.6.0",
	"1.5.7", "1.5.6", "1.5.5", "1.5.4", "1.5.3", "1.5.2", "1.5.1", "1.5.0",
}
