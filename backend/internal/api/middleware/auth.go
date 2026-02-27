// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/iac-platform/backend/internal/services/auth"
)

func AuthMiddleware(authService *auth.Service) gin.HandlerFunc {
	return authService.AuthenticateMiddleware()
}
