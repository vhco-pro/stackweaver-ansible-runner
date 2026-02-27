// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/rbac"
	"github.com/iac-platform/backend/internal/services/vcs"
)

type VCSConnectionHandlerV2 struct {
	vcsConnectionRepo *repository.VCSConnectionRepository
	orgRepo           *repository.OrganizationRepository
	authService       *auth.Service
	vcsRegistry       *vcs.ProviderRegistry
	rbacService       *rbac.Service
}

func NewVCSConnectionHandlerV2(
	vcsConnectionRepo *repository.VCSConnectionRepository,
	orgRepo *repository.OrganizationRepository,
	authService *auth.Service,
	vcsRegistry *vcs.ProviderRegistry,
	rbacService *rbac.Service,
) *VCSConnectionHandlerV2 {
	return &VCSConnectionHandlerV2{
		vcsConnectionRepo: vcsConnectionRepo,
		orgRepo:           orgRepo,
		authService:       authService,
		vcsRegistry:       vcsRegistry,
		rbacService:       rbacService,
	}
}

type CreateVCSConnectionRequestV2 struct {
	Provider       string `json:"provider" binding:"required"` // "github", "gitlab", "bitbucket", "azure_devops"
	InstallationID string `json:"installation_id,omitempty"`
	AccessToken    string `json:"access_token" binding:"required"` //nolint:gosec // G117: token field, encrypted before storage
	RefreshToken   string `json:"refresh_token,omitempty"`         //nolint:gosec // G117: token field
	TokenExpiresAt string `json:"token_expires_at,omitempty"`      // ISO 8601 format
	AccountName    string `json:"account_name" binding:"required"`
	AccountType    string `json:"account_type" binding:"required"` // "organization" or "user"
}

// List lists all VCS connections for an organization
// GET /api/v2/organizations/:name/vcs-connections
func (h *VCSConnectionHandlerV2) List(c *gin.Context) {
	orgName := c.Param("name")

	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}},
		})
		return
	}

	connections, err := h.vcsConnectionRepo.ListByOrganization(org.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to list VCS connections"}},
		})
		return
	}

	responseData := make([]gin.H, 0, len(connections))
	for _, conn := range connections {
		responseData = append(responseData, gin.H{
			"id":   conn.ID,
			"type": "vcs-connections",
			"attributes": gin.H{
				"provider":         conn.Provider,
				"account_name":     conn.AccountName,
				"account_type":     conn.AccountType,
				"token_expires_at": conn.TokenExpiresAt,
				"created_at":       conn.CreatedAt,
				"updated_at":       conn.UpdatedAt,
			},
			"relationships": gin.H{
				"organization": gin.H{"data": gin.H{"id": org.ID, "type": "organizations"}},
			},
		})
	}

	c.JSON(http.StatusOK, gin.H{"data": responseData})
}

// Create creates a new VCS connection
// POST /api/v2/organizations/:name/vcs-connections
func (h *VCSConnectionHandlerV2) Create(c *gin.Context) {
	orgName := c.Param("name")

	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}},
		})
		return
	}

	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}},
		})
		return
	}

	hasPermission, err := h.rbacService.CheckOrgManageVCSSettings(c.Request.Context(), user.ID, org.ID)
	if err != nil || !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{{"status": "403", "title": "Forbidden", "detail": "You do not have permission to manage VCS connections. This requires organization-level manage-vcs-settings permission via team membership."}},
		})
		return
	}

	var req CreateVCSConnectionRequestV2
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}},
		})
		return
	}

	provider := models.VCSProvider(req.Provider)
	if provider != models.VCSProviderGitHub &&
		provider != models.VCSProviderGitLab &&
		provider != models.VCSProviderBitbucket &&
		provider != models.VCSProviderAzureDevOps {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid provider. Must be 'github', 'gitlab', 'bitbucket', or 'azure_devops'"}},
		})
		return
	}

	existing, _ := h.vcsConnectionRepo.GetByOrganizationAndProvider(org.ID, provider)
	if existing != nil {
		c.JSON(http.StatusConflict, gin.H{
			"errors": []gin.H{{"status": "409", "title": "Conflict", "detail": "VCS connection for this provider already exists"}},
		})
		return
	}

	var tokenExpiresAt *time.Time
	if req.TokenExpiresAt != "" {
		parsed, err := time.Parse(time.RFC3339, req.TokenExpiresAt)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid token_expires_at format. Use ISO 8601 format (RFC3339)"}},
			})
			return
		}
		tokenExpiresAt = &parsed
	}

	// TODO: Encrypt access token and refresh token before storage
	connection := &models.VCSConnection{
		OrganizationID: org.ID,
		Provider:       provider,
		InstallationID: req.InstallationID,
		AccessToken:    req.AccessToken,  // TODO: Encrypt
		RefreshToken:   req.RefreshToken, // TODO: Encrypt
		TokenExpiresAt: tokenExpiresAt,
		AccountName:    req.AccountName,
		AccountType:    req.AccountType,
	}

	if err := h.vcsConnectionRepo.Create(connection); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to create VCS connection"}},
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"data": gin.H{
			"id":   connection.ID,
			"type": "vcs-connections",
			"attributes": gin.H{
				"provider":         connection.Provider,
				"account_name":     connection.AccountName,
				"account_type":     connection.AccountType,
				"token_expires_at": connection.TokenExpiresAt,
				"created_at":       connection.CreatedAt,
				"updated_at":       connection.UpdatedAt,
			},
			"relationships": gin.H{
				"organization": gin.H{"data": gin.H{"id": org.ID, "type": "organizations"}},
			},
		},
	})
}

// Get returns a VCS connection by ID
// GET /api/v2/vcs-connections/:id
func (h *VCSConnectionHandlerV2) Get(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid VCS connection ID"}},
		})
		return
	}

	connection, err := h.vcsConnectionRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "VCS connection not found"}},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"id":   connection.ID,
			"type": "vcs-connections",
			"attributes": gin.H{
				"provider":         connection.Provider,
				"account_name":     connection.AccountName,
				"account_type":     connection.AccountType,
				"token_expires_at": connection.TokenExpiresAt,
				"created_at":       connection.CreatedAt,
				"updated_at":       connection.UpdatedAt,
			},
			"relationships": gin.H{
				"organization": gin.H{"data": gin.H{"id": connection.OrganizationID, "type": "organizations"}},
			},
		},
	})
}

// Delete deletes a VCS connection
// DELETE /api/v2/vcs-connections/:id
func (h *VCSConnectionHandlerV2) Delete(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid VCS connection ID"}},
		})
		return
	}

	connection, err := h.vcsConnectionRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "VCS connection not found"}},
		})
		return
	}

	org, err := h.orgRepo.GetByID(connection.OrganizationID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to retrieve organization"}},
		})
		return
	}

	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}},
		})
		return
	}

	hasPermission, err := h.rbacService.CheckOrgManageVCSSettings(c.Request.Context(), user.ID, org.ID)
	if err != nil || !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{{"status": "403", "title": "Forbidden", "detail": "You do not have permission to manage VCS connections. This requires organization-level manage-vcs-settings permission via team membership."}},
		})
		return
	}

	if err := h.vcsConnectionRepo.Delete(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to delete VCS connection"}},
		})
		return
	}

	c.Status(http.StatusNoContent)
}

// getProvider resolves the ProviderService for a connection and handles error responses.
// Returns nil if an error was written to c.
func (h *VCSConnectionHandlerV2) getProvider(c *gin.Context, connection *models.VCSConnection) vcs.ProviderService {
	provider, err := h.vcsRegistry.GetProvider(connection)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": fmt.Sprintf("Failed to resolve VCS provider: %v", err)}},
		})
		return nil
	}
	return provider
}

// isNotImplemented reports whether an error from a provider indicates "not implemented".
func isNotImplemented(err error) bool {
	return strings.Contains(err.Error(), "not implemented")
}

// isIdentityNotMaterialized reports whether an Azure DevOps error is the "identity not materialized" error.
func isIdentityNotMaterialized(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "azure_devops_identity_not_materialized") ||
		strings.Contains(msg, "AadUserStateException") ||
		strings.Contains(msg, "not been materialized")
}

// ListRepositories lists repositories for a VCS connection
// GET /api/v2/vcs-connections/:id/repositories
func (h *VCSConnectionHandlerV2) ListRepositories(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid VCS connection ID"}},
		})
		return
	}

	connection, err := h.vcsConnectionRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "VCS connection not found"}},
		})
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	perPage, _ := strconv.Atoi(c.DefaultQuery("per_page", "30"))
	if perPage > 100 {
		perPage = 100
	}

	provider := h.getProvider(c, connection)
	if provider == nil {
		return
	}

	repos, err := provider.ListRepositories(c.Request.Context(), connection, page, perPage)
	if err != nil {
		switch {
		case isNotImplemented(err):
			c.JSON(http.StatusNotImplemented, gin.H{
				"errors": []gin.H{{"status": "501", "title": "Not Implemented", "detail": fmt.Sprintf("Repository listing is not yet supported for %s", connection.Provider)}},
			})
		case isIdentityNotMaterialized(err):
			c.JSON(http.StatusForbidden, gin.H{
				"errors": []gin.H{{
					"status": "403", "title": "Identity Not Materialized",
					"detail": "Your Azure DevOps identity has not been activated in this organization. " +
						"Open https://dev.azure.com/ in a browser, sign in with the same Microsoft account you used to authorize Stackweaver, " +
						"then delete this VCS connection and reconnect.",
				}},
			})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": fmt.Sprintf("Failed to list repositories: %v", err)}},
			})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": repos,
		"meta": gin.H{"pagination": gin.H{"page": page, "per_page": perPage}},
	})
}

// ListBranches lists branches for a repository
// GET /api/v2/vcs-connections/:id/repositories/:owner/:repo/branches
func (h *VCSConnectionHandlerV2) ListBranches(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid VCS connection ID"}},
		})
		return
	}

	connection, err := h.vcsConnectionRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "VCS connection not found"}},
		})
		return
	}

	owner := c.Param("owner")
	repo := c.Param("repo")

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	perPage, _ := strconv.Atoi(c.DefaultQuery("per_page", "30"))
	if perPage > 100 {
		perPage = 100
	}

	provider := h.getProvider(c, connection)
	if provider == nil {
		return
	}

	branches, err := provider.ListBranches(c.Request.Context(), connection, owner, repo, page, perPage)
	if err != nil {
		if isNotImplemented(err) {
			c.JSON(http.StatusNotImplemented, gin.H{
				"errors": []gin.H{{"status": "501", "title": "Not Implemented", "detail": fmt.Sprintf("Branch listing is not yet supported for %s", connection.Provider)}},
			})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": fmt.Sprintf("Failed to list branches: %v", err)}},
			})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": branches,
		"meta": gin.H{"pagination": gin.H{"page": page, "per_page": perPage}},
	})
}

// GetFileContent retrieves file content from a repository
// GET /api/v2/vcs-connections/:id/repositories/:owner/:repo/contents/*path
func (h *VCSConnectionHandlerV2) GetFileContent(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid VCS connection ID"}},
		})
		return
	}

	connection, err := h.vcsConnectionRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "VCS connection not found"}},
		})
		return
	}

	owner := c.Param("owner")
	repo := c.Param("repo")
	path := c.Param("path")
	if len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}
	ref := c.Query("ref")

	provider := h.getProvider(c, connection)
	if provider == nil {
		return
	}

	content, err := provider.GetFileContent(c.Request.Context(), connection, owner, repo, path, ref)
	if err != nil {
		if isNotImplemented(err) {
			c.JSON(http.StatusNotImplemented, gin.H{
				"errors": []gin.H{{"status": "501", "title": "Not Implemented", "detail": fmt.Sprintf("File content retrieval is not yet supported for %s", connection.Provider)}},
			})
		} else {
			c.JSON(http.StatusNotFound, gin.H{
				"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": fmt.Sprintf("Failed to get file content: %v", err)}},
			})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{"content": content, "path": path, "ref": ref},
	})
}

// ListYamlFiles lists all .yaml and .yml files in a repository
// GET /api/v2/vcs-connections/:id/repositories/:owner/:repo/yaml-files
func (h *VCSConnectionHandlerV2) ListYamlFiles(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid VCS connection ID"}},
		})
		return
	}

	connection, err := h.vcsConnectionRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "VCS connection not found"}},
		})
		return
	}

	owner := c.Param("owner")
	repo := c.Param("repo")
	ref := c.Query("ref")

	provider := h.getProvider(c, connection)
	if provider == nil {
		return
	}

	files, err := provider.ListFiles(c.Request.Context(), connection, owner, repo, ref, []string{".yaml", ".yml"})
	if err != nil {
		if isNotImplemented(err) {
			c.JSON(http.StatusNotImplemented, gin.H{
				"errors": []gin.H{{"status": "501", "title": "Not Implemented", "detail": fmt.Sprintf("YAML file listing is not yet supported for %s", connection.Provider)}},
			})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": fmt.Sprintf("Failed to list YAML files: %v", err)}},
			})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": files})
}

// ListInventoryFiles lists all inventory files (.ini, .yaml, .yml, .json) in a repository
// GET /api/v2/vcs-connections/:id/repositories/:owner/:repo/inventory-files
func (h *VCSConnectionHandlerV2) ListInventoryFiles(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "Invalid VCS connection ID"}},
		})
		return
	}

	connection, err := h.vcsConnectionRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "VCS connection not found"}},
		})
		return
	}

	owner := c.Param("owner")
	repo := c.Param("repo")
	ref := c.Query("ref")

	provider := h.getProvider(c, connection)
	if provider == nil {
		return
	}

	files, err := provider.ListFiles(c.Request.Context(), connection, owner, repo, ref, []string{".ini", ".yaml", ".yml", ".json"})
	if err != nil {
		if isNotImplemented(err) {
			c.JSON(http.StatusNotImplemented, gin.H{
				"errors": []gin.H{{"status": "501", "title": "Not Implemented", "detail": fmt.Sprintf("Inventory file listing is not yet supported for %s", connection.Provider)}},
			})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": fmt.Sprintf("Failed to list inventory files: %v", err)}},
			})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": files})
}
