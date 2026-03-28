// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// TeamOrganizationAccess represents organization-level permissions for a team
// TFE-compatible structure with 16 permission fields
type TeamOrganizationAccess struct {
	ID     uuid.UUID `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	TeamID uuid.UUID `gorm:"type:uuid;not null;uniqueIndex" json:"team_id"`

	// Organization access permissions (all default to false)
	ManagePolicies           bool `gorm:"default:false" json:"manage_policies"`
	ManagePolicyOverrides    bool `gorm:"default:false" json:"manage_policy_overrides"`
	ManageWorkspaces         bool `gorm:"default:false" json:"manage_workspaces"`
	ManageVCSSettings        bool `gorm:"default:false" json:"manage_vcs_settings"`
	ManageProviders          bool `gorm:"default:false" json:"manage_providers"`
	ManageModules            bool `gorm:"default:false" json:"manage_modules"`
	ManageRunTasks           bool `gorm:"default:false" json:"manage_run_tasks"`
	ManageProjects           bool `gorm:"default:false" json:"manage_projects"`
	ReadWorkspaces           bool `gorm:"default:false" json:"read_workspaces"`
	ReadProjects             bool `gorm:"default:false" json:"read_projects"`
	ManageMembership         bool `gorm:"default:false" json:"manage_membership"`
	ManageTeams              bool `gorm:"default:false" json:"manage_teams"`
	ManageOrganizationAccess bool `gorm:"default:false" json:"manage_organization_access"`
	AccessSecretTeams        bool `gorm:"default:false" json:"access_secret_teams"`
	ManageAgentPools         bool `gorm:"default:false" json:"manage_agent_pools"`
	ManageAnsible            bool `gorm:"default:false" json:"manage_ansible"`
	ReadAnsible              bool `gorm:"default:false" json:"read_ansible"`

	// Fine-grained Ansible permissions (per-resource-type)
	// When ManageAnsible is true, all manage+read sub-permissions are implied.
	// When ReadAnsible is true, all read sub-permissions are implied.
	// These fields allow restricting a team to specific Ansible resource types.
	ManageAnsiblePlaybooks    bool `gorm:"default:false" json:"manage_ansible_playbooks"`
	ReadAnsiblePlaybooks      bool `gorm:"default:false" json:"read_ansible_playbooks"`
	ManageAnsibleInventories  bool `gorm:"default:false" json:"manage_ansible_inventories"`
	ReadAnsibleInventories    bool `gorm:"default:false" json:"read_ansible_inventories"`
	ManageAnsibleCredentials  bool `gorm:"default:false" json:"manage_ansible_credentials"`
	ReadAnsibleCredentials    bool `gorm:"default:false" json:"read_ansible_credentials"`
	ManageAnsibleJobTemplates bool `gorm:"default:false" json:"manage_ansible_job_templates"`
	ReadAnsibleJobTemplates   bool `gorm:"default:false" json:"read_ansible_job_templates"`
	ManageAnsibleJobs         bool `gorm:"default:false" json:"manage_ansible_jobs"`
	ReadAnsibleJobs           bool `gorm:"default:false" json:"read_ansible_jobs"`
	ManageAnsibleSchedules    bool `gorm:"default:false" json:"manage_ansible_schedules"`
	ReadAnsibleSchedules      bool `gorm:"default:false" json:"read_ansible_schedules"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Relationships
	Team Team `gorm:"foreignKey:TeamID" json:"team,omitempty"`
}

func (t *TeamOrganizationAccess) BeforeCreate(tx *gorm.DB) error {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	return nil
}
