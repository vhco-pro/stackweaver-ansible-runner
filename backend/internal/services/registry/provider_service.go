// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package registry

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
)

type ProviderService struct {
	providerRepo         *repository.ProviderRepository
	providerVersionRepo  *repository.ProviderVersionRepository
	providerPlatformRepo *repository.ProviderPlatformRepository
	providerDownloadRepo *repository.ProviderDownloadRepository
	orgRepo              *repository.OrganizationRepository
	storage              StorageBackend
	storageBucket        string
}

func NewProviderService(
	providerRepo *repository.ProviderRepository,
	providerVersionRepo *repository.ProviderVersionRepository,
	providerPlatformRepo *repository.ProviderPlatformRepository,
	providerDownloadRepo *repository.ProviderDownloadRepository,
	orgRepo *repository.OrganizationRepository,
	storage StorageBackend,
	storageBucket string,
) *ProviderService {
	return &ProviderService{
		providerRepo:         providerRepo,
		providerVersionRepo:  providerVersionRepo,
		providerPlatformRepo: providerPlatformRepo,
		providerDownloadRepo: providerDownloadRepo,
		orgRepo:              orgRepo,
		storage:              storage,
		storageBucket:        storageBucket,
	}
}

// ListProviders lists providers with optional filters
func (s *ProviderService) ListProviders(namespace string, verified *bool, limit, offset int) ([]models.Provider, int64, error) {
	var organizationID *uuid.UUID

	if namespace != "" {
		org, err := s.orgRepo.GetByName(namespace)
		if err != nil {
			return nil, 0, err
		}
		organizationID = &org.ID
	}

	return s.providerRepo.List(organizationID, verified, limit, offset)
}

// SearchProviders searches providers by keyword
func (s *ProviderService) SearchProviders(query string, namespace string, verified *bool, limit, offset int) ([]models.Provider, int64, error) {
	var organizationID *uuid.UUID

	if namespace != "" {
		org, err := s.orgRepo.GetByName(namespace)
		if err != nil {
			return nil, 0, err
		}
		organizationID = &org.ID
	}

	return s.providerRepo.Search(query, organizationID, verified, limit, offset)
}

// GetProvider gets a provider by namespace and name
func (s *ProviderService) GetProvider(namespace, name string) (*models.Provider, error) {
	org, err := s.orgRepo.GetByName(namespace)
	if err != nil {
		return nil, err
	}

	return s.providerRepo.GetByOrganizationAndName(org.ID, name)
}

// GetProviderVersions lists all versions for a provider
func (s *ProviderService) GetProviderVersions(namespace, name string) ([]models.ProviderVersion, error) {
	provider, err := s.GetProvider(namespace, name)
	if err != nil {
		return nil, err
	}

	return s.providerVersionRepo.ListByProvider(provider.ID)
}

// GetLatestVersion gets the latest version of a provider
func (s *ProviderService) GetLatestVersion(namespace, name string) (*models.ProviderVersion, error) {
	provider, err := s.GetProvider(namespace, name)
	if err != nil {
		return nil, err
	}

	return s.providerVersionRepo.GetLatest(provider.ID)
}

// GetProviderVersion gets a specific version of a provider
func (s *ProviderService) GetProviderVersion(namespace, name, version string) (*models.ProviderVersion, error) {
	provider, err := s.GetProvider(namespace, name)
	if err != nil {
		return nil, err
	}

	return s.providerVersionRepo.GetByProviderAndVersion(provider.ID, version)
}

// GetDownloadURL generates a presigned URL for downloading a provider binary
func (s *ProviderService) GetDownloadURL(ctx context.Context, namespace, name, version, os, arch string) (string, error) {
	providerVersion, err := s.GetProviderVersion(namespace, name, version)
	if err != nil {
		return "", err
	}

	platform, err := s.providerPlatformRepo.GetByVersionAndPlatform(providerVersion.ID, os, arch)
	if err != nil {
		return "", ErrProviderPlatformNotFound
	}

	if platform.BinaryPath == "" {
		return "", ErrProviderBinaryNotAvailable
	}

	// Generate presigned URL (15 minutes expiry as per Terraform Registry spec)
	url, err := s.storage.PresignGetObject(ctx, s.storageBucket, platform.BinaryPath, 15*time.Minute)
	if err != nil {
		return "", err
	}

	return url, nil
}

// TrackDownload records a download event
func (s *ProviderService) TrackDownload(providerPlatformID uuid.UUID, ipAddress, userAgent string) error {
	download := &models.ProviderDownload{
		ProviderPlatformID: providerPlatformID,
		IPAddress:          ipAddress,
		UserAgent:          userAgent,
		DownloadedAt:       time.Now(),
	}

	if err := s.providerDownloadRepo.Create(download); err != nil {
		return err
	}

	// Get platform to increment version downloads
	platform, err := s.providerPlatformRepo.GetByID(providerPlatformID)
	if err == nil {
		_ = s.providerVersionRepo.IncrementDownloads(platform.ProviderVersionID)
	}

	return nil
}

// GetDownloadStats gets download statistics for a provider platform
func (s *ProviderService) GetDownloadStats(providerPlatformID uuid.UUID) (map[string]interface{}, error) {
	return s.providerDownloadRepo.GetDownloadStats(providerPlatformID)
}

var (
	ErrProviderPlatformNotFound   = &ProviderError{Message: "Provider platform not found"}
	ErrProviderBinaryNotAvailable = &ProviderError{Message: "Provider binary not available for download"}
	ErrProviderNotFound           = &ProviderError{Message: "Provider not found"}
)

type ProviderError struct {
	Message string
}

func (e *ProviderError) Error() string {
	return e.Message
}
