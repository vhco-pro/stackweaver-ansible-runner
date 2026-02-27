// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/queue"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/vcs"
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

func main() {
	// Initialize logger first (reads LOG_LEVEL from environment)
	logLevel := os.Getenv("LOG_LEVEL")
	logger.Init(logLevel)

	// Initialize queue
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

	// Initialize database
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

	runRepo := repository.NewRunRepository(db)
	workspaceRepo := repository.NewWorkspaceRepository(db)
	configVersionRepo := repository.NewConfigurationVersionRepository(db)
	vcsConnectionRepo := repository.NewVCSConnectionRepository(db)
	agentPoolRepo := repository.NewAgentPoolRepository(db)

	// Initialize GitHub App Manager and status service for PR status checks
	var statusService *vcs.GitHubStatusService
	githubAppManager, err := vcs.NewGitHubAppManager()
	if err != nil {
		logger.Warnf("Failed to initialize GitHub App Manager: %v (GitHub PR status checks will be disabled)", err)
	} else if githubAppManager != nil && githubAppManager.IsEnabled() {
		appService := githubAppManager.GetService()
		if appService != nil {
			statusService = vcs.NewGitHubStatusService(appService)
			logger.Info("GitHub status check service initialized for PR status checks")
		}
	}

	// Initialize Azure DevOps Manager and status service for PR status checks
	var adoStatusService *vcs.AzureDevOpsStatusService
	azureDevOpsManager, adoErr := vcs.NewAzureDevOpsManager()
	if adoErr != nil {
		logger.Warnf("Failed to initialize Azure DevOps Manager: %v (ADO PR status checks will be disabled)", adoErr)
	} else if azureDevOpsManager != nil && azureDevOpsManager.IsEnabled() {
		// Create a connUpdater that persists token changes to the database
		connUpdater := func(conn *models.VCSConnection) error {
			return vcsConnectionRepo.Update(conn)
		}
		adoStatusService = vcs.NewAzureDevOpsStatusService(azureDevOpsManager, connUpdater)
		logger.Info("Azure DevOps status check service initialized for PR status checks")
	}

	// Create a provider registry for ADO token refresh
	vcsRegistry := vcs.NewProviderRegistry(githubAppManager, azureDevOpsManager, func(conn *models.VCSConnection) error {
		return vcsConnectionRepo.Update(conn)
	})

	// Start orchestrator
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Process pending runs every 5 seconds
	go func() {
		logger.Info("Orchestrator started - processing pending runs")
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := processPendingRuns(ctx, redisQueue, runRepo, workspaceRepo, agentPoolRepo); err != nil {
					logger.Errorf("Error processing pending runs: %v", err)
				}
			}
		}
	}()

	// Clean up stuck runs every 1 minute
	go func() {
		logger.Info("Orchestrator started - cleaning up stuck runs")
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := cleanupStuckRuns(ctx, runRepo); err != nil {
					logger.Errorf("Error cleaning up stuck runs: %v", err)
				}
			}
		}
	}()

	// Update PR status checks every 10 seconds
	go func() {
		logger.Info("Orchestrator started - updating PR status checks")
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := updatePRStatusChecks(ctx, runRepo, workspaceRepo, configVersionRepo, vcsConnectionRepo, statusService, adoStatusService, vcsRegistry); err != nil {
					logger.Errorf("Error updating PR status checks: %v", err)
				}
			}
		}
	}()

	// Wait for interrupt
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	logger.Info("Shutting down orchestrator...")
	cancel()
}

func processPendingRuns(ctx context.Context, redisQueue *queue.RedisQueue, runRepo *repository.RunRepository, workspaceRepo *repository.WorkspaceRepository, agentPoolRepo *repository.AgentPoolRepository) error {
	// Get pending runs (initial state) and applying runs (plan-and-apply runs ready for apply phase)
	pendingRuns, err := runRepo.ListByStatus(models.RunStatusPending, 10)
	if err != nil {
		return fmt.Errorf("failed to list pending runs: %w", err)
	}

	// Also get runs in "applying" status (plan-and-apply runs ready for apply phase)
	applyingRuns, err := runRepo.ListByStatus(models.RunStatusApplying, 10)
	if err != nil {
		return fmt.Errorf("failed to list applying runs: %w", err)
	}

	// Combine both lists
	runs := make([]models.Run, 0, len(pendingRuns)+len(applyingRuns))
	runs = append(runs, pendingRuns...)
	runs = append(runs, applyingRuns...)

	for _, run := range runs {
		// Skip runs that are too old - they should be cleaned up by cleanupStuckRuns
		// This prevents re-enqueueing abandoned runs
		if time.Since(run.CreatedAt) > 30*time.Minute {
			logger.Infof("Skipping enqueue for old pending run %s (created %s ago), it should be cleaned up by the stuck run cleaner.", run.ID, time.Since(run.CreatedAt).Round(time.Second))
			continue
		}

		// Reload run to check if status changed (might have been cancelled or failed)
		// This prevents enqueueing runs that were cancelled/failed between the query and now
		reloadedRun, err := runRepo.GetByID(run.ID)
		if err != nil {
			logger.Errorf("Failed to reload run %s: %v", run.ID, err)
			continue
		}

		// Skip if run is no longer pending or applying (might have been cancelled, failed, or started)
		// "applying" status means plan-and-apply run is ready for apply phase
		if reloadedRun.Status != models.RunStatusPending && reloadedRun.Status != models.RunStatusApplying {
			logger.Infof("Skipping enqueue for run %s - status changed to %s", reloadedRun.ID, reloadedRun.Status)
			continue
		}

		// Check if workspace is configured for self-hosted execution
		// If so, skip enqueueing to Redis - self-hosted runners poll the API directly
		workspace, err := workspaceRepo.GetByID(reloadedRun.WorkspaceID)
		if err != nil {
			logger.Errorf("Failed to get workspace for run %s: %v", reloadedRun.ID, err)
			continue
		}

		// If workspace has execution_mode="agent" and agent_pool_id set, it's for self-hosted runners
		if workspace.ExecutionMode == "agent" && workspace.AgentPoolID != nil {
			// Verify the agent pool exists
			_, err := agentPoolRepo.GetByID(*workspace.AgentPoolID, false)
			if err == nil {
				// Run is for self-hosted execution - assign agent_pool_id to the run if not already set
				if reloadedRun.AgentPoolID == nil {
					reloadedRun.AgentPoolID = workspace.AgentPoolID
					if err := runRepo.Update(reloadedRun); err != nil {
						logger.Errorf("Failed to update run %s with agent_pool_id: %v", reloadedRun.ID, err)
					} else {
						logger.Infof("Run %s assigned to agent pool %s for self-hosted execution", reloadedRun.ID, workspace.AgentPoolID)
					}
				}
				// Skip enqueueing to Redis - self-hosted runners will poll for this job
				logger.Debugf("Skipping Redis enqueue for run %s - configured for self-hosted execution (pool: %s)", reloadedRun.ID, workspace.AgentPoolID)
				continue
			}
			logger.Warnf("Workspace %s has agent_pool_id set but pool not found, falling back to platform-hosted execution", workspace.ID)
		}

		// Run is for platform execution (Redis). Clear agent_pool_id so self-hosted
		// runners don't pick it up (e.g. workspace was switched from agent to remote).
		if reloadedRun.AgentPoolID != nil {
			reloadedRun.AgentPoolID = nil
			if err := runRepo.Update(reloadedRun); err != nil {
				logger.Warnf("Failed to clear agent_pool_id on run %s: %v", reloadedRun.ID, err)
				continue // Do not enqueue; run would still be visible to agent
			}
		}

		// Create job struct matching runner's Job type
		job := map[string]interface{}{
			"run_id":       reloadedRun.ID,
			"workspace_id": reloadedRun.WorkspaceID,
			"operation":    string(reloadedRun.Operation),
		}

		// Enqueue expects interface{} and will marshal it itself
		// Don't marshal here to avoid double-marshaling
		if err := redisQueue.Enqueue(ctx, "runs", job); err != nil {
			logger.Errorf("Failed to enqueue job: %v", err)
			continue
		}

		logger.Infof("Enqueued run: %s", reloadedRun.ID)
	}

	return nil
}

// cleanupStuckRuns finds and marks stuck runs as failed
// - Running runs that have exceeded their timeout (workspace RunTimeout, default 1 hour)
// - Pending runs that have been pending for more than 30 minutes (truly abandoned)
// Note: We only clean up runs that have exceeded their actual timeout - normal runs can take 30+ minutes
func cleanupStuckRuns(ctx context.Context, runRepo *repository.RunRepository) error {
	// Find stuck runs (pending for more than 30 minutes, or running longer than timeout)
	stuckRuns, err := runRepo.FindStuckRuns(30 * time.Minute)
	if err != nil {
		return fmt.Errorf("failed to find stuck runs: %w", err)
	}

	if len(stuckRuns) == 0 {
		return nil
	}

	logger.Infof("Found %d stuck run(s), marking as failed", len(stuckRuns))

	for _, run := range stuckRuns {
		var errorMessage string
		if run.Status == models.RunStatusRunning {
			// Running run that exceeded timeout
			timeout := 3600 // Default 1 hour
			if run.Workspace.RunTimeout > 0 {
				timeout = run.Workspace.RunTimeout
			}
			errorMessage = fmt.Sprintf("Run exceeded timeout of %d seconds and was automatically cancelled", timeout)
		} else {
			// Pending run that was abandoned (more than 30 minutes)
			errorMessage = "Run was pending for too long and was automatically cancelled"
		}

		if err := runRepo.MarkAsFailed(run.ID, errorMessage); err != nil {
			logger.Errorf("Failed to mark run %s as failed: %v", run.ID, err)
			continue
		}

		logger.Infof("Marked stuck run %s as failed: %s", run.ID, errorMessage)
	}

	return nil
}

// updatePRStatusChecks polls for PR runs and updates their VCS status checks (GitHub and Azure DevOps)
func updatePRStatusChecks(
	ctx context.Context,
	runRepo *repository.RunRepository,
	workspaceRepo *repository.WorkspaceRepository,
	configVersionRepo *repository.ConfigurationVersionRepository,
	vcsConnectionRepo *repository.VCSConnectionRepository,
	statusService *vcs.GitHubStatusService,
	adoStatusService *vcs.AzureDevOpsStatusService,
	vcsRegistry *vcs.ProviderRegistry,
) error {
	// Skip if no status service is available at all
	if statusService == nil && adoStatusService == nil {
		return nil
	}

	// Get PR runs that need status check updates (speculative runs with active statuses)
	runs, err := runRepo.ListPRRunsForStatusChecks(50)
	if err != nil {
		return fmt.Errorf("failed to list PR runs: %w", err)
	}

	if len(runs) == 0 {
		return nil
	}

	logger.Infof("[STATUS_CHECK] Found %d PR run(s) to check for status check updates", len(runs))

	for _, run := range runs {
		updatePRStatusCheck(ctx, &run, workspaceRepo, configVersionRepo, vcsConnectionRepo, statusService, adoStatusService, vcsRegistry)
	}

	return nil
}
