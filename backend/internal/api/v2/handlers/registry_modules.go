// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	// "github.com/gomarkdown/markdown"
	// "github.com/gomarkdown/markdown/html"
	// "github.com/gomarkdown/markdown/parser"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/services/registry"
)

type RegistryModuleHandler struct {
	moduleService *registry.ModuleService
}

func NewRegistryModuleHandler(moduleService *registry.ModuleService) *RegistryModuleHandler {
	return &RegistryModuleHandler{
		moduleService: moduleService,
	}
}

// ListModules handles GET /v1/modules
// Query params: offset, limit, provider, verified, namespace (optional in path)
func (h *RegistryModuleHandler) ListModules(c *gin.Context) {
	namespace := c.Param("namespace") // Optional path parameter
	provider := c.Query("provider")
	verifiedStr := c.Query("verified")

	var verified *bool
	if verifiedStr != "" {
		v := verifiedStr == "true"
		verified = &v
	}

	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "15"))
	if limit > 100 {
		limit = 100 // Max limit
	}
	if limit < 1 {
		limit = 15 // Default limit
	}

	modules, total, err := h.moduleService.ListModules(namespace, provider, verified, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []string{err.Error()},
		})
		return
	}

	// Format response according to Terraform Registry API spec
	response := gin.H{
		"meta": gin.H{
			"limit":          limit,
			"current_offset": offset,
		},
		"modules": formatModules(modules),
	}

	if int64(offset+limit) < total {
		response["meta"].(gin.H)["next_offset"] = offset + limit
		response["meta"].(gin.H)["next_url"] = buildNextURL(c, offset+limit, limit, provider, verified)
	}

	c.JSON(http.StatusOK, response)
}

// SearchModules handles GET /v1/modules/search
func (h *RegistryModuleHandler) SearchModules(c *gin.Context) {
	query := c.Query("q")
	if query == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []string{"query parameter 'q' is required"},
		})
		return
	}

	namespace := c.Query("namespace")
	provider := c.Query("provider")
	verifiedStr := c.Query("verified")

	var verified *bool
	if verifiedStr != "" {
		v := verifiedStr == "true"
		verified = &v
	}

	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "15"))
	if limit > 100 {
		limit = 100
	}
	if limit < 1 {
		limit = 15
	}

	modules, total, err := h.moduleService.SearchModules(query, namespace, provider, verified, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []string{err.Error()},
		})
		return
	}

	response := gin.H{
		"meta": gin.H{
			"limit":          limit,
			"current_offset": offset,
		},
		"modules": formatModules(modules),
	}

	if int64(offset+limit) < total {
		response["meta"].(gin.H)["next_offset"] = offset + limit
		response["meta"].(gin.H)["next_url"] = buildSearchNextURL(c, query, offset+limit, limit, namespace, provider, verified)
	}

	c.JSON(http.StatusOK, response)
}

// GetModuleVersions handles GET /v1/modules/:namespace/:name/:provider/versions
func (h *RegistryModuleHandler) GetModuleVersions(c *gin.Context) {
	namespace := c.Param("namespace")
	name := c.Param("name")
	provider := c.Param("provider")

	versions, err := h.moduleService.GetModuleVersions(namespace, name, provider)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []string{"Module not found"},
		})
		return
	}

	// Format according to Terraform Registry API spec
	versionList := make([]gin.H, 0, len(versions))
	for _, v := range versions {
		submodules := []string{}
		if v.Submodules != nil {
			if submods, ok := v.Submodules["paths"].([]interface{}); ok {
				for _, sm := range submods {
					if smStr, ok := sm.(string); ok {
						submodules = append(submodules, smStr)
					}
				}
			}
		}

		versionList = append(versionList, gin.H{
			"version":    v.Version,
			"submodules": submodules,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"modules": []gin.H{
			{
				"source":   "", // Will be populated from module source
				"versions": versionList,
			},
		},
	})
}

// GetModule handles GET /v1/modules/:namespace/:name/:provider (latest version)
func (h *RegistryModuleHandler) GetModule(c *gin.Context) {
	namespace := c.Param("namespace")
	name := c.Param("name")
	provider := c.Param("provider")

	module, err := h.moduleService.GetModule(namespace, name, provider)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []string{"Module not found"},
		})
		return
	}

	latestVersion, err := h.moduleService.GetLatestVersion(namespace, name, provider)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []string{"No versions found for this module"},
		})
		return
	}

	response := formatModuleDetail(module, latestVersion)
	c.JSON(http.StatusOK, response)
}

// GetModuleVersion handles GET /v1/modules/:namespace/:name/:provider/:version
func (h *RegistryModuleHandler) GetModuleVersion(c *gin.Context) {
	namespace := c.Param("namespace")
	name := c.Param("name")
	provider := c.Param("provider")
	version := c.Param("version")

	module, err := h.moduleService.GetModule(namespace, name, provider)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []string{"Module not found"},
		})
		return
	}

	moduleVersion, err := h.moduleService.GetModuleVersion(namespace, name, provider, version)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []string{"Module version not found"},
		})
		return
	}

	response := formatModuleDetail(module, moduleVersion)
	c.JSON(http.StatusOK, response)
}

// DownloadModule handles GET /v1/modules/:namespace/:name/:provider/:version/download
// and GET /v1/modules/:namespace/:name/:provider/download (latest version)
func (h *RegistryModuleHandler) DownloadModule(c *gin.Context) {
	namespace := c.Param("namespace")
	name := c.Param("name")
	provider := c.Param("provider")
	version := c.Param("version")

	// If version is empty, get latest version
	if version == "" {
		latestVersion, err := h.moduleService.GetLatestVersion(namespace, name, provider)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{
				"errors": []string{"Module or version not found"},
			})
			return
		}
		version = latestVersion.Version
	}

	// Get download URL (presigned)
	downloadURL, err := h.moduleService.GetDownloadURL(c.Request.Context(), namespace, name, provider, version)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []string{"Module version not available for download"},
		})
		return
	}

	// Track download asynchronously
	go func() {
		moduleVersion, err := h.moduleService.GetModuleVersion(namespace, name, provider, version)
		if err == nil {
			ipAddress := c.ClientIP()
			userAgent := c.GetHeader("User-Agent")
			_ = h.moduleService.TrackDownload(moduleVersion.ID, ipAddress, userAgent)
		}
	}()

	// Redirect to presigned URL (302 as per Terraform Registry spec)
	c.Redirect(http.StatusFound, downloadURL)
}

// GetModuleDownloadsSummary handles GET /v2/modules/:namespace/:name/:provider/downloads/summary
func (h *RegistryModuleHandler) GetModuleDownloadsSummary(c *gin.Context) {
	namespace := c.Param("namespace")
	name := c.Param("name")
	provider := c.Param("provider")

	latestVersion, err := h.moduleService.GetLatestVersion(namespace, name, provider)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []string{"No versions found for this module"},
		})
		return
	}

	stats, err := h.moduleService.GetDownloadStats(latestVersion.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []string{"Failed to get download statistics"},
		})
		return
	}

	// Format according to Terraform Registry v2 API spec
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"type": "module-downloads-summary",
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

func formatModules(modules []models.Module) []gin.H {
	result := make([]gin.H, 0, len(modules))
	for _, m := range modules {
		// Get latest version for each module
		var latestVersion string
		var publishedAt time.Time
		var downloads int

		if len(m.Versions) > 0 {
			latestVersion = m.Versions[0].Version
			publishedAt = m.Versions[0].PublishedAt
			downloads = m.Versions[0].Downloads
		}

		moduleID := m.Organization.Name + "/" + m.Name + "/" + m.Provider
		if latestVersion != "" {
			moduleID += "/" + latestVersion
		}

		result = append(result, gin.H{
			"id":           moduleID,
			"owner":        "",
			"namespace":    m.Organization.Name,
			"name":         m.Name,
			"version":      latestVersion,
			"provider":     m.Provider,
			"description":  m.Description,
			"source":       m.Source,
			"published_at": publishedAt.Format("2006-01-02T15:04:05Z"),
			"downloads":    downloads,
			"verified":     m.Verified,
		})
	}
	return result
}

func formatModuleDetail(module *models.Module, version *models.ModuleVersion) gin.H {
	// Get all versions for this module
	allVersions := make([]string, 0, len(module.Versions))
	for _, v := range module.Versions {
		allVersions = append(allVersions, v.Version)
	}

	// Format inputs
	inputs := []gin.H{}
	if version.Inputs != nil {
		if inputsList, ok := version.Inputs["inputs"].([]interface{}); ok {
			for _, input := range inputsList {
				if inputMap, ok := input.(map[string]interface{}); ok {
					inputs = append(inputs, gin.H{
						"name":        inputMap["name"],
						"description": inputMap["description"],
						"default":     inputMap["default"],
						"type":        inputMap["type"],
					})
				}
			}
		}
	}

	// Format outputs
	outputs := []gin.H{}
	if version.Outputs != nil {
		if outputsList, ok := version.Outputs["outputs"].([]interface{}); ok {
			for _, output := range outputsList {
				if outputMap, ok := output.(map[string]interface{}); ok {
					outputs = append(outputs, gin.H{
						"name":        outputMap["name"],
						"description": outputMap["description"],
					})
				}
			}
		}
	}

	// Format resources
	resources := []gin.H{}
	if version.Resources != nil {
		if resourcesList, ok := version.Resources["resources"].([]interface{}); ok {
			for _, resource := range resourcesList {
				if resourceMap, ok := resource.(map[string]interface{}); ok {
					resources = append(resources, gin.H{
						"name": resourceMap["name"],
						"type": resourceMap["type"],
					})
				}
			}
		}
	}

	// Format submodules
	submodules := []gin.H{}
	if version.Submodules != nil {
		if submodsList, ok := version.Submodules["submodules"].([]interface{}); ok {
			for _, submod := range submodsList {
				if submodMap, ok := submod.(map[string]interface{}); ok {
					submodReadme := ""
					if readmeVal, ok := submodMap["readme"].(string); ok {
						submodReadme = readmeVal // Return raw markdown for frontend Shiki rendering
					}
					submodules = append(submodules, gin.H{
						"path":    submodMap["path"],
						"readme":  submodReadme,
						"empty":   submodMap["empty"],
						"inputs":  submodMap["inputs"],
						"outputs": submodMap["outputs"],
					})
				}
			}
		}
	}

	return gin.H{
		"id":           formatModuleIDWithVersion(module, version),
		"owner":        "",
		"namespace":    module.Organization.Name,
		"name":         module.Name,
		"version":      version.Version,
		"provider":     module.Provider,
		"description":  module.Description,
		"source":       module.Source,
		"published_at": version.PublishedAt.Format("2006-01-02T15:04:05Z"),
		"downloads":    version.Downloads,
		"verified":     module.Verified,
		"root": gin.H{
			"path":         "",
			"readme":       version.Readme, // Return raw markdown for frontend Shiki rendering
			"empty":        false,
			"inputs":       inputs,
			"outputs":      outputs,
			"dependencies": []gin.H{}, // TODO: parse from version.Dependencies
			"resources":    resources,
		},
		"submodules": submodules,
		"providers":  []string{module.Provider}, // TODO: extract from dependencies
		"versions":   allVersions,
	}
}

// func formatModuleID(module models.Module) string {
// 	latestVersion := ""
// 	if len(module.Versions) > 0 {
// 		latestVersion = module.Versions[0].Version
// 	}
// 	if latestVersion == "" {
// 		return module.Organization.Name + "/" + module.Name + "/" + module.Provider
// 	}
// 	return formatModuleIDWithVersion(&module, &module.Versions[0])
// }

func formatModuleIDWithVersion(module *models.Module, version *models.ModuleVersion) string {
	return module.Organization.Name + "/" + module.Name + "/" + module.Provider + "/" + version.Version
}

func buildNextURL(c *gin.Context, offset, limit int, provider string, verified *bool) string {
	url := "/v1/modules"
	if c.Param("namespace") != "" {
		url += "/" + c.Param("namespace")
	}
	url += "?offset=" + strconv.Itoa(offset) + "&limit=" + strconv.Itoa(limit)
	if provider != "" {
		url += "&provider=" + provider
	}
	if verified != nil {
		url += "&verified=" + strconv.FormatBool(*verified)
	}
	return url
}

func buildSearchNextURL(c *gin.Context, query string, offset, limit int, namespace, provider string, verified *bool) string {
	url := "/v1/modules/search?q=" + query + "&offset=" + strconv.Itoa(offset) + "&limit=" + strconv.Itoa(limit)
	if namespace != "" {
		url += "&namespace=" + namespace
	}
	if provider != "" {
		url += "&provider=" + provider
	}
	if verified != nil {
		url += "&verified=" + strconv.FormatBool(*verified)
	}
	return url
}

// markdownToHTML converts markdown text to HTML with proper formatting
// func markdownToHTML(markdownText string) string {
// 	if markdownText == "" {
// 		return ""
// 	}

// 	// Configure parser with comprehensive extensions
// 	// CommonExtensions includes: tables, fenced code, autolinks, strikethrough, etc.
// 	extensions := parser.CommonExtensions | parser.AutoHeadingIDs | parser.NoEmptyLineBeforeBlock
// 	p := parser.NewWithExtensions(extensions)

// 	// Parse markdown
// 	doc := p.Parse([]byte(markdownText))

// 	// Configure HTML renderer with comprehensive flags
// 	// CommonFlags includes: UseXHTML, Smartypants, SmartypantsFractions, etc.
// 	htmlFlags := html.CommonFlags | html.HrefTargetBlank
// 	opts := html.RendererOptions{
// 		Flags: htmlFlags,
// 	}
// 	renderer := html.NewRenderer(opts)

// 	// Render to HTML
// 	htmlBytes := markdown.Render(doc, renderer)
// 	htmlStr := string(htmlBytes)

// 	// Wrap in a container div with proper classes for styling
// 	// This ensures proper spacing and formatting even without Tailwind Typography
// 	return "<div class=\"markdown-content\">" + htmlStr + "</div>"
// }
