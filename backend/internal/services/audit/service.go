// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package audit

import (
	"context"
	"net/http"
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

func (s *Service) Log(ctx context.Context, entry *models.AuditLog) error {
	return s.auditRepo.Create(entry)
}

func (s *Service) LogAction(
	ctx context.Context,
	userID *uuid.UUID,
	action string,
	resourceType string,
	resourceID *uuid.UUID,
	organizationID *uuid.UUID,
	projectID *uuid.UUID,
	workspaceID *uuid.UUID,
	details map[string]interface{},
	req *http.Request,
) error {
	entry := &models.AuditLog{
		UserID:         userID,
		OrganizationID: organizationID,
		ProjectID:      projectID,
		WorkspaceID:    workspaceID,
		Action:         action,
		ResourceType:   resourceType,
		ResourceID:     resourceID,
		Details:        models.AuditDetails(details),
		IPAddress:      getClientIP(req),
		UserAgent:      req.UserAgent(),
		CreatedAt:      time.Now(),
	}

	return s.Log(ctx, entry)
}

func getClientIP(req *http.Request) string {
	ip := req.Header.Get("X-Forwarded-For")
	if ip == "" {
		ip = req.Header.Get("X-Real-IP")
	}
	if ip == "" {
		ip = req.RemoteAddr
	}
	return ip
}
