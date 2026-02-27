// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// CredentialType defines the type of credential
type CredentialType string

const (
	CredentialTypeSSH          CredentialType = "ssh"         // SSH key for host access
	CredentialTypeSCM          CredentialType = "scm"         // SCM credentials for repo access
	CredentialTypeVault        CredentialType = "vault"       // Ansible Vault password
	CredentialTypeMachineSSH   CredentialType = "machine-ssh" // Machine SSH (username + password)
	CredentialTypeAWSAccessKey CredentialType = "aws"         // AWS access key for dynamic inventory
	CredentialTypeAzure        CredentialType = "azure"       // Azure credentials
	CredentialTypeGCP          CredentialType = "gcp"         // GCP credentials
	CredentialTypeVMware       CredentialType = "vmware"      // VMware vCenter credentials
)

// AnsibleCredential represents a credential for Ansible operations
// All sensitive fields are encrypted using AES-256-GCM before storage
type AnsibleCredential struct {
	ID             uuid.UUID      `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	OrganizationID uuid.UUID      `gorm:"type:uuid;not null;index" json:"organization_id"`
	ProjectID      *uuid.UUID     `gorm:"type:uuid;index" json:"project_id,omitempty"` // Optional: null = org-scoped, set = project-scoped
	Name           string         `gorm:"type:varchar(255);not null;uniqueIndex:idx_org_credential" json:"name"`
	Description    string         `gorm:"type:text" json:"description"`
	Type           CredentialType `gorm:"type:varchar(50);not null" json:"type"`

	// Common fields (not all are used for every type)
	Username string `gorm:"type:varchar(255)" json:"username,omitempty"`

	// Encrypted fields - stored as base64 encoded encrypted data
	// These are NEVER exposed in API responses
	SSHPrivateKey  string `gorm:"type:text" json:"-"` // Encrypted SSH private key
	SSHPassphrase  string `gorm:"type:text" json:"-"` // Encrypted SSH key passphrase
	Password       string `gorm:"type:text" json:"-"` // Encrypted password (for various uses)
	VaultPassword  string `gorm:"type:text" json:"-"` // Encrypted Ansible Vault password
	BecomePassword string `gorm:"type:text" json:"-"` // Encrypted sudo password

	// Cloud-specific fields (encrypted)
	AWSAccessKeyID     string `gorm:"type:text" json:"-"` // Encrypted AWS access key ID
	AWSSecretAccessKey string `gorm:"type:text" json:"-"` // Encrypted AWS secret access key
	AzureTenantID      string `gorm:"type:varchar(100)" json:"azure_tenant_id,omitempty"`
	AzureClientID      string `gorm:"type:varchar(100)" json:"azure_client_id,omitempty"`
	AzureClientSecret  string `gorm:"type:text" json:"-"` // Encrypted
	GCPServiceAccount  string `gorm:"type:text" json:"-"` // Encrypted GCP service account JSON

	// SSH-specific options
	SSHPort       int    `gorm:"default:22" json:"ssh_port"`
	SSHBecomeUser string `gorm:"type:varchar(100);default:'root'" json:"ssh_become_user"`

	// Indicators for UI (whether fields have values - never expose actual values)
	HasSSHPrivateKey  bool `gorm:"-" json:"has_ssh_private_key,omitempty"`
	HasPassword       bool `gorm:"-" json:"has_password,omitempty"`
	HasVaultPassword  bool `gorm:"-" json:"has_vault_password,omitempty"`
	HasBecomePassword bool `gorm:"-" json:"has_become_password,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Relationships
	Organization Organization `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"`
	Project      *Project     `gorm:"foreignKey:ProjectID" json:"project,omitempty"`
}

func (c *AnsibleCredential) BeforeCreate(tx *gorm.DB) error {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	return nil
}

// AfterFind populates the Has* fields for API responses
func (c *AnsibleCredential) AfterFind(tx *gorm.DB) error {
	c.HasSSHPrivateKey = c.SSHPrivateKey != ""
	c.HasPassword = c.Password != ""
	c.HasVaultPassword = c.VaultPassword != ""
	c.HasBecomePassword = c.BecomePassword != ""
	return nil
}
