// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package terraform

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/storage"
	"github.com/michielvha/logger"
)

type ConfigurationVersionHandlerV2 struct {
	configVersionRepo *repository.ConfigurationVersionRepository
	workspaceRepo     *repository.WorkspaceRepository
	authService       *auth.Service
	storageClient     storage.Client
	storageBucket     string
}

func NewConfigurationVersionHandlerV2(
	configVersionRepo *repository.ConfigurationVersionRepository,
	workspaceRepo *repository.WorkspaceRepository,
	authService *auth.Service,
	storageClient storage.Client,
	storageBucket string,
) *ConfigurationVersionHandlerV2 {
	return &ConfigurationVersionHandlerV2{
		configVersionRepo: configVersionRepo,
		workspaceRepo:     workspaceRepo,
		authService:       authService,
		storageClient:     storageClient,
		storageBucket:     storageBucket,
	}
}

type CreateConfigurationVersionRequestV2 struct {
	Data struct {
		Type       string `json:"type"`
		Attributes struct {
			AutoQueueRuns *bool `json:"auto-queue-runs,omitempty"`
			Speculative   *bool `json:"speculative,omitempty"`
		} `json:"attributes"`
	} `json:"data"`
}

// Create creates a new configuration version (TFE-compatible)
// POST /api/v2/workspaces/:id/configuration-versions
// Returns a configuration version with an upload URL
func (h *ConfigurationVersionHandlerV2) Create(c *gin.Context) {
	workspaceID := c.Param("id")
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid workspace ID",
				},
			},
		})
		return
	}

	// Verify workspace exists
	workspace, err := h.workspaceRepo.GetByID(workspaceID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Workspace not found",
				},
			},
		})
		return
	}

	var req CreateConfigurationVersionRequestV2
	if err := c.ShouldBindJSON(&req); err != nil {
		// If JSON parsing fails, use defaults
		req.Data.Attributes.AutoQueueRuns = new(bool)
		req.Data.Attributes.Speculative = new(bool)
		*req.Data.Attributes.Speculative = false
	}

	autoQueueRuns := false
	if req.Data.Attributes.AutoQueueRuns != nil {
		autoQueueRuns = *req.Data.Attributes.AutoQueueRuns
	}

	speculative := false
	if req.Data.Attributes.Speculative != nil {
		speculative = *req.Data.Attributes.Speculative
	}

	// TFE-compatible: Determine configuration version source based on auth method
	// This allows runs to correctly identify their source (CLI vs UI vs VCS)
	configSource := "tfe-api" // Default fallback
	if authMethod, exists := c.Get("auth_method"); exists {
		switch authMethod {
		case "tfe_token":
			// TFE token = Terraform CLI remote backend
			configSource = "tfe-cli"
		case "jwt":
			// JWT token = UI/web interface
			configSource = "tfe-ui"
		case "api_key":
			// API key = programmatic access (similar to CLI)
			configSource = "tfe-cli"
		}
	}

	// Create configuration version
	configVersion := &models.ConfigurationVersion{
		WorkspaceID:   workspaceID,
		Status:        models.ConfigurationVersionStatusPending,
		Source:        configSource, // TFE-compatible: "tfe-api", "tfe-ui", "tfe-cli", "tfe-vcs" (indicates how config version was created)
		AutoQueueRuns: autoQueueRuns,
		Speculative:   speculative,
	}

	if err := h.configVersionRepo.Create(configVersion); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to create configuration version",
				},
			},
		})
		return
	}

	// Generate upload URL with temporary token (TFE-compatible format)
	// Terraform CLI doesn't send Authorization header for upload URLs, so we use
	// a temporary token in the URL query parameter (similar to pre-signed URLs)
	host := c.GetHeader("Host")
	if host == "" {
		host = c.Request.Host
	}
	scheme := "https"
	if c.GetHeader("X-Forwarded-Proto") == "http" || c.Request.TLS == nil {
		scheme = "http"
	}
	// Include upload token in URL query parameter
	uploadURL := fmt.Sprintf("%s://%s/api/v2/configuration-versions/%s/upload?token=%s",
		scheme, host, configVersion.ID, configVersion.UploadToken)

	logger.Infof("Created config version %s for workspace %s", configVersion.ID, workspaceID)
	logger.Infof("Upload URL: %s", uploadURL)
	logger.Infof("Request Host: %s, Scheme: %s", host, scheme)

	// Format in TFE-compatible JSON:API format
	c.JSON(http.StatusCreated, gin.H{
		"data": gin.H{
			"id":   configVersion.ID,
			"type": "configuration-versions",
			"attributes": gin.H{
				"status":          configVersion.Status,
				"upload-url":      uploadURL,
				"source":          configVersion.Source,
				"auto-queue-runs": configVersion.AutoQueueRuns,
				"speculative":     configVersion.Speculative,
				"created-at":      configVersion.CreatedAt.Format("2006-01-02T15:04:05Z"),
			},
			"relationships": gin.H{
				"workspace": gin.H{
					"data": gin.H{
						"id":   workspace.ID,
						"type": "workspaces",
					},
				},
			},
			"links": gin.H{
				"upload": uploadURL,
			},
		},
	})
}

// Get retrieves a configuration version by ID (TFE-compatible)
// GET /api/v2/configuration-versions/:id
func (h *ConfigurationVersionHandlerV2) Get(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid configuration version ID",
				},
			},
		})
		return
	}

	configVersion, err := h.configVersionRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Configuration version not found",
				},
			},
		})
		return
	}

	// Use absolute URL based on request host
	host := c.GetHeader("Host")
	if host == "" {
		host = c.Request.Host
	}
	scheme := "https"
	if c.GetHeader("X-Forwarded-Proto") == "http" || c.Request.TLS == nil {
		scheme = "http"
	}
	uploadURL := fmt.Sprintf("%s://%s/api/v2/configuration-versions/%s/upload", scheme, host, configVersion.ID)

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"id":   configVersion.ID,
			"type": "configuration-versions",
			"attributes": gin.H{
				"status":          configVersion.Status,
				"upload-url":      uploadURL,
				"source":          configVersion.Source,
				"auto-queue-runs": configVersion.AutoQueueRuns,
				"speculative":     configVersion.Speculative,
				"created-at":      configVersion.CreatedAt.Format("2006-01-02T15:04:05Z"),
				"updated-at":      configVersion.UpdatedAt.Format("2006-01-02T15:04:05Z"),
			},
			"relationships": gin.H{
				"workspace": gin.H{
					"data": gin.H{
						"id":   configVersion.WorkspaceID,
						"type": "workspaces",
					},
				},
			},
			"links": gin.H{
				"upload": uploadURL,
			},
		},
	})
}

// Upload handles configuration file upload (TFE-compatible)
// PUT /api/v2/configuration-versions/:id/upload?token=<upload_token>
// Terraform sends the configuration as raw binary data (tar.gz) in the request body
// Authentication is done via token in query parameter (not Authorization header)
func (h *ConfigurationVersionHandlerV2) Upload(c *gin.Context) {
	// Log for debugging
	logger.Infof("Received PUT request to /api/v2/configuration-versions/%s/upload", c.Param("id"))
	logger.Infof("Content-Type: %s", c.GetHeader("Content-Type"))
	logger.Infof("Content-Length: %s", c.GetHeader("Content-Length"))

	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid configuration version ID",
				},
			},
		})
		return
	}

	// Validate upload token from query parameter
	uploadToken := c.Query("token")
	if uploadToken == "" {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{
					"status": "401",
					"title":  "Unauthorized",
					"detail": "Upload token required",
				},
			},
		})
		return
	}

	configVersion, err := h.configVersionRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "Configuration version not found",
				},
			},
		})
		return
	}

	// Verify upload token matches
	if configVersion.UploadToken != uploadToken {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{
					"status": "401",
					"title":  "Unauthorized",
					"detail": "Invalid upload token",
				},
			},
		})
		return
	}

	if configVersion.Status != models.ConfigurationVersionStatusPending {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": fmt.Sprintf("Configuration version is not in pending status (current: %s)", configVersion.Status),
				},
			},
		})
		return
	}

	// Read uploaded file (Terraform sends configuration as tar.gz)
	// Store in MinIO so runners can access it
	// TFE expects raw binary data in PUT request body
	uploadData, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": fmt.Sprintf("Failed to read upload data: %v", err),
				},
			},
		})
		return
	}

	if len(uploadData) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "No upload data provided",
				},
			},
		})
		return
	}

	// Store configuration files in MinIO
	// Path: configuration-versions/{config_version_id}/config.tar.gz
	storageKey := fmt.Sprintf("configuration-versions/%s/config.tar.gz", configVersion.ID)
	if h.storageClient == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Storage client not initialized",
				},
			},
		})
		return
	}

	if err := h.storageClient.Put(c.Request.Context(), storageKey, uploadData); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": fmt.Sprintf("Failed to store configuration files: %v", err),
				},
			},
		})
		return
	}

	// Update status to uploaded
	configVersion.Status = models.ConfigurationVersionStatusUploaded
	configVersion.UpdatedAt = time.Now()
	if err := h.configVersionRepo.Update(configVersion); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to update configuration version",
				},
			},
		})
		return
	}

	logger.Infof("Successfully stored configuration file (%d bytes) for config version %s", len(uploadData), configVersion.ID)

	// TFE returns 200 OK with the updated configuration version
	// Use absolute URL based on request host
	host := c.GetHeader("Host")
	if host == "" {
		host = c.Request.Host
	}
	scheme := "https"
	if c.GetHeader("X-Forwarded-Proto") == "http" || c.Request.TLS == nil {
		scheme = "http"
	}
	uploadURL := fmt.Sprintf("%s://%s/api/v2/configuration-versions/%s/upload", scheme, host, configVersion.ID)
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"id":   configVersion.ID,
			"type": "configuration-versions",
			"attributes": gin.H{
				"status":          configVersion.Status,
				"upload-url":      uploadURL,
				"source":          configVersion.Source,
				"auto-queue-runs": configVersion.AutoQueueRuns,
				"speculative":     configVersion.Speculative,
				"created-at":      configVersion.CreatedAt.Format("2006-01-02T15:04:05Z"),
				"updated-at":      configVersion.UpdatedAt.Format("2006-01-02T15:04:05Z"),
			},
			"relationships": gin.H{
				"workspace": gin.H{
					"data": gin.H{
						"id":   configVersion.WorkspaceID,
						"type": "workspaces",
					},
				},
			},
			"links": gin.H{
				"upload": uploadURL,
			},
		},
	})
}

// ListByWorkspace lists configuration versions for a workspace (TFE-compatible)
// GET /api/v2/workspaces/:id/configuration-versions
func (h *ConfigurationVersionHandlerV2) ListByWorkspace(c *gin.Context) {
	workspaceID := c.Param("id")
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid workspace ID",
				},
			},
		})
		return
	}

	configVersions, err := h.configVersionRepo.GetByWorkspaceID(workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to list configuration versions",
				},
			},
		})
		return
	}

	data := make([]gin.H, len(configVersions))
	for i, cv := range configVersions {
		uploadURL := fmt.Sprintf("/api/v2/configuration-versions/%s/upload", cv.ID)
		data[i] = gin.H{
			"id":   cv.ID,
			"type": "configuration-versions",
			"attributes": gin.H{
				"status":          cv.Status,
				"upload-url":      uploadURL,
				"source":          cv.Source,
				"auto-queue-runs": cv.AutoQueueRuns,
				"speculative":     cv.Speculative,
				"created-at":      cv.CreatedAt.Format("2006-01-02T15:04:05Z"),
				"updated-at":      cv.UpdatedAt.Format("2006-01-02T15:04:05Z"),
			},
			"relationships": gin.H{
				"workspace": gin.H{
					"data": gin.H{
						"id":   cv.WorkspaceID,
						"type": "workspaces",
					},
				},
			},
			"links": gin.H{
				"upload": uploadURL,
			},
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"data": data,
	})
}
