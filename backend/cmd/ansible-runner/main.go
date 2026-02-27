// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/queue"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/ansible"
	"github.com/iac-platform/backend/internal/services/oidc"
	"github.com/iac-platform/backend/internal/services/vcs"
	"github.com/iac-platform/backend/internal/storage"
	vcsGitHub "github.com/iac-platform/backend/internal/vcs/github"
	"github.com/iac-platform/backend/pkg/crypto"
	"github.com/michielvha/logger"
)

// AnsibleJobMessage represents the message received from the job queue
type AnsibleJobMessage struct {
	JobID       uuid.UUID `json:"job_id"`
	PlaybookID  uuid.UUID `json:"playbook_id"`
	InventoryID uuid.UUID `json:"inventory_id"`
	JobType     string    `json:"job_type"`
}

// PlaybookSyncMessage represents a request to sync a playbook from VCS
type PlaybookSyncMessage struct {
	PlaybookID uuid.UUID `json:"playbook_id"`
}

// InventorySyncMessage represents a request to sync a VCS inventory from repository
type InventorySyncMessage struct {
	InventoryID uuid.UUID `json:"inventory_id"`
}

// InventorySourceSyncMessage represents a request to sync a dynamic inventory source
type InventorySourceSyncMessage struct {
	SourceID uuid.UUID `json:"source_id"`
}

// syncResult holds the outcome of an inventory sync operation
type syncResult struct {
	HostsDiscovered int    // Number of hosts found during sync
	Stderr          string // Stderr output from ansible-inventory (warnings, debug info)
}

// Config holds the runner configuration
type Config struct {
	RedisHost         string
	RedisPort         int
	RedisPassword     string
	RedisDB           int
	DatabaseHost      string
	DatabasePort      int
	DatabaseUser      string
	DatabasePassword  string
	DatabaseName      string
	StorageEndpoint   string
	StorageAccessKey  string
	StorageSecretKey  string
	StorageBucket     string
	StorageUseSSL     bool
	EncryptionKey     []byte
	WorkspacesDir     string
	AnsibleBinaryPath string
}

func main() {
	// Initialize logger first (reads LOG_LEVEL from environment)
	logLevel := os.Getenv("LOG_LEVEL")
	logger.Init(logLevel)

	// Check for agent mode - if set, run as self-hosted runner polling the API
	if os.Getenv("RUNNER_MODE") == "agent" {
		logger.Info("Starting in agent mode (self-hosted runner)")
		RunAgentMode()
		return
	}

	// Default mode: platform-hosted, connected to Redis queue and database
	logger.Info("Starting in platform mode (Redis queue worker)")
	config := loadConfig()

	// Initialize Redis queue
	redisQueue, err := queue.NewRedisQueue(config.RedisHost, config.RedisPort, config.RedisPassword, config.RedisDB)
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
		Host:            config.DatabaseHost,
		Port:            config.DatabasePort,
		User:            config.DatabaseUser,
		Password:        config.DatabasePassword,
		DBName:          config.DatabaseName,
		SSLMode:         "disable",
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

	// Initialize storage client
	storageClient, err := storage.NewMinIOClient(
		config.StorageEndpoint,
		config.StorageAccessKey,
		config.StorageSecretKey,
		config.StorageBucket,
		config.StorageUseSSL,
	)
	if err != nil {
		logger.Fatalf("Failed to connect to storage: %v", err)
	}

	// Initialize repositories
	jobRepo := repository.NewAnsibleJobRepository(db)
	playbookRepo := repository.NewAnsiblePlaybookRepository(db)
	inventoryRepo := repository.NewAnsibleInventoryRepository(db)
	credentialRepo := repository.NewAnsibleCredentialRepository(db)
	orgRepo := repository.NewOrganizationRepository(db)
	vcsConnectionRepo := repository.NewVCSConnectionRepository(db)
	ansibleConfigRepo := repository.NewAnsibleConfigRepository(db)
	projectRepo := repository.NewProjectRepository(db)

	// Initialize services
	inventoryService := ansible.NewInventoryService(inventoryRepo, orgRepo)
	credentialService := ansible.NewCredentialService(credentialRepo, config.EncryptionKey)

	// Initialize inventory source service (for dynamic inventory source sync)
	inventorySourceRepo := repository.NewAnsibleInventorySourceRepository(db)
	cryptoService, cryptoErr := crypto.NewCryptoService(config.EncryptionKey)
	if cryptoErr != nil {
		logger.Fatalf("Failed to initialize crypto service: %v", cryptoErr)
	}
	inventorySourceService := ansible.NewInventorySourceService(inventorySourceRepo, inventoryRepo, credentialRepo, cryptoService)

	// Initialize VCS provider registry for multi-provider clone support
	githubAppManager, err := vcs.NewGitHubAppManager()
	if err != nil {
		logger.Warnf("GitHub App manager not configured: %v", err)
	}
	azureDevOpsManager, err := vcs.NewAzureDevOpsManager()
	if err != nil {
		logger.Warnf("Azure DevOps manager not configured: %v", err)
	}
	vcsRegistry := vcs.NewProviderRegistry(githubAppManager, azureDevOpsManager, func(conn *models.VCSConnection) error {
		return vcsConnectionRepo.Update(conn)
	})

	// OIDC Workload Identity: Initialize signing key and token service for Azure OIDC
	// This allows inventory sync to authenticate to Azure for dynamic inventory plugins (azure_rm)
	azureOIDCRepo := repository.NewAzureOIDCConfigurationRepository(db)
	oidcSigningKey, oidcErr := oidc.NewSigningKey()
	var oidcTokenService *oidc.TokenService
	if oidcErr != nil {
		logger.Warnf("Failed to initialize OIDC signing key: %v (OIDC workload identity will be disabled for inventory sync)", oidcErr)
	} else {
		issuerURL := os.Getenv("OIDC_ISSUER_URL")
		if issuerURL == "" {
			issuerURL = os.Getenv("API_URL")
		}
		if issuerURL == "" {
			issuerURL = "http://localhost:8022"
		}
		oidcTokenService = oidc.NewTokenService(oidcSigningKey, issuerURL)
		logger.Info("OIDC workload identity token service initialized for inventory sync")
		inventorySourceService.SetOIDCServices(azureOIDCRepo, oidcTokenService)
	}

	// Create runner
	runner := &AnsibleRunner{
		config:                 config,
		queue:                  redisQueue,
		jobRepo:                jobRepo,
		playbookRepo:           playbookRepo,
		inventoryRepo:          inventoryRepo,
		vcsConnectionRepo:      vcsConnectionRepo,
		configRepo:             ansibleConfigRepo,
		projectRepo:            projectRepo,
		inventoryService:       inventoryService,
		credentialService:      credentialService,
		inventorySourceService: inventorySourceService,
		storageClient:          storageClient,
		vcsRegistry:            vcsRegistry,
		azureOIDCRepo:          azureOIDCRepo,
		oidcTokenService:       oidcTokenService,
	}

	// Start worker
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Job execution worker
	go func() {
		logger.Info("Ansible Runner started, waiting for jobs...")
		for {
			select {
			case <-ctx.Done():
				return
			default:
				if err := runner.processJob(ctx); err != nil {
					if err != queue.ErrQueueEmpty {
						logger.Infof("Error processing job: %v", err)
					}
					time.Sleep(1 * time.Second)
				}
			}
		}
	}()

	// Playbook sync worker
	go func() {
		logger.Info("Ansible Sync Worker started, waiting for sync requests...")
		for {
			select {
			case <-ctx.Done():
				return
			default:
				if err := runner.processSyncJob(ctx); err != nil {
					if err != queue.ErrQueueEmpty {
						logger.Infof("Error processing sync job: %v", err)
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

	logger.Info("Shutting down Ansible Runner...")
	cancel()
}

// AnsibleRunner handles Ansible job execution
type AnsibleRunner struct {
	config                 Config
	queue                  *queue.RedisQueue
	jobRepo                *repository.AnsibleJobRepository
	playbookRepo           *repository.AnsiblePlaybookRepository
	inventoryRepo          *repository.AnsibleInventoryRepository
	vcsConnectionRepo      *repository.VCSConnectionRepository
	configRepo             *repository.AnsibleConfigRepository
	projectRepo            *repository.ProjectRepository
	inventoryService       *ansible.InventoryService
	credentialService      *ansible.CredentialService
	inventorySourceService *ansible.InventorySourceService
	storageClient          storage.Client
	vcsRegistry            *vcs.ProviderRegistry
	// OIDC workload identity for Azure dynamic inventory sync
	azureOIDCRepo    *repository.AzureOIDCConfigurationRepository
	oidcTokenService *oidc.TokenService
}

func (r *AnsibleRunner) processJob(ctx context.Context) error {
	// Dequeue job
	jobData, err := r.queue.Dequeue(ctx, "ansible_jobs", 5*time.Second)
	if err != nil {
		return err
	}

	var msg AnsibleJobMessage
	if err := json.Unmarshal(jobData, &msg); err != nil {
		return fmt.Errorf("failed to unmarshal job message: %w", err)
	}

	logger.Infof("Processing Ansible job: JobID=%s, PlaybookID=%s, InventoryID=%s",
		msg.JobID, msg.PlaybookID, msg.InventoryID)

	// Get job from database
	job, err := r.jobRepo.GetByID(msg.JobID)
	if err != nil {
		return fmt.Errorf("failed to get job: %w", err)
	}

	// Check if job was cancelled
	if job.Status == models.AnsibleJobStatusCanceled {
		logger.Infof("Job %s was cancelled, skipping", job.ID.String())
		return nil
	}

	// Update job status to running
	now := time.Now()
	job.Status = models.AnsibleJobStatusRunning
	job.StartedAt = &now
	if err := r.jobRepo.Update(job); err != nil {
		return fmt.Errorf("failed to update job status: %w", err)
	}

	// Execute the job
	err = r.executeJob(ctx, job)

	// Update job completion status
	completedAt := time.Now()
	job.FinishedAt = &completedAt

	if err != nil {
		job.Status = models.AnsibleJobStatusFailed
		job.ErrorMessage = err.Error()
		logger.Infof("Job %s failed: %v", job.ID.String(), err)
	} else {
		job.Status = models.AnsibleJobStatusSuccessful
		logger.Infof("Job %s completed successfully", job.ID.String())
	}

	if updateErr := r.jobRepo.Update(job); updateErr != nil {
		logger.Warnf("Failed to update job completion status: %v", updateErr)
	}

	return nil
}

func (r *AnsibleRunner) processSyncJob(ctx context.Context) error {
	// Dequeue sync job
	syncData, err := r.queue.Dequeue(ctx, "ansible_sync", 5*time.Second)
	if err != nil {
		return err
	}

	// Try to determine message type by checking for presence of fields
	// First try playbook sync
	var playbookMsg PlaybookSyncMessage
	if err := json.Unmarshal(syncData, &playbookMsg); err == nil && playbookMsg.PlaybookID != uuid.Nil {
		logger.Infof("Processing playbook sync: PlaybookID=%s", playbookMsg.PlaybookID)

		// Get playbook from database
		playbook, err := r.playbookRepo.GetByID(playbookMsg.PlaybookID)
		if err != nil {
			return fmt.Errorf("failed to get playbook: %w", err)
		}

		// Execute the sync
		err = r.syncPlaybook(ctx, playbook)

		// Update sync status
		now := time.Now()
		playbook.LastSyncAt = &now

		if err != nil {
			playbook.LastSyncStatus = "failed"
			playbook.LastSyncError = err.Error()
			logger.Infof("Playbook sync %s failed: %v", playbook.ID.String(), err)
		} else {
			playbook.LastSyncStatus = "successful"
			playbook.LastSyncError = ""
			logger.Infof("Playbook sync %s completed successfully", playbook.ID.String())
		}

		if updateErr := r.playbookRepo.Update(playbook); updateErr != nil {
			logger.Warnf("Failed to update playbook sync status: %v", updateErr)
		}

		return nil
	}

	// Try inventory sync
	var inventoryMsg InventorySyncMessage
	if err := json.Unmarshal(syncData, &inventoryMsg); err == nil && inventoryMsg.InventoryID != uuid.Nil {
		logger.Infof("Processing inventory sync: InventoryID=%s", inventoryMsg.InventoryID)

		// Get inventory from database
		inventory, err := r.inventoryRepo.GetByID(inventoryMsg.InventoryID)
		if err != nil {
			return fmt.Errorf("failed to get inventory: %w", err)
		}

		// Execute the sync
		result, err := r.syncInventory(ctx, inventory)

		// Update sync status
		now := time.Now()
		inventory.LastSyncAt = &now

		if err != nil {
			inventory.LastSyncStatus = "failed"
			inventory.LastSyncError = err.Error()
			inventory.LastSyncLog = result.Stderr // preserve stderr even on failure
			logger.Infof("Inventory sync %s failed: %v", inventory.ID.String(), err)
		} else {
			inventory.LastSyncStatus = "successful"
			inventory.LastSyncError = ""
			inventory.LastSyncHostsDiscovered = result.HostsDiscovered
			inventory.LastSyncLog = result.Stderr
			logger.Infof("Inventory sync %s completed successfully (hosts: %d)", inventory.ID.String(), result.HostsDiscovered)
			if result.HostsDiscovered == 0 {
				logger.Warnf("Inventory sync %s: 0 hosts discovered — check plugin configuration and authentication", inventory.ID.String())
			}
		}

		if updateErr := r.inventoryRepo.Update(inventory); updateErr != nil {
			logger.Warnf("Failed to update inventory sync status: %v", updateErr)
		}

		return nil
	}

	// Try dynamic inventory source sync
	var sourceMsg InventorySourceSyncMessage
	if err := json.Unmarshal(syncData, &sourceMsg); err == nil && sourceMsg.SourceID != uuid.Nil {
		logger.Infof("Processing dynamic inventory source sync: SourceID=%s", sourceMsg.SourceID)

		result, err := r.inventorySourceService.SyncInventorySource(ctx, sourceMsg.SourceID)
		if err != nil {
			logger.Infof("Inventory source sync %s failed: %v", sourceMsg.SourceID, err)
		} else {
			logger.Infof("Inventory source sync %s completed successfully (hosts: %d)", sourceMsg.SourceID, result.HostsDiscovered)
		}

		return nil
	}

	return fmt.Errorf("failed to unmarshal sync message: invalid message type")
}

func (r *AnsibleRunner) syncPlaybook(ctx context.Context, playbook *models.AnsiblePlaybook) error {
	// Check if playbook has a VCS connection
	if playbook.VCSConnectionID == nil {
		return fmt.Errorf("playbook has no VCS connection configured")
	}
	if playbook.VCSRepository == "" {
		return fmt.Errorf("playbook has no VCS repository configured")
	}

	// Create a temporary directory for the sync
	syncDir := filepath.Join(r.config.WorkspacesDir, "ansible-sync", playbook.ID.String())
	if err := os.MkdirAll(syncDir, 0o750); err != nil {
		return fmt.Errorf("failed to create sync directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(syncDir); err != nil {
			logger.Warnf("Failed to remove sync directory %s: %v", syncDir, err)
		}
	}()

	// Clone the repository
	repoDir, err := r.cloneVCSRepo(ctx, syncDir, playbook)
	if err != nil {
		return fmt.Errorf("failed to clone repository: %w", err)
	}

	// Verify playbook file exists (strip leading / for ADO paths)
	playbookPath := filepath.Join(repoDir, strings.TrimPrefix(playbook.PlaybookPath, "/"))
	if _, err := os.Stat(playbookPath); os.IsNotExist(err) {
		return fmt.Errorf("playbook file not found at path: %s", playbook.PlaybookPath)
	}

	// Get the latest commit hash
	commitHash, err := r.getLatestCommit(ctx, repoDir)
	if err != nil {
		logger.Warnf("Could not get commit hash: %v", err)
	} else {
		playbook.LastSyncCommit = commitHash
	}

	logger.Infof("Successfully synced playbook %s from %s (branch: %s, commit: %s)",
		playbook.Name, playbook.VCSRepository, playbook.VCSBranch, commitHash)

	return nil
}

func (r *AnsibleRunner) syncInventory(ctx context.Context, inventory *models.AnsibleInventory) (syncResult, error) {
	var res syncResult

	// Check if inventory has a VCS connection
	if inventory.VCSConnectionID == nil {
		return res, fmt.Errorf("inventory has no VCS connection configured")
	}
	if inventory.VCSRepository == "" {
		return res, fmt.Errorf("inventory has no VCS repository configured")
	}
	if inventory.InventoryPath == "" {
		return res, fmt.Errorf("inventory has no inventory path configured")
	}

	// Create a temporary directory for the sync
	syncDir := filepath.Join(r.config.WorkspacesDir, "ansible-sync-inventory", inventory.ID.String())
	if err := os.MkdirAll(syncDir, 0o750); err != nil {
		return res, fmt.Errorf("failed to create sync directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(syncDir); err != nil {
			logger.Warnf("Failed to remove sync directory %s: %v", syncDir, err)
		}
	}()

	// Clone the repository
	repoDir, err := r.cloneVCSRepoGeneric(ctx, syncDir, inventory.VCSConnectionID, inventory.VCSRepository, inventory.VCSBranch)
	if err != nil {
		return res, fmt.Errorf("failed to clone repository: %w", err)
	}

	// Verify inventory file exists
	inventoryFilePath := filepath.Join(repoDir, inventory.InventoryPath)
	if _, err := os.Stat(inventoryFilePath); os.IsNotExist(err) {
		return res, fmt.Errorf("inventory file not found at path: %s", inventory.InventoryPath)
	}

	// Run ansible-inventory --list to parse the inventory file
	// Determine the ansible-inventory command.
	// For VCS inventories with Azure OIDC configured, use the OIDC-aware wrapper that
	// monkey-patches the Azure RM collection to use azure-identity's WorkloadIdentityCredential.
	cmdName := "ansible-inventory"
	cmdArgs := []string{"-i", inventoryFilePath, "--list"}
	cmdEnv := os.Environ()

	if r.azureOIDCRepo != nil && r.oidcTokenService != nil {
		configs, oidcErr := r.azureOIDCRepo.GetByOrganization(inventory.OrganizationID)
		if oidcErr != nil {
			logger.Warnf("Failed to look up Azure OIDC config for inventory sync (org %s): %v", inventory.OrganizationID, oidcErr)
		} else if len(configs) > 0 {
			oidcConfig := configs[0]
			orgName := inventory.Organization.Name
			projectName := "default"
			if inventory.Project != nil {
				projectName = inventory.Project.Name
			}
			token, tokenErr := r.oidcTokenService.GenerateWorkloadToken(oidc.WorkloadTokenRequest{
				Audience:         "api://AzureADTokenExchange",
				OrganizationName: orgName,
				ProjectName:      projectName,
				ResourceType:     oidc.ResourceTypeInventory,
				ResourceName:     inventory.Name,
				ActionKind:       oidc.ActionSync,
				ActionID:         inventory.ID.String(),
			})
			if tokenErr != nil {
				logger.Warnf("Failed to generate OIDC token for inventory sync (inventory %s): %v", inventory.ID, tokenErr)
			} else {
				// Write the signed JWT to a temp file for WorkloadIdentityCredential
				tokenFile := filepath.Join(syncDir, "oidc-token.jwt")
				if writeErr := os.WriteFile(tokenFile, []byte(token), 0o600); writeErr != nil {
					logger.Warnf("Failed to write OIDC token file for inventory sync (inventory %s): %v", inventory.ID, writeErr)
				} else {
					cmdEnv = append(cmdEnv,
						"AZURE_CLIENT_ID="+oidcConfig.ClientID,
						"AZURE_TENANT_ID="+oidcConfig.TenantID,
						"AZURE_SUBSCRIPTION_ID="+oidcConfig.SubscriptionID,
						"AZURE_FEDERATED_TOKEN_FILE="+tokenFile,
					)
					// Use the OIDC-aware wrapper instead of ansible-inventory
					cmdName = "python3"
					cmdArgs = append([]string{"/usr/local/bin/oidc-ansible-inventory"}, cmdArgs...)
					logger.Infof("Using OIDC workload identity wrapper for inventory sync (inventory %s, org %s)", inventory.ID, inventory.OrganizationID)
				}
			}
		}
	}

	cmd := exec.CommandContext(ctx, cmdName, cmdArgs...) //nolint:gosec // intentional: executing ansible command
	cmd.Env = cmdEnv

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		res.Stderr = stderr.String()
		return res, fmt.Errorf("ansible-inventory failed: %w: %s", err, res.Stderr)
	}

	// Capture stderr (may contain warnings even on success)
	res.Stderr = stderr.String()
	if res.Stderr != "" {
		logger.Infof("ansible-inventory stderr for %s: %s", inventory.Name, res.Stderr)
	}

	// Parse the JSON output from ansible-inventory
	var inventoryOutput map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &inventoryOutput); err != nil {
		return res, fmt.Errorf("failed to parse ansible-inventory output: %w", err)
	}

	// Process the inventory output and update hosts/groups in database
	hostsDiscovered, err := r.processInventoryOutput(ctx, inventory.ID, inventoryOutput)
	if err != nil {
		return res, fmt.Errorf("failed to process inventory output: %w", err)
	}
	res.HostsDiscovered = hostsDiscovered

	// Get the latest commit hash
	commitHash, err := r.getLatestCommit(ctx, repoDir)
	if err != nil {
		logger.Warnf("Could not get commit hash: %v", err)
	}

	logger.Infof("Successfully synced inventory %s from %s (branch: %s, path: %s, commit: %s, hosts: %d)",
		inventory.Name, inventory.VCSRepository, inventory.VCSBranch, inventory.InventoryPath, commitHash, hostsDiscovered)

	return res, nil
}

// processInventoryOutput processes the ansible-inventory JSON output and updates the inventory
// This is similar to InventorySourceService.processInventoryOutput but works directly with the inventory repo
func (r *AnsibleRunner) processInventoryOutput(ctx context.Context, inventoryID uuid.UUID, output map[string]interface{}) (int, error) {
	hostsDiscovered := 0

	// Get _meta.hostvars for host variables
	hostvars := make(map[string]map[string]interface{})
	if meta, ok := output["_meta"].(map[string]interface{}); ok {
		if hv, ok := meta["hostvars"].(map[string]interface{}); ok {
			for host, vars := range hv {
				if varsMap, ok := vars.(map[string]interface{}); ok {
					hostvars[host] = varsMap
				}
			}
		}
	}

	// Process each group
	processedHosts := make(map[string]bool)
	for groupName, groupData := range output {
		if groupName == "_meta" {
			continue
		}

		groupMap, ok := groupData.(map[string]interface{})
		if !ok {
			continue
		}

		// Get hosts in this group
		hosts, ok := groupMap["hosts"].([]interface{})
		if !ok {
			continue
		}

		// Find or create the group
		var group *models.AnsibleInventoryGroup
		existingGroup, _ := r.inventoryRepo.GetGroupByInventoryAndName(inventoryID, groupName)
		if existingGroup != nil {
			group = existingGroup
		} else {
			group = &models.AnsibleInventoryGroup{
				InventoryID: inventoryID,
				Name:        groupName,
			}
			if err := r.inventoryRepo.CreateGroup(group); err != nil {
				logger.Warnf("Failed to create group %s: %v", groupName, err)
				continue
			}
		}

		// Process hosts
		for _, hostInterface := range hosts {
			hostName, ok := hostInterface.(string)
			if !ok {
				continue
			}

			if processedHosts[hostName] {
				continue
			}
			processedHosts[hostName] = true
			hostsDiscovered++

			// Get host variables
			vars := hostvars[hostName]
			if vars == nil {
				vars = make(map[string]interface{})
			}

			// Determine hostname
			hostname := hostName
			if ansibleHost, ok := vars["ansible_host"].(string); ok && ansibleHost != "" {
				hostname = ansibleHost
			}

			// Find or create the host
			existingHost, _ := r.inventoryRepo.GetHostByInventoryAndName(inventoryID, hostName)
			var host *models.AnsibleInventoryHost
			if existingHost != nil {
				// Update existing host
				existingHost.Hostname = hostname
				existingHost.Variables = vars
				if err := r.inventoryRepo.UpdateHost(existingHost); err != nil {
					logger.Warnf("Failed to update host %s: %v", hostName, err)
					continue
				}
				host = existingHost
			} else {
				host = &models.AnsibleInventoryHost{
					InventoryID: inventoryID,
					Name:        hostName,
					Hostname:    hostname,
					Variables:   vars,
					Enabled:     true,
				}
				if err := r.inventoryRepo.CreateHost(host); err != nil {
					logger.Warnf("Failed to create host %s: %v", hostName, err)
					continue
				}
			}

			// Associate host with group
			if err := r.inventoryRepo.AddHostToGroup(host.ID, group.ID); err != nil {
				logger.Warnf("Failed to add host %s to group %s: %v", host.ID, group.ID, err)
			}
		}
	}

	return hostsDiscovered, nil
}

func (r *AnsibleRunner) getLatestCommit(ctx context.Context, repoDir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir = repoDir
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func (r *AnsibleRunner) executeJob(ctx context.Context, job *models.AnsibleJob) error {
	// Create job workspace directory
	jobDir := filepath.Join(r.config.WorkspacesDir, "ansible-jobs", job.ID.String())
	if err := os.MkdirAll(jobDir, 0o755); err != nil { //nolint:gosec // workspace directories need 0o755 for compatibility
		return fmt.Errorf("failed to create job directory: %w", err)
	}
	defer func() {
		// Cleanup job directory after execution (optional, can be disabled for debugging)
		if os.Getenv("ANSIBLE_RUNNER_KEEP_WORKSPACE") != "true" {
			if err := os.RemoveAll(jobDir); err != nil {
				logger.Warnf("Failed to remove job directory %s: %v", jobDir, err)
			}
		}
	}()

	// Get playbook
	playbook, err := r.playbookRepo.GetByID(job.PlaybookID)
	if err != nil {
		return fmt.Errorf("failed to get playbook: %w", err)
	}

	// Prepare playbook files (sync from SCM or download from storage)
	playbookDir, err := r.preparePlaybook(ctx, jobDir, playbook)
	if err != nil {
		return fmt.Errorf("failed to prepare playbook: %w", err)
	}

	// Install Galaxy requirements if requirements.yml exists
	if err := r.installGalaxyRequirements(ctx, job, playbookDir); err != nil {
		// Log warning but don't fail the job
		logger.Warnf("Failed to install Galaxy requirements: %v", err)
	}

	// Get credential early so we can inject password into inventory if needed
	var cred *models.AnsibleCredential
	if job.CredentialID != nil {
		cred, err = r.credentialService.GetDecryptedCredential(*job.CredentialID)
		if err != nil {
			return fmt.Errorf("failed to get credential: %w", err)
		}
	}

	// Generate inventory file (will inject password if needed)
	inventoryFile, err := r.prepareInventory(ctx, jobDir, job.InventoryID, cred)
	if err != nil {
		return fmt.Errorf("failed to prepare inventory: %w", err)
	}

	// Prepare credentials
	envVars, sshKeyFile, err := r.prepareCredentials(ctx, jobDir, job)
	if err != nil {
		return fmt.Errorf("failed to prepare credentials: %w", err)
	}
	defer func() {
		// Securely delete SSH key file
		if sshKeyFile != "" {
			if err := os.Remove(sshKeyFile); err != nil {
				logger.Warnf("Failed to remove SSH key file %s: %v", sshKeyFile, err)
			}
		}
	}()

	// Build ansible-playbook command and get the working directory
	args, workDir := r.buildAnsibleArgs(job, playbook, playbookDir, inventoryFile, sshKeyFile)

	// Prepare ansible.cfg from stored configuration (project > org priority)
	if err := r.prepareAnsibleConfig(ctx, workDir, job); err != nil {
		// Log warning but don't fail - ansible will use defaults
		logger.Warnf("Failed to prepare ansible.cfg: %v", err)
	}

	// Execute ansible-playbook from the directory containing the playbook
	// This ensures Ansible can find relative paths to roles, group_vars, etc.
	return r.runAnsiblePlaybook(ctx, job, workDir, args, envVars)
}

func (r *AnsibleRunner) preparePlaybook(ctx context.Context, jobDir string, playbook *models.AnsiblePlaybook) (string, error) {
	playbookDir := filepath.Join(jobDir, "playbook")
	if err := os.MkdirAll(playbookDir, 0o755); err != nil { //nolint:gosec // workspace directories need 0o755 for compatibility
		return "", err
	}

	// Check if playbook has a VCS connection (GitHub App integration)
	if playbook.VCSConnectionID != nil && playbook.VCSRepository != "" {
		// Use VCS connection to clone repository
		return r.cloneVCSRepo(ctx, playbookDir, playbook)
	}

	// No VCS connection - check if playbook content is stored in storage
	storageKey := fmt.Sprintf("playbooks/%s/content.tar.gz", playbook.ID.String())
	data, err := r.storageClient.Get(ctx, storageKey)
	if err != nil {
		// No stored content, create a simple playbook from path
		logger.Warnf("No stored playbook content, assuming local path: %s", playbook.PlaybookPath)
		return playbookDir, nil
	}
	// Extract content
	if err := extractTarGz(data, playbookDir); err != nil {
		return "", fmt.Errorf("failed to extract playbook content: %w", err)
	}
	return playbookDir, nil
}

func (r *AnsibleRunner) cloneVCSRepo(ctx context.Context, targetDir string, playbook *models.AnsiblePlaybook) (string, error) {
	return r.cloneVCSRepoGeneric(ctx, targetDir, playbook.VCSConnectionID, playbook.VCSRepository, playbook.VCSBranch)
}

func (r *AnsibleRunner) cloneVCSRepoGeneric(ctx context.Context, targetDir string, vcsConnectionID *uuid.UUID, repository, branch string) (string, error) {
	if vcsConnectionID == nil {
		return "", fmt.Errorf("VCS connection ID is required")
	}
	if repository == "" {
		return "", fmt.Errorf("VCS repository is required")
	}

	// Get VCS connection
	vcsConn, err := r.vcsConnectionRepo.GetByID(*vcsConnectionID)
	if err != nil {
		return "", fmt.Errorf("failed to get VCS connection: %w", err)
	}

	// Use provider registry to get a fresh token and build the clone URL
	provider, err := r.vcsRegistry.GetProvider(vcsConn)
	if err != nil {
		return "", fmt.Errorf("unsupported VCS provider %s: %w", vcsConn.Provider, err)
	}

	token, err := provider.GetFreshToken(ctx, vcsConn)
	if err != nil {
		logger.Warnf("Failed to get fresh token for VCS connection %s: %v", vcsConn.ID, err)
	}

	repoURL := provider.BuildCloneURL(vcsConn, token, repository)

	if branch == "" {
		branch = "main"
	}

	// Use VCS client to clone
	vcsClient := vcsGitHub.NewClient("")
	if err := vcsClient.CloneRepository(ctx, repoURL, branch, targetDir); err != nil {
		return "", fmt.Errorf("failed to clone repository: %w", err)
	}

	logger.Infof("Successfully cloned repository: %s (branch: %s)", repository, branch)
	return targetDir, nil
}

// installGalaxyRequirements checks for requirements.yml in the playbook directory
// and installs any Galaxy collections/roles defined there.
// Uses a persistent cache directory to avoid re-downloading collections.
func (r *AnsibleRunner) installGalaxyRequirements(ctx context.Context, job *models.AnsibleJob, playbookDir string) error {
	// Common locations for requirements files
	requirementsPaths := []string{
		filepath.Join(playbookDir, "requirements.yml"),
		filepath.Join(playbookDir, "collections", "requirements.yml"),
		filepath.Join(playbookDir, "roles", "requirements.yml"),
	}

	var foundPath string
	for _, path := range requirementsPaths {
		if _, err := os.Stat(path); err == nil {
			foundPath = path
			break
		}
	}

	if foundPath == "" {
		// No requirements file found, nothing to do
		return nil
	}

	logger.Infof("Found Galaxy requirements at: %s", foundPath)

	// Create persistent cache directories for collections and roles
	// These persist between job runs to avoid re-downloading
	collectionsCache := "/home/iac/galaxy-cache/collections"
	rolesCache := "/home/iac/galaxy-cache/roles"

	if err := os.MkdirAll(collectionsCache, 0o755); err != nil { //nolint:gosec // cache directories need 0o755 for compatibility
		logger.Warnf("Failed to create collections cache dir: %v", err)
		collectionsCache = filepath.Join(playbookDir, "collections")
	}
	if err := os.MkdirAll(rolesCache, 0o755); err != nil { //nolint:gosec // cache directories need 0o755 for compatibility
		logger.Warnf("Failed to create roles cache dir: %v", err)
		rolesCache = filepath.Join(playbookDir, "roles")
	}

	// Create event for Galaxy installation
	event := &models.AnsibleJobEvent{
		JobID:   job.ID,
		Event:   "galaxy_install",
		Counter: 0,
		Task:    "Installing Galaxy Requirements",
		Stdout:  fmt.Sprintf("Installing collections/roles from %s (using cache: %s)\n", filepath.Base(foundPath), collectionsCache),
	}
	if err := r.jobRepo.CreateEvent(event); err != nil {
		logger.Warnf("Failed to create Galaxy install event: %v", err)
	}

	// Run ansible-galaxy collection install with --upgrade to check for new versions
	// Using cache directory for persistence
	galaxyCmd := exec.CommandContext(ctx, "ansible-galaxy", "collection", "install", "-r", foundPath, "-p", collectionsCache) //nolint:gosec // intentional: executing ansible command
	galaxyCmd.Dir = playbookDir

	output, err := galaxyCmd.CombinedOutput()
	if err != nil {
		// Create error event
		errorEvent := &models.AnsibleJobEvent{
			JobID:   job.ID,
			Event:   "galaxy_install_failed",
			Counter: 1,
			Task:    "Galaxy Installation Failed",
			Stderr:  string(output),
			Failed:  true,
		}
		if createErr := r.jobRepo.CreateEvent(errorEvent); createErr != nil {
			logger.Warnf("Failed to create error event: %v", createErr)
		}
		return fmt.Errorf("ansible-galaxy collection install failed: %w: %s", err, string(output))
	}

	// Also install roles if the requirements file contains roles
	rolesCmd := exec.CommandContext(ctx, "ansible-galaxy", "role", "install", "-r", foundPath, "-p", rolesCache) //nolint:gosec // intentional: executing ansible command
	rolesCmd.Dir = playbookDir
	rolesOutput, rolesErr := rolesCmd.CombinedOutput()

	// Log results
	resultStdout := string(output)
	if rolesErr == nil && len(rolesOutput) > 0 {
		resultStdout += "\n" + string(rolesOutput)
	}

	// Create success event
	successEvent := &models.AnsibleJobEvent{
		JobID:   job.ID,
		Event:   "galaxy_install_complete",
		Counter: 2,
		Task:    "Galaxy Requirements Installed",
		Stdout:  resultStdout,
	}
	if err := r.jobRepo.CreateEvent(successEvent); err != nil {
		logger.Warnf("Failed to create success event: %v", err)
	}

	logger.Infof("Galaxy requirements installed successfully to cache")
	return nil
}

func (r *AnsibleRunner) prepareInventory(ctx context.Context, jobDir string, inventoryID uuid.UUID, cred *models.AnsibleCredential) (string, error) {
	// Generate inventory in JSON format
	inventoryJSON, err := r.inventoryService.GenerateInventoryJSON(inventoryID)
	if err != nil {
		return "", fmt.Errorf("failed to generate inventory: %w", err)
	}

	// If we have a Machine SSH credential with a password, inject it into the inventory
	if cred != nil && cred.Type == models.CredentialTypeMachineSSH && cred.Password != "" {
		inventoryJSON, err = r.injectPasswordIntoInventory(inventoryJSON, cred.Password)
		if err != nil {
			return "", fmt.Errorf("failed to inject password into inventory: %w", err)
		}
	}

	// Write inventory to file
	inventoryFile := filepath.Join(jobDir, "inventory.json")
	if err := os.WriteFile(inventoryFile, []byte(inventoryJSON), 0o600); err != nil {
		return "", fmt.Errorf("failed to write inventory file: %w", err)
	}

	return inventoryFile, nil
}

// injectPasswordIntoInventory adds ansible_password to all hosts in the inventory JSON
func (r *AnsibleRunner) injectPasswordIntoInventory(inventoryJSON, password string) (string, error) {
	var inventory map[string]interface{}
	if err := json.Unmarshal([]byte(inventoryJSON), &inventory); err != nil {
		return "", fmt.Errorf("failed to parse inventory JSON: %w", err)
	}

	// Iterate through all groups in the inventory
	for _, groupData := range inventory {
		groupMap, ok := groupData.(map[string]interface{})
		if !ok {
			continue
		}

		// Check if this group has a "hosts" field
		if hosts, exists := groupMap["hosts"]; exists {
			hostsMap, ok := hosts.(map[string]interface{})
			if !ok {
				continue
			}

			// Add ansible_password to each host
			for hostName, hostVars := range hostsMap {
				if hostVars == nil {
					// If host has no vars, create a new map
					hostsMap[hostName] = map[string]interface{}{
						"ansible_password": password,
					}
				} else if hostVarsMap, ok := hostVars.(map[string]interface{}); ok {
					// If host has vars, add password to existing vars
					hostVarsMap["ansible_password"] = password
				}
			}
		}
	}

	// Convert back to JSON
	modifiedJSON, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal inventory JSON: %w", err)
	}

	return string(modifiedJSON), nil
}

func (r *AnsibleRunner) prepareCredentials(ctx context.Context, jobDir string, job *models.AnsibleJob) (map[string]string, string, error) {
	envVars := make(map[string]string)
	var sshKeyFile string

	if job.CredentialID == nil {
		return envVars, "", nil
	}

	// Get decrypted credential
	cred, err := r.credentialService.GetDecryptedCredential(*job.CredentialID)
	if err != nil {
		return nil, "", fmt.Errorf("failed to get credential: %w", err)
	}

	switch cred.Type {
	case models.CredentialTypeSSH, models.CredentialTypeMachineSSH:
		// Write SSH private key to file
		if cred.SSHPrivateKey != "" {
			sshKeyFile = filepath.Join(jobDir, "ssh_key")
			if err := os.WriteFile(sshKeyFile, []byte(cred.SSHPrivateKey), 0o600); err != nil {
				return nil, "", fmt.Errorf("failed to write SSH key: %w", err)
			}
		}

		// Set username if provided
		if cred.Username != "" {
			envVars["ANSIBLE_REMOTE_USER"] = cred.Username
		}

		// Set SSH passphrase if provided
		if cred.SSHPassphrase != "" {
			// Note: SSH_ASKPASS handling would require more complex setup
			// For now, we assume unencrypted keys or use ssh-agent
			logger.Warnf("SSH passphrase handling not fully implemented")
		}

	case models.CredentialTypeVault:
		// Write vault password to file
		if cred.VaultPassword != "" {
			vaultPassFile := filepath.Join(jobDir, "vault_pass")
			if err := os.WriteFile(vaultPassFile, []byte(cred.VaultPassword), 0o600); err != nil {
				return nil, "", fmt.Errorf("failed to write vault password: %w", err)
			}
			envVars["ANSIBLE_VAULT_PASSWORD_FILE"] = vaultPassFile
		}

	case models.CredentialTypeAWSAccessKey:
		if cred.AWSAccessKeyID != "" {
			envVars["AWS_ACCESS_KEY_ID"] = cred.AWSAccessKeyID
		}
		if cred.AWSSecretAccessKey != "" {
			envVars["AWS_SECRET_ACCESS_KEY"] = cred.AWSSecretAccessKey
		}

	case models.CredentialTypeAzure:
		if cred.AzureTenantID != "" {
			envVars["AZURE_TENANT_ID"] = cred.AzureTenantID
		}
		if cred.AzureClientID != "" {
			envVars["AZURE_CLIENT_ID"] = cred.AzureClientID
		}
		if cred.AzureClientSecret != "" {
			envVars["AZURE_CLIENT_SECRET"] = cred.AzureClientSecret
		}

	case models.CredentialTypeGCP:
		if cred.GCPServiceAccount != "" {
			gcpCredsFile := filepath.Join(jobDir, "gcp_credentials.json")
			if err := os.WriteFile(gcpCredsFile, []byte(cred.GCPServiceAccount), 0o600); err != nil {
				return nil, "", fmt.Errorf("failed to write GCP credentials: %w", err)
			}
			envVars["GOOGLE_APPLICATION_CREDENTIALS"] = gcpCredsFile
			envVars["GCP_AUTH_KIND"] = "serviceaccount"
		}

	case models.CredentialTypeVMware:
		// VMware credentials - use username/password fields
		if cred.Username != "" {
			envVars["VMWARE_USER"] = cred.Username
		}
		if cred.Password != "" {
			envVars["VMWARE_PASSWORD"] = cred.Password
		}
	case models.CredentialTypeSCM:
		// SCM credentials are used for repository access (Git, etc.)
		// These are typically handled by the VCS connection, not here
		// But if needed, we could set GIT_ASKPASS or similar
		logger.Infof("SCM credential type detected - repository access should be handled via VCS connection")
	}

	return envVars, sshKeyFile, nil
}

// prepareAnsibleConfig fetches the ansible.cfg from the database (project > org priority) and writes it to the workDir
func (r *AnsibleRunner) prepareAnsibleConfig(ctx context.Context, workDir string, job *models.AnsibleJob) error {
	// We need to get the project to find the organization ID
	project, err := r.projectRepo.GetByID(job.ProjectID)
	if err != nil {
		return fmt.Errorf("failed to get project: %w", err)
	}

	// Try project-level config first (higher priority)
	config, err := r.configRepo.GetByProject(job.ProjectID)
	if err == nil && config != nil && config.ConfigContent != "" {
		configPath := filepath.Join(workDir, "ansible.cfg")
		if err := os.WriteFile(configPath, []byte(config.ConfigContent), 0o644); err != nil { //nolint:gosec // ansible.cfg needs to be readable
			return fmt.Errorf("failed to write project ansible.cfg: %w", err)
		}
		logger.Infof("Using project-level ansible.cfg for job %s", job.ID)
		return nil
	}

	// Fall back to organization-level config
	config, err = r.configRepo.GetByOrganization(project.OrganizationID)
	if err == nil && config != nil && config.ConfigContent != "" {
		configPath := filepath.Join(workDir, "ansible.cfg")
		if err := os.WriteFile(configPath, []byte(config.ConfigContent), 0o644); err != nil { //nolint:gosec // ansible.cfg needs to be readable
			return fmt.Errorf("failed to write org ansible.cfg: %w", err)
		}
		logger.Infof("Using organization-level ansible.cfg for job %s", job.ID)
		return nil
	}

	// No custom config found - Ansible will use its defaults
	logger.Debugf("No custom ansible.cfg found for job %s, using Ansible defaults", job.ID)
	return nil
}

func (r *AnsibleRunner) buildAnsibleArgs(job *models.AnsibleJob, playbook *models.AnsiblePlaybook, playbookDir, inventoryFile, sshKeyFile string) ([]string, string) {
	// Determine playbook path
	playbookPath := playbook.PlaybookPath
	if playbookPath == "" {
		playbookPath = "site.yml"
	}
	// Strip leading slash — playbook paths are relative to the cloned repo root.
	// Azure DevOps file listing returns paths with a leading "/" which would cause
	// filepath.IsAbs() to treat them as absolute system paths instead of repo-relative.
	playbookPath = strings.TrimPrefix(playbookPath, "/")

	// Build absolute path to playbook
	var absolutePlaybookPath string
	var workingDir string

	if filepath.IsAbs(playbookPath) {
		absolutePlaybookPath = playbookPath
		workingDir = filepath.Dir(playbookPath)
	} else {
		absolutePlaybookPath = filepath.Join(playbookDir, playbookPath)
		// Set working directory to the directory containing the playbook
		// This ensures Ansible can find relative paths to roles, group_vars, etc.
		workingDir = filepath.Dir(absolutePlaybookPath)
	}

	args := []string{
		"-i", inventoryFile,
		absolutePlaybookPath,
	}

	// Add SSH key if provided
	if sshKeyFile != "" {
		args = append(args, "--private-key", sshKeyFile)
	}

	// Job type specific options
	switch job.JobType {
	case models.AnsibleJobTypeRun:
		// Normal execution - no special flags needed
	case models.AnsibleJobTypeCheck:
		args = append(args, "--check")
	case models.AnsibleJobTypeSyntax:
		args = append(args, "--syntax-check")
	}

	// Verbosity
	if job.Verbosity > 0 && job.Verbosity <= 5 {
		args = append(args, "-"+strings.Repeat("v", job.Verbosity))
	}

	// Forks
	if job.Forks > 0 {
		args = append(args, "--forks", fmt.Sprintf("%d", job.Forks))
	}

	// Limit
	if job.Limit != "" {
		args = append(args, "--limit", job.Limit)
	}

	// Tags
	if job.Tags != "" {
		args = append(args, "--tags", job.Tags)
	}

	// Skip tags
	if job.SkipTags != "" {
		args = append(args, "--skip-tags", job.SkipTags)
	}

	// Become (sudo)
	if job.BecomeEnabled {
		args = append(args, "--become")
	}

	// Diff mode
	if job.DiffMode {
		args = append(args, "--diff")
	}

	// Extra vars
	if len(job.ExtraVars) > 0 {
		extraVarsJSON, err := json.Marshal(job.ExtraVars)
		if err == nil {
			args = append(args, "--extra-vars", string(extraVarsJSON))
		}
	}

	return args, workingDir
}

func (r *AnsibleRunner) runAnsiblePlaybook(ctx context.Context, job *models.AnsibleJob, workDir string, args []string, envVars map[string]string) error {
	// Determine ansible-playbook binary
	ansibleBin := r.config.AnsibleBinaryPath
	if ansibleBin == "" {
		ansibleBin = "ansible-playbook"
	}

	// If specific version requested, use versioned path
	if job.AnsibleVersion != "" {
		versionedPath := fmt.Sprintf("/opt/ansible/%s/bin/ansible-playbook", job.AnsibleVersion)
		if _, err := os.Stat(versionedPath); err == nil {
			ansibleBin = versionedPath
		}
	}

	logger.Infof("Executing: %s %s", ansibleBin, strings.Join(args, " "))

	cmd := exec.CommandContext(ctx, ansibleBin, args...) //nolint:gosec // intentional: executing ansible command
	cmd.Dir = workDir

	// Set up environment
	cmd.Env = os.Environ()

	// Use JSONL callback for streaming output (events as they happen)
	cmd.Env = append(cmd.Env, "ANSIBLE_STDOUT_CALLBACK=ansible.posix.jsonl")
	cmd.Env = append(cmd.Env, "ANSIBLE_LOAD_CALLBACK_PLUGINS=true")
	cmd.Env = append(cmd.Env, "ANSIBLE_HOST_KEY_CHECKING=false")
	cmd.Env = append(cmd.Env, "ANSIBLE_RETRY_FILES_ENABLED=false")

	// Use cached collections directory to avoid re-downloading
	// This includes both the cache and the playbook's local collections
	collectionsPath := "/home/iac/galaxy-cache/collections:" + filepath.Join(workDir, "collections")
	cmd.Env = append(cmd.Env, fmt.Sprintf("ANSIBLE_COLLECTIONS_PATH=%s", collectionsPath))
	// Also set roles path to include cache
	rolesPath := "/home/iac/galaxy-cache/roles:" + filepath.Join(workDir, "roles")
	cmd.Env = append(cmd.Env, fmt.Sprintf("ANSIBLE_ROLES_PATH=%s", rolesPath))

	// Add credential environment variables
	for k, v := range envVars {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	// Create pipes for stdout/stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	// Start the command
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start ansible-playbook: %w", err)
	}

	// Capture output and create events
	var stderrOutput strings.Builder
	var eventCounter int64 // atomic counter for thread safety
	var wg sync.WaitGroup

	// Track stats incrementally
	var hostsOk, hostsChanged, hostsFailed, hostsSkipped, hostsUnreachable, hostsRescued, hostsIgnored int64
	var warningsCount int64 // atomic counter for warnings

	// Process stdout - stream JSONL events line by line
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		// Increase buffer size for large JSON lines
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			if strings.TrimSpace(line) == "" {
				continue
			}

			// Try to parse as JSON
			var eventData map[string]interface{}
			if err := json.Unmarshal([]byte(line), &eventData); err != nil {
				// Not JSON - might be plain text output, store as raw event
				counter := int(atomic.AddInt64(&eventCounter, 1))
				event := &models.AnsibleJobEvent{
					JobID:   job.ID,
					Event:   "runner_output",
					Counter: counter,
					Stdout:  line,
				}
				if err := r.jobRepo.CreateEvent(event); err != nil {
					logger.Warnf("Failed to store output event: %v", err)
				}
				continue
			}

			// Parse JSONL event and store - pass raw line for output display
			r.parseAndStoreJSONLEvent(job.ID, eventData, line, &eventCounter, &hostsOk, &hostsChanged, &hostsFailed, &hostsSkipped, &hostsUnreachable, &hostsRescued, &hostsIgnored, &warningsCount)
		}

		if err := scanner.Err(); err != nil {
			logger.Warnf("Scanner error reading stdout: %v", err)
		}
	}()

	// Process stderr - store lines as events so errors are visible in UI
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			stderrOutput.WriteString(line)
			stderrOutput.WriteString("\n")

			// Store stderr lines as error events
			if strings.TrimSpace(line) != "" {
				// Check for warnings in stderr and count them
				if strings.Contains(line, "[WARNING]:") || strings.Contains(line, "[DEPRECATION WARNING]:") {
					atomic.AddInt64(&warningsCount, 1)
				}

				counter := int(atomic.AddInt64(&eventCounter, 1))
				event := &models.AnsibleJobEvent{
					JobID:     job.ID,
					Event:     "runner_stderr",
					EventData: map[string]interface{}{"stderr": line},
					Counter:   counter,
					Stderr:    line,
				}
				if err := r.jobRepo.CreateEvent(event); err != nil {
					logger.Warnf("Failed to store stderr event: %v", err)
				}
			}
		}
	}()

	// Wait for output goroutines to finish before Wait()
	wg.Wait()

	// Wait for command completion
	err = cmd.Wait()

	stderrStr := stderrOutput.String()

	// Log stderr if any
	if stderrStr != "" {
		logger.Infof("Job %s stderr: %s", job.ID.String(), stderrStr)
	}

	// Update job stats with final counts from streaming
	job.HostsOk = int(atomic.LoadInt64(&hostsOk))
	job.HostsChanged = int(atomic.LoadInt64(&hostsChanged))
	job.HostsFailed = int(atomic.LoadInt64(&hostsFailed))
	job.HostsSkipped = int(atomic.LoadInt64(&hostsSkipped))
	job.HostsUnreachable = int(atomic.LoadInt64(&hostsUnreachable))
	job.HostsRescued = int(atomic.LoadInt64(&hostsRescued))
	job.HostsIgnored = int(atomic.LoadInt64(&hostsIgnored))
	job.WarningsCount = int(atomic.LoadInt64(&warningsCount))
	job.HasWarnings = job.WarningsCount > 0

	// If command failed, include stderr in error message
	if err != nil {
		// If command failed but we have no failures/unreachable counted,
		// it means the playbook failed early (e.g., connection error during Gathering Facts)
		// and never emitted a v2_playbook_on_stats event. In this case, we should
		// increment the failure count to at least 1 so the stats reflect the failure.
		// However, if we have unreachable hosts, that already indicates the failure.
		if job.HostsFailed == 0 && job.HostsUnreachable == 0 {
			job.HostsFailed = 1
			logger.Infof("Job failed early with no stats event, setting failures to 1")
		}

		errMsg := err.Error()
		if stderrStr != "" {
			errMsg = fmt.Sprintf("%s\nStderr: %s", errMsg, stderrStr)
		}
		return fmt.Errorf("%s", errMsg)
	}

	return nil
}

// parseAndStoreJSONLEvent parses a single JSONL event line and stores it
func (r *AnsibleRunner) parseAndStoreJSONLEvent(jobID uuid.UUID, eventData map[string]interface{}, rawLine string, eventCounter *int64, hostsOk, hostsChanged, hostsFailed, hostsSkipped, hostsUnreachable, hostsRescued, hostsIgnored, warningsCount *int64) {
	counter := int(atomic.AddInt64(eventCounter, 1))

	// Extract common fields from JSONL event
	host := ""
	task := ""
	playName := ""
	eventType := "runner_on_ok"
	changed := false
	failed := false
	skipped := false
	unreachable := false
	// stdoutStr will contain parsed task output for the Events tab
	// rawLine will be stored as Stdout for the Output tab display
	stdoutStr := ""

	// Check for v2_playbook_on_stats - this contains the authoritative final stats
	if evtType, ok := eventData["_event"].(string); ok && evtType == "v2_playbook_on_stats" {
		if stats, ok := eventData["stats"].(map[string]interface{}); ok {
			// Aggregate stats from all hosts
			var totalOk, totalChanged, totalFailed, totalSkipped, totalUnreachable, totalRescued, totalIgnored int64
			for _, hostStats := range stats {
				if hs, ok := hostStats.(map[string]interface{}); ok {
					if v, ok := hs["ok"].(float64); ok {
						totalOk += int64(v)
					}
					if v, ok := hs["changed"].(float64); ok {
						totalChanged += int64(v)
					}
					if v, ok := hs["failures"].(float64); ok {
						totalFailed += int64(v)
					}
					if v, ok := hs["skipped"].(float64); ok {
						totalSkipped += int64(v)
					}
					if v, ok := hs["unreachable"].(float64); ok {
						totalUnreachable += int64(v)
					}
					if v, ok := hs["rescued"].(float64); ok {
						totalRescued += int64(v)
					}
					if v, ok := hs["ignored"].(float64); ok {
						totalIgnored += int64(v)
					}
				}
			}
			// Set the final stats (overwrite any previous incremental counts)
			atomic.StoreInt64(hostsOk, totalOk)
			atomic.StoreInt64(hostsChanged, totalChanged)
			atomic.StoreInt64(hostsFailed, totalFailed)
			atomic.StoreInt64(hostsSkipped, totalSkipped)
			atomic.StoreInt64(hostsUnreachable, totalUnreachable)
			atomic.StoreInt64(hostsRescued, totalRescued)
			atomic.StoreInt64(hostsIgnored, totalIgnored)
		}
		// Store the stats event
		event := &models.AnsibleJobEvent{
			JobID:     jobID,
			Event:     "v2_playbook_on_stats",
			EventData: eventData,
			Counter:   counter,
			Stdout:    rawLine + "\n",
		}
		if err := r.jobRepo.CreateEvent(event); err != nil {
			logger.Warnf("Failed to store stats event: %v", err)
		}
		return
	}

	// Try different event formats from ansible.posix.jsonl
	// The JSONL callback outputs different event types

	// Check for host (standard format)
	if h, ok := eventData["host"].(string); ok {
		host = h
	}

	// Check for task name
	if t, ok := eventData["task"].(string); ok {
		task = t
	} else if taskMap, ok := eventData["task"].(map[string]interface{}); ok {
		if name, ok := taskMap["name"].(string); ok {
			task = name
		}
	}

	// Check for play name
	if p, ok := eventData["play"].(string); ok {
		playName = p
	} else if playMap, ok := eventData["play"].(map[string]interface{}); ok {
		if name, ok := playMap["name"].(string); ok {
			playName = name
		}
	}

	// Check status flags - used for event type classification only
	// Actual stats come from v2_playbook_on_stats event at the end
	if v, ok := eventData["changed"].(bool); ok && v {
		changed = true
	}
	if v, ok := eventData["failed"].(bool); ok && v {
		failed = true
		eventType = "runner_on_failed"
	} else if v, ok := eventData["skipped"].(bool); ok && v {
		skipped = true
		eventType = "runner_on_skipped"
	} else if v, ok := eventData["unreachable"].(bool); ok && v {
		unreachable = true
		eventType = "runner_on_unreachable"
	}

	// Extract output from various JSONL fields
	// The jsonl callback can output different structures
	if msg, ok := eventData["msg"].(string); ok && msg != "" {
		// Count warnings in msg
		warningMatches := strings.Count(msg, "[WARNING]:") + strings.Count(msg, "[DEPRECATION WARNING]:")
		if warningMatches > 0 {
			atomic.AddInt64(warningsCount, int64(warningMatches))
		}
		stdoutStr = msg
	}

	// Check for result object (contains module output)
	if result, ok := eventData["result"].(map[string]interface{}); ok {
		// Get stdout from result
		if stdout, ok := result["stdout"].(string); ok && stdout != "" {
			// Count warnings in stdout
			warningMatches := strings.Count(stdout, "[WARNING]:") + strings.Count(stdout, "[DEPRECATION WARNING]:")
			if warningMatches > 0 {
				atomic.AddInt64(warningsCount, int64(warningMatches))
			}
			if stdoutStr != "" {
				stdoutStr += "\n" + stdout
			} else {
				stdoutStr = stdout
			}
		}
		// Get msg from result
		if msg, ok := result["msg"].(string); ok && msg != "" {
			// Count warnings in msg
			warningMatches := strings.Count(msg, "[WARNING]:") + strings.Count(msg, "[DEPRECATION WARNING]:")
			if warningMatches > 0 {
				atomic.AddInt64(warningsCount, int64(warningMatches))
			}
			if stdoutStr != "" {
				stdoutStr += "\n" + msg
			} else {
				stdoutStr = msg
			}
		}
		// Get stdout_lines (some modules use this)
		if stdoutLines, ok := result["stdout_lines"].([]interface{}); ok && len(stdoutLines) > 0 {
			var lines []string
			for _, line := range stdoutLines {
				if s, ok := line.(string); ok {
					lines = append(lines, s)
				}
			}
			if len(lines) > 0 {
				output := strings.Join(lines, "\n")
				if stdoutStr != "" {
					stdoutStr += "\n" + output
				} else {
					stdoutStr = output
				}
			}
		}
	}

	// Skip events without meaningful content (but keep raw line for output stream)
	if host == "" && task == "" && playName == "" && stdoutStr == "" && rawLine == "" {
		return
	}

	// Store parsed task output in EventData for Events tab display
	if stdoutStr != "" {
		eventData["_parsed_output"] = stdoutStr
	}

	// Count warnings in stdout/stderr fields if present in event
	if stderrVal, ok := eventData["stderr"].(string); ok && stderrVal != "" {
		warningMatches := strings.Count(stderrVal, "[WARNING]:") + strings.Count(stderrVal, "[DEPRECATION WARNING]:")
		if warningMatches > 0 {
			atomic.AddInt64(warningsCount, int64(warningMatches))
		}
	}
	if stdoutVal, ok := eventData["stdout"].(string); ok && stdoutVal != "" {
		warningMatches := strings.Count(stdoutVal, "[WARNING]:") + strings.Count(stdoutVal, "[DEPRECATION WARNING]:")
		if warningMatches > 0 {
			atomic.AddInt64(warningsCount, int64(warningMatches))
		}
	}

	event := &models.AnsibleJobEvent{
		JobID:     jobID,
		Event:     eventType,
		EventData: eventData,
		Counter:   counter,
		Host:      host,
		Task:      task,
		Play:      playName,
		Stdout:    rawLine + "\n", // Store raw JSONL line for Output tab
		Changed:   changed,
		Failed:    failed,
		Skipped:   skipped,
	}

	if unreachable {
		event.Failed = true
	}

	if err := r.jobRepo.CreateEvent(event); err != nil {
		logger.Warnf("Failed to store JSONL event: %v", err)
	}
}

func loadConfig() Config {
	config := Config{
		RedisHost:         getEnv("REDIS_HOST", "localhost"),
		RedisPort:         getEnvInt("REDIS_PORT", 6379),
		RedisPassword:     os.Getenv("REDIS_PASSWORD"),
		RedisDB:           getEnvInt("REDIS_DB", 0),
		DatabaseHost:      getEnv("DATABASE_HOST", "localhost"),
		DatabasePort:      getEnvInt("DATABASE_PORT", 5432),
		DatabaseUser:      getEnv("DATABASE_USER", "iac"),
		DatabasePassword:  getEnv("DATABASE_PASSWORD", "iac_password"),
		DatabaseName:      getEnv("DATABASE_NAME", "iac_platform"),
		StorageEndpoint:   getEnv("STORAGE_ENDPOINT", "localhost:9000"),
		StorageAccessKey:  getEnv("STORAGE_ACCESS_KEY", "minioadmin"),
		StorageSecretKey:  getEnv("STORAGE_SECRET_KEY", "minioadmin"),
		StorageBucket:     getEnv("STORAGE_BUCKET", "ansible-artifacts"),
		StorageUseSSL:     getEnv("STORAGE_USE_SSL", "false") == "true",
		WorkspacesDir:     getEnv("WORKSPACES_DIR", "/home/iac/workspaces"),
		AnsibleBinaryPath: os.Getenv("ANSIBLE_BINARY_PATH"),
	}

	// Load encryption key
	encryptionKeyStr := os.Getenv("ANSIBLE_ENCRYPTION_KEY")
	if encryptionKeyStr == "" {
		encryptionKeyStr = os.Getenv("ENCRYPTION_KEY")
	}

	if encryptionKeyStr != "" {
		var err error
		config.EncryptionKey, err = hex.DecodeString(encryptionKeyStr)
		if err != nil {
			logger.Warnf("Failed to decode encryption key as hex, using raw bytes")
			config.EncryptionKey = []byte(encryptionKeyStr)
		}
		// Ensure 32 bytes for AES-256
		if len(config.EncryptionKey) < 32 {
			paddedKey := make([]byte, 32)
			copy(paddedKey, config.EncryptionKey)
			config.EncryptionKey = paddedKey
		} else if len(config.EncryptionKey) > 32 {
			config.EncryptionKey = config.EncryptionKey[:32]
		}
	} else {
		logger.Warn("No encryption key configured - credentials will not be decrypted")
		config.EncryptionKey = make([]byte, 32)
	}

	return config
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		var result int
		if _, err := fmt.Sscanf(value, "%d", &result); err == nil {
			return result
		}
	}
	return defaultValue
}

// extractTarGz extracts a tar.gz archive to the target directory
func extractTarGz(data []byte, targetDir string) error {
	gzr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to create gzip reader: %w", err)
	}
	defer func() {
		if err := gzr.Close(); err != nil {
			logger.Warnf("Failed to close gzip reader: %v", err)
		}
	}()

	tr := tar.NewReader(gzr)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read tar entry: %w", err)
		}

		target := filepath.Join(targetDir, header.Name) //nolint:gosec // path traversal protection below

		// Security: Prevent directory traversal - ensure target is within targetDir
		cleanTarget := filepath.Clean(target)
		cleanTargetDir := filepath.Clean(targetDir)
		if !strings.HasPrefix(cleanTarget, cleanTargetDir+string(filepath.Separator)) && cleanTarget != cleanTargetDir {
			return fmt.Errorf("invalid file path in archive (directory traversal attempt): %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil { //nolint:gosec // archive directory permissions
				return fmt.Errorf("failed to create directory: %w", err)
			}
		case tar.TypeReg:
			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return fmt.Errorf("failed to create parent directory: %w", err)
			}

			// Security: Validate file mode to prevent integer overflow
			fileMode := header.Mode & 0o777 // Only use permission bits (0-777 octal)
			if fileMode > 0o777 {
				fileMode = 0o644 // Default to safe permissions if invalid
			}

			f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, os.FileMode(fileMode)) //nolint:gosec // fileMode is validated above
			if err != nil {
				return fmt.Errorf("failed to create file: %w", err)
			}

			// Security: Limit decompression size to prevent decompression bombs (100MB limit)
			const maxDecompressedSize = 100 * 1024 * 1024 // 100MB
			limitedReader := io.LimitReader(tr, maxDecompressedSize)
			if _, err := io.Copy(f, limitedReader); err != nil {
				if closeErr := f.Close(); closeErr != nil {
					logger.Warnf("Failed to close file after copy error: %v", closeErr)
				}
				return fmt.Errorf("failed to write file: %w", err)
			}
			if err := f.Close(); err != nil {
				logger.Warnf("Failed to close file: %v", err)
			}
		}
	}

	return nil
}
