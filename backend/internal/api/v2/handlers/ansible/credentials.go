// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansible

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/ansible"
	"github.com/iac-platform/backend/internal/services/auth"
)

// CredentialHandler handles Ansible credential API endpoints
type CredentialHandler struct {
	credentialService *ansible.CredentialService
	orgRepo           *repository.OrganizationRepository
	projectRepo       *repository.ProjectRepository
	authService       *auth.Service
}

// NewCredentialHandler creates a new credential handler
func NewCredentialHandler(
	credentialService *ansible.CredentialService,
	orgRepo *repository.OrganizationRepository,
	projectRepo *repository.ProjectRepository,
	authService *auth.Service,
) *CredentialHandler {
	return &CredentialHandler{
		credentialService: credentialService,
		orgRepo:           orgRepo,
		projectRepo:       projectRepo,
		authService:       authService,
	}
}

// CreateCredentialRequest represents the request to create a credential
type CreateCredentialRequest struct {
	Data struct {
		Type       string `json:"type"`
		Attributes struct {
			Name               string `json:"name" binding:"required"`
			Description        string `json:"description"`
			Type               string `json:"credential-type" binding:"required"`
			Username           string `json:"username"`
			SSHPrivateKey      string `json:"ssh-private-key"` //nolint:gosec // G117: credential field
			SSHPassphrase      string `json:"ssh-passphrase"`
			Password           string `json:"password"`              //nolint:gosec // G117: credential field
			VaultPassword      string `json:"vault-password"`        //nolint:gosec // G117: credential field
			BecomePassword     string `json:"become-password"`       //nolint:gosec // G117: credential field
			AWSAccessKeyID     string `json:"aws-access-key-id"`     //nolint:gosec // G117: credential field
			AWSSecretAccessKey string `json:"aws-secret-access-key"` //nolint:gosec // G117: credential field
			AzureTenantID      string `json:"azure-tenant-id"`
			AzureClientID      string `json:"azure-client-id"`
			AzureClientSecret  string `json:"azure-client-secret"` //nolint:gosec // G117: credential field
			GCPServiceAccount  string `json:"gcp-service-account"`
			SSHPort            int    `json:"ssh-port"`
			SSHBecomeUser      string `json:"ssh-become-user"`
		} `json:"attributes"`
		Relationships struct {
			Project struct {
				Data *struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"project"`
		} `json:"relationships"`
	} `json:"data"`
}

// UpdateCredentialRequest represents the request to update a credential
type UpdateCredentialRequest struct {
	Data struct {
		Type       string `json:"type"`
		Attributes struct {
			Name               *string `json:"name"`
			Description        *string `json:"description"`
			Username           *string `json:"username"`
			SSHPrivateKey      *string `json:"ssh-private-key"` //nolint:gosec // G117: credential field
			SSHPassphrase      *string `json:"ssh-passphrase"`
			Password           *string `json:"password"`              //nolint:gosec // G117: credential field
			VaultPassword      *string `json:"vault-password"`        //nolint:gosec // G117: credential field
			BecomePassword     *string `json:"become-password"`       //nolint:gosec // G117: credential field
			AWSAccessKeyID     *string `json:"aws-access-key-id"`     //nolint:gosec // G117: credential field
			AWSSecretAccessKey *string `json:"aws-secret-access-key"` //nolint:gosec // G117: credential field
			AzureTenantID      *string `json:"azure-tenant-id"`
			AzureClientID      *string `json:"azure-client-id"`
			AzureClientSecret  *string `json:"azure-client-secret"` //nolint:gosec // G117: credential field
			GCPServiceAccount  *string `json:"gcp-service-account"`
			SSHPort            *int    `json:"ssh-port"`
			SSHBecomeUser      *string `json:"ssh-become-user"`
		} `json:"attributes"`
		Relationships struct {
			Project struct {
				Data *struct {
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"data"`
			} `json:"project"`
		} `json:"relationships"`
	} `json:"data"`
}

// List lists all credentials for an organization
// GET /api/v2/organizations/:name/ansible/credentials
func (h *CredentialHandler) List(c *gin.Context) {
	orgName := c.Param("name")

	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "Organization not found"},
			},
		})
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page[number]", "1"))
	perPage, _ := strconv.Atoi(c.DefaultQuery("page[size]", "20"))
	if perPage > 100 {
		perPage = 100
	}
	offset := (page - 1) * perPage

	// Optional type filter
	credTypeFilter := c.Query("filter[type]")

	var credentials []models.AnsibleCredential
	var total int64

	if credTypeFilter != "" {
		credType := models.CredentialType(credTypeFilter)
		credentials, total, err = h.credentialService.ListCredentialsByType(org.ID, credType, perPage, offset)
	} else {
		credentials, total, err = h.credentialService.ListCredentials(org.ID, perPage, offset)
	}

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to list credentials"},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatCredentialsResponse(credentials),
		"meta": gin.H{
			"pagination": gin.H{
				"current-page": page,
				"page-size":    perPage,
				"total-count":  total,
				"total-pages":  (total + int64(perPage) - 1) / int64(perPage),
			},
		},
	})
}

// Create creates a new credential
// POST /api/v2/organizations/:name/ansible/credentials
func (h *CredentialHandler) Create(c *gin.Context) {
	orgName := c.Param("name")

	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "Organization not found"},
			},
		})
		return
	}

	var req CreateCredentialRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": err.Error()},
			},
		})
		return
	}

	// Validate credential type
	credType := models.CredentialType(req.Data.Attributes.Type)
	switch credType {
	case models.CredentialTypeSSH, models.CredentialTypeSCM, models.CredentialTypeVault,
		models.CredentialTypeMachineSSH, models.CredentialTypeAWSAccessKey,
		models.CredentialTypeAzure, models.CredentialTypeGCP, models.CredentialTypeVMware:
		// Valid
	default:
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid credential type"},
			},
		})
		return
	}

	// Parse project ID if provided, otherwise use default project
	var projectID *uuid.UUID
	if req.Data.Relationships.Project.Data != nil {
		pid, err := uuid.Parse(req.Data.Relationships.Project.Data.ID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{"status": "400", "title": "Bad Request", "detail": "Invalid project ID"},
				},
			})
			return
		}
		// Verify project belongs to organization
		project, err := h.projectRepo.GetByID(pid)
		if err != nil || project.OrganizationID != org.ID {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{"status": "400", "title": "Bad Request", "detail": "Project not found or does not belong to organization"},
				},
			})
			return
		}
		projectID = &pid
	} else {
		// Use default project
		defaultProject, err := h.projectRepo.GetByOrganizationAndName(org.ID, "default")
		if err == nil && defaultProject != nil {
			projectID = &defaultProject.ID
		} else {
			// Create default project if it doesn't exist
			defaultProject = &models.Project{
				OrganizationID: org.ID,
				Name:           "default",
				Description:    "Default project for your organization",
			}
			if err := h.projectRepo.Create(defaultProject); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{
					"errors": []gin.H{
						{"status": "500", "title": "Internal Server Error", "detail": "Failed to get default project"},
					},
				})
				return
			}
			projectID = &defaultProject.ID
		}
	}

	input := ansible.CreateCredentialInput{
		OrganizationID:     org.ID,
		ProjectID:          projectID,
		Name:               req.Data.Attributes.Name,
		Description:        req.Data.Attributes.Description,
		Type:               credType,
		Username:           req.Data.Attributes.Username,
		SSHPrivateKey:      req.Data.Attributes.SSHPrivateKey,
		SSHPassphrase:      req.Data.Attributes.SSHPassphrase,
		Password:           req.Data.Attributes.Password,
		VaultPassword:      req.Data.Attributes.VaultPassword,
		BecomePassword:     req.Data.Attributes.BecomePassword,
		AWSAccessKeyID:     req.Data.Attributes.AWSAccessKeyID,
		AWSSecretAccessKey: req.Data.Attributes.AWSSecretAccessKey,
		AzureTenantID:      req.Data.Attributes.AzureTenantID,
		AzureClientID:      req.Data.Attributes.AzureClientID,
		AzureClientSecret:  req.Data.Attributes.AzureClientSecret,
		GCPServiceAccount:  req.Data.Attributes.GCPServiceAccount,
		SSHPort:            req.Data.Attributes.SSHPort,
		SSHBecomeUser:      req.Data.Attributes.SSHBecomeUser,
	}

	credential, err := h.credentialService.CreateCredential(input)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": err.Error()},
			},
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"data": formatCredentialResponse(credential),
	})
}

// Get retrieves a credential by ID
// GET /api/v2/ansible/credentials/:id
func (h *CredentialHandler) Get(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid credential ID"},
			},
		})
		return
	}

	credential, err := h.credentialService.GetCredential(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{
				{"status": "404", "title": "Not Found", "detail": "Credential not found"},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatCredentialResponse(credential),
	})
}

// Update updates a credential
// PATCH /api/v2/ansible/credentials/:id
func (h *CredentialHandler) Update(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid credential ID"},
			},
		})
		return
	}

	var req UpdateCredentialRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": err.Error()},
			},
		})
		return
	}

	// Parse project ID if provided
	var projectID *uuid.UUID
	if req.Data.Relationships.Project.Data != nil {
		pid, err := uuid.Parse(req.Data.Relationships.Project.Data.ID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{"status": "400", "title": "Bad Request", "detail": "Invalid project ID"},
				},
			})
			return
		}
		// Get existing credential to verify organization
		existingCredential, err := h.credentialService.GetCredential(id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{
				"errors": []gin.H{
					{"status": "404", "title": "Not Found", "detail": "Credential not found"},
				},
			})
			return
		}
		// Verify project belongs to same organization
		project, err := h.projectRepo.GetByID(pid)
		if err != nil || project.OrganizationID != existingCredential.OrganizationID {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{
					{"status": "400", "title": "Bad Request", "detail": "Project not found or does not belong to organization"},
				},
			})
			return
		}
		projectID = &pid
	}

	input := ansible.UpdateCredentialInput{
		ProjectID:          projectID,
		Name:               req.Data.Attributes.Name,
		Description:        req.Data.Attributes.Description,
		Username:           req.Data.Attributes.Username,
		SSHPrivateKey:      req.Data.Attributes.SSHPrivateKey,
		SSHPassphrase:      req.Data.Attributes.SSHPassphrase,
		Password:           req.Data.Attributes.Password,
		VaultPassword:      req.Data.Attributes.VaultPassword,
		BecomePassword:     req.Data.Attributes.BecomePassword,
		AWSAccessKeyID:     req.Data.Attributes.AWSAccessKeyID,
		AWSSecretAccessKey: req.Data.Attributes.AWSSecretAccessKey,
		AzureTenantID:      req.Data.Attributes.AzureTenantID,
		AzureClientID:      req.Data.Attributes.AzureClientID,
		AzureClientSecret:  req.Data.Attributes.AzureClientSecret,
		GCPServiceAccount:  req.Data.Attributes.GCPServiceAccount,
		SSHPort:            req.Data.Attributes.SSHPort,
		SSHBecomeUser:      req.Data.Attributes.SSHBecomeUser,
	}

	credential, err := h.credentialService.UpdateCredential(id, input)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": err.Error()},
			},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": formatCredentialResponse(credential),
	})
}

// Delete deletes a credential
// DELETE /api/v2/ansible/credentials/:id
func (h *CredentialHandler) Delete(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{
				{"status": "400", "title": "Bad Request", "detail": "Invalid credential ID"},
			},
		})
		return
	}

	if err := h.credentialService.DeleteCredential(id); err != nil {
		// Check for foreign key constraint violation
		errStr := err.Error()
		if strings.Contains(errStr, "violates foreign key constraint") {
			c.JSON(http.StatusConflict, gin.H{
				"errors": []gin.H{
					{"status": "409", "title": "Conflict", "detail": "Cannot delete credential: it is referenced by one or more job templates, jobs, or inventory sources. Remove the credential from those resources first."},
				},
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": err.Error()},
			},
		})
		return
	}

	c.Status(http.StatusNoContent)
}

// formatCredentialResponse formats a credential for JSON:API response
// Note: Sensitive fields are never included in responses
func formatCredentialResponse(cred *models.AnsibleCredential) gin.H {
	return gin.H{
		"id":   cred.ID.String(),
		"type": "ansible-credentials",
		"attributes": gin.H{
			"name":                cred.Name,
			"description":         cred.Description,
			"credential-type":     cred.Type,
			"username":            cred.Username,
			"azure-tenant-id":     cred.AzureTenantID,
			"azure-client-id":     cred.AzureClientID,
			"ssh-port":            cred.SSHPort,
			"ssh-become-user":     cred.SSHBecomeUser,
			"has-ssh-private-key": cred.HasSSHPrivateKey,
			"has-password":        cred.HasPassword,
			"has-vault-password":  cred.HasVaultPassword,
			"has-become-password": cred.HasBecomePassword,
			"created-at":          cred.CreatedAt.Format("2006-01-02T15:04:05Z"),
			"updated-at":          cred.UpdatedAt.Format("2006-01-02T15:04:05Z"),
		},
		"relationships": gin.H{
			"organization": gin.H{
				"data": gin.H{
					"id":   cred.OrganizationID.String(),
					"type": "organizations",
				},
			},
		},
	}
}

// formatCredentialsResponse formats multiple credentials for JSON:API response
func formatCredentialsResponse(credentials []models.AnsibleCredential) []gin.H {
	result := make([]gin.H, len(credentials))
	for i, cred := range credentials {
		result[i] = formatCredentialResponse(&cred)
	}
	return result
}
