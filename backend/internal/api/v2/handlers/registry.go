// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// HandleServiceDiscovery handles the Terraform service discovery endpoint
// GET /.well-known/terraform.json
// This endpoint is used by Terraform CLI and terraform-provider-tfe to discover available services
// Based on terraform-provider-tfe source code analysis:
// - The provider checks for "tfe.v2.2" service ID (see terraform-provider-tfe/internal/client/client.go:29)
// - If not found, it returns: "host does not support tfe version v2.2"
// CLI checks for
// - tfe.v2: Terraform Enterprise API v2 (base version)
// - tfe.v2.1: Terraform Enterprise API v2.1 (specific version required by newer Terraform CLI)
// - modules.v1: Module registry service
// - providers.v1: Provider registry service
func HandleServiceDiscovery(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"tfe.v2":       "/api/v2/",       // Terraform Enterprise API v2 (base version)
		"tfe.v2.1":     "/api/v2/",       // Terraform Enterprise API v2.1 (required by newer Terraform CLI)
		"tfe.v2.2":     "/api/v2/",       // Terraform Enterprise API v2.2 (required by terraform-provider-tfe)
		"modules.v1":   "/v1/modules/",   // Module registry service
		"providers.v1": "/v1/providers/", // Provider registry service
	})
}
