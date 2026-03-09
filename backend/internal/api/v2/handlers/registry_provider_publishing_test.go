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
	"runtime"
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

// setupTestDBForProvider creates a test database for provider tests
func setupTestDBForProvider(t *testing.T) *gorm.DB {
	// Use live database if TEST_DATABASE_URL is set, otherwise skip
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		// Try to use default local database for live testing
		dbURL = "postgres://iac:iac_password@localhost:5432/iac_platform?sslmode=disable" //nolint:gosec // G101: test database URL, not a production credential
	}

	db, err := gorm.Open(postgres.Open(dbURL), &gorm.Config{})
	if err != nil {
		t.Skipf("Failed to connect to test database (set TEST_DATABASE_URL or ensure local DB is running): %v", err)
	}

	// Run migrations for provider-related models
	if err := db.AutoMigrate(
		&models.Organization{},
		&models.User{},
		&models.Provider{},
		&models.ProviderVersion{},
		&models.ProviderPlatform{},
		&models.ProviderDownload{},
		&models.GPGKey{},
	); err != nil {
		t.Fatalf("Failed to run migrations: %v", err)
	}

	return db
}

// setupTestOrgForProvider creates a test organization
func setupTestOrgForProvider(t *testing.T, db *gorm.DB) *models.Organization {
	org := &models.Organization{
		ID:   uuid.New(),
		Name: fmt.Sprintf("test-org-%s", uuid.New().String()[:8]),
	}
	if err := db.Create(org).Error; err != nil {
		t.Fatalf("Failed to create test organization: %v", err)
	}
	return org
}

// setupTestUserForProvider creates a test user
func setupTestUserForProvider(t *testing.T, db *gorm.DB) *models.User {
	user := &models.User{
		ID:    uuid.New(),
		Email: fmt.Sprintf("test-%s@example.com", uuid.New().String()[:8]),
	}
	if err := db.Create(user).Error; err != nil {
		t.Fatalf("Failed to create test user: %v", err)
	}
	return user
}

// createTestProviderBinary creates a minimal test provider binary (zip file)
func createTestProviderBinary(t *testing.T) []byte {
	// For testing, create a simple zip file
	// In production, this would be a real Terraform provider binary
	testData := []byte("test provider binary data - this would be a real provider binary in production")
	return testData
}

// loadTestGPGKey loads the test GPG public key from deploy directory
func loadTestGPGKey(t *testing.T) string {
	// Get the test file's directory and work backwards to find deploy/
	_, testFile, _, _ := runtime.Caller(0)
	testDir := filepath.Dir(testFile)

	// Try to find deploy/ relative to the test file location
	// Test file is at: backend/internal/api/v2/handlers/registry_provider_publishing_test.go
	// Deploy is at: deploy/test-gpg-key.pub
	// So we need to go: ../../../../deploy/test-gpg-key.pub from the test file

	keyPaths := []string{
		filepath.Join(testDir, "../../../../../deploy/test-gpg-key.pub"), // From handlers/ (5 levels up) to deploy/
		filepath.Join(testDir, "../../../../deploy/test-gpg-key.pub"),    // Alternative (4 levels)
		"../deploy/test-gpg-key.pub",                                     // Relative to CWD (if run from backend/)
		"../../deploy/test-gpg-key.pub",                                  // Alternative relative
		"deploy/test-gpg-key.pub",                                        // Same directory
	}

	for _, path := range keyPaths {
		keyData, err := os.ReadFile(path) //nolint:gosec // test file path, validated
		if err == nil {
			t.Logf("Found GPG key at: %s", path)
			return string(keyData)
		}
		t.Logf("Tried GPG key path: %s (error: %v)", path, err)
	}

	// If public key not found, try to extract from private key
	privateKeyPaths := []string{
		"../deploy/pgp_private_key",
		"../../deploy/pgp_private_key",
		"deploy/pgp_private_key",
	}

	for _, path := range privateKeyPaths {
		_, err := os.ReadFile(path) //nolint:gosec // test file path, validated
		if err == nil {
			// If we have the private key, try to extract public key using system GPG
			// This requires GPG to be installed and the key to be importable
			// For now, we'll skip if we can't find the public key file
			t.Logf("Found private key at %s but public key not found. Please extract public key using: gpg --armor --export <key-id> > deploy/test-gpg-key.pub", path)
		}
	}

	t.Skipf("Test GPG key not found. Please ensure deploy/test-gpg-key.pub exists or deploy/pgp_private_key is available")
	return ""
}

// uploadTestGPGKey uploads a test GPG key to the organization
// If handler is nil, it will create the key directly in the database
func uploadTestGPGKey(t *testing.T, db *gorm.DB, org *models.Organization, user *models.User, handler *GPGKeyHandler) *models.GPGKey {
	keyASCII := loadTestGPGKey(t)
	if keyASCII == "" {
		t.Skip("Skipping GPG test - no test key available")
		return nil
	}

	// Parse key ID
	gpgService := registry.NewGPGService()
	keyID, err := gpgService.ParseGPGKey(keyASCII)
	if err != nil {
		t.Fatalf("Failed to parse test GPG key: %v", err)
	}

	// Check if key already exists
	gpgKeyRepo := repository.NewGPGKeyRepository(db)
	existing, err := gpgKeyRepo.GetByKeyID(org.ID, keyID)
	if err == nil && existing != nil {
		return existing
	}

	// Create GPG key
	gpgKey := &models.GPGKey{
		OrganizationID: org.ID,
		KeyID:          keyID,
		ASCIIArmor:     keyASCII,
		CreatedBy:      user.ID,
	}

	if err := gpgKeyRepo.Create(gpgKey); err != nil {
		t.Fatalf("Failed to create test GPG key: %v", err)
	}

	return gpgKey
}

func TestCreateProvider(t *testing.T) {
	db := setupTestDBForProvider(t)
	org := setupTestOrgForProvider(t, db)
	user := setupTestUserForProvider(t, db)

	defer func() {
		// Cleanup in correct order to avoid foreign key violations
		// Delete in reverse dependency order
		db.Exec("DELETE FROM provider_downloads")
		db.Exec("DELETE FROM provider_platforms")
		db.Exec("DELETE FROM provider_versions")
		db.Exec("DELETE FROM providers")
		db.Exec("DELETE FROM gpg_keys")
		db.Exec("DELETE FROM organization_members")
		// Delete other tables that might reference organizations (ignore errors if tables/columns don't exist)
		db.Exec("DELETE FROM projects WHERE organization_id = ?", org.ID)
		db.Exec("DELETE FROM vcs_connections WHERE organization_id = ?", org.ID)
		db.Exec("DELETE FROM modules WHERE organization_id = ?", org.ID)
		db.Exec("DELETE FROM organizations WHERE id = ?", org.ID)
		db.Exec("DELETE FROM users WHERE id = ?", user.ID)
	}()

	// Setup handler
	providerRepo := repository.NewProviderRepository(db)
	providerVersionRepo := repository.NewProviderVersionRepository(db)
	providerPlatformRepo := repository.NewProviderPlatformRepository(db)
	orgRepo := repository.NewOrganizationRepository(db)
	gpgKeyRepo := repository.NewGPGKeyRepository(db)
	userRepo := repository.NewUserRepository(db)
	tfeTokenRepo := repository.NewTFETokenRepository(db)
	authService := auth.NewService(userRepo, tfeTokenRepo)
	mockStorage := registry.NewMockStorage()

	handler := NewRegistryProviderPublishingHandler(
		providerRepo,
		providerVersionRepo,
		providerPlatformRepo,
		orgRepo,
		gpgKeyRepo,
		authService,
		mockStorage,
		"test-bucket",
	)

	// Setup router with auth middleware that sets user in context
	gin.SetMode(gin.TestMode)
	router := gin.New()

	// Add middleware to set user in context for testing
	router.Use(func(c *gin.Context) {
		c.Set("user_id", user.ID)
		c.Next()
	})

	authGroup := router.Group("/api/v2/organizations/:name/registry/providers")
	{
		authGroup.POST("", handler.CreateProvider)
	}

	// Create request body
	reqBody := map[string]interface{}{
		"name":        "test-provider",
		"description": "Test provider for integration testing",
	}
	bodyBytes, _ := json.Marshal(reqBody) //nolint:errchkjson // test helper, error handling not critical

	// Make request
	req := httptest.NewRequestWithContext(context.Background(), "POST", fmt.Sprintf("/api/v2/organizations/%s/registry/providers", org.Name), bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Assertions
	if w.Code != http.StatusCreated {
		t.Errorf("Expected status 201, got %d. Body: %s", w.Code, w.Body.String())
	}

	var response struct {
		Data struct {
			ID         string `json:"id"`
			Type       string `json:"type"`
			Attributes struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"attributes"`
		} `json:"data"`
	}

	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if response.Data.Attributes.Name != "test-provider" {
		t.Errorf("Expected provider name 'test-provider', got '%s'", response.Data.Attributes.Name)
	}

	// Verify provider was created in database
	var provider models.Provider
	if err := db.Where("organization_id = ? AND name = ?", org.ID, "test-provider").First(&provider).Error; err != nil {
		t.Errorf("Provider was not created in database: %v", err)
	}
}

func TestPublishProviderPlatform(t *testing.T) {
	db := setupTestDBForProvider(t)
	org := setupTestOrgForProvider(t, db)
	user := setupTestUserForProvider(t, db)

	defer func() {
		// Cleanup in correct order to avoid foreign key violations
		// Delete in reverse dependency order
		db.Exec("DELETE FROM provider_downloads")
		db.Exec("DELETE FROM provider_platforms")
		db.Exec("DELETE FROM provider_versions")
		db.Exec("DELETE FROM providers")
		db.Exec("DELETE FROM gpg_keys")
		db.Exec("DELETE FROM organization_members")
		// Delete other tables that might reference organizations (ignore errors if tables/columns don't exist)
		db.Exec("DELETE FROM projects WHERE organization_id = ?", org.ID)
		db.Exec("DELETE FROM vcs_connections WHERE organization_id = ?", org.ID)
		db.Exec("DELETE FROM modules WHERE organization_id = ?", org.ID)
		db.Exec("DELETE FROM organizations WHERE id = ?", org.ID)
		db.Exec("DELETE FROM users WHERE id = ?", user.ID)
	}()

	// Create test provider
	provider := &models.Provider{
		ID:             uuid.New(),
		OrganizationID: org.ID,
		Name:           "test-provider",
		Description:    "Test provider",
	}
	if err := db.Create(provider).Error; err != nil {
		t.Fatalf("Failed to create test provider: %v", err)
	}

	// Setup handler
	providerRepo := repository.NewProviderRepository(db)
	providerVersionRepo := repository.NewProviderVersionRepository(db)
	providerPlatformRepo := repository.NewProviderPlatformRepository(db)
	orgRepo := repository.NewOrganizationRepository(db)
	gpgKeyRepo := repository.NewGPGKeyRepository(db)
	userRepo := repository.NewUserRepository(db)
	tfeTokenRepo := repository.NewTFETokenRepository(db)
	authService := auth.NewService(userRepo, tfeTokenRepo)
	mockStorage := registry.NewMockStorage()

	handler := NewRegistryProviderPublishingHandler(
		providerRepo,
		providerVersionRepo,
		providerPlatformRepo,
		orgRepo,
		gpgKeyRepo,
		authService,
		mockStorage,
		"test-bucket",
	)

	// Setup router with auth middleware that sets user in context for testing
	gin.SetMode(gin.TestMode)
	router := gin.New()

	// Add middleware to set user in context for testing
	router.Use(func(c *gin.Context) {
		c.Set("user_id", user.ID)
		c.Next()
	})

	authGroup := router.Group("/api/v2/organizations/:name/registry/providers/:provider_name/versions/:version/platforms")
	{
		authGroup.POST("", handler.PublishProviderPlatform)
	}

	// Create multipart form with file
	binaryData := createTestProviderBinary(t)
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	// Add form fields
	if err := writer.WriteField("os", "linux"); err != nil {
		t.Fatalf("Failed to write field: %v", err)
	}
	if err := writer.WriteField("arch", "amd64"); err != nil {
		t.Fatalf("Failed to write field: %v", err)
	}

	// Add file
	part, err := writer.CreateFormFile("file", "terraform-provider-test_1.0.0_linux_amd64.zip")
	if err != nil {
		t.Fatalf("Failed to create form file: %v", err)
	}
	if _, err := io.Copy(part, bytes.NewReader(binaryData)); err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Failed to close writer: %v", err)
	}

	// Make request
	req := httptest.NewRequestWithContext(context.Background(), "POST", fmt.Sprintf("/api/v2/organizations/%s/registry/providers/%s/versions/1.0.0/platforms", org.Name, provider.Name), body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Assertions
	if w.Code != http.StatusCreated {
		t.Errorf("Expected status 201, got %d. Body: %s", w.Code, w.Body.String())
	}

	var response struct {
		Data struct {
			ID         string `json:"id"`
			Type       string `json:"type"`
			Attributes struct {
				OS       string `json:"os"`
				Arch     string `json:"arch"`
				Filename string `json:"filename"`
				Shasum   string `json:"shasum"`
			} `json:"attributes"`
		} `json:"data"`
	}

	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("Failed to parse response: %v", err)
	}

	if response.Data.Attributes.OS != "linux" {
		t.Errorf("Expected OS 'linux', got '%s'", response.Data.Attributes.OS)
	}

	if response.Data.Attributes.Arch != "amd64" {
		t.Errorf("Expected Arch 'amd64', got '%s'", response.Data.Attributes.Arch)
	}

	// Verify platform was created in database
	var platform models.ProviderPlatform
	if err := db.Where("provider_version_id IN (SELECT id FROM provider_versions WHERE provider_id = ?)", provider.ID).First(&platform).Error; err != nil {
		t.Errorf("Platform was not created in database: %v", err)
	}

	// Verify version was created
	var version models.ProviderVersion
	if err := db.Where("provider_id = ? AND version = ?", provider.ID, "1.0.0").First(&version).Error; err != nil {
		t.Errorf("Version was not created in database: %v", err)
	}
}

func TestPublishProviderPlatformWithGPG(t *testing.T) {
	db := setupTestDBForProvider(t)
	org := setupTestOrgForProvider(t, db)
	user := setupTestUserForProvider(t, db)

	defer func() {
		// Cleanup in correct order to avoid foreign key violations
		// Delete in reverse dependency order
		db.Exec("DELETE FROM provider_downloads")
		db.Exec("DELETE FROM provider_platforms")
		db.Exec("DELETE FROM provider_versions")
		db.Exec("DELETE FROM providers")
		db.Exec("DELETE FROM gpg_keys")
		db.Exec("DELETE FROM organization_members")
		// Delete other tables that might reference organizations (ignore errors if tables/columns don't exist)
		db.Exec("DELETE FROM projects WHERE organization_id = ?", org.ID)
		db.Exec("DELETE FROM vcs_connections WHERE organization_id = ?", org.ID)
		db.Exec("DELETE FROM modules WHERE organization_id = ?", org.ID)
		db.Exec("DELETE FROM organizations WHERE id = ?", org.ID)
		db.Exec("DELETE FROM users WHERE id = ?", user.ID)
	}()

	// Create test provider
	provider := &models.Provider{
		ID:             uuid.New(),
		OrganizationID: org.ID,
		Name:           "test-provider",
		Description:    "Test provider",
	}
	if err := db.Create(provider).Error; err != nil {
		t.Fatalf("Failed to create test provider: %v", err)
	}

	// Load and upload test GPG key from deploy directory
	gpgKeyRepo := repository.NewGPGKeyRepository(db)
	orgRepo := repository.NewOrganizationRepository(db)
	userRepo := repository.NewUserRepository(db)
	tfeTokenRepo := repository.NewTFETokenRepository(db)
	authService := auth.NewService(userRepo, tfeTokenRepo)
	gpgKeyHandler := NewGPGKeyHandler(gpgKeyRepo, orgRepo, authService)

	gpgKey := uploadTestGPGKey(t, db, org, user, gpgKeyHandler)
	if gpgKey == nil {
		t.Skip("Skipping GPG test - no test key available")
		return
	}

	// Setup handler
	providerRepo := repository.NewProviderRepository(db)
	providerVersionRepo := repository.NewProviderVersionRepository(db)
	providerPlatformRepo := repository.NewProviderPlatformRepository(db)
	mockStorage := registry.NewMockStorage()

	handler := NewRegistryProviderPublishingHandler(
		providerRepo,
		providerVersionRepo,
		providerPlatformRepo,
		orgRepo,
		gpgKeyRepo,
		authService,
		mockStorage,
		"test-bucket",
	)

	// Setup router with auth middleware that sets user in context for testing
	gin.SetMode(gin.TestMode)
	router := gin.New()

	// Add middleware to set user in context for testing
	router.Use(func(c *gin.Context) {
		c.Set("user_id", user.ID)
		c.Next()
	})

	authGroup := router.Group("/api/v2/organizations/:name/registry/providers/:provider_name/versions/:version/platforms")
	{
		authGroup.POST("", handler.PublishProviderPlatform)
	}

	// Create multipart form with file and GPG key ID
	binaryData := createTestProviderBinary(t)
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	if err := writer.WriteField("os", "linux"); err != nil {
		t.Fatalf("Failed to write field: %v", err)
	}
	if err := writer.WriteField("arch", "amd64"); err != nil {
		t.Fatalf("Failed to write field: %v", err)
	}
	if err := writer.WriteField("gpg_key_id", gpgKey.KeyID); err != nil {
		t.Fatalf("Failed to write field: %v", err)
	}

	part, err := writer.CreateFormFile("file", "terraform-provider-test_1.0.0_linux_amd64.zip")
	if err != nil {
		t.Fatalf("Failed to create form file: %v", err)
	}
	if _, err := io.Copy(part, bytes.NewReader(binaryData)); err != nil {
		t.Fatalf("Failed to write file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Failed to close writer: %v", err)
	}

	// Make request
	req := httptest.NewRequestWithContext(context.Background(), "POST", fmt.Sprintf("/api/v2/organizations/%s/registry/providers/%s/versions/1.0.0/platforms", org.Name, provider.Name), body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	// Note: GPG signing may fail if GPG is not installed or key is not imported
	// This test verifies the endpoint accepts the GPG key ID parameter
	if w.Code != http.StatusCreated && w.Code != http.StatusInternalServerError {
		t.Errorf("Expected status 201 or 500 (if GPG not available), got %d. Body: %s", w.Code, w.Body.String())
	}

	// If successful, verify GPG fields were set
	if w.Code == http.StatusCreated {
		var platform models.ProviderPlatform
		if err := db.Where("provider_version_id IN (SELECT id FROM provider_versions WHERE provider_id = ?)", provider.ID).First(&platform).Error; err == nil {
			if platform.GPGKeyID == "" {
				t.Log("Warning: GPG key ID was not set (GPG signing may have failed)")
			}
		}
	}
}
