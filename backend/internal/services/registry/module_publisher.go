// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package registry

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/michielvha/logger"
)

// ModulePublisher handles module publishing operations
type ModulePublisher struct {
	moduleRepo        *repository.ModuleRepository
	moduleVersionRepo *repository.ModuleVersionRepository
	orgRepo           *repository.OrganizationRepository
	vcsConnectionRepo *repository.VCSConnectionRepository
	parser            *ModuleParser
	storage           StorageBackend
	storageBucket     string
}

// NewModulePublisher creates a new module publisher
func NewModulePublisher(
	moduleRepo *repository.ModuleRepository,
	moduleVersionRepo *repository.ModuleVersionRepository,
	orgRepo *repository.OrganizationRepository,
	vcsConnectionRepo *repository.VCSConnectionRepository,
	storage StorageBackend,
	storageBucket string,
) *ModulePublisher {
	return &ModulePublisher{
		moduleRepo:        moduleRepo,
		moduleVersionRepo: moduleVersionRepo,
		orgRepo:           orgRepo,
		vcsConnectionRepo: vcsConnectionRepo,
		parser:            NewModuleParser(),
		storage:           storage,
		storageBucket:     storageBucket,
	}
}

// CreateModule creates a new module (VCS-connected or standalone)
func (p *ModulePublisher) CreateModule(
	organizationID uuid.UUID,
	name, provider, description string,
	vcsConnectionID *uuid.UUID,
	vcsRepository string,
	autoPublishTags bool,
	publishedBy uuid.UUID,
) (*models.Module, error) {
	// Validate module name and provider
	if name == "" || provider == "" {
		return nil, fmt.Errorf("module name and provider are required")
	}

	// Check if module already exists
	existing, err := p.moduleRepo.GetByOrganizationAndName(organizationID, name, provider)
	if err == nil && existing != nil {
		return nil, fmt.Errorf("module %s/%s already exists", name, provider)
	}

	module := &models.Module{
		OrganizationID:  organizationID,
		Name:            name,
		Provider:        provider,
		Description:     description,
		VCSConnectionID: vcsConnectionID,
		VCSRepository:   vcsRepository,
		AutoPublishTags: autoPublishTags,
		PublishedBy:     publishedBy,
	}

	if vcsConnectionID != nil {
		vcsConn, err := p.vcsConnectionRepo.GetByID(*vcsConnectionID)
		if err != nil {
			return nil, fmt.Errorf("VCS connection not found: %w", err)
		}
		module.VCSConnection = vcsConn
		module.Source = fmt.Sprintf("https://github.com/%s", vcsRepository) // TODO: Support GitLab
	}

	if err := p.moduleRepo.Create(module); err != nil {
		return nil, fmt.Errorf("failed to create module: %w", err)
	}

	return module, nil
}

// PublishVersionFromTarball publishes a module version from an uploaded tarball
func (p *ModulePublisher) PublishVersionFromTarball(
	ctx context.Context,
	moduleID uuid.UUID,
	version string,
	tarballReader io.Reader,
	tarballSize int64,
) (*models.ModuleVersion, error) {
	// Validate version
	if err := ValidateSemanticVersion(version); err != nil {
		return nil, err
	}

	version = NormalizeVersion(version)

	// Get module
	module, err := p.moduleRepo.GetByID(moduleID)
	if err != nil {
		return nil, fmt.Errorf("module not found: %w", err)
	}

	// Check if version already exists
	if p.moduleVersionRepo.Exists(moduleID, version) {
		return nil, fmt.Errorf("version %s already exists for this module", version)
	}

	// Create temporary directory for extraction
	tempDir, err := os.MkdirTemp("", "module-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			logger.Warnf("Failed to remove temp directory %s: %v", tempDir, err)
		}
	}()

	// Extract tarball
	if err := p.extractTarball(tarballReader, tempDir); err != nil {
		return nil, fmt.Errorf("failed to extract tarball: %w", err)
	}

	// Parse metadata
	metadata, err := p.parser.ParseModule(tempDir)
	if err != nil {
		return nil, fmt.Errorf("failed to parse module: %w", err)
	}

	// Create tarball from extracted directory (to ensure clean format)
	tarballPath := filepath.Join(tempDir, "module.tar.gz")
	if err := p.createTarball(tempDir, tarballPath); err != nil {
		return nil, fmt.Errorf("failed to create tarball: %w", err)
	}

	// Upload to storage
	storagePath := fmt.Sprintf("modules/%s/%s/%s/%s.tar.gz",
		module.Organization.Name, module.Name, module.Provider, version)

	tarballFile, err := os.Open(tarballPath) //nolint:gosec // tarballPath is validated (in temp directory)
	if err != nil {
		return nil, fmt.Errorf("failed to open tarball: %w", err)
	}
	defer func() {
		if err := tarballFile.Close(); err != nil {
			logger.Warnf("Failed to close tarball file: %v", err)
		}
	}()

	fileInfo, err := tarballFile.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to get file info: %w", err)
	}

	if err := p.storage.PutObject(ctx, p.storageBucket, storagePath, tarballFile, fileInfo.Size()); err != nil {
		return nil, fmt.Errorf("failed to upload to storage: %w", err)
	}

	// Create module version
	metadataJSONB := models.JSONB(metadata.ConvertToJSONB())
	moduleVersion := &models.ModuleVersion{
		ModuleID:     moduleID,
		Version:      version,
		TarballPath:  storagePath,
		TarballSize:  fileInfo.Size(),
		Readme:       metadata.Readme,
		Inputs:       models.JSONB{"inputs": metadataJSONB["inputs"]},
		Outputs:      models.JSONB{"outputs": metadataJSONB["outputs"]},
		Dependencies: models.JSONB{"dependencies": metadataJSONB["dependencies"]},
		Resources:    models.JSONB{"resources": metadataJSONB["resources"]},
		Submodules:   models.JSONB{"submodules": metadataJSONB["submodules"]},
		PublishedAt:  time.Now(),
	}

	if err := p.moduleVersionRepo.Create(moduleVersion); err != nil {
		return nil, fmt.Errorf("failed to create module version: %w", err)
	}

	return moduleVersion, nil
}

// PublishVersionFromDirectory publishes a module version from a directory (used for Git tag publishing)
func (p *ModulePublisher) PublishVersionFromDirectory(
	ctx context.Context,
	moduleID uuid.UUID,
	version string,
	sourceDir string,
) (*models.ModuleVersion, error) {
	// Validate version
	if err := ValidateSemanticVersion(version); err != nil {
		return nil, err
	}

	version = NormalizeVersion(version)

	// Get module
	module, err := p.moduleRepo.GetByID(moduleID)
	if err != nil {
		return nil, fmt.Errorf("module not found: %w", err)
	}

	// Check if version already exists
	if p.moduleVersionRepo.Exists(moduleID, version) {
		return nil, fmt.Errorf("version %s already exists for this module", version)
	}

	// Parse metadata
	metadata, err := p.parser.ParseModule(sourceDir)
	if err != nil {
		return nil, fmt.Errorf("failed to parse module: %w", err)
	}

	// Create tarball
	tarballPath := filepath.Join(os.TempDir(), fmt.Sprintf("module-%s-%s.tar.gz", module.Name, version))
	defer func() {
		if err := os.Remove(tarballPath); err != nil {
			logger.Warnf("Failed to remove tarball %s: %v", tarballPath, err)
		}
	}()

	if err := p.createTarball(sourceDir, tarballPath); err != nil {
		return nil, fmt.Errorf("failed to create tarball: %w", err)
	}

	// Upload to storage
	storagePath := fmt.Sprintf("modules/%s/%s/%s/%s.tar.gz",
		module.Organization.Name, module.Name, module.Provider, version)

	// Security: Validate tarballPath is within temp directory
	cleanTarballPath := filepath.Clean(tarballPath)
	cleanTempDir := filepath.Clean(os.TempDir())
	if !strings.HasPrefix(cleanTarballPath, cleanTempDir+string(filepath.Separator)) && cleanTarballPath != cleanTempDir {
		return nil, fmt.Errorf("invalid tarball path: %s", tarballPath)
	}

	tarballFile, err := os.Open(tarballPath) //nolint:gosec // tarballPath is validated above
	if err != nil {
		return nil, fmt.Errorf("failed to open tarball: %w", err)
	}
	defer func() {
		if err := tarballFile.Close(); err != nil {
			logger.Warnf("Failed to close tarball file: %v", err)
		}
	}()

	fileInfo, err := tarballFile.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to get file info: %w", err)
	}

	if err := p.storage.PutObject(ctx, p.storageBucket, storagePath, tarballFile, fileInfo.Size()); err != nil {
		return nil, fmt.Errorf("failed to upload to storage: %w", err)
	}

	// Create module version
	metadataJSONB := models.JSONB(metadata.ConvertToJSONB())
	moduleVersion := &models.ModuleVersion{
		ModuleID:     moduleID,
		Version:      version,
		Source:       fmt.Sprintf("git tag: %s", version),
		TarballPath:  storagePath,
		TarballSize:  fileInfo.Size(),
		Readme:       metadata.Readme,
		Inputs:       models.JSONB{"inputs": metadataJSONB["inputs"]},
		Outputs:      models.JSONB{"outputs": metadataJSONB["outputs"]},
		Dependencies: models.JSONB{"dependencies": metadataJSONB["dependencies"]},
		Resources:    models.JSONB{"resources": metadataJSONB["resources"]},
		Submodules:   models.JSONB{"submodules": metadataJSONB["submodules"]},
		PublishedAt:  time.Now(),
	}

	if err := p.moduleVersionRepo.Create(moduleVersion); err != nil {
		return nil, fmt.Errorf("failed to create module version: %w", err)
	}

	return moduleVersion, nil
}

// createTarball creates a gzipped tarball from a directory
func (p *ModulePublisher) createTarball(sourceDir, outputPath string) error {
	file, err := os.Create(outputPath) //nolint:gosec // outputPath is validated (in temp directory)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); err != nil {
			logger.Warnf("Failed to close file: %v", err)
		}
	}()

	gzipWriter := gzip.NewWriter(file)
	defer func() {
		if err := gzipWriter.Close(); err != nil {
			logger.Warnf("Failed to close gzip writer: %v", err)
		}
	}()

	tarWriter := tar.NewWriter(gzipWriter)
	defer func() {
		if err := tarWriter.Close(); err != nil {
			logger.Warnf("Failed to close tar writer: %v", err)
		}
	}()

	return filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip hidden files and directories
		if strings.HasPrefix(info.Name(), ".") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip .git, .terraform, *.tfstate, .terraform.lock.hcl
		if info.IsDir() && (info.Name() == ".git" || info.Name() == ".terraform") {
			return filepath.SkipDir
		}
		if !info.IsDir() && (strings.HasSuffix(path, ".tfstate") || strings.HasSuffix(path, ".terraform.lock.hcl")) {
			return nil
		}

		// Get relative path
		relPath, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}

		// Create tar header
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = relPath

		// Write header
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}

		// Write file content if not a directory
		if !info.IsDir() {
			data, err := os.Open(path) //nolint:gosec // path is from filepath.Walk (sourceDir), validated
			if err != nil {
				return err
			}
			defer func() {
				if err := data.Close(); err != nil {
					logger.Warnf("Failed to close data file: %v", err)
				}
			}()

			if _, err := io.Copy(tarWriter, data); err != nil {
				return err
			}
		}

		return nil
	})
}

// extractTarball extracts a gzipped tarball to a directory
func (p *ModulePublisher) extractTarball(reader io.Reader, destDir string) error {
	gzipReader, err := gzip.NewReader(reader)
	if err != nil {
		return err
	}
	defer func() {
		if err := gzipReader.Close(); err != nil {
			logger.Warnf("Failed to close gzip reader: %v", err)
		}
	}()

	tarReader := tar.NewReader(gzipReader)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		targetPath := filepath.Join(destDir, header.Name) //nolint:gosec // path traversal protection below

		// Security: Prevent directory traversal - ensure targetPath is within destDir
		cleanTargetPath := filepath.Clean(targetPath)
		cleanDestDir := filepath.Clean(destDir)
		if !strings.HasPrefix(cleanTargetPath, cleanDestDir+string(filepath.Separator)) && cleanTargetPath != cleanDestDir {
			return fmt.Errorf("invalid file path in archive (directory traversal attempt): %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			// Security: Validate directory mode to prevent integer overflow
			dirMode := header.Mode & 0o777 // Only use permission bits
			if dirMode > 0o777 {
				dirMode = 0o750 // Default to safe permissions if invalid
			}
			if err := os.MkdirAll(targetPath, os.FileMode(dirMode)); err != nil { //nolint:gosec // dirMode is validated above
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0o750); err != nil {
				return err
			}
			// Security: Validate file mode to prevent integer overflow
			fileMode := header.Mode & 0o777 // Only use permission bits
			if fileMode > 0o777 {
				fileMode = 0o644 // Default to safe permissions if invalid
			}
			file, err := os.OpenFile(targetPath, os.O_CREATE|os.O_RDWR, os.FileMode(fileMode)) //nolint:gosec // fileMode is validated above
			if err != nil {
				return err
			}
			// Security: Limit decompression size to prevent decompression bombs (100MB limit)
			const maxDecompressedSize = 100 * 1024 * 1024 // 100MB
			limitedReader := io.LimitReader(tarReader, maxDecompressedSize)
			if _, err := io.Copy(file, limitedReader); err != nil {
				if closeErr := file.Close(); closeErr != nil {
					logger.Warnf("Failed to close file after copy error: %v", closeErr)
				}
				return err
			}
			if err := file.Close(); err != nil {
				logger.Warnf("Failed to close file: %v", err)
			}
		}
	}

	return nil
}
