// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type WebhookEventRepository struct {
	db *gorm.DB
}

func NewWebhookEventRepository(db *gorm.DB) *WebhookEventRepository {
	return &WebhookEventRepository{db: db}
}

func (r *WebhookEventRepository) Create(event *models.WebhookEvent) error {
	return r.db.Create(event).Error
}

func (r *WebhookEventRepository) GetByID(id uuid.UUID) (*models.WebhookEvent, error) {
	var event models.WebhookEvent
	err := r.db.First(&event, "id = ?", id).Error
	return &event, err
}

// ListByOrganization lists recent webhook events for an organization
func (r *WebhookEventRepository) ListByOrganization(orgID uuid.UUID, limit, offset int) ([]models.WebhookEvent, int64, error) {
	var events []models.WebhookEvent
	var total int64

	if err := r.db.Model(&models.WebhookEvent{}).Where("organization_id = ?", orgID).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("organization_id = ?", orgID).
		Order("delivered_at DESC").
		Limit(limit).
		Offset(offset).
		Find(&events).Error

	return events, total, err
}

// ListRecent lists the most recent webhook events across all organizations (for admin)
func (r *WebhookEventRepository) ListRecent(limit int) ([]models.WebhookEvent, error) {
	var events []models.WebhookEvent
	err := r.db.Order("delivered_at DESC").
		Limit(limit).
		Find(&events).Error
	return events, err
}

// DeleteOlderThan deletes events older than a specified time (for cleanup)
func (r *WebhookEventRepository) DeleteOlderThan(before string) (int64, error) {
	result := r.db.Where("delivered_at < ?", before).Delete(&models.WebhookEvent{})
	return result.RowsAffected, result.Error
}
