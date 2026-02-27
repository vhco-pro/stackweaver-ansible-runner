// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// TeamMember represents a user membership in a team (many-to-many)
type TeamMember struct {
	ID        uuid.UUID `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	TeamID    uuid.UUID `gorm:"type:uuid;not null;uniqueIndex:idx_team_user" json:"team_id"`
	UserID    uuid.UUID `gorm:"type:uuid;not null;uniqueIndex:idx_team_user" json:"user_id"`
	CreatedAt time.Time `json:"created_at"`

	// Relationships
	Team Team `gorm:"foreignKey:TeamID" json:"team,omitempty"`
	User User `gorm:"foreignKey:UserID" json:"user,omitempty"`
}

func (tm *TeamMember) BeforeCreate(tx *gorm.DB) error {
	if tm.ID == uuid.Nil {
		tm.ID = uuid.New()
	}
	return nil
}
