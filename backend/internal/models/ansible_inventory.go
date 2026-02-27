// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"database/sql/driver"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// InventoryType defines the type of inventory
type InventoryType string

const (
	InventoryTypeStatic  InventoryType = "static"  // Manually defined hosts/groups
	InventoryTypeDynamic InventoryType = "dynamic" // Plugin-based dynamic inventory
	InventoryTypeVCS     InventoryType = "vcs"     // Inventory file from VCS repository
)

// InventoryVariables is a map for storing inventory variables as JSONB
type InventoryVariables map[string]interface{}

func (v InventoryVariables) Value() (driver.Value, error) {
	if v == nil {
		return json.Marshal(map[string]interface{}{})
	}
	return json.Marshal(v)
}

func (v *InventoryVariables) Scan(value interface{}) error {
	if value == nil {
		*v = make(InventoryVariables)
		return nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		return nil
	}
	return json.Unmarshal(bytes, v)
}

// AnsibleInventory represents an Ansible inventory
type AnsibleInventory struct {
	ID                      uuid.UUID          `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	OrganizationID          uuid.UUID          `gorm:"type:uuid;not null;index" json:"organization_id"`
	ProjectID               *uuid.UUID         `gorm:"type:uuid;index" json:"project_id,omitempty"` // Optional: null = org-scoped, set = project-scoped
	Name                    string             `gorm:"type:varchar(255);not null;uniqueIndex:idx_org_inventory" json:"name"`
	Description             string             `gorm:"type:text" json:"description"`
	Type                    InventoryType      `gorm:"type:varchar(50);not null;default:'static'" json:"type"`
	Source                  string             `gorm:"type:text" json:"source,omitempty"`                  // VCS URL or plugin config (deprecated for VCS inventories)
	Variables               InventoryVariables `gorm:"type:jsonb;default:'{}'" json:"variables"`           // Global inventory variables
	LastSyncAt              *time.Time         `json:"last_sync_at,omitempty"`                             // For dynamic/VCS inventories
	LastSyncStatus          string             `gorm:"type:varchar(50)" json:"last_sync_status,omitempty"` // syncing, successful, failed
	LastSyncError           string             `gorm:"type:text" json:"last_sync_error,omitempty"`
	LastSyncHostsDiscovered int                `gorm:"default:0" json:"last_sync_hosts_discovered"` // Number of hosts found during last sync
	LastSyncLog             string             `gorm:"type:text" json:"last_sync_log,omitempty"`    // Stderr/warnings from ansible-inventory
	CreatedAt               time.Time          `json:"created_at"`
	UpdatedAt               time.Time          `json:"updated_at"`

	// VCS Connection (GitHub App integration - for VCS inventories)
	VCSConnectionID *uuid.UUID     `gorm:"type:uuid;index" json:"vcs_connection_id,omitempty"`
	VCSRepository   string         `gorm:"type:varchar(500)" json:"vcs_repository,omitempty"` // Repository full name (e.g., "owner/repo")
	VCSBranch       string         `gorm:"type:varchar(255);default:'main'" json:"vcs_branch"`
	InventoryPath   string         `gorm:"type:varchar(500)" json:"inventory_path,omitempty"` // Path to inventory file within repo
	VCSConnection   *VCSConnection `gorm:"foreignKey:VCSConnectionID" json:"vcs_connection,omitempty"`

	// Relationships
	Organization Organization            `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"`
	Project      *Project                `gorm:"foreignKey:ProjectID" json:"project,omitempty"`
	Hosts        []AnsibleInventoryHost  `gorm:"foreignKey:InventoryID" json:"hosts,omitempty"`
	Groups       []AnsibleInventoryGroup `gorm:"foreignKey:InventoryID" json:"groups,omitempty"`
}

func (i *AnsibleInventory) BeforeCreate(tx *gorm.DB) error {
	if i.ID == uuid.Nil {
		i.ID = uuid.New()
	}
	if i.Variables == nil {
		i.Variables = make(InventoryVariables)
	}
	return nil
}

// AnsibleInventoryHost represents a host in an Ansible inventory
type AnsibleInventoryHost struct {
	ID          uuid.UUID          `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	InventoryID uuid.UUID          `gorm:"type:uuid;not null;index" json:"inventory_id"`
	Name        string             `gorm:"type:varchar(255);not null;uniqueIndex:idx_inventory_host" json:"name"`
	Description string             `gorm:"type:text" json:"description"`
	Hostname    string             `gorm:"type:varchar(255)" json:"hostname,omitempty"` // Actual hostname/IP if different from name
	Port        int                `gorm:"default:22" json:"port"`
	Variables   InventoryVariables `gorm:"type:jsonb;default:'{}'" json:"variables"` // Host-specific variables
	Enabled     bool               `gorm:"default:true" json:"enabled"`
	CreatedAt   time.Time          `json:"created_at"`
	UpdatedAt   time.Time          `json:"updated_at"`

	// Relationships
	Inventory AnsibleInventory        `gorm:"foreignKey:InventoryID" json:"inventory,omitempty"`
	Groups    []AnsibleInventoryGroup `gorm:"many2many:ansible_inventory_host_groups;" json:"groups,omitempty"`
}

func (h *AnsibleInventoryHost) BeforeCreate(tx *gorm.DB) error {
	if h.ID == uuid.Nil {
		h.ID = uuid.New()
	}
	if h.Variables == nil {
		h.Variables = make(InventoryVariables)
	}
	return nil
}

// AnsibleInventoryGroup represents a group in an Ansible inventory
type AnsibleInventoryGroup struct {
	ID          uuid.UUID          `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	InventoryID uuid.UUID          `gorm:"type:uuid;not null;index" json:"inventory_id"`
	Name        string             `gorm:"type:varchar(255);not null;uniqueIndex:idx_inventory_group" json:"name"`
	Description string             `gorm:"type:text" json:"description"`
	Variables   InventoryVariables `gorm:"type:jsonb;default:'{}'" json:"variables"` // Group-specific variables
	CreatedAt   time.Time          `json:"created_at"`
	UpdatedAt   time.Time          `json:"updated_at"`

	// Relationships
	Inventory AnsibleInventory        `gorm:"foreignKey:InventoryID" json:"inventory,omitempty"`
	Hosts     []AnsibleInventoryHost  `gorm:"many2many:ansible_inventory_host_groups;" json:"hosts,omitempty"`
	Parent    *AnsibleInventoryGroup  `gorm:"foreignKey:ParentID" json:"parent,omitempty"`
	ParentID  *uuid.UUID              `gorm:"type:uuid;index" json:"parent_id,omitempty"` // For nested groups
	Children  []AnsibleInventoryGroup `gorm:"foreignKey:ParentID" json:"children,omitempty"`
}

func (g *AnsibleInventoryGroup) BeforeCreate(tx *gorm.DB) error {
	if g.ID == uuid.Nil {
		g.ID = uuid.New()
	}
	if g.Variables == nil {
		g.Variables = make(InventoryVariables)
	}
	return nil
}
