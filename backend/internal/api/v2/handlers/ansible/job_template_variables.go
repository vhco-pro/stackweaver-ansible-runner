// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansible

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/rbac"
	"github.com/iac-platform/backend/internal/services/variable"
)

type JobTemplateVariableHandlerV2 struct {
	templateVariableRepo *repository.AnsibleJobTemplateVariableRepository
	templateRepo         *repository.AnsibleJobTemplateRepository
	orgRepo              *repository.OrganizationRepository
	projectRepo          *repository.ProjectRepository
	authService          *auth.Service
	rbacService          *rbac.Service
	variableService      *variable.Service
}

func NewJobTemplateVariableHandlerV2(
	templateVariableRepo *repository.AnsibleJobTemplateVariableRepository,
	templateRepo *repository.AnsibleJobTemplateRepository,
	authService *auth.Service,
	rbacService *rbac.Service,
	variableService *variable.Service,
) *JobTemplateVariableHandlerV2 {
	return &JobTemplateVariableHandlerV2{
		templateVariableRepo: templateVariableRepo,
		templateRepo:         templateRepo,
		authService:          authService,
		rbacService:          rbacService,
		variableService:      variableService,
	}
}

// SetRepositories allows setting org and project repos for building TFE-compatible links
func (h *JobTemplateVariableHandlerV2) SetRepositories(orgRepo *repository.OrganizationRepository, projectRepo *repository.ProjectRepository) {
	h.orgRepo = orgRepo
	h.projectRepo = projectRepo
}

// formatVariableResponse formats a template variable in TFE-compatible JSON:API format
func (h *JobTemplateVariableHandlerV2) formatVariableResponse(variable *models.AnsibleJobTemplateVariable, templateID uuid.UUID) gin.H {
	// TFE-compatible response format
	// Sensitive variable values must be masked in API responses
	value := variable.Value
	if variable.Sensitive {
		value = "••••••••"
	}

	return gin.H{
		"id":   variable.ID,
		"type": "vars", // TFE uses "vars" not "variables"
		"attributes": gin.H{
			"key":         variable.Key,
			"value":       value, // Masked if sensitive
			"description": variable.Description,
			"sensitive":   variable.Sensitive,
			"category":    variable.Category,
			"hcl":         variable.HCL,
		},
		"relationships": gin.H{
			"configurable": gin.H{ // TFE uses "configurable"
				"data": gin.H{
					"id":   templateID.String(),
					"type": "job-templates",
				},
			},
		},
		"links": gin.H{
			"self": fmt.Sprintf("/api/v2/ansible/job-templates/%s/vars/%s", templateID.String(), variable.ID),
		},
	}
}

// CreateVariableRequestV2 uses JSON:API format (TFE-compatible)
type CreateVariableRequestV2 struct {
	Data struct {
		Type       string `json:"type"` // Must be "vars"
		Attributes struct {
			Key         string `json:"key" binding:"required"`
			Value       string `json:"value" binding:"required"`
			Description string `json:"description,omitempty"`
			Category    string `json:"category,omitempty"`  // "terraform" or "env", defaults to "env" for Ansible
			HCL         bool   `json:"hcl,omitempty"`       // Defaults to false
			Sensitive   bool   `json:"sensitive,omitempty"` // Defaults to false
		} `json:"attributes"`
	} `json:"data"`
}

// UpdateVariableRequestV2 uses JSON:API format (TFE-compatible)
type UpdateVariableRequestV2 struct {
	Data struct {
		ID         string `json:"id"`   // Variable ID
		Type       string `json:"type"` // Must be "vars"
		Attributes struct {
			Key         string `json:"key,omitempty"`
			Value       string `json:"value,omitempty"`
			Description string `json:"description,omitempty"`
			Category    string `json:"category,omitempty"`
			HCL         *bool  `json:"hcl,omitempty"`
			Sensitive   *bool  `json:"sensitive,omitempty"`
		} `json:"attributes"`
	} `json:"data"`
}

// ListByJobTemplate lists variables for a job template
// GET /api/v2/ansible/job-templates/:id/vars
func (h *JobTemplateVariableHandlerV2) ListByJobTemplate(c *gin.Context) {
	templateIDStr := c.Param("id")
	templateID, err := uuid.Parse(templateIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid job template ID"})
		return
	}

	// Get template to verify it exists
	_, err = h.templateRepo.GetByID(templateID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Job template not found"})
		return
	}

	// Check RBAC permissions - verify user is authenticated
	_, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	// List variables
	variables, err := h.templateVariableRepo.ListByJobTemplate(templateID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list variables"})
		return
	}

	// Format response (TFE-compatible JSON:API format)
	data := make([]gin.H, 0, len(variables))
	for _, v := range variables {
		data = append(data, h.formatVariableResponse(&v, templateID))
	}

	c.JSON(http.StatusOK, gin.H{
		"data": data,
		"links": gin.H{
			"self": fmt.Sprintf("/api/v2/ansible/job-templates/%s/vars", templateIDStr),
		},
	})
}

// Create creates a variable for a job template
// POST /api/v2/ansible/job-templates/:id/vars
func (h *JobTemplateVariableHandlerV2) Create(c *gin.Context) {
	templateIDStr := c.Param("id")
	templateID, err := uuid.Parse(templateIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid job template ID"})
		return
	}

	// Get template to verify it exists
	_, err = h.templateRepo.GetByID(templateID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Job template not found"})
		return
	}

	// Check RBAC permissions - verify user is authenticated
	_, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	// Parse request
	var req CreateVariableRequestV2
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Data.Type != "vars" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid type: must be 'vars'"})
		return
	}

	// Set default category to "env" for Ansible if not specified
	category := req.Data.Attributes.Category
	if category == "" {
		category = "env"
	}

	// Encrypt value if sensitive
	var finalValue string
	var encrypted bool
	if req.Data.Attributes.Sensitive {
		encryptedValue, err := h.variableService.Encrypt(req.Data.Attributes.Value)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to encrypt variable"})
			return
		}
		finalValue = encryptedValue
		encrypted = true
	} else {
		finalValue = req.Data.Attributes.Value
		encrypted = false
	}

	// Create variable
	variable := &models.AnsibleJobTemplateVariable{
		JobTemplateID: templateID,
		Key:           req.Data.Attributes.Key,
		Value:         finalValue,
		Description:   req.Data.Attributes.Description,
		Category:      category,
		HCL:           req.Data.Attributes.HCL,
		Encrypted:     encrypted,
		Sensitive:     req.Data.Attributes.Sensitive,
	}

	if err := h.templateVariableRepo.Create(variable); err != nil {
		// Check for duplicate key error
		if err.Error() == "pq: duplicate key value violates unique constraint \"idx_job_template_key\"" {
			c.JSON(http.StatusConflict, gin.H{"error": "Variable with this key already exists"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create variable"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"data": h.formatVariableResponse(variable, templateID),
	})
}

// Update updates a variable for a job template
// PATCH /api/v2/ansible/job-templates/:id/vars/:variable_id
func (h *JobTemplateVariableHandlerV2) Update(c *gin.Context) {
	templateIDStr := c.Param("id")
	templateID, err := uuid.Parse(templateIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid job template ID"})
		return
	}

	variableID := c.Param("variable_id")
	if variableID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Variable ID is required"})
		return
	}

	// Get template to verify it exists
	_, err = h.templateRepo.GetByID(templateID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Job template not found"})
		return
	}

	// Check RBAC permissions - verify user is authenticated
	_, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	// Get existing variable
	variable, err := h.templateVariableRepo.GetByID(variableID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Variable not found"})
		return
	}

	// Verify variable belongs to this template
	if variable.JobTemplateID != templateID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Variable does not belong to this job template"})
		return
	}

	// Parse request
	var req UpdateVariableRequestV2
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Data.Type != "vars" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid type: must be 'vars'"})
		return
	}

	// Update fields if provided
	if req.Data.Attributes.Key != "" {
		variable.Key = req.Data.Attributes.Key
	}
	if req.Data.Attributes.Value != "" {
		// If sensitive, encrypt the new value
		if variable.Sensitive {
			encryptedValue, err := h.variableService.Encrypt(req.Data.Attributes.Value)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to encrypt variable"})
				return
			}
			variable.Value = encryptedValue
		} else {
			variable.Value = req.Data.Attributes.Value
		}
	}
	if req.Data.Attributes.Description != "" {
		variable.Description = req.Data.Attributes.Description
	}
	if req.Data.Attributes.Category != "" {
		variable.Category = req.Data.Attributes.Category
	}
	if req.Data.Attributes.HCL != nil {
		variable.HCL = *req.Data.Attributes.HCL
	}
	if req.Data.Attributes.Sensitive != nil {
		// If changing from non-sensitive to sensitive, encrypt the value
		if *req.Data.Attributes.Sensitive && !variable.Sensitive {
			encryptedValue, err := h.variableService.Encrypt(variable.Value)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to encrypt variable"})
				return
			}
			variable.Value = encryptedValue
			variable.Encrypted = true
		} else if !*req.Data.Attributes.Sensitive && variable.Sensitive {
			// If changing from sensitive to non-sensitive, decrypt the value
			decryptedValue, err := h.variableService.GetDecryptedTemplateVariableValue(variable)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to decrypt variable"})
				return
			}
			variable.Value = decryptedValue
			variable.Encrypted = false
		}
		variable.Sensitive = *req.Data.Attributes.Sensitive
	}

	// Save updated variable
	if err := h.templateVariableRepo.Update(variable); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update variable"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": h.formatVariableResponse(variable, templateID),
	})
}

// Delete deletes a variable for a job template
// DELETE /api/v2/ansible/job-templates/:id/vars/:variable_id
func (h *JobTemplateVariableHandlerV2) Delete(c *gin.Context) {
	templateIDStr := c.Param("id")
	templateID, err := uuid.Parse(templateIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid job template ID"})
		return
	}

	variableID := c.Param("variable_id")
	if variableID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Variable ID is required"})
		return
	}

	// Get template to verify it exists
	_, err = h.templateRepo.GetByID(templateID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Job template not found"})
		return
	}

	// Check RBAC permissions - verify user is authenticated
	_, exists := c.Get("user_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
		return
	}

	// Verify variable belongs to this template
	variable, err := h.templateVariableRepo.GetByID(variableID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Variable not found"})
		return
	}
	if variable.JobTemplateID != templateID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Variable does not belong to this job template"})
		return
	}

	// Delete variable
	if err := h.templateVariableRepo.Delete(variableID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete variable"})
		return
	}

	c.Status(http.StatusNoContent)
}
