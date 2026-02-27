// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import (
	"database/sql/driver"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// InventorySourceType defines the type of dynamic inventory source
type InventorySourceType string

const (
	InventorySourceTypeAWS    InventorySourceType = "aws"    // AWS EC2 instances
	InventorySourceTypeAzure  InventorySourceType = "azure"  // Azure VMs
	InventorySourceTypeGCP    InventorySourceType = "gcp"    // GCP Compute instances
	InventorySourceTypeVMware InventorySourceType = "vmware" // VMware vCenter
	InventorySourceTypeCustom InventorySourceType = "custom" // Custom script/plugin
)

// InventorySourceStatus defines the sync status
type InventorySourceStatus string

const (
	InventorySourceStatusNeverSynced InventorySourceStatus = "never_synced"
	InventorySourceStatusSyncing     InventorySourceStatus = "syncing"
	InventorySourceStatusSuccessful  InventorySourceStatus = "successful"
	InventorySourceStatusFailed      InventorySourceStatus = "failed"
)

// InventorySourceConfig stores provider-specific configuration
type InventorySourceConfig map[string]interface{}

func (c InventorySourceConfig) Value() (driver.Value, error) {
	if c == nil {
		return json.Marshal(map[string]interface{}{})
	}
	return json.Marshal(c)
}

func (c *InventorySourceConfig) Scan(value interface{}) error {
	if value == nil {
		*c = make(InventorySourceConfig)
		return nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		return nil
	}
	return json.Unmarshal(bytes, c)
}

// AnsibleInventorySource represents a dynamic inventory source
// These sources automatically discover hosts from cloud providers or other systems
type AnsibleInventorySource struct {
	ID           uuid.UUID           `gorm:"type:uuid;primary_key;default:uuid_generate_v4()" json:"id"`
	InventoryID  uuid.UUID           `gorm:"type:uuid;not null;index" json:"inventory_id"`
	Name         string              `gorm:"type:varchar(255);not null" json:"name"`
	Description  string              `gorm:"type:text" json:"description"`
	Type         InventorySourceType `gorm:"type:varchar(50);not null" json:"type"`
	CredentialID *uuid.UUID          `gorm:"type:uuid;index" json:"credential_id,omitempty"` // Cloud credential

	// Provider-specific configuration stored as JSONB
	// AWS: region, filters (tag:Name, instance-state-name, etc.)
	// Azure: resource_groups, subscription_id
	// GCP: project_id, zones
	Config InventorySourceConfig `gorm:"type:jsonb;default:'{}'" json:"config"`

	// Sync settings
	UpdateOnLaunch     bool `gorm:"default:true" json:"update_on_launch"`  // Sync before each job run
	UpdateCacheTimeout int  `gorm:"default:0" json:"update_cache_timeout"` // Minutes to cache results (0 = always fetch)

	// Group configuration
	// Defines how discovered hosts are grouped (by region, tag, etc.)
	GroupByInstanceID       bool   `gorm:"default:false" json:"group_by_instance_id"`
	GroupByRegion           bool   `gorm:"default:true" json:"group_by_region"`
	GroupByAvailabilityZone bool   `gorm:"default:false" json:"group_by_availability_zone"`
	GroupByTag              string `gorm:"type:varchar(255)" json:"group_by_tag,omitempty"` // Tag key to group by (e.g., "Environment")

	// Host variable settings
	HostnameVar     string `gorm:"type:varchar(100);default:'public_ip'" json:"hostname_var"` // Which var to use as hostname (public_ip, private_ip, name)
	InstanceFilters string `gorm:"type:text" json:"instance_filters,omitempty"`               // JSON array of filters

	// Schedule settings
	SyncSchedule string `gorm:"type:varchar(100)" json:"sync_schedule,omitempty"` // Cron expression for scheduled sync (e.g., "0 */6 * * *")

	// Sync status
	Status        InventorySourceStatus `gorm:"type:varchar(50);default:'never_synced'" json:"status"`
	LastSyncAt    *time.Time            `json:"last_sync_at,omitempty"`
	LastSyncError string                `gorm:"type:text" json:"last_sync_error,omitempty"`
	LastSyncLog   string                `gorm:"type:text" json:"last_sync_log,omitempty"` // Stderr/warnings from ansible-inventory
	HostsCount    int                   `gorm:"default:0" json:"hosts_count"`             // Number of hosts discovered

	// Enabled flag
	Enabled bool `gorm:"default:true" json:"enabled"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// Relationships
	Inventory  AnsibleInventory   `gorm:"foreignKey:InventoryID" json:"inventory,omitempty"`
	Credential *AnsibleCredential `gorm:"foreignKey:CredentialID" json:"credential,omitempty"`
}

func (s *AnsibleInventorySource) BeforeCreate(tx *gorm.DB) error {
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	if s.Config == nil {
		s.Config = make(InventorySourceConfig)
	}
	return nil
}

// AWSInventoryConfig represents AWS-specific configuration for the amazon.aws.ec2 plugin
type AWSInventoryConfig struct {
	Regions []string `json:"regions,omitempty"` // AWS regions to query (empty = all)
	Filters []struct {
		Name   string   `json:"name"`   // Filter name (e.g., "tag:Environment", "instance-state-name")
		Values []string `json:"values"` // Filter values (e.g., ["production"])
	} `json:"filters,omitempty"`

	// Grouping options
	GroupByInstanceType     bool     `json:"group_by_instance_type,omitempty"`     // Group by EC2 instance type
	GroupByRegion           bool     `json:"group_by_region,omitempty"`            // Group by AWS region
	GroupByAvailabilityZone bool     `json:"group_by_availability_zone,omitempty"` // Group by availability zone
	GroupByVPC              bool     `json:"group_by_vpc,omitempty"`               // Group by VPC ID
	GroupByTags             []string `json:"group_by_tags,omitempty"`              // Tags to group by (e.g., ["Environment", "Project"])

	// Host configuration
	HostnameVariable     string `json:"hostname_variable,omitempty"` // Variable to use as hostname (private-dns-name, private-ip-address, etc.)
	IncludeIPv6Addresses bool   `json:"include_ipv6_addresses,omitempty"`
	StrictPermissions    bool   `json:"strict_permissions,omitempty"` // Fail if any region fails
}

// AzureInventoryConfig represents Azure-specific configuration for azure.azcollection.azure_rm plugin
type AzureInventoryConfig struct {
	SubscriptionID   string   `json:"subscription_id,omitempty"`    // Azure subscription ID
	ResourceGroups   []string `json:"resource_groups,omitempty"`    // Filter by resource groups
	Locations        []string `json:"locations,omitempty"`          // Filter by locations/regions
	PowerStateFilter string   `json:"power_state_filter,omitempty"` // running, stopped, etc.

	// Grouping options
	GroupByResourceGroup bool     `json:"group_by_resource_group,omitempty"` // Group by Azure resource group
	GroupByLocation      bool     `json:"group_by_location,omitempty"`       // Group by Azure location/region
	GroupByOSType        bool     `json:"group_by_os_type,omitempty"`        // Group by OS type (Windows/Linux)
	GroupByTags          []string `json:"group_by_tags,omitempty"`           // Tags to group by

	// Host configuration
	HostnameVariable string `json:"hostname_variable,omitempty"` // Variable to use as hostname
}

// GCPInventoryConfig represents GCP-specific configuration for google.cloud.gcp_compute plugin
type GCPInventoryConfig struct {
	Projects []string `json:"projects,omitempty"` // GCP projects to query
	Zones    []string `json:"zones,omitempty"`    // Filter by zones
	Filters  []string `json:"filters,omitempty"`  // GCP filter expressions

	// Grouping options
	GroupByProject     bool     `json:"group_by_project,omitempty"`      // Group by GCP project
	GroupByZone        bool     `json:"group_by_zone,omitempty"`         // Group by zone
	GroupByMachineType bool     `json:"group_by_machine_type,omitempty"` // Group by machine type
	GroupByNetwork     bool     `json:"group_by_network,omitempty"`      // Group by VPC network
	GroupByLabels      []string `json:"group_by_labels,omitempty"`       // Labels to group by

	// Host configuration
	HostnameVariable string `json:"hostname_variable,omitempty"` // Variable to use as hostname
}

// VMwareInventoryConfig represents VMware-specific configuration for community.vmware.vmware_vm_inventory plugin
type VMwareInventoryConfig struct {
	// vCenter server hostname (required)
	Hostname          string   `json:"hostname"`                      // vCenter server hostname
	Datacenter        string   `json:"datacenter,omitempty"`          // Filter by datacenter
	Clusters          []string `json:"clusters,omitempty"`            // Filter by clusters
	Folders           []string `json:"folders,omitempty"`             // Filter by VM folders
	ResourcePools     []string `json:"resource_pools,omitempty"`      // Filter by resource pools
	ValidateCerts     *bool    `json:"validate_certs,omitempty"`      // SSL certificate validation (default: false)
	IncludePoweredOff bool     `json:"include_powered_off,omitempty"` // Include powered off VMs

	// Grouping options
	GroupByDatacenter bool     `json:"group_by_datacenter,omitempty"` // Group by datacenter
	GroupByCluster    bool     `json:"group_by_cluster,omitempty"`    // Group by cluster
	GroupByFolder     bool     `json:"group_by_folder,omitempty"`     // Group by folder
	GroupByGuestOS    bool     `json:"group_by_guest_os,omitempty"`   // Group by guest OS
	GroupByTags       []string `json:"group_by_tags,omitempty"`       // Tags to group by

	// Host configuration
	HostnameVariable string `json:"hostname_variable,omitempty"` // Variable to use as hostname
}
