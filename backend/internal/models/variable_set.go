// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/pkg/id"
	"gorm.io/gorm"
)

// VariableSet represents a group of variables that can be applied to multiple workspaces
// Similar to Terraform Enterprise's variable sets
type VariableSet struct {
	ID             string    `gorm:"type:varchar(25);primary_key" json:"id"` // Format: varset-{16-char-id} = 23 chars, but varchar(25) for safety
	OrganizationID uuid.UUID `gorm:"type:uuid;not null;index" json:"organization_id"`
	Name           string    `gorm:"type:varchar(255);not null" json:"name"`
	Description    string    `gorm:"type:text" json:"description"`

	// Scope: "organization" (applies to all workspaces in org) or "workspace" (applies to specific workspaces)
	Scope string `gorm:"type:varchar(50);default:'workspace'" json:"scope"` // "organization" or "workspace"

	// Priority: when true, variables in this set override other variables with the same key
	// TFE-compatible: priority variable sets take precedence over workspace variables and CLI inputs
	Priority bool `gorm:"default:false" json:"priority"`

	// Parent ownership: can be owned by organization (default) or project
	// TFE-compatible: parent relationship determines ownership
	// If ProjectID is set, the variable set is project-owned; otherwise organization-owned
	ProjectID *uuid.UUID `gorm:"type:uuid;index" json:"project_id,omitempty"` // If set, variable set is project-owned

	// For workspace scope, we use a many-to-many relationship via VariableSetWorkspace
	Workspaces []Workspace `gorm:"many2many:variable_set_workspaces;" json:"workspaces,omitempty"`

	// For organization scope, can optionally assign to specific projects
	Projects []Project `gorm:"many2many:variable_set_projects;" json:"projects,omitempty"`

	// For Ansible: can assign to job templates
	JobTemplates []AnsibleJobTemplate `gorm:"many2many:variable_set_job_templates;" json:"job_templates,omitempty"`

	// Relationship to parent project (if project-owned)
	Project Project `gorm:"foreignKey:ProjectID" json:"project,omitempty"`

	// Variables in this set
	Variables []VariableSetVariable `gorm:"foreignKey:VariableSetID" json:"variables,omitempty"`

	CreatedBy uuid.UUID `gorm:"type:uuid;index" json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Relationships
	Organization Organization `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"`
}

func (vs *VariableSet) BeforeCreate(tx *gorm.DB) error {
	if vs.ID == "" {
		generatedID, err := id.GenerateVariableSetID()
		if err != nil {
			return err
		}
		vs.ID = generatedID
	}
	return nil
}

// VariableSetVariable represents a variable within a variable set (TFE-compatible)
type VariableSetVariable struct {
	ID            string    `gorm:"type:varchar(20);primary_key" json:"id"`                                                  // Format: var-{16-char-id}
	VariableSetID string    `gorm:"type:varchar(25);not null;index;uniqueIndex:idx_variable_set_key" json:"variable_set_id"` // Format: varset-{16-char-id}
	Key           string    `gorm:"type:varchar(255);not null;uniqueIndex:idx_variable_set_key" json:"key"`
	Value         string    `gorm:"type:text;not null" json:"value"`
	Description   string    `gorm:"type:text" json:"description"`                         // TFE-compatible: description field
	Category      string    `gorm:"type:varchar(50);default:'terraform'" json:"category"` // TFE-compatible: "terraform" or "env"
	HCL           bool      `gorm:"default:false" json:"hcl"`                             // TFE-compatible: whether value is HCL code
	Encrypted     bool      `gorm:"default:false" json:"encrypted"`                       // Legacy field
	Sensitive     bool      `gorm:"default:false" json:"sensitive"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`

	// Relationships
	VariableSet VariableSet `gorm:"foreignKey:VariableSetID" json:"variable_set,omitempty"`
}

func (vsv *VariableSetVariable) BeforeCreate(tx *gorm.DB) error {
	if vsv.ID == "" {
		generatedID, err := id.GenerateVariableID()
		if err != nil {
			return err
		}
		vsv.ID = generatedID
	}
	return nil
}

// VariableSetWorkspace is the join table for many-to-many relationship
// between VariableSet and Workspace (for workspace-scoped variable sets)
type VariableSetWorkspace struct {
	VariableSetID string    `gorm:"type:varchar(25);primary_key" json:"variable_set_id"` // Format: varset-{16-char-id}
	WorkspaceID   string    `gorm:"type:varchar(20);primary_key" json:"workspace_id"`    // Format: ws-{16-char-id}
	CreatedAt     time.Time `json:"created_at"`
}

// VariableSetProject is the join table for many-to-many relationship
// between VariableSet and Project (for organization-scoped variable sets assigned to specific projects)
type VariableSetProject struct {
	VariableSetID string    `gorm:"type:varchar(25);primary_key" json:"variable_set_id"` // Format: varset-{16-char-id}
	ProjectID     uuid.UUID `gorm:"type:uuid;primary_key" json:"project_id"`             // Projects still use UUID
	CreatedAt     time.Time `json:"created_at"`
}

// VariableSetJobTemplate is the join table for many-to-many relationship
// between VariableSet and AnsibleJobTemplate (for Ansible variable sets)
type VariableSetJobTemplate struct {
	VariableSetID string    `gorm:"type:varchar(25);primary_key" json:"variable_set_id"` // Format: varset-{16-char-id}
	JobTemplateID uuid.UUID `gorm:"type:uuid;primary_key" json:"job_template_id"`        // Job templates use UUID
	CreatedAt     time.Time `json:"created_at"`
}
