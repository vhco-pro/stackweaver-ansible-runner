// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"time"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type AuditLogRepository struct {
	db *gorm.DB
}

func NewAuditLogRepository(db *gorm.DB) *AuditLogRepository {
	return &AuditLogRepository{db: db}
}

func (r *AuditLogRepository) Create(log *models.AuditLog) error {
	return r.db.Create(log).Error
}

func (r *AuditLogRepository) List(filters AuditLogFilters, limit, offset int) ([]models.AuditLog, int64, error) {
	var logs []models.AuditLog
	var total int64

	query := r.db.Model(&models.AuditLog{})

	if filters.UserID != nil {
		query = query.Where("user_id = ?", *filters.UserID)
	}
	if filters.OrganizationID != nil {
		query = query.Where("organization_id = ?", *filters.OrganizationID)
	}
	if filters.WorkspaceID != nil {
		query = query.Where("workspace_id = ?", *filters.WorkspaceID)
	}
	if filters.Action != "" {
		query = query.Where("action = ?", filters.Action)
	}
	if filters.ResourceType != "" {
		query = query.Where("resource_type = ?", filters.ResourceType)
	}
	if !filters.StartTime.IsZero() {
		query = query.Where("created_at >= ?", filters.StartTime)
	}
	if !filters.EndTime.IsZero() {
		query = query.Where("created_at <= ?", filters.EndTime)
	}

	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := query.Order("created_at DESC").Limit(limit).Offset(offset).Find(&logs).Error
	return logs, total, err
}

type AuditLogFilters struct {
	UserID         *uuid.UUID
	OrganizationID *uuid.UUID
	WorkspaceID    *string // Now uses prefixed string IDs (ws-...)
	Action         string
	ResourceType   string
	StartTime      time.Time
	EndTime        time.Time
}
