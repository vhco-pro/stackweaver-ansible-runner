// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/totp"
)

type TOTPHandler struct {
	totpService *totp.Service
	authService *auth.Service
}

func NewTOTPHandler(totpService *totp.Service, authService *auth.Service) *TOTPHandler {
	return &TOTPHandler{
		totpService: totpService,
		authService: authService,
	}
}

// StartTOTPRegistration starts TOTP registration
// POST /api/v2/settings/2fa/start
func (h *TOTPHandler) StartTOTPRegistration(c *gin.Context) {
	// Get user's Zitadel subject from context
	userSubject, err := h.authService.GetUserSubject(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	// Start TOTP registration
	resp, err := h.totpService.StartTOTPRegistration(userSubject)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start TOTP registration", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"secret": resp.Secret,
		"url":    resp.URL,
	})
}

// VerifyTOTP verifies a TOTP code
// POST /api/v2/settings/2fa/verify
type VerifyTOTPRequest struct {
	Code string `json:"code" binding:"required"`
}

func (h *TOTPHandler) VerifyTOTP(c *gin.Context) {
	var req VerifyTOTPRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Get user's Zitadel subject from context
	userSubject, err := h.authService.GetUserSubject(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	// Verify TOTP code
	if err := h.totpService.VerifyTOTP(userSubject, req.Code); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid TOTP code", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "TOTP verified successfully"})
}

// GetTOTPStatus gets the TOTP status for the current user
// GET /api/v2/settings/2fa/status
func (h *TOTPHandler) GetTOTPStatus(c *gin.Context) {
	// Get user's Zitadel subject from context
	userSubject, err := h.authService.GetUserSubject(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	// Check TOTP status
	enabled, err := h.totpService.CheckTOTPStatus(userSubject)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to check TOTP status", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"enabled": enabled})
}

// RemoveTOTP removes TOTP from the user
// DELETE /api/v2/settings/2fa
func (h *TOTPHandler) RemoveTOTP(c *gin.Context) {
	// Get user's Zitadel subject from context
	userSubject, err := h.authService.GetUserSubject(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	// Remove TOTP
	if err := h.totpService.RemoveTOTP(userSubject); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to remove TOTP", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "TOTP removed successfully"})
}

// ChangePassword changes the user's password
// POST /api/v1/settings/password
type ChangePasswordRequest struct {
	CurrentPassword string `json:"current_password" binding:"required"`
	NewPassword     string `json:"new_password" binding:"required"`
}

func (h *TOTPHandler) ChangePassword(c *gin.Context) {
	var req ChangePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Get user's Zitadel subject from context
	userSubject, err := h.authService.GetUserSubject(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	// Change password
	if err := h.totpService.ChangePassword(userSubject, req.CurrentPassword, req.NewPassword); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to change password", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Password changed successfully"})
}

// ListMFADevices lists all MFA devices for the current user
// GET /api/v1/settings/mfa-devices
func (h *TOTPHandler) ListMFADevices(c *gin.Context) {
	// Get user's Zitadel subject from context
	userSubject, err := h.authService.GetUserSubject(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	// List MFA devices
	devices, err := h.totpService.ListMFADevices(userSubject)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list MFA devices", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"devices": devices})
}
