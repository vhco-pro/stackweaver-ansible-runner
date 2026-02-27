// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
)

// WebhookEvent stores received webhook deliveries for debugging and auditing.
type WebhookEvent struct {
	ID             uuid.UUID  `gorm:"type:uuid;primaryKey;default:uuid_generate_v4()" json:"id"`
	OrganizationID *uuid.UUID `gorm:"type:uuid;index" json:"organization_id,omitempty"`          // Nullable - may be unknown for some events
	EventType      string     `gorm:"type:varchar(50);not null" json:"event_type"`               // push, pull_request, ping, installation, etc.
	Provider       string     `gorm:"type:varchar(50);not null" json:"provider"`                 // github, gitlab, etc.
	Repository     string     `gorm:"type:varchar(500)" json:"repository"`                       // full repo name (e.g., owner/repo)
	Branch         string     `gorm:"type:varchar(255)" json:"branch"`                           // branch name if applicable
	Commit         string     `gorm:"type:varchar(64)" json:"commit"`                            // commit SHA if applicable
	Status         string     `gorm:"type:varchar(20);not null;default:'success'" json:"status"` // success, failed, ignored
	ResponseCode   int        `gorm:"type:int" json:"response_code"`                             // HTTP response code we returned
	Message        string     `gorm:"type:text" json:"message"`                                  // Additional info/error message
	Payload        string     `gorm:"type:text" json:"-"`                                        // Raw payload (not exposed in API)
	DeliveredAt    time.Time  `gorm:"not null" json:"delivered_at"`
	ProcessedAt    *time.Time `gorm:"" json:"processed_at"`
	CreatedAt      time.Time  `gorm:"autoCreateTime" json:"created_at"`

	// Relations (optional since OrganizationID is nullable)
	Organization *Organization `gorm:"foreignKey:OrganizationID" json:"-"`
}

func (WebhookEvent) TableName() string {
	return "webhook_events"
}
