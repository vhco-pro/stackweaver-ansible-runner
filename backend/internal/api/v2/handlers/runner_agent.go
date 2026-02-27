// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package handlers

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/iac-platform/backend/internal/services/ansible"
	"github.com/iac-platform/backend/internal/services/apikey"
	"github.com/iac-platform/backend/internal/services/oidc"
	"github.com/iac-platform/backend/internal/services/variable"
	"github.com/iac-platform/backend/internal/services/vcs"
	"github.com/iac-platform/backend/internal/storage"
	"github.com/iac-platform/backend/pkg/crypto"
	"github.com/michielvha/logger"
	"gorm.io/gorm"
)

// RunnerAgentHandler handles runner agent API endpoints
// These endpoints are used by the runner agents to register, heartbeat, and report job status.
type RunnerAgentHandler struct {
	runnerRepo        *repository.RunnerRepository
	jobExecRepo       *repository.RunnerJobExecutionRepository
	poolRepo          *repository.AgentPoolRepository
	apiKeyService     *apikey.Service
	ansibleJobRepo    *repository.AnsibleJobRepository
	playbookRepo      *repository.AnsiblePlaybookRepository
	inventoryRepo     *repository.AnsibleInventoryRepository
	credentialRepo    *repository.AnsibleCredentialRepository
	ansibleConfigRepo *repository.AnsibleConfigRepository
	inventoryService  *ansible.InventoryService
	vcsRegistry       *vcs.ProviderRegistry
	cryptoService     *crypto.CryptoService
	// Terraform run support
	variableService *variable.Service
	storageClient   storage.Client
	// OIDC workload identity support for self-hosted runners
	azureOIDCRepo    *repository.AzureOIDCConfigurationRepository
	oidcTokenService *oidc.TokenService
	db               *gorm.DB
}

// NewRunnerAgentHandler creates a new runner agent handler
func NewRunnerAgentHandler(
	runnerRepo *repository.RunnerRepository,
	jobExecRepo *repository.RunnerJobExecutionRepository,
	poolRepo *repository.AgentPoolRepository,
	apiKeyService *apikey.Service,
	db *gorm.DB,
) *RunnerAgentHandler {
	return &RunnerAgentHandler{
		runnerRepo:    runnerRepo,
		jobExecRepo:   jobExecRepo,
		poolRepo:      poolRepo,
		apiKeyService: apiKeyService,
		db:            db,
	}
}

// NewRunnerAgentHandlerWithRepos creates a new runner agent handler with all repositories
func NewRunnerAgentHandlerWithRepos(
	runnerRepo *repository.RunnerRepository,
	jobExecRepo *repository.RunnerJobExecutionRepository,
	poolRepo *repository.AgentPoolRepository,
	apiKeyService *apikey.Service,
	ansibleJobRepo *repository.AnsibleJobRepository,
	playbookRepo *repository.AnsiblePlaybookRepository,
	inventoryRepo *repository.AnsibleInventoryRepository,
	credentialRepo *repository.AnsibleCredentialRepository,
	ansibleConfigRepo *repository.AnsibleConfigRepository,
	inventoryService *ansible.InventoryService,
	vcsRegistry *vcs.ProviderRegistry,
	cryptoService *crypto.CryptoService,
	variableService *variable.Service,
	storageClient storage.Client,
	db *gorm.DB,
) *RunnerAgentHandler {
	return &RunnerAgentHandler{
		runnerRepo:        runnerRepo,
		jobExecRepo:       jobExecRepo,
		poolRepo:          poolRepo,
		apiKeyService:     apiKeyService,
		ansibleJobRepo:    ansibleJobRepo,
		playbookRepo:      playbookRepo,
		inventoryRepo:     inventoryRepo,
		credentialRepo:    credentialRepo,
		ansibleConfigRepo: ansibleConfigRepo,
		inventoryService:  inventoryService,
		vcsRegistry:       vcsRegistry,
		cryptoService:     cryptoService,
		variableService:   variableService,
		storageClient:     storageClient,
		db:                db,
	}
}

// SetOIDCServices injects OIDC workload identity services for self-hosted runner OIDC injection.
// Called after handler creation because OIDC is optional (may not be configured).
func (h *RunnerAgentHandler) SetOIDCServices(
	azureOIDCRepo *repository.AzureOIDCConfigurationRepository,
	oidcTokenService *oidc.TokenService,
) {
	h.azureOIDCRepo = azureOIDCRepo
	h.oidcTokenService = oidcTokenService
}

// RegisterRequest is the request body for runner registration
type RegisterRequest struct {
	AgentPoolID          string   `json:"agent_pool_id" binding:"required"`
	Name                 string   `json:"name" binding:"required"`
	Hostname             string   `json:"hostname"`
	OSType               string   `json:"os_type"`
	OSVersion            string   `json:"os_version"`
	AgentVersion         string   `json:"agent_version"`
	TerraformVersion     string   `json:"terraform_version"`
	AnsibleVersion       string   `json:"ansible_version"`
	AvailableCollections []string `json:"available_collections"`
	MaxConcurrentJobs    int      `json:"max_concurrent_jobs"`
	Labels               []string `json:"labels"`
}

// RegisterResponse is the response body for runner registration
type RegisterResponse struct {
	RunnerID            string `json:"runner_id"`
	RunnerAPIKey        string `json:"runner_api_key"`
	PollIntervalSeconds int    `json:"poll_interval_seconds"`
}

// Register registers a new runner
// POST /api/v2/runner/register
// Requires an API key with runner:register scope
func (h *RunnerAgentHandler) Register(c *gin.Context) {
	var req RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}}})
		return
	}

	// Parse agent pool ID
	poolID, err := uuid.Parse(req.AgentPoolID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Invalid agent_pool_id"}}})
		return
	}

	// Get the pool to verify it exists and get org ID
	pool, err := h.poolRepo.GetByID(poolID, false)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Agent pool not found"}}})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		}
		return
	}

	// Get the API key from context (set by auth middleware)
	apiKeyID, exists := c.Get("api_key_id")
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized", "detail": "API key required for runner registration"}}})
		return
	}

	// Check if API key has runner:register scope for this organization
	scopes, _ := c.Get("api_key_scopes")
	scopeStrs, ok := scopes.([]string)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized"}}})
		return
	}

	scopeChecker, err := apikey.NewScopeChecker(scopeStrs)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"errors": []gin.H{{"status": "401", "title": "Unauthorized"}}})
		return
	}

	// Check for runner:register permission on the organization
	if !scopeChecker.HasOrgPermission(pool.OrganizationID, "runner:register") && !scopeChecker.IsUnrestricted() {
		c.JSON(http.StatusForbidden, gin.H{"errors": []gin.H{{"status": "403", "title": "Forbidden", "detail": "API key does not have runner:register scope for this organization"}}})
		return
	}

	// Check if runner with this name already exists — if so, re-register (update and reuse)
	existing, _ := h.runnerRepo.GetByName(pool.OrganizationID, req.Name)
	if existing != nil {
		// Re-register: update the existing runner entry and return it
		existing.Status = models.RunnerStatusOnline
		existing.AgentPoolID = poolID
		existing.IPAddress = c.ClientIP()
		now := time.Now()
		existing.LastHeartbeatAt = &now
		if req.TerraformVersion != "" {
			existing.TerraformVersion = req.TerraformVersion
		}
		if req.AnsibleVersion != "" {
			existing.AnsibleVersion = req.AnsibleVersion
		}
		apiKeyUUID, _ := uuid.Parse(fmt.Sprintf("%v", apiKeyID))
		existing.RegisteredWithAPIKeyID = &apiKeyUUID
		if err := h.runnerRepo.Update(existing); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Failed to re-register runner"}}})
			return
		}
		logger.Infof("Runner re-registered: %s (ID: %s, pool: %s)", existing.Name, existing.ID, existing.AgentPoolID)

		// On re-registration, also check for pending jobs (same as heartbeat) so
		// runners that re-register on each cycle still pick up work.
		pendingJobs := []PendingJob{}
		if jobs, err := h.findPendingJobsForRunner(existing); err == nil {
			pendingJobs = jobs
		}

		c.JSON(http.StatusOK, gin.H{
			"runner_id":    existing.ID.String(),
			"status":       "re-registered",
			"pending_jobs": pendingJobs,
		})
		return
	}

	// Determine runner type based on versions
	runnerType := models.RunnerTypeCombined
	if req.TerraformVersion != "" && req.AnsibleVersion == "" {
		runnerType = models.RunnerTypeTerraform
	} else if req.AnsibleVersion != "" && req.TerraformVersion == "" {
		runnerType = models.RunnerTypeAnsible
	}

	// Get client IP
	clientIP := c.ClientIP()

	// Create the runner
	apiKeyUUID := apiKeyID.(uuid.UUID)
	now := time.Now()
	maxJobs := req.MaxConcurrentJobs
	if maxJobs <= 0 {
		maxJobs = 1
	}

	runner := &models.Runner{
		OrganizationID:         pool.OrganizationID,
		AgentPoolID:            poolID,
		Name:                   req.Name,
		RunnerType:             runnerType,
		Status:                 models.RunnerStatusOnline,
		Hostname:               req.Hostname,
		IPAddress:              clientIP,
		OSType:                 req.OSType,
		OSVersion:              req.OSVersion,
		AgentVersion:           req.AgentVersion,
		TerraformVersion:       req.TerraformVersion,
		AnsibleVersion:         req.AnsibleVersion,
		AvailableCollections:   models.RunnerCollections(req.AvailableCollections),
		MaxConcurrentJobs:      maxJobs,
		Labels:                 models.RunnerLabels(req.Labels),
		LastHeartbeatAt:        &now,
		RegisteredAt:           now,
		RegisteredWithAPIKeyID: &apiKeyUUID,
	}

	if err := h.runnerRepo.Create(runner); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error", "detail": "Failed to create runner"}}})
		return
	}

	// Generate a runner-specific API key
	// This key is scoped to only this runner: runner:<runner_id>:*
	runnerAPIKey, err := h.generateRunnerAPIKey(runner)
	if err != nil {
		// Runner was created but key generation failed - not ideal but runner can re-register
		c.JSON(http.StatusCreated, gin.H{
			"runner_id":             runner.ID.String(),
			"runner_api_key":        "", // Empty, runner should use original key
			"poll_interval_seconds": 10,
			"warning":               "Failed to generate runner-specific API key. Use original key for heartbeats.",
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"runner_id":             runner.ID.String(),
		"runner_api_key":        runnerAPIKey,
		"poll_interval_seconds": 10,
	})
}

// generateRunnerAPIKey creates a runner-specific API key
func (h *RunnerAgentHandler) generateRunnerAPIKey(runner *models.Runner) (string, error) {
	// Generate a random key
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		return "", err
	}
	rawKey := "tfe-" + hex.EncodeToString(keyBytes)

	// The scopes for this key - only access to this runner
	// TODO: In a full implementation, we'd create an actual api_key record with these scopes
	// For MVP, runners will continue using their registration key
	_ = []string{
		"runner:" + runner.ID.String() + ":heartbeat",
		"runner:" + runner.ID.String() + ":jobs",
	}

	return rawKey, nil
}

// HeartbeatRequest is the request body for runner heartbeat
type HeartbeatRequest struct {
	RunnerID          string `json:"runner_id" binding:"required"`
	Status            string `json:"status"` // "online" or "busy"
	CurrentJobs       int    `json:"current_jobs"`
	AvailableCapacity int    `json:"available_capacity"`
}

// HeartbeatResponse is the response body for runner heartbeat
type HeartbeatResponse struct {
	PendingJobs []PendingJob `json:"pending_jobs"`
}

// PendingJob represents a job waiting to be executed
type PendingJob struct {
	JobID         string `json:"job_id"`
	JobType       string `json:"job_type"`
	WorkspaceID   string `json:"workspace_id"`
	WorkspaceName string `json:"workspace_name"`
	RunType       string `json:"run_type,omitempty"` // For terraform: "plan", "apply"
	Priority      int    `json:"priority"`
}

// Heartbeat processes a runner heartbeat and returns pending jobs
// POST /api/v2/runner/heartbeat
func (h *RunnerAgentHandler) Heartbeat(c *gin.Context) {
	var req HeartbeatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}}})
		return
	}

	runnerID, err := uuid.Parse(req.RunnerID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Invalid runner_id"}}})
		return
	}

	runner, err := h.runnerRepo.GetByID(runnerID)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Runner not found"}}})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		}
		return
	}

	// Update runner status
	status := models.RunnerStatusOnline
	if req.Status == "busy" {
		status = models.RunnerStatusBusy
	}
	if err := h.runnerRepo.UpdateHeartbeat(runnerID, status, req.CurrentJobs); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		return
	}

	// Find pending jobs for this runner's pool
	pendingJobs := []PendingJob{}

	// Determine available capacity: use explicit field if sent, otherwise derive from runner settings
	availableCapacity := req.AvailableCapacity
	if availableCapacity == 0 && req.CurrentJobs == 0 {
		// Runner didn't send available_capacity but has no current jobs — it has capacity
		maxJobs := runner.MaxConcurrentJobs
		if maxJobs <= 0 {
			maxJobs = 1
		}
		availableCapacity = maxJobs - req.CurrentJobs
	}

	// Only query for jobs if runner has capacity
	if availableCapacity > 0 {
		// Query pending jobs (Ansible and/or Terraform depending on runner type)
		jobs, err := h.findPendingJobsForRunner(runner)
		if err != nil {
			// Log but don't fail the heartbeat
			_ = err
		} else {
			pendingJobs = jobs
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"pending_jobs": pendingJobs,
	})
}

// JobStartRequest is the request body for starting a job
type JobStartRequest struct {
	RunnerID string `json:"runner_id" binding:"required"`
}

// JobStart marks a job as started
// POST /api/v2/runner/jobs/:id/start
func (h *RunnerAgentHandler) JobStart(c *gin.Context) {
	jobIDStr := c.Param("id")

	var req JobStartRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}}})
		return
	}

	// Determine if this is a Terraform run (ID starts with "run-") or Ansible job (UUID)
	if strings.HasPrefix(jobIDStr, "run-") {
		// Terraform run
		h.jobStartTerraformRun(c, jobIDStr, req)
		return
	}

	// Ansible job (UUID)
	jobID, err := uuid.Parse(jobIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Invalid job ID"}}})
		return
	}

	// Get the job execution record
	exec, err := h.jobExecRepo.GetByJobID(jobID)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Job not found"}}})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		}
		return
	}

	// Update execution record status to running
	if err := h.jobExecRepo.UpdateStatus(exec.ID, models.JobExecutionStatusRunning, ""); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		return
	}

	// Update the ansible job status to running and set start time
	if h.ansibleJobRepo != nil {
		job, err := h.ansibleJobRepo.GetByID(jobID)
		if err == nil {
			now := time.Now()
			job.Status = models.AnsibleJobStatusRunning
			job.StartedAt = &now

			// Set the runner ID on the job
			runnerID, parseErr := uuid.Parse(req.RunnerID)
			if parseErr == nil {
				job.RunnerID = &runnerID
			}

			_ = h.ansibleJobRepo.Update(job)
		}
	}

	c.JSON(http.StatusOK, gin.H{"status": "started"})
}

// jobStartTerraformRun handles job start for Terraform runs
func (h *RunnerAgentHandler) jobStartTerraformRun(c *gin.Context, runID string, req JobStartRequest) {
	var run models.Run
	if err := h.db.First(&run, "id = ?", runID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Run not found"}}})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		}
		return
	}

	now := time.Now()
	runnerID, _ := uuid.Parse(req.RunnerID)

	// Update run status based on current phase
	switch run.Status { //nolint:exhaustive // only pending and applying need action on job start
	case models.RunStatusPending:
		run.Status = models.RunStatusPlanning
		run.StartedAt = &now
	case models.RunStatusApplying:
		// Apply phase start - already in applying status
		run.ApplyStartedAt = &now
	}
	run.RunnerID = &runnerID
	_ = h.db.Save(&run).Error

	c.JSON(http.StatusOK, gin.H{"status": "started"})
}

// JobOutputRequest is the request body for streaming job output
type JobOutputRequest struct {
	RunnerID string `json:"runner_id" binding:"required"`
	Output   string `json:"output" binding:"required"`
	Stream   string `json:"stream"` // "stdout" or "stderr"
}

// JobOutput receives streaming output from a job and stores it as structured events
// POST /api/v2/runner/jobs/:id/output
func (h *RunnerAgentHandler) JobOutput(c *gin.Context) {
	jobIDStr := c.Param("id")

	var req JobOutputRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}}})
		return
	}

	// Terraform run output - store in MinIO logs for the run
	if strings.HasPrefix(jobIDStr, "run-") {
		if h.storageClient != nil && req.Output != "" {
			ctx := context.Background()
			// Append output to the run's log in storage
			// Phase must match the log retrieval handler's expectations:
			// plan → runs/{id}/logs/plan.log, apply → runs/{id}/logs/apply.log
			// TFE-compatible: destroy runs use plan + apply phases (no separate "destroy" phase)
			phase := "plan"
			if req.Stream == "apply" {
				phase = "apply"
			}
			logsKey := fmt.Sprintf("runs/%s/logs/%s.log", jobIDStr, phase)
			// Read existing, append, write back (simple approach for streaming)
			existing, _ := h.storageClient.Get(ctx, logsKey)
			updated := string(existing) + req.Output + "\n"
			_ = h.storageClient.Put(ctx, logsKey, []byte(updated))
		}
		c.JSON(http.StatusOK, gin.H{"status": "received"})
		return
	}

	jobID, err := uuid.Parse(jobIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Invalid job ID"}}})
		return
	}

	if h.ansibleJobRepo == nil {
		c.JSON(http.StatusOK, gin.H{"status": "received"})
		return
	}

	line := strings.TrimSpace(req.Output)
	if line == "" {
		c.JSON(http.StatusOK, gin.H{"status": "received"})
		return
	}

	// Handle stderr output - store as stderr events
	if req.Stream == "stderr" {
		maxCounter, _ := h.ansibleJobRepo.GetMaxEventCounter(jobID)
		event := &models.AnsibleJobEvent{
			JobID:     jobID,
			Event:     "runner_stderr",
			EventData: map[string]interface{}{"stderr": line},
			Counter:   maxCounter + 1,
			Stderr:    line,
		}
		_ = h.ansibleJobRepo.CreateEvent(event)
		c.JSON(http.StatusOK, gin.H{"status": "received"})
		return
	}

	// Handle stdout - parse as JSONL from ansible.posix.jsonl callback
	var eventData map[string]interface{}
	if err := json.Unmarshal([]byte(line), &eventData); err != nil {
		// Not JSON - store as raw output event
		maxCounter, _ := h.ansibleJobRepo.GetMaxEventCounter(jobID)
		event := &models.AnsibleJobEvent{
			JobID:   jobID,
			Event:   "runner_output",
			Counter: maxCounter + 1,
			Stdout:  line,
		}
		_ = h.ansibleJobRepo.CreateEvent(event)
		c.JSON(http.StatusOK, gin.H{"status": "received"})
		return
	}

	// Parse JSONL event and store as structured event
	h.parseAndStoreAgentEvent(jobID, eventData, line)

	c.JSON(http.StatusOK, gin.H{"status": "received"})
}

// parseAndStoreAgentEvent parses a JSONL event from a self-hosted runner and stores it
func (h *RunnerAgentHandler) parseAndStoreAgentEvent(jobID uuid.UUID, eventData map[string]interface{}, rawLine string) {
	maxCounter, _ := h.ansibleJobRepo.GetMaxEventCounter(jobID)
	counter := maxCounter + 1

	// Extract common fields from JSONL event
	host := ""
	task := ""
	playName := ""
	eventType := "runner_on_ok"
	changed := false
	failed := false
	skipped := false
	unreachable := false
	stdoutStr := ""

	// Check for v2_playbook_on_stats - update job stats
	if evtType, ok := eventData["_event"].(string); ok && evtType == "v2_playbook_on_stats" {
		if stats, ok := eventData["stats"].(map[string]interface{}); ok {
			var totalOk, totalChanged, totalFailed, totalSkipped, totalUnreachable, totalRescued, totalIgnored int
			for _, hostStats := range stats {
				if hs, ok := hostStats.(map[string]interface{}); ok {
					if v, ok := hs["ok"].(float64); ok {
						totalOk += int(v)
					}
					if v, ok := hs["changed"].(float64); ok {
						totalChanged += int(v)
					}
					if v, ok := hs["failures"].(float64); ok {
						totalFailed += int(v)
					}
					if v, ok := hs["skipped"].(float64); ok {
						totalSkipped += int(v)
					}
					if v, ok := hs["unreachable"].(float64); ok {
						totalUnreachable += int(v)
					}
					if v, ok := hs["rescued"].(float64); ok {
						totalRescued += int(v)
					}
					if v, ok := hs["ignored"].(float64); ok {
						totalIgnored += int(v)
					}
				}
			}

			// Update the ansible job stats directly
			job, err := h.ansibleJobRepo.GetByID(jobID)
			if err == nil {
				job.HostsOk = totalOk
				job.HostsChanged = totalChanged
				job.HostsFailed = totalFailed
				job.HostsSkipped = totalSkipped
				job.HostsUnreachable = totalUnreachable
				job.HostsRescued = totalRescued
				job.HostsIgnored = totalIgnored
				_ = h.ansibleJobRepo.Update(job)
			}
		}

		// Store the stats event
		event := &models.AnsibleJobEvent{
			JobID:     jobID,
			Event:     "v2_playbook_on_stats",
			EventData: eventData,
			Counter:   counter,
			Stdout:    rawLine + "\n",
		}
		_ = h.ansibleJobRepo.CreateEvent(event)
		return
	}

	// Extract host
	if h, ok := eventData["host"].(string); ok {
		host = h
	}

	// Extract task name
	if t, ok := eventData["task"].(string); ok {
		task = t
	} else if taskMap, ok := eventData["task"].(map[string]interface{}); ok {
		if name, ok := taskMap["name"].(string); ok {
			task = name
		}
	}

	// Extract play name
	if p, ok := eventData["play"].(string); ok {
		playName = p
	} else if playMap, ok := eventData["play"].(map[string]interface{}); ok {
		if name, ok := playMap["name"].(string); ok {
			playName = name
		}
	}

	// Check status flags
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
	if msg, ok := eventData["msg"].(string); ok && msg != "" {
		stdoutStr = msg
	}

	// Check for result object (contains module output)
	if result, ok := eventData["result"].(map[string]interface{}); ok {
		if stdout, ok := result["stdout"].(string); ok && stdout != "" {
			if stdoutStr != "" {
				stdoutStr += "\n" + stdout
			} else {
				stdoutStr = stdout
			}
		}
		if msg, ok := result["msg"].(string); ok && msg != "" {
			if stdoutStr != "" {
				stdoutStr += "\n" + msg
			} else {
				stdoutStr = msg
			}
		}
		if stdoutLines, ok := result["stdout_lines"].([]interface{}); ok && len(stdoutLines) > 0 {
			var lines []string
			for _, l := range stdoutLines {
				if s, ok := l.(string); ok {
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

	// Skip events without meaningful content
	if host == "" && task == "" && playName == "" && stdoutStr == "" && rawLine == "" {
		return
	}

	// Store parsed task output in EventData for Events tab display
	if stdoutStr != "" {
		eventData["_parsed_output"] = stdoutStr
	}

	event := &models.AnsibleJobEvent{
		JobID:     jobID,
		Event:     eventType,
		EventData: eventData,
		Counter:   counter,
		Host:      host,
		Task:      task,
		Play:      playName,
		Stdout:    rawLine + "\n",
		Changed:   changed,
		Failed:    failed,
		Skipped:   skipped,
	}

	if unreachable {
		event.Failed = true
	}

	_ = h.ansibleJobRepo.CreateEvent(event)
}

// JobCompleteRequest is the request body for completing a job
type JobCompleteRequest struct {
	RunnerID             string `json:"runner_id" binding:"required"`
	Status               string `json:"status" binding:"required"` // "completed", "failed", "canceled"
	ExitCode             int    `json:"exit_code"`
	ErrorMessage         string `json:"error_message,omitempty"`
	Output               string `json:"output,omitempty"`      // Terraform/command output from runner
	PlanJSON             string `json:"plan_json,omitempty"`   // Terraform plan JSON (from terraform show -json)
	ResourceAdditions    int    `json:"resource_additions"`    // Number of resources to add
	ResourceChanges      int    `json:"resource_changes"`      // Number of resources to change
	ResourceDestructions int    `json:"resource_destructions"` // Number of resources to destroy
	OutputChanges        int    `json:"output_changes"`        // Number of output changes
	HasChanges           bool   `json:"has_changes"`           // Whether the plan has any changes
}

// JobComplete marks a job as completed
// POST /api/v2/runner/jobs/:id/complete
func (h *RunnerAgentHandler) JobComplete(c *gin.Context) {
	jobIDStr := c.Param("id")

	var req JobCompleteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}}})
		return
	}

	// Determine if this is a Terraform run or Ansible job
	if strings.HasPrefix(jobIDStr, "run-") {
		h.jobCompleteTerraformRun(c, jobIDStr, req)
		return
	}

	jobID, err := uuid.Parse(jobIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Invalid job ID"}}})
		return
	}

	// Get the job execution record
	exec, err := h.jobExecRepo.GetByJobID(jobID)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Job not found"}}})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		}
		return
	}

	// Map status string to enum
	var status models.JobExecutionStatus
	switch req.Status {
	case "completed":
		status = models.JobExecutionStatusCompleted
	case "failed":
		status = models.JobExecutionStatusFailed
	case "canceled":
		status = models.JobExecutionStatusCanceled
	default:
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Invalid status"}}})
		return
	}

	// Update execution record status
	if err := h.jobExecRepo.UpdateStatus(exec.ID, status, req.ErrorMessage); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		return
	}

	// Update the ansible job status and finalize stats
	if h.ansibleJobRepo != nil {
		job, err := h.ansibleJobRepo.GetByID(jobID)
		if err == nil {
			now := time.Now()
			job.FinishedAt = &now

			switch req.Status {
			case "completed":
				job.Status = models.AnsibleJobStatusSuccessful
			case "failed":
				job.Status = models.AnsibleJobStatusFailed
				if req.ErrorMessage != "" {
					job.ErrorMessage = req.ErrorMessage
				}
				// If no failures or unreachable hosts were counted, set failures to 1
				if job.HostsFailed == 0 && job.HostsUnreachable == 0 {
					job.HostsFailed = 1
				}
			case "canceled":
				job.Status = models.AnsibleJobStatusCanceled
			}

			// Count warnings from stderr events
			events, _, _ := h.ansibleJobRepo.ListEventsByJob(jobID, 10000, 0)
			warningsCount := 0
			for _, evt := range events {
				if evt.Stderr != "" {
					warningsCount += strings.Count(evt.Stderr, "[WARNING]:") + strings.Count(evt.Stderr, "[DEPRECATION WARNING]:")
				}
			}
			if warningsCount > 0 {
				job.HasWarnings = true
				job.WarningsCount = warningsCount
			}

			_ = h.ansibleJobRepo.Update(job)
		}
	}

	// Update runner status back to online (if it was busy)
	runnerID, _ := uuid.Parse(req.RunnerID)
	activeJobs, _ := h.jobExecRepo.CountActiveByRunner(runnerID)
	if activeJobs == 0 {
		_ = h.runnerRepo.UpdateStatus(runnerID, models.RunnerStatusOnline)
	}

	c.JSON(http.StatusOK, gin.H{"status": "completed"})
}

// jobCompleteTerraformRun handles job completion for Terraform runs
func (h *RunnerAgentHandler) jobCompleteTerraformRun(c *gin.Context, runID string, req JobCompleteRequest) {
	var run models.Run
	if err := h.db.First(&run, "id = ?", runID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Run not found"}}})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		}
		return
	}

	now := time.Now()

	// Store plan metadata if provided (from self-hosted runner's terraform show -json)
	if req.PlanJSON != "" || req.HasChanges {
		if run.PlanOutput == nil {
			run.PlanOutput = make(models.PlanOutput)
		}
		run.PlanOutput["AddCount"] = float64(req.ResourceAdditions)
		run.PlanOutput["ChangeCount"] = float64(req.ResourceChanges)
		run.PlanOutput["DestroyCount"] = float64(req.ResourceDestructions)
		run.PlanOutput["OutputChangeCount"] = float64(req.OutputChanges)

		// Parse the full plan JSON and store resource_changes and output_changes for hasChanges() detection
		if req.PlanJSON != "" {
			var planData map[string]interface{}
			if err := json.Unmarshal([]byte(req.PlanJSON), &planData); err == nil {
				if rc, ok := planData["resource_changes"]; ok {
					run.PlanOutput["resource_changes"] = rc
				}
				if oc, ok := planData["output_changes"]; ok {
					run.PlanOutput["output_changes"] = oc
				}
			}
		}

		logger.Infof("Stored plan metadata for run %s: additions=%d, changes=%d, destructions=%d, hasChanges=%v",
			runID, req.ResourceAdditions, req.ResourceChanges, req.ResourceDestructions, req.HasChanges)
	}

	switch req.Status {
	case "completed":
		switch run.Status { //nolint:exhaustive // only planning has special handling
		case models.RunStatusPlanning:
			// Plan phase completed (for plan-and-apply and destroy runs)
			run.Status = models.RunStatusPlanned
			run.PlanCompletedAt = &now
			// TFE-compatible: Only auto-apply for VCS-triggered runs with workspace.AutoApply enabled
			// UI-triggered "Plan and Apply" runs must wait for user confirmation via the Apply endpoint.
			// The AutoApplyAfterPlan flag only indicates this is a "plan-and-apply" run (not "plan-only"),
			// it does NOT mean the run should auto-apply without user confirmation.
			if run.AutoApplyAfterPlan && (run.Operation == models.RunOperationPlanAndApply || run.Operation == models.RunOperationDestroy) {
				shouldAutoApply := false
				if run.ConfigurationVersionID != nil {
					var configVersion models.ConfigurationVersion
					if err := h.db.First(&configVersion, "id = ?", *run.ConfigurationVersionID).Error; err == nil {
						if configVersion.Source == "tfe-vcs" {
							var workspace models.Workspace
							if err := h.db.First(&workspace, "id = ?", run.WorkspaceID).Error; err == nil && workspace.AutoApply {
								shouldAutoApply = true
								logger.Infof("Auto-applying run %s: VCS-triggered with workspace auto-apply enabled", runID)
							}
						}
					}
				}
				if shouldAutoApply {
					run.Status = models.RunStatusApplying
				} else {
					logger.Infof("Run %s plan completed, waiting for user confirmation (not VCS-triggered or workspace auto-apply disabled)", runID)
				}
			}
		case models.RunStatusCancelled:
			// Run was cancelled (e.g. user cancelled mid-apply); do not overwrite with applied
			break
		default:
			// Apply phase, plan-only, or destroy completed. Only set applied if run is not cancelled
			// (atomic update so we never overwrite cancelled, regardless of race with Cancel API).
			res := h.db.Model(&models.Run{}).Where("id = ? AND status != ?", runID, models.RunStatusCancelled).Updates(map[string]interface{}{
				"status":       models.RunStatusApplied,
				"completed_at": now,
				"updated_at":   now,
			})
			if res.RowsAffected == 0 {
				c.JSON(http.StatusOK, gin.H{"status": "completed"})
				return
			}
			// Persist plan metadata if provided (run struct was updated above the switch)
			if req.PlanJSON != "" || req.HasChanges {
				_ = h.db.Model(&models.Run{}).Where("id = ?", runID).Update("plan_output", run.PlanOutput)
			}
			c.JSON(http.StatusOK, gin.H{"status": "completed"})
			return
		}
	case "failed":
		run.Status = models.RunStatusFailed
		// Use ErrorMessage if provided, fall back to Output for error context
		errMsg := req.ErrorMessage
		if errMsg == "" && req.Output != "" {
			errMsg = req.Output
		}
		run.ErrorMessage = errMsg
		run.CompletedAt = &now
	case "canceled":
		run.Status = models.RunStatusCancelled
		run.CompletedAt = &now
	default:
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Invalid status"}}})
		return
	}

	if err := h.db.Save(&run).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		return
	}

	// Update runner status back to online (if it was busy)
	runnerID, _ := uuid.Parse(req.RunnerID)
	activeJobs, _ := h.jobExecRepo.CountActiveByRunner(runnerID)
	if activeJobs == 0 {
		_ = h.runnerRepo.UpdateStatus(runnerID, models.RunnerStatusOnline)
	}

	c.JSON(http.StatusOK, gin.H{"status": "completed"})
}

// Deregister removes a runner
// POST /api/v2/runner/deregister
func (h *RunnerAgentHandler) Deregister(c *gin.Context) {
	var req struct {
		RunnerID string `json:"runner_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}}})
		return
	}

	runnerID, err := uuid.Parse(req.RunnerID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Invalid runner_id"}}})
		return
	}

	// Simply mark the runner as offline rather than deleting
	// Admins can delete through the management API
	if err := h.runnerRepo.UpdateStatus(runnerID, models.RunnerStatusOffline); err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Runner not found"}}})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		}
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "deregistered"})
}

// JobArtifactsResponse contains all files needed to execute an ansible job
type JobArtifactsResponse struct {
	JobID            string                 `json:"job_id"`
	JobType          string                 `json:"job_type"` // "ansible_job" or "terraform_run"
	PlaybookContent  string                 `json:"playbook_content,omitempty"`
	PlaybookPath     string                 `json:"playbook_path,omitempty"`
	InventoryContent string                 `json:"inventory_content,omitempty"`
	AnsibleCfg       string                 `json:"ansible_cfg,omitempty"`
	ExtraVars        map[string]interface{} `json:"extra_vars,omitempty"`
	EnvironmentVars  map[string]string      `json:"environment_vars,omitempty"` // Cloud auth env vars (OIDC, etc.)
	Credential       *CredentialArtifact    `json:"credential,omitempty"`
	JobConfig        *AnsibleJobConfig      `json:"job_config,omitempty"`
	VCS              *VCSArtifact           `json:"vcs,omitempty"`
}

// VCSArtifact contains VCS info for cloning the repository on a self-hosted runner
type VCSArtifact struct {
	RepoURL    string `json:"repo_url"`
	Branch     string `json:"branch"`
	Repository string `json:"repository"` // e.g. "owner/repo"
}

// CredentialArtifact contains decrypted credential data for job execution
type CredentialArtifact struct {
	Type       string `json:"type"` // "ssh", "vault", "cloud"
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"` //nolint:gosec // G117: intentional credential field for runner artifacts
	SSHKey     string `json:"ssh_key,omitempty"`
	VaultToken string `json:"vault_token,omitempty"`
}

// AnsibleJobConfig contains ansible-specific execution configuration
type AnsibleJobConfig struct {
	Limit         string `json:"limit,omitempty"`
	Tags          string `json:"tags,omitempty"`
	SkipTags      string `json:"skip_tags,omitempty"`
	Verbosity     int    `json:"verbosity"`
	Forks         int    `json:"forks"`
	BecomeEnabled bool   `json:"become_enabled"`
	DiffMode      bool   `json:"diff_mode"`
}

// GetJobStatus returns the current run/job status so the agent can poll for cancellation.
// GET /api/v2/runner/jobs/:id/status
func (h *RunnerAgentHandler) GetJobStatus(c *gin.Context) {
	jobIDStr := c.Param("id")
	if jobIDStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "job id required"}}})
		return
	}
	// Terraform jobs use run ID (run-xxx)
	if strings.HasPrefix(jobIDStr, "run-") {
		var run models.Run
		if err := h.db.Select("status").First(&run, "id = ?", jobIDStr).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found"}}})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": string(run.Status)})
		return
	}
	// Ansible jobs use UUID
	if h.ansibleJobRepo == nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found"}}})
		return
	}
	jobID, parseErr := uuid.Parse(jobIDStr)
	if parseErr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Invalid job ID"}}})
		return
	}
	job, getErr := h.ansibleJobRepo.GetByID(jobID)
	if getErr != nil || job == nil {
		c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Not Found"}}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": string(job.Status)})
}

// GetJobArtifacts returns all artifacts needed to execute a job
// GET /api/v2/runner/jobs/:id/artifacts
func (h *RunnerAgentHandler) GetJobArtifacts(c *gin.Context) {
	jobIDStr := c.Param("id")

	// Determine if this is a Terraform run or Ansible job
	if strings.HasPrefix(jobIDStr, "run-") {
		h.getTerraformRunArtifacts(c, jobIDStr)
		return
	}

	jobID, err := uuid.Parse(jobIDStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Invalid job ID"}}})
		return
	}

	// Check if repos are available
	if h.ansibleJobRepo == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"errors": []gin.H{{"status": "501", "title": "Job artifacts not configured"}}})
		return
	}

	// Get the ansible job
	job, err := h.ansibleJobRepo.GetByID(jobID)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Job not found"}}})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		}
		return
	}

	response := JobArtifactsResponse{
		JobID:   job.ID.String(),
		JobType: "ansible_job",
		JobConfig: &AnsibleJobConfig{
			Limit:         job.Limit,
			Tags:          job.Tags,
			SkipTags:      job.SkipTags,
			Verbosity:     job.Verbosity,
			Forks:         job.Forks,
			BecomeEnabled: job.BecomeEnabled,
			DiffMode:      job.DiffMode,
		},
	}

	// Get playbook info and VCS details for the runner to clone the repo
	if h.playbookRepo != nil {
		playbook, err := h.playbookRepo.GetByID(job.PlaybookID)
		if err == nil {
			response.PlaybookPath = playbook.PlaybookPath

			// Include VCS info so agent can clone the repository
			if playbook.VCSConnectionID != nil && playbook.VCSRepository != "" {
				var vcsConn models.VCSConnection
				if err := h.db.First(&vcsConn, "id = ?", playbook.VCSConnectionID).Error; err == nil {
					branch := playbook.VCSBranch
					if branch == "" {
						branch = "main"
					}

					// Get a fresh token and build the clone URL via the provider registry.
					var repoURL string
					if h.vcsRegistry != nil {
						if provider, err := h.vcsRegistry.GetProvider(&vcsConn); err == nil {
							accessToken, _ := provider.GetFreshToken(c.Request.Context(), &vcsConn)
							repoURL = provider.BuildCloneURL(&vcsConn, accessToken, playbook.VCSRepository)
						}
					}

					if repoURL != "" {
						response.VCS = &VCSArtifact{
							RepoURL:    repoURL,
							Branch:     branch,
							Repository: playbook.VCSRepository,
						}
					}
				}
			}
		}
	}

	// Get credential first so we can inject password into inventory if needed
	var decryptedPassword string
	if job.CredentialID != nil && h.credentialRepo != nil && h.cryptoService != nil {
		cred, err := h.credentialRepo.GetByID(*job.CredentialID)
		if err == nil {
			credArtifact := &CredentialArtifact{
				Type: string(cred.Type),
			}

			// Copy username if set
			if cred.Username != "" {
				credArtifact.Username = cred.Username
			}

			// Decrypt credential fields
			if cred.Password != "" {
				if decrypted, err := h.cryptoService.Decrypt(cred.Password); err == nil {
					credArtifact.Password = decrypted
					// Track password for injection into inventory (Machine SSH credentials)
					if cred.Type == models.CredentialTypeMachineSSH {
						decryptedPassword = decrypted
					}
				}
			}
			if cred.SSHPrivateKey != "" {
				if decrypted, err := h.cryptoService.Decrypt(cred.SSHPrivateKey); err == nil {
					credArtifact.SSHKey = decrypted
				}
			}

			response.Credential = credArtifact
		}
	}

	// Generate inventory content using the inventory service (same approach as platform-managed runner)
	if h.inventoryService != nil {
		inventoryJSON, err := h.inventoryService.GenerateInventoryJSON(job.InventoryID)
		if err == nil {
			// If we have a Machine SSH credential with a password, inject it into the inventory
			if decryptedPassword != "" {
				if injected, err := injectPasswordIntoInventory(inventoryJSON, decryptedPassword); err == nil {
					inventoryJSON = injected
				}
			}
			// If we have a credential with a username, inject ansible_user so the runner uses it
			// (avoids falling back to the runner process user e.g. "iac" in Docker)
			if job.CredentialID != nil && h.credentialRepo != nil {
				if cred, err := h.credentialRepo.GetByID(*job.CredentialID); err == nil && cred.Username != "" {
					if injected, err := injectUserIntoInventory(inventoryJSON, cred.Username); err == nil {
						inventoryJSON = injected
					}
				}
			}
			response.InventoryContent = inventoryJSON
		}
	}

	// Get ansible.cfg for the job's project
	if h.ansibleConfigRepo != nil {
		// Get project to find workspace
		var project models.Project
		if err := h.db.First(&project, "id = ?", job.ProjectID).Error; err == nil {
			// Try project config first, then org (no workspace for ansible jobs)
			// GetForWorkspace signature: (workspaceID string, projectID, orgID uuid.UUID)
			config, err := h.ansibleConfigRepo.GetForWorkspace("", job.ProjectID, project.OrganizationID)
			if err == nil && config != nil {
				response.AnsibleCfg = config.ConfigContent
			}
		}
	}

	// Get extra vars (already in job)
	if job.ExtraVars != nil {
		response.ExtraVars = job.ExtraVars
	}

	// OIDC Workload Identity: Inject Azure OIDC env vars for self-hosted Ansible runners.
	// This enables Ansible playbooks on self-hosted runners to authenticate to Azure via OIDC.
	if h.azureOIDCRepo != nil && h.oidcTokenService != nil {
		var project models.Project
		if err := h.db.First(&project, "id = ?", job.ProjectID).Error; err == nil {
			configs, oidcErr := h.azureOIDCRepo.GetByOrganization(project.OrganizationID)
			if oidcErr != nil {
				logger.Warnf("Failed to look up Azure OIDC configurations for self-hosted Ansible runner (job %s): %v", job.ID, oidcErr)
			} else if len(configs) > 0 {
				oidcConfig := configs[0]

				var org models.Organization
				_ = h.db.First(&org, "id = ?", project.OrganizationID)

				token, tokenErr := h.oidcTokenService.GenerateWorkloadToken(oidc.WorkloadTokenRequest{
					Audience:         "api://AzureADTokenExchange",
					OrganizationName: org.Name,
					ProjectName:      project.Name,
					ResourceType:     oidc.ResourceTypeJob,
					ResourceName:     job.Name,
					ActionKind:       oidc.ActionRun,
					ActionID:         job.ID.String(),
				})
				if tokenErr != nil {
					logger.Warnf("Failed to generate OIDC token for self-hosted Ansible runner (job %s): %v", job.ID, tokenErr)
				} else {
					response.EnvironmentVars = map[string]string{
						"AZURE_CLIENT_ID":       oidcConfig.ClientID,
						"AZURE_TENANT_ID":       oidcConfig.TenantID,
						"AZURE_SUBSCRIPTION_ID": oidcConfig.SubscriptionID,
						"AZURE_FEDERATED_TOKEN": token,
						"ARM_OIDC_TOKEN":        token,
						"ARM_CLIENT_ID":         oidcConfig.ClientID,
						"ARM_SUBSCRIPTION_ID":   oidcConfig.SubscriptionID,
						"ARM_TENANT_ID":         oidcConfig.TenantID,
						"ARM_USE_OIDC":          "true",
					}
					logger.Infof("Injected OIDC workload identity token for self-hosted Ansible runner (job %s, org=%s)", job.ID, org.Name)
				}
			}
		}
	}

	c.JSON(http.StatusOK, response)
}

// getTerraformRunArtifacts returns artifacts needed for a self-hosted Terraform runner
// to execute a run: configuration tarball, variables, environment variables, and workspace metadata.
func (h *RunnerAgentHandler) getTerraformRunArtifacts(c *gin.Context, runID string) {
	var run models.Run
	if err := h.db.Preload("Workspace").First(&run, "id = ?", runID).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Run not found"}}})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		}
		return
	}

	// Resolve terraform version: workspace -> org default
	tfVersion := run.Workspace.TerraformVersion
	if tfVersion == "" {
		// Look up org default via workspace -> project -> organization
		var project models.Project
		if err := h.db.First(&project, "id = ?", run.Workspace.ProjectID).Error; err == nil {
			var org models.Organization
			if err := h.db.First(&org, "id = ?", project.OrganizationID).Error; err == nil {
				tfVersion = org.DefaultTerraformVersion
			}
		}
	}

	response := gin.H{
		"job_id":            run.ID,
		"job_type":          "terraform_run",
		"terraform_version": tfVersion,
		"working_directory": run.Workspace.WorkingDirectory,
	}

	// Get configuration tarball from storage if configuration version exists
	if run.ConfigurationVersionID != nil && h.storageClient != nil {
		storageKey := fmt.Sprintf("configuration-versions/%s/config.tar.gz", *run.ConfigurationVersionID)
		ctx := context.Background()
		if data, err := h.storageClient.Get(ctx, storageKey); err == nil {
			response["config_tarball"] = base64.StdEncoding.EncodeToString(data)
		}
	}

	// Get VCS info from workspace for cloning (fallback when no config version)
	if run.ConfigurationVersionID == nil && run.Workspace.VCSConnectionID != nil && run.Workspace.VCSRepository != "" {
		var vcsConn models.VCSConnection
		if err := h.db.First(&vcsConn, "id = ?", run.Workspace.VCSConnectionID).Error; err == nil {
			ctx := c.Request.Context()
			var repoURL string

			// Use provider registry for token refresh and clone URL building
			if h.vcsRegistry != nil {
				provider, provErr := h.vcsRegistry.GetProvider(&vcsConn)
				if provErr == nil {
					freshToken, tokenErr := provider.GetFreshToken(ctx, &vcsConn)
					if tokenErr != nil {
						logger.Warnf("Failed to get fresh token for VCS connection %s: %v", vcsConn.ID, tokenErr)
					}
					repoURL = provider.BuildCloneURL(&vcsConn, freshToken, run.Workspace.VCSRepository)
				} else {
					logger.Warnf("Failed to get VCS provider for connection %s: %v", vcsConn.ID, provErr)
				}
			} else {
				logger.Warnf("VCS registry not available for run %s", runID)
			}

			if repoURL != "" {
				// Log whether token auth is being used (without leaking the token)
				hasAuth := strings.Contains(repoURL, "@")
				logger.Infof("VCS clone URL for run %s: hasAuth=%v, repo=%s, branch=%s, provider=%s",
					runID, hasAuth, run.Workspace.VCSRepository, run.Workspace.VCSBranch, vcsConn.Provider)
				response["vcs"] = gin.H{
					"repo_url":   repoURL,
					"branch":     run.Workspace.VCSBranch,
					"repository": run.Workspace.VCSRepository,
				}
			} else {
				logger.Warnf("VCS clone URL is empty for run %s: provider=%s",
					runID, vcsConn.Provider)
			}
		} else {
			logger.Warnf("VCS connection lookup failed for run %s, vcsConnectionID=%v", runID, run.Workspace.VCSConnectionID)
		}
	}

	// Get Terraform variables (category == "terraform")
	if h.variableService != nil {
		ctx := context.Background()
		if vars, err := h.variableService.GetVariablesForRun(ctx, run.WorkspaceID); err == nil && len(vars) > 0 {
			response["variables"] = vars
		}
		// Get environment variables (category == "env")
		if envVars, err := h.variableService.GetEnvironmentVariablesForRun(ctx, run.WorkspaceID); err == nil && len(envVars) > 0 {
			response["environment_vars"] = envVars
		}
	}

	// OIDC Workload Identity: Inject Azure OIDC token into environment variables for self-hosted runners.
	// This is the equivalent of what the platform-hosted runner does in processJob().
	if h.azureOIDCRepo != nil && h.oidcTokenService != nil {
		// Resolve organization ID via workspace -> project -> organization
		var project models.Project
		if err := h.db.First(&project, "id = ?", run.Workspace.ProjectID).Error; err == nil {
			configs, oidcErr := h.azureOIDCRepo.GetByOrganization(project.OrganizationID)
			if oidcErr != nil {
				logger.Warnf("Failed to look up Azure OIDC configurations for self-hosted runner (run %s): %v", runID, oidcErr)
			} else if len(configs) > 0 {
				config := configs[0]

				// Determine run phase
				runPhase := "plan"
				if run.Status == models.RunStatusApplying {
					runPhase = "apply"
				}

				// Look up org and workspace names for token claims
				var org models.Organization
				_ = h.db.First(&org, "id = ?", project.OrganizationID)

				token, tokenErr := h.oidcTokenService.GenerateToken(
					"api://AzureADTokenExchange",
					org.Name,
					project.Name,
					run.Workspace.Name,
					run.ID,
					runPhase,
				)
				if tokenErr != nil {
					logger.Warnf("Failed to generate OIDC token for self-hosted runner (run %s): %v", runID, tokenErr)
				} else {
					// Ensure environment_vars map exists
					envVars, _ := response["environment_vars"].(map[string]string)
					if envVars == nil {
						envVars = make(map[string]string)
					}
					envVars["TFC_WORKLOAD_IDENTITY_TOKEN"] = token
					envVars["ARM_OIDC_TOKEN"] = token
					envVars["ARM_CLIENT_ID"] = config.ClientID
					envVars["ARM_SUBSCRIPTION_ID"] = config.SubscriptionID
					envVars["ARM_TENANT_ID"] = config.TenantID
					envVars["ARM_USE_OIDC"] = "true"
					response["environment_vars"] = envVars
					logger.Infof("Injected OIDC workload identity token for self-hosted runner (run %s, org=%s)", runID, org.Name)
				}
			}
		}
	}

	// TFE-compatible: Include latest state so the self-hosted runner can restore it.
	// Self-hosted runners use fresh temp dirs per job, so they need the current state
	// to know about existing resources and avoid re-creating them.
	if h.storageClient != nil {
		// Find the latest state version for this workspace
		var latestState models.StateVersion
		if err := h.db.Where("workspace_id = ?", run.WorkspaceID).
			Order("version DESC").First(&latestState).Error; err == nil {
			// Try to get state from object storage first (more complete)
			stateKey := fmt.Sprintf("workspaces/%s/state/%d.json", run.WorkspaceID, latestState.Version)
			ctx := context.Background()
			if stateData, err := h.storageClient.Get(ctx, stateKey); err == nil && len(stateData) > 0 {
				response["state_json"] = base64.StdEncoding.EncodeToString(stateData)
				logger.Infof("Including state version %d (%d bytes) in artifacts for run %s", latestState.Version, len(stateData), runID)
			} else if latestState.StateData != nil {
				// Fallback to state data from DB
				if stateJSON, err := json.Marshal(latestState.StateData); err == nil {
					response["state_json"] = base64.StdEncoding.EncodeToString(stateJSON)
					logger.Infof("Including state version %d from DB in artifacts for run %s", latestState.Version, runID)
				}
			}
		}
	}

	c.JSON(http.StatusOK, response)
}

// findPendingJobsForRunner finds pending jobs that can be executed by the given runner.
// It also creates RunnerJobExecution records so that JobStart/JobComplete can find them.
// Supports both Ansible jobs and Terraform runs depending on runner type.
func (h *RunnerAgentHandler) findPendingJobsForRunner(runner *models.Runner) ([]PendingJob, error) {
	pendingJobs := []PendingJob{}

	// Query Ansible jobs if runner can execute them
	if runner.CanExecuteAnsible() && h.ansibleJobRepo != nil {
		var jobs []models.AnsibleJob
		if err := h.db.Where("status = ? AND agent_pool_id = ?", models.AnsibleJobStatusPending, runner.AgentPoolID).
			Preload("Project").Order("created_at ASC").Limit(5).Find(&jobs).Error; err == nil {
			for _, job := range jobs {
				// Ensure a RunnerJobExecution record exists
				if _, err := h.jobExecRepo.GetByJobID(job.ID); err != nil {
					exec := &models.RunnerJobExecution{
						RunnerID:      runner.ID,
						JobType:       models.JobTypeAnsibleJob,
						JobID:         job.ID,
						WorkspaceName: job.Project.Name,
						Status:        models.JobExecutionStatusPending,
					}
					_ = h.jobExecRepo.Create(exec)
				}
				pendingJobs = append(pendingJobs, PendingJob{
					JobID:         job.ID.String(),
					JobType:       "ansible_job",
					WorkspaceName: job.Project.Name,
				})
			}
		}
	}

	// Query Terraform runs if runner can execute them.
	// Only return runs whose workspace is still agent (execution_mode = 'agent'); if workspace
	// was switched to remote, the run may still have agent_pool_id set until orchestrator clears it.
	if runner.CanExecuteTerraform() {
		var runs []models.Run
		if err := h.db.Joins("JOIN workspaces ON workspaces.id = runs.workspace_id").
			Where("runs.status IN ? AND runs.agent_pool_id = ? AND workspaces.execution_mode = ?",
				[]models.RunStatus{models.RunStatusPending, models.RunStatusApplying}, runner.AgentPoolID, "agent").
			Preload("Workspace").Order("runs.created_at ASC").Limit(5).Find(&runs).Error; err == nil {
			for _, run := range runs {
				// Map operation to run type for the agent
				runType := "plan"
				switch run.Operation {
				case models.RunOperationPlanAndApply:
					if run.Status == models.RunStatusApplying {
						runType = "apply"
					} else {
						runType = "plan"
					}
				case models.RunOperationPlanOnly:
					runType = "plan"
				case models.RunOperationDestroy:
					// TFE-compatible: destroy runs follow the same two-phase flow as plan-and-apply
					if run.Status == models.RunStatusApplying {
						runType = "apply-destroy"
					} else {
						runType = "plan-destroy"
					}
				}
				pendingJobs = append(pendingJobs, PendingJob{
					JobID:         run.ID,
					JobType:       "terraform_run",
					WorkspaceID:   run.WorkspaceID,
					WorkspaceName: run.Workspace.Name,
					RunType:       runType,
				})
			}
		}
	}

	return pendingJobs, nil
}

// injectPasswordIntoInventory adds ansible_password to all hosts in the inventory JSON.
// This matches the behavior of the platform-managed runner's password injection.
func injectPasswordIntoInventory(inventoryJSON, password string) (string, error) {
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
					hostsMap[hostName] = map[string]interface{}{
						"ansible_password": password,
					}
				} else if hostVarsMap, ok := hostVars.(map[string]interface{}); ok {
					hostVarsMap["ansible_password"] = password
				}
			}
		}
	}

	modifiedJSON, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal inventory JSON: %w", err)
	}

	return string(modifiedJSON), nil
}

// injectUserIntoInventory adds ansible_user to all hosts in the inventory JSON so the
// self-hosted runner uses the credential username instead of the process user (e.g. iac).
func injectUserIntoInventory(inventoryJSON, username string) (string, error) {
	var inventory map[string]interface{}
	if err := json.Unmarshal([]byte(inventoryJSON), &inventory); err != nil {
		return "", fmt.Errorf("failed to parse inventory JSON: %w", err)
	}

	for _, groupData := range inventory {
		groupMap, ok := groupData.(map[string]interface{})
		if !ok {
			continue
		}
		if hosts, exists := groupMap["hosts"]; exists {
			hostsMap, ok := hosts.(map[string]interface{})
			if !ok {
				continue
			}
			for hostName, hostVars := range hostsMap {
				if hostVars == nil {
					hostsMap[hostName] = map[string]interface{}{
						"ansible_user": username,
					}
				} else if hostVarsMap, ok := hostVars.(map[string]interface{}); ok {
					hostVarsMap["ansible_user"] = username
				}
			}
		}
	}

	modifiedJSON, err := json.MarshalIndent(inventory, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal inventory JSON: %w", err)
	}
	return string(modifiedJSON), nil
}

// UploadState handles state file uploads from self-hosted runners.
// This is called after apply (both successful and cancelled) to persist Terraform state.
// POST /api/v2/runner/jobs/:id/state
func (h *RunnerAgentHandler) UploadState(c *gin.Context) {
	jobIDStr := c.Param("id")
	if jobIDStr == "" || !strings.HasPrefix(jobIDStr, "run-") {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "valid run ID required"}}})
		return
	}

	var req struct {
		RunnerID string `json:"runner_id"`
		State    string `json:"state"` // base64-encoded terraform.tfstate
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": err.Error()}}})
		return
	}

	if req.State == "" {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "state field required"}}})
		return
	}

	// Decode base64 state
	stateData, err := base64.StdEncoding.DecodeString(req.State)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "invalid base64 state data"}}})
		return
	}

	// Parse state JSON
	var stateJSON map[string]interface{}
	if err := json.Unmarshal(stateData, &stateJSON); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"errors": []gin.H{{"status": "400", "title": "Bad Request", "detail": "invalid state JSON"}}})
		return
	}

	// Look up the run to get workspace ID
	var run models.Run
	if err := h.db.First(&run, "id = ?", jobIDStr).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, gin.H{"errors": []gin.H{{"status": "404", "title": "Run not found"}}})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Internal Server Error"}}})
		}
		return
	}

	// Get the next state version number
	var maxVersion int
	if err := h.db.Model(&models.StateVersion{}).
		Where("workspace_id = ?", run.WorkspaceID).
		Select("COALESCE(MAX(version), 0)").
		Scan(&maxVersion).Error; err != nil {
		logger.Warnf("Failed to get next state version for workspace %s: %v", run.WorkspaceID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Failed to determine state version"}}})
		return
	}
	nextVersion := maxVersion + 1

	// Extract serial and lineage from state JSON
	var serial *int
	var lineage string
	if s, ok := stateJSON["serial"].(float64); ok {
		sInt := int(s)
		serial = &sInt
	}
	if l, ok := stateJSON["lineage"].(string); ok {
		lineage = l
	}

	// Extract commit info from configuration version if available
	commitHash := ""
	committer := ""
	if run.ConfigurationVersionID != nil {
		var configVersion models.ConfigurationVersion
		if err := h.db.First(&configVersion, "id = ?", *run.ConfigurationVersionID).Error; err == nil {
			commitHash = configVersion.CommitHash
			committer = configVersion.Committer
		}
	}

	// Create state version record
	runID := jobIDStr
	stateVersion := models.StateVersion{
		WorkspaceID: run.WorkspaceID,
		RunID:       &runID,
		Version:     nextVersion,
		StateData:   models.StateData(stateJSON),
		Serial:      serial,
		Lineage:     lineage,
		CommitHash:  commitHash,
		Committer:   committer,
	}

	if err := h.db.Create(&stateVersion).Error; err != nil {
		logger.Warnf("Failed to create state version for run %s: %v", jobIDStr, err)
		c.JSON(http.StatusInternalServerError, gin.H{"errors": []gin.H{{"status": "500", "title": "Failed to save state version"}}})
		return
	}

	// Also save to object storage (MinIO)
	if h.storageClient != nil {
		key := fmt.Sprintf("workspaces/%s/state/%d.json", run.WorkspaceID, nextVersion)
		if err := h.storageClient.Put(c.Request.Context(), key, stateData); err != nil {
			logger.Warnf("Failed to save state to object storage for run %s: %v", jobIDStr, err)
			// Don't fail the request - state is already in DB
		}
	}

	logger.Infof("State version %d saved for run %s (workspace %s, serial=%v)", nextVersion, jobIDStr, run.WorkspaceID, serial)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "version": nextVersion})
}
