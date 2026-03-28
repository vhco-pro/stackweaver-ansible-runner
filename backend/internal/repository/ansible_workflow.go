// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

// AnsibleWorkflowRepository handles database operations for Ansible workflows
type AnsibleWorkflowRepository struct {
	db *gorm.DB
}

// NewAnsibleWorkflowRepository creates a new AnsibleWorkflowRepository
func NewAnsibleWorkflowRepository(db *gorm.DB) *AnsibleWorkflowRepository {
	return &AnsibleWorkflowRepository{db: db}
}

// Create creates a new workflow
func (r *AnsibleWorkflowRepository) Create(workflow *models.AnsibleWorkflow) error {
	return r.db.Create(workflow).Error
}

// GetByID retrieves a workflow by ID
func (r *AnsibleWorkflowRepository) GetByID(id uuid.UUID) (*models.AnsibleWorkflow, error) {
	var workflow models.AnsibleWorkflow
	err := r.db.Preload("Nodes").Preload("Organization").Preload("Project").Preload("Inventory").First(&workflow, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &workflow, nil
}

// GetByIDWithEdges retrieves a workflow with all nodes and edges
func (r *AnsibleWorkflowRepository) GetByIDWithEdges(id uuid.UUID) (*models.AnsibleWorkflow, []models.AnsibleWorkflowEdge, error) {
	var workflow models.AnsibleWorkflow
	err := r.db.Preload("Nodes").Preload("Nodes.JobTemplate").Preload("Organization").Preload("Project").First(&workflow, "id = ?", id).Error
	if err != nil {
		return nil, nil, err
	}

	var edges []models.AnsibleWorkflowEdge
	err = r.db.Where("workflow_id = ?", id).Find(&edges).Error
	if err != nil {
		return nil, nil, err
	}

	return &workflow, edges, nil
}

// Update updates an existing workflow
func (r *AnsibleWorkflowRepository) Update(workflow *models.AnsibleWorkflow) error {
	return r.db.Save(workflow).Error
}

// Delete deletes a workflow
func (r *AnsibleWorkflowRepository) Delete(id uuid.UUID) error {
	return r.db.Delete(&models.AnsibleWorkflow{}, "id = ?", id).Error
}

// ListByOrganization lists workflows for an organization
func (r *AnsibleWorkflowRepository) ListByOrganization(orgID uuid.UUID, limit, offset int) ([]models.AnsibleWorkflow, int64, error) {
	var workflows []models.AnsibleWorkflow
	var total int64

	query := r.db.Model(&models.AnsibleWorkflow{}).Where("organization_id = ?", orgID)
	query.Count(&total)

	if limit > 0 {
		query = query.Limit(limit)
	}
	if offset > 0 {
		query = query.Offset(offset)
	}

	err := query.Preload("Nodes").Order("created_at DESC").Find(&workflows).Error
	return workflows, total, err
}

// ListByProject lists workflows for a project
func (r *AnsibleWorkflowRepository) ListByProject(projectID uuid.UUID, limit, offset int) ([]models.AnsibleWorkflow, int64, error) {
	var workflows []models.AnsibleWorkflow
	var total int64

	query := r.db.Model(&models.AnsibleWorkflow{}).Where("project_id = ?", projectID)
	query.Count(&total)

	if limit > 0 {
		query = query.Limit(limit)
	}
	if offset > 0 {
		query = query.Offset(offset)
	}

	err := query.Preload("Nodes").Order("created_at DESC").Find(&workflows).Error
	return workflows, total, err
}

// ============================================================================
// Node operations
// ============================================================================

// CreateNode creates a new workflow node
func (r *AnsibleWorkflowRepository) CreateNode(node *models.AnsibleWorkflowNode) error {
	return r.db.Create(node).Error
}

// GetNodeByID retrieves a node by ID
func (r *AnsibleWorkflowRepository) GetNodeByID(id uuid.UUID) (*models.AnsibleWorkflowNode, error) {
	var node models.AnsibleWorkflowNode
	err := r.db.Preload("JobTemplate").First(&node, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &node, nil
}

// UpdateNode updates a workflow node
func (r *AnsibleWorkflowRepository) UpdateNode(node *models.AnsibleWorkflowNode) error {
	return r.db.Save(node).Error
}

// DeleteNode deletes a workflow node and its edges
func (r *AnsibleWorkflowRepository) DeleteNode(id uuid.UUID) error {
	// Delete edges first
	if err := r.db.Where("source_node_id = ? OR target_node_id = ?", id, id).Delete(&models.AnsibleWorkflowEdge{}).Error; err != nil {
		return err
	}
	return r.db.Delete(&models.AnsibleWorkflowNode{}, "id = ?", id).Error
}

// ListNodesByWorkflow lists all nodes in a workflow
func (r *AnsibleWorkflowRepository) ListNodesByWorkflow(workflowID uuid.UUID) ([]models.AnsibleWorkflowNode, error) {
	var nodes []models.AnsibleWorkflowNode
	err := r.db.Preload("JobTemplate").Where("workflow_id = ?", workflowID).Find(&nodes).Error
	return nodes, err
}

// ============================================================================
// Edge operations
// ============================================================================

// GetEdgeByID retrieves a workflow edge by ID
func (r *AnsibleWorkflowRepository) GetEdgeByID(id uuid.UUID) (*models.AnsibleWorkflowEdge, error) {
	var edge models.AnsibleWorkflowEdge
	err := r.db.First(&edge, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &edge, nil
}

// CreateEdge creates a new workflow edge
func (r *AnsibleWorkflowRepository) CreateEdge(edge *models.AnsibleWorkflowEdge) error {
	return r.db.Create(edge).Error
}

// DeleteEdge deletes a workflow edge
func (r *AnsibleWorkflowRepository) DeleteEdge(id uuid.UUID) error {
	return r.db.Delete(&models.AnsibleWorkflowEdge{}, "id = ?", id).Error
}

// ListEdgesByWorkflow lists all edges in a workflow
func (r *AnsibleWorkflowRepository) ListEdgesByWorkflow(workflowID uuid.UUID) ([]models.AnsibleWorkflowEdge, error) {
	var edges []models.AnsibleWorkflowEdge
	err := r.db.Where("workflow_id = ?", workflowID).Find(&edges).Error
	return edges, err
}

// GetEdgesBySourceNode gets all edges from a source node
func (r *AnsibleWorkflowRepository) GetEdgesBySourceNode(nodeID uuid.UUID) ([]models.AnsibleWorkflowEdge, error) {
	var edges []models.AnsibleWorkflowEdge
	err := r.db.Where("source_node_id = ?", nodeID).Find(&edges).Error
	return edges, err
}

// ============================================================================
// Workflow Job operations
// ============================================================================

// CreateWorkflowJob creates a new workflow job
func (r *AnsibleWorkflowRepository) CreateWorkflowJob(job *models.AnsibleWorkflowJob) error {
	return r.db.Create(job).Error
}

// GetWorkflowJobByID retrieves a workflow job by ID
func (r *AnsibleWorkflowRepository) GetWorkflowJobByID(id uuid.UUID) (*models.AnsibleWorkflowJob, error) {
	var job models.AnsibleWorkflowJob
	err := r.db.Preload("Workflow").Preload("NodeJobs").Preload("NodeJobs.Node").Preload("NodeJobs.AnsibleJob").First(&job, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// UpdateWorkflowJob updates a workflow job
func (r *AnsibleWorkflowRepository) UpdateWorkflowJob(job *models.AnsibleWorkflowJob) error {
	return r.db.Save(job).Error
}

// ListWorkflowJobsByWorkflow lists workflow jobs for a workflow
func (r *AnsibleWorkflowRepository) ListWorkflowJobsByWorkflow(workflowID uuid.UUID, limit, offset int) ([]models.AnsibleWorkflowJob, int64, error) {
	var jobs []models.AnsibleWorkflowJob
	var total int64

	query := r.db.Model(&models.AnsibleWorkflowJob{}).Where("workflow_id = ?", workflowID)
	query.Count(&total)

	if limit > 0 {
		query = query.Limit(limit)
	}
	if offset > 0 {
		query = query.Offset(offset)
	}

	err := query.Preload("NodeJobs").Order("created_at DESC").Find(&jobs).Error
	return jobs, total, err
}

// ListWorkflowJobsByOrganization lists workflow jobs for an organization
func (r *AnsibleWorkflowRepository) ListWorkflowJobsByOrganization(orgID uuid.UUID, limit, offset int) ([]models.AnsibleWorkflowJob, int64, error) {
	var jobs []models.AnsibleWorkflowJob
	var total int64

	query := r.db.Model(&models.AnsibleWorkflowJob{}).
		Joins("JOIN ansible_workflows ON ansible_workflow_jobs.workflow_id = ansible_workflows.id").
		Where("ansible_workflows.organization_id = ?", orgID)
	query.Count(&total)

	if limit > 0 {
		query = query.Limit(limit)
	}
	if offset > 0 {
		query = query.Offset(offset)
	}

	err := query.Preload("Workflow").Preload("NodeJobs").Order("ansible_workflow_jobs.created_at DESC").Find(&jobs).Error
	return jobs, total, err
}

// ============================================================================
// Node Job operations
// ============================================================================

// CreateNodeJob creates a new node job
func (r *AnsibleWorkflowRepository) CreateNodeJob(nodeJob *models.AnsibleWorkflowNodeJob) error {
	return r.db.Create(nodeJob).Error
}

// GetNodeJobByID retrieves a node job by ID
func (r *AnsibleWorkflowRepository) GetNodeJobByID(id uuid.UUID) (*models.AnsibleWorkflowNodeJob, error) {
	var nodeJob models.AnsibleWorkflowNodeJob
	err := r.db.Preload("Node").Preload("AnsibleJob").First(&nodeJob, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &nodeJob, nil
}

// UpdateNodeJob updates a node job
func (r *AnsibleWorkflowRepository) UpdateNodeJob(nodeJob *models.AnsibleWorkflowNodeJob) error {
	return r.db.Save(nodeJob).Error
}

// GetPendingNodeJobs gets pending node jobs for a workflow job
func (r *AnsibleWorkflowRepository) GetPendingNodeJobs(workflowJobID uuid.UUID) ([]models.AnsibleWorkflowNodeJob, error) {
	var nodeJobs []models.AnsibleWorkflowNodeJob
	err := r.db.Where("workflow_job_id = ? AND status IN ?", workflowJobID, []string{"pending", "waiting"}).
		Preload("Node").Find(&nodeJobs).Error
	return nodeJobs, err
}

// GetRunningNodeJobs gets running node jobs for a workflow job
func (r *AnsibleWorkflowRepository) GetRunningNodeJobs(workflowJobID uuid.UUID) ([]models.AnsibleWorkflowNodeJob, error) {
	var nodeJobs []models.AnsibleWorkflowNodeJob
	err := r.db.Where("workflow_job_id = ? AND status = ?", workflowJobID, "running").
		Preload("Node").Preload("AnsibleJob").Find(&nodeJobs).Error
	return nodeJobs, err
}
