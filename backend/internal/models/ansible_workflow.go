// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// AnsibleWorkflow represents a workflow template that orchestrates multiple job templates
// Similar to AWX Workflow Job Templates
type AnsibleWorkflow struct {
	ID             uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	OrganizationID uuid.UUID `gorm:"type:uuid;not null;index"`
	ProjectID      uuid.UUID `gorm:"type:uuid;index"`
	Name           string    `gorm:"not null"`
	Description    string

	// Workflow settings
	AllowSimultaneous    bool `gorm:"default:false"` // Allow multiple runs at once
	AskVariablesOnLaunch bool `gorm:"default:false"`
	AskInventoryOnLaunch bool `gorm:"default:false"`
	AskLimitOnLaunch     bool `gorm:"default:false"`

	// Default values (can be overridden at launch)
	InventoryID *uuid.UUID     `gorm:"type:uuid"`
	ExtraVars   datatypes.JSON `gorm:"type:jsonb;default:'{}'"`
	Limit       string

	// Survey (prompts for extra vars)
	SurveyEnabled bool           `gorm:"default:false"`
	SurveySpec    datatypes.JSON `gorm:"type:jsonb"` // Survey questions definition

	// Metadata
	CreatedBy *uuid.UUID `gorm:"type:uuid"`
	CreatedAt time.Time  `gorm:"autoCreateTime"`
	UpdatedAt time.Time  `gorm:"autoUpdateTime"`

	// Relationships (preloaded)
	Organization Organization          `gorm:"foreignKey:OrganizationID"`
	Project      Project               `gorm:"foreignKey:ProjectID"`
	Inventory    *AnsibleInventory     `gorm:"foreignKey:InventoryID"`
	Nodes        []AnsibleWorkflowNode `gorm:"foreignKey:WorkflowID;constraint:OnDelete:CASCADE"`
}

// WorkflowNodeType defines the type of node in a workflow
type WorkflowNodeType string

const (
	WorkflowNodeTypeJobTemplate   WorkflowNodeType = "job_template"
	WorkflowNodeTypeWorkflow      WorkflowNodeType = "workflow" // Nested workflow
	WorkflowNodeTypeInventorySync WorkflowNodeType = "inventory_sync"
	WorkflowNodeTypeApproval      WorkflowNodeType = "approval" // Manual approval gate
)

// WorkflowEdgeCondition defines when an edge should be followed
type WorkflowEdgeCondition string

const (
	WorkflowEdgeOnSuccess WorkflowEdgeCondition = "on_success"
	WorkflowEdgeOnFailure WorkflowEdgeCondition = "on_failure"
	WorkflowEdgeAlways    WorkflowEdgeCondition = "always"
)

// AnsibleWorkflowNode represents a node in a workflow (a job template to run)
type AnsibleWorkflowNode struct {
	ID         uuid.UUID        `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	WorkflowID uuid.UUID        `gorm:"type:uuid;not null;index"`
	NodeType   WorkflowNodeType `gorm:"type:varchar(50);not null;default:'job_template'"`

	// Node position (for visual editor)
	PositionX float64 `gorm:"default:0"`
	PositionY float64 `gorm:"default:0"`

	// Reference to what this node runs
	JobTemplateID     *uuid.UUID `gorm:"type:uuid"` // For job_template nodes
	NestedWorkflowID  *uuid.UUID `gorm:"type:uuid"` // For workflow nodes
	InventorySourceID *uuid.UUID `gorm:"type:uuid"` // For inventory_sync nodes

	// Node-specific overrides
	InventoryID  *uuid.UUID     `gorm:"type:uuid"`
	CredentialID *uuid.UUID     `gorm:"type:uuid"`
	ExtraVars    datatypes.JSON `gorm:"type:jsonb;default:'{}'"`
	Limit        string
	Tags         string
	SkipTags     string
	Verbosity    int

	// Approval settings (for approval nodes)
	ApprovalTimeout int `gorm:"default:0"` // Seconds, 0 = no timeout
	ApprovalMessage string

	// Convergence: wait for all incoming edges before running
	AllParentsMustConverge bool `gorm:"default:false"`

	// Identifier for the node in the workflow (unique within workflow)
	Identifier string `gorm:"not null"`

	CreatedAt time.Time `gorm:"autoCreateTime"`
	UpdatedAt time.Time `gorm:"autoUpdateTime"`

	// Relationships
	Workflow       AnsibleWorkflow       `gorm:"foreignKey:WorkflowID"`
	JobTemplate    *AnsibleJobTemplate   `gorm:"foreignKey:JobTemplateID"`
	NestedWorkflow *AnsibleWorkflow      `gorm:"foreignKey:NestedWorkflowID"`
	Inventory      *AnsibleInventory     `gorm:"foreignKey:InventoryID"`
	Credential     *AnsibleCredential    `gorm:"foreignKey:CredentialID"`
	SuccessNodes   []AnsibleWorkflowNode `gorm:"-"` // Loaded separately
	FailureNodes   []AnsibleWorkflowNode `gorm:"-"` // Loaded separately
	AlwaysNodes    []AnsibleWorkflowNode `gorm:"-"` // Loaded separately
}

// AnsibleWorkflowEdge represents a connection between two nodes
type AnsibleWorkflowEdge struct {
	ID           uuid.UUID             `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	WorkflowID   uuid.UUID             `gorm:"type:uuid;not null;index"`
	SourceNodeID uuid.UUID             `gorm:"type:uuid;not null;index"`
	TargetNodeID uuid.UUID             `gorm:"type:uuid;not null;index"`
	Condition    WorkflowEdgeCondition `gorm:"type:varchar(20);not null;default:'on_success'"`

	CreatedAt time.Time `gorm:"autoCreateTime"`

	// Relationships
	Workflow   AnsibleWorkflow     `gorm:"foreignKey:WorkflowID"`
	SourceNode AnsibleWorkflowNode `gorm:"foreignKey:SourceNodeID"`
	TargetNode AnsibleWorkflowNode `gorm:"foreignKey:TargetNodeID"`
}

// AnsibleWorkflowJob represents a running instance of a workflow
type AnsibleWorkflowJob struct {
	ID         uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	WorkflowID uuid.UUID `gorm:"type:uuid;not null;index"`
	Name       string    `gorm:"not null"`
	Status     string    `gorm:"type:varchar(20);not null;default:'pending'"` // pending, running, successful, failed, canceled

	// Runtime values
	InventoryID *uuid.UUID     `gorm:"type:uuid"`
	ExtraVars   datatypes.JSON `gorm:"type:jsonb;default:'{}'"`
	Limit       string

	// Timing
	StartedAt  *time.Time
	FinishedAt *time.Time

	// Results
	LaunchedBy *uuid.UUID `gorm:"type:uuid"`

	CreatedAt time.Time `gorm:"autoCreateTime"`
	UpdatedAt time.Time `gorm:"autoUpdateTime"`

	// Relationships
	Workflow  AnsibleWorkflow          `gorm:"foreignKey:WorkflowID"`
	Inventory *AnsibleInventory        `gorm:"foreignKey:InventoryID"`
	NodeJobs  []AnsibleWorkflowNodeJob `gorm:"foreignKey:WorkflowJobID;constraint:OnDelete:CASCADE"`
}

// WorkflowNodeJobStatus represents the status of a node in a workflow job
type WorkflowNodeJobStatus string

const (
	WorkflowNodeJobPending    WorkflowNodeJobStatus = "pending"
	WorkflowNodeJobWaiting    WorkflowNodeJobStatus = "waiting" // Waiting for parent nodes
	WorkflowNodeJobRunning    WorkflowNodeJobStatus = "running"
	WorkflowNodeJobSuccessful WorkflowNodeJobStatus = "successful"
	WorkflowNodeJobFailed     WorkflowNodeJobStatus = "failed"
	WorkflowNodeJobSkipped    WorkflowNodeJobStatus = "skipped" // Condition not met
	WorkflowNodeJobCanceled   WorkflowNodeJobStatus = "canceled"
)

// AnsibleWorkflowNodeJob tracks the execution of a single node in a workflow job
type AnsibleWorkflowNodeJob struct {
	ID            uuid.UUID             `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	WorkflowJobID uuid.UUID             `gorm:"type:uuid;not null;index"`
	NodeID        uuid.UUID             `gorm:"type:uuid;not null;index"`
	Status        WorkflowNodeJobStatus `gorm:"type:varchar(20);not null;default:'pending'"`

	// Reference to the actual job that was launched (for job_template nodes)
	AnsibleJobID *uuid.UUID `gorm:"type:uuid"`

	// Timing
	StartedAt  *time.Time
	FinishedAt *time.Time

	// For approval nodes
	ApprovedBy *uuid.UUID `gorm:"type:uuid"`
	ApprovedAt *time.Time
	Denied     bool
	DeniedBy   *uuid.UUID `gorm:"type:uuid"`
	DeniedAt   *time.Time

	CreatedAt time.Time `gorm:"autoCreateTime"`
	UpdatedAt time.Time `gorm:"autoUpdateTime"`

	// Relationships
	WorkflowJob AnsibleWorkflowJob  `gorm:"foreignKey:WorkflowJobID"`
	Node        AnsibleWorkflowNode `gorm:"foreignKey:NodeID"`
	AnsibleJob  *AnsibleJob         `gorm:"foreignKey:AnsibleJobID"`
}

// TableName sets the table name for AnsibleWorkflow
func (AnsibleWorkflow) TableName() string {
	return "ansible_workflows"
}

// TableName sets the table name for AnsibleWorkflowNode
func (AnsibleWorkflowNode) TableName() string {
	return "ansible_workflow_nodes"
}

// TableName sets the table name for AnsibleWorkflowEdge
func (AnsibleWorkflowEdge) TableName() string {
	return "ansible_workflow_edges"
}

// TableName sets the table name for AnsibleWorkflowJob
func (AnsibleWorkflowJob) TableName() string {
	return "ansible_workflow_jobs"
}

// TableName sets the table name for AnsibleWorkflowNodeJob
func (AnsibleWorkflowNodeJob) TableName() string {
	return "ansible_workflow_node_jobs"
}
