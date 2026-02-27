// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansible

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/oidc"
	"github.com/iac-platform/backend/pkg/crypto"
	"github.com/michielvha/logger"
	"gopkg.in/yaml.v3"
)

// InventorySourceService handles dynamic inventory source operations
// This version uses native Ansible inventory plugins via ansible-inventory CLI
type InventorySourceService struct {
	sourceRepo       *repository.AnsibleInventorySourceRepository
	inventoryRepo    *repository.AnsibleInventoryRepository
	credentialRepo   *repository.AnsibleCredentialRepository
	cryptoService    *crypto.CryptoService
	azureOIDCRepo    *repository.AzureOIDCConfigurationRepository // Optional: for OIDC workload identity auth
	oidcTokenService *oidc.TokenService                           // Optional: for generating OIDC tokens
}

// NewInventorySourceService creates a new inventory source service
func NewInventorySourceService(
	sourceRepo *repository.AnsibleInventorySourceRepository,
	inventoryRepo *repository.AnsibleInventoryRepository,
	credentialRepo *repository.AnsibleCredentialRepository,
	cryptoService *crypto.CryptoService,
) *InventorySourceService {
	return &InventorySourceService{
		sourceRepo:     sourceRepo,
		inventoryRepo:  inventoryRepo,
		credentialRepo: credentialRepo,
		cryptoService:  cryptoService,
	}
}

// SetOIDCServices configures OIDC workload identity support for Azure inventory sync.
// When configured, Azure inventory sources can authenticate using OIDC instead of client secrets.
func (s *InventorySourceService) SetOIDCServices(
	azureOIDCRepo *repository.AzureOIDCConfigurationRepository,
	oidcTokenService *oidc.TokenService,
) {
	s.azureOIDCRepo = azureOIDCRepo
	s.oidcTokenService = oidcTokenService
}

// CreateInventorySource creates a new inventory source
func (s *InventorySourceService) CreateInventorySource(
	inventoryID uuid.UUID,
	name, description string,
	sourceType models.InventorySourceType,
	credentialID *uuid.UUID,
	config models.InventorySourceConfig,
) (*models.AnsibleInventorySource, error) {
	// Validate inventory exists
	if _, err := s.inventoryRepo.GetByID(inventoryID); err != nil {
		return nil, fmt.Errorf("inventory not found: %w", err)
	}

	// Validate credential if provided
	if credentialID != nil {
		cred, err := s.credentialRepo.GetByID(*credentialID)
		if err != nil {
			return nil, fmt.Errorf("credential not found: %w", err)
		}
		// Validate credential type matches source type
		if !s.isCredentialTypeValidForSource(cred.Type, sourceType) {
			return nil, fmt.Errorf("credential type %s is not valid for source type %s", cred.Type, sourceType)
		}
	}

	source := &models.AnsibleInventorySource{
		InventoryID:  inventoryID,
		Name:         name,
		Description:  description,
		Type:         sourceType,
		CredentialID: credentialID,
		Config:       config,
		Status:       models.InventorySourceStatusNeverSynced,
		Enabled:      true,
	}

	if err := s.sourceRepo.Create(source); err != nil {
		return nil, fmt.Errorf("failed to create inventory source: %w", err)
	}

	return source, nil
}

// GetInventorySource retrieves an inventory source by ID
func (s *InventorySourceService) GetInventorySource(id uuid.UUID) (*models.AnsibleInventorySource, error) {
	return s.sourceRepo.GetByID(id)
}

// MarkSyncing sets the inventory source status to syncing
func (s *InventorySourceService) MarkSyncing(id uuid.UUID) error {
	return s.sourceRepo.UpdateSyncStatus(id, models.InventorySourceStatusSyncing, "", 0)
}

// MarkSyncFailed sets the inventory source status to failed with an error message
func (s *InventorySourceService) MarkSyncFailed(id uuid.UUID, errMsg string) error {
	return s.sourceRepo.UpdateSyncStatus(id, models.InventorySourceStatusFailed, errMsg, 0)
}

// ListInventorySources lists inventory sources for an inventory
func (s *InventorySourceService) ListInventorySources(inventoryID uuid.UUID, limit, offset int) ([]models.AnsibleInventorySource, int64, error) {
	return s.sourceRepo.ListByInventory(inventoryID, limit, offset)
}

// UpdateInventorySource updates an inventory source
// UpdateInventorySourceOptions holds optional fields for updating an inventory source
type UpdateInventorySourceOptions struct {
	Name               *string
	Description        *string
	CredentialID       *uuid.UUID // If non-nil, sets credential; use uuid.Nil to clear
	ClearCredential    bool       // If true, clears the credential (sets to nil)
	Config             *models.InventorySourceConfig
	Enabled            *bool
	HostnameVar        *string
	GroupByRegion      *bool
	GroupByAZ          *bool
	GroupByInstanceID  *bool
	GroupByTag         *string
	InstanceFilters    *string
	UpdateOnLaunch     *bool
	UpdateCacheTimeout *int
	SyncSchedule       *string
}

func (s *InventorySourceService) UpdateInventorySource(
	id uuid.UUID,
	opts UpdateInventorySourceOptions,
) (*models.AnsibleInventorySource, error) {
	source, err := s.sourceRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("inventory source not found: %w", err)
	}

	if opts.Name != nil {
		source.Name = *opts.Name
	}
	if opts.Description != nil {
		source.Description = *opts.Description
	}
	if opts.ClearCredential {
		source.CredentialID = nil
	} else if opts.CredentialID != nil {
		source.CredentialID = opts.CredentialID
	}
	if opts.Config != nil {
		source.Config = *opts.Config
	}
	if opts.Enabled != nil {
		source.Enabled = *opts.Enabled
	}
	if opts.HostnameVar != nil {
		source.HostnameVar = *opts.HostnameVar
	}
	if opts.GroupByRegion != nil {
		source.GroupByRegion = *opts.GroupByRegion
	}
	if opts.GroupByAZ != nil {
		source.GroupByAvailabilityZone = *opts.GroupByAZ
	}
	if opts.GroupByInstanceID != nil {
		source.GroupByInstanceID = *opts.GroupByInstanceID
	}
	if opts.GroupByTag != nil {
		source.GroupByTag = *opts.GroupByTag
	}
	if opts.InstanceFilters != nil {
		source.InstanceFilters = *opts.InstanceFilters
	}
	if opts.UpdateOnLaunch != nil {
		source.UpdateOnLaunch = *opts.UpdateOnLaunch
	}
	if opts.UpdateCacheTimeout != nil {
		source.UpdateCacheTimeout = *opts.UpdateCacheTimeout
	}
	if opts.SyncSchedule != nil {
		source.SyncSchedule = *opts.SyncSchedule
	}

	if err := s.sourceRepo.Update(source); err != nil {
		return nil, fmt.Errorf("failed to update inventory source: %w", err)
	}

	return source, nil
}

// DeleteInventorySource deletes an inventory source
func (s *InventorySourceService) DeleteInventorySource(id uuid.UUID) error {
	return s.sourceRepo.Delete(id)
}

// SyncInventorySource synchronizes a dynamic inventory source using native Ansible plugins
func (s *InventorySourceService) SyncInventorySource(ctx context.Context, id uuid.UUID) (*SyncResult, error) {
	source, err := s.sourceRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("inventory source not found: %w", err)
	}

	if !source.Enabled {
		return nil, fmt.Errorf("inventory source is disabled")
	}

	// Mark as syncing
	if err := s.sourceRepo.UpdateSyncStatus(id, models.InventorySourceStatusSyncing, "", 0); err != nil {
		return nil, fmt.Errorf("failed to update sync status: %w", err)
	}

	startTime := time.Now()
	result := &SyncResult{
		Groups: []string{},
		Errors: []string{},
	}

	// Generate the inventory plugin YAML file
	inventoryYAML, err := s.generateInventoryPluginYAML(source)
	if err != nil {
		if updateErr := s.sourceRepo.UpdateSyncStatus(id, models.InventorySourceStatusFailed, err.Error(), 0); updateErr != nil {
			logger.Warnf("Failed to update sync status: %v", updateErr)
		}
		return nil, fmt.Errorf("failed to generate inventory plugin config: %w", err)
	}

	// Create temporary directory for inventory file
	tempDir, err := os.MkdirTemp("", "ansible-inventory-*")
	if err != nil {
		if updateErr := s.sourceRepo.UpdateSyncStatus(id, models.InventorySourceStatusFailed, err.Error(), 0); updateErr != nil {
			logger.Warnf("Failed to update sync status: %v", updateErr)
		}
		return nil, fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			logger.Warnf("Failed to remove temp directory %s: %v", tempDir, err)
		}
	}()

	// Write inventory plugin file
	inventoryFile := filepath.Join(tempDir, s.getInventoryFileName(source.Type))
	if err := os.WriteFile(inventoryFile, []byte(inventoryYAML), 0o600); err != nil {
		if updateErr := s.sourceRepo.UpdateSyncStatus(id, models.InventorySourceStatusFailed, err.Error(), 0); updateErr != nil {
			logger.Warnf("Failed to update sync status: %v", updateErr)
		}
		return nil, fmt.Errorf("failed to write inventory file: %w", err)
	}

	// Set up environment variables for credentials
	env := os.Environ()
	credEnv, err := s.getCredentialEnvironment(source, tempDir)
	if err != nil {
		if updateErr := s.sourceRepo.UpdateSyncStatus(id, models.InventorySourceStatusFailed, err.Error(), 0); updateErr != nil {
			logger.Warnf("Failed to update sync status: %v", updateErr)
		}
		return nil, fmt.Errorf("failed to get credential environment: %w", err)
	}
	env = append(env, credEnv...)

	// Determine command: use OIDC-aware wrapper for Azure sources when OIDC env vars are set,
	// otherwise use ansible-inventory directly
	useOIDCWrapper := false
	for _, e := range env {
		if strings.HasPrefix(e, "AZURE_FEDERATED_TOKEN_FILE=") {
			useOIDCWrapper = true
			break
		}
	}

	var cmd *exec.Cmd
	if useOIDCWrapper {
		cmd = exec.CommandContext(ctx, "python3", "/usr/local/bin/oidc-ansible-inventory", "-i", inventoryFile, "--list") //nolint:gosec // intentional: executing ansible OIDC wrapper
		logger.Infof("Using OIDC-aware ansible-inventory wrapper for source %s", id)
	} else {
		cmd = exec.CommandContext(ctx, "ansible-inventory", "-i", inventoryFile, "--list") //nolint:gosec // intentional: executing ansible command
	}
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := fmt.Sprintf("ansible-inventory failed: %v\nstderr: %s", err, stderr.String())
		if updateErr := s.sourceRepo.UpdateSyncStatus(id, models.InventorySourceStatusFailed, errMsg, 0, stderr.String()); updateErr != nil {
			logger.Warnf("Failed to update sync status: %v", updateErr)
		}
		return nil, errors.New(errMsg)
	}

	// Parse the JSON output from ansible-inventory
	var inventoryOutput map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &inventoryOutput); err != nil {
		errMsg := fmt.Sprintf("failed to parse ansible-inventory output: %v", err)
		if updateErr := s.sourceRepo.UpdateSyncStatus(id, models.InventorySourceStatusFailed, errMsg, 0); updateErr != nil {
			logger.Warnf("Failed to update sync status: %v", updateErr)
		}
		return nil, errors.New(errMsg)
	}

	// Process the inventory output and update the inventory
	hostsDiscovered, err := s.processInventoryOutput(ctx, source.InventoryID, inventoryOutput)
	if err != nil {
		if updateErr := s.sourceRepo.UpdateSyncStatus(id, models.InventorySourceStatusFailed, err.Error(), 0); updateErr != nil {
			logger.Warnf("Failed to update sync status: %v", updateErr)
		}
		return nil, fmt.Errorf("failed to process inventory output: %w", err)
	}

	result.HostsDiscovered = hostsDiscovered
	result.Duration = time.Since(startTime)

	// Extract groups from the output
	for groupName := range inventoryOutput {
		if groupName != "_meta" && groupName != "all" {
			result.Groups = append(result.Groups, groupName)
		}
	}

	// Update status to successful, preserving stderr as sync log (may contain deprecation warnings)
	if err := s.sourceRepo.UpdateSyncStatus(id, models.InventorySourceStatusSuccessful, "", hostsDiscovered, stderr.String()); err != nil {
		logger.Warnf("Failed to update sync status to successful: %v", err)
	}

	return result, nil
}

// SyncResult contains the result of an inventory source sync
type SyncResult struct {
	HostsDiscovered int           `json:"hosts_discovered"`
	HostsCreated    int           `json:"hosts_created"`
	HostsUpdated    int           `json:"hosts_updated"`
	HostsRemoved    int           `json:"hosts_removed"`
	Groups          []string      `json:"groups"`
	Errors          []string      `json:"errors,omitempty"`
	Duration        time.Duration `json:"duration"`
}

// generateInventoryPluginYAML generates the inventory plugin YAML configuration
func (s *InventorySourceService) generateInventoryPluginYAML(source *models.AnsibleInventorySource) (string, error) {
	switch source.Type {
	case models.InventorySourceTypeAWS:
		return s.generateAWSInventoryYAML(source)
	case models.InventorySourceTypeAzure:
		return s.generateAzureInventoryYAML(source)
	case models.InventorySourceTypeGCP:
		return s.generateGCPInventoryYAML(source)
	case models.InventorySourceTypeVMware:
		return s.generateVMwareInventoryYAML(source)
	case models.InventorySourceTypeCustom:
		// Custom inventory sources use a script or plugin
		// For custom types, the Config should contain the inventory script/plugin configuration
		// This would need to be implemented based on the actual custom inventory format
		return "", fmt.Errorf("custom inventory source type not yet fully implemented")
	default:
		return "", fmt.Errorf("unsupported inventory source type: %s", source.Type)
	}
}

// getInventoryFileName returns the appropriate inventory plugin file name
func (s *InventorySourceService) getInventoryFileName(sourceType models.InventorySourceType) string {
	switch sourceType {
	case models.InventorySourceTypeAWS:
		return "aws_ec2.yml"
	case models.InventorySourceTypeAzure:
		return "azure_rm.yml"
	case models.InventorySourceTypeGCP:
		return "gcp_compute.yml"
	case models.InventorySourceTypeVMware:
		return "vmware.yml"
	case models.InventorySourceTypeCustom:
		return "custom_inventory.yml"
	default:
		return "inventory.yml"
	}
}

// generateAWSInventoryYAML generates AWS EC2 inventory plugin configuration
// Uses the amazon.aws.ec2 collection plugin
func (s *InventorySourceService) generateAWSInventoryYAML(source *models.AnsibleInventorySource) (string, error) {
	// Parse AWS config
	var awsConfig models.AWSInventoryConfig
	configBytes, err := json.Marshal(source.Config)
	if err != nil {
		return "", fmt.Errorf("failed to marshal AWS config: %w", err)
	}
	if err := json.Unmarshal(configBytes, &awsConfig); err != nil {
		return "", fmt.Errorf("invalid AWS config: %w", err)
	}

	// Build inventory plugin configuration
	inventoryConfig := map[string]interface{}{
		"plugin": "amazon.aws.ec2",
	}

	// Set regions
	if len(awsConfig.Regions) > 0 {
		inventoryConfig["regions"] = awsConfig.Regions
	}

	// Set filters
	if len(awsConfig.Filters) > 0 {
		filters := make(map[string][]string)
		for _, f := range awsConfig.Filters {
			filters[f.Name] = f.Values
		}
		inventoryConfig["filters"] = filters
	}

	// Set keyed groups for dynamic grouping
	keyedGroups := []map[string]interface{}{}

	// Group by instance type
	if awsConfig.GroupByInstanceType {
		keyedGroups = append(keyedGroups, map[string]interface{}{
			"key":    "instance_type",
			"prefix": "instance_type",
		})
	}

	// Group by region
	if awsConfig.GroupByRegion {
		keyedGroups = append(keyedGroups, map[string]interface{}{
			"key":    "placement.region",
			"prefix": "region",
		})
	}

	// Group by availability zone
	if awsConfig.GroupByAvailabilityZone {
		keyedGroups = append(keyedGroups, map[string]interface{}{
			"key":    "placement.availability_zone",
			"prefix": "az",
		})
	}

	// Group by VPC
	if awsConfig.GroupByVPC {
		keyedGroups = append(keyedGroups, map[string]interface{}{
			"key":    "vpc_id",
			"prefix": "vpc",
		})
	}

	// Group by tags
	if len(awsConfig.GroupByTags) > 0 {
		for _, tag := range awsConfig.GroupByTags {
			keyedGroups = append(keyedGroups, map[string]interface{}{
				"key":    fmt.Sprintf("tags.%s", tag),
				"prefix": fmt.Sprintf("tag_%s", strings.ReplaceAll(tag, "-", "_")),
			})
		}
	}

	if len(keyedGroups) > 0 {
		inventoryConfig["keyed_groups"] = keyedGroups
	}

	// Set hostnames preference
	if awsConfig.HostnameVariable != "" {
		inventoryConfig["hostnames"] = []string{awsConfig.HostnameVariable}
	} else {
		// Default: prefer private DNS, then private IP, then public DNS
		inventoryConfig["hostnames"] = []string{
			"private-dns-name",
			"private-ip-address",
			"dns-name",
			"ip-address",
		}
	}

	// Include extra variables from instance data
	inventoryConfig["compose"] = map[string]interface{}{
		"ansible_host":           "private_ip_address | default(public_ip_address)",
		"ec2_instance_id":        "instance_id",
		"ec2_instance_type":      "instance_type",
		"ec2_region":             "placement.region",
		"ec2_availability_zone":  "placement.availability_zone",
		"ec2_vpc_id":             "vpc_id",
		"ec2_subnet_id":          "subnet_id",
		"ec2_security_groups":    "security_groups | map(attribute='group_name') | list",
		"ec2_private_ip_address": "private_ip_address",
		"ec2_public_ip_address":  "public_ip_address | default('')",
		"ec2_tags":               "tags",
	}

	// Convert to YAML
	yamlBytes, err := yaml.Marshal(inventoryConfig)
	if err != nil {
		return "", fmt.Errorf("failed to generate YAML: %w", err)
	}

	return string(yamlBytes), nil
}

// generateAzureInventoryYAML generates Azure Resource Manager inventory plugin configuration
// Uses the azure.azcollection.azure_rm collection plugin
func (s *InventorySourceService) generateAzureInventoryYAML(source *models.AnsibleInventorySource) (string, error) {
	var azureConfig models.AzureInventoryConfig
	configBytes, err := json.Marshal(source.Config)
	if err != nil {
		return "", fmt.Errorf("failed to marshal Azure config: %w", err)
	}
	if err := json.Unmarshal(configBytes, &azureConfig); err != nil {
		return "", fmt.Errorf("invalid Azure config: %w", err)
	}

	inventoryConfig := map[string]interface{}{
		"plugin": "azure.azcollection.azure_rm",
	}

	// Set subscription ID
	if azureConfig.SubscriptionID != "" {
		inventoryConfig["subscription_id"] = azureConfig.SubscriptionID
	}

	// Set resource groups filter
	if len(azureConfig.ResourceGroups) > 0 {
		inventoryConfig["include_vm_resource_groups"] = azureConfig.ResourceGroups
	}

	// Set location filter
	if len(azureConfig.Locations) > 0 {
		inventoryConfig["include_vm_locations"] = azureConfig.Locations
	}

	// Set power state filter (e.g., "running", "stopped")
	if azureConfig.PowerStateFilter != "" {
		// The azure_rm plugin uses conditional_groups to filter by power state
		// We add a group that only includes VMs matching the desired power state
		inventoryConfig["conditional_groups"] = map[string]interface{}{
			"power_state_" + azureConfig.PowerStateFilter: fmt.Sprintf("powerstate == '%s'", azureConfig.PowerStateFilter),
		}
	}

	// Add conditional groups
	conditionalGroups := []map[string]interface{}{}

	if azureConfig.GroupByResourceGroup {
		conditionalGroups = append(conditionalGroups, map[string]interface{}{
			"key":    "resource_group",
			"prefix": "rg",
		})
	}

	if azureConfig.GroupByLocation {
		conditionalGroups = append(conditionalGroups, map[string]interface{}{
			"key":    "location",
			"prefix": "location",
		})
	}

	if azureConfig.GroupByOSType {
		conditionalGroups = append(conditionalGroups, map[string]interface{}{
			"key":    "os_disk.os_type",
			"prefix": "os",
		})
	}

	if len(conditionalGroups) > 0 {
		inventoryConfig["keyed_groups"] = conditionalGroups
	}

	// Set hostname
	if azureConfig.HostnameVariable != "" {
		inventoryConfig["hostvar_expressions"] = map[string]interface{}{
			"ansible_host": azureConfig.HostnameVariable,
		}
	} else {
		inventoryConfig["hostvar_expressions"] = map[string]interface{}{
			"ansible_host": "private_ipv4_addresses[0] if private_ipv4_addresses else public_ipv4_addresses[0] if public_ipv4_addresses else name",
		}
	}

	yamlBytes, err := yaml.Marshal(inventoryConfig)
	if err != nil {
		return "", fmt.Errorf("failed to generate YAML: %w", err)
	}

	return string(yamlBytes), nil
}

// generateGCPInventoryYAML generates GCP Compute inventory plugin configuration
// Uses the google.cloud.gcp_compute collection plugin
func (s *InventorySourceService) generateGCPInventoryYAML(source *models.AnsibleInventorySource) (string, error) {
	var gcpConfig models.GCPInventoryConfig
	configBytes, err := json.Marshal(source.Config)
	if err != nil {
		return "", fmt.Errorf("failed to marshal GCP config: %w", err)
	}
	if err := json.Unmarshal(configBytes, &gcpConfig); err != nil {
		return "", fmt.Errorf("invalid GCP config: %w", err)
	}

	inventoryConfig := map[string]interface{}{
		"plugin": "google.cloud.gcp_compute",
	}

	// Set projects
	if len(gcpConfig.Projects) > 0 {
		inventoryConfig["projects"] = gcpConfig.Projects
	}

	// Set zones
	if len(gcpConfig.Zones) > 0 {
		inventoryConfig["zones"] = gcpConfig.Zones
	}

	// Set filters
	if len(gcpConfig.Filters) > 0 {
		inventoryConfig["filters"] = gcpConfig.Filters
	}

	// Set keyed groups
	keyedGroups := []map[string]interface{}{}

	if gcpConfig.GroupByProject {
		keyedGroups = append(keyedGroups, map[string]interface{}{
			"key":    "project",
			"prefix": "project",
		})
	}

	if gcpConfig.GroupByZone {
		keyedGroups = append(keyedGroups, map[string]interface{}{
			"key":    "zone",
			"prefix": "zone",
		})
	}

	if gcpConfig.GroupByMachineType {
		keyedGroups = append(keyedGroups, map[string]interface{}{
			"key":    "machineType",
			"prefix": "machine_type",
		})
	}

	if gcpConfig.GroupByNetwork {
		keyedGroups = append(keyedGroups, map[string]interface{}{
			"key":    "networkInterfaces[0].network",
			"prefix": "network",
		})
	}

	if len(keyedGroups) > 0 {
		inventoryConfig["keyed_groups"] = keyedGroups
	}

	// Set hostname
	if gcpConfig.HostnameVariable != "" {
		inventoryConfig["hostnames"] = []string{gcpConfig.HostnameVariable}
	} else {
		inventoryConfig["hostnames"] = []string{
			"name",
			"networkInterfaces[0].networkIP",
		}
	}

	// Set compose variables
	inventoryConfig["compose"] = map[string]interface{}{
		"ansible_host":     "networkInterfaces[0].networkIP",
		"gcp_instance_id":  "id",
		"gcp_project":      "project",
		"gcp_zone":         "zone",
		"gcp_machine_type": "machineType.split('/')[-1]",
	}

	yamlBytes, err := yaml.Marshal(inventoryConfig)
	if err != nil {
		return "", fmt.Errorf("failed to generate YAML: %w", err)
	}

	return string(yamlBytes), nil
}

// generateVMwareInventoryYAML generates VMware vSphere inventory plugin configuration
// Uses the community.vmware.vmware_vm_inventory collection plugin
func (s *InventorySourceService) generateVMwareInventoryYAML(source *models.AnsibleInventorySource) (string, error) {
	var vmwareConfig models.VMwareInventoryConfig
	configBytes, _ := json.Marshal(source.Config) //nolint:errchkjson // config is already validated
	if err := json.Unmarshal(configBytes, &vmwareConfig); err != nil {
		return "", fmt.Errorf("invalid VMware config: %w", err)
	}

	inventoryConfig := map[string]interface{}{
		"plugin": "community.vmware.vmware_vm_inventory",
	}

	// vCenter hostname comes from credential
	if vmwareConfig.ValidateCerts != nil {
		inventoryConfig["validate_certs"] = *vmwareConfig.ValidateCerts
	} else {
		inventoryConfig["validate_certs"] = false
	}

	// Set datacenter
	if vmwareConfig.Datacenter != "" {
		inventoryConfig["resources"] = []map[string]interface{}{
			{"datacenter": vmwareConfig.Datacenter},
		}
	}

	// Set filters for powered on VMs only by default
	if !vmwareConfig.IncludePoweredOff {
		inventoryConfig["filters"] = []map[string]interface{}{
			{"runtime.powerState": []string{"poweredOn"}},
		}
	}

	// Set properties to fetch
	inventoryConfig["properties"] = []string{
		"name",
		"guest.hostName",
		"guest.ipAddress",
		"config.cpuHotAddEnabled",
		"config.cpuHotRemoveEnabled",
		"config.guestFullName",
		"config.guestId",
		"config.hardware.memoryMB",
		"config.hardware.numCPU",
		"config.template",
		"runtime.powerState",
		"guest.net",
	}

	// Set keyed groups
	keyedGroups := []map[string]interface{}{}

	if vmwareConfig.GroupByDatacenter {
		keyedGroups = append(keyedGroups, map[string]interface{}{
			"key":    "datacenter",
			"prefix": "dc",
		})
	}

	if vmwareConfig.GroupByCluster {
		keyedGroups = append(keyedGroups, map[string]interface{}{
			"key":    "cluster",
			"prefix": "cluster",
		})
	}

	if vmwareConfig.GroupByFolder {
		keyedGroups = append(keyedGroups, map[string]interface{}{
			"key":    "folder",
			"prefix": "folder",
		})
	}

	if vmwareConfig.GroupByGuestOS {
		keyedGroups = append(keyedGroups, map[string]interface{}{
			"key":    "config.guestId",
			"prefix": "guest",
		})
	}

	if len(keyedGroups) > 0 {
		inventoryConfig["keyed_groups"] = keyedGroups
	}

	// Set hostname preference
	inventoryConfig["hostnames"] = []string{
		"config.name",
		"guest.ipAddress",
	}

	// Set compose variables
	inventoryConfig["compose"] = map[string]interface{}{
		"ansible_host": "guest.ipAddress | default(config.name)",
		"vmware_uuid":  "config.uuid",
	}

	yamlBytes, err := yaml.Marshal(inventoryConfig)
	if err != nil {
		return "", fmt.Errorf("failed to generate YAML: %w", err)
	}

	return string(yamlBytes), nil
}

// getCredentialEnvironment returns environment variables for cloud provider authentication.
// For Azure, OIDC workload identity is preferred over credential-based auth when configured.
// tempDir is used to write temporary credential files (e.g., OIDC token file for Azure).
func (s *InventorySourceService) getCredentialEnvironment(source *models.AnsibleInventorySource, tempDir string) ([]string, error) {
	// For Azure sources, try OIDC workload identity first (no credential needed)
	if source.Type == models.InventorySourceTypeAzure {
		oidcEnv, err := s.getAzureOIDCEnvironment(source, tempDir)
		if err != nil {
			logger.Warnf("OIDC auth unavailable for Azure inventory source %s: %v (falling back to credential)", source.ID, err)
		} else if len(oidcEnv) > 0 {
			logger.Infof("Using OIDC workload identity for Azure inventory source %s", source.ID)
			return oidcEnv, nil
		}
	}

	// Fall back to credential-based auth
	if source.CredentialID == nil {
		if source.Type == models.InventorySourceTypeAzure {
			return nil, fmt.Errorf("no OIDC configuration or credential found for Azure inventory source; configure an Azure OIDC configuration at the organization level or attach an Azure credential")
		}
		return nil, nil
	}

	cred, err := s.credentialRepo.GetByID(*source.CredentialID)
	if err != nil {
		return nil, fmt.Errorf("credential not found: %w", err)
	}

	var env []string

	switch source.Type {
	case models.InventorySourceTypeAWS:
		if cred.AWSAccessKeyID != "" {
			accessKeyID, err := s.cryptoService.Decrypt(cred.AWSAccessKeyID)
			if err != nil {
				return nil, fmt.Errorf("failed to decrypt access key ID: %w", err)
			}
			env = append(env, fmt.Sprintf("AWS_ACCESS_KEY_ID=%s", accessKeyID))
		}
		if cred.AWSSecretAccessKey != "" {
			secretAccessKey, err := s.cryptoService.Decrypt(cred.AWSSecretAccessKey)
			if err != nil {
				return nil, fmt.Errorf("failed to decrypt secret access key: %w", err)
			}
			env = append(env, fmt.Sprintf("AWS_SECRET_ACCESS_KEY=%s", secretAccessKey))
		}
	case models.InventorySourceTypeCustom:
		// Custom inventory sources may not need specific credentials
		// Credentials would be handled by the custom script/plugin itself
		// Note: AWS session token could be added if needed

	case models.InventorySourceTypeAzure:
		// Subscription ID comes from the inventory source config, not credential
		if cred.AzureClientID != "" {
			env = append(env, fmt.Sprintf("AZURE_CLIENT_ID=%s", cred.AzureClientID))
		}
		if cred.AzureClientSecret != "" {
			clientSecret, err := s.cryptoService.Decrypt(cred.AzureClientSecret)
			if err != nil {
				return nil, fmt.Errorf("failed to decrypt Azure client secret: %w", err)
			}
			env = append(env, fmt.Sprintf("AZURE_CLIENT_SECRET=%s", clientSecret))
		}
		if cred.AzureTenantID != "" {
			env = append(env, fmt.Sprintf("AZURE_TENANT=%s", cred.AzureTenantID))
		}

	case models.InventorySourceTypeGCP:
		if cred.GCPServiceAccount != "" {
			// For GCP, we write the service account JSON to a temp file
			// and set GOOGLE_APPLICATION_CREDENTIALS
			serviceAccountJSON, err := s.cryptoService.Decrypt(cred.GCPServiceAccount)
			if err != nil {
				return nil, fmt.Errorf("failed to decrypt GCP service account: %w", err)
			}
			tempFile, err := os.CreateTemp("", "gcp-sa-*.json")
			if err != nil {
				return nil, fmt.Errorf("failed to create temp file for GCP credentials: %w", err)
			}
			if _, err := tempFile.WriteString(serviceAccountJSON); err != nil {
				if closeErr := tempFile.Close(); closeErr != nil {
					logger.Warnf("Failed to close temp file after write error: %v", closeErr)
				}
				return nil, fmt.Errorf("failed to write GCP credentials: %w", err)
			}
			if err := tempFile.Close(); err != nil {
				logger.Warnf("Failed to close temp file: %v", err)
			}
			env = append(env, fmt.Sprintf("GOOGLE_APPLICATION_CREDENTIALS=%s", tempFile.Name()))
		}

	case models.InventorySourceTypeVMware:
		// Get VMware hostname from config
		var vmwareConfig models.VMwareInventoryConfig
		configBytes, _ := json.Marshal(source.Config) //nolint:errchkjson // config is already validated
		if err := json.Unmarshal(configBytes, &vmwareConfig); err == nil && vmwareConfig.Hostname != "" {
			env = append(env, fmt.Sprintf("VMWARE_HOST=%s", vmwareConfig.Hostname))
		}
		// VMware credentials use Username and Password from the credential model
		if cred.Username != "" {
			env = append(env, fmt.Sprintf("VMWARE_USER=%s", cred.Username))
		}
		if cred.Password != "" {
			password, err := s.cryptoService.Decrypt(cred.Password)
			if err != nil {
				return nil, fmt.Errorf("failed to decrypt VMware password: %w", err)
			}
			env = append(env, fmt.Sprintf("VMWARE_PASSWORD=%s", password))
		}
	}

	return env, nil
}

// getAzureOIDCEnvironment generates OIDC workload identity environment variables for Azure.
// It reuses the organization's AzureOIDCConfiguration (same one used for Terraform runs)
// to generate a signed JWT and write it to a temp file. The OIDC-aware ansible-inventory
// wrapper reads AZURE_FEDERATED_TOKEN_FILE and uses azure-identity's WorkloadIdentityCredential
// to authenticate natively — no token exchange needed.
func (s *InventorySourceService) getAzureOIDCEnvironment(source *models.AnsibleInventorySource, tempDir string) ([]string, error) {
	if s.azureOIDCRepo == nil || s.oidcTokenService == nil {
		return nil, fmt.Errorf("OIDC services not configured")
	}

	// Get the inventory to find the organization ID
	inventory, err := s.inventoryRepo.GetByID(source.InventoryID)
	if err != nil {
		return nil, fmt.Errorf("failed to get inventory: %w", err)
	}

	// Look up Azure OIDC configuration for the organization
	configs, err := s.azureOIDCRepo.GetByOrganization(inventory.OrganizationID)
	if err != nil {
		return nil, fmt.Errorf("failed to look up Azure OIDC configuration: %w", err)
	}
	if len(configs) == 0 {
		return nil, fmt.Errorf("no Azure OIDC configuration found for organization %s", inventory.OrganizationID)
	}

	config := configs[0]

	// Generate OIDC token with audience set for Azure federated identity
	// Subject format: organization:<org>:project:<project>:inventory:<source_name>:sync
	projectName := "default"
	if inventory.Project != nil {
		projectName = inventory.Project.Name
	}
	token, err := s.oidcTokenService.GenerateWorkloadToken(oidc.WorkloadTokenRequest{
		Audience:         "api://AzureADTokenExchange",
		OrganizationName: inventory.Organization.Name,
		ProjectName:      projectName,
		ResourceType:     oidc.ResourceTypeInventory,
		ResourceName:     source.Name,
		ActionKind:       oidc.ActionSync,
		ActionID:         source.ID.String(),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to generate OIDC token: %w", err)
	}

	// Write the OIDC JWT to a temp file for the WorkloadIdentityCredential
	tokenFile := filepath.Join(tempDir, "oidc-token.jwt")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		return nil, fmt.Errorf("failed to write OIDC token file: %w", err)
	}

	// The OIDC-aware wrapper reads these env vars and creates a WorkloadIdentityCredential
	env := []string{
		fmt.Sprintf("AZURE_CLIENT_ID=%s", config.ClientID),
		fmt.Sprintf("AZURE_TENANT_ID=%s", config.TenantID),
		fmt.Sprintf("AZURE_SUBSCRIPTION_ID=%s", config.SubscriptionID),
		fmt.Sprintf("AZURE_FEDERATED_TOKEN_FILE=%s", tokenFile),
	}

	logger.Infof("OIDC workload identity configured for inventory source %s (org=%s)", source.ID, inventory.OrganizationID)

	return env, nil
}

// processInventoryOutput processes the ansible-inventory JSON output and updates the inventory
func (s *InventorySourceService) processInventoryOutput(ctx context.Context, inventoryID uuid.UUID, output map[string]interface{}) (int, error) {
	hostsDiscovered := 0

	// Get _meta.hostvars for host variables
	hostvars := make(map[string]map[string]interface{})
	if meta, ok := output["_meta"].(map[string]interface{}); ok {
		if hv, ok := meta["hostvars"].(map[string]interface{}); ok {
			for host, vars := range hv {
				if varsMap, ok := vars.(map[string]interface{}); ok {
					hostvars[host] = varsMap
				}
			}
		}
	}

	// Process each group
	processedHosts := make(map[string]bool)
	for groupName, groupData := range output {
		if groupName == "_meta" {
			continue
		}

		groupMap, ok := groupData.(map[string]interface{})
		if !ok {
			continue
		}

		// Get hosts in this group
		hosts, ok := groupMap["hosts"].([]interface{})
		if !ok {
			continue
		}

		// Find or create the group
		var group *models.AnsibleInventoryGroup
		existingGroup, _ := s.inventoryRepo.GetGroupByInventoryAndName(inventoryID, groupName)
		if existingGroup != nil {
			group = existingGroup
		} else {
			group = &models.AnsibleInventoryGroup{
				InventoryID: inventoryID,
				Name:        groupName,
			}
			if err := s.inventoryRepo.CreateGroup(group); err != nil {
				continue
			}
		}

		// Process hosts
		for _, hostInterface := range hosts {
			hostName, ok := hostInterface.(string)
			if !ok {
				continue
			}

			if processedHosts[hostName] {
				continue
			}
			processedHosts[hostName] = true
			hostsDiscovered++

			// Get host variables
			vars := hostvars[hostName]
			if vars == nil {
				vars = make(map[string]interface{})
			}

			// Determine hostname
			hostname := hostName
			if ansibleHost, ok := vars["ansible_host"].(string); ok && ansibleHost != "" {
				hostname = ansibleHost
			}

			// Find or create the host
			existingHost, _ := s.inventoryRepo.GetHostByInventoryAndName(inventoryID, hostName)
			var host *models.AnsibleInventoryHost
			if existingHost != nil {
				// Update existing host
				existingHost.Hostname = hostname
				existingHost.Variables = vars
				if err := s.inventoryRepo.UpdateHost(existingHost); err != nil {
					continue
				}
				host = existingHost
			} else {
				host = &models.AnsibleInventoryHost{
					InventoryID: inventoryID,
					Name:        hostName,
					Hostname:    hostname,
					Variables:   vars,
					Enabled:     true,
				}
				if err := s.inventoryRepo.CreateHost(host); err != nil {
					continue
				}
			}

			// Associate host with group
			if err := s.inventoryRepo.AddHostToGroup(host.ID, group.ID); err != nil {
				logger.Warnf("Failed to add host %s to group %s: %v", host.ID, group.ID, err)
			}
		}
	}

	return hostsDiscovered, nil
}

// isCredentialTypeValidForSource validates that the credential type is valid for the source type
func (s *InventorySourceService) isCredentialTypeValidForSource(credType models.CredentialType, sourceType models.InventorySourceType) bool {
	//nolint:exhaustive // Custom source type doesn't require credentials
	switch sourceType {
	case models.InventorySourceTypeAWS:
		return credType == models.CredentialTypeAWSAccessKey
	case models.InventorySourceTypeAzure:
		return credType == models.CredentialTypeAzure
	case models.InventorySourceTypeGCP:
		return credType == models.CredentialTypeGCP
	case models.InventorySourceTypeVMware:
		return credType == models.CredentialTypeVMware
	default:
		return false
	}
}
