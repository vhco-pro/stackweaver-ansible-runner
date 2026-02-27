// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/registry"
	"github.com/michielvha/logger"
)

// RegistryProviderPublishingHandler handles provider publishing operations
type RegistryProviderPublishingHandler struct {
	providerRepo         *repository.ProviderRepository
	providerVersionRepo  *repository.ProviderVersionRepository
	providerPlatformRepo *repository.ProviderPlatformRepository
	orgRepo              *repository.OrganizationRepository
	gpgKeyRepo           *repository.GPGKeyRepository
	authService          *auth.Service
	storage              registry.StorageBackend
	storageBucket        string
	gpgService           *registry.GPGService
}

func NewRegistryProviderPublishingHandler(
	providerRepo *repository.ProviderRepository,
	providerVersionRepo *repository.ProviderVersionRepository,
	providerPlatformRepo *repository.ProviderPlatformRepository,
	orgRepo *repository.OrganizationRepository,
	gpgKeyRepo *repository.GPGKeyRepository,
	authService *auth.Service,
	storage registry.StorageBackend,
	storageBucket string,
) *RegistryProviderPublishingHandler {
	return &RegistryProviderPublishingHandler{
		providerRepo:         providerRepo,
		providerVersionRepo:  providerVersionRepo,
		providerPlatformRepo: providerPlatformRepo,
		orgRepo:              orgRepo,
		gpgKeyRepo:           gpgKeyRepo,
		authService:          authService,
		storage:              storage,
		storageBucket:        storageBucket,
		gpgService:           registry.NewGPGService(),
	}
}

// CreateProvider handles POST /api/v2/organizations/:name/registry/providers
func (h *RegistryProviderPublishingHandler) CreateProvider(c *gin.Context) {
	orgName := c.Param("name")

	// Get authenticated user
	user, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}},
		})
		return
	}

	// Get organization
	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}},
		})
		return
	}

	// Parse request body
	var req struct {
		Name        string `json:"name" binding:"required"`
		Description string `json:"description"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}},
		})
		return
	}

	// Check if provider already exists
	existing, err := h.providerRepo.GetByOrganizationAndName(org.ID, req.Name)
	if err == nil && existing != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": fmt.Sprintf("Provider %s already exists", req.Name)}},
		})
		return
	}

	// Create provider
	provider := &models.Provider{
		OrganizationID: org.ID,
		Name:           req.Name,
		Description:    req.Description,
		PublishedBy:    user.ID,
	}

	if err := h.providerRepo.Create(provider); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}},
		})
		return
	}

	// Format response (TFE-compatible)
	c.JSON(http.StatusCreated, gin.H{
		"data": gin.H{
			"id":   provider.ID.String(),
			"type": "registry-providers",
			"attributes": gin.H{
				"name":        provider.Name,
				"description": provider.Description,
			},
		},
	})
}

// ListProviders handles GET /api/v2/organizations/:name/registry/providers
func (h *RegistryProviderPublishingHandler) ListProviders(c *gin.Context) {
	orgName := c.Param("name")

	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}},
		})
		return
	}

	providers, _, err := h.providerRepo.List(&org.ID, nil, 100, 0)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": err.Error()}},
		})
		return
	}

	// Format response
	data := make([]gin.H, len(providers))
	for i, p := range providers {
		data[i] = gin.H{
			"id":   p.ID.String(),
			"type": "registry-providers",
			"attributes": gin.H{
				"name":        p.Name,
				"description": p.Description,
			},
		}
	}

	c.JSON(http.StatusOK, gin.H{"data": data})
}

// GetProvider handles GET /api/v2/organizations/:name/registry/providers/:name
func (h *RegistryProviderPublishingHandler) GetProvider(c *gin.Context) {
	orgName := c.Param("name")
	providerName := c.Param("provider_name")

	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}},
		})
		return
	}

	provider, err := h.providerRepo.GetByOrganizationAndName(org.ID, providerName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Provider not found"}},
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"id":   provider.ID.String(),
			"type": "registry-providers",
			"attributes": gin.H{
				"name":        provider.Name,
				"description": provider.Description,
			},
		},
	})
}

// PublishProviderPlatform handles POST /api/v2/organizations/:name/registry/providers/:name/versions/:version/platforms
func (h *RegistryProviderPublishingHandler) PublishProviderPlatform(c *gin.Context) {
	orgName := c.Param("name")
	providerName := c.Param("provider_name")
	version := c.Param("version")

	// Get authenticated user
	_, err := h.authService.GetUserFromContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "Authentication required"}},
		})
		return
	}

	// Get organization
	org, err := h.orgRepo.GetByName(orgName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Organization not found"}},
		})
		return
	}

	// Get provider
	provider, err := h.providerRepo.GetByOrganizationAndName(org.ID, providerName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{
			"errors": []gin.H{{"status": "404", "title": "Not Found", "detail": "Provider not found"}},
		})
		return
	}

	// Validate version
	normalizedVersion := registry.NormalizeVersion(version)
	if err := registry.ValidateSemanticVersion(normalizedVersion); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}},
		})
		return
	}

	// Get or create provider version
	providerVersion, err := h.providerVersionRepo.GetByProviderAndVersion(provider.ID, normalizedVersion)
	if err != nil {
		// Create new version
		providerVersion = &models.ProviderVersion{
			ProviderID:  provider.ID,
			Version:     normalizedVersion,
			PublishedAt: time.Now(), // Will be set by BeforeCreate if zero
		}
		if err := h.providerVersionRepo.Create(providerVersion); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": err.Error()}},
			})
			return
		}
	}

	// Get platform info from form
	os := c.PostForm("os")
	arch := c.PostForm("arch")
	if os == "" || arch == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "os and arch are required"}},
		})
		return
	}

	// Check if platform already exists
	existingPlatform, err := h.providerPlatformRepo.GetByVersionAndPlatform(providerVersion.ID, os, arch)
	if err == nil && existingPlatform != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": fmt.Sprintf("Platform %s/%s already exists for this version", os, arch)}},
		})
		return
	}

	// Get binary file
	file, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "file is required"}},
		})
		return
	}

	// Open uploaded file
	src, err := file.Open()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": err.Error()}},
		})
		return
	}
	defer func() {
		if err := src.Close(); err != nil {
			logger.Warnf("Failed to close source file: %v", err)
		}
	}()

	// Calculate SHA256 checksum
	hasher := sha256.New()
	if _, err := io.Copy(hasher, src); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to calculate checksum"}},
		})
		return
	}
	shasum := hex.EncodeToString(hasher.Sum(nil))

	// Reset file reader
	if _, err := src.Seek(0, 0); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to reset file reader"}},
		})
		return
	}

	// Upload to storage
	storagePath := fmt.Sprintf("providers/%s/%s/%s/%s_%s/%s",
		org.Name, provider.Name, normalizedVersion, os, arch, file.Filename)

	if err := h.storage.PutObject(c.Request.Context(), h.storageBucket, storagePath, src, file.Size); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to upload to storage"}},
		})
		return
	}

	// GPG signing (optional - if GPG key ID is provided)
	var gpgSignaturePath string
	var gpgKeyID string
	gpgKeyIDParam := c.PostForm("gpg_key_id")
	if gpgKeyIDParam != "" {
		// Get GPG key
		gpgKey, err := h.gpgKeyRepo.GetByKeyID(org.ID, gpgKeyIDParam)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": fmt.Sprintf("GPG key %s not found", gpgKeyIDParam)}},
			})
			return
		}

		// Reset file reader again for signing
		if _, err := src.Seek(0, 0); err != nil {
			logger.Warnf("Failed to reset file reader for GPG signing: %v", err)
		}

		// Sign binary
		signature, err := h.gpgService.SignBinary(gpgKey.KeyID, src)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": fmt.Sprintf("Failed to sign binary: %v", err)}},
			})
			return
		}

		// Upload signature to storage
		signaturePath := fmt.Sprintf("providers/%s/%s/%s/%s_%s/%s.sig",
			org.Name, provider.Name, normalizedVersion, os, arch, file.Filename)

		signatureReader := bytes.NewReader(signature)
		if err := h.storage.PutObject(c.Request.Context(), h.storageBucket, signaturePath, signatureReader, int64(len(signature))); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to upload signature to storage"}},
			})
			return
		}

		gpgSignaturePath = signaturePath
		gpgKeyID = gpgKey.KeyID
	}

	// Create platform record
	platform := &models.ProviderPlatform{
		ProviderVersionID: providerVersion.ID,
		OS:                os,
		Arch:              arch,
		Filename:          file.Filename,
		Shasum:            shasum,
		BinaryPath:        storagePath,
		BinarySize:        file.Size,
		GPGSignaturePath:  gpgSignaturePath,
		GPGKeyID:          gpgKeyID,
	}

	if err := h.providerPlatformRepo.Create(platform); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": err.Error()}},
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"data": gin.H{
			"id":   platform.ID.String(),
			"type": "registry-provider-platforms",
			"attributes": gin.H{
				"os":       platform.OS,
				"arch":     platform.Arch,
				"filename": platform.Filename,
				"shasum":   platform.Shasum,
				"signing_keys": func() gin.H {
					if platform.GPGKeyID != "" {
						// Get GPG key to include in response
						gpgKey, err := h.gpgKeyRepo.GetByKeyID(org.ID, platform.GPGKeyID)
						if err == nil && gpgKey != nil {
							return gin.H{
								"gpg_public_keys": []gin.H{
									{
										"key_id":      gpgKey.KeyID,
										"ascii_armor": gpgKey.ASCIIArmor,
									},
								},
							}
						}
					}
					return gin.H{"gpg_public_keys": []gin.H{}}
				}(),
			},
		},
	})
}
