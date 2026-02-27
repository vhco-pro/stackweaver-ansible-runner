// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type GPGKeyRepository struct {
	db *gorm.DB
}

func NewGPGKeyRepository(db *gorm.DB) *GPGKeyRepository {
	return &GPGKeyRepository{db: db}
}

// Create creates a new GPG key
func (r *GPGKeyRepository) Create(key *models.GPGKey) error {
	return r.db.Create(key).Error
}

// GetByID retrieves a GPG key by ID
func (r *GPGKeyRepository) GetByID(id uuid.UUID) (*models.GPGKey, error) {
	var key models.GPGKey
	err := r.db.Where("id = ?", id).First(&key).Error
	if err != nil {
		return nil, err
	}
	return &key, nil
}

// GetByOrganization retrieves all GPG keys for an organization
func (r *GPGKeyRepository) GetByOrganization(orgID uuid.UUID) ([]models.GPGKey, error) {
	var keys []models.GPGKey
	err := r.db.Where("organization_id = ?", orgID).Order("created_at DESC").Find(&keys).Error
	return keys, err
}

// GetByKeyID retrieves a GPG key by key ID (short key ID)
func (r *GPGKeyRepository) GetByKeyID(orgID uuid.UUID, keyID string) (*models.GPGKey, error) {
	var key models.GPGKey
	err := r.db.Where("organization_id = ? AND key_id = ?", orgID, keyID).First(&key).Error
	if err != nil {
		return nil, err
	}
	return &key, nil
}

// Delete deletes a GPG key
func (r *GPGKeyRepository) Delete(id uuid.UUID) error {
	return r.db.Delete(&models.GPGKey{}, "id = ?", id).Error
}
