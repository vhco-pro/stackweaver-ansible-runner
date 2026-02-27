// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansible

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
)

// InventoryService handles Ansible inventory operations
type InventoryService struct {
	inventoryRepo *repository.AnsibleInventoryRepository
	orgRepo       *repository.OrganizationRepository
}

// NewInventoryService creates a new inventory service
func NewInventoryService(inventoryRepo *repository.AnsibleInventoryRepository, orgRepo *repository.OrganizationRepository) *InventoryService {
	return &InventoryService{
		inventoryRepo: inventoryRepo,
		orgRepo:       orgRepo,
	}
}

// CreateInventory creates a new inventory
func (s *InventoryService) CreateInventory(orgID uuid.UUID, projectID *uuid.UUID, name, description string, invType models.InventoryType, source string, variables models.InventoryVariables, vcsConnectionID *uuid.UUID, vcsRepository, vcsBranch, inventoryPath string) (*models.AnsibleInventory, error) {
	// Validate type
	switch invType {
	case models.InventoryTypeStatic, models.InventoryTypeDynamic, models.InventoryTypeVCS:
		// Valid
	default:
		invType = models.InventoryTypeStatic
	}

	inventory := &models.AnsibleInventory{
		OrganizationID:  orgID,
		ProjectID:       projectID,
		Name:            name,
		Description:     description,
		Type:            invType,
		Source:          source,
		Variables:       variables,
		VCSConnectionID: vcsConnectionID,
		VCSRepository:   vcsRepository,
		VCSBranch:       vcsBranch,
		InventoryPath:   inventoryPath,
	}

	if inventory.Variables == nil {
		inventory.Variables = make(models.InventoryVariables)
	}

	if err := s.inventoryRepo.Create(inventory); err != nil {
		return nil, fmt.Errorf("failed to create inventory: %w", err)
	}

	return inventory, nil
}

// GetInventory retrieves an inventory by ID
func (s *InventoryService) GetInventory(id uuid.UUID) (*models.AnsibleInventory, error) {
	return s.inventoryRepo.GetByID(id)
}

// ListInventories lists inventories for an organization
func (s *InventoryService) ListInventories(orgID uuid.UUID, limit, offset int) ([]models.AnsibleInventory, int64, error) {
	return s.inventoryRepo.ListByOrganization(orgID, limit, offset)
}

// UpdateInventory updates an inventory
func (s *InventoryService) UpdateInventory(id uuid.UUID, projectID *uuid.UUID, name, description *string, source *string, variables *models.InventoryVariables, vcsConnectionID *uuid.UUID, vcsRepository, vcsBranch, inventoryPath *string) (*models.AnsibleInventory, error) {
	inventory, err := s.inventoryRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("inventory not found: %w", err)
	}

	if projectID != nil {
		inventory.ProjectID = projectID
	}
	if name != nil {
		inventory.Name = *name
	}
	if description != nil {
		inventory.Description = *description
	}
	if source != nil {
		inventory.Source = *source
	}
	if variables != nil {
		inventory.Variables = *variables
	}
	if vcsConnectionID != nil {
		inventory.VCSConnectionID = vcsConnectionID
	}
	if vcsRepository != nil {
		inventory.VCSRepository = *vcsRepository
	}
	if vcsBranch != nil {
		inventory.VCSBranch = *vcsBranch
	}
	if inventoryPath != nil {
		inventory.InventoryPath = *inventoryPath
	}

	if err := s.inventoryRepo.Update(inventory); err != nil {
		return nil, fmt.Errorf("failed to update inventory: %w", err)
	}

	return inventory, nil
}

// DeleteInventory deletes an inventory after checking for dependencies
// Hosts and groups are automatically deleted (cascade)
func (s *InventoryService) DeleteInventory(id uuid.UUID) error {
	// Check for dependencies (job templates, jobs, inventory sources)
	// Note: hosts and groups are allowed - they will be cascade deleted
	templateCount, err := s.inventoryRepo.CountJobTemplatesByInventory(id)
	if err != nil {
		return fmt.Errorf("failed to check job template dependencies: %w", err)
	}

	jobCount, err := s.inventoryRepo.CountJobsByInventory(id)
	if err != nil {
		return fmt.Errorf("failed to check job dependencies: %w", err)
	}

	sourceCount, err := s.inventoryRepo.CountInventorySourcesByInventory(id)
	if err != nil {
		return fmt.Errorf("failed to check inventory source dependencies: %w", err)
	}

	// Build descriptive error message if dependencies exist
	if templateCount > 0 || jobCount > 0 || sourceCount > 0 {
		var deps []string
		if templateCount > 0 {
			deps = append(deps, fmt.Sprintf("%d job template(s)", templateCount))
		}
		if jobCount > 0 {
			deps = append(deps, fmt.Sprintf("%d job(s)", jobCount))
		}
		if sourceCount > 0 {
			deps = append(deps, fmt.Sprintf("%d inventory source(s)", sourceCount))
		}
		return fmt.Errorf("cannot delete inventory: it is referenced by %s. Remove the inventory from those resources first", strings.Join(deps, ", "))
	}

	return s.inventoryRepo.Delete(id)
}

// CreateHost creates a new host in an inventory
func (s *InventoryService) CreateHost(inventoryID uuid.UUID, name, description, hostname string, port int, variables models.InventoryVariables, enabled bool) (*models.AnsibleInventoryHost, error) {
	// Verify inventory exists
	_, err := s.inventoryRepo.GetByID(inventoryID)
	if err != nil {
		return nil, fmt.Errorf("inventory not found: %w", err)
	}

	if port == 0 {
		port = 22
	}

	host := &models.AnsibleInventoryHost{
		InventoryID: inventoryID,
		Name:        name,
		Description: description,
		Hostname:    hostname,
		Port:        port,
		Variables:   variables,
		Enabled:     enabled,
	}

	if host.Variables == nil {
		host.Variables = make(models.InventoryVariables)
	}

	if err := s.inventoryRepo.CreateHost(host); err != nil {
		return nil, fmt.Errorf("failed to create host: %w", err)
	}

	return host, nil
}

// GetHost retrieves a host by ID
func (s *InventoryService) GetHost(id uuid.UUID) (*models.AnsibleInventoryHost, error) {
	return s.inventoryRepo.GetHostByID(id)
}

// ListHosts lists hosts in an inventory
func (s *InventoryService) ListHosts(inventoryID uuid.UUID, limit, offset int) ([]models.AnsibleInventoryHost, int64, error) {
	return s.inventoryRepo.ListHostsByInventory(inventoryID, limit, offset)
}

// UpdateHost updates a host
func (s *InventoryService) UpdateHost(id uuid.UUID, name, description, hostname *string, port *int, variables *models.InventoryVariables, enabled *bool) (*models.AnsibleInventoryHost, error) {
	host, err := s.inventoryRepo.GetHostByID(id)
	if err != nil {
		return nil, fmt.Errorf("host not found: %w", err)
	}

	if name != nil {
		host.Name = *name
	}
	if description != nil {
		host.Description = *description
	}
	if hostname != nil {
		host.Hostname = *hostname
	}
	if port != nil {
		host.Port = *port
	}
	if variables != nil {
		host.Variables = *variables
	}
	if enabled != nil {
		host.Enabled = *enabled
	}

	if err := s.inventoryRepo.UpdateHost(host); err != nil {
		return nil, fmt.Errorf("failed to update host: %w", err)
	}

	return host, nil
}

// DeleteHost deletes a host
func (s *InventoryService) DeleteHost(id uuid.UUID) error {
	return s.inventoryRepo.DeleteHost(id)
}

// CreateGroup creates a new group in an inventory
func (s *InventoryService) CreateGroup(inventoryID uuid.UUID, name, description string, variables models.InventoryVariables, parentID *uuid.UUID) (*models.AnsibleInventoryGroup, error) {
	// Verify inventory exists
	_, err := s.inventoryRepo.GetByID(inventoryID)
	if err != nil {
		return nil, fmt.Errorf("inventory not found: %w", err)
	}

	// Verify parent group exists if specified
	if parentID != nil {
		parent, err := s.inventoryRepo.GetGroupByID(*parentID)
		if err != nil {
			return nil, fmt.Errorf("parent group not found: %w", err)
		}
		if parent.InventoryID != inventoryID {
			return nil, fmt.Errorf("parent group does not belong to this inventory")
		}
	}

	group := &models.AnsibleInventoryGroup{
		InventoryID: inventoryID,
		Name:        name,
		Description: description,
		Variables:   variables,
		ParentID:    parentID,
	}

	if group.Variables == nil {
		group.Variables = make(models.InventoryVariables)
	}

	if err := s.inventoryRepo.CreateGroup(group); err != nil {
		return nil, fmt.Errorf("failed to create group: %w", err)
	}

	return group, nil
}

// GetGroup retrieves a group by ID
func (s *InventoryService) GetGroup(id uuid.UUID) (*models.AnsibleInventoryGroup, error) {
	return s.inventoryRepo.GetGroupByID(id)
}

// ListGroups lists groups in an inventory
func (s *InventoryService) ListGroups(inventoryID uuid.UUID, limit, offset int) ([]models.AnsibleInventoryGroup, int64, error) {
	return s.inventoryRepo.ListGroupsByInventory(inventoryID, limit, offset)
}

// UpdateGroup updates a group
func (s *InventoryService) UpdateGroup(id uuid.UUID, name, description *string, variables *models.InventoryVariables, parentID *uuid.UUID, clearParent bool) (*models.AnsibleInventoryGroup, error) {
	group, err := s.inventoryRepo.GetGroupByID(id)
	if err != nil {
		return nil, fmt.Errorf("group not found: %w", err)
	}

	if name != nil {
		group.Name = *name
	}
	if description != nil {
		group.Description = *description
	}
	if variables != nil {
		group.Variables = *variables
	}
	if clearParent {
		group.ParentID = nil
	} else if parentID != nil {
		group.ParentID = parentID
	}

	if err := s.inventoryRepo.UpdateGroup(group); err != nil {
		return nil, fmt.Errorf("failed to update group: %w", err)
	}

	return group, nil
}

// DeleteGroup deletes a group
func (s *InventoryService) DeleteGroup(id uuid.UUID) error {
	return s.inventoryRepo.DeleteGroup(id)
}

// AddHostToGroup adds a host to a group
func (s *InventoryService) AddHostToGroup(hostID, groupID uuid.UUID) error {
	return s.inventoryRepo.AddHostToGroup(hostID, groupID)
}

// RemoveHostFromGroup removes a host from a group
func (s *InventoryService) RemoveHostFromGroup(hostID, groupID uuid.UUID) error {
	return s.inventoryRepo.RemoveHostFromGroup(hostID, groupID)
}

// GenerateInventoryINI generates an Ansible-compatible INI inventory file content
func (s *InventoryService) GenerateInventoryINI(inventoryID uuid.UUID) (string, error) {
	inventory, err := s.inventoryRepo.GetByID(inventoryID)
	if err != nil {
		return "", fmt.Errorf("inventory not found: %w", err)
	}

	var builder strings.Builder

	// Write global variables
	if len(inventory.Variables) > 0 {
		builder.WriteString("[all:vars]\n")
		for key, value := range inventory.Variables {
			fmt.Fprintf(&builder, "%s=%v\n", key, value)
		}
		builder.WriteString("\n")
	}

	// Get all hosts
	hosts, _, err := s.inventoryRepo.ListHostsByInventory(inventoryID, 10000, 0)
	if err != nil {
		return "", fmt.Errorf("failed to list hosts: %w", err)
	}

	// Get all groups
	groups, _, err := s.inventoryRepo.ListGroupsByInventory(inventoryID, 10000, 0)
	if err != nil {
		return "", fmt.Errorf("failed to list groups: %w", err)
	}

	// Create a map of host IDs to groups
	hostGroups := make(map[uuid.UUID][]string)
	for _, group := range groups {
		for _, host := range group.Hosts {
			hostGroups[host.ID] = append(hostGroups[host.ID], group.Name)
		}
	}

	// Write ungrouped hosts first
	ungroupedHosts := []models.AnsibleInventoryHost{}
	for _, host := range hosts {
		if len(hostGroups[host.ID]) == 0 && host.Enabled {
			ungroupedHosts = append(ungroupedHosts, host)
		}
	}

	if len(ungroupedHosts) > 0 {
		builder.WriteString("[ungrouped]\n")
		for _, host := range ungroupedHosts {
			builder.WriteString(s.formatHostLine(host))
		}
		builder.WriteString("\n")
	}

	// Write each group
	for _, group := range groups {
		fmt.Fprintf(&builder, "[%s]\n", group.Name)
		for _, host := range group.Hosts {
			if host.Enabled {
				builder.WriteString(s.formatHostLine(host))
			}
		}
		builder.WriteString("\n")

		// Write group variables
		if len(group.Variables) > 0 {
			fmt.Fprintf(&builder, "[%s:vars]\n", group.Name)
			for key, value := range group.Variables {
				fmt.Fprintf(&builder, "%s=%v\n", key, value)
			}
			builder.WriteString("\n")
		}

		// Write children (nested groups)
		if len(group.Children) > 0 {
			fmt.Fprintf(&builder, "[%s:children]\n", group.Name)
			for _, child := range group.Children {
				fmt.Fprintf(&builder, "%s\n", child.Name)
			}
			builder.WriteString("\n")
		}
	}

	return builder.String(), nil
}

// formatHostLine formats a host line for INI inventory
func (s *InventoryService) formatHostLine(host models.AnsibleInventoryHost) string {
	var parts []string

	// Use hostname or name
	hostIdentifier := host.Name
	if host.Hostname != "" {
		parts = append(parts, fmt.Sprintf("ansible_host=%s", host.Hostname))
	}

	// Add port if not default
	if host.Port != 22 {
		parts = append(parts, fmt.Sprintf("ansible_port=%d", host.Port))
	}

	// Add host variables
	for key, value := range host.Variables {
		parts = append(parts, fmt.Sprintf("%s=%v", key, value))
	}

	if len(parts) > 0 {
		return fmt.Sprintf("%s %s\n", hostIdentifier, strings.Join(parts, " "))
	}
	return fmt.Sprintf("%s\n", hostIdentifier)
}

// GenerateInventoryJSON generates an Ansible-compatible JSON inventory
// Uses modern Ansible 2.20+ format with host vars embedded in hosts dict
func (s *InventoryService) GenerateInventoryJSON(inventoryID uuid.UUID) (string, error) {
	inventory, err := s.inventoryRepo.GetByID(inventoryID)
	if err != nil {
		return "", fmt.Errorf("inventory not found: %w", err)
	}

	// Get all hosts
	hosts, _, err := s.inventoryRepo.ListHostsByInventory(inventoryID, 10000, 0)
	if err != nil {
		return "", fmt.Errorf("failed to list hosts: %w", err)
	}

	// Get all groups
	groups, _, err := s.inventoryRepo.ListGroupsByInventory(inventoryID, 10000, 0)
	if err != nil {
		return "", fmt.Errorf("failed to list groups: %w", err)
	}

	// Build the inventory structure
	// Modern Ansible JSON inventory format: host vars go directly in the hosts dict
	inventoryData := make(map[string]interface{})

	// Track hosts in groups
	hostsInGroups := make(map[uuid.UUID]bool)

	// Add groups
	for _, group := range groups {
		// Ansible JSON inventory: hosts is a dict with hostname -> hostvars (or null)
		hostDict := make(map[string]interface{})
		for _, host := range group.Hosts {
			if host.Enabled {
				hostsInGroups[host.ID] = true

				// Build host variables - embed directly in hosts dict
				hostVars := make(map[string]interface{})
				if host.Hostname != "" {
					hostVars["ansible_host"] = host.Hostname
				}
				if host.Port != 22 {
					hostVars["ansible_port"] = host.Port
				}
				for k, v := range host.Variables {
					hostVars[k] = v
				}

				// If host has vars, use the vars dict; otherwise use null
				if len(hostVars) > 0 {
					hostDict[host.Name] = hostVars
				} else {
					hostDict[host.Name] = nil
				}
			}
		}

		groupData := map[string]interface{}{
			"hosts": hostDict,
		}

		// Add group variables
		if len(group.Variables) > 0 {
			groupData["vars"] = group.Variables
		}

		// Add children as dict (Ansible requires dict, not list)
		if len(group.Children) > 0 {
			childrenDict := make(map[string]interface{})
			for _, child := range group.Children {
				childrenDict[child.Name] = nil
			}
			groupData["children"] = childrenDict
		}

		inventoryData[group.Name] = groupData
	}

	// Add ungrouped hosts
	ungroupedHosts := make(map[string]interface{})
	for _, host := range hosts {
		if !hostsInGroups[host.ID] && host.Enabled {
			// Build host variables - embed directly in hosts dict
			hostVars := make(map[string]interface{})
			if host.Hostname != "" {
				hostVars["ansible_host"] = host.Hostname
			}
			if host.Port != 22 {
				hostVars["ansible_port"] = host.Port
			}
			for k, v := range host.Variables {
				hostVars[k] = v
			}

			// If host has vars, use the vars dict; otherwise use null
			if len(hostVars) > 0 {
				ungroupedHosts[host.Name] = hostVars
			} else {
				ungroupedHosts[host.Name] = nil
			}
		}
	}

	if len(ungroupedHosts) > 0 {
		inventoryData["ungrouped"] = map[string]interface{}{
			"hosts": ungroupedHosts,
		}
	}

	// Add all group with global variables
	// children must be a dict, not a list
	childrenDict := make(map[string]interface{})
	if len(ungroupedHosts) > 0 {
		childrenDict["ungrouped"] = nil
	}
	for _, group := range groups {
		if group.ParentID == nil {
			childrenDict[group.Name] = nil
		}
	}
	allGroup := map[string]interface{}{
		"children": childrenDict,
	}
	if len(inventory.Variables) > 0 {
		allGroup["vars"] = inventory.Variables
	}
	inventoryData["all"] = allGroup

	jsonBytes, err := json.MarshalIndent(inventoryData, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal inventory: %w", err)
	}

	return string(jsonBytes), nil
}
