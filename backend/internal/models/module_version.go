// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type ModuleVersion struct {
	ID          uuid.UUID `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	ModuleID    uuid.UUID `gorm:"type:uuid;not null;index;uniqueIndex:idx_module_version" json:"module_id"`
	Version     string    `gorm:"type:varchar(50);not null;uniqueIndex:idx_module_version" json:"version"` // Semantic version
	Source      string    `gorm:"type:varchar(500)" json:"source"`                                         // Git tag/commit or tarball path
	Readme      string    `gorm:"type:text" json:"readme"`                                                 // README content
	PublishedAt time.Time `json:"published_at"`
	Downloads   int       `gorm:"default:0" json:"downloads"`

	// Module metadata (parsed from Terraform files)
	Inputs       JSONB `gorm:"type:jsonb" json:"inputs,omitempty"`       // Array of input definitions
	Outputs      JSONB `gorm:"type:jsonb" json:"outputs,omitempty"`      // Array of output definitions
	Dependencies JSONB `gorm:"type:jsonb" json:"dependencies,omitempty"` // Array of required providers
	Resources    JSONB `gorm:"type:jsonb" json:"resources,omitempty"`    // Array of resource types used
	Submodules   JSONB `gorm:"type:jsonb" json:"submodules,omitempty"`   // Array of submodule paths

	// Storage
	TarballPath string `gorm:"type:varchar(500)" json:"tarball_path"` // MinIO path
	TarballSize int64  `json:"tarball_size"`                          // Size in bytes

	// Relationships
	Module Module `gorm:"foreignKey:ModuleID" json:"module,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (mv *ModuleVersion) BeforeCreate(tx *gorm.DB) error {
	if mv.ID == uuid.Nil {
		mv.ID = uuid.New()
	}
	if mv.PublishedAt.IsZero() {
		mv.PublishedAt = time.Now()
	}
	return nil
}

// TableName specifies the table name for GORM
func (ModuleVersion) TableName() string {
	return "module_versions"
}
