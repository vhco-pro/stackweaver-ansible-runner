// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansible

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// CollectionInfo represents an installed Ansible Galaxy collection
type CollectionInfo struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source"` // "pre-installed", "requirements.yml", "manual"
}

// CollectionsHandler handles Galaxy collection-related endpoints
type CollectionsHandler struct{}

// NewCollectionsHandler creates a new CollectionsHandler
func NewCollectionsHandler() *CollectionsHandler {
	return &CollectionsHandler{}
}

// ListPreInstalledCollections returns the list of pre-installed collections in the runner
// GET /ansible/collections/pre-installed
func (h *CollectionsHandler) ListPreInstalledCollections(c *gin.Context) {
	// These are the collections pre-installed in runner-images/ansible/Dockerfile
	collections := []CollectionInfo{
		{
			Name:        "amazon.aws",
			Namespace:   "amazon",
			Version:     "latest",
			Description: "AWS cloud modules and dynamic inventory plugins",
			Source:      "pre-installed",
		},
		{
			Name:        "azure.azcollection",
			Namespace:   "azure",
			Version:     "latest",
			Description: "Azure cloud modules and dynamic inventory plugins",
			Source:      "pre-installed",
		},
		{
			Name:        "google.cloud",
			Namespace:   "google",
			Version:     "latest",
			Description: "GCP cloud modules and dynamic inventory plugins",
			Source:      "pre-installed",
		},
		{
			Name:        "community.vmware",
			Namespace:   "community",
			Version:     "latest",
			Description: "VMware vSphere modules",
			Source:      "pre-installed",
		},
		{
			Name:        "community.general",
			Namespace:   "community",
			Version:     "latest",
			Description: "General-purpose modules (1000+ modules)",
			Source:      "pre-installed",
		},
		{
			Name:        "ansible.posix",
			Namespace:   "ansible",
			Version:     "latest",
			Description: "POSIX system modules and JSONL callback",
			Source:      "pre-installed",
		},
		{
			Name:        "ansible.netcommon",
			Namespace:   "ansible",
			Version:     "latest",
			Description: "Network automation base modules",
			Source:      "pre-installed",
		},
	}

	// Convert to JSON:API format
	data := make([]map[string]interface{}, len(collections))
	for i, col := range collections {
		data[i] = map[string]interface{}{
			"type": "ansible-collections",
			"id":   col.Name,
			"attributes": map[string]interface{}{
				"name":        col.Name,
				"namespace":   col.Namespace,
				"version":     col.Version,
				"description": col.Description,
				"source":      col.Source,
			},
		}
	}

	c.JSON(http.StatusOK, gin.H{"data": data})
}

// ListJobCollections returns collections installed for a specific job
// GET /ansible/jobs/:id/collections
func (h *CollectionsHandler) ListJobCollections(c *gin.Context) {
	jobID := c.Param("id")
	if jobID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []map[string]string{{"detail": "job ID is required"}},
		})
		return
	}

	// For now, return the pre-installed collections
	// In the future, we'll track per-job installations from requirements.yml
	h.ListPreInstalledCollections(c)
}

// SearchGalaxyCollections searches for collections on Galaxy Hub
// GET /ansible/collections/search?q=keyword
func (h *CollectionsHandler) SearchGalaxyCollections(c *gin.Context) {
	// This would call the Galaxy API in a real implementation
	// For now, return a placeholder response
	c.JSON(http.StatusOK, gin.H{
		"data": []interface{}{},
		"meta": map[string]interface{}{
			"message": "Galaxy search not yet implemented. Browse collections at https://galaxy.ansible.com",
		},
	})
}
