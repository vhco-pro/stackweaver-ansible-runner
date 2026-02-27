// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/services/registry"
)

type RegistryProviderHandler struct {
	providerService *registry.ProviderService
}

func NewRegistryProviderHandler(providerService *registry.ProviderService) *RegistryProviderHandler {
	return &RegistryProviderHandler{
		providerService: providerService,
	}
}

// ListProviders handles GET /v1/providers
// Query params: offset, limit, verified, namespace (optional in path)
func (h *RegistryProviderHandler) ListProviders(c *gin.Context) {
	namespace := c.Param("namespace") // Optional path parameter
	verifiedStr := c.Query("verified")

	var verified *bool
	switch verifiedStr {
	case "true":
		v := true
		verified = &v
	case "false":
		v := false
		verified = &v
	}

	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "15"))
	if limit > 100 {
		limit = 100
	}

	providers, total, err := h.providerService.ListProviders(namespace, verified, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []string{"Failed to list providers"},
		})
		return
	}

	// Format response according to Terraform Registry API spec
	response := gin.H{
		"meta": gin.H{
			"limit":          limit,
			"current_offset": offset,
		},
		"providers": formatProviders(providers),
	}

	if offset+limit < int(total) {
		response["meta"].(gin.H)["next_offset"] = offset + limit
		response["meta"].(gin.H)["next_url"] = c.Request.URL.Path + "?limit=" + strconv.Itoa(limit) + "&offset=" + strconv.Itoa(offset+limit)
	}

	c.JSON(http.StatusOK, response)
}

// SearchProviders handles GET /v1/providers/search
func (h *RegistryProviderHandler) SearchProviders(c *gin.Context) {
	query := c.Query("q")
	if query == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []string{"Query parameter 'q' is required"},
		})
		return
	}

	namespace := c.Query("namespace")
	verifiedStr := c.Query("verified")

	var verified *bool
	switch verifiedStr {
	case "true":
		v := true
		verified = &v
	case "false":
		v := false
		verified = &v
	}

	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "15"))
	if limit > 100 {
		limit = 100
	}

	providers, total, err := h.providerService.SearchProviders(query, namespace, verified, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []string{"Failed to search providers"},
		})
		return
	}

	response := gin.H{
		"meta": gin.H{
			"limit":          limit,
			"current_offset": offset,
		},
		"providers": formatProviders(providers),
	}

	if offset+limit < int(total) {
		response["meta"].(gin.H)["next_offset"] = offset + limit
		response["meta"].(gin.H)["next_url"] = c.Request.URL.Path + "?q=" + query + "&limit=" + strconv.Itoa(limit) + "&offset=" + strconv.Itoa(offset+limit)
	}

	c.JSON(http.StatusOK, response)
}

// GetProviderVersions handles GET /v1/providers/:namespace/:name/versions
func (h *RegistryProviderHandler) GetProviderVersions(c *gin.Context) {
	namespace := c.Param("namespace")
	name := c.Param("name")

	versions, err := h.providerService.GetProviderVersions(namespace, name)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []string{"Provider not found"},
		})
		return
	}

	// Format response according to Terraform Registry API spec
	versionList := make([]gin.H, len(versions))
	for i, v := range versions {
		platforms := make([]gin.H, len(v.Platforms))
		for j, p := range v.Platforms {
			platforms[j] = gin.H{
				"os":   p.OS,
				"arch": p.Arch,
			}
		}

		versionList[i] = gin.H{
			"version":   v.Version,
			"platforms": platforms,
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"versions": versionList,
	})
}

// GetProvider handles GET /v1/providers/:namespace/:name (latest version)
func (h *RegistryProviderHandler) GetProvider(c *gin.Context) {
	namespace := c.Param("namespace")
	name := c.Param("name")

	provider, err := h.providerService.GetProvider(namespace, name)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []string{"Provider not found"},
		})
		return
	}

	latestVersion, err := h.providerService.GetLatestVersion(namespace, name)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []string{"No versions found for this provider"},
		})
		return
	}

	response := formatProviderDetail(provider, latestVersion)
	c.JSON(http.StatusOK, response)
}

// GetProviderVersion handles GET /v1/providers/:namespace/:name/:version
func (h *RegistryProviderHandler) GetProviderVersion(c *gin.Context) {
	namespace := c.Param("namespace")
	name := c.Param("name")
	version := c.Param("version")

	provider, err := h.providerService.GetProvider(namespace, name)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []string{"Provider not found"},
		})
		return
	}

	providerVersion, err := h.providerService.GetProviderVersion(namespace, name, version)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []string{"Provider version not found"},
		})
		return
	}

	response := formatProviderDetail(provider, providerVersion)
	c.JSON(http.StatusOK, response)
}

// DownloadProvider handles GET /v1/providers/:namespace/:name/:version/download/:os/:arch
// and GET /v1/providers/:namespace/:name/download/:os/:arch (latest version)
func (h *RegistryProviderHandler) DownloadProvider(c *gin.Context) {
	namespace := c.Param("namespace")
	name := c.Param("name")
	version := c.Param("version")
	os := c.Param("os")
	arch := c.Param("arch")

	// If version is empty, get latest version
	if version == "" {
		latestVersion, err := h.providerService.GetLatestVersion(namespace, name)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{
				"errors": []string{"Provider or version not found"},
			})
			return
		}
		version = latestVersion.Version
	}

	// Get download URL (presigned)
	downloadURL, err := h.providerService.GetDownloadURL(c.Request.Context(), namespace, name, version, os, arch)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []string{"Provider binary not available for download"},
		})
		return
	}

	// Track download asynchronously
	go func() {
		providerVersion, err := h.providerService.GetProviderVersion(namespace, name, version)
		if err == nil && len(providerVersion.Platforms) > 0 {
			// Find the matching platform
			for _, platform := range providerVersion.Platforms {
				if platform.OS == os && platform.Arch == arch {
					ipAddress := c.ClientIP()
					userAgent := c.GetHeader("User-Agent")
					_ = h.providerService.TrackDownload(platform.ID, ipAddress, userAgent)
					break
				}
			}
		}
	}()

	// Redirect to presigned URL (302 as per Terraform Registry spec)
	c.Redirect(http.StatusFound, downloadURL)
}

// GetProviderDownloadsSummary handles GET /v2/providers/:namespace/:name/downloads/summary
func (h *RegistryProviderHandler) GetProviderDownloadsSummary(c *gin.Context) {
	namespace := c.Param("namespace")
	name := c.Param("name")

	latestVersion, err := h.providerService.GetLatestVersion(namespace, name)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []string{"No versions found for this provider"},
		})
		return
	}

	// Get stats for the first platform (or aggregate all platforms)
	if len(latestVersion.Platforms) == 0 {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []string{"No platforms found for this provider version"},
		})
		return
	}

	// For now, use the first platform's stats
	stats, err := h.providerService.GetDownloadStats(latestVersion.Platforms[0].ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []string{"Failed to get download statistics"},
		})
		return
	}

	// Format according to Terraform Registry v2 API spec
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"type": "provider-downloads-summary",
			"id":   latestVersion.ID.String(),
			"attributes": gin.H{
				"week":  stats["week"],
				"month": stats["month"],
				"year":  stats["year"],
				"total": stats["total"],
			},
		},
	})
}

// Helper functions

func formatProviders(providers []models.Provider) []gin.H {
	result := make([]gin.H, 0, len(providers))
	for _, p := range providers {
		// Get latest version for each provider
		var latestVersion *models.ProviderVersion
		if len(p.Versions) > 0 {
			latestVersion = &p.Versions[0]
		}

		if latestVersion != nil {
			result = append(result, gin.H{
				"id":           p.Organization.Name + "/" + p.Name + "/" + latestVersion.Version,
				"namespace":    p.Organization.Name,
				"name":         p.Name,
				"version":      latestVersion.Version,
				"published_at": latestVersion.PublishedAt.Format("2006-01-02T15:04:05Z"),
				"downloads":    latestVersion.Downloads,
				"verified":     p.Verified,
			})
		}
	}
	return result
}

func formatProviderDetail(provider *models.Provider, version *models.ProviderVersion) gin.H {
	platforms := make([]gin.H, len(version.Platforms))
	for i, p := range version.Platforms {
		platforms[i] = gin.H{
			"os":       p.OS,
			"arch":     p.Arch,
			"shasum":   p.Shasum,
			"filename": p.Filename,
		}
	}

	// Get all versions for the provider
	allVersions := make([]string, len(provider.Versions))
	for i, v := range provider.Versions {
		allVersions[i] = v.Version
	}

	return gin.H{
		"id":           provider.Organization.Name + "/" + provider.Name + "/" + version.Version,
		"namespace":    provider.Organization.Name,
		"name":         provider.Name,
		"version":      version.Version,
		"published_at": version.PublishedAt.Format("2006-01-02T15:04:05Z"),
		"downloads":    version.Downloads,
		"verified":     provider.Verified,
		"platforms":    platforms,
		"versions":     allVersions,
	}
}
