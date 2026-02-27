// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/sessions"
)

type SessionsHandler struct {
	sessionsService *sessions.Service
	authService     *auth.Service
}

func NewSessionsHandler(sessionsService *sessions.Service, authService *auth.Service) *SessionsHandler {
	return &SessionsHandler{
		sessionsService: sessionsService,
		authService:     authService,
	}
}

// ListSessions lists all active sessions for the current user
// GET /api/v2/settings/sessions
func (h *SessionsHandler) ListSessions(c *gin.Context) {
	if h.sessionsService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "sessions service is not available"})
		return
	}

	// Get user's Zitadel subject from context
	userSubject, err := h.authService.GetUserSubject(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized", "details": err.Error()})
		return
	}

	if userSubject == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user subject is missing"})
		return
	}

	// List sessions
	sessionList, err := h.sessionsService.ListUserSessions(userSubject)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list sessions", "details": err.Error()})
		return
	}

	// Identify current session by comparing request metadata
	// Get current request's IP address
	currentIP := c.ClientIP()

	// Find the session that best matches the current request
	type SessionWithCurrent struct {
		*sessions.Session
		IsCurrent bool `json:"is_current"`
	}

	sessionsWithCurrent := make([]SessionWithCurrent, len(sessionList))

	// First, try to match by IP address (most reliable)
	matchedByIP := false
	for i, session := range sessionList {
		isCurrent := false

		// Match by IP address if available
		if currentIP != "" && session.IPAddress != "" && session.IPAddress == currentIP {
			isCurrent = true
			matchedByIP = true
		}

		sessionsWithCurrent[i] = SessionWithCurrent{
			Session:   session,
			IsCurrent: isCurrent,
		}
	}

	// If no match by IP, mark the most recent session as current (fallback)
	if !matchedByIP && len(sessionsWithCurrent) > 0 {
		sessionsWithCurrent[0].IsCurrent = true
	}

	c.JSON(http.StatusOK, gin.H{"sessions": sessionsWithCurrent})
}

// RevokeSession revokes a session
// DELETE /api/v2/settings/sessions/:sessionId
func (h *SessionsHandler) RevokeSession(c *gin.Context) {
	if h.sessionsService == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "sessions service is not available"})
		return
	}

	sessionID := c.Param("sessionId")
	if sessionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session ID is required"})
		return
	}

	// Get user's Zitadel subject from context
	userSubject, err := h.authService.GetUserSubject(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized", "details": err.Error()})
		return
	}

	if userSubject == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user subject is missing"})
		return
	}

	// Optional: Verify the session belongs to the user before revoking
	// This adds an extra security layer - we can list the user's sessions and check if the session ID exists
	sessions, err := h.sessionsService.ListUserSessions(userSubject)
	if err == nil {
		sessionExists := false
		for _, session := range sessions {
			if session.ID == sessionID {
				sessionExists = true
				break
			}
		}
		if !sessionExists {
			c.JSON(http.StatusForbidden, gin.H{"error": "session does not belong to user"})
			return
		}
	}

	// Revoke session
	if err := h.sessionsService.RevokeSession(sessionID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to revoke session", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Session revoked successfully"})
}
