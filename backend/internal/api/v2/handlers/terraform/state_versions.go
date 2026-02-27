// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package terraform

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/rbac"
	"github.com/iac-platform/backend/internal/services/state"
	"github.com/iac-platform/backend/internal/storage"
	"github.com/michielvha/logger"
)

type StateVersionHandlerV2 struct {
	stateVersionRepo *repository.StateVersionRepository
	workspaceRepo    *repository.WorkspaceRepository
	projectRepo      *repository.ProjectRepository
	authService      *auth.Service
	rbacService      *rbac.Service
	stateService     *state.Service
	storageClient    storage.Client
	storageBucket    string
}

func NewStateVersionHandlerV2(
	stateVersionRepo *repository.StateVersionRepository,
	workspaceRepo *repository.WorkspaceRepository,
	projectRepo *repository.ProjectRepository,
	authService *auth.Service,
	rbacService *rbac.Service,
	stateService *state.Service,
	storageClient storage.Client,
	storageBucket string,
) *StateVersionHandlerV2 {
	return &StateVersionHandlerV2{
		stateVersionRepo: stateVersionRepo,
		workspaceRepo:    workspaceRepo,
		projectRepo:      projectRepo,
		authService:      authService,
		rbacService:      rbacService,
		stateService:     stateService,
		storageClient:    storageClient,
		storageBucket:    storageBucket,
	}
}

type CreateStateVersionRequestV2 struct {
	StateData map[string]interface{} `json:"state_data" binding:"required"`
	Serial    *int                   `json:"serial,omitempty"`
	Lineage   string                 `json:"lineage,omitempty"`
}

// hostedStateDownloadURL returns the API URL for downloading state (TFE hosted-state-download-url).
// Terraform fetches state from this URL; it must be reachable (use API URL, not internal MinIO).
func (h *StateVersionHandlerV2) hostedStateDownloadURL(c *gin.Context, v *models.StateVersion) string {
	host := c.GetHeader("Host")
	if host == "" {
		host = c.Request.Host
	}
	scheme := "https"
	if c.GetHeader("X-Forwarded-Proto") == "http" || c.Request.TLS == nil {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s/api/v2/state-versions/%s/download", scheme, host, v.ID)
}

// ListByWorkspace lists state versions for a workspace (TFE-compatible)
// GET /api/v2/workspaces/:id/state-versions
func (h *StateVersionHandlerV2) ListByWorkspace(c *gin.Context) {
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

	// Verify workspace exists and get project ID
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

	// Check permission: state versions read
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{
					"status": "401",
					"title":  "Unauthorized",
					"detail": "Authentication required",
				},
			},
		})
		return
	}

	hasPermission, err := h.rbacService.CheckStateVersionPermission(c.Request.Context(), user.ID, workspaceID, workspace.ProjectID, "read")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to check permissions",
				},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Insufficient permissions to view state versions",
				},
			},
		})
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	perPage, _ := strconv.Atoi(c.DefaultQuery("per_page", "20"))
	if perPage > 100 {
		perPage = 100
	}
	offset := (page - 1) * perPage

	versions, total, err := h.stateVersionRepo.ListByWorkspace(workspaceID, perPage, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to list state versions",
				},
			},
		})
		return
	}

	// TFE-compatible response format
	c.JSON(http.StatusOK, gin.H{
		"data": versions,
		"meta": gin.H{
			"pagination": gin.H{
				"page":     page,
				"per_page": perPage,
				"total":    total,
			},
		},
	})
}

// CurrentStateVersion returns the latest state version for a workspace (TFE-compatible).
// GET /api/v2/workspaces/:id/current-state-version
// Terraform remote backend uses this plus hosted-state-download-url to pull state; missing URL caused tfe_* drift.
func (h *StateVersionHandlerV2) CurrentStateVersion(c *gin.Context) {
	workspaceID := c.Param("id")
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid workspace ID"},
			},
		})
		return
	}

	workspace, err := h.workspaceRepo.GetByID(workspaceID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "Workspace not found"},
			},
		})
		return
	}

	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{"status": "401", "title": "Unauthorized", "detail": "Authentication required"},
			},
		})
		return
	}

	ok, err := h.rbacService.CheckStateVersionPermission(c.Request.Context(), user.ID, workspaceID, workspace.ProjectID, "read")
	if err != nil || !ok {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "Insufficient permissions to view state version"},
			},
		})
		return
	}

	version, err := h.stateVersionRepo.GetLatest(workspaceID)
	if err != nil || version == nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "No state version for this workspace"},
			},
		})
		return
	}

	attrs := buildStateVersionAttributes(version)
	attrs["hosted-state-download-url"] = h.hostedStateDownloadURL(c, version)

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"id":         version.ID,
			"type":       "state-versions",
			"attributes": attrs,
			"relationships": gin.H{
				"workspace": gin.H{
					"data": gin.H{"id": version.WorkspaceID, "type": "workspaces"},
				},
			},
		},
	})
}

// Get returns a single state version by ID (TFE-compatible)
// GET /api/v2/state-versions/:id
func (h *StateVersionHandlerV2) Get(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid state version ID",
				},
			},
		})
		return
	}

	version, err := h.stateVersionRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "State version not found",
				},
			},
		})
		return
	}

	// Get workspace for permission check
	workspace, err := h.workspaceRepo.GetByID(version.WorkspaceID)
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

	// Check permission: state versions read
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{
					"status": "401",
					"title":  "Unauthorized",
					"detail": "Authentication required",
				},
			},
		})
		return
	}

	hasPermission, err := h.rbacService.CheckStateVersionPermission(c.Request.Context(), user.ID, version.WorkspaceID, workspace.ProjectID, "read")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to check permissions",
				},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Insufficient permissions to view state version",
				},
			},
		})
		return
	}

	attrs := buildStateVersionAttributes(version)
	attrs["hosted-state-download-url"] = h.hostedStateDownloadURL(c, version)

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"id":         version.ID,
			"type":       "state-versions",
			"attributes": attrs,
			"relationships": gin.H{
				"workspace": gin.H{
					"data": gin.H{
						"id":   version.WorkspaceID,
						"type": "workspaces",
					},
				},
			},
		},
	})
}

// buildStateVersionAttributes returns attributes map for state version (JSON:API).
func buildStateVersionAttributes(v *models.StateVersion) map[string]interface{} {
	var m map[string]interface{}
	b, err := json.Marshal(v)
	if err != nil {
		return map[string]interface{}{}
	}
	_ = json.Unmarshal(b, &m)
	if m == nil {
		m = make(map[string]interface{})
	}
	return m
}

// Download streams the raw state JSON for a state version (TFE hosted-state-download-url target).
// GET /api/v2/state-versions/:id/download
func (h *StateVersionHandlerV2) Download(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid state version ID"},
			},
		})
		return
	}

	version, err := h.stateVersionRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "State version not found"},
			},
		})
		return
	}

	workspace, err := h.workspaceRepo.GetByID(version.WorkspaceID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "Workspace not found"},
			},
		})
		return
	}

	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{"status": "401", "title": "Unauthorized", "detail": "Authentication required"},
			},
		})
		return
	}

	ok, err := h.rbacService.CheckStateVersionPermission(c.Request.Context(), user.ID, version.WorkspaceID, workspace.ProjectID, "read")
	if err != nil || !ok {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "Insufficient permissions to download state"},
			},
		})
		return
	}

	var stateJSON []byte
	switch {
	case len(version.StateData) > 0:
		stateJSON, err = json.Marshal(version.StateData)
	case h.storageClient != nil:
		key := fmt.Sprintf("workspaces/%s/state/%d.json", version.WorkspaceID, version.Version)
		stateJSON, err = h.storageClient.Get(c.Request.Context(), key)
	default:
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "State storage unavailable"},
			},
		})
		return
	}
	if err != nil {
		logger.Warnf("State version %s download: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to load state"},
			},
		})
		return
	}

	c.Header("Content-Type", "application/json")
	c.Data(http.StatusOK, "application/json", stateJSON)
}

// GetOutputs returns outputs for a state version (TFE-compatible)
// GET /api/v2/state-versions/:id/outputs
func (h *StateVersionHandlerV2) GetOutputs(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid state version ID",
				},
			},
		})
		return
	}

	version, err := h.stateVersionRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "State version not found",
				},
			},
		})
		return
	}

	// Get workspace for permission check
	workspace, err := h.workspaceRepo.GetByID(version.WorkspaceID)
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

	// Check permission: state versions read-outputs (allows reading outputs even if full state is restricted)
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{
					"status": "401",
					"title":  "Unauthorized",
					"detail": "Authentication required",
				},
			},
		})
		return
	}

	hasPermission, err := h.rbacService.CheckStateVersionPermission(c.Request.Context(), user.ID, version.WorkspaceID, workspace.ProjectID, "read-outputs")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to check permissions",
				},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Insufficient permissions to view state version outputs",
				},
			},
		})
		return
	}

	// TFE-compatible: mask sensitive values (return nil); see current-state-version-outputs.
	outputs := extractOutputsFromStateData(version, true)

	c.JSON(http.StatusOK, gin.H{
		"data": outputs,
	})
}

// extractOutputsFromStateData builds the TFE-compatible outputs array from state.
// When maskSensitive is true, sensitive output values are set to nil (TFE behaviour for list endpoints).
func extractOutputsFromStateData(version *models.StateVersion, maskSensitive bool) []gin.H {
	outputs := []gin.H{}
	if version == nil || version.StateData == nil {
		return outputs
	}
	stateData, ok := version.StateData["outputs"].(map[string]interface{})
	if !ok {
		return outputs
	}
	for name, outputData := range stateData {
		outputMap, ok := outputData.(map[string]interface{})
		if !ok {
			continue
		}
		outputID := fmt.Sprintf("%s-%s", version.ID, name)
		value := outputMap["value"]
		if maskSensitive {
			if sens, ok := outputMap["sensitive"].(bool); ok && sens {
				value = nil
			}
		}
		attrs := gin.H{"name": name, "value": value}
		if outputType, hasType := outputMap["type"]; hasType {
			attrs["type"] = outputType
		}
		if sensitive, hasSensitive := outputMap["sensitive"]; hasSensitive {
			attrs["sensitive"] = sensitive
		}
		outputs = append(outputs, gin.H{
			"id":         outputID,
			"type":       "state-version-outputs",
			"attributes": attrs,
		})
	}
	return outputs
}

// Create creates a new state version for a workspace (TFE-compatible)
// POST /api/v2/workspaces/:id/state-versions
func (h *StateVersionHandlerV2) Create(c *gin.Context) {
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

	// Check permission: state versions write
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{
					"status": "401",
					"title":  "Unauthorized",
					"detail": "Authentication required",
				},
			},
		})
		return
	}

	hasPermission, err := h.rbacService.CheckStateVersionPermission(c.Request.Context(), user.ID, workspaceID, workspace.ProjectID, "write")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to check permissions",
				},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Insufficient permissions to create state versions",
				},
			},
		})
		return
	}

	// Check if workspace is manually locked (TFE-compatible)
	if workspace.Locked {
		detail := "Workspace is locked. Unlock the workspace to create state versions."
		if workspace.LockedReason != "" {
			detail = fmt.Sprintf("Workspace is locked: %s", workspace.LockedReason)
		}
		c.JSON(http.StatusConflict, gin.H{
			"errors": []gin.H{
				{
					"status": "409",
					"title":  "Conflict",
					"detail": detail,
				},
			},
		})
		return
	}

	// Check if state is locked by an active run (TFE-compatible)
	if h.stateService != nil {
		existingLock, lockErr := h.stateService.GetStateLock(c.Request.Context(), workspaceID)
		if lockErr == nil && existingLock != nil && !existingLock.IsExpired() {
			c.JSON(http.StatusConflict, gin.H{
				"errors": []gin.H{
					{
						"status": "409",
						"title":  "Conflict",
						"detail": fmt.Sprintf("State is locked by run %v (lock ID: %s)", existingLock.LockedBy, existingLock.LockID),
					},
				},
			})
			return
		}
	}

	var req CreateStateVersionRequestV2
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": err.Error(),
				},
			},
		})
		return
	}

	// Get next version number
	nextVersion, err := h.stateVersionRepo.GetNextVersion(workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to get next version number",
				},
			},
		})
		return
	}

	// Store state file in MinIO (TFE-compatible)
	// Path: workspaces/{workspace_id}/state/{version}.json
	stateJSON, err := json.Marshal(req.StateData)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Failed to marshal state data",
				},
			},
		})
		return
	}

	storageKey := fmt.Sprintf("workspaces/%s/state/%d.json", workspaceID, nextVersion)
	if h.storageClient != nil {
		if err := h.storageClient.Put(c.Request.Context(), storageKey, stateJSON); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{
					{
						"status": "500",
						"title":  "Internal Server Error",
						"detail": fmt.Sprintf("Failed to store state in object storage: %v", err),
					},
				},
			})
			return
		}
	}

	// Create state version record (metadata only - actual state is in MinIO)
	// Store minimal state data in DB for quick access (or empty if we want to force MinIO retrieval)
	stateVersion := &models.StateVersion{
		WorkspaceID: workspaceID,
		Version:     nextVersion,
		StateData:   models.StateData{}, // Empty - state is in MinIO
		Serial:      req.Serial,
		Lineage:     req.Lineage,
	}

	if err := h.stateVersionRepo.Create(stateVersion); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to create state version",
				},
			},
		})
		return
	}

	// TFE-compatible response format
	c.JSON(http.StatusCreated, gin.H{
		"data": gin.H{
			"id":         stateVersion.ID,
			"type":       "state-versions",
			"attributes": stateVersion,
			"relationships": gin.H{
				"workspace": gin.H{
					"data": gin.H{
						"id":   workspaceID,
						"type": "workspaces",
					},
				},
			},
		},
	})
}

// RemoveResource removes a resource from the latest state version by address
// POST /api/v2/workspaces/:id/state-versions/remove-resource
func (h *StateVersionHandlerV2) RemoveResource(c *gin.Context) {
	logger.Debugf("StateVersionHandlerV2 RemoveResource - Request received for workspace: %s", c.Param("id"))
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

	// Verify workspace exists and get project ID
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

	// Check permission: state versions write
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{
					"status": "401",
					"title":  "Unauthorized",
					"detail": "Authentication required",
				},
			},
		})
		return
	}

	hasPermission, err := h.rbacService.CheckStateVersionPermission(c.Request.Context(), user.ID, workspaceID, workspace.ProjectID, "write")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to check permissions",
				},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Insufficient permissions to modify state",
				},
			},
		})
		return
	}

	// Parse request body
	var req struct {
		Address string `json:"address" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": fmt.Sprintf("Invalid request: %v", err),
				},
			},
		})
		return
	}

	// Remove resource from state
	if h.stateService == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "State service not initialized",
				},
			},
		})
		return
	}

	if err := h.stateService.RemoveResourceFromState(c.Request.Context(), workspaceID, req.Address); err != nil {
		// Check if it's a "not found" error
		if strings.Contains(err.Error(), "not found") {
			c.JSON(http.StatusNotFound, gin.H{
				"errors": []gin.H{
					{
						"status": "404",
						"title":  "Not Found",
						"detail": err.Error(),
					},
				},
			})
			return
		}

		// Check if it's a "locked" error
		if strings.Contains(err.Error(), "locked") {
			c.JSON(http.StatusConflict, gin.H{
				"errors": []gin.H{
					{
						"status": "409",
						"title":  "Conflict",
						"detail": err.Error(),
					},
				},
			})
			return
		}

		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": err.Error(),
				},
			},
		})
		return
	}

	// Return success
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"message": fmt.Sprintf("Resource %s removed from state", req.Address),
		},
	})
}

// Delete deletes a state version (StackWeaver-specific feature)
// DELETE /api/v2/state-versions/:id
func (h *StateVersionHandlerV2) Delete(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{
					"status": "400",
					"title":  "Bad Request",
					"detail": "Invalid state version ID",
				},
			},
		})
		return
	}

	// Get state version
	version, err := h.stateVersionRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{
					"status": "404",
					"title":  "Not Found",
					"detail": "State version not found",
				},
			},
		})
		return
	}

	// Get workspace for permission check
	workspace, err := h.workspaceRepo.GetByID(version.WorkspaceID)
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

	// Check permission: state versions write
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{
				{
					"status": "401",
					"title":  "Unauthorized",
					"detail": "Authentication required",
				},
			},
		})
		return
	}

	hasPermission, err := h.rbacService.CheckStateVersionPermission(c.Request.Context(), user.ID, version.WorkspaceID, workspace.ProjectID, "write")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "Failed to check permissions",
				},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{
					"status": "403",
					"title":  "Forbidden",
					"detail": "Insufficient permissions to delete state version",
				},
			},
		})
		return
	}

	// Delete state version
	if h.stateService == nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": "State service not initialized",
				},
			},
		})
		return
	}

	if err := h.stateService.DeleteStateVersion(c.Request.Context(), id); err != nil {
		// Check if it's a "locked" error
		if strings.Contains(err.Error(), "locked") {
			c.JSON(http.StatusConflict, gin.H{
				"errors": []gin.H{
					{
						"status": "409",
						"title":  "Conflict",
						"detail": err.Error(),
					},
				},
			})
			return
		}

		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{
					"status": "500",
					"title":  "Internal Server Error",
					"detail": err.Error(),
				},
			},
		})
		return
	}

	// Return success (204 No Content)
	c.Status(http.StatusNoContent)
}
