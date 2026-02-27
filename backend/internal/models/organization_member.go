// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type OrganizationMember struct {
	ID             uuid.UUID    `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	OrganizationID uuid.UUID    `gorm:"type:uuid;not null;uniqueIndex:idx_org_user" json:"organization_id"`
	UserID         uuid.UUID    `gorm:"type:uuid;not null;uniqueIndex:idx_org_user" json:"user_id"`
	Role           *string      `gorm:"type:varchar(50)" json:"role,omitempty"` // Deprecated: Nullable, will be removed. Use team memberships instead.
	CreatedAt      time.Time    `json:"created_at"`
	Organization   Organization `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"`
	User           User         `gorm:"foreignKey:UserID" json:"user,omitempty"`
}

func (om *OrganizationMember) BeforeCreate(tx *gorm.DB) error {
	if om.ID == uuid.Nil {
		om.ID = uuid.New()
	}
	return nil
}
