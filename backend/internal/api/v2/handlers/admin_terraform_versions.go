// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/michielvha/logger"
	"gorm.io/gorm"
)

// AdminTerraformVersionsHandler handles the /admin/terraform-versions endpoints.
// Access restricted to users in an "owners" team (site admins).
// TFE API docs: https://developer.hashicorp.com/terraform/enterprise/api-docs/admin/terraform-versions
type AdminTerraformVersionsHandler struct {
	db          *gorm.DB
	authService *auth.Service
}

// NewAdminTerraformVersionsHandler creates a new handler.
func NewAdminTerraformVersionsHandler(db *gorm.DB, authService *auth.Service) *AdminTerraformVersionsHandler {
	return &AdminTerraformVersionsHandler{db: db, authService: authService}
}

// requireAdmin checks that the authenticated user is in an "owners" team.
// Returns false and sends a 404 (matching TFE behavior) if not authorized.
func (h *AdminTerraformVersionsHandler) requireAdmin(c *gin.Context) bool {
	user, err := h.authService.GetUserFromContext(c)
	if err != nil || user == nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found"}}})
		return false
	}

	// Check if user is in "owners" team of any organization using GORM models
	var count int64
	h.db.Model(&models.TeamMember{}).
		Joins("JOIN teams ON teams.id = team_members.team_id").
		Where("team_members.user_id = ? AND teams.name = ?", user.ID, "owners").
		Count(&count)

	if count == 0 {
		logger.Warnf("Admin terraform versions: User %s (%s) denied - not in any owners team", user.Email, user.ID)
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found"}}})
		return false
	}
	return true
}

// List all terraform versions.
// GET /api/v2/admin/terraform-versions
func (h *AdminTerraformVersionsHandler) List(c *gin.Context) {
	if !h.requireAdmin(c) {
		return
	}

	var versions []models.TerraformVersion

	// Order by semantic version: split into major.minor.patch numeric parts for proper sorting
	// e.g. 1.13.0 > 1.9.8 > 1.5.0 (not lexicographic where 1.9 > 1.13)
	query := h.db.Model(&models.TerraformVersion{}).Order(`
		CAST(split_part(regexp_replace(version, '-.*$', ''), '.', 1) AS INTEGER) DESC,
		CAST(split_part(regexp_replace(version, '-.*$', ''), '.', 2) AS INTEGER) DESC,
		CAST(split_part(regexp_replace(version, '-.*$', ''), '.', 3) AS INTEGER) DESC,
		version DESC
	`)

	// filter[version] - exact match (takes precedence)
	if filter := c.Query("filter[version]"); filter != "" {
		query = query.Where("version = ?", filter)
	} else if search := c.Query("search[version]"); search != "" {
		// search[version] - partial match
		query = query.Where("version LIKE ?", "%"+search+"%")
	}

	// Only show enabled versions unless admin explicitly asks
	// (TFE shows all but we default to enabled for usability)

	// Pagination
	page := 1
	pageSize := 20
	if p := c.Query("page[number]"); p != "" {
		if _, err := fmt.Sscanf(p, "%d", &page); err != nil || page < 1 {
			page = 1
		}
	}
	if ps := c.Query("page[size]"); ps != "" {
		if _, err := fmt.Sscanf(ps, "%d", &pageSize); err != nil || pageSize < 1 {
			pageSize = 20
		}
		if pageSize > 100 {
			pageSize = 100
		}
	}

	var totalCount int64
	query.Count(&totalCount)

	offset := (page - 1) * pageSize
	query.Offset(offset).Limit(pageSize).Find(&versions)

	totalPages := int(totalCount) / pageSize
	if int(totalCount)%pageSize > 0 {
		totalPages++
	}

	// Format as JSON:API
	data := make([]gin.H, 0, len(versions))
	for _, v := range versions {
		data = append(data, formatTerraformVersion(&v))
	}

	c.JSON(http.StatusOK, gin.H{
		"data": data,
		"meta": gin.H{
			"pagination": gin.H{
				"current-page": page,
				"page-size":    pageSize,
				"total-pages":  totalPages,
				"total-count":  totalCount,
			},
		},
	})
}

// Read a single terraform version by ID.
// GET /api/v2/admin/terraform-versions/:id
func (h *AdminTerraformVersionsHandler) Read(c *gin.Context) {
	if !h.requireAdmin(c) {
		return
	}

	id := c.Param("id")

	var version models.TerraformVersion
	if err := h.db.First(&version, "id = ?", id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Terraform version not found"}}})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": formatTerraformVersion(&version)})
}

type createTerraformVersionRequest struct {
	Data struct {
		Type       string `json:"type"`
		Attributes struct {
			Version          string                 `json:"version"`
			URL              string                 `json:"url,omitempty"`
			Sha              string                 `json:"sha,omitempty"`
			Deprecated       *bool                  `json:"deprecated,omitempty"`
			DeprecatedReason *string                `json:"deprecated-reason,omitempty"`
			Official         *bool                  `json:"official,omitempty"`
			Enabled          *bool                  `json:"enabled,omitempty"`
			Beta             *bool                  `json:"beta,omitempty"`
			Archs            []terraformVersionArch `json:"archs,omitempty"`
		} `json:"attributes"`
	} `json:"data"`
}

type terraformVersionArch struct {
	URL  string `json:"url"`
	Sha  string `json:"sha"`
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// Create a terraform version.
// POST /api/v2/admin/terraform-versions
func (h *AdminTerraformVersionsHandler) Create(c *gin.Context) {
	if !h.requireAdmin(c) {
		return
	}

	var req createTerraformVersionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"errors": []gin.H{{"status": "422", "title": "Invalid request", "detail": err.Error()}}})
		return
	}

	if req.Data.Attributes.Version == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"errors": []gin.H{{"status": "422", "title": "version is required"}}})
		return
	}

	// Check uniqueness
	var existing models.TerraformVersion
	if err := h.db.Where("version = ?", req.Data.Attributes.Version).First(&existing).Error; err == nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"errors": []gin.H{{"status": "422", "title": "Version already exists", "detail": fmt.Sprintf("Terraform version %s already exists", req.Data.Attributes.Version)}}})
		return
	}

	// Determine URL and SHA from archs or top-level fields
	url, sha := req.Data.Attributes.URL, req.Data.Attributes.Sha
	validArchs := filterValidArchs(req.Data.Attributes.Archs)
	if len(validArchs) > 0 {
		// Use first matching arch for linux/amd64
		for _, arch := range validArchs {
			if arch.OS == "linux" && arch.Arch == runtime.GOARCH {
				url = arch.URL
				sha = arch.Sha
				break
			}
		}
		// Fallback to first arch if no matching one
		if url == "" && len(validArchs) > 0 {
			url = validArchs[0].URL
			sha = validArchs[0].Sha
		}
	}

	// Auto-generate URL if not provided
	if url == "" {
		url = fmt.Sprintf("https://releases.hashicorp.com/terraform/%s/terraform_%s_linux_amd64.zip", req.Data.Attributes.Version, req.Data.Attributes.Version)
	}

	version := &models.TerraformVersion{
		Version:   req.Data.Attributes.Version,
		URL:       url,
		Sha:       sha,
		Official:  derefBool(req.Data.Attributes.Official, false),
		Enabled:   derefBool(req.Data.Attributes.Enabled, true),
		Beta:      derefBool(req.Data.Attributes.Beta, false),
		ArchsJSON: marshalArchs(validArchs),
	}

	if req.Data.Attributes.Deprecated != nil {
		version.Deprecated = *req.Data.Attributes.Deprecated
	}
	// Only store non-empty deprecated reasons (provider sends "" even when user doesn't set it)
	if req.Data.Attributes.DeprecatedReason != nil && *req.Data.Attributes.DeprecatedReason != "" {
		version.DeprecatedReason = req.Data.Attributes.DeprecatedReason
	}

	if err := h.db.Create(version).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Failed to create terraform version", "detail": err.Error()}}})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": formatTerraformVersion(version)})
}

type updateTerraformVersionRequest struct {
	Data struct {
		Type       string `json:"type"`
		Attributes struct {
			Version          *string                `json:"version,omitempty"`
			URL              *string                `json:"url,omitempty"`
			Sha              *string                `json:"sha,omitempty"`
			Deprecated       *bool                  `json:"deprecated,omitempty"`
			DeprecatedReason *string                `json:"deprecated-reason,omitempty"`
			Official         *bool                  `json:"official,omitempty"`
			Enabled          *bool                  `json:"enabled,omitempty"`
			Beta             *bool                  `json:"beta,omitempty"`
			Archs            []terraformVersionArch `json:"archs,omitempty"`
		} `json:"attributes"`
	} `json:"data"`
}

// Update a terraform version.
// PATCH /api/v2/admin/terraform-versions/:id
func (h *AdminTerraformVersionsHandler) Update(c *gin.Context) {
	if !h.requireAdmin(c) {
		return
	}

	id := c.Param("id")

	var version models.TerraformVersion
	if err := h.db.First(&version, "id = ?", id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Terraform version not found"}}})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		}
		return
	}

	var req updateTerraformVersionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"errors": []gin.H{{"status": "422", "title": "Invalid request", "detail": err.Error()}}})
		return
	}

	if req.Data.Attributes.Version != nil {
		version.Version = *req.Data.Attributes.Version
	}
	if req.Data.Attributes.URL != nil {
		version.URL = *req.Data.Attributes.URL
	}
	if req.Data.Attributes.Sha != nil {
		version.Sha = *req.Data.Attributes.Sha
	}
	if req.Data.Attributes.Official != nil {
		version.Official = *req.Data.Attributes.Official
	}
	if req.Data.Attributes.Enabled != nil {
		version.Enabled = *req.Data.Attributes.Enabled
	}
	if req.Data.Attributes.Beta != nil {
		version.Beta = *req.Data.Attributes.Beta
	}
	if req.Data.Attributes.Deprecated != nil {
		version.Deprecated = *req.Data.Attributes.Deprecated
	}
	// Only store non-empty deprecated reasons; clear to nil if empty string sent
	if req.Data.Attributes.DeprecatedReason != nil {
		if *req.Data.Attributes.DeprecatedReason != "" {
			version.DeprecatedReason = req.Data.Attributes.DeprecatedReason
		} else {
			version.DeprecatedReason = nil
		}
	}

	// Handle archs update — only process archs with non-empty URLs.
	// The provider's plan modifier can send archs with url="" when the user
	// didn't specify archs in config; we must not let those overwrite real data.
	validArchs := filterValidArchs(req.Data.Attributes.Archs)
	if len(validArchs) > 0 {
		for _, arch := range validArchs {
			if arch.OS == "linux" && arch.Arch == runtime.GOARCH {
				version.URL = arch.URL
				version.Sha = arch.Sha
				break
			}
		}
		version.ArchsJSON = marshalArchs(validArchs)
	}

	if err := h.db.Save(&version).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Failed to update terraform version", "detail": err.Error()}}})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": formatTerraformVersion(&version)})
}

// Delete a terraform version.
// DELETE /api/v2/admin/terraform-versions/:id
func (h *AdminTerraformVersionsHandler) Delete(c *gin.Context) {
	if !h.requireAdmin(c) {
		return
	}

	id := c.Param("id")

	var version models.TerraformVersion
	if err := h.db.First(&version, "id = ?", id).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Terraform version not found"}}})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		}
		return
	}

	// Don't allow deleting official versions (like TFE)
	if version.Official {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"errors": []gin.H{{"status": "422", "title": "Cannot delete official Terraform version"}}})
		return
	}

	// Check if any workspaces use this version
	var count int64
	h.db.Model(&models.Workspace{}).Where("terraform_version = ?", version.Version).Count(&count)
	if count > 0 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"errors": []gin.H{{"status": "422", "title": "Cannot delete", "detail": fmt.Sprintf("Version %s is in use by %d workspace(s)", version.Version, count)}}})
		return
	}

	if err := h.db.Delete(&version).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Failed to delete terraform version"}}})
		return
	}

	c.Status(http.StatusNoContent)
}

// SeedOfficialVersions seeds the database with official Terraform versions if they don't exist.
// Called during platform startup.
func SeedOfficialVersions(db *gorm.DB) {
	for _, ver := range models.OfficialTerraformVersions {
		var existing models.TerraformVersion
		if err := db.Where("version = ?", ver).First(&existing).Error; err == nil {
			continue // Already exists
		}

		version := &models.TerraformVersion{
			Version:  ver,
			URL:      fmt.Sprintf("https://releases.hashicorp.com/terraform/%s/terraform_%s_linux_amd64.zip", ver, ver),
			Official: true,
			Enabled:  true,
			Beta:     strings.Contains(ver, "-"),
		}
		if err := db.Create(version).Error; err != nil {
			// Ignore duplicate key errors from concurrent startups
			if !strings.Contains(err.Error(), "duplicate") {
				logger.Warnf("Failed to seed terraform version %s: %v", ver, err)
			}
		}
	}
}

// ListEnabled returns all enabled, non-deprecated terraform versions.
// This endpoint does NOT require admin access - any authenticated user can call it.
// Used by workspace create/edit dialogs to show available versions.
// GET /api/v2/terraform-versions
func (h *AdminTerraformVersionsHandler) ListEnabled(c *gin.Context) {
	var versions []models.TerraformVersion
	h.db.Model(&models.TerraformVersion{}).
		Where("enabled = ? AND deprecated = ?", true, false).
		Order(`
			CAST(split_part(regexp_replace(version, '-.*$', ''), '.', 1) AS INTEGER) DESC,
			CAST(split_part(regexp_replace(version, '-.*$', ''), '.', 2) AS INTEGER) DESC,
			CAST(split_part(regexp_replace(version, '-.*$', ''), '.', 3) AS INTEGER) DESC,
			version DESC
		`).
		Find(&versions)

	data := make([]gin.H, 0, len(versions))
	for _, v := range versions {
		data = append(data, formatTerraformVersion(&v))
	}

	c.JSON(http.StatusOK, gin.H{"data": data})
}

// filterValidArchs returns only archs with non-empty URLs.
// The provider's PreserveAMD64ArchsOnChange plan modifier can produce archs
// where url is null (serialized as "") when the user didn't declare archs in
// their config. We filter those out to avoid corrupting stored data.
func filterValidArchs(archs []terraformVersionArch) []terraformVersionArch {
	var valid []terraformVersionArch
	for _, a := range archs {
		if a.URL != "" {
			valid = append(valid, a)
		}
	}
	return valid
}

// marshalArchs serializes archs to JSON for storage. Returns nil if empty.
func marshalArchs(archs []terraformVersionArch) *string {
	if len(archs) == 0 {
		return nil
	}
	b, err := json.Marshal(archs)
	if err != nil {
		return nil
	}
	s := string(b)
	return &s
}

// formatTerraformVersion formats a TerraformVersion as a JSON:API resource.
func formatTerraformVersion(v *models.TerraformVersion) gin.H {
	// Return stored archs if they exist (user explicitly provided them).
	// Otherwise return empty array — this prevents the provider's
	// PreserveAMD64ArchsOnChange plan modifier from corrupting archs by
	// setting url=null when url isn't in the user's config.
	var archs []gin.H
	if v.ArchsJSON != nil && *v.ArchsJSON != "" {
		var stored []terraformVersionArch
		if err := json.Unmarshal([]byte(*v.ArchsJSON), &stored); err == nil {
			for _, a := range stored {
				archs = append(archs, gin.H{
					"url":  a.URL,
					"sha":  a.Sha,
					"os":   a.OS,
					"arch": a.Arch,
				})
			}
		}
	}
	if archs == nil {
		archs = []gin.H{}
	}

	attrs := gin.H{
		"version":    v.Version,
		"url":        v.URL,
		"sha":        v.Sha,
		"deprecated": v.Deprecated,
		"official":   v.Official,
		"enabled":    v.Enabled,
		"beta":       v.Beta,
		"usage":      v.Usage,
		"created-at": v.CreatedAt.Format(time.RFC3339),
		"archs":      archs,
	}
	// Only include deprecated-reason when non-nil AND non-empty.
	// The tfe provider always sends deprecated-reason="" even when user doesn't set it,
	// but on read it maps nil → types.StringNull(). Including "" would cause:
	// "was null, but now cty.StringVal("")" inconsistency.
	if v.DeprecatedReason != nil && *v.DeprecatedReason != "" {
		attrs["deprecated-reason"] = *v.DeprecatedReason
	}

	return gin.H{
		"id":         v.ID,
		"type":       "terraform-versions",
		"attributes": attrs,
	}
}

func derefBool(ptr *bool, defaultVal bool) bool {
	if ptr != nil {
		return *ptr
	}
	return defaultVal
}
