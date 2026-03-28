// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansible

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/rbac"
	"gorm.io/datatypes"
)

// WorkflowHandler handles Ansible workflow template endpoints
type WorkflowHandler struct {
	workflowRepo *repository.AnsibleWorkflowRepository
	orgRepo      *repository.OrganizationRepository
	projectRepo  *repository.ProjectRepository
	authService  *auth.Service
	rbacService  *rbac.Service
}

// NewWorkflowHandler creates a new WorkflowHandler
func NewWorkflowHandler(
	workflowRepo *repository.AnsibleWorkflowRepository,
	orgRepo *repository.OrganizationRepository,
	projectRepo *repository.ProjectRepository,
	authService *auth.Service,
	rbacService *rbac.Service,
) *WorkflowHandler {
	return &WorkflowHandler{
		workflowRepo: workflowRepo,
		orgRepo:      orgRepo,
		projectRepo:  projectRepo,
		authService:  authService,
		rbacService:  rbacService,
	}
}

// ============================================================================
// Request/Response types
// ============================================================================

type CreateWorkflowRequest struct {
	Data struct {
		Type       string `json:"type"`
		Attributes struct {
			Name                 string `json:"name"`
			Description          string `json:"description"`
			AllowSimultaneous    bool   `json:"allow-simultaneous"`
			AskVariablesOnLaunch bool   `json:"ask-variables-on-launch"`
			AskInventoryOnLaunch bool   `json:"ask-inventory-on-launch"`
			AskLimitOnLaunch     bool   `json:"ask-limit-on-launch"`
			ExtraVars            string `json:"extra-vars"`
			Limit                string `json:"limit"`
			SurveyEnabled        bool   `json:"survey-enabled"`
		} `json:"attributes"`
		Relationships struct {
			Project struct {
				Data *struct {
					ID string `json:"id"`
				} `json:"data"`
			} `json:"project"`
			Inventory struct {
				Data *struct {
					ID string `json:"id"`
				} `json:"data"`
			} `json:"inventory"`
		} `json:"relationships"`
	} `json:"data"`
}

type CreateNodeRequest struct {
	Data struct {
		Type       string `json:"type"`
		Attributes struct {
			NodeType               string  `json:"node-type"`
			Identifier             string  `json:"identifier"`
			PositionX              float64 `json:"position-x"`
			PositionY              float64 `json:"position-y"`
			ExtraVars              string  `json:"extra-vars"`
			Limit                  string  `json:"limit"`
			Tags                   string  `json:"tags"`
			SkipTags               string  `json:"skip-tags"`
			Verbosity              int     `json:"verbosity"`
			AllParentsMustConverge bool    `json:"all-parents-must-converge"`
			ApprovalTimeout        int     `json:"approval-timeout"`
			ApprovalMessage        string  `json:"approval-message"`
		} `json:"attributes"`
		Relationships struct {
			JobTemplate struct {
				Data *struct {
					ID string `json:"id"`
				} `json:"data"`
			} `json:"job-template"`
			Inventory struct {
				Data *struct {
					ID string `json:"id"`
				} `json:"data"`
			} `json:"inventory"`
			Credential struct {
				Data *struct {
					ID string `json:"id"`
				} `json:"data"`
			} `json:"credential"`
		} `json:"relationships"`
	} `json:"data"`
}

type CreateEdgeRequest struct {
	Data struct {
		Type       string `json:"type"`
		Attributes struct {
			Condition string `json:"condition"` // on_success, on_failure, always
		} `json:"attributes"`
		Relationships struct {
			SourceNode struct {
				Data struct {
					ID string `json:"id"`
				} `json:"data"`
			} `json:"source-node"`
			TargetNode struct {
				Data struct {
					ID string `json:"id"`
				} `json:"data"`
			} `json:"target-node"`
		} `json:"relationships"`
	} `json:"data"`
}

// ============================================================================
// Workflow CRUD
// ============================================================================

// List lists workflows for an organization
// GET /api/v2/organizations/:name/ansible/workflows
func (h *WorkflowHandler) List(c *gin.Context) {
	orgName := c.Param("name")
	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"detail": "Organization not found"}}})
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

	hasPermission, err := h.rbacService.CheckOrgReadAnsible(c.Request.Context(), user.ID, org.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to check permissions"},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "You do not have permission to list workflows in this organization"},
			},
		})
		return
	}

	limit, _ := strconv.Atoi(c.DefaultQuery("page[size]", "20"))
	offset, _ := strconv.Atoi(c.DefaultQuery("page[number]", "0"))
	if offset > 0 {
		offset = (offset - 1) * limit
	}

	workflows, total, err := h.workflowRepo.ListByOrganization(org.ID, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"detail": "Failed to list workflows"}}})
		return
	}

	data := make([]gin.H, len(workflows))
	for i, w := range workflows {
		data[i] = formatWorkflowResponse(&w)
	}

	c.JSON(http.StatusOK, gin.H{
		"data": data,
		"meta": gin.H{"total": total},
	})
}

// Create creates a new workflow
// POST /api/v2/organizations/:name/ansible/workflows
func (h *WorkflowHandler) Create(c *gin.Context) {
	orgName := c.Param("name")
	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"detail": "Organization not found"}}})
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

	hasPermission, err := h.rbacService.CheckOrgManageAnsible(c.Request.Context(), user.ID, org.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to check permissions"},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "You do not have permission to create workflows in this organization"},
			},
		})
		return
	}

	var req CreateWorkflowRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"detail": err.Error()}}})
		return
	}

	workflow := &models.AnsibleWorkflow{
		OrganizationID:       org.ID,
		Name:                 req.Data.Attributes.Name,
		Description:          req.Data.Attributes.Description,
		AllowSimultaneous:    req.Data.Attributes.AllowSimultaneous,
		AskVariablesOnLaunch: req.Data.Attributes.AskVariablesOnLaunch,
		AskInventoryOnLaunch: req.Data.Attributes.AskInventoryOnLaunch,
		AskLimitOnLaunch:     req.Data.Attributes.AskLimitOnLaunch,
		Limit:                req.Data.Attributes.Limit,
		SurveyEnabled:        req.Data.Attributes.SurveyEnabled,
	}

	if req.Data.Attributes.ExtraVars != "" {
		workflow.ExtraVars = datatypes.JSON([]byte(req.Data.Attributes.ExtraVars))
	}

	if req.Data.Relationships.Project.Data != nil {
		projectID, _ := uuid.Parse(req.Data.Relationships.Project.Data.ID)
		workflow.ProjectID = projectID
	}

	if req.Data.Relationships.Inventory.Data != nil {
		inventoryID, _ := uuid.Parse(req.Data.Relationships.Inventory.Data.ID)
		workflow.InventoryID = &inventoryID
	}

	workflow.CreatedBy = &user.ID

	if err := h.workflowRepo.Create(workflow); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"detail": "Failed to create workflow"}}})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": formatWorkflowResponse(workflow)})
}

// Get retrieves a workflow by ID
// GET /api/v2/ansible/workflows/:id
func (h *WorkflowHandler) Get(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"detail": "Invalid workflow ID"}}})
		return
	}

	workflow, edges, err := h.workflowRepo.GetByIDWithEdges(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"detail": "Workflow not found"}}})
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

	hasPermission, err := h.rbacService.CheckAnsibleResourcePermission(
		c.Request.Context(),
		user.ID,
		rbac.ResourceTypeAnsibleJobTemplate,
		workflow.ID.String(),
		rbac.PermissionAnsibleJobTemplateRead,
		&workflow.ProjectID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to check permissions"},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "You do not have permission to view this workflow"},
			},
		})
		return
	}

	response := formatWorkflowResponse(workflow)

	// Add nodes and edges
	nodesData := make([]gin.H, len(workflow.Nodes))
	for i, node := range workflow.Nodes {
		nodesData[i] = formatNodeResponse(&node)
	}
	response["relationships"].(gin.H)["nodes"] = gin.H{"data": nodesData}

	edgesData := make([]gin.H, len(edges))
	for i, edge := range edges {
		edgesData[i] = formatEdgeResponse(&edge)
	}
	response["relationships"].(gin.H)["edges"] = gin.H{"data": edgesData}

	c.JSON(http.StatusOK, gin.H{"data": response})
}

// Update updates a workflow
// PATCH /api/v2/ansible/workflows/:id
func (h *WorkflowHandler) Update(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"detail": "Invalid workflow ID"}}})
		return
	}

	workflow, err := h.workflowRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"detail": "Workflow not found"}}})
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

	hasPermission, err := h.rbacService.CheckAnsibleResourcePermission(
		c.Request.Context(),
		user.ID,
		rbac.ResourceTypeAnsibleJobTemplate,
		workflow.ID.String(),
		rbac.PermissionAnsibleJobTemplateWrite,
		&workflow.ProjectID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to check permissions"},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "You do not have permission to update this workflow"},
			},
		})
		return
	}

	var req CreateWorkflowRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"detail": err.Error()}}})
		return
	}

	workflow.Name = req.Data.Attributes.Name
	workflow.Description = req.Data.Attributes.Description
	workflow.AllowSimultaneous = req.Data.Attributes.AllowSimultaneous
	workflow.AskVariablesOnLaunch = req.Data.Attributes.AskVariablesOnLaunch
	workflow.AskInventoryOnLaunch = req.Data.Attributes.AskInventoryOnLaunch
	workflow.AskLimitOnLaunch = req.Data.Attributes.AskLimitOnLaunch
	workflow.Limit = req.Data.Attributes.Limit
	workflow.SurveyEnabled = req.Data.Attributes.SurveyEnabled

	if req.Data.Attributes.ExtraVars != "" {
		workflow.ExtraVars = datatypes.JSON([]byte(req.Data.Attributes.ExtraVars))
	}

	if err := h.workflowRepo.Update(workflow); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"detail": "Failed to update workflow"}}})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": formatWorkflowResponse(workflow)})
}

// Delete deletes a workflow
// DELETE /api/v2/ansible/workflows/:id
func (h *WorkflowHandler) Delete(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"detail": "Invalid workflow ID"}}})
		return
	}

	workflow, err := h.workflowRepo.GetByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"detail": "Workflow not found"}}})
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

	hasPermission, err := h.rbacService.CheckAnsibleResourcePermission(
		c.Request.Context(),
		user.ID,
		rbac.ResourceTypeAnsibleJobTemplate,
		workflow.ID.String(),
		rbac.PermissionAnsibleJobTemplateWrite,
		&workflow.ProjectID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to check permissions"},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "You do not have permission to delete this workflow"},
			},
		})
		return
	}

	if err := h.workflowRepo.Delete(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"detail": "Failed to delete workflow"}}})
		return
	}

	c.Status(http.StatusNoContent)
}

// ============================================================================
// Node operations
// ============================================================================

// CreateNode creates a new node in a workflow
// POST /api/v2/ansible/workflows/:id/nodes
func (h *WorkflowHandler) CreateNode(c *gin.Context) {
	workflowIDStr := c.Param("id")
	workflowID, err := uuid.Parse(workflowIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"detail": "Invalid workflow ID"}}})
		return
	}

	workflow, err := h.workflowRepo.GetByID(workflowID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"detail": "Workflow not found"}}})
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

	hasPermission, err := h.rbacService.CheckAnsibleResourcePermission(
		c.Request.Context(),
		user.ID,
		rbac.ResourceTypeAnsibleJobTemplate,
		workflow.ID.String(),
		rbac.PermissionAnsibleJobTemplateWrite,
		&workflow.ProjectID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to check permissions"},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "You do not have permission to modify this workflow"},
			},
		})
		return
	}

	var req CreateNodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"detail": err.Error()}}})
		return
	}

	node := &models.AnsibleWorkflowNode{
		WorkflowID:             workflowID,
		NodeType:               models.WorkflowNodeType(req.Data.Attributes.NodeType),
		Identifier:             req.Data.Attributes.Identifier,
		PositionX:              req.Data.Attributes.PositionX,
		PositionY:              req.Data.Attributes.PositionY,
		Limit:                  req.Data.Attributes.Limit,
		Tags:                   req.Data.Attributes.Tags,
		SkipTags:               req.Data.Attributes.SkipTags,
		Verbosity:              req.Data.Attributes.Verbosity,
		AllParentsMustConverge: req.Data.Attributes.AllParentsMustConverge,
		ApprovalTimeout:        req.Data.Attributes.ApprovalTimeout,
		ApprovalMessage:        req.Data.Attributes.ApprovalMessage,
	}

	if req.Data.Attributes.ExtraVars != "" {
		node.ExtraVars = datatypes.JSON([]byte(req.Data.Attributes.ExtraVars))
	}

	if req.Data.Relationships.JobTemplate.Data != nil {
		templateID, _ := uuid.Parse(req.Data.Relationships.JobTemplate.Data.ID)
		node.JobTemplateID = &templateID
	}

	if req.Data.Relationships.Inventory.Data != nil {
		inventoryID, _ := uuid.Parse(req.Data.Relationships.Inventory.Data.ID)
		node.InventoryID = &inventoryID
	}

	if req.Data.Relationships.Credential.Data != nil {
		credentialID, _ := uuid.Parse(req.Data.Relationships.Credential.Data.ID)
		node.CredentialID = &credentialID
	}

	if err := h.workflowRepo.CreateNode(node); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"detail": "Failed to create node"}}})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": formatNodeResponse(node)})
}

// ListNodes lists nodes in a workflow
// GET /api/v2/ansible/workflows/:id/nodes
func (h *WorkflowHandler) ListNodes(c *gin.Context) {
	workflowIDStr := c.Param("id")
	workflowID, err := uuid.Parse(workflowIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"detail": "Invalid workflow ID"}}})
		return
	}

	workflow, err := h.workflowRepo.GetByID(workflowID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"detail": "Workflow not found"}}})
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

	hasPermission, err := h.rbacService.CheckAnsibleResourcePermission(
		c.Request.Context(),
		user.ID,
		rbac.ResourceTypeAnsibleJobTemplate,
		workflow.ID.String(),
		rbac.PermissionAnsibleJobTemplateRead,
		&workflow.ProjectID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to check permissions"},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "You do not have permission to view this workflow"},
			},
		})
		return
	}

	nodes, err := h.workflowRepo.ListNodesByWorkflow(workflowID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"detail": "Failed to list nodes"}}})
		return
	}

	data := make([]gin.H, len(nodes))
	for i, node := range nodes {
		data[i] = formatNodeResponse(&node)
	}

	c.JSON(http.StatusOK, gin.H{"data": data})
}

// UpdateNode updates a node
// PATCH /api/v2/ansible/workflow-nodes/:id
func (h *WorkflowHandler) UpdateNode(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"detail": "Invalid node ID"}}})
		return
	}

	node, err := h.workflowRepo.GetNodeByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"detail": "Node not found"}}})
		return
	}

	workflow, err := h.workflowRepo.GetByID(node.WorkflowID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"detail": "Workflow not found"}}})
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

	hasPermission, err := h.rbacService.CheckAnsibleResourcePermission(
		c.Request.Context(),
		user.ID,
		rbac.ResourceTypeAnsibleJobTemplate,
		workflow.ID.String(),
		rbac.PermissionAnsibleJobTemplateWrite,
		&workflow.ProjectID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to check permissions"},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "You do not have permission to modify this workflow"},
			},
		})
		return
	}

	var req CreateNodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"detail": err.Error()}}})
		return
	}

	node.PositionX = req.Data.Attributes.PositionX
	node.PositionY = req.Data.Attributes.PositionY
	node.Limit = req.Data.Attributes.Limit
	node.Tags = req.Data.Attributes.Tags
	node.SkipTags = req.Data.Attributes.SkipTags
	node.Verbosity = req.Data.Attributes.Verbosity
	node.AllParentsMustConverge = req.Data.Attributes.AllParentsMustConverge

	if req.Data.Attributes.ExtraVars != "" {
		node.ExtraVars = datatypes.JSON([]byte(req.Data.Attributes.ExtraVars))
	}

	if err := h.workflowRepo.UpdateNode(node); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"detail": "Failed to update node"}}})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": formatNodeResponse(node)})
}

// DeleteNode deletes a node
// DELETE /api/v2/ansible/workflow-nodes/:id
func (h *WorkflowHandler) DeleteNode(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"detail": "Invalid node ID"}}})
		return
	}

	node, err := h.workflowRepo.GetNodeByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"detail": "Node not found"}}})
		return
	}

	workflow, err := h.workflowRepo.GetByID(node.WorkflowID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"detail": "Workflow not found"}}})
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

	hasPermission, err := h.rbacService.CheckAnsibleResourcePermission(
		c.Request.Context(),
		user.ID,
		rbac.ResourceTypeAnsibleJobTemplate,
		workflow.ID.String(),
		rbac.PermissionAnsibleJobTemplateWrite,
		&workflow.ProjectID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to check permissions"},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "You do not have permission to modify this workflow"},
			},
		})
		return
	}

	if err := h.workflowRepo.DeleteNode(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"detail": "Failed to delete node"}}})
		return
	}

	c.Status(http.StatusNoContent)
}

// ============================================================================
// Edge operations
// ============================================================================

// CreateEdge creates a new edge between nodes
// POST /api/v2/ansible/workflows/:id/edges
func (h *WorkflowHandler) CreateEdge(c *gin.Context) {
	workflowIDStr := c.Param("id")
	workflowID, err := uuid.Parse(workflowIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"detail": "Invalid workflow ID"}}})
		return
	}

	workflow, err := h.workflowRepo.GetByID(workflowID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"detail": "Workflow not found"}}})
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

	hasPermission, err := h.rbacService.CheckAnsibleResourcePermission(
		c.Request.Context(),
		user.ID,
		rbac.ResourceTypeAnsibleJobTemplate,
		workflow.ID.String(),
		rbac.PermissionAnsibleJobTemplateWrite,
		&workflow.ProjectID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to check permissions"},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "You do not have permission to modify this workflow"},
			},
		})
		return
	}

	var req CreateEdgeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"detail": err.Error()}}})
		return
	}

	sourceNodeID, _ := uuid.Parse(req.Data.Relationships.SourceNode.Data.ID)
	targetNodeID, _ := uuid.Parse(req.Data.Relationships.TargetNode.Data.ID)

	edge := &models.AnsibleWorkflowEdge{
		WorkflowID:   workflowID,
		SourceNodeID: sourceNodeID,
		TargetNodeID: targetNodeID,
		Condition:    models.WorkflowEdgeCondition(req.Data.Attributes.Condition),
	}

	if err := h.workflowRepo.CreateEdge(edge); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"detail": "Failed to create edge"}}})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"data": formatEdgeResponse(edge)})
}

// ListEdges lists edges in a workflow
// GET /api/v2/ansible/workflows/:id/edges
func (h *WorkflowHandler) ListEdges(c *gin.Context) {
	workflowIDStr := c.Param("id")
	workflowID, err := uuid.Parse(workflowIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"detail": "Invalid workflow ID"}}})
		return
	}

	workflow, err := h.workflowRepo.GetByID(workflowID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"detail": "Workflow not found"}}})
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

	hasPermission, err := h.rbacService.CheckAnsibleResourcePermission(
		c.Request.Context(),
		user.ID,
		rbac.ResourceTypeAnsibleJobTemplate,
		workflow.ID.String(),
		rbac.PermissionAnsibleJobTemplateRead,
		&workflow.ProjectID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to check permissions"},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "You do not have permission to view this workflow"},
			},
		})
		return
	}

	edges, err := h.workflowRepo.ListEdgesByWorkflow(workflowID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"detail": "Failed to list edges"}}})
		return
	}

	data := make([]gin.H, len(edges))
	for i, edge := range edges {
		data[i] = formatEdgeResponse(&edge)
	}

	c.JSON(http.StatusOK, gin.H{"data": data})
}

// DeleteEdge deletes an edge
// DELETE /api/v2/ansible/workflow-edges/:id
func (h *WorkflowHandler) DeleteEdge(c *gin.Context) {
	idStr := c.Param("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"detail": "Invalid edge ID"}}})
		return
	}

	edge, err := h.workflowRepo.GetEdgeByID(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"detail": "Edge not found"}}})
		return
	}

	workflow, err := h.workflowRepo.GetByID(edge.WorkflowID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"detail": "Workflow not found"}}})
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

	hasPermission, err := h.rbacService.CheckAnsibleResourcePermission(
		c.Request.Context(),
		user.ID,
		rbac.ResourceTypeAnsibleJobTemplate,
		workflow.ID.String(),
		rbac.PermissionAnsibleJobTemplateWrite,
		&workflow.ProjectID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{
				{"status": "500", "title": "Internal Server Error", "detail": "Failed to check permissions"},
			},
		})
		return
	}
	if !hasPermission {
		c.JSON(http.StatusForbidden, gin.H{
			"errors": []gin.H{
				{"status": "403", "title": "Forbidden", "detail": "You do not have permission to modify this workflow"},
			},
		})
		return
	}

	if err := h.workflowRepo.DeleteEdge(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"detail": "Failed to delete edge"}}})
		return
	}

	c.Status(http.StatusNoContent)
}

// ============================================================================
// Response formatters
// ============================================================================

func formatWorkflowResponse(w *models.AnsibleWorkflow) gin.H {
	response := gin.H{
		"type": "ansible-workflows",
		"id":   w.ID.String(),
		"attributes": gin.H{
			"name":                    w.Name,
			"description":             w.Description,
			"allow-simultaneous":      w.AllowSimultaneous,
			"ask-variables-on-launch": w.AskVariablesOnLaunch,
			"ask-inventory-on-launch": w.AskInventoryOnLaunch,
			"ask-limit-on-launch":     w.AskLimitOnLaunch,
			"extra-vars":              string(w.ExtraVars),
			"limit":                   w.Limit,
			"survey-enabled":          w.SurveyEnabled,
			"created-at":              w.CreatedAt,
			"updated-at":              w.UpdatedAt,
		},
		"relationships": gin.H{
			"organization": gin.H{
				"data": gin.H{"type": "organizations", "id": w.OrganizationID.String()},
			},
		},
	}

	if w.ProjectID != uuid.Nil {
		response["relationships"].(gin.H)["project"] = gin.H{
			"data": gin.H{"type": "projects", "id": w.ProjectID.String()},
		}
	}

	if w.InventoryID != nil {
		response["relationships"].(gin.H)["inventory"] = gin.H{
			"data": gin.H{"type": "ansible-inventories", "id": w.InventoryID.String()},
		}
	}

	return response
}

func formatNodeResponse(n *models.AnsibleWorkflowNode) gin.H {
	response := gin.H{
		"type": "ansible-workflow-nodes",
		"id":   n.ID.String(),
		"attributes": gin.H{
			"node-type":                 string(n.NodeType),
			"identifier":                n.Identifier,
			"position-x":                n.PositionX,
			"position-y":                n.PositionY,
			"extra-vars":                string(n.ExtraVars),
			"limit":                     n.Limit,
			"tags":                      n.Tags,
			"skip-tags":                 n.SkipTags,
			"verbosity":                 n.Verbosity,
			"all-parents-must-converge": n.AllParentsMustConverge,
			"approval-timeout":          n.ApprovalTimeout,
			"approval-message":          n.ApprovalMessage,
			"created-at":                n.CreatedAt,
		},
		"relationships": gin.H{
			"workflow": gin.H{
				"data": gin.H{"type": "ansible-workflows", "id": n.WorkflowID.String()},
			},
		},
	}

	if n.JobTemplateID != nil {
		response["relationships"].(gin.H)["job-template"] = gin.H{
			"data": gin.H{"type": "ansible-job-templates", "id": n.JobTemplateID.String()},
		}
		if n.JobTemplate != nil {
			response["attributes"].(gin.H)["job-template-name"] = n.JobTemplate.Name
		}
	}

	if n.InventoryID != nil {
		response["relationships"].(gin.H)["inventory"] = gin.H{
			"data": gin.H{"type": "ansible-inventories", "id": n.InventoryID.String()},
		}
	}

	if n.CredentialID != nil {
		response["relationships"].(gin.H)["credential"] = gin.H{
			"data": gin.H{"type": "ansible-credentials", "id": n.CredentialID.String()},
		}
	}

	return response
}

func formatEdgeResponse(e *models.AnsibleWorkflowEdge) gin.H {
	return gin.H{
		"type": "ansible-workflow-edges",
		"id":   e.ID.String(),
		"attributes": gin.H{
			"condition":  string(e.Condition),
			"created-at": e.CreatedAt,
		},
		"relationships": gin.H{
			"workflow": gin.H{
				"data": gin.H{"type": "ansible-workflows", "id": e.WorkflowID.String()},
			},
			"source-node": gin.H{
				"data": gin.H{"type": "ansible-workflow-nodes", "id": e.SourceNodeID.String()},
			},
			"target-node": gin.H{
				"data": gin.H{"type": "ansible-workflow-nodes", "id": e.TargetNodeID.String()},
			},
		},
	}
}
