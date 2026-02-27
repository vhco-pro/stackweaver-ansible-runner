// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/plugins/terraform"
	"github.com/iac-platform/backend/internal/queue"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/logbuffer"
	"github.com/iac-platform/backend/internal/services/logparser"
	"github.com/iac-platform/backend/internal/services/oidc"
	"github.com/iac-platform/backend/internal/services/state"
	"github.com/iac-platform/backend/internal/services/variable"
	"github.com/iac-platform/backend/internal/storage"
	"github.com/michielvha/logger"
)

// getEnv returns the value of an environment variable or a fallback default.
func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

// getEnvInt returns the integer value of an environment variable or a fallback default.
func getEnvInt(key string, fallback int) int {
	if value, ok := os.LookupEnv(key); ok {
		if i, err := strconv.Atoi(value); err == nil {
			return i
		}
	}
	return fallback
}

type Job struct {
	RunID       string `json:"run_id"`
	WorkspaceID string `json:"workspace_id"`
	Operation   string `json:"operation"`
}

// createCancellableContext wraps a context with database polling to detect cancellation
// It polls the database every 2 seconds to check if the run has been cancelled
// If cancelled, it cancels the context which will kill the terraform process
// Returns the cancellable context and a cancel function
func createCancellableContext(ctx context.Context, runRepo *repository.RunRepository, runID string) (context.Context, context.CancelFunc) {
	cancelCtx, cancel := context.WithCancel(ctx)

	go func() {
		ticker := time.NewTicker(2 * time.Second) // Check every 2 seconds
		defer ticker.Stop()

		for {
			select {
			case <-cancelCtx.Done():
				// Context was cancelled (timeout or parent cancelled), stop polling
				return
			case <-ticker.C:
				// Poll database to check if run was cancelled
				run, err := runRepo.GetByID(runID)
				if err != nil {
					// If we can't read the run, log but continue polling
					logger.Warnf("Failed to check cancellation status for run %s: %v", runID, err)
					continue
				}
				if run.Status == models.RunStatusCancelled {
					logger.Infof("Run %s was cancelled, cancelling context to stop terraform process", runID)
					cancel() // Cancel the context, which will kill terraform process
					return
				}
			}
		}
	}()

	return cancelCtx, cancel
}

func main() {
	// Initialize logger first (reads LOG_LEVEL from environment)
	logLevel := os.Getenv("LOG_LEVEL")
	logger.Init(logLevel)

	// Check if running in agent mode (self-hosted runner)
	if os.Getenv("RUNNER_MODE") == "agent" {
		logger.Info("Starting Terraform runner in agent mode...")
		RunAgentMode()
		return
	}

	// Initialize dependencies (platform-hosted mode)
	redisQueue, err := queue.NewRedisQueue(
		getEnv("REDIS_HOST", "localhost"),
		getEnvInt("REDIS_PORT", 6379),
		os.Getenv("REDIS_PASSWORD"),
		getEnvInt("REDIS_DB", 0),
	)
	if err != nil {
		logger.Fatalf("Failed to connect to Redis: %v", err)
	}
	defer func() {
		if err := redisQueue.Close(); err != nil {
			logger.Warnf("Failed to close Redis queue: %v", err)
		}
	}()

	db, err := repository.NewDatabase(repository.Config{
		Host:            getEnv("DATABASE_HOST", "localhost"),
		Port:            getEnvInt("DATABASE_PORT", 5432),
		User:            getEnv("DATABASE_USER", "iac"),
		Password:        getEnv("DATABASE_PASSWORD", "iac_password"),
		DBName:          getEnv("DATABASE_NAME", "iac_platform"),
		SSLMode:         getEnv("DATABASE_SSLMODE", "disable"),
		MaxOpenConns:    10,
		MaxIdleConns:    5,
		ConnMaxLifetime: 5 * time.Minute,
	})
	if err != nil {
		// Close Redis queue before exiting
		if closeErr := redisQueue.Close(); closeErr != nil {
			logger.Warnf("Failed to close Redis queue before exit: %v", closeErr)
		}
		//nolint:gocritic // False positive: we explicitly close redisQueue before logger.Fatalf
		logger.Fatalf("Failed to connect to database: %v", err)
	}

	// Initialize storage clients
	storageEndpoint := getEnv("STORAGE_ENDPOINT", "localhost:9000")
	storageAccessKey := getEnv("STORAGE_ACCESS_KEY", "minioadmin")
	storageSecretKey := getEnv("STORAGE_SECRET_KEY", "minioadmin")
	storageUseSSL := getEnv("STORAGE_USE_SSL", "false") == "true"

	// State storage (for state files)
	stateBucket := getEnv("STATE_BUCKET", "iac-state")
	stateStorageClient, err := storage.NewMinIOClient(storageEndpoint, storageAccessKey, storageSecretKey, stateBucket, storageUseSSL)
	if err != nil {
		logger.Fatalf("Failed to connect to state storage: %v", err)
	}

	// Configuration storage (for configuration files - config.tar.gz)
	// Must match the bucket used by the API (terraform-registry by default)
	configStorageBucket := os.Getenv("STORAGE_BUCKET")
	if configStorageBucket == "" {
		configStorageBucket = "terraform-registry" // Default bucket (matches API default)
	}
	configStorageClient, err := storage.NewMinIOClient(storageEndpoint, storageAccessKey, storageSecretKey, configStorageBucket, storageUseSSL)
	if err != nil {
		logger.Fatalf("Failed to connect to config storage: %v", err)
	}

	// Initialize Redis log buffer service (reuse Redis connection from queue)
	logBufferService := logbuffer.NewRedisLogBuffer(redisQueue.Client())

	// Initialize repositories and services
	workspaceRepo := repository.NewWorkspaceRepository(db)
	runRepo := repository.NewRunRepository(db)
	configVersionRepo := repository.NewConfigurationVersionRepository(db)
	phaseStateRepo := repository.NewRunPhaseStateRepository(db)
	stateVersionRepo := repository.NewStateVersionRepository(db)
	stateLockRepo := repository.NewStateLockRepository(db)
	varRepo := repository.NewVariableRepository(db)
	variableSetRepo := repository.NewVariableSetRepository(db)

	stateService := state.NewService(stateVersionRepo, stateLockRepo, workspaceRepo, stateStorageClient)

	// Get encryption key for variables (match API handling)
	encryptionKeyStr := os.Getenv("ENCRYPTION_KEY")
	var encryptionKey []byte
	if encryptionKeyStr != "" {
		var decodeErr error
		encryptionKey, decodeErr = hex.DecodeString(encryptionKeyStr)
		if decodeErr != nil {
			logger.Warn("Failed to decode encryption key as hex, using raw bytes")
			encryptionKey = []byte(encryptionKeyStr)
		}
		// Ensure key is 32 bytes for AES-256
		if len(encryptionKey) < 32 {
			paddedKey := make([]byte, 32)
			copy(paddedKey, encryptionKey)
			encryptionKey = paddedKey
		} else if len(encryptionKey) > 32 {
			encryptionKey = encryptionKey[:32]
		}
	} else {
		logger.Fatal("ENCRYPTION_KEY environment variable is required")
	}
	variableService := variable.NewServiceWithVariableSetsAndWorkspace(varRepo, variableSetRepo, workspaceRepo, encryptionKey)

	// OIDC Workload Identity: Initialize signing key and token service for Azure OIDC
	azureOIDCRepo := repository.NewAzureOIDCConfigurationRepository(db)
	oidcSigningKey, oidcErr := oidc.NewSigningKey()
	var oidcTokenService *oidc.TokenService
	if oidcErr != nil {
		logger.Warnf("Failed to initialize OIDC signing key: %v (OIDC workload identity will be disabled)", oidcErr)
	} else {
		issuerURL := os.Getenv("OIDC_ISSUER_URL")
		if issuerURL == "" {
			issuerURL = os.Getenv("API_URL")
		}
		if issuerURL == "" {
			issuerURL = "http://localhost:8022"
		}
		oidcTokenService = oidc.NewTokenService(oidcSigningKey, issuerURL)
		logger.Info("OIDC workload identity token service initialized")
	}

	// Start worker
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		logger.Info("Runner started, waiting for jobs...")
		for {
			select {
			case <-ctx.Done():
				return
			default:
				if err := processJob(ctx, redisQueue, logBufferService, workspaceRepo, runRepo, configVersionRepo, phaseStateRepo, configStorageClient, stateService, variableService, azureOIDCRepo, oidcTokenService); err != nil {
					if err != queue.ErrQueueEmpty {
						logger.Errorf("Error processing job: %v", err)
					}
					time.Sleep(1 * time.Second)
				}
			}
		}
	}()

	// Wait for interrupt
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	logger.Info("Shutting down runner...")
	cancel()
}

func processJob(
	ctx context.Context,
	redisQueue *queue.RedisQueue,
	logBufferService *logbuffer.RedisLogBuffer,
	workspaceRepo *repository.WorkspaceRepository,
	runRepo *repository.RunRepository,
	configVersionRepo *repository.ConfigurationVersionRepository,
	phaseStateRepo *repository.RunPhaseStateRepository,
	configStorageClient storage.Client,
	stateService *state.Service,
	variableService *variable.Service,
	azureOIDCRepo *repository.AzureOIDCConfigurationRepository,
	oidcTokenService *oidc.TokenService,
) error {
	// Dequeue job
	jobData, err := redisQueue.Dequeue(ctx, "runs", 5*time.Second)
	if err != nil {
		return err
	}

	var job Job
	if err := json.Unmarshal(jobData, &job); err != nil {
		return fmt.Errorf("failed to unmarshal job: %w", err)
	}

	logger.Infof("Processing job: RunID=%s, WorkspaceID=%s, Operation=%s", job.RunID, job.WorkspaceID, job.Operation)

	// Get run
	run, err := runRepo.GetByID(job.RunID)
	if err != nil {
		return fmt.Errorf("failed to get run: %w", err)
	}

	// Check if run was cancelled before starting
	if run.Status == models.RunStatusCancelled {
		logger.Infof("Run %s was cancelled before execution, skipping", run.ID)
		return nil
	}

	// Update run status based on operation and current phase
	now := time.Now()

	// Determine appropriate status based on run operation and current phase
	switch run.Operation {
	case models.RunOperationPlanAndApply:
		// Plan-and-apply run: Check current phase
		if run.Status == models.RunStatusPending {
			// Starting plan phase
			run.Status = models.RunStatusPlanning
		}
		// Note: If status is already RunStatusApplying, we keep it as is
	case models.RunOperationPlanOnly:
		// Plan-only run: Set to planning
		if run.Status == models.RunStatusPending {
			run.Status = models.RunStatusPlanning
		}
	case models.RunOperationDestroy:
		// Destroy run: Set to planning (destroy uses plan phase)
		if run.Status == models.RunStatusPending {
			run.Status = models.RunStatusPlanning
		}
	default:
		// Unknown operation type - log warning and set to planning as fallback
		logger.Warnf("Unknown run operation type %s for run %s, defaulting to planning status", run.Operation, run.ID)
		if run.Status == models.RunStatusPending {
			run.Status = models.RunStatusPlanning
		}
	}

	// Set started time if not already set
	if run.StartedAt == nil {
		run.StartedAt = &now
	}
	if err := runRepo.Update(run); err != nil {
		return fmt.Errorf("failed to update run: %w", err)
	}

	// Get workspace
	workspace, err := workspaceRepo.GetByID(job.WorkspaceID)
	if err != nil {
		return fmt.Errorf("failed to get workspace: %w", err)
	}

	// Check if workspace is manually locked (TFE-compatible)
	if workspace.Locked {
		run.Status = models.RunStatusFailed
		reason := "Workspace is manually locked. Unlock the workspace to allow runs."
		if workspace.LockedReason != "" {
			reason = fmt.Sprintf("Workspace is manually locked: %s", workspace.LockedReason)
		}
		run.ErrorMessage = reason
		now := time.Now()
		run.CompletedAt = &now
		if err := runRepo.Update(run); err != nil {
			logger.Warnf("Failed to update run status: %v", err)
		}
		return fmt.Errorf("workspace is manually locked")
	}

	// Acquire state lock for apply/destroy operations (TFE-compatible)
	// Use defer to ensure lock is released even if function returns early
	var lockID string
	if run.Operation == models.RunOperationPlanAndApply || run.Operation == models.RunOperationDestroy {
		lockID = fmt.Sprintf("run-%s", run.ID)
		ttl := time.Duration(workspace.RunTimeout) * time.Second
		if ttl == 0 {
			ttl = 2 * time.Hour // Default TTL
		}

		// Try to acquire lock
		if err := stateService.LockState(ctx, job.WorkspaceID, lockID, string(run.Operation), &run.ID, ttl); err != nil {
			// Check if lock exists and is not expired
			existingLock, lockErr := stateService.GetStateLock(ctx, job.WorkspaceID)
			if lockErr == nil && existingLock != nil && !existingLock.IsExpired() {
				run.Status = models.RunStatusFailed
				run.ErrorMessage = fmt.Sprintf("State is locked by another operation (lock ID: %s)", existingLock.LockID)
				now := time.Now()
				run.CompletedAt = &now
				if updateErr := runRepo.Update(run); updateErr != nil {
					logger.Warnf("Failed to update run status: %v", updateErr)
				}
				lockedByStr := "unknown"
				if existingLock.LockedBy != nil {
					lockedByStr = *existingLock.LockedBy
				}
				return fmt.Errorf("failed to acquire state lock: state is locked by run %s", lockedByStr)
			}
			// If expired or doesn't exist, try again
			if retryErr := stateService.LockState(ctx, job.WorkspaceID, lockID, string(run.Operation), &run.ID, ttl); retryErr != nil {
				run.Status = models.RunStatusFailed
				run.ErrorMessage = fmt.Sprintf("Failed to acquire state lock: %s", retryErr.Error())
				now := time.Now()
				run.CompletedAt = &now
				if updateErr := runRepo.Update(run); updateErr != nil {
					logger.Warnf("Failed to update run status: %v", updateErr)
				}
				return fmt.Errorf("failed to acquire state lock: %w", retryErr)
			}
		}
		logger.Infof("State lock acquired for run %s (lock ID: %s)", run.ID, lockID)

		// Defer lock release to ensure cleanup
		defer func() {
			if unlockErr := stateService.UnlockState(ctx, job.WorkspaceID, lockID); unlockErr != nil {
				logger.Warnf("Failed to release state lock for run %s: %v", run.ID, unlockErr)
			} else {
				logger.Infof("State lock released for run %s", run.ID)
			}
		}()
	}

	// Create workspace directory
	// Use /home/iac/workspaces for non-root user compatibility
	workspaceDir := fmt.Sprintf("/home/iac/workspaces/%s", workspace.ID)
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil { //nolint:gosec // workspace directories need 0o755 for compatibility
		return fmt.Errorf("failed to create workspace directory: %w", err)
	}

	// TFE-compatible: Download configuration files from MinIO if configuration version exists
	// This is the primary method - configuration files are uploaded via PUT /api/v2/configuration-versions/:id/upload
	var configVersion *models.ConfigurationVersion
	if run.ConfigurationVersionID != nil {
		var err error
		configVersion, err = configVersionRepo.GetByID(*run.ConfigurationVersionID)
		if err != nil {
			run.Status = models.RunStatusFailed
			run.ErrorMessage = fmt.Sprintf("Failed to get configuration version: %v", err)
			run.CompletedAt = &now
			if err := runRepo.Update(run); err != nil {
				logger.Warnf("Failed to update run status: %v", err)
			}
			return fmt.Errorf("failed to get configuration version: %w", err)
		}

		// Check if configuration version is uploaded
		if configVersion.Status != models.ConfigurationVersionStatusUploaded {
			run.Status = models.RunStatusFailed
			run.ErrorMessage = fmt.Sprintf("Configuration version not uploaded (status: %s)", configVersion.Status)
			run.CompletedAt = &now
			if err := runRepo.Update(run); err != nil {
				logger.Warnf("Failed to update run status: %v", err)
			}
			return fmt.Errorf("configuration version not uploaded")
		}

		// Download configuration files from MinIO
		// Path: configuration-versions/{config_version_id}/config.tar.gz
		configStorageKey := fmt.Sprintf("configuration-versions/%s/config.tar.gz", configVersion.ID)
		logger.Infof("Downloading configuration from MinIO: %s", configStorageKey)

		configData, err := configStorageClient.Get(ctx, configStorageKey)
		if err != nil {
			run.Status = models.RunStatusFailed
			run.ErrorMessage = fmt.Sprintf("Failed to download configuration files: %v", err)
			run.CompletedAt = &now
			if err := runRepo.Update(run); err != nil {
				logger.Warnf("Failed to update run status: %v", err)
			}
			return fmt.Errorf("failed to download configuration files: %w", err)
		}

		// Extract tar.gz to workspace directory
		logger.Infof("Extracting configuration files to %s", workspaceDir)
		if err := extractTarGz(configData, workspaceDir); err != nil {
			run.Status = models.RunStatusFailed
			run.ErrorMessage = fmt.Sprintf("Failed to extract configuration files: %v", err)
			run.CompletedAt = &now
			if err := runRepo.Update(run); err != nil {
				logger.Warnf("Failed to update run status: %v", err)
			}
			return fmt.Errorf("failed to extract configuration files: %w", err)
		}
		logger.Infof("Configuration files extracted successfully")

	} else {
		// Fallback: If no configuration version, allow the run to proceed
		// This is for backward compatibility and manual runs from the UI
		// TFE primarily uses configuration versions, but we allow manual runs without them
		logger.Warn("Run has no configuration version, proceeding without configuration files")
		// Note: The workspace directory will be empty, which is fine for manual runs
		// where configuration might be provided via other means or the workspace might
		// be configured to use VCS directly
	}

	// Handle working directory (TFE-compatible)
	// If workspace has a working directory specified, use that subdirectory
	terraformDir := workspaceDir
	if workspace.WorkingDirectory != "" && workspace.WorkingDirectory != "." && workspace.WorkingDirectory != "/" {
		// For VCS-triggered runs, tarball contains full repository structure from root
		// For manual uploads, tarball may contain only the working directory
		if workspace.VCSConnectionID != nil && workspace.VCSRepository != "" {
			// VCS-triggered: tarball contains full repo structure, working directory is a subdirectory
			terraformDir = filepath.Join(workspaceDir, strings.TrimPrefix(workspace.WorkingDirectory, "/"))
			logger.Infof("Using working directory (VCS-triggered, full repo structure): %s", terraformDir)
		} else {
			// Manual upload: append working directory to find files
			terraformDir = filepath.Join(workspaceDir, strings.TrimPrefix(workspace.WorkingDirectory, "/"))
			logger.Infof("Using working directory: %s", terraformDir)
		}

		// Verify working directory exists
		if _, err := os.Stat(terraformDir); os.IsNotExist(err) {
			run.Status = models.RunStatusFailed
			run.ErrorMessage = fmt.Sprintf("Working directory not found: %s (resolved to: %s)", workspace.WorkingDirectory, terraformDir)
			run.CompletedAt = &now
			if err := runRepo.Update(run); err != nil {
				logger.Warnf("Failed to update run status: %v", err)
			}
			return fmt.Errorf("working directory not found: %s (resolved to: %s)", workspace.WorkingDirectory, terraformDir)
		}
	}

	// TFE-compatible: Replace remote backend with local backend for runner execution
	// When runs are executed remotely, the backend should use local state or state service
	// This prevents the runner from creating nested runs (infinite loop)
	// IMPORTANT: Do this BEFORE init, so init uses the correct backend
	if err := replaceRemoteBackendWithLocal(terraformDir); err != nil {
		logger.Warnf("Failed to replace remote backend: %v (continuing anyway)", err)
	}

	// Get terraform variables (category == "terraform") - these go in stackweaver.auto.tfvars
	variables, err := variableService.GetVariablesForRun(ctx, workspace.ID)
	if err != nil {
		// Update run status to failed if variable retrieval fails
		run.Status = models.RunStatusFailed
		run.ErrorMessage = fmt.Sprintf("Failed to get variables: %v", err)
		run.CompletedAt = &now
		if updateErr := runRepo.Update(run); updateErr != nil {
			logger.Warnf("Failed to update run status: %v", updateErr)
		}
		return fmt.Errorf("failed to get variables: %w", err)
	}

	// Get environment variables (category == "env") - these are set as actual environment variables
	// TFE-compatible: Environment variables are not included in stackweaver.auto.tfvars
	envVars, err := variableService.GetEnvironmentVariablesForRun(ctx, workspace.ID)
	if err != nil {
		return fmt.Errorf("failed to get environment variables: %w", err)
	}

	// OIDC Workload Identity: If Azure OIDC configurations exist for this organization,
	// generate a signed JWT and inject environment variables for Azure authentication.
	// This enables keyless authentication from Terraform runs to Azure.
	if azureOIDCRepo != nil && oidcTokenService != nil {
		orgID := workspace.Project.OrganizationID
		configs, oidcErr := azureOIDCRepo.GetByOrganization(orgID)
		if oidcErr != nil {
			logger.Warnf("Failed to look up Azure OIDC configurations for org %s: %v", orgID, oidcErr)
		} else if len(configs) > 0 {
			// Use the first OIDC configuration (an org typically has one Azure OIDC config)
			config := configs[0]

			// Determine the run phase
			runPhase := "plan"
			if run.Status == models.RunStatusApplying {
				runPhase = "apply"
			}

			orgName := workspace.Project.Organization.Name
			projectName := workspace.Project.Name

			// Generate OIDC token with audience set to the Azure client ID
			// TFC-compatible: the audience is "api://AzureADTokenExchange" by default,
			// but we use the client ID which is what Azure federated credentials expect
			token, tokenErr := oidcTokenService.GenerateToken(
				"api://AzureADTokenExchange",
				orgName,
				projectName,
				workspace.Name,
				run.ID,
				runPhase,
			)
			if tokenErr != nil {
				logger.Warnf("Failed to generate OIDC token for run %s: %v", run.ID, tokenErr)
			} else {
				// Inject TFC-compatible workload identity env vars
				envVars["TFC_WORKLOAD_IDENTITY_TOKEN"] = token

				// Inject Azure-specific env vars for the AzureRM/AzAPI Terraform providers
				envVars["ARM_OIDC_TOKEN"] = token
				envVars["ARM_CLIENT_ID"] = config.ClientID
				envVars["ARM_SUBSCRIPTION_ID"] = config.SubscriptionID
				envVars["ARM_TENANT_ID"] = config.TenantID
				envVars["ARM_USE_OIDC"] = "true"

				logger.Infof("Injected OIDC workload identity token for run %s (org=%s, workspace=%s)", run.ID, orgName, workspace.Name)
			}
		}
	}

	// Resolve terraform version: workspace -> org default
	terraformVersion := workspace.TerraformVersion
	if terraformVersion == "" {
		terraformVersion = workspace.Project.Organization.DefaultTerraformVersion
	}
	if terraformVersion == "" {
		run.Status = models.RunStatusFailed
		run.ErrorMessage = "No Terraform version configured. Set a version on the workspace or set an organization default in Settings > Terraform Versions."
		now := time.Now()
		run.CompletedAt = &now
		if err := runRepo.Update(run); err != nil {
			logger.Warnf("Failed to update run status: %v", err)
		}
		return fmt.Errorf("no terraform version configured for workspace %s", workspace.Name)
	}
	logger.Infof("Using Terraform version %s for workspace %s", terraformVersion, workspace.Name)

	// Initialize Terraform plugin (may download binary if not cached locally)
	plugin := terraform.NewPlugin(terraformVersion) //nolint:contextcheck // download happens once at init, not per-request

	// Helper function to store logs to MinIO (for long-term persistence)
	// Logs should already be in Redis (streamed during execution), this copies them to MinIO
	storeLogs := func(logs string, phase string) {
		var logsKey string
		if run.Operation == models.RunOperationPlanAndApply {
			// For plan-and-apply runs, use phase-specific keys: plan.log or apply.log
			logsKey = fmt.Sprintf("runs/%s/logs/%s.log", run.ID, phase)
		} else {
			// For other runs, use operation name
			logsKey = fmt.Sprintf("runs/%s/logs/%s.log", run.ID, job.Operation)
		}
		if err := configStorageClient.Put(ctx, logsKey, []byte(logs)); err != nil {
			logger.Warnf("Failed to store logs to MinIO for run %s: %v", run.ID, err)
		}
	}

	// Helper function to copy logs from Redis to MinIO (called after completion)
	copyLogsFromRedisToMinIO := func(phase string) {
		if err := logBufferService.CopyToMinIO(ctx, run.ID, phase, configStorageClient); err != nil {
			logger.Warnf("Failed to copy logs from Redis to MinIO for run %s phase %s: %v", run.ID, phase, err)
		}
	}

	// Helper function to store parsed phase state in database
	storePhaseState := func(phase string) {
		// Get logs from MinIO
		logsKey := fmt.Sprintf("runs/%s/logs/%s.log", run.ID, phase)
		logsBytes, err := configStorageClient.Get(ctx, logsKey)
		if err != nil {
			logger.Warnf("Failed to get logs from MinIO for run %s phase %s: %v", run.ID, phase, err)
			return
		}
		logs := string(logsBytes)

		// For apply phase, extract planned resources from plan output
		var plannedResources []logparser.PlannedResource
		if phase == "apply" && run.PlanOutput != nil {
			plannedResources = logparser.ExtractPlannedResourcesFromPlanOutput(map[string]interface{}(run.PlanOutput))
		}

		// Parse logs
		parseResult, err := logparser.ParseApplyLogs(logs, plannedResources)
		if err != nil {
			logger.Warnf("Failed to parse logs for run %s phase %s: %v", run.ID, phase, err)
			return
		}

		// Store phase state
		phaseState := &models.RunPhaseState{
			RunID:     run.ID,
			Phase:     phase,
			Resources: parseResult.Resources,
			Summary:   parseResult.Summary,
			ParsedAt:  time.Now(),
		}

		if err := phaseStateRepo.Upsert(phaseState); err != nil {
			logger.Warnf("Failed to store phase state for run %s phase %s: %v", run.ID, phase, err)
		} else {
			logger.Infof("Stored phase state for run %s phase %s (%d resources)", run.ID, phase, len(parseResult.Resources))
		}
	}

	// TFE-compatible: Initialize Terraform (after backend replacement)
	// This ensures providers are downloaded and backend is configured correctly
	// Add timeout for init to prevent hanging (15 minutes should be enough for provider downloads)
	initTimeout := 15 * time.Minute
	initCtx, initCancel := context.WithTimeout(ctx, initTimeout)
	defer initCancel()

	logger.Infof("Starting terraform init for run %s (operation: %s)", run.ID, run.Operation)
	initResult, err := plugin.Init(initCtx, terraformDir, nil, envVars)
	if err != nil {
		// Store init logs even on failure
		if initResult != nil {
			storeLogs(initResult.Logs, "init")
		}
		// Check if timeout was exceeded
		if initCtx.Err() == context.DeadlineExceeded {
			run.Status = models.RunStatusFailed
			run.ErrorMessage = fmt.Sprintf("Terraform init exceeded timeout of %v", initTimeout)
		} else {
			run.Status = models.RunStatusFailed
			run.ErrorMessage = fmt.Sprintf("Terraform init failed: %v", err)
		}
		run.CompletedAt = &now
		if updateErr := runRepo.Update(run); updateErr != nil {
			logger.Warnf("Failed to update run status: %v", updateErr)
		}
		return err
	}
	// Store init logs
	if initResult != nil {
		storeLogs(initResult.Logs, "init")
	}
	logger.Infof("Terraform init completed successfully for run %s", run.ID)

	// Check if run was cancelled after init
	run, err = runRepo.GetByID(job.RunID)
	if err != nil {
		return fmt.Errorf("failed to reload run: %w", err)
	}
	if run.Status == models.RunStatusCancelled {
		logger.Infof("Run %s was cancelled after init, stopping execution", run.ID)
		return nil
	}

	// Execute operation and collect logs
	var operationLogs strings.Builder
	if initResult != nil {
		operationLogs.WriteString("=== Terraform Init ===\n")
		operationLogs.WriteString(initResult.Logs)
		operationLogs.WriteString("\n\n")
	}

	// Determine what phase to execute based on run operation and status
	// For plan-and-apply runs, check if we're in plan phase or apply phase
	executePlan := false
	executeApply := false

	switch run.Operation {
	case models.RunOperationPlanAndApply:
		// Plan-and-apply run: Check current phase
		switch run.Status {
		case models.RunStatusPlanning, models.RunStatusPending:
			// Execute plan phase
			executePlan = true
		case models.RunStatusApplying:
			// Execute apply phase
			executeApply = true
		case models.RunStatusPlanned, models.RunStatusApplied, models.RunStatusFailed, models.RunStatusCancelled, models.RunStatusRunning, models.RunStatusCompleted:
			// These statuses don't trigger execution - run is already in progress or completed
		}
	case models.RunOperationPlanOnly:
		// Plan-only run: Always execute plan
		executePlan = true
	case models.RunOperationDestroy:
		// TFE-compatible: Destroy runs follow the same two-phase flow as plan-and-apply
		// Phase 1: terraform plan -destroy (shows what will be destroyed)
		// Phase 2: terraform apply plan.out (actually destroys resources)
		switch run.Status { //nolint:exhaustive // only pending/planning/applying trigger execution
		case models.RunStatusPlanning, models.RunStatusPending:
			executePlan = true
		case models.RunStatusApplying:
			executeApply = true
		}
	}

	// Execute plan phase
	if executePlan {
		// Create timeout context for plan operation based on workspace timeout
		// Use half of apply timeout for plan, or minimum 30 minutes
		planTimeout := time.Duration(workspace.RunTimeout) * time.Second / 2
		if planTimeout <= 0 {
			planTimeout = 30 * time.Minute // Default 30 minutes for plan
		} else if planTimeout < 30*time.Minute {
			planTimeout = 30 * time.Minute // Minimum 30 minutes
		}
		planCtx, planCancel := context.WithTimeout(ctx, planTimeout)
		defer planCancel()

		// Wrap with cancellation polling to detect cancellation during execution
		cancellablePlanCtx, cancelPolling := createCancellableContext(planCtx, runRepo, run.ID)
		defer cancelPolling()

		logger.Infof("Starting plan operation with timeout of %v for run %s", planTimeout, run.ID)

		// Use streaming Plan with callback to write logs to Redis
		// For destroy runs, add -destroy flag to plan (TFE-compatible two-phase destroy)
		planOptions := &terraform.PlanOptions{
			OnOutputLine: func(line string) {
				if err := logBufferService.Append(cancellablePlanCtx, run.ID, "plan", line); err != nil {
					logger.Warnf("Failed to append plan log line to Redis: %v", err)
				}
			},
			Destroy: run.Operation == models.RunOperationDestroy,
		}
		planResult, err := plugin.PlanWithOptions(cancellablePlanCtx, terraformDir, variables, envVars, planOptions)
		if planResult != nil {
			operationLogs.WriteString("=== Terraform Plan ===\n")
			operationLogs.WriteString(planResult.Logs)
		}
		// ALWAYS copy logs from Redis to MinIO, even if cancelled
		// Logs are already in Redis from streaming callback, even if planResult is nil
		copyLogsFromRedisToMinIO("plan")
		// Check if run was cancelled during plan
		run, _ = runRepo.GetByID(job.RunID)
		if run.Status == models.RunStatusCancelled {
			logger.Infof("Run %s was cancelled during plan execution", run.ID)
			return nil
		}

		// Check if timeout was exceeded
		if planCtx.Err() == context.DeadlineExceeded {
			logger.Infof("Plan operation exceeded timeout of %v for run %s", planTimeout, run.ID)
			run.Status = models.RunStatusFailed
			run.ErrorMessage = fmt.Sprintf("Plan operation exceeded timeout of %v and was automatically cancelled", planTimeout)
			if err := runRepo.Update(run); err != nil {
				return fmt.Errorf("failed to update run status: %w", err)
			}
			return nil
		}

		if err != nil {
			// TFE-compatible: If plan fails with provider error, re-run init and retry plan
			// This matches Terraform CLI behavior where it automatically re-initializes on provider errors
			if providerErr, ok := err.(*terraform.ProviderError); ok {
				logger.Infof("Plan failed with provider error, re-running init: %v", providerErr)

				// Re-run init with upgrade to ensure providers are downloaded
				initResult, initErr := plugin.Init(ctx, terraformDir, nil, envVars)
				if initResult != nil {
					operationLogs.WriteString("\n=== Terraform Init (Retry) ===\n")
					operationLogs.WriteString(initResult.Logs)
					storeLogs(operationLogs.String(), "plan")
				}
				if initErr != nil {
					logger.Infof("Re-init failed: %v", initErr)
					run.Status = models.RunStatusFailed
					run.ErrorMessage = fmt.Sprintf("Provider initialization failed: %v (original error: %v)", initErr, providerErr)
					run.CompletedAt = &now
					if err := runRepo.Update(run); err != nil {
						logger.Warnf("Failed to update run status: %v", err)
					}
					return fmt.Errorf("re-init failed after provider error: %w", initErr)
				}

				// Retry plan after successful re-init
				logger.Infof("Retrying plan after successful re-init")
				retryPlanOptions := &terraform.PlanOptions{
					OnOutputLine: func(line string) {
						if err := logBufferService.Append(ctx, run.ID, "plan", line); err != nil {
							logger.Warnf("Failed to append plan log line to Redis (retry): %v", err)
						}
					},
					Destroy: run.Operation == models.RunOperationDestroy,
				}
				planResult, err = plugin.PlanWithOptions(ctx, terraformDir, variables, envVars, retryPlanOptions)
				if planResult != nil {
					operationLogs.WriteString("\n=== Terraform Plan (Retry) ===\n")
					operationLogs.WriteString(planResult.Logs)
					// Copy logs from Redis to MinIO for long-term persistence
					copyLogsFromRedisToMinIO("plan")
				}

				// Check cancellation again
				run, _ = runRepo.GetByID(job.RunID)
				if run.Status == models.RunStatusCancelled {
					logger.Infof("Run %s was cancelled during plan retry", run.ID)
					return nil
				}

				if err != nil {
					run.Status = models.RunStatusFailed
					run.ErrorMessage = err.Error()
					if updateErr := runRepo.Update(run); updateErr != nil {
						logger.Warnf("Failed to update run status: %v", updateErr)
					}
				} else {
					// Store plan output with computed counts
					planOutput := make(models.PlanOutput)
					if planResult.JSONOutput != nil {
						for k, v := range planResult.JSONOutput {
							planOutput[k] = v
						}
					}
					// Add computed counts to PlanOutput for status checks
					planOutput["AddCount"] = float64(planResult.AddCount)
					planOutput["ChangeCount"] = float64(planResult.ChangeCount)
					planOutput["DestroyCount"] = float64(planResult.DestroyCount)
					planOutput["OutputChangeCount"] = float64(planResult.OutputChangeCount)
					run.PlanOutput = planOutput
					// Check if plan has changes (including output-only changes)
					hasChanges := planResult.AddCount > 0 || planResult.ChangeCount > 0 || planResult.DestroyCount > 0 || planResult.OutputChangeCount > 0
					// Set status based on run operation type
					now := time.Now()
					switch run.Operation {
					case models.RunOperationPlanAndApply:
						// Plan-and-apply run: If no changes, mark as completed (finished)
						// Otherwise, set to "planned" (waiting for apply)
						if !hasChanges {
							run.Status = models.RunStatusCompleted
							run.PlanCompletedAt = &now
							run.CompletedAt = &now
						} else {
							run.Status = models.RunStatusPlanned
							run.PlanCompletedAt = &now // Track when plan phase completed
						}
					case models.RunOperationPlanOnly:
						run.Status = models.RunStatusPlanned
						run.PlanCompletedAt = &now // Track when plan phase completed
						run.CompletedAt = &now     // Also set CompletedAt for plan-only runs (plan is complete)
					case models.RunOperationDestroy:
						// Destroy runs follow same logic as plan-and-apply
						if !hasChanges {
							run.Status = models.RunStatusCompleted
							run.PlanCompletedAt = &now
							run.CompletedAt = &now
						} else {
							run.Status = models.RunStatusPlanned
							run.PlanCompletedAt = &now
						}
					}
					if updateErr := runRepo.Update(run); updateErr != nil {
						logger.Warnf("Failed to update run status: %v", updateErr)
					}
				}
			} else {
				// Non-provider error, fail normally
				run.Status = models.RunStatusFailed
				run.ErrorMessage = err.Error()
				if updateErr := runRepo.Update(run); updateErr != nil {
					logger.Warnf("Failed to update run status: %v", updateErr)
				}
			}
		} else {
			// Store plan output with computed counts
			planOutput := make(models.PlanOutput)
			if planResult.JSONOutput != nil {
				for k, v := range planResult.JSONOutput {
					planOutput[k] = v
				}
			}
			// Add computed counts to PlanOutput for status checks
			planOutput["AddCount"] = float64(planResult.AddCount)
			planOutput["ChangeCount"] = float64(planResult.ChangeCount)
			planOutput["DestroyCount"] = float64(planResult.DestroyCount)
			planOutput["OutputChangeCount"] = float64(planResult.OutputChangeCount)
			run.PlanOutput = planOutput

			// Check if plan has changes (including output-only changes)
			hasChanges := planResult.AddCount > 0 || planResult.ChangeCount > 0 || planResult.DestroyCount > 0 || planResult.OutputChangeCount > 0

			// Set status based on run operation type
			now := time.Now()
			switch run.Operation {
			case models.RunOperationPlanAndApply:
				// Plan-and-apply run: If no changes, mark as completed (finished)
				// Otherwise, set to "planned" (waiting for apply)
				if !hasChanges {
					run.Status = models.RunStatusCompleted
					run.PlanCompletedAt = &now
					run.CompletedAt = &now
				} else {
					run.Status = models.RunStatusPlanned
					run.PlanCompletedAt = &now // Track when plan phase completed
				}
			case models.RunOperationPlanOnly:
				// Plan-only run: Plan completed, set to "planned" (final state)
				run.Status = models.RunStatusPlanned
				run.PlanCompletedAt = &now // Track when plan phase completed
				run.CompletedAt = &now     // Also set CompletedAt for plan-only runs (plan is complete)
			case models.RunOperationDestroy:
				// Destroy runs follow same logic as plan-and-apply
				if !hasChanges {
					run.Status = models.RunStatusCompleted
					run.PlanCompletedAt = &now
					run.CompletedAt = &now
				} else {
					run.Status = models.RunStatusPlanned
					run.PlanCompletedAt = &now
				}
			}

			// Store parsed plan phase state for persistence
			if run.PlanCompletedAt != nil {
				storePhaseState("plan")
			}

			// Update run in database first
			if err := runRepo.Update(run); err != nil {
				return fmt.Errorf("failed to update run after plan: %w", err)
			}

			// TFE-compatible: Auto-apply logic
			// Only auto-apply for VCS-triggered configuration version runs with workspace.AutoApply enabled
			// Note: Run source is "tfe-configuration-version" for all runs created from configuration versions
			// We check the configuration version's source to determine if it's VCS-triggered
			// UI "Plan and Apply" runs should NOT auto-apply - they follow the 2-phase process:
			//   1. Plan runs and completes
			//   2. User sees plan output and clicks "Apply Plan" button
			//   3. Apply run is created via POST /api/v2/runs/:id/actions/apply
			// CLI runs should NEVER auto-apply (they're just for preview, prevents drift with git)
			//
			// The AutoApplyAfterPlan flag only indicates that this is a "plan-and-apply" run (not "plan-only"),
			// but it does NOT mean the run should auto-apply without user confirmation.
			// All UI-triggered runs require user confirmation via the Apply endpoint.
			if run.Operation == models.RunOperationPlanAndApply || run.Operation == models.RunOperationDestroy {
				// Only auto-apply if VCS-triggered and workspace has AutoApply enabled
				if run.ConfigurationVersionID != nil {
					configVersion, err := configVersionRepo.GetByID(*run.ConfigurationVersionID)
					if err == nil && configVersion != nil && configVersion.Source == "tfe-vcs" {
						if workspace.AutoApply {
							logger.Infof("Plan-and-apply run %s plan phase completed, transitioning to applying phase (VCS-triggered with workspace auto-apply)", run.ID)

							// Transition to applying phase (orchestrator will pick it up)
							now := time.Now()
							run.Status = models.RunStatusApplying
							run.ApplyStartedAt = &now // Track when apply phase started
							run.UpdatedAt = now
							if err := runRepo.Update(run); err != nil {
								logger.Warnf("Failed to transition run %s to applying phase: %v", run.ID, err)
							} else {
								logger.Infof("Run %s transitioned to applying phase, orchestrator will pick it up", run.ID)
							}
						} else {
							logger.Infof("Plan-and-apply run %s plan phase completed, waiting for user confirmation (VCS-triggered but workspace auto-apply disabled)", run.ID)
						}
					} else {
						logger.Infof("Plan-and-apply run %s plan phase completed, waiting for user confirmation (UI-triggered run)", run.ID)
					}
				} else {
					logger.Infof("Plan-and-apply run %s plan phase completed, waiting for user confirmation (UI-triggered run)", run.ID)
				}
			}
		}
	}

	// Execute apply phase (for plan-and-apply runs in applying status)
	if executeApply {
		// Create timeout context for apply operation based on workspace timeout
		applyTimeout := time.Duration(workspace.RunTimeout) * time.Second
		if applyTimeout <= 0 {
			// Default to 2 hours if not configured
			applyTimeout = 2 * time.Hour
		}
		applyCtx, applyCancel := context.WithTimeout(ctx, applyTimeout)
		defer applyCancel()

		// Wrap with cancellation polling to detect cancellation during execution
		cancellableApplyCtx, cancelPolling := createCancellableContext(applyCtx, runRepo, run.ID)
		defer cancelPolling()

		logger.Infof("Starting apply operation with timeout of %v for run %s", applyTimeout, run.ID)

		// Use streaming Apply with callback to write logs to Redis
		applyOptions := &terraform.ApplyOptions{
			OnOutputLine: func(line string) {
				if err := logBufferService.Append(cancellableApplyCtx, run.ID, "apply", line); err != nil {
					logger.Warnf("Failed to append apply log line to Redis: %v", err)
				}
			},
		}
		applyResult, err := plugin.ApplyWithOptions(cancellableApplyCtx, terraformDir, "plan.out", envVars, applyOptions)
		if applyResult != nil {
			operationLogs.WriteString("=== Terraform Apply ===\n")
			operationLogs.WriteString(applyResult.Logs)
		}
		// ALWAYS copy logs from Redis to MinIO, even if cancelled
		// Logs are already in Redis from streaming callback, even if applyResult is nil
		copyLogsFromRedisToMinIO("apply")
		// Check if run was cancelled during apply
		run, _ = runRepo.GetByID(job.RunID)
		if run.Status == models.RunStatusCancelled {
			logger.Infof("Run %s was cancelled during apply execution", run.ID)
			// TFE-compatible: Save partial state after cancelled apply
			// Terraform receives SIGINT on cancel and saves state for already-changed resources.
			// We must upload this partial state to prevent state drift and orphaned resources.
			storePhaseState("apply")
			stateFile := filepath.Join(terraformDir, "terraform.tfstate")
			if stateData, readErr := os.ReadFile(stateFile); readErr == nil { //nolint:gosec // stateFile is from workspace directory
				var stateJSON map[string]interface{}
				if jsonErr := json.Unmarshal(stateData, &stateJSON); jsonErr == nil {
					commitHash := ""
					committer := ""
					if run.ConfigurationVersionID != nil {
						cv, cvErr := configVersionRepo.GetByID(*run.ConfigurationVersionID)
						if cvErr == nil && cv != nil {
							commitHash = cv.CommitHash
							committer = cv.Committer
						}
					}
					runID := job.RunID
					stateSaveCtx, stateSaveCancel := context.WithTimeout(ctx, 30*time.Second)
					defer stateSaveCancel()
					if _, saveErr := stateService.SaveState(stateSaveCtx, job.WorkspaceID, stateJSON, &runID, commitHash, committer); saveErr != nil {
						logger.Warnf("Failed to save partial state after cancelled apply: %v", saveErr)
					} else {
						logger.Infof("Partial state saved successfully after cancelled apply for run %s", run.ID)
					}
				} else {
					logger.Warnf("Failed to parse partial state file after cancelled apply: %v", jsonErr)
				}
			} else {
				logger.Debugf("No state file found after cancelled apply (may be expected for first run): %v", readErr)
			}
			return nil
		}

		// Check if timeout was exceeded
		if applyCtx.Err() == context.DeadlineExceeded {
			logger.Infof("Apply operation exceeded timeout of %v for run %s", applyTimeout, run.ID)
			run.Status = models.RunStatusFailed
			run.ErrorMessage = fmt.Sprintf("Apply operation exceeded timeout of %v and was automatically cancelled", applyTimeout)
			if err := runRepo.Update(run); err != nil {
				return fmt.Errorf("failed to update run status: %w", err)
			}
			return nil
		}

		if err != nil {
			run.Status = models.RunStatusFailed
			run.ErrorMessage = err.Error()
			// Store phase state even on failure (to show completed resources)
			storePhaseState("apply")
		} else {
			// Set status based on run operation type
			applyCompletedAt := time.Now()
			if run.Operation == models.RunOperationPlanAndApply || run.Operation == models.RunOperationDestroy {
				// Plan-and-apply or destroy run: Apply phase completed
				run.Status = models.RunStatusApplied
				run.CompletedAt = &applyCompletedAt // Set CompletedAt when apply phase completes
			} else {
				// This should not happen - apply phase should only execute for plan-and-apply or destroy runs
				logger.Warnf("Apply phase completed for unexpected run %s (operation: %s)", run.ID, run.Operation)
				run.Status = models.RunStatusFailed
				run.ErrorMessage = "Apply phase should only execute for plan-and-apply or destroy runs"
				run.CompletedAt = &applyCompletedAt
			}
			// Store parsed apply phase state for persistence
			storePhaseState("apply")

			// TFE-compatible: Save state after successful apply
			// Read terraform.tfstate file and create state version
			stateFile := filepath.Join(terraformDir, "terraform.tfstate")
			if stateData, readErr := os.ReadFile(stateFile); readErr == nil { //nolint:gosec // stateFile is from workspace directory, validated
				var stateJSON map[string]interface{}
				if jsonErr := json.Unmarshal(stateData, &stateJSON); jsonErr == nil {
					// Create state version via state service, linking to the run that created it
					// Extract commit info from run's configuration version if available (for VCS-triggered runs)
					commitHash := ""
					committer := ""
					if run.ConfigurationVersionID != nil {
						configVersion, err := configVersionRepo.GetByID(*run.ConfigurationVersionID)
						if err == nil && configVersion != nil {
							// Extract commit info from configuration version (set by VCS webhook)
							commitHash = configVersion.CommitHash
							committer = configVersion.Committer
						}
					}
					runID := job.RunID
					// Save state with timeout (30 seconds) to prevent hanging
					// Run status is already set to "applied" above, so state saving failure won't block status update
					stateSaveCtx, stateSaveCancel := context.WithTimeout(ctx, 30*time.Second)
					defer stateSaveCancel()
					if _, saveErr := stateService.SaveState(stateSaveCtx, job.WorkspaceID, stateJSON, &runID, commitHash, committer); saveErr != nil {
						logger.Warnf("Failed to save state after apply: %v", saveErr)
						// Don't fail the run if state saving fails, just log it
						// Run status is already set to "applied", so the run will complete successfully
					} else {
						logger.Infof("State saved successfully after apply for workspace %s", job.WorkspaceID)
					}
				} else {
					logger.Warnf("Failed to parse state file: %v", jsonErr)
				}
			} else {
				logger.Warnf("Failed to read state file: %v", readErr)
			}
		}
	}

	// Set CompletedAt for any run that doesn't have it set yet, but ONLY if run is in a terminal state
	// Don't set CompletedAt for runs that are still waiting (e.g., plan-and-apply runs in "planned" status waiting for apply)
	// Terminal states:
	// - applied, failed, canceled, completed: always terminal
	// - planned: terminal for plan-only runs, but NOT for plan-and-apply runs (they wait for apply)
	if run.CompletedAt == nil {
		isTerminal := false
		switch run.Status {
		case models.RunStatusApplied, models.RunStatusFailed, models.RunStatusCancelled, models.RunStatusCompleted:
			isTerminal = true
		case models.RunStatusPlanned:
			// "planned" is terminal for plan-only runs, but NOT for plan-and-apply runs
			isTerminal = (run.Operation == models.RunOperationPlanOnly)
		case models.RunStatusPending, models.RunStatusPlanning, models.RunStatusApplying, models.RunStatusRunning:
			// These are non-terminal states
			isTerminal = false
		}

		if isTerminal {
			now := time.Now()
			run.CompletedAt = &now
		}
	}
	if err := runRepo.Update(run); err != nil {
		return fmt.Errorf("failed to update run: %w", err)
	}

	logger.Infof("Job completed: RunID=%s, Status=%s", job.RunID, run.Status)
	return nil
}

// extractTarGz extracts a gzipped tarball ([]byte) to a directory
// TFE stores configuration files as tar.gz archives
func extractTarGz(data []byte, destDir string) error {
	gzipReader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
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
			return fmt.Errorf("failed to read tar header: %w", err)
		}

		targetPath := filepath.Join(destDir, header.Name) //nolint:gosec // path traversal protection below

		// Security: Prevent directory traversal - ensure targetPath is within destDir
		cleanTargetPath := filepath.Clean(targetPath)
		cleanDestDir := filepath.Clean(destDir)
		if !strings.HasPrefix(cleanTargetPath, cleanDestDir+string(filepath.Separator)) && cleanTargetPath != cleanDestDir {
			return fmt.Errorf("invalid file path (directory traversal attempt): %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			// Security: Validate directory mode to prevent integer overflow
			dirMode := header.Mode & 0o777 // Only use permission bits
			if dirMode > 0o777 {
				dirMode = 0o750 // Default to safe permissions if invalid
			}
			if err := os.MkdirAll(targetPath, os.FileMode(dirMode)); err != nil { //nolint:gosec // dirMode is validated above
				return fmt.Errorf("failed to create directory: %w", err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(targetPath), 0o750); err != nil {
				return fmt.Errorf("failed to create parent directory: %w", err)
			}
			// Security: Validate file mode to prevent integer overflow
			fileMode := header.Mode & 0o777 // Only use permission bits
			if fileMode > 0o777 {
				fileMode = 0o644 // Default to safe permissions if invalid
			}
			file, err := os.OpenFile(targetPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, os.FileMode(fileMode)) //nolint:gosec // fileMode is validated above
			if err != nil {
				return fmt.Errorf("failed to create file: %w", err)
			}
			// Security: Limit decompression size to prevent decompression bombs (100MB limit)
			const maxDecompressedSize = 100 * 1024 * 1024 // 100MB
			limitedReader := io.LimitReader(tarReader, maxDecompressedSize)
			if _, err := io.Copy(file, limitedReader); err != nil {
				if closeErr := file.Close(); closeErr != nil {
					logger.Warnf("Failed to close file after copy error: %v", closeErr)
				}
				return fmt.Errorf("failed to write file: %w", err)
			}
			if err := file.Close(); err != nil {
				logger.Warnf("Failed to close file: %v", err)
			}
		default:
			// Skip other types (symlinks, etc.)
			logger.Infof("Skipping unsupported file type: %s (type: %c)", header.Name, header.Typeflag)
		}
	}

	return nil
}

// filterLocalPathMessages filters out local file path messages from logs
// TFE-compatible: Remote execution doesn't show "Saved the plan to: /path/to/plan.out" messages
// func filterLocalPathMessages(logs string) string {
// 	lines := strings.Split(logs, "\n")
// 	var filtered []string
// 	for _, line := range lines {
// 		// Filter out "Saved the plan to:" messages (local backend artifact)
// 		if strings.Contains(line, "Saved the plan to:") {
// 			continue
// 		}
// 		// Filter out "To perform exactly these actions, run:" messages (not applicable for remote execution)
// 		if strings.Contains(line, "To perform exactly these actions, run:") {
// 			continue
// 		}
// 		filtered = append(filtered, line)
// 	}
// 	return strings.Join(filtered, "\n")
// }

// replaceRemoteBackendWithLocal replaces remote backend with local backend in terraform config files
// This prevents the runner from creating nested runs when executing terraform commands
// TFE-compatible: Remote execution should use local state, not remote backend
func replaceRemoteBackendWithLocal(workspaceDir string) error {
	terraformFiles := []string{"main.tf", "terraform.tf", "backend.tf", "providers.tf"}

	for _, filename := range terraformFiles {
		filePath := filepath.Join(workspaceDir, filename)
		content, err := os.ReadFile(filePath) //nolint:gosec // filePath is from workspace directory, validated
		if err != nil {
			continue // File doesn't exist, skip
		}

		contentStr := string(content)
		// Check if file contains remote backend
		if !strings.Contains(contentStr, "backend \"remote\"") {
			continue // No remote backend in this file
		}

		// Replace remote backend with local backend
		// Pattern: backend "remote" { ... } -> backend "local" { path = "terraform.tfstate" }
		// Use regex to match and replace the entire backend block (handles nested braces)
		re := regexp.MustCompile(`(?s)backend\s+"remote"\s*\{[^{}]*(?:\{[^{}]*\}[^{}]*)*\}`)
		replacement := `backend "local" {
  path = "terraform.tfstate"
}`
		newContent := re.ReplaceAllString(contentStr, replacement)

		// If the simple regex didn't match (nested braces), try a more aggressive approach
		// Find the start of backend "remote" and replace until the matching closing brace
		if newContent == contentStr {
			// Find backend "remote" block start
			startIdx := strings.Index(contentStr, `backend "remote"`)
			if startIdx != -1 {
				// Find the opening brace
				braceStart := strings.Index(contentStr[startIdx:], "{")
				if braceStart != -1 {
					braceStart += startIdx
					// Count braces to find matching closing brace
					braceCount := 0
					endIdx := braceStart
					for i := braceStart; i < len(contentStr); i++ {
						if contentStr[i] == '{' {
							braceCount++
						} else if contentStr[i] == '}' {
							braceCount--
							if braceCount == 0 {
								endIdx = i + 1
								break
							}
						}
					}
					// Replace the entire block
					newContent = contentStr[:startIdx] + `backend "local" {
  path = "terraform.tfstate"
}` + contentStr[endIdx:]
				}
			}
		}

		// If replacement occurred, write the file back
		if newContent != contentStr {
			if err := os.WriteFile(filePath, []byte(newContent), 0o600); err != nil {
				return fmt.Errorf("failed to write %s: %w", filename, err)
			}
			logger.Infof("Replaced remote backend with local backend in %s", filename)
		}
	}

	return nil
}
