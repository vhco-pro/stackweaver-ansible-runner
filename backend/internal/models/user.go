// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type User struct {
	ID             uuid.UUID `gorm:"type:uuid;primary_key" json:"id"`
	ZitadelSubject string    `gorm:"type:varchar(255);uniqueIndex;not null" json:"zitadel_subject"`
	Email          string    `gorm:"type:varchar(255);uniqueIndex" json:"email"` // Optional - may not be in JWT claims
	Name           string    `gorm:"type:varchar(255)" json:"name"`              // Optional - may not be in JWT claims
	Username       string    `gorm:"type:varchar(255)" json:"username"`          // Profile username
	Bio            string    `gorm:"type:text" json:"bio"`                       // Profile bio
	Company        string    `gorm:"type:varchar(255)" json:"company"`           // Company name
	Location       string    `gorm:"type:varchar(255)" json:"location"`          // Location
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (u *User) BeforeCreate(tx *gorm.DB) error {
	if u.ID == uuid.Nil {
		u.ID = uuid.New()
	}
	return nil
}
