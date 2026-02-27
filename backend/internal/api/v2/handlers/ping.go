// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Ping handles the ping endpoint for health checks
// GET /api/v2/ping
// TFE-compatible endpoint - returns simple "pong" response with version headers
//
// Based on go-tfe library source code analysis (go-tfe/tfe.go):
// - The go-tfe client calls this endpoint during initialization (getRawAPIMetadata)
// - It reads version information from response headers:
//   - TFP-API-Version: API version (e.g., "2.2")
//   - X-TFE-Version: TFE monthly version (e.g., "202205-1")
//   - X-TFE-Current-Version: TFE numeric version (e.g., "1.1.0")
//   - X-RateLimit-Limit: Rate limit header
//   - TFP-AppName: Application name ("HCP Terraform" or "Terraform Enterprise")
//
// Note: TFE System API uses /api/v1/ping, but Terraform remote backend and go-tfe call /api/v2/ping
func Ping(c *gin.Context) {
	// Set required version headers for TFE compatibility
	// These headers are read by go-tfe client during initialization
	c.Header("TFP-API-Version", "2.2")              // API version - required by terraform-provider-tfe
	c.Header("X-TFE-Version", "202501-1")           // TFE monthly version (format: YYYYMM-N)
	c.Header("X-TFE-Current-Version", "1.0.0")      // TFE numeric version
	c.Header("TFP-AppName", "Terraform Enterprise") // Application name
	// X-RateLimit-Limit is optional and can be set by rate limiting middleware

	// TFE returns plain text "pong" for this endpoint
	c.String(http.StatusOK, "pong")
}
