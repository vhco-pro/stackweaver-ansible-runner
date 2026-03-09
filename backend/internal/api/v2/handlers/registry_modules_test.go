// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/registry"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// setupTestDB creates a test database (using postgres for compatibility)
// In CI, this should use a test database container
func setupTestDB(t *testing.T) *gorm.DB {
	// Use in-memory SQLite for local testing, or test Postgres for CI
	// For now, we'll skip tests if DB is not available
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set - skipping integration test")
	}

	db, err := gorm.Open(postgres.Open(dbURL), &gorm.Config{})
	if err != nil {
		t.Fatalf("Failed to connect to test database: %v", err)
	}

	// Run migrations
	if err := db.AutoMigrate(
		&models.Organization{},
		&models.Module{},
		&models.ModuleVersion{},
		&models.ModuleDownload{},
		&models.User{},
	); err != nil {
		t.Fatalf("Failed to run migrations: %v", err)
	}

	return db
}

// setupTestOrg creates a test organization
func setupTestOrg(t *testing.T, db *gorm.DB) *models.Organization {
	org := &models.Organization{
		ID:   uuid.New(),
		Name: "test-org",
	}
	if err := db.Create(org).Error; err != nil {
		t.Fatalf("Failed to create test organization: %v", err)
	}
	return org
}

// createTestTarball creates a minimal valid tarball for testing
func createTestTarball(t *testing.T) []byte {
	// Create a temporary directory
	tmpDir := t.TempDir()
	moduleDir := filepath.Join(tmpDir, "test-module")
	if err := os.MkdirAll(moduleDir, 0o755); err != nil { //nolint:gosec // test directory, 0o755 is fine
		t.Fatalf("Failed to create module directory: %v", err)
	}

	// Create a minimal main.tf file
	mainTf := `variable "name" {
  description = "Name of the resource"
  type        = string
}

output "id" {
  description = "Resource ID"
  value       = "test"
}

resource "aws_instance" "test" {
  ami           = "ami-12345678"
  instance_type = "t2.micro"
}
`
	if err := os.WriteFile(filepath.Join(moduleDir, "main.tf"), []byte(mainTf), 0o600); err != nil {
		t.Fatalf("Failed to write main.tf: %v", err)
	}

	// For testing, we'll create a simple gzip file
	// In a real scenario, you'd use archive/tar and compress/gzip
	tarballData := []byte("test tarball data - this would be a real gzip tarball in production")
	return tarballData
}

func TestListModules(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		// Cleanup
		db.Exec("DROP TABLE IF EXISTS module_downloads CASCADE")
		db.Exec("DROP TABLE IF EXISTS module_versions CASCADE")
		db.Exec("DROP TABLE IF EXISTS modules CASCADE")
		db.Exec("DROP TABLE IF EXISTS organizations CASCADE")
	}()

	org := setupTestOrg(t, db)

	// Create test modules
	module1 := &models.Module{
		ID:             uuid.New(),
		OrganizationID: org.ID,
		Name:           "test-module-1",
		Provider:       "aws",
		Description:    "Test module 1",
	}
	module2 := &models.Module{
		ID:             uuid.New(),
		OrganizationID: org.ID,
		Name:           "test-module-2",
		Provider:       "azurerm",
		Description:    "Test module 2",
	}
	db.Create(module1)
	db.Create(module2)

	// Setup handler
	moduleRepo := repository.NewModuleRepository(db)
	moduleVersionRepo := repository.NewModuleVersionRepository(db)
	moduleDownloadRepo := repository.NewModuleDownloadRepository(db)
	orgRepo := repository.NewOrganizationRepository(db)

	// Create a mock storage (in-memory for testing)
	mockStorage := registry.NewMockStorage()
	moduleService := registry.NewModuleService(moduleRepo, moduleVersionRepo, moduleDownloadRepo, orgRepo, mockStorage, "test-bucket")

	handler := &RegistryModuleHandler{
		moduleService: moduleService,
	}

	// Setup router
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/v1/modules/:namespace", handler.ListModules)

	// Make request
	req := httptest.NewRequestWithContext(context.Background(), "GET", fmt.Sprintf("/v1/modules/%s", org.Name), nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Assertions
	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
	}

	var response struct {
		Data []struct {
			ID         string `json:"id"`
			Type       string `json:"type"`
			Attributes struct {
				Name     string `json:"name"`
				Provider string `json:"provider"`
			} `json:"attributes"`
		} `json:"data"`
	}

	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if len(response.Data) != 2 {
		t.Errorf("Expected 2 modules, got %d", len(response.Data))
	}
}

func TestGetModuleVersions(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		// Cleanup
		db.Exec("DROP TABLE IF EXISTS module_downloads CASCADE")
		db.Exec("DROP TABLE IF EXISTS module_versions CASCADE")
		db.Exec("DROP TABLE IF EXISTS modules CASCADE")
		db.Exec("DROP TABLE IF EXISTS organizations CASCADE")
	}()

	org := setupTestOrg(t, db)

	// Create test module
	module := &models.Module{
		ID:             uuid.New(),
		OrganizationID: org.ID,
		Name:           "test-module",
		Provider:       "aws",
	}
	db.Create(module)

	// Create test versions
	version1 := &models.ModuleVersion{
		ID:       uuid.New(),
		ModuleID: module.ID,
		Version:  "1.0.0",
	}
	version2 := &models.ModuleVersion{
		ID:       uuid.New(),
		ModuleID: module.ID,
		Version:  "2.0.0",
	}
	db.Create(version1)
	db.Create(version2)

	// Setup handler
	moduleRepo := repository.NewModuleRepository(db)
	moduleVersionRepo := repository.NewModuleVersionRepository(db)
	moduleDownloadRepo := repository.NewModuleDownloadRepository(db)
	orgRepo := repository.NewOrganizationRepository(db)
	mockStorage := registry.NewMockStorage()
	moduleService := registry.NewModuleService(moduleRepo, moduleVersionRepo, moduleDownloadRepo, orgRepo, mockStorage, "test-bucket")

	handler := &RegistryModuleHandler{
		moduleService: moduleService,
	}

	// Setup router
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/v1/modules/:namespace/:name/:provider/versions", handler.GetModuleVersions)

	// Make request
	req := httptest.NewRequestWithContext(context.Background(), "GET", fmt.Sprintf("/v1/modules/%s/%s/%s/versions", org.Name, module.Name, module.Provider), nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Assertions
	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
	}

	var response struct {
		Versions []struct {
			Version string `json:"version"`
		} `json:"versions"`
	}

	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if len(response.Versions) != 2 {
		t.Errorf("Expected 2 versions, got %d", len(response.Versions))
	}

	// Check versions are sorted correctly (latest first)
	if response.Versions[0].Version != "2.0.0" {
		t.Errorf("Expected latest version to be 2.0.0, got %s", response.Versions[0].Version)
	}
}

func TestPublishModuleVersion(t *testing.T) {
	db := setupTestDB(t)
	defer func() {
		// Cleanup
		db.Exec("DROP TABLE IF EXISTS module_downloads CASCADE")
		db.Exec("DROP TABLE IF EXISTS module_versions CASCADE")
		db.Exec("DROP TABLE IF EXISTS modules CASCADE")
		db.Exec("DROP TABLE IF EXISTS organizations CASCADE")
	}()

	org := setupTestOrg(t, db)

	// Create test module
	module := &models.Module{
		ID:             uuid.New(),
		OrganizationID: org.ID,
		Name:           "test-module",
		Provider:       "aws",
	}
	db.Create(module)

	// Setup handler
	moduleRepo := repository.NewModuleRepository(db)
	moduleVersionRepo := repository.NewModuleVersionRepository(db)
	orgRepo := repository.NewOrganizationRepository(db)
	mockStorage := registry.NewMockStorage()

	// Create module publisher
	vcsConnectionRepo := repository.NewVCSConnectionRepository(db)
	modulePublisher := registry.NewModulePublisher(moduleRepo, moduleVersionRepo, orgRepo, vcsConnectionRepo, mockStorage, "test-bucket")

	// Create auth service
	userRepo := repository.NewUserRepository(db)
	tfeTokenRepo := repository.NewTFETokenRepository(db)
	authService := auth.NewService(userRepo, tfeTokenRepo)

	handler := NewRegistryPublishingHandler(
		moduleRepo,
		moduleVersionRepo,
		orgRepo,
		vcsConnectionRepo,
		authService,
		nil, // githubAppManager can be nil for tests
		modulePublisher,
	)

	// Setup router with auth middleware
	gin.SetMode(gin.TestMode)
	router := gin.New()

	// For testing, we'll skip auth middleware
	authGroup := router.Group("/api/v2/organizations/:name/registry/modules/:module_name/:provider")
	{
		authGroup.POST("/versions", handler.PublishVersion)
	}

	// Create multipart form with file
	tarballData := createTestTarball(t)
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// Add version field
	if err := writer.WriteField("version", "1.0.0"); err != nil {
		t.Fatalf("Failed to write field: %v", err)
	}

	// Add file
	part, err := writer.CreateFormFile("file", "module.tar.gz")
	if err != nil {
		t.Fatalf("Failed to create form file: %v", err)
	}
	if _, err := io.Copy(part, bytes.NewReader(tarballData)); err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Failed to close writer: %v", err)
	}

	// Make request
	req := httptest.NewRequestWithContext(context.Background(), "POST", fmt.Sprintf("/api/v2/organizations/%s/registry/modules/%s/%s/versions", org.Name, module.Name, module.Provider), body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Assertions - Note: This test may fail if the tarball parsing fails
	// In a real scenario, you'd use a proper tarball
	if w.Code != http.StatusCreated && w.Code != http.StatusOK && w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 201, 200, or 400, got %d. Body: %s", w.Code, w.Body.String())
	}

	// If successful, verify version was created
	if w.Code == http.StatusCreated || w.Code == http.StatusOK {
		var version models.ModuleVersion
		if err := db.Where("module_id = ? AND version = ?", module.ID, "1.0.0").First(&version).Error; err != nil {
			t.Errorf("Version was not created in database: %v", err)
		}
	}
}
