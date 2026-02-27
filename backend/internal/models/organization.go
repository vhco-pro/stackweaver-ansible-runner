// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Organization struct {
	ID                      uuid.UUID            `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	Name                    string               `gorm:"type:varchar(255);uniqueIndex;not null" json:"name"`
	Description             string               `gorm:"type:text" json:"description"`
	Email                   string               `gorm:"type:varchar(255)" json:"email"`                                      // Admin email address (TFE-compatible)
	CollaboratorAuthPolicy  string               `gorm:"type:varchar(50);default:'password'" json:"collaborator_auth_policy"` // password or two_factor_mandatory
	CostEstimationEnabled   bool                 `gorm:"default:true" json:"cost_estimation_enabled"`                         // TFE-compatible
	DefaultTerraformVersion string               `gorm:"type:varchar(50)" json:"default_terraform_version"`                   // Org-wide default terraform version
	CreatedAt               time.Time            `json:"created_at"`
	UpdatedAt               time.Time            `json:"updated_at"`
	Members                 []OrganizationMember `gorm:"foreignKey:OrganizationID" json:"members,omitempty"`
	Projects                []Project            `gorm:"foreignKey:OrganizationID" json:"projects,omitempty"`
}

func (o *Organization) BeforeCreate(tx *gorm.DB) error {
	if o.ID == uuid.Nil {
		o.ID = uuid.New()
	}
	return nil
}
