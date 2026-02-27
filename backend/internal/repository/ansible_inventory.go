// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type AnsibleInventoryRepository struct {
	db *gorm.DB
}

func NewAnsibleInventoryRepository(db *gorm.DB) *AnsibleInventoryRepository {
	return &AnsibleInventoryRepository{db: db}
}

func (r *AnsibleInventoryRepository) Create(inventory *models.AnsibleInventory) error {
	return r.db.Create(inventory).Error
}

func (r *AnsibleInventoryRepository) GetByID(id uuid.UUID) (*models.AnsibleInventory, error) {
	var inventory models.AnsibleInventory
	err := r.db.Preload("Hosts").Preload("Groups").Preload("Organization").Preload("Project").
		First(&inventory, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &inventory, nil
}

func (r *AnsibleInventoryRepository) GetByOrganizationAndName(orgID uuid.UUID, name string) (*models.AnsibleInventory, error) {
	var inventory models.AnsibleInventory
	err := r.db.First(&inventory, "organization_id = ? AND name = ?", orgID, name).Error
	if err != nil {
		return nil, err
	}
	return &inventory, nil
}

func (r *AnsibleInventoryRepository) ListByOrganization(orgID uuid.UUID, limit, offset int) ([]models.AnsibleInventory, int64, error) {
	var inventories []models.AnsibleInventory
	var total int64

	if err := r.db.Model(&models.AnsibleInventory{}).Where("organization_id = ?", orgID).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("organization_id = ?", orgID).
		Order("name ASC").
		Limit(limit).
		Offset(offset).
		Find(&inventories).Error
	return inventories, total, err
}

func (r *AnsibleInventoryRepository) ListByProject(projectID uuid.UUID, limit, offset int) ([]models.AnsibleInventory, int64, error) {
	var inventories []models.AnsibleInventory
	var total int64

	if err := r.db.Model(&models.AnsibleInventory{}).Where("project_id = ?", projectID).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("project_id = ?", projectID).
		Order("name ASC").
		Limit(limit).
		Offset(offset).
		Find(&inventories).Error
	return inventories, total, err
}

func (r *AnsibleInventoryRepository) Update(inventory *models.AnsibleInventory) error {
	return r.db.Save(inventory).Error
}

// FindByVCSRepositoryAndBranch finds VCS inventories by repository and branch
// Used for webhook-triggered syncs
func (r *AnsibleInventoryRepository) FindByVCSRepositoryAndBranch(repository string, branch string) ([]models.AnsibleInventory, error) {
	var inventories []models.AnsibleInventory
	err := r.db.Where("vcs_repository = ? AND (vcs_branch = ? OR vcs_branch = '') AND type = ?", repository, branch, models.InventoryTypeVCS).
		Preload("Organization").
		Find(&inventories).Error
	return inventories, err
}

// CountJobTemplatesByInventory counts job templates using this inventory
func (r *AnsibleInventoryRepository) CountJobTemplatesByInventory(id uuid.UUID) (int64, error) {
	var count int64
	err := r.db.Model(&models.AnsibleJobTemplate{}).Where("inventory_id = ?", id).Count(&count).Error
	return count, err
}

// CountJobsByInventory counts jobs using this inventory
func (r *AnsibleInventoryRepository) CountJobsByInventory(id uuid.UUID) (int64, error) {
	var count int64
	err := r.db.Model(&models.AnsibleJob{}).Where("inventory_id = ?", id).Count(&count).Error
	return count, err
}

// CountInventorySourcesByInventory counts inventory sources using this inventory
func (r *AnsibleInventoryRepository) CountInventorySourcesByInventory(id uuid.UUID) (int64, error) {
	var count int64
	err := r.db.Model(&models.AnsibleInventorySource{}).Where("inventory_id = ?", id).Count(&count).Error
	return count, err
}

func (r *AnsibleInventoryRepository) Delete(id uuid.UUID) error {
	// Use a transaction to ensure all deletions succeed or none do
	return r.db.Transaction(func(tx *gorm.DB) error {
		// Delete host-group associations (many-to-many relationships) first
		if err := tx.Exec("DELETE FROM ansible_inventory_host_groups WHERE ansible_inventory_host_id IN (SELECT id FROM ansible_inventory_hosts WHERE inventory_id = ?)", id).Error; err != nil {
			return fmt.Errorf("failed to delete host-group associations: %w", err)
		}

		// Delete all hosts in this inventory
		if err := tx.Delete(&models.AnsibleInventoryHost{}, "inventory_id = ?", id).Error; err != nil {
			return fmt.Errorf("failed to delete hosts: %w", err)
		}

		// Delete child groups first (groups with parent_id pointing to groups in this inventory)
		if err := tx.Exec("DELETE FROM ansible_inventory_groups WHERE parent_id IN (SELECT id FROM ansible_inventory_groups WHERE inventory_id = ?)", id).Error; err != nil {
			return fmt.Errorf("failed to delete child groups: %w", err)
		}

		// Delete top-level groups
		if err := tx.Delete(&models.AnsibleInventoryGroup{}, "inventory_id = ?", id).Error; err != nil {
			return fmt.Errorf("failed to delete groups: %w", err)
		}

		// Finally, delete the inventory itself
		if err := tx.Delete(&models.AnsibleInventory{}, "id = ?", id).Error; err != nil {
			return fmt.Errorf("failed to delete inventory: %w", err)
		}

		return nil
	})
}

// Host operations

func (r *AnsibleInventoryRepository) CreateHost(host *models.AnsibleInventoryHost) error {
	return r.db.Create(host).Error
}

func (r *AnsibleInventoryRepository) GetHostByID(id uuid.UUID) (*models.AnsibleInventoryHost, error) {
	var host models.AnsibleInventoryHost
	err := r.db.Preload("Groups").First(&host, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &host, nil
}

func (r *AnsibleInventoryRepository) GetHostByInventoryAndName(inventoryID uuid.UUID, name string) (*models.AnsibleInventoryHost, error) {
	var host models.AnsibleInventoryHost
	err := r.db.First(&host, "inventory_id = ? AND name = ?", inventoryID, name).Error
	if err != nil {
		return nil, err
	}
	return &host, nil
}

func (r *AnsibleInventoryRepository) ListHostsByInventory(inventoryID uuid.UUID, limit, offset int) ([]models.AnsibleInventoryHost, int64, error) {
	var hosts []models.AnsibleInventoryHost
	var total int64

	if err := r.db.Model(&models.AnsibleInventoryHost{}).Where("inventory_id = ?", inventoryID).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("inventory_id = ?", inventoryID).
		Preload("Groups").
		Order("name ASC").
		Limit(limit).
		Offset(offset).
		Find(&hosts).Error
	return hosts, total, err
}

func (r *AnsibleInventoryRepository) UpdateHost(host *models.AnsibleInventoryHost) error {
	return r.db.Save(host).Error
}

func (r *AnsibleInventoryRepository) DeleteHost(id uuid.UUID) error {
	return r.db.Delete(&models.AnsibleInventoryHost{}, "id = ?", id).Error
}

// Group operations

func (r *AnsibleInventoryRepository) CreateGroup(group *models.AnsibleInventoryGroup) error {
	return r.db.Create(group).Error
}

func (r *AnsibleInventoryRepository) GetGroupByID(id uuid.UUID) (*models.AnsibleInventoryGroup, error) {
	var group models.AnsibleInventoryGroup
	err := r.db.Preload("Hosts").Preload("Children").First(&group, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &group, nil
}

func (r *AnsibleInventoryRepository) GetGroupByInventoryAndName(inventoryID uuid.UUID, name string) (*models.AnsibleInventoryGroup, error) {
	var group models.AnsibleInventoryGroup
	err := r.db.First(&group, "inventory_id = ? AND name = ?", inventoryID, name).Error
	if err != nil {
		return nil, err
	}
	return &group, nil
}

func (r *AnsibleInventoryRepository) ListGroupsByInventory(inventoryID uuid.UUID, limit, offset int) ([]models.AnsibleInventoryGroup, int64, error) {
	var groups []models.AnsibleInventoryGroup
	var total int64

	if err := r.db.Model(&models.AnsibleInventoryGroup{}).Where("inventory_id = ?", inventoryID).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	err := r.db.Where("inventory_id = ?", inventoryID).
		Preload("Hosts").
		Preload("Children").
		Order("name ASC").
		Limit(limit).
		Offset(offset).
		Find(&groups).Error
	return groups, total, err
}

func (r *AnsibleInventoryRepository) UpdateGroup(group *models.AnsibleInventoryGroup) error {
	return r.db.Save(group).Error
}

func (r *AnsibleInventoryRepository) DeleteGroup(id uuid.UUID) error {
	return r.db.Delete(&models.AnsibleInventoryGroup{}, "id = ?", id).Error
}

// Host-Group associations

func (r *AnsibleInventoryRepository) AddHostToGroup(hostID, groupID uuid.UUID) error {
	host, err := r.GetHostByID(hostID)
	if err != nil {
		return err
	}
	group, err := r.GetGroupByID(groupID)
	if err != nil {
		return err
	}
	return r.db.Model(host).Association("Groups").Append(group)
}

func (r *AnsibleInventoryRepository) RemoveHostFromGroup(hostID, groupID uuid.UUID) error {
	host, err := r.GetHostByID(hostID)
	if err != nil {
		return err
	}
	group, err := r.GetGroupByID(groupID)
	if err != nil {
		return err
	}
	return r.db.Model(host).Association("Groups").Delete(group)
}
