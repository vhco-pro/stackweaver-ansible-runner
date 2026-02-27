// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package registry

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
)

type ModuleService struct {
	moduleRepo         *repository.ModuleRepository
	moduleVersionRepo  *repository.ModuleVersionRepository
	moduleDownloadRepo *repository.ModuleDownloadRepository
	orgRepo            *repository.OrganizationRepository
	storage            StorageBackend
	storageBucket      string
}

func NewModuleService(
	moduleRepo *repository.ModuleRepository,
	moduleVersionRepo *repository.ModuleVersionRepository,
	moduleDownloadRepo *repository.ModuleDownloadRepository,
	orgRepo *repository.OrganizationRepository,
	storage StorageBackend,
	storageBucket string,
) *ModuleService {
	return &ModuleService{
		moduleRepo:         moduleRepo,
		moduleVersionRepo:  moduleVersionRepo,
		moduleDownloadRepo: moduleDownloadRepo,
		orgRepo:            orgRepo,
		storage:            storage,
		storageBucket:      storageBucket,
	}
}

// ListModules lists modules with optional filters
func (s *ModuleService) ListModules(namespace string, provider string, verified *bool, limit, offset int) ([]models.Module, int64, error) {
	var organizationID *uuid.UUID

	if namespace != "" {
		org, err := s.orgRepo.GetByName(namespace)
		if err != nil {
			return nil, 0, err
		}
		organizationID = &org.ID
	}

	return s.moduleRepo.List(organizationID, provider, verified, limit, offset)
}

// SearchModules searches modules by query string
func (s *ModuleService) SearchModules(query, namespace, provider string, verified *bool, limit, offset int) ([]models.Module, int64, error) {
	var organizationID *uuid.UUID

	if namespace != "" {
		org, err := s.orgRepo.GetByName(namespace)
		if err != nil {
			return nil, 0, err
		}
		organizationID = &org.ID
	}

	return s.moduleRepo.Search(query, organizationID, provider, verified, limit, offset)
}

// GetModule gets a module by organization name, module name, and provider
func (s *ModuleService) GetModule(namespace, name, provider string) (*models.Module, error) {
	org, err := s.orgRepo.GetByName(namespace)
	if err != nil {
		return nil, err
	}

	return s.moduleRepo.GetByOrganizationAndName(org.ID, name, provider)
}

// GetModuleVersions gets all versions for a module
func (s *ModuleService) GetModuleVersions(namespace, name, provider string) ([]models.ModuleVersion, error) {
	module, err := s.GetModule(namespace, name, provider)
	if err != nil {
		return nil, err
	}

	return s.moduleVersionRepo.ListByModule(module.ID)
}

// GetLatestVersion gets the latest version for a module
func (s *ModuleService) GetLatestVersion(namespace, name, provider string) (*models.ModuleVersion, error) {
	module, err := s.GetModule(namespace, name, provider)
	if err != nil {
		return nil, err
	}

	return s.moduleVersionRepo.GetLatest(module.ID)
}

// GetModuleVersion gets a specific version of a module
func (s *ModuleService) GetModuleVersion(namespace, name, provider, version string) (*models.ModuleVersion, error) {
	module, err := s.GetModule(namespace, name, provider)
	if err != nil {
		return nil, err
	}

	return s.moduleVersionRepo.GetByModuleAndVersion(module.ID, version)
}

// GetDownloadURL generates a presigned URL for downloading a module version
func (s *ModuleService) GetDownloadURL(ctx context.Context, namespace, name, provider, version string) (string, error) {
	moduleVersion, err := s.GetModuleVersion(namespace, name, provider, version)
	if err != nil {
		return "", err
	}

	if moduleVersion.TarballPath == "" {
		return "", ErrModuleVersionNotAvailable
	}

	// Generate presigned URL (15 minutes expiry as per Terraform Registry spec)
	url, err := s.storage.PresignGetObject(ctx, s.storageBucket, moduleVersion.TarballPath, 15*time.Minute)
	if err != nil {
		return "", err
	}

	return url, nil
}

// TrackDownload records a download event
func (s *ModuleService) TrackDownload(moduleVersionID uuid.UUID, ipAddress, userAgent string) error {
	download := &models.ModuleDownload{
		ModuleVersionID: moduleVersionID,
		IPAddress:       ipAddress,
		UserAgent:       userAgent,
		DownloadedAt:    time.Now(),
	}

	if err := s.moduleDownloadRepo.Create(download); err != nil {
		return err
	}

	// Increment download counter
	return s.moduleVersionRepo.IncrementDownloads(moduleVersionID)
}

// GetDownloadStats gets download statistics for a module version
func (s *ModuleService) GetDownloadStats(moduleVersionID uuid.UUID) (map[string]interface{}, error) {
	return s.moduleDownloadRepo.GetDownloadStats(moduleVersionID)
}

var (
	ErrModuleVersionNotAvailable = &ModuleError{Message: "Module version not available for download"}
	ErrModuleNotFound            = &ModuleError{Message: "Module not found"}
)

type ModuleError struct {
	Message string
}

func (e *ModuleError) Error() string {
	return e.Message
}
