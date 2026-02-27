// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package activity

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
)

type Service struct {
	auditRepo *repository.AuditLogRepository
}

func NewService(auditRepo *repository.AuditLogRepository) *Service {
	return &Service{
		auditRepo: auditRepo,
	}
}

// ActivityContext contains information about the activity
type ActivityContext struct {
	UserID         *uuid.UUID
	OrganizationID *uuid.UUID
	ProjectID      *uuid.UUID
	WorkspaceID    *string // Now uses prefixed string IDs (ws-...)
	IPAddress      string
	UserAgent      string
}

// LogActivity logs an activity/audit event
func (s *Service) LogActivity(ctx context.Context, action, resourceType string, resourceID *uuid.UUID, details map[string]interface{}, activityCtx ActivityContext) error {
	// Store workspace ID in details if provided (since audit log WorkspaceID is UUID)
	detailsWithWorkspace := details
	if activityCtx.WorkspaceID != nil {
		detailsWithWorkspace = make(map[string]interface{})
		for k, v := range details {
			detailsWithWorkspace[k] = v
		}
		detailsWithWorkspace["workspace_id"] = *activityCtx.WorkspaceID
	}

	auditLog := &models.AuditLog{
		UserID:         activityCtx.UserID,
		OrganizationID: activityCtx.OrganizationID,
		ProjectID:      activityCtx.ProjectID,
		WorkspaceID:    nil, // WorkspaceID is now string, store in details instead
		Action:         action,
		ResourceType:   resourceType,
		ResourceID:     resourceID,
		Details:        models.AuditDetails(detailsWithWorkspace),
		IPAddress:      activityCtx.IPAddress,
		UserAgent:      activityCtx.UserAgent,
		CreatedAt:      time.Now(),
	}

	return s.auditRepo.Create(auditLog)
}

// GetRecentActivities gets recent activities with optional filters
func (s *Service) GetRecentActivities(userID *uuid.UUID, organizationID *uuid.UUID, limit int) ([]models.AuditLog, error) {
	filters := repository.AuditLogFilters{
		UserID:         userID,
		OrganizationID: organizationID,
	}

	activities, _, err := s.auditRepo.List(filters, limit, 0)
	return activities, err
}

// GetActivities gets activities with filters
func (s *Service) GetActivities(filters repository.AuditLogFilters, limit, offset int) ([]models.AuditLog, int64, error) {
	return s.auditRepo.List(filters, limit, offset)
}

// Helper functions for common activities
func (s *Service) LogCreate(ctx context.Context, resourceType string, resourceID string, resourceName string, activityCtx ActivityContext) error {
	// Store string ID in details since audit log resource_id is UUID
	// For prefixed IDs (ws-, run-, sv-, var-, varset-), we store the full ID in details
	details := map[string]interface{}{
		"resource_name": resourceName,
		"resource_id":   resourceID, // Store the actual prefixed ID
	}
	return s.LogActivity(ctx, "create", resourceType, nil, details, activityCtx)
}

func (s *Service) LogUpdate(ctx context.Context, resourceType string, resourceID string, resourceName string, changes map[string]interface{}, activityCtx ActivityContext) error {
	details := map[string]interface{}{
		"resource_name": resourceName,
		"resource_id":   resourceID, // Store the actual prefixed ID
	}
	for k, v := range changes {
		details[k] = v
	}
	return s.LogActivity(ctx, "update", resourceType, nil, details, activityCtx)
}

func (s *Service) LogDelete(ctx context.Context, resourceType string, resourceID string, resourceName string, activityCtx ActivityContext) error {
	details := map[string]interface{}{
		"resource_name": resourceName,
		"resource_id":   resourceID, // Store the actual prefixed ID
	}
	return s.LogActivity(ctx, "delete", resourceType, nil, details, activityCtx)
}

func (s *Service) LogRun(ctx context.Context, workspaceID uuid.UUID, runID uuid.UUID, operation, status string, activityCtx ActivityContext) error {
	return s.LogActivity(ctx, fmt.Sprintf("run_%s", operation), "run", &runID, map[string]interface{}{
		"workspace_id": workspaceID.String(),
		"status":       status,
		"operation":    operation,
	}, activityCtx)
}

func (s *Service) LogAPIKeyCreate(ctx context.Context, apiKeyID uuid.UUID, keyName string, activityCtx ActivityContext) error {
	return s.LogActivity(ctx, "create", "api_key", &apiKeyID, map[string]interface{}{
		"key_name": keyName,
	}, activityCtx)
}

func (s *Service) LogAPIKeyDelete(ctx context.Context, apiKeyID uuid.UUID, keyName string, activityCtx ActivityContext) error {
	return s.LogActivity(ctx, "delete", "api_key", &apiKeyID, map[string]interface{}{
		"key_name": keyName,
	}, activityCtx)
}
