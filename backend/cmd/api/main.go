// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/iac-platform/backend/internal/api/routes"
	"github.com/iac-platform/backend/internal/api/v2/handlers"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/ansible"
	"github.com/iac-platform/backend/internal/services/apikey"
	"github.com/iac-platform/backend/internal/services/auth"
	"github.com/iac-platform/backend/internal/services/oidc"
	"github.com/iac-platform/backend/internal/services/profile"
	"github.com/iac-platform/backend/internal/services/runner"
	"github.com/iac-platform/backend/internal/services/sessions"
	teamsync "github.com/iac-platform/backend/internal/services/team_sync"
	"github.com/iac-platform/backend/internal/services/terraform"
	"github.com/iac-platform/backend/internal/services/totp"
	"github.com/iac-platform/backend/internal/services/variable"
	"github.com/iac-platform/backend/internal/services/vcs"
	"github.com/iac-platform/backend/pkg/crypto"
	"github.com/michielvha/logger"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Server struct {
		Host         string        `yaml:"host"`
		Port         int           `yaml:"port"`
		ReadTimeout  time.Duration `yaml:"read_timeout"`
		WriteTimeout time.Duration `yaml:"write_timeout"`
	} `yaml:"server"`
	Database repository.Config `yaml:"database"`
	Zitadel  struct {
		Issuer       string `yaml:"issuer"`
		ClientID     string `yaml:"client_id"`
		ClientSecret string `yaml:"client_secret"` //nolint:gosec // G117: config field, not a hardcoded secret
	} `yaml:"zitadel"`
}

func main() {
	// Initialize logger first (reads LOG_LEVEL from environment)
	logLevel := os.Getenv("LOG_LEVEL")
	logger.Init(logLevel)

	// Load configuration
	// When CONFIG_PATH is explicitly set, the file must exist (misconfiguration is fatal).
	// When CONFIG_PATH is unset, the default path is tried; if missing the binary
	// continues with defaults + env-var overrides (enables file-free Kubernetes deploys).
	configPath := os.Getenv("CONFIG_PATH")
	explicitPath := configPath != ""
	if !explicitPath {
		configPath = "config/config.yaml"
	}

	config := defaultConfig()
	configData, err := os.ReadFile(configPath) //nolint:gosec // configPath is from environment variable, validated
	switch {
	case err == nil:
		if err := yaml.Unmarshal(configData, &config); err != nil {
			logger.Fatalf("Failed to parse config: %v", err)
		}
	case errors.Is(err, os.ErrNotExist) && !explicitPath:
		logger.Info("No config file found, using environment variables only")
	default:
		logger.Fatalf("Failed to read config file: %v", err)
	}

	// Apply environment variable overrides (allows Kubernetes deployments to
	// inject configuration without modifying config.yaml).
	applyEnvOverrides(&config)

	// Initialize database
	db, err := repository.NewDatabase(config.Database)
	if err != nil {
		logger.Fatalf("Failed to connect to database: %v", err)
	}

	// Run database migrations
	logger.Info("Running database migrations...")

	// Enable UUID extension if not already enabled
	sqlDB, err := db.DB()
	if err != nil {
		logger.Fatalf("Failed to get underlying sql.DB: %v", err)
	}
	if _, err := sqlDB.Exec("CREATE EXTENSION IF NOT EXISTS \"uuid-ossp\""); err != nil {
		logger.Warnf("Failed to enable uuid-ossp extension (may already be enabled): %v", err)
	}

	// Run GORM AutoMigrate to create/update tables
	if err := db.AutoMigrate(
		&models.User{},
		&models.Organization{},
		&models.OrganizationMember{},
		&models.Team{},
		&models.TeamMember{},
		&models.TeamProjectAccess{},
		&models.TeamWorkspaceAccess{},
		&models.TeamOrganizationAccess{},
		&models.AgentPool{},
		&models.Runner{},
		&models.RunnerJobExecution{},
		&models.AnsibleConfig{},
		&models.Project{},
		&models.VCSConnection{},
		&models.Workspace{},
		&models.ConfigurationVersion{},
		&models.Run{},
		&models.RunPhaseState{},
		&models.Variable{},
		&models.VariableSet{},
		&models.VariableSetVariable{},
		&models.VariableSetWorkspace{},
		&models.VariableSetProject{},
		&models.StateVersion{},
		&models.StateLock{},
		&models.AuditLog{},
		&models.APIKey{},
		&models.WebhookEvent{},
		// Registry models
		&models.Module{},
		&models.ModuleVersion{},
		&models.ModuleDownload{},
		&models.Provider{},
		&models.ProviderVersion{},
		&models.ProviderPlatform{},
		&models.ProviderDownload{},
		&models.GPGKey{},
		// Ansible models
		&models.AnsibleInventory{},
		&models.AnsibleInventoryHost{},
		&models.AnsibleInventoryGroup{},
		&models.AnsibleCredential{},
		&models.AnsiblePlaybook{},
		&models.AnsibleJobTemplate{},
		&models.AnsibleJobTemplateVariable{},
		&models.AnsibleJob{},
		&models.AnsibleJobEvent{},
		&models.AnsibleInventorySource{},
		&models.AnsibleSchedule{},
		// Ansible Workflow models
		&models.AnsibleWorkflow{},
		&models.AnsibleWorkflowNode{},
		&models.AnsibleWorkflowEdge{},
		&models.AnsibleWorkflowJob{},
		&models.AnsibleWorkflowNodeJob{},
		// Admin models
		&models.TerraformVersion{},
		// OIDC configuration models
		&models.AzureOIDCConfiguration{},
	); err != nil {
		logger.Fatalf("Failed to run database migrations: %v", err)
	}
	logger.Info("Database migrations completed successfully")

	// Seed official Terraform versions (like TFE's built-in version catalog)
	handlers.SeedOfficialVersions(db)
	logger.Info("Terraform versions seeded")

	// Initialize repositories
	userRepo := repository.NewUserRepository(db)

	// Initialize auth service
	tfeTokenRepo := repository.NewTFETokenRepository(db)
	authService := auth.NewService(userRepo, tfeTokenRepo)

	// Initialize Zitadel verifier
	// Prefer environment variables over config file values
	zitadelClientID := os.Getenv("ZITADEL_API_CLIENT_ID")
	if zitadelClientID == "" {
		zitadelClientID = config.Zitadel.ClientID
	}

	zitadelClientSecret := os.Getenv("ZITADEL_API_CLIENT_SECRET")
	if zitadelClientSecret == "" {
		zitadelClientSecret = config.Zitadel.ClientSecret
	}

	zitadelIssuer := os.Getenv("ZITADEL_ISSUER")
	if zitadelIssuer == "" {
		zitadelIssuer = config.Zitadel.Issuer
	}

	// ZITADEL_INTERNAL_ADDR is used for JWKS fetching and gRPC connections (stays on localhost)
	// ZITADEL_ISSUER may be an external domain (e.g. https://zitadel.example.com) for JWT validation
	zitadelInternalAddr := os.Getenv("ZITADEL_INTERNAL_ADDR")
	if zitadelInternalAddr == "" {
		zitadelInternalAddr = "internal-zitadel:8080"
	}

	if err := authService.InitializeZitadel(zitadelIssuer, zitadelClientID, zitadelClientSecret, zitadelInternalAddr); err != nil {
		logger.Fatalf("Failed to initialize Zitadel verifier: %v", err)
	}

	loginServicePAT := os.Getenv("ZITADEL_LOGIN_SERVICE_USER_TOKEN")
	if loginServicePAT == "" {
		logger.Warn("ZITADEL_LOGIN_SERVICE_USER_TOKEN not set, TOTP service will not be available")
	}

	var totpService *totp.Service
	if loginServicePAT != "" {
		totpService, err = totp.NewService(zitadelIssuer, zitadelInternalAddr, loginServicePAT)
		if err != nil {
			logger.Warnf("Failed to initialize TOTP service: %v", err)
		}
	}

	// Initialize Profile service
	var profileService *profile.Service
	if loginServicePAT != "" {
		profileService, err = profile.NewService(zitadelIssuer, zitadelInternalAddr, loginServicePAT)
		if err != nil {
			logger.Warnf("Failed to initialize Profile service: %v", err)
		}
	}

	// Initialize Sessions service
	var sessionsService *sessions.Service
	if loginServicePAT != "" {
		sessionsService, err = sessions.NewService(zitadelIssuer, zitadelInternalAddr, loginServicePAT)
		if err != nil {
			logger.Warnf("Failed to initialize Sessions service: %v", err)
		}
	}

	// Initialize API Key service
	apiKeyRepo := repository.NewAPIKeyRepository(db)
	orgRepo := repository.NewOrganizationRepository(db)
	projectRepo := repository.NewProjectRepository(db)
	teamRepo := repository.NewTeamRepository(db)
	apiKeyService := apikey.NewService(apiKeyRepo, orgRepo, projectRepo, teamRepo)

	// Set API key service in auth service for authentication
	authService.SetAPIKeyService(apiKeyService)

	// Initialize TeamSync service for automatic SSO team assignment
	teamSyncConfig := teamsync.ConfigFromEnv()
	teamSyncService := teamsync.NewService(teamSyncConfig, teamRepo, orgRepo)
	authService.SetTeamSyncer(teamSyncService)
	if teamSyncConfig.Enabled {
		logger.Info("SSO team sync enabled")
	}

	// Initialize GitHub App Manager (loaded once at startup - like Terraform Enterprise)
	githubAppManager, err := vcs.NewGitHubAppManager()
	switch {
	case err != nil:
		logger.Warnf("Failed to initialize GitHub App Manager: %v (GitHub App features will be disabled)", err)
		githubAppManager = nil
	case githubAppManager != nil && githubAppManager.IsEnabled():
		logger.Info("GitHub App Manager initialized successfully")
	default:
		logger.Info("GitHub App Manager not configured (set GITHUB_APP_ID, GITHUB_APP_NAME, and GITHUB_APP_PRIVATE_KEY to enable)")
	}

	// Initialize Scheduler Service for scheduled Ansible jobs
	var schedulerService *ansible.SchedulerService
	schedulerEnabled := os.Getenv("ANSIBLE_SCHEDULER_ENABLED") != "false" // Enabled by default

	if schedulerEnabled {
		// Initialize repositories needed for scheduler
		scheduleRepo := repository.NewAnsibleScheduleRepository(db)
		ansibleJobRepo := repository.NewAnsibleJobRepository(db)
		ansibleTemplateRepo := repository.NewAnsibleJobTemplateRepository(db)
		ansiblePlaybookRepo := repository.NewAnsiblePlaybookRepository(db)
		ansibleInventoryRepo := repository.NewAnsibleInventoryRepository(db)
		ansibleCredentialRepo := repository.NewAnsibleCredentialRepository(db)
		inventorySourceRepo := repository.NewAnsibleInventorySourceRepository(db)

		// Get encryption key for credentials
		encryptionKey := os.Getenv("ANSIBLE_ENCRYPTION_KEY")
		if encryptionKey == "" {
			encryptionKey = os.Getenv("ENCRYPTION_KEY")
		}

		var encryptionKeyBytes []byte
		if encryptionKey != "" {
			var decodeErr error
			encryptionKeyBytes, decodeErr = hex.DecodeString(encryptionKey)
			if decodeErr != nil {
				encryptionKeyBytes = []byte(encryptionKey)
			}
			// Ensure key is 32 bytes for AES-256
			if len(encryptionKeyBytes) < 32 {
				paddedKey := make([]byte, 32)
				copy(paddedKey, encryptionKeyBytes)
				encryptionKeyBytes = paddedKey
			} else if len(encryptionKeyBytes) > 32 {
				encryptionKeyBytes = encryptionKeyBytes[:32]
			}
		} else {
			encryptionKeyBytes = make([]byte, 32)
		}

		// Initialize crypto service
		cryptoService, err := crypto.NewCryptoService(encryptionKeyBytes)
		if err != nil {
			logger.Warnf("Failed to initialize crypto service for scheduler: %v", err)
		}

		// Initialize inventory source service
		inventorySourceService := ansible.NewInventorySourceService(
			inventorySourceRepo,
			ansibleInventoryRepo,
			ansibleCredentialRepo,
			cryptoService,
		)

		// Wire OIDC workload identity support for Azure inventory sync
		azureOIDCRepo := repository.NewAzureOIDCConfigurationRepository(db)
		oidcSigningKey, oidcErr := oidc.NewSigningKey()
		if oidcErr != nil {
			logger.Warnf("Failed to initialize OIDC signing key for inventory sync: %v (OIDC auth will be disabled)", oidcErr)
		} else {
			issuerURL := os.Getenv("OIDC_ISSUER_URL")
			if issuerURL == "" {
				issuerURL = os.Getenv("API_URL")
			}
			if issuerURL == "" {
				issuerURL = "http://localhost:8022"
			}
			oidcTokenService := oidc.NewTokenService(oidcSigningKey, issuerURL)
			inventorySourceService.SetOIDCServices(azureOIDCRepo, oidcTokenService)
			logger.Info("OIDC workload identity enabled for Azure inventory sync")
		}

		// Initialize variable service for Ansible (with variable sets, workspace, and template variable support)
		workspaceRepoForAnsible := repository.NewWorkspaceRepository(db)
		variableRepoForAnsible := repository.NewVariableRepository(db)
		variableSetRepoForAnsible := repository.NewVariableSetRepository(db)
		templateVariableRepoForAnsible := repository.NewAnsibleJobTemplateVariableRepository(db)
		variableServiceForAnsible := variable.NewServiceWithTemplateVariables(variableRepoForAnsible, variableSetRepoForAnsible, workspaceRepoForAnsible, templateVariableRepoForAnsible, encryptionKeyBytes)

		// Initialize job service with variable set support (nil queue for now - jobs handled by scheduler)
		jobService := ansible.NewJobServiceWithVariables(
			ansibleJobRepo,
			ansiblePlaybookRepo,
			ansibleInventoryRepo,
			ansibleTemplateRepo,
			projectRepo,
			variableServiceForAnsible,
			nil, // No queue - scheduler creates jobs directly
		)

		// Create scheduler service
		schedulerService = ansible.NewSchedulerService(
			scheduleRepo,
			ansibleJobRepo,
			ansibleTemplateRepo,
			ansiblePlaybookRepo,
			inventorySourceService,
			jobService,
		)

		// Start the scheduler
		schedulerService.Start()
		logger.Info("Ansible Scheduler Service started")
	} else {
		logger.Info("Ansible Scheduler Service disabled (set ANSIBLE_SCHEDULER_ENABLED=true to enable)")
	}

	// Initialize Drift Detection Service for Terraform workspaces
	var driftDetectionService *terraform.DriftDetectionService
	driftDetectionEnabled := os.Getenv("TERRAFORM_DRIFT_DETECTION_ENABLED") != "false" // Enabled by default

	if driftDetectionEnabled {
		workspaceRepo := repository.NewWorkspaceRepository(db)
		runRepo := repository.NewRunRepository(db)
		configVersionRepo := repository.NewConfigurationVersionRepository(db)

		driftDetectionService = terraform.NewDriftDetectionService(
			workspaceRepo,
			runRepo,
			configVersionRepo,
		)

		// Start the drift detection service
		driftDetectionService.Start()
		logger.Info("Terraform Drift Detection Service started")
	} else {
		logger.Info("Terraform Drift Detection Service disabled (set TERRAFORM_DRIFT_DETECTION_ENABLED=true to enable)")
	}

	// Initialize and start Runner Monitor Service (marks stale runners as offline)
	runnerMonitorEnabled := os.Getenv("RUNNER_MONITOR_ENABLED") != "false" // Enabled by default
	var runnerMonitorService *runner.MonitorService
	if runnerMonitorEnabled {
		runnerRepo := repository.NewRunnerRepository(db)
		runnerMonitorService = runner.NewMonitorService(runnerRepo)
		go runnerMonitorService.Start(context.Background())
		logger.Info("Runner Monitor Service started")
	} else {
		logger.Info("Runner Monitor Service disabled (set RUNNER_MONITOR_ENABLED=true to enable)")
	}

	// Setup routes
	router := routes.SetupRoutes(db, authService, totpService, profileService, sessionsService, apiKeyService, githubAppManager)

	// Create HTTP server
	addr := fmt.Sprintf("%s:%d", config.Server.Host, config.Server.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  config.Server.ReadTimeout,
		WriteTimeout: config.Server.WriteTimeout,
	}

	// Start server in goroutine
	go func() {
		logger.Infof("Server starting on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("Server failed to start: %v", err)
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("Shutting down server...")

	// Stop scheduler service if running
	if schedulerService != nil {
		schedulerService.Stop()
		logger.Info("Ansible Scheduler Service stopped")
	}

	// Stop drift detection service if running
	if driftDetectionService != nil {
		driftDetectionService.Stop()
		logger.Info("Terraform Drift Detection Service stopped")
	}

	// Stop runner monitor service if running
	if runnerMonitorService != nil {
		runnerMonitorService.Stop()
		logger.Info("Runner Monitor Service stopped")
	}

	// Graceful shutdown
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	if err := srv.Shutdown(ctx); err != nil {
		cancel()
		logger.Fatalf("Server forced to shutdown: %v", err)
	}
	cancel()

	logger.Info("Server exited")
}

// defaultConfig returns a Config with sensible defaults matching the values
// in config/config.yaml. This ensures the binary works without a config file
// when all required values are supplied via environment variables.
func defaultConfig() Config {
	var cfg Config
	cfg.Server.Host = "0.0.0.0"
	cfg.Server.Port = 8022
	cfg.Server.ReadTimeout = 30 * time.Second
	cfg.Server.WriteTimeout = 30 * time.Second
	cfg.Database.Port = 5432
	cfg.Database.SSLMode = "disable"
	cfg.Database.MaxOpenConns = 25
	cfg.Database.MaxIdleConns = 5
	cfg.Database.ConnMaxLifetime = 5 * time.Minute
	return cfg
}

// applyEnvOverrides overrides config.yaml values with environment variables when set.
// This allows Kubernetes pods to inject configuration via env vars without modifying config.yaml.
func applyEnvOverrides(config *Config) {
	// Server
	if v := os.Getenv("SERVER_HOST"); v != "" {
		config.Server.Host = v
	}
	if v := os.Getenv("SERVER_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			config.Server.Port = p
		}
	}
	if v := os.Getenv("SERVER_READ_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			config.Server.ReadTimeout = d
		}
	}
	if v := os.Getenv("SERVER_WRITE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			config.Server.WriteTimeout = d
		}
	}

	// Database
	if v := os.Getenv("DATABASE_HOST"); v != "" {
		config.Database.Host = v
	}
	if v := os.Getenv("DATABASE_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			config.Database.Port = p
		}
	}
	if v := os.Getenv("DATABASE_USER"); v != "" {
		config.Database.User = v
	}
	if v := os.Getenv("DATABASE_PASSWORD"); v != "" {
		config.Database.Password = v
	}
	if v := os.Getenv("DATABASE_NAME"); v != "" {
		config.Database.DBName = v
	}
	if v := os.Getenv("DATABASE_SSLMODE"); v != "" {
		config.Database.SSLMode = v
	}
	if v := os.Getenv("DATABASE_MAX_OPEN_CONNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			config.Database.MaxOpenConns = n
		}
	}
	if v := os.Getenv("DATABASE_MAX_IDLE_CONNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			config.Database.MaxIdleConns = n
		}
	}
	if v := os.Getenv("DATABASE_CONN_MAX_LIFETIME"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			config.Database.ConnMaxLifetime = d
		}
	}

	// Zitadel (also overridden later in main via os.Getenv, kept here for completeness)
	if v := os.Getenv("ZITADEL_ISSUER"); v != "" {
		config.Zitadel.Issuer = v
	}
	if v := os.Getenv("ZITADEL_API_CLIENT_ID"); v != "" {
		config.Zitadel.ClientID = v
	}
	if v := os.Getenv("ZITADEL_API_CLIENT_SECRET"); v != "" {
		config.Zitadel.ClientSecret = v
	}
}
