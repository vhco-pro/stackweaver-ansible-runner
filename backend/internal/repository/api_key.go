// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package repository

import (
	"time"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"gorm.io/gorm"
)

type APIKeyRepository struct {
	db *gorm.DB
}

func NewAPIKeyRepository(db *gorm.DB) *APIKeyRepository {
	return &APIKeyRepository{db: db}
}

func (r *APIKeyRepository) Create(apiKey *models.APIKey) error {
	return r.db.Create(apiKey).Error
}

func (r *APIKeyRepository) GetByID(id uuid.UUID) (*models.APIKey, error) {
	var apiKey models.APIKey
	err := r.db.Preload("User").First(&apiKey, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &apiKey, nil
}

func (r *APIKeyRepository) GetByUserID(userID uuid.UUID) ([]*models.APIKey, error) {
	var apiKeys []*models.APIKey
	err := r.db.Where("user_id = ?", userID).Order("created_at DESC").Find(&apiKeys).Error
	if err != nil {
		return nil, err
	}
	return apiKeys, nil
}

func (r *APIKeyRepository) GetByKeyHash(keyHash string) (*models.APIKey, error) {
	var apiKey models.APIKey
	err := r.db.Preload("User").Where("key_hash = ?", keyHash).First(&apiKey).Error
	if err != nil {
		return nil, err
	}
	return &apiKey, nil
}

func (r *APIKeyRepository) Update(apiKey *models.APIKey) error {
	return r.db.Save(apiKey).Error
}

func (r *APIKeyRepository) Delete(id uuid.UUID) error {
	return r.db.Delete(&models.APIKey{}, "id = ?", id).Error
}

func (r *APIKeyRepository) UpdateLastUsed(id uuid.UUID) error {
	now := time.Now()
	return r.db.Model(&models.APIKey{}).Where("id = ?", id).Update("last_used_at", now).Error
}

// GetAllKeys gets all API keys (for verification - use with caution)
// In production, you'd want to add filtering by prefix for efficiency
func (r *APIKeyRepository) GetAllKeys() ([]*models.APIKey, error) {
	var apiKeys []*models.APIKey
	err := r.db.Find(&apiKeys).Error
	if err != nil {
		return nil, err
	}
	return apiKeys, nil
}

// GetByPrefix gets API keys by prefix for faster lookup
func (r *APIKeyRepository) GetByPrefix(prefix string) ([]*models.APIKey, error) {
	var apiKeys []*models.APIKey
	err := r.db.Where("key_prefix = ?", prefix).Find(&apiKeys).Error
	if err != nil {
		return nil, err
	}
	return apiKeys, nil
}
