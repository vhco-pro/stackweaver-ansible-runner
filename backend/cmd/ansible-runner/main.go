// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/michielvha/logger"
	"github.com/michielvha/stackweaver/core/crypto"
	"github.com/michielvha/stackweaver/core/models"
	"github.com/michielvha/stackweaver/core/queue"
	"github.com/michielvha/stackweaver/core/repository"
	"github.com/michielvha/stackweaver/core/security/archive"
	"github.com/michielvha/stackweaver/core/services/ansible"
	"github.com/michielvha/stackweaver/core/services/encryptionkey"
	"github.com/michielvha/stackweaver/core/services/oidc"
	"github.com/michielvha/stackweaver/core/services/vcs"
	"github.com/michielvha/stackweaver/core/storage"
	vcsGitHub "github.com/michielvha/stackweaver/core/vcs/github"
	"gopkg.in/yaml.v3"
)

// parseAzureSubscriptionID extracts the top-level subscription_id from a cloned azure_rm.yml
// inventory file. Returns "" if the file can't be read/parsed or the key is absent.
func parseAzureSubscriptionID(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // path is a runner-controlled per-sync workspace file
	if err != nil {
		return ""
	}
	var doc map[string]interface{}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return ""
	}
	if v, ok := doc["subscription_id"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// upsertEnv sets key=val in a KEY=VALUE env slice, replacing any existing entry for key.
func upsertEnv(env []string, key, val string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = prefix + val
			return env
		}
	}
	return append(env, prefix+val)
}

// lookupOrgAzureSubscriptionID returns the subscription_id from the organization's
// AzureOIDCConfiguration (the same row the runner uses for OIDC token generation), or "" if there
// is no config / repo. Used as the subscription fallback for Azure Workload Identity when the
// inventory file omits subscription_id.
func (r *AnsibleRunner) lookupOrgAzureSubscriptionID(orgID uuid.UUID) string {
	if r.azureOIDCRepo == nil {
		return ""
	}
	configs, err := r.azureOIDCRepo.GetByOrganization(orgID)
	if err != nil || len(configs) == 0 {
		return ""
	}
	return strings.TrimSpace(configs[0].SubscriptionID)
}

// injectAzureOIDCEnv adds the organization's Azure workload-identity environment
// to a playbook run so tasks (e.g. azure.azcollection Key Vault lookups) can
// authenticate via federated OIDC without a static credential. A job-attached
// Azure credential wins: nothing is injected when AZURE_CLIENT_ID is already
// set. Both the azure-identity SDK names (AZURE_TENANT_ID) and the
// azure.azcollection names (AZURE_TENANT, AZURE_FEDERATED_TOKEN) are exported,
// plus ARM_* for Terraform-style consumers — mirroring the self-hosted agent
// artifact environment.
func (r *AnsibleRunner) injectAzureOIDCEnv(job *models.AnsibleJob, jobDir string, envVars map[string]string) {
	if envVars == nil || envVars["AZURE_CLIENT_ID"] != "" {
		return
	}
	if r.azureOIDCRepo == nil || r.oidcTokenService == nil || r.projectRepo == nil || r.orgRepo == nil {
		return
	}
	project, err := r.projectRepo.GetByID(job.ProjectID)
	if err != nil {
		return
	}
	configs, err := r.azureOIDCRepo.GetByOrganization(project.OrganizationID)
	if err != nil || len(configs) == 0 {
		return
	}
	cfg := configs[0]
	org, err := r.orgRepo.GetByID(project.OrganizationID)
	if err != nil {
		return
	}

	token, err := r.oidcTokenService.GenerateWorkloadToken(oidc.WorkloadTokenRequest{
		Audience:         "api://AzureADTokenExchange",
		OrganizationName: org.Name,
		ProjectName:      project.Name,
		ResourceType:     oidc.ResourceTypeJob,
		ResourceName:     job.Name,
		ActionKind:       oidc.ActionRun,
		ActionID:         job.ID.String(),
	})
	if err != nil {
		logger.Warnf("Failed to generate Azure OIDC token for job %s: %v", job.ID, err)
		return
	}

	tokenFile := filepath.Join(jobDir, "azure-oidc-token.jwt")
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		logger.Warnf("Failed to write Azure OIDC token file for job %s: %v", job.ID, err)
		return
	}

	envVars["AZURE_CLIENT_ID"] = cfg.ClientID
	envVars["AZURE_TENANT_ID"] = cfg.TenantID
	envVars["AZURE_TENANT"] = cfg.TenantID
	envVars["AZURE_FEDERATED_TOKEN"] = token
	envVars["AZURE_FEDERATED_TOKEN_FILE"] = tokenFile
	if sub := strings.TrimSpace(cfg.SubscriptionID); sub != "" {
		envVars["AZURE_SUBSCRIPTION_ID"] = sub
		envVars["ARM_SUBSCRIPTION_ID"] = sub
	}
	envVars["ARM_CLIENT_ID"] = cfg.ClientID
	envVars["ARM_TENANT_ID"] = cfg.TenantID
	envVars["ARM_OIDC_TOKEN"] = token
	envVars["ARM_USE_OIDC"] = "true"
	logger.Infof("Injected Azure OIDC workload identity env for job %s (org=%s)", job.ID, org.Name)
}

// AnsibleJobMessage represents the message received from the job queue
type AnsibleJobMessage struct {
	JobID       uuid.UUID `json:"job_id"`
	PlaybookID  uuid.UUID `json:"playbook_id"`
	InventoryID uuid.UUID `json:"inventory_id"`
	JobType     string    `json:"job_type"`
	// CloneURL is a pre-authenticated, token-embedded clone URL for the playbook
	// repo, resolved by the API at enqueue time. When set, the runner clones the
	// playbook with it directly and needs no VCS OAuth credentials. Empty falls
	// back to resolving from the DB.
	CloneURL string `json:"clone_url,omitempty"`
	// Branch accompanies CloneURL on the pre-resolved path.
	Branch string `json:"branch,omitempty"`
}

// PlaybookSyncMessage represents a request to sync a playbook from VCS
type PlaybookSyncMessage struct {
	PlaybookID uuid.UUID `json:"playbook_id"`
	// CloneURL is a pre-authenticated, token-embedded clone URL resolved by the API
	// at enqueue time. When set, the runner clones with it directly and needs no
	// VCS OAuth credentials. Empty falls back to resolving from the DB.
	CloneURL string `json:"clone_url,omitempty"`
	// Branch accompanies CloneURL on the pre-resolved path.
	Branch string `json:"branch,omitempty"`
}

// InventorySyncMessage represents a request to sync a VCS inventory from repository
type InventorySyncMessage struct {
	InventoryID uuid.UUID `json:"inventory_id"`
	// TriggeredBy records what started the sync; recorded in the sync history.
	TriggeredBy string `json:"triggered_by,omitempty"`
	// CloneURL is a pre-authenticated, token-embedded clone URL resolved by the API
	// at enqueue time. When set, the runner clones with it directly and does not
	// need its own VCS OAuth credentials. Empty falls back to resolving from the DB.
	CloneURL string `json:"clone_url,omitempty"`
	// Branch accompanies CloneURL on the pre-resolved path.
	Branch string `json:"branch,omitempty"`
}

// InventorySourceSyncMessage represents a request to sync a dynamic inventory source
type InventorySourceSyncMessage struct {
	SourceID uuid.UUID `json:"source_id"`
	// TriggeredBy records what started the sync (manual | schedule | launch |
	// workflow | webhook); recorded in the sync history. Empty = manual.
	TriggeredBy string `json:"triggered_by,omitempty"`
}

// syncResult holds the outcome of an inventory sync operation
type syncResult struct {
	HostsDiscovered int    // Number of hosts found during sync
	Stderr          string // Stderr output from ansible-inventory (warnings, debug info)
	Commit          string // Resolved commit (VCS file syncs)
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
	EncryptionKey     []byte
	WorkspacesDir     string
	AnsibleBinaryPath string
}

func main() {
	// Initialize logger first (reads LOG_LEVEL from environment)
	logLevel := os.Getenv("LOG_LEVEL")
	logger.Init(logLevel)

	// AKS Workload Identity bridge: the workload-identity webhook injects AZURE_TENANT_ID, but the
	// azure.azcollection azure_rm plugin reads tenant from AZURE_TENANT. Alias it once at startup so
	// every Azure inventory sync (VCS passthrough and dynamic) gets the tenant for free — a pure
	// rename of a webhook-injected var, no file parsing and no StackWeaver config. Subscription is
	// not bridgeable here (the webhook never provides it): dynamic sources inject it from their
	// config; VCS deployments set AZURE_SUBSCRIPTION_ID via the runner pod env (ansibleRunner.env).
	if os.Getenv("AZURE_TENANT") == "" {
		if tid := os.Getenv("AZURE_TENANT_ID"); tid != "" {
			_ = os.Setenv("AZURE_TENANT", tid)
			logger.Info("Bridged AZURE_TENANT_ID -> AZURE_TENANT for Azure Workload Identity")
		}
	}

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
	storageCfg := storage.ConfigFromEnv()
	storageClient, err := storage.NewClient(context.Background(), storageCfg)
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
	// Record sync runs in the history table (Syncs tab)
	inventorySyncRepo := repository.NewAnsibleInventorySyncRepository(db)
	inventorySourceService.SetSyncRepo(inventorySyncRepo)

	// Initialize VCS provider registry for multi-provider clone support
	githubAppManager, err := vcs.NewGitHubAppManager()
	if err != nil {
		logger.Warnf("GitHub App manager not configured: %v", err)
	}
	if githubAppManager == nil || !githubAppManager.IsEnabled() {
		logger.Warn("GitHub App VCS manager is DISABLED (GITHUB_APP_ID / GITHUB_APP_NAME / GITHUB_APP_PRIVATE_KEY[_PATH] not set on this runner); GitHub App clones cannot mint installation tokens and will fail")
	}
	azureDevOpsManager, err := vcs.NewAzureDevOpsManager()
	if err != nil {
		logger.Warnf("Azure DevOps manager not configured: %v", err)
	}
	if azureDevOpsManager == nil || !azureDevOpsManager.IsEnabled() {
		logger.Warn("Azure DevOps VCS manager is DISABLED (AZURE_DEVOPS_CLIENT_ID / AZURE_DEVOPS_CLIENT_SECRET not set on this runner); Azure DevOps OAuth tokens cannot be refreshed, so clones will fail once the stored access token expires (~1h)")
	}
	vcsRegistry := vcs.NewProviderRegistry(githubAppManager, azureDevOpsManager, func(conn *models.VCSConnection) error {
		return vcsConnectionRepo.Update(conn)
	}, cryptoService)

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
		// Fail-fast signal: a localhost issuer can never satisfy Azure OIDC
		// federation (Entra fetches {issuer}/.well-known/openid-configuration over
		// the public internet), so auth_method=oidc inventories would later die
		// with an opaque AADSTS700211. Warn loudly at boot; the sync path itself
		// then refuses with an actionable error. Other auth modes are unaffected.
		if strings.Contains(issuerURL, "localhost") {
			logger.Warnf("OIDC issuer is %q — auth_method=oidc Azure inventories will be rejected by Entra (AADSTS700211). Set oidc.issuerUrl (Helm) / OIDC_ISSUER_URL to the chart's public root host. (managed_identity / workload_identity / credential inventories are unaffected.)", issuerURL)
		}
		oidcTokenService = oidc.NewTokenService(oidcSigningKey, issuerURL)
		logger.Info("OIDC workload identity token service initialized for inventory sync")
		inventorySourceService.SetOIDCServices(azureOIDCRepo, oidcTokenService)
	}

	// Create runner
	ansibleConsumerID, _ := os.Hostname()
	if ansibleConsumerID == "" {
		ansibleConsumerID = "ansible-runner"
	}
	runner := &AnsibleRunner{
		config:                 config,
		queue:                  redisQueue,
		consumerID:             ansibleConsumerID,
		jobRepo:                jobRepo,
		templateRepo:           repository.NewAnsibleJobTemplateRepository(db),
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
		syncRepo:               inventorySyncRepo,
		orgRepo:                orgRepo,
	}

	// Start worker
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// AUD-015: recover any jobs this consumer was mid-processing when it last crashed.
	for _, qn := range []string{"ansible_jobs", "ansible_sync"} {
		if n, rErr := redisQueue.RequeueProcessing(context.Background(), qn, runner.consumerID); rErr != nil {
			logger.Warnf("Failed to recover in-flight %s from processing list: %v", qn, rErr)
		} else if n > 0 {
			logger.Infof("Recovered %d in-flight %s message(s) from a previous crash", n, qn)
		}
	}

	// Job execution worker
	go func() {
		logger.Info("Ansible Runner started, waiting for jobs...")
		for {
			select {
			case <-ctx.Done():
				return
			default:
				runner.consumeReliable(ctx, "ansible_jobs", runner.processJob)
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
				runner.consumeReliable(ctx, "ansible_sync", runner.processSyncJob)
			}
		}
	}()

	// Background janitor: bound the per-project Galaxy cache on the dedicated
	// cache volume by evicting caches idle past the TTL.
	runner.startGalaxyCacheJanitor(ctx)

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
	consumerID             string // stable per-instance ID for reliable-queue processing lists (AUD-015)
	jobRepo                *repository.AnsibleJobRepository
	templateRepo           *repository.AnsibleJobTemplateRepository
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
	// OIDC workload identity for Azure dynamic inventory sync and job runs
	azureOIDCRepo    *repository.AzureOIDCConfigurationRepository
	oidcTokenService *oidc.TokenService
	orgRepo          *repository.OrganizationRepository
	syncRepo         *repository.AnsibleInventorySyncRepository
}

// errPoisonMessage marks a queue message that can never be processed (unmarshalable / unknown
// type) so consumeReliable dead-letters it instead of retrying it forever or dropping it (AUD-015).
var errPoisonMessage = errors.New("unprocessable message")

// consumeReliable BLMOVEs one message to this consumer's processing list, runs the handler, then
// acks it (or dead-letters a poison message). A crash before the ack leaves the message in-flight
// for RequeueProcessing to recover on restart — the previous plain BRPop lost it (AUD-015).
func (r *AnsibleRunner) consumeReliable(ctx context.Context, queueName string, handler func(context.Context, []byte) error) {
	payload, err := r.queue.DequeueReliable(ctx, queueName, r.consumerID, 5*time.Second)
	if err != nil {
		if err != queue.ErrQueueEmpty {
			logger.Infof("Error dequeuing from %s: %v", queueName, err)
			time.Sleep(1 * time.Second)
		}
		return
	}
	perr := handler(ctx, payload)
	if errors.Is(perr, errPoisonMessage) {
		logger.Infof("Dead-lettering unprocessable %s message: %v", queueName, perr)
		if dErr := r.queue.DeadLetter(context.Background(), queueName, r.consumerID, payload); dErr != nil { //nolint:contextcheck
			logger.Warnf("Failed to dead-letter %s message: %v", queueName, dErr)
		}
		return
	}
	if perr != nil {
		logger.Infof("Error processing %s message: %v", queueName, perr)
	}
	if aErr := r.queue.Ack(context.Background(), queueName, r.consumerID, payload); aErr != nil { //nolint:contextcheck
		logger.Warnf("Failed to ack %s message: %v", queueName, aErr)
	}
}

func (r *AnsibleRunner) processJob(ctx context.Context, jobData []byte) error {
	var msg AnsibleJobMessage
	if err := json.Unmarshal(jobData, &msg); err != nil {
		return fmt.Errorf("%w: failed to unmarshal job message: %v", errPoisonMessage, err)
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

	// Execute the job. The pre-resolved clone URL (if any) is threaded through so
	// the playbook clone needs no VCS OAuth credentials on this runner.
	err = r.executeJob(ctx, job, msg.CloneURL, msg.Branch)

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

	// AUD-118: write the terminal result only if the job is still `running`. The previous
	// unconditional whole-struct Update clobbered a concurrent API cancel (which set
	// status=canceled) back to successful/failed. CompleteIfRunning row-locks and re-checks.
	if ok, updateErr := r.jobRepo.CompleteIfRunning(job); updateErr != nil {
		logger.Warnf("Failed to update job completion status: %v", updateErr)
	} else if !ok {
		logger.Infof("Job %s was canceled during execution; leaving canceled status intact", job.ID.String())
	}

	return nil
}

// vcsSyncRunLog composes the sync-history log of a VCS file inventory sync:
// clone/parse context, ansible-inventory diagnostics, and the result summary.
func vcsSyncRunLog(inventory *models.AnsibleInventory, result syncResult) string {
	var b strings.Builder
	if inventory.Type == models.InventoryTypeConstructed {
		b.WriteString("Rebuilding constructed inventory from its input inventories\n")
		b.WriteString("$ ansible-inventory -i <inputs...> -i constructed.yml --list\n\n")
	} else {
		branch := inventory.VCSBranch
		if branch == "" {
			branch = "main"
		}
		fmt.Fprintf(&b, "Cloning %s (branch %s)\n", inventory.VCSRepository, branch)
		if result.Commit != "" {
			fmt.Fprintf(&b, "Parsing %s @ %s\n", inventory.InventoryPath, result.Commit)
		} else {
			fmt.Fprintf(&b, "Parsing %s\n", inventory.InventoryPath)
		}
		b.WriteString("$ ansible-inventory -i " + inventory.InventoryPath + " --list\n\n")
	}
	if result.Stderr != "" {
		b.WriteString(result.Stderr)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "\nDiscovered %d hosts\n", result.HostsDiscovered)
	return b.String()
}

func (r *AnsibleRunner) processSyncJob(ctx context.Context, syncData []byte) error {
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
		err = r.syncPlaybook(ctx, playbook, playbookMsg.CloneURL, playbookMsg.Branch)

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

		// Record the run in the sync history (SourceID nil = VCS file sync)
		trigger := inventoryMsg.TriggeredBy
		if trigger == "" {
			trigger = "manual"
		}
		var syncRun *models.AnsibleInventorySync
		if r.syncRepo != nil {
			startedAt := time.Now()
			syncRun = &models.AnsibleInventorySync{
				InventoryID: inventory.ID,
				Status:      models.InventorySyncStatusRunning,
				TriggeredBy: trigger,
				StartedAt:   &startedAt,
			}
			if createErr := r.syncRepo.Create(syncRun); createErr != nil {
				logger.Warnf("Failed to record inventory sync run: %v", createErr)
				syncRun = nil
			}
		}

		// Execute the sync (constructed inventories rebuild from their inputs;
		// VCS inventories clone and parse their inventory file)
		var result syncResult
		if inventory.Type == models.InventoryTypeConstructed {
			var tail *ansible.SyncRunTail
			if syncRun != nil {
				tail = ansible.NewSyncRunTail(r.syncRepo, syncRun.ID)
			}
			result, err = r.buildConstructedInventory(ctx, inventory, tail)
		} else {
			result, err = r.syncInventory(ctx, inventory, inventoryMsg.CloneURL, inventoryMsg.Branch)
		}

		// Update sync status
		now := time.Now()
		inventory.LastSyncAt = &now

		if err != nil {
			inventory.LastSyncStatus = "failed"
			inventory.LastSyncError = err.Error()
			inventory.LastSyncLog = result.Stderr // preserve stderr even on failure
			logger.Infof("Inventory sync %s failed: %v", inventory.ID.String(), err)
			if syncRun != nil {
				if finishErr := r.syncRepo.Finish(syncRun.ID, models.InventorySyncStatusFailed, vcsSyncRunLog(inventory, result), err.Error(), 0, 0); finishErr != nil {
					logger.Warnf("Failed to finalize inventory sync run: %v", finishErr)
				}
			}
		} else {
			inventory.LastSyncStatus = "successful"
			inventory.LastSyncError = ""
			inventory.LastSyncHostsDiscovered = result.HostsDiscovered
			inventory.LastSyncLog = result.Stderr
			logger.Infof("Inventory sync %s completed successfully (hosts: %d)", inventory.ID.String(), result.HostsDiscovered)
			if result.HostsDiscovered == 0 {
				logger.Warnf("Inventory sync %s: 0 hosts discovered, check plugin configuration and authentication", inventory.ID.String())
			}
			if syncRun != nil {
				if finishErr := r.syncRepo.Finish(syncRun.ID, models.InventorySyncStatusSuccessful, vcsSyncRunLog(inventory, result), "", result.HostsDiscovered, 0); finishErr != nil {
					logger.Warnf("Failed to finalize inventory sync run: %v", finishErr)
				}
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

		trigger := sourceMsg.TriggeredBy
		if trigger == "" {
			trigger = "manual"
		}
		result, err := r.inventorySourceService.SyncInventorySourceTriggered(ctx, sourceMsg.SourceID, trigger)
		if err != nil {
			logger.Infof("Inventory source sync %s failed: %v", sourceMsg.SourceID, err)
		} else {
			logger.Infof("Inventory source sync %s completed successfully (hosts: %d)", sourceMsg.SourceID, result.HostsDiscovered)
		}

		return nil
	}

	return fmt.Errorf("%w: sync message has no recognized type", errPoisonMessage)
}

// snapshotStorageKey is the object-storage key for a playbook's cached snapshot.
// Latest only — overwritten on each sync (no history).
func snapshotStorageKey(playbookID uuid.UUID) string {
	return fmt.Sprintf("playbooks/%s/snapshot.tar.gz", playbookID.String())
}

// captureAndStoreSnapshot clones the playbook repo (preferring the API-resolved
// cloneURL so this runner needs no VCS OAuth credentials), validates the playbook
// file exists, builds a dependency-scoped snapshot tarball, and uploads it to
// object storage. It returns the snapshot bytes and resolved commit. It does NOT
// persist the playbook row — callers set the Cached* metadata via
// recordSnapshotMetadata and persist as appropriate.
func (r *AnsibleRunner) captureAndStoreSnapshot(ctx context.Context, playbook *models.AnsiblePlaybook, cloneURL, branch string) ([]byte, string, error) {
	if playbook.VCSConnectionID == nil {
		return nil, "", fmt.Errorf("playbook has no VCS connection configured")
	}
	if playbook.VCSRepository == "" {
		return nil, "", fmt.Errorf("playbook has no VCS repository configured")
	}

	// Use a unique working directory per capture. A sync-worker sync and a job's
	// inline cache-miss auto-sync can run concurrently for the same playbook (the
	// create-then-launch flow triggers exactly this), and a shared
	// ansible-snapshot/<id> path makes them collide: git clone refuses a non-empty
	// destination, and one capture's deferred cleanup yanks the other's clone.
	snapshotBase := filepath.Join(r.config.WorkspacesDir, "ansible-snapshot")
	if err := os.MkdirAll(snapshotBase, 0o750); err != nil {
		return nil, "", fmt.Errorf("failed to create snapshot base dir: %w", err)
	}
	syncDir, err := os.MkdirTemp(snapshotBase, playbook.ID.String()+"-")
	if err != nil {
		return nil, "", fmt.Errorf("failed to create snapshot dir: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(syncDir); err != nil {
			logger.Warnf("Failed to remove snapshot dir %s: %v", syncDir, err)
		}
	}()

	// Clone into a fresh subdir of the unique working dir (git clone wants a
	// non-existent or empty destination). Prefer the pre-authenticated URL; fall
	// back to the DB connection.
	cloneTarget := filepath.Join(syncDir, "repo")
	var repoDir string
	if cloneURL != "" {
		if branch == "" {
			branch = playbook.VCSBranch
		}
		repoDir, err = r.cloneVCSRepoWithURL(ctx, cloneTarget, cloneURL, branch)
	} else {
		repoDir, err = r.cloneVCSRepo(ctx, cloneTarget, playbook)
	}
	if err != nil {
		return nil, "", fmt.Errorf("failed to clone repository: %w", err)
	}

	// Verify the playbook file exists (strip leading / for ADO paths).
	if _, statErr := os.Stat(filepath.Join(repoDir, strings.TrimPrefix(playbook.PlaybookPath, "/"))); os.IsNotExist(statErr) {
		return nil, "", fmt.Errorf("playbook file not found at path: %s", playbook.PlaybookPath)
	}

	commitHash, err := r.getLatestCommit(ctx, repoDir)
	if err != nil {
		logger.Warnf("Playbook %s: could not get commit hash: %v", playbook.ID, err)
		commitHash = ""
	}

	data, wholeRepo, err := buildPlaybookSnapshot(repoDir, playbook.PlaybookPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to build playbook snapshot: %w", err)
	}
	if wholeRepo {
		logger.Infof("Playbook %s: snapshot used whole-repo fallback (could not statically scope dependencies)", playbook.ID)
	}

	if err := r.storageClient.Put(ctx, snapshotStorageKey(playbook.ID), data); err != nil {
		return nil, "", fmt.Errorf("failed to store playbook snapshot: %w", err)
	}

	logger.Infof("Captured playbook %s snapshot from %s (branch: %s, commit: %s, %d bytes)",
		playbook.Name, playbook.VCSRepository, playbook.VCSBranch, commitHash, len(data))
	return data, commitHash, nil
}

// recordSnapshotMetadata stamps the playbook's cached-snapshot + sync metadata. When
// persist is true it also writes the row (used by the inline auto-sync path); the
// sync-worker path passes false because its caller persists.
func (r *AnsibleRunner) recordSnapshotMetadata(playbook *models.AnsiblePlaybook, commit string, size int, persist bool) {
	now := time.Now()
	if commit != "" {
		playbook.LastSyncCommit = commit
		playbook.CachedCommit = commit
	}
	playbook.CachedAt = &now
	playbook.CachedSizeBytes = int64(size)
	playbook.LastSyncAt = &now
	playbook.LastSyncStatus = "successful"
	playbook.LastSyncError = ""
	if persist {
		if err := r.playbookRepo.Update(playbook); err != nil {
			logger.Warnf("Failed to persist playbook %s snapshot metadata: %v", playbook.ID, err)
		}
	}
}

// recordSourceModeEvent emits a job-log event describing which playbook source the
// run used. For cached runs it states the snapshot commit and capture time so an
// operator can see at a glance that the job ran captured-at-sync-time code, not the
// remote's current HEAD. It is a no-op for fresh runs and non-VCS playbooks, whose
// output always reflects what was just fetched. The event carries counter 0 so it
// sorts to the top of the combined job output.
func (r *AnsibleRunner) recordSourceModeEvent(playbook *models.AnsiblePlaybook, job *models.AnsibleJob) {
	// Mirror preparePlaybook's mode resolution: only VCS-backed playbooks have a
	// source mode, and an empty or "cached" mode runs from the snapshot.
	if playbook.VCSConnectionID == nil || playbook.VCSRepository == "" {
		return
	}
	if playbook.SourceMode == models.PlaybookSourceModeFresh {
		return
	}

	commit := playbook.CachedCommit
	if commit == "" {
		commit = "unknown"
	} else if len(commit) > 7 {
		commit = commit[:7]
	}
	capturedAt := "an earlier sync"
	if playbook.CachedAt != nil {
		capturedAt = playbook.CachedAt.UTC().Format("2006-01-02 15:04 MST")
	}

	stdout := fmt.Sprintf(
		"Source: cached snapshot — running commit %s captured %s, not the remote's current HEAD. "+
			"To run the latest commit, set the playbook source mode to \"fresh\" or trigger a sync.\n",
		commit, capturedAt,
	)
	event := &models.AnsibleJobEvent{
		JobID:   job.ID,
		Event:   "playbook_source",
		Counter: 0,
		Task:    "Playbook source",
		Stdout:  stdout,
	}
	if err := r.jobRepo.CreateEvent(event); err != nil {
		logger.Warnf("Failed to create playbook-source event for job %s: %v", job.ID, err)
	}
}

func (r *AnsibleRunner) syncPlaybook(ctx context.Context, playbook *models.AnsiblePlaybook, cloneURL, branch string) error {
	data, commit, err := r.captureAndStoreSnapshot(ctx, playbook, cloneURL, branch)
	if err != nil {
		return err
	}
	// Stamp metadata on the struct; the sync-worker caller (processSyncJob) persists.
	r.recordSnapshotMetadata(playbook, commit, len(data), false)
	return nil
}

// buildConstructedInventory materializes a constructed inventory: every input
// inventory is exported to a JSON file (the YAML-plugin dict shape Ansible
// reads natively), a constructed.yml is generated from the inventory's
// SourceVars rules, and `ansible-inventory --list` over all of them produces
// the derived hosts/groups, persisted with full-overwrite semantics (the
// constructed inventory wholly owns its rows).
func (r *AnsibleRunner) buildConstructedInventory(ctx context.Context, inventory *models.AnsibleInventory, tail *ansible.SyncRunTail) (syncResult, error) {
	res := syncResult{}

	inputs, err := r.inventoryRepo.ListConstructedInputs(inventory.ID)
	if err != nil {
		return res, fmt.Errorf("failed to list input inventories: %w", err)
	}
	if len(inputs) == 0 {
		return res, fmt.Errorf("constructed inventory has no input inventories")
	}

	tempDir, err := os.MkdirTemp("", "constructed-inventory-*")
	if err != nil {
		return res, fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			logger.Warnf("Failed to remove temp directory %s: %v", tempDir, err)
		}
	}()

	// Export each input in order; later sources can reference earlier hosts.
	args := []string{}
	for i := range inputs {
		content, err := r.inventoryService.GenerateInventoryJSON(inputs[i].ID)
		if err != nil {
			return res, fmt.Errorf("failed to export input inventory %s: %w", inputs[i].Name, err)
		}
		inputFile := filepath.Join(tempDir, fmt.Sprintf("%02d_input.json", i))
		if err := os.WriteFile(inputFile, []byte(content), 0o600); err != nil {
			return res, fmt.Errorf("failed to write input inventory file: %w", err)
		}
		args = append(args, "-i", inputFile)
	}

	// Generate constructed.yml: user rules + the plugin header. Parsing the
	// user YAML up front surfaces rule errors as a readable failure.
	rules := map[string]interface{}{}
	if strings.TrimSpace(inventory.SourceVars) != "" {
		if err := yaml.Unmarshal([]byte(inventory.SourceVars), &rules); err != nil {
			return res, fmt.Errorf("invalid source_vars YAML: %w", err)
		}
	}
	rules["plugin"] = "ansible.builtin.constructed"
	if _, ok := rules["strict"]; !ok {
		rules["strict"] = false
	}
	constructedYAML, err := yaml.Marshal(rules)
	if err != nil {
		return res, fmt.Errorf("failed to generate constructed.yml: %w", err)
	}
	constructedFile := filepath.Join(tempDir, "constructed.yml")
	if err := os.WriteFile(constructedFile, constructedYAML, 0o600); err != nil {
		return res, fmt.Errorf("failed to write constructed.yml: %w", err)
	}
	args = append(args, "-i", constructedFile, "--list")
	if inventory.ConstructedLimit != "" {
		args = append(args, "--limit", inventory.ConstructedLimit)
	}

	tail.WriteString("Rebuilding constructed inventory from its input inventories\n$ ansible-inventory " + strings.Join(args, " ") + "\n\n")
	cmd := exec.CommandContext(ctx, "ansible-inventory", args...) //nolint:gosec // intentional: executing ansible command
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	// Stream diagnostics into the live tail while the build runs.
	cmd.Stderr = io.MultiWriter(&stderr, tail)
	if err := cmd.Run(); err != nil {
		res.Stderr = stderr.String()
		return res, fmt.Errorf("ansible-inventory failed: %w\nstderr: %s", err, stderr.String())
	}
	res.Stderr = stderr.String()

	var inventoryOutput map[string]interface{}
	if err := json.Unmarshal(stdout.Bytes(), &inventoryOutput); err != nil {
		return res, fmt.Errorf("failed to parse ansible-inventory output: %w", err)
	}

	hostsDiscovered, err := ansible.ProcessInventoryOutputWithOptions(r.inventoryRepo, inventory.ID, inventoryOutput, ansible.SyncOptions{
		OverwriteVars: true,
		PruneUnseen:   true,
	})
	if err != nil {
		return res, fmt.Errorf("failed to persist constructed inventory: %w", err)
	}
	res.HostsDiscovered = hostsDiscovered
	logger.Infof("Constructed inventory %s rebuilt from %d inputs (hosts: %d)", inventory.Name, len(inputs), hostsDiscovered)
	return res, nil
}

func (r *AnsibleRunner) syncInventory(ctx context.Context, inventory *models.AnsibleInventory, cloneURL, branch string) (syncResult, error) {
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

	// Clone the repository. Prefer the pre-authenticated clone URL resolved by the
	// API (so this runner never needs the provider's OAuth credentials); fall back
	// to resolving the URL from the DB connection when one wasn't supplied.
	var repoDir string
	var err error
	if cloneURL != "" {
		repoDir, err = r.cloneVCSRepoWithURL(ctx, syncDir, cloneURL, branch)
	} else {
		repoDir, err = r.cloneVCSRepoGeneric(ctx, syncDir, inventory.VCSConnectionID, inventory.VCSRepository, inventory.VCSBranch)
	}
	if err != nil {
		return res, fmt.Errorf("failed to clone repository: %w", err)
	}

	// Verify inventory file exists
	inventoryFilePath := filepath.Join(repoDir, inventory.InventoryPath)
	if _, err := os.Stat(inventoryFilePath); os.IsNotExist(err) {
		return res, fmt.Errorf("inventory file not found at path: %s", inventory.InventoryPath)
	}

	// Run ansible-inventory --list to parse the inventory file. VCS-backed inventories are pure
	// passthrough: StackWeaver does not inject any Azure auth. The repo file's own auth_source
	// (msi/auto/env/cli) plus the pod runtime — IMDS for Managed Identity, the AKS workload-identity
	// webhook's projected token for Workload Identity (read natively by azure.azcollection >= 3.17.0)
	// — decide how the azure_rm plugin authenticates. We never rewrite the user's azure_rm.yml.
	cmdArgs := []string{"-i", inventoryFilePath, "--list"}
	cmdEnv := os.Environ()
	// Isolate Ansible's home under the per-sync workspace so ansible-inventory
	// works under a read-only root filesystem (it eagerly creates ~/.ansible/tmp
	// on import).
	cmdEnv = append(cmdEnv, "ANSIBLE_HOME="+filepath.Join(syncDir, ".ansible"))

	// Azure Workload Identity needs the subscription in AZURE_SUBSCRIPTION_ID (the azure_rm OIDC
	// path doesn't read the inventory file's subscription_id there). Resolution order:
	//   1. subscription_id in the cloned azure_rm.yml (explicit per-inventory override → multi-sub),
	//   2. the org's AzureOIDCConfiguration.SubscriptionID (already stored; same row used for OIDC),
	//   3. the inherited env (e.g. AZURE_SUBSCRIPTION_ID set on the runner pod via ansibleRunner.env).
	// Tags-only inventory files (no subscription_id) are the common case, so (2) is what makes
	// Workload Identity work for an org without any per-inventory or per-deployment config.
	sub := parseAzureSubscriptionID(inventoryFilePath)
	if sub == "" {
		sub = r.lookupOrgAzureSubscriptionID(inventory.OrganizationID)
	}
	if sub != "" {
		cmdEnv = upsertEnv(cmdEnv, "AZURE_SUBSCRIPTION_ID", sub)
	}

	cmd := exec.CommandContext(ctx, "ansible-inventory", cmdArgs...) //nolint:gosec // intentional: executing ansible command
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
	hostsDiscovered, err := ansible.ProcessInventoryOutput(r.inventoryRepo, inventory.ID, inventoryOutput)
	if err != nil {
		return res, fmt.Errorf("failed to process inventory output: %w", err)
	}
	res.HostsDiscovered = hostsDiscovered

	// Get the latest commit hash
	commitHash, err := r.getLatestCommit(ctx, repoDir)
	if err != nil {
		logger.Warnf("Could not get commit hash: %v", err)
	}
	res.Commit = commitHash

	logger.Infof("Successfully synced inventory %s from %s (branch: %s, path: %s, commit: %s, hosts: %d)",
		inventory.Name, inventory.VCSRepository, inventory.VCSBranch, inventory.InventoryPath, commitHash, hostsDiscovered)

	return res, nil
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

// adHocPlaybookFileName is the generated transient playbook for ad hoc jobs.
const adHocPlaybookFileName = "adhoc.yml"

// writeAdHocPlaybook materializes the shared transient ad hoc playbook into
// the job dir so ad hoc commands reuse the whole playbook pipeline: streaming
// output, events, statistics, and the jobs UI.
func writeAdHocPlaybook(jobDir string, job *models.AnsibleJob) error {
	content, err := ansible.BuildAdHocPlaybook(job.Module, job.ModuleArgs)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(jobDir, adHocPlaybookFileName), content, 0o600)
}

func (r *AnsibleRunner) executeJob(ctx context.Context, job *models.AnsibleJob, playbookCloneURL, playbookBranch string) error {
	// Create a unique per-job scratch directory under the workspaces volume.
	// Using os.MkdirTemp (rather than a deterministic ansible-jobs/{id} path)
	// makes leftover credentials from a prior run impossible to inherit even if
	// that run's cleanup was skipped, and prevents collisions between retries of
	// the same job id. This mirrors the self-hosted agent mode's ephemeral model.
	if err := os.MkdirAll(r.config.WorkspacesDir, 0o755); err != nil { //nolint:gosec // workspace root needs 0o755 for compatibility
		return fmt.Errorf("failed to create workspaces directory: %w", err)
	}
	jobDir, mkErr := os.MkdirTemp(r.config.WorkspacesDir, fmt.Sprintf("ansible-job-%s-", job.ID.String()))
	if mkErr != nil {
		return fmt.Errorf("failed to create job directory: %w", mkErr)
	}
	defer func() {
		// Cleanup job directory after execution (optional, can be disabled for debugging)
		if os.Getenv("ANSIBLE_RUNNER_KEEP_WORKSPACE") != "true" {
			if err := os.RemoveAll(jobDir); err != nil {
				logger.Warnf("Failed to remove job directory %s: %v", jobDir, err)
			}
		}
	}()

	// Get playbook. Ad hoc jobs have none — a transient one-task playbook is
	// generated from module + args instead.
	var playbook *models.AnsiblePlaybook
	var playbookDir string
	if job.JobType == models.AnsibleJobTypeAdHoc {
		playbookDir = jobDir
		if err := writeAdHocPlaybook(jobDir, job); err != nil {
			return fmt.Errorf("failed to generate ad hoc playbook: %w", err)
		}
	} else {
		if job.PlaybookID == nil {
			return fmt.Errorf("job has no playbook")
		}
		var err error
		playbook, err = r.playbookRepo.GetByID(*job.PlaybookID)
		if err != nil {
			return fmt.Errorf("failed to get playbook: %w", err)
		}

		// Prepare playbook files (sync from SCM or download from storage)
		playbookDir, err = r.preparePlaybook(ctx, jobDir, playbook, playbookCloneURL, playbookBranch)
		if err != nil {
			return fmt.Errorf("failed to prepare playbook: %w", err)
		}

		// Surface in the job log when this run used a cached snapshot (a commit captured
		// at sync time) rather than the remote's current HEAD, so an operator can tell at
		// a glance they may be running stale code. preparePlaybook leaves the cached
		// commit/time on the playbook (loaded from the DB on a cache hit, stamped inline
		// on an auto-sync miss).
		r.recordSourceModeEvent(playbook, job)
	}

	// Install Galaxy requirements if requirements.yml exists. Returns the
	// per-job collections/roles directories this run should use.
	galaxyCollections, galaxyRoles, err := r.installGalaxyRequirements(ctx, job, jobDir, playbookDir)
	if err != nil {
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
	inventoryFile, err := r.prepareInventory(ctx, jobDir, job, cred)
	if err != nil {
		return fmt.Errorf("failed to prepare inventory: %w", err)
	}

	// Prepare credentials
	prep, err := r.prepareCredentials(ctx, jobDir, job)
	if err != nil {
		return fmt.Errorf("failed to prepare credentials: %w", err)
	}
	envVars := prep.envVars
	sshKeyFile := prep.sshKeyFile
	r.injectAzureOIDCEnv(job, jobDir, envVars)
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
	// Vault credentials contribute --vault-id arguments.
	args = append(args, prep.extraArgs...)
	// Inject the become password as an extra-vars file when escalating.
	if job.BecomeEnabled && prep.becomePassword != "" {
		becomeVars, merr := json.Marshal(map[string]string{"ansible_become_pass": prep.becomePassword})
		if merr == nil {
			becomeFile := filepath.Join(jobDir, "become_vars.json")
			if werr := os.WriteFile(becomeFile, becomeVars, 0o600); werr == nil {
				args = append(args, "-e", "@"+becomeFile)
			} else {
				logger.Warnf("Failed to write become password file for job %s: %v", job.ID, werr)
			}
		}
	}

	if envVars == nil {
		envVars = map[string]string{}
	}

	// Isolate Ansible's home (tmp, cp, etc.) under the per-job workspace so it
	// works under a read-only root filesystem. Ansible derives DEFAULT_LOCAL_TMP
	// from ANSIBLE_HOME, auto-namespacing a tmp dir per invocation.
	envVars["ANSIBLE_HOME"] = filepath.Join(jobDir, ".ansible")
	// Keep the SSH ControlPath dir short to stay under the ~104-char unix socket
	// path limit; the per-job ANSIBLE_HOME path is far too long for it.
	envVars["ANSIBLE_SSH_CONTROL_PATH_DIR"] = "/tmp/.ansible-cp"

	// ssh writes ~/.ssh (known_hosts) under $HOME. The container runs with a
	// read-only root filesystem, so the default HOME (/home/iac) is not writable
	// and ssh fails with "Could not create directory '/home/iac/.ssh'
	// (Read-only file system)". Point HOME at a writable per-job dir under /tmp.
	// cmd.Env appends these after os.Environ(), and os/exec uses the last value
	// for a duplicate key, so this overrides the inherited HOME.
	sshHome := filepath.Join("/tmp", "ansible-home", job.ID.String())
	if err := os.MkdirAll(sshHome, 0o700); err != nil {
		return fmt.Errorf("failed to create ssh home directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(sshHome); err != nil {
			logger.Warnf("Failed to remove ssh home directory %s: %v", sshHome, err)
		}
	}()
	envVars["HOME"] = sshHome
	// Keep ssh connections multiplexed. By default (AUD-116) we also stop ssh from reading/writing
	// the user known_hosts file and disable strict checking — ephemeral job hosts have no stable
	// host keys. An operator who sets ANSIBLE_HOST_KEY_CHECKING=true on the runner opts into real
	// host-key verification, so we must NOT inject the insecure args (they would defeat it); ssh and
	// the project/org ansible.cfg then govern known_hosts.
	if hostKeyChecking() {
		envVars["ANSIBLE_SSH_ARGS"] = "-o ControlMaster=auto -o ControlPersist=60s"
	} else {
		envVars["ANSIBLE_SSH_ARGS"] = "-o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no -o ControlMaster=auto -o ControlPersist=60s"
	}

	// Point collection/role lookups at this job's installed Galaxy cache plus the
	// playbook-local directories.
	playbookCollections := filepath.Join(workDir, "collections")
	playbookRoles := filepath.Join(workDir, "roles")
	if galaxyCollections != "" {
		envVars["ANSIBLE_COLLECTIONS_PATH"] = galaxyCollections + ":" + playbookCollections
	} else {
		envVars["ANSIBLE_COLLECTIONS_PATH"] = playbookCollections
	}
	if galaxyRoles != "" {
		envVars["ANSIBLE_ROLES_PATH"] = galaxyRoles + ":" + playbookRoles
	} else {
		envVars["ANSIBLE_ROLES_PATH"] = playbookRoles
	}

	// Prepare ansible.cfg from stored configuration (project > org priority)
	if err := r.prepareAnsibleConfig(ctx, workDir, job); err != nil {
		// Log warning but don't fail - ansible will use defaults
		logger.Warnf("Failed to prepare ansible.cfg: %v", err)
	}

	// Execute ansible-playbook from the directory containing the playbook
	// This ensures Ansible can find relative paths to roles, group_vars, etc.
	return r.runAnsiblePlaybook(ctx, job, workDir, args, envVars)
}

func (r *AnsibleRunner) preparePlaybook(ctx context.Context, jobDir string, playbook *models.AnsiblePlaybook, cloneURL, branch string) (string, error) {
	playbookDir := filepath.Join(jobDir, "playbook")
	if err := os.MkdirAll(playbookDir, 0o755); err != nil { //nolint:gosec // workspace directories need 0o755 for compatibility
		return "", err
	}

	// VCS-backed playbook: pick the source per the playbook's source mode.
	if playbook.VCSConnectionID != nil && playbook.VCSRepository != "" {
		mode := playbook.SourceMode
		if mode == "" {
			mode = models.PlaybookSourceModeCached
		}

		// fresh: clone from the remote at runtime (always latest HEAD). Prefer the
		// API-resolved clone URL so this runner needs no VCS OAuth credentials.
		if mode == models.PlaybookSourceModeFresh {
			if cloneURL != "" {
				if branch == "" {
					branch = playbook.VCSBranch
				}
				return r.cloneVCSRepoWithURL(ctx, playbookDir, cloneURL, branch)
			}
			return r.cloneVCSRepo(ctx, playbookDir, playbook)
		}

		// cached: run the stored snapshot; on a cache miss, auto-sync inline (clone +
		// capture + upload) then run from the freshly-built snapshot. After the first
		// run the playbook no longer depends on the VCS being reachable at job time.
		data, err := r.storageClient.Get(ctx, snapshotStorageKey(playbook.ID))
		if err != nil {
			logger.Infof("Playbook %s: no cached snapshot, auto-syncing inline before run", playbook.ID)
			snap, commit, serr := r.captureAndStoreSnapshot(ctx, playbook, cloneURL, branch)
			if serr != nil {
				return "", fmt.Errorf("cached source mode: auto-sync failed: %w", serr)
			}
			r.recordSnapshotMetadata(playbook, commit, len(snap), true)
			data = snap
		} else {
			logger.Infof("Playbook %s: running cached snapshot (commit %s)", playbook.ID, playbook.CachedCommit)
		}
		if err := extractTarGz(data, playbookDir); err != nil {
			return "", fmt.Errorf("failed to extract cached playbook snapshot: %w", err)
		}
		return playbookDir, nil
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
		return "", fmt.Errorf("failed to obtain access token for %s VCS connection %s: %w (ensure this runner has the provider's OAuth credentials so tokens can be refreshed)", vcsConn.Provider, vcsConn.ID, err)
	}
	// A token that is still expired after GetFreshToken means the provider could not
	// refresh it (e.g. the runner's OAuth manager is disabled because its credentials
	// were not injected). Fail loudly with an actionable message instead of cloning
	// with a stale token and surfacing a generic "Authentication failed" git error.
	if vcsConn.IsExpired() {
		return "", fmt.Errorf("access token for %s VCS connection %s is expired and could not be refreshed; configure the %s OAuth credentials on this runner so it can refresh tokens", vcsConn.Provider, vcsConn.ID, vcsConn.Provider)
	}

	repoURL := provider.BuildCloneURL(vcsConn, token, repository)

	return r.cloneVCSRepoWithURL(ctx, targetDir, repoURL, branch)
}

// cloneVCSRepoWithURL clones using a pre-built, token-embedded clone URL. This is
// the path used when the API has already resolved a fresh URL for us, so no VCS
// OAuth credentials or token refresh are needed on the runner.
func (r *AnsibleRunner) cloneVCSRepoWithURL(ctx context.Context, targetDir, repoURL, branch string) (string, error) {
	if repoURL == "" {
		return "", fmt.Errorf("clone URL is required")
	}
	if branch == "" {
		branch = "main"
	}

	// Use VCS client to clone
	vcsClient := vcsGitHub.NewClient("")
	if err := vcsClient.CloneRepository(ctx, repoURL, branch, targetDir); err != nil {
		return "", fmt.Errorf("failed to clone repository: %w", err)
	}

	logger.Infof("Successfully cloned repository (branch: %s)", branch)
	return targetDir, nil
}

// installGalaxyRequirements checks for requirements.yml in the playbook directory
// and installs any Galaxy collections/roles defined there.
//
// Collections/roles are staged inside the per-job workspace (seeded from a warm,
// per-project cache on the shared workspaces volume) so the run is isolated from
// concurrent cache writes and works under a read-only root filesystem. After a
// successful install the staged result is published back to the per-project
// cache via a temp-dir + atomic rename so future jobs can reuse it.
//
// It returns the collections/roles directories this job should use. When no
// requirements file is present it returns the warm per-project cache paths.
func (r *AnsibleRunner) installGalaxyRequirements(ctx context.Context, job *models.AnsibleJob, jobDir, playbookDir string) (collectionsDir, rolesDir string, err error) {
	// Per-project Galaxy cache on the shared workspaces volume. Namespacing by
	// project keeps one tenant's collections from being served to another.
	cacheBase := filepath.Join(r.config.WorkspacesDir, "galaxy-cache", job.ProjectID.String())
	canonicalCollections := filepath.Join(cacheBase, "collections")
	canonicalRoles := filepath.Join(cacheBase, "roles")

	// Common locations for requirements files
	requirementsPaths := []string{
		filepath.Join(playbookDir, "requirements.yml"),
		filepath.Join(playbookDir, "collections", "requirements.yml"),
		filepath.Join(playbookDir, "roles", "requirements.yml"),
	}

	var foundPath string
	for _, path := range requirementsPaths {
		if _, statErr := os.Stat(path); statErr == nil {
			foundPath = path
			break
		}
	}

	if foundPath == "" {
		// No requirements file found; reuse the warm per-project cache if present.
		return canonicalCollections, canonicalRoles, nil
	}

	logger.Infof("Found Galaxy requirements at: %s", foundPath)

	// Stage the install inside the per-job workspace, seeded from the warm cache.
	// The job runs against this isolated copy, so a concurrent cache publish can
	// never pull collections out from under it.
	stageCollections := filepath.Join(jobDir, "galaxy", "collections")
	stageRoles := filepath.Join(jobDir, "galaxy", "roles")
	if mkErr := os.MkdirAll(filepath.Join(jobDir, "galaxy"), 0o755); mkErr != nil { //nolint:gosec // workspace directories need 0o755 for compatibility
		return "", "", fmt.Errorf("failed to create galaxy staging dir: %w", mkErr)
	}
	if seedErr := seedGalaxyCache(canonicalCollections, stageCollections); seedErr != nil {
		logger.Warnf("Failed to seed collections cache: %v", seedErr)
	}
	if seedErr := seedGalaxyCache(canonicalRoles, stageRoles); seedErr != nil {
		logger.Warnf("Failed to seed roles cache: %v", seedErr)
	}
	if mkErr := os.MkdirAll(stageCollections, 0o755); mkErr != nil { //nolint:gosec // workspace directories need 0o755 for compatibility
		return "", "", fmt.Errorf("failed to create collections staging dir: %w", mkErr)
	}
	if mkErr := os.MkdirAll(stageRoles, 0o755); mkErr != nil { //nolint:gosec // workspace directories need 0o755 for compatibility
		return "", "", fmt.Errorf("failed to create roles staging dir: %w", mkErr)
	}

	// Create event for Galaxy installation
	event := &models.AnsibleJobEvent{
		JobID:   job.ID,
		Event:   "galaxy_install",
		Counter: 0,
		Task:    "Installing Galaxy Requirements",
		Stdout:  fmt.Sprintf("Installing collections/roles from %s\n", filepath.Base(foundPath)),
	}
	if createErr := r.jobRepo.CreateEvent(event); createErr != nil {
		logger.Warnf("Failed to create Galaxy install event: %v", createErr)
	}

	// Run ansible-galaxy collection install into the staged directory.
	galaxyCmd := exec.CommandContext(ctx, "ansible-galaxy", "collection", "install", "-r", foundPath, "-p", stageCollections) //nolint:gosec // intentional: executing ansible command
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
		return "", "", fmt.Errorf("ansible-galaxy collection install failed: %w: %s", err, string(output))
	}

	// Also install roles if the requirements file contains roles
	rolesCmd := exec.CommandContext(ctx, "ansible-galaxy", "role", "install", "-r", foundPath, "-p", stageRoles) //nolint:gosec // intentional: executing ansible command
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
	if createErr := r.jobRepo.CreateEvent(successEvent); createErr != nil {
		logger.Warnf("Failed to create success event: %v", createErr)
	}

	// Publish the freshly installed cache back to the shared per-project cache for
	// future jobs. Best-effort: failures here never fail the job.
	if pubErr := publishGalaxyCache(stageCollections, canonicalCollections); pubErr != nil {
		logger.Warnf("Failed to publish collections cache: %v", pubErr)
	}
	if pubErr := publishGalaxyCache(stageRoles, canonicalRoles); pubErr != nil {
		logger.Warnf("Failed to publish roles cache: %v", pubErr)
	}

	logger.Infof("Galaxy requirements installed successfully")
	return stageCollections, stageRoles, nil
}

// seedGalaxyCache copies a warm cache directory into a fresh staging directory so
// ansible-galaxy can reuse already-downloaded collections. It is a no-op when the
// source cache does not yet exist. The destination must not already exist.
func seedGalaxyCache(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil || !info.IsDir() {
		return nil //nolint:nilerr // no warm cache to seed from is not an error
	}
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		return fmt.Errorf("seed %s -> %s: %w", src, dst, err)
	}
	return nil
}

// publishGalaxyCache atomically publishes a freshly installed staging directory to
// the shared per-project cache using a temp-dir copy followed by an atomic rename.
// It tolerates concurrent publishers without corrupting the cache.
func publishGalaxyCache(src, dst string) error {
	if info, err := os.Stat(src); err != nil || !info.IsDir() {
		return nil //nolint:nilerr // nothing staged to publish
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { //nolint:gosec // cache directories need 0o755 for compatibility
		return fmt.Errorf("create cache parent: %w", err)
	}

	// Copy the staged tree to a sibling temp dir on the same filesystem so the
	// final swap is an atomic rename.
	tmp := dst + ".tmp-" + uuid.NewString()
	if err := os.CopyFS(tmp, os.DirFS(src)); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("stage cache copy: %w", err)
	}

	// rename(2) onto a non-empty directory fails, so move any existing cache aside
	// first, then publish, then drop the old copy.
	old := dst + ".old-" + uuid.NewString()
	renamedAside := false
	if err := os.Rename(dst, old); err == nil {
		renamedAside = true
	} else if !errors.Is(err, os.ErrNotExist) {
		// A concurrent publisher already replaced dst; treat its result as
		// authoritative and discard our temp copy.
		_ = os.RemoveAll(tmp)
		return nil //nolint:nilerr // concurrent publish won; not a failure
	}

	if err := os.Rename(tmp, dst); err != nil {
		_ = os.RemoveAll(tmp)
		if renamedAside {
			_ = os.Rename(old, dst) // best-effort restore
		}
		return fmt.Errorf("publish cache: %w", err)
	}

	if renamedAside {
		_ = os.RemoveAll(old)
	}
	return nil
}

// galaxyCacheTTL is how long an idle per-project Galaxy cache is kept before the
// janitor evicts it. Configurable via GALAXY_CACHE_TTL_DAYS; 0 disables eviction.
func galaxyCacheTTL() time.Duration {
	days := getEnvInt("GALAXY_CACHE_TTL_DAYS", 14)
	if days <= 0 {
		return 0
	}
	return time.Duration(days) * 24 * time.Hour
}

// startGalaxyCacheJanitor periodically evicts per-project Galaxy caches that have
// gone idle past the TTL, bounding growth on the dedicated cache volume. The
// cache is regenerable (collections re-download on demand), so eviction never
// loses anything a job needs. A project's cache mtime is refreshed on every
// (re)publish, so an actively used cache is never evicted.
func (r *AnsibleRunner) startGalaxyCacheJanitor(ctx context.Context) {
	ttl := galaxyCacheTTL()
	if ttl == 0 {
		logger.Info("Galaxy cache eviction disabled (GALAXY_CACHE_TTL_DAYS<=0)")
		return
	}
	go func() {
		r.pruneGalaxyCache(ttl) // sweep once at startup
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.pruneGalaxyCache(ttl)
			}
		}
	}()
}

// pruneGalaxyCache removes per-project cache directories not refreshed within ttl.
func (r *AnsibleRunner) pruneGalaxyCache(ttl time.Duration) {
	base := filepath.Join(r.config.WorkspacesDir, "galaxy-cache")
	entries, err := os.ReadDir(base)
	if err != nil {
		return // no cache yet — nothing to prune
	}
	cutoff := time.Now().Add(-ttl)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, infoErr := e.Info()
		if infoErr != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		p := filepath.Join(base, e.Name())
		if rmErr := os.RemoveAll(p); rmErr != nil {
			logger.Warnf("Galaxy cache janitor: failed to evict %s: %v", p, rmErr)
			continue
		}
		logger.Infof("Galaxy cache janitor: evicted idle project cache %s (last refreshed %s)", e.Name(), info.ModTime().Format(time.RFC3339))
	}
}

func (r *AnsibleRunner) prepareInventory(ctx context.Context, jobDir string, job *models.AnsibleJob, cred *models.AnsibleCredential) (string, error) {
	// Generate inventory in JSON format
	inventoryJSON, err := r.inventoryService.GenerateInventoryJSON(job.InventoryID)
	if err != nil {
		return "", fmt.Errorf("failed to generate inventory: %w", err)
	}

	// Job slicing: keep only this slice's modulo-partition of the hosts.
	if job.SliceCount > 1 {
		inventoryJSON, err = ansible.FilterInventoryJSONForSlice(inventoryJSON, job.SliceNumber, job.SliceCount)
		if err != nil {
			return "", fmt.Errorf("failed to slice inventory: %w", err)
		}
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

// preparedCredentials is everything the attached credentials contribute to an
// ansible-playbook invocation.
type preparedCredentials struct {
	envVars    map[string]string
	sshKeyFile string
	// extraArgs are appended to the ansible-playbook command line (e.g.
	// --vault-id label@file for each attached vault credential).
	extraArgs []string
	// becomePassword is injected as ansible_become_pass via a 0600 extra-vars
	// file when the job runs with privilege escalation.
	becomePassword string
}

// prepareCredentials resolves and applies every credential attached to the job:
// the template's multi-credential set (AWX-style: one per type, multiple vaults
// with distinct vault IDs) plus the job's own credential (the legacy single
// machine credential, applied last so a launch-time override wins).
func (r *AnsibleRunner) prepareCredentials(ctx context.Context, jobDir string, job *models.AnsibleJob) (*preparedCredentials, error) {
	prep := &preparedCredentials{envVars: make(map[string]string)}

	var credIDs []uuid.UUID
	if job.TemplateID != nil && r.templateRepo != nil {
		ids, err := r.templateRepo.ListCredentialIDs(*job.TemplateID)
		if err != nil {
			logger.Warnf("Failed to list template credentials for job %s: %v", job.ID, err)
		} else {
			credIDs = ids
		}
	}
	if job.CredentialID != nil {
		found := false
		for _, id := range credIDs {
			if id == *job.CredentialID {
				found = true
				break
			}
		}
		if !found {
			credIDs = append(credIDs, *job.CredentialID)
		}
	}

	vaultCount := 0
	for _, id := range credIDs {
		cred, err := r.credentialService.GetDecryptedCredential(id)
		if err != nil {
			return nil, fmt.Errorf("failed to get credential %s: %w", id, err)
		}
		if err := r.applyCredential(prep, cred, jobDir, &vaultCount); err != nil {
			return nil, err
		}
	}
	return prep, nil
}

// applyCredential maps one decrypted credential onto the prepared invocation.
func (r *AnsibleRunner) applyCredential(prep *preparedCredentials, cred *models.AnsibleCredential, jobDir string, vaultCount *int) error {
	envVars := prep.envVars
	switch cred.Type {
	case models.CredentialTypeSSH, models.CredentialTypeMachineSSH:
		// Write SSH private key to file
		if cred.SSHPrivateKey != "" {
			sshKeyFile := filepath.Join(jobDir, "ssh_key")
			// Normalize at write-time too (not only at store-time) so credentials
			// saved before normalization existed are repaired without re-entry.
			// An un-normalized key (CRLF, missing trailing newline, stray
			// whitespace) fails to load with "error in libcrypto".
			key := ansible.NormalizePrivateKey(cred.SSHPrivateKey)
			if err := os.WriteFile(sshKeyFile, []byte(key), 0o600); err != nil {
				return fmt.Errorf("failed to write SSH key: %w", err)
			}
			prep.sshKeyFile = sshKeyFile
		}

		// Set username if provided
		if cred.Username != "" {
			envVars["ANSIBLE_REMOTE_USER"] = cred.Username
		}

		if cred.BecomePassword != "" {
			prep.becomePassword = cred.BecomePassword
		}

		// Set SSH passphrase if provided
		if cred.SSHPassphrase != "" {
			// Note: SSH_ASKPASS handling would require more complex setup
			// For now, we assume unencrypted keys or use ssh-agent
			logger.Warnf("SSH passphrase handling not fully implemented")
		}

	case models.CredentialTypeVault:
		if cred.VaultPassword != "" {
			vaultPassFile := filepath.Join(jobDir, fmt.Sprintf("vault_pass_%d", *vaultCount))
			if err := os.WriteFile(vaultPassFile, []byte(cred.VaultPassword), 0o600); err != nil {
				return fmt.Errorf("failed to write vault password: %w", err)
			}
			*vaultCount++
			if cred.VaultID != "" {
				prep.extraArgs = append(prep.extraArgs, "--vault-id", cred.VaultID+"@"+vaultPassFile)
			} else {
				// Unlabeled vault: keep the historical env-var behavior so a single
				// legacy vault credential keeps working exactly as before.
				envVars["ANSIBLE_VAULT_PASSWORD_FILE"] = vaultPassFile
			}
		}

	case models.CredentialTypeAWSAccessKey:
		if cred.AWSAccessKeyID != "" {
			envVars["AWS_ACCESS_KEY_ID"] = cred.AWSAccessKeyID
		}
		if cred.AWSSecretAccessKey != "" {
			envVars["AWS_SECRET_ACCESS_KEY"] = cred.AWSSecretAccessKey
		}

	case models.CredentialTypeAzure:
		// Export both naming schemes: AZURE_TENANT_ID/AZURE_CLIENT_SECRET for the
		// azure-identity SDK, AZURE_TENANT/AZURE_SECRET for azure.azcollection
		// modules (which read the legacy names).
		if cred.AzureTenantID != "" {
			envVars["AZURE_TENANT_ID"] = cred.AzureTenantID
			envVars["AZURE_TENANT"] = cred.AzureTenantID
		}
		if cred.AzureClientID != "" {
			envVars["AZURE_CLIENT_ID"] = cred.AzureClientID
		}
		if cred.AzureClientSecret != "" {
			envVars["AZURE_CLIENT_SECRET"] = cred.AzureClientSecret
			envVars["AZURE_SECRET"] = cred.AzureClientSecret
		}

	case models.CredentialTypeGCP:
		if cred.GCPServiceAccount != "" {
			gcpCredsFile := filepath.Join(jobDir, "gcp_credentials.json")
			if err := os.WriteFile(gcpCredsFile, []byte(cred.GCPServiceAccount), 0o600); err != nil {
				return fmt.Errorf("failed to write GCP credentials: %w", err)
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

	return nil
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
	// Determine playbook path (ad hoc jobs run the generated transient playbook)
	playbookPath := adHocPlaybookFileName
	if playbook != nil {
		playbookPath = playbook.PlaybookPath
	}
	if playbookPath == "" {
		playbookPath = "site.yml"
	}
	// Strip leading slash, playbook paths are relative to the cloned repo root.
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
	case models.AnsibleJobTypeRun, models.AnsibleJobTypeAdHoc:
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
	// AUD-118: per-job cancellable context so a mid-run API cancel actually stops the playbook.
	// The parent ctx is the long-lived worker context (shutdown-scoped only), so previously a
	// cancel was checked once before execution and never honored during the run — the playbook ran
	// to completion. Poll the job status on a ticker and cancel the exec context when it flips to
	// canceled. Agent mode does the same via an HTTP /status poll; platform mode reads the DB via
	// jobRepo. When the process dies from this cancel, the terminal write is skipped by
	// CompleteIfRunning (status is already canceled), so the canceled state is preserved.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	// Enforce the template's job timeout: the context deadline kills the ansible-playbook process
	// when exceeded.
	if job.TimeoutSeconds > 0 {
		var cancelTimeout context.CancelFunc
		runCtx, cancelTimeout = context.WithTimeout(runCtx, time.Duration(job.TimeoutSeconds)*time.Second)
		defer cancelTimeout()
	}
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				current, err := r.jobRepo.GetByID(job.ID)
				if err != nil {
					continue
				}
				if current.Status == models.AnsibleJobStatusCanceled {
					logger.Infof("Job %s canceled via API, stopping ansible-playbook", job.ID.String())
					cancelRun()
					return
				}
			}
		}
	}()
	ctx = runCtx

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
	cmd.Env = append(cmd.Env, "ANSIBLE_HOST_KEY_CHECKING="+ansibleHostKeyCheckingValue()) // AUD-116: operator-overridable
	cmd.Env = append(cmd.Env, "ANSIBLE_RETRY_FILES_ENABLED=false")

	// ANSIBLE_HOME, ANSIBLE_SSH_CONTROL_PATH_DIR and the collection/role paths are
	// supplied via envVars (set in executeJob) so they reference the per-job
	// workspace under the read-only root filesystem.

	// Add credential and runtime environment variables
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
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			errMsg = fmt.Sprintf("job timeout exceeded (%ds): ansible-playbook was killed", job.TimeoutSeconds)
		}
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
		WorkspacesDir:     getEnv("WORKSPACES_DIR", "/home/iac/workspaces"),
		AnsibleBinaryPath: os.Getenv("ANSIBLE_BINARY_PATH"),
	}

	// Load encryption key
	encryptionKeyStr := os.Getenv("ANSIBLE_ENCRYPTION_KEY")
	if encryptionKeyStr == "" {
		encryptionKeyStr = os.Getenv("ENCRYPTION_KEY")
	}

	// Fail loud on a missing/insecure key instead of silently encrypting
	// credentials under a publicly known zero key (AUD-013). DEV_INSECURE_KEY=1
	// is the explicit escape hatch for LOCAL DEVELOPMENT ONLY.
	config.EncryptionKey = encryptionkey.Resolve(encryptionKeyStr)

	return config
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// hostKeyChecking reports whether SSH host-key checking should be enabled for ansible runs.
// Defaults to false (the historical behavior — ephemeral job hosts have no stable host keys), but
// an operator can set ANSIBLE_HOST_KEY_CHECKING=true on the runner to enforce it (AUD-116 — it was
// hardcoded off, which silently overrode any host_key_checking set in a project/org ansible.cfg).
func hostKeyChecking() bool {
	return strings.EqualFold(os.Getenv("ANSIBLE_HOST_KEY_CHECKING"), "true")
}

// ansibleHostKeyCheckingValue returns "true"/"false" for the ANSIBLE_HOST_KEY_CHECKING env var.
func ansibleHostKeyCheckingValue() string {
	if hostKeyChecking() {
		return "true"
	}
	return "false"
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

		// Validate the entry name BEFORE constructing any filesystem path.
		// archive.SafeEntryName uses filepath.IsLocal which CodeQL's
		// go/zipslip query recognises as a sanitiser (Wave 8 / D3).
		safeName, err := archive.SafeEntryName(header.Name)
		if err != nil {
			return fmt.Errorf("invalid file path in archive: %w", err)
		}
		target := filepath.Join(targetDir, safeName) //nolint:gosec // safeName validated by archive.SafeEntryName

		// Defence-in-depth: also verify the joined path stays under targetDir.
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
