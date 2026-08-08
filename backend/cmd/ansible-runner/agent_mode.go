// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/michielvha/logger"
	"github.com/michielvha/stackweaver/core/security/gitargs"
)

// errJobClaimLost is returned by notifyJobStart when the server responds 409 -
// another runner already claimed the job. The agent treats this as "skip", not
// as a failure, so a runner that loses a claim race is a no-op.
var errJobClaimLost = errors.New("job already claimed by another runner")

// AgentConfig holds configuration for agent mode
type AgentConfig struct {
	ServerURL    string
	Token        string
	AgentPoolID  string
	RunnerName   string
	Labels       []string
	PollInterval time.Duration
}

// AgentRunner handles the agent mode lifecycle
type AgentRunner struct {
	config      AgentConfig
	runnerID    string
	runnerToken string // Runner-scoped API key minted at registration (AUD-001); used for all control-plane calls
	client      *http.Client
	shutdown    chan struct{}
}

// authToken returns the credential for a control-plane call: the runner-scoped
// token once registered, otherwise the org registration key (used only for the
// initial /runner/register). AUD-001: post-registration calls authenticate as the
// specific runner, not with the shared org key.
func (a *AgentRunner) authToken() string {
	if a.runnerToken != "" {
		return a.runnerToken
	}
	return a.config.Token
}

// RegisterResponse is the response from the register endpoint
type RegisterResponse struct {
	RunnerID            string       `json:"runner_id"`
	RunnerAPIKey        string       `json:"runner_api_key"`
	PollIntervalSeconds int          `json:"poll_interval_seconds"`
	PendingJobs         []PendingJob `json:"pending_jobs"`
}

// HeartbeatResponse is the response from the heartbeat endpoint
type HeartbeatResponse struct {
	PendingJobs []PendingJob `json:"pending_jobs"`
}

// PendingJob represents a job waiting to be executed
type PendingJob struct {
	JobID         string `json:"job_id"`
	JobType       string `json:"job_type"`
	WorkspaceID   string `json:"workspace_id"`
	WorkspaceName string `json:"workspace_name"`
	RunType       string `json:"run_type,omitempty"`
	Priority      int    `json:"priority"`
}

// RunAgentMode starts the runner in agent mode
func RunAgentMode() {
	config := loadAgentConfig()

	agent := &AgentRunner{
		config:   config,
		client:   &http.Client{Timeout: 30 * time.Second},
		shutdown: make(chan struct{}),
	}

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		logger.Info("Received shutdown signal, gracefully stopping...")
		close(agent.shutdown)
	}()

	// Register with the server
	initialJobs, err := agent.register()
	if err != nil {
		logger.Fatalf("Failed to register runner: %v", err)
	}

	logger.Infof("Runner registered successfully with ID: %s", agent.runnerID)

	// Process any pending jobs returned with the register response (re-registration case)
	if len(initialJobs) > 0 {
		logger.Infof("Received %d pending job(s) from registration, processing...", len(initialJobs))
		for _, job := range initialJobs {
			agent.executeJob(job)
		}
	}

	logger.Infof("Starting heartbeat loop (poll interval: %v)", agent.config.PollInterval)

	// Start heartbeat loop
	agent.heartbeatLoop()

	// Deregister on shutdown
	agent.deregister()
	logger.Info("Runner shut down cleanly")
}

// loadAgentConfig loads agent configuration from environment
func loadAgentConfig() AgentConfig {
	serverURL := os.Getenv("STACKWEAVER_SERVER")
	if serverURL == "" {
		logger.Fatal("STACKWEAVER_SERVER environment variable is required")
	}

	token := os.Getenv("STACKWEAVER_TOKEN")
	if token == "" {
		logger.Fatal("STACKWEAVER_TOKEN environment variable is required")
	}

	agentPoolID := os.Getenv("RUNNER_AGENT_POOL_ID")
	if agentPoolID == "" {
		logger.Fatal("RUNNER_AGENT_POOL_ID environment variable is required")
	}

	runnerName := os.Getenv("RUNNER_NAME")
	if runnerName == "" {
		hostname, _ := os.Hostname()
		runnerName = hostname
	}

	labels := []string{}
	if labelsStr := os.Getenv("RUNNER_LABELS"); labelsStr != "" {
		// Simple comma-separated parsing
		for _, l := range splitAndTrim(labelsStr, ",") {
			if l != "" {
				labels = append(labels, l)
			}
		}
	}

	pollInterval := 10 * time.Second
	if intervalStr := os.Getenv("POLL_INTERVAL"); intervalStr != "" {
		if d, err := time.ParseDuration(intervalStr); err == nil {
			pollInterval = d
		}
	}

	return AgentConfig{
		ServerURL:    serverURL,
		Token:        token,
		AgentPoolID:  agentPoolID,
		RunnerName:   runnerName,
		Labels:       labels,
		PollInterval: pollInterval,
	}
}

// register registers the runner with the StackWeaver server.
// Returns any pending jobs included in the response (e.g. on re-registration).
func (a *AgentRunner) register() ([]PendingJob, error) {
	hostname, _ := os.Hostname()

	// Get Ansible version
	ansibleVersion := getAnsibleVersion()

	// Get available collections
	collections := getInstalledCollections()

	payload := map[string]interface{}{
		"agent_pool_id":         a.config.AgentPoolID,
		"name":                  a.config.RunnerName,
		"hostname":              hostname,
		"os_type":               runtime.GOOS,
		"os_version":            getOSVersion(),
		"agent_version":         "1.0.0",
		"ansible_version":       ansibleVersion,
		"available_collections": collections,
		"max_concurrent_jobs":   1,
		"labels":                a.config.Labels,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal register payload: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", a.config.ServerURL+"/api/v2/runner/register", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create register request: %w", err)
	}

	// Registration always authenticates with the org registration key (runner:register
	// scope); the runner-scoped token cannot register. All other calls use authToken().
	req.Header.Set("Authorization", "Bearer "+a.config.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req) //nolint:gosec // G704: URL is from trusted config, not user input
	if err != nil {
		return nil, fmt.Errorf("failed to send register request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)

	// Accept both 201 (first registration) and 200 (re-registration of existing runner)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("register failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var registerResp RegisterResponse
	if err := json.Unmarshal(respBody, &registerResp); err != nil {
		return nil, fmt.Errorf("failed to parse register response: %w", err)
	}

	a.runnerID = registerResp.RunnerID
	if registerResp.RunnerAPIKey != "" {
		a.runnerToken = registerResp.RunnerAPIKey
	}

	// Update poll interval if server specifies one
	if registerResp.PollIntervalSeconds > 0 {
		a.config.PollInterval = time.Duration(registerResp.PollIntervalSeconds) * time.Second
	}

	return registerResp.PendingJobs, nil
}

// heartbeatLoop continuously polls for jobs
// startHeartbeatKeepalive sends periodic "busy" heartbeats until the returned stop func is called,
// so a runner executing a long playbook (which blocks the heartbeat loop) is not marked offline by
// the server-side monitor after 30s (AUD-025). The stop func blocks until the goroutine has exited.
func (a *AgentRunner) startHeartbeatKeepalive() func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-a.shutdown:
				return
			case <-ticker.C:
				_, _ = a.sendHeartbeat("busy", 1)
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func (a *AgentRunner) heartbeatLoop() {
	ticker := time.NewTicker(a.config.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-a.shutdown:
			return
		case <-ticker.C:
			jobs, err := a.sendHeartbeat("online", 0)
			if err != nil {
				logger.Warnf("Heartbeat failed: %v", err)
				continue
			}

			if len(jobs) > 0 {
				logger.Infof("Received %d pending job(s)", len(jobs))
				for _, job := range jobs {
					a.executeJob(job)
				}
			}
		}
	}
}

// sendHeartbeat sends a heartbeat and returns pending jobs
func (a *AgentRunner) sendHeartbeat(status string, currentJobs int) ([]PendingJob, error) {
	payload := map[string]interface{}{
		"runner_id":          a.runnerID,
		"status":             status,
		"current_jobs":       currentJobs,
		"available_capacity": 1 - currentJobs,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal heartbeat payload: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", a.config.ServerURL+"/api/v2/runner/heartbeat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create heartbeat request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+a.authToken())
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req) //nolint:gosec // G704: URL is from trusted config, not user input
	if err != nil {
		return nil, fmt.Errorf("failed to send heartbeat: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("heartbeat failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var heartbeatResp HeartbeatResponse
	if err := json.NewDecoder(resp.Body).Decode(&heartbeatResp); err != nil {
		return nil, fmt.Errorf("failed to parse heartbeat response: %w", err)
	}

	return heartbeatResp.PendingJobs, nil
}

// JobArtifacts contains everything needed to run an ansible job
type JobArtifacts struct {
	JobID            string                 `json:"job_id"`
	JobType          string                 `json:"job_type"`
	PlaybookContent  string                 `json:"playbook_content,omitempty"`
	PlaybookPath     string                 `json:"playbook_path,omitempty"`
	InventoryContent string                 `json:"inventory_content,omitempty"`
	AnsibleCfg       string                 `json:"ansible_cfg,omitempty"`
	ExtraVars        map[string]interface{} `json:"extra_vars,omitempty"`
	EnvironmentVars  map[string]string      `json:"environment_vars,omitempty"` // Cloud auth env vars (OIDC, etc.)
	Credential       *CredentialData        `json:"credential,omitempty"`
	// Vaults carries every attached vault credential (multi-vault support).
	Vaults []VaultData `json:"vaults,omitempty"`
	// GCPServiceAccount is a decrypted service-account JSON to write to a file
	// and expose via GOOGLE_APPLICATION_CREDENTIALS.
	GCPServiceAccount string         `json:"gcp_service_account,omitempty"`
	JobConfig         *JobConfigData `json:"job_config,omitempty"`
	VCS               *VCSData       `json:"vcs,omitempty"`
}

// VaultData is one decrypted vault password.
type VaultData struct {
	Password string `json:"password"` //nolint:gosec // G117: credential field, not a hardcoded secret
	VaultID  string `json:"vault_id,omitempty"`
}

// VCSData contains VCS info for cloning the repository
type VCSData struct {
	RepoURL    string `json:"repo_url"`
	Branch     string `json:"branch"`
	Repository string `json:"repository"`
}

// CredentialData contains credential info for the job
type CredentialData struct {
	Type       string `json:"type"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"` //nolint:gosec // G117: credential field, not a hardcoded secret
	SSHKey     string `json:"ssh_key,omitempty"`
	VaultToken string `json:"vault_token,omitempty"`
	// BecomePassword is injected as ansible_become_pass when the job escalates.
	BecomePassword string `json:"become_password,omitempty"` //nolint:gosec // G117: credential field, not a hardcoded secret
}

// JobConfigData contains ansible execution config
type JobConfigData struct {
	Limit          string `json:"limit,omitempty"`
	Tags           string `json:"tags,omitempty"`
	SkipTags       string `json:"skip_tags,omitempty"`
	Verbosity      int    `json:"verbosity"`
	Forks          int    `json:"forks"`
	BecomeEnabled  bool   `json:"become_enabled"`
	DiffMode       bool   `json:"diff_mode"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"` // kills the run when exceeded (0 = none)
}

// executeJob executes a pending job
func (a *AgentRunner) executeJob(job PendingJob) {
	logger.Infof("Executing job %s (type: %s, workspace: %s)", job.JobID, job.JobType, job.WorkspaceName)

	// Update status to busy
	_, _ = a.sendHeartbeat("busy", 1)

	// AUD-025: keep heartbeating "busy" while the (synchronous, possibly long) playbook runs, so
	// the server-side monitor doesn't mark this runner offline 30s in.
	stopKeepalive := a.startHeartbeatKeepalive()
	defer stopKeepalive()

	// Notify server that job is starting. A 409 means another runner already
	// claimed this job - skip it entirely (no artifact download, no run) and
	// return to idle so we can pick up other work.
	if err := a.notifyJobStart(job.JobID); err != nil {
		if errors.Is(err, errJobClaimLost) {
			logger.Infof("Job %s already claimed by another runner, skipping", job.JobID)
			_, _ = a.sendHeartbeat("online", 0)
			return
		}
		logger.Warnf("Failed to notify job start: %v", err)
	}

	// Download job artifacts
	artifacts, err := a.downloadJobArtifacts(job.JobID)
	if err != nil {
		logger.Errorf("Failed to download job artifacts: %v", err)
		_ = a.notifyJobComplete(job.JobID, "failed", "Failed to download job artifacts: "+err.Error())
		_, _ = a.sendHeartbeat("online", 0)
		return
	}

	// Execute the job
	status, errMsg := a.runAnsiblePlaybook(job.JobID, artifacts)

	// Report completion
	if err := a.notifyJobComplete(job.JobID, status, errMsg); err != nil {
		logger.Warnf("Failed to notify job completion: %v", err)
	}

	// Update status back to online
	_, _ = a.sendHeartbeat("online", 0)
}

// downloadJobArtifacts fetches artifacts from the server
func (a *AgentRunner) downloadJobArtifacts(jobID string) (*JobArtifacts, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", a.config.ServerURL+"/api/v2/runner/jobs/"+jobID+"/artifacts", nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+a.authToken())

	resp, err := a.client.Do(req) //nolint:gosec // G704: URL is from trusted config, not user input
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("failed to download artifacts (status %d): %s", resp.StatusCode, string(body))
	}

	var artifacts JobArtifacts
	if err := json.NewDecoder(resp.Body).Decode(&artifacts); err != nil {
		return nil, fmt.Errorf("failed to decode artifacts: %w", err)
	}

	return &artifacts, nil
}

// getJobStatus fetches the job status from the API for cancellation polling.
func (a *AgentRunner) getJobStatus(ctx context.Context, jobID string) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "GET", a.config.ServerURL+"/api/v2/runner/jobs/"+jobID+"/status", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+a.authToken())
	resp, err := a.client.Do(req) //nolint:gosec // G704: URL is from trusted config, not user input
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	var out struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Status, nil
}

// runAnsiblePlaybook executes the ansible playbook and streams output.
// It polls the API for job cancellation and stops the playbook if the job is canceled.
func (a *AgentRunner) runAnsiblePlaybook(jobID string, artifacts *JobArtifacts) (status string, errorMsg string) {
	// Create work directory
	workDir := fmt.Sprintf("/tmp/ansible-job-%s", jobID)
	if err := os.MkdirAll(workDir, 0o750); err != nil {
		return "failed", "Failed to create work directory: " + err.Error()
	}
	defer func() { _ = os.RemoveAll(workDir) }() // Cleanup after execution

	// Clone VCS repository if VCS info is provided
	repoDir := workDir
	if artifacts.VCS != nil && artifacts.VCS.RepoURL != "" {
		logger.Infof("Cloning repository %s (branch: %s)", artifacts.VCS.Repository, artifacts.VCS.Branch)
		cloneDir := workDir + "/repo"
		if err := a.cloneRepo(artifacts.VCS.RepoURL, artifacts.VCS.Branch, cloneDir); err != nil {
			return "failed", "Failed to clone repository: " + err.Error()
		}
		repoDir = cloneDir
		logger.Infof("Repository cloned successfully to %s", cloneDir)
	} else {
		logger.Warnf("No VCS info provided for job - playbook files may not be available")
	}

	// Write ansible.cfg if provided
	if artifacts.AnsibleCfg != "" {
		cfgPath := workDir + "/ansible.cfg"
		if err := os.WriteFile(cfgPath, []byte(artifacts.AnsibleCfg), 0o600); err != nil {
			return "failed", "Failed to write ansible.cfg: " + err.Error()
		}
	}

	// Write inventory file (use .json so Ansible parses it as JSON and picks up host vars like ansible_password)
	inventoryPath := workDir + "/inventory.json"
	if artifacts.InventoryContent != "" {
		if err := os.WriteFile(inventoryPath, []byte(artifacts.InventoryContent), 0o600); err != nil {
			return "failed", "Failed to write inventory: " + err.Error()
		}
		// Diagnostic: confirm inventory has expected keys (do not log secret values)
		invLen := len(artifacts.InventoryContent)
		hasPasswordKey := strings.Contains(artifacts.InventoryContent, `"ansible_password"`)
		hasUserKey := strings.Contains(artifacts.InventoryContent, `"ansible_user"`)
		logger.Infof("Inventory: %d bytes, has ansible_user=%v, has ansible_password=%v", invLen, hasUserKey, hasPasswordKey)
	}
	if artifacts.Credential != nil {
		hasUser := artifacts.Credential.Username != ""
		hasPass := artifacts.Credential.Password != ""
		hasKey := artifacts.Credential.SSHKey != ""
		logger.Infof("Credential: type=%s, username=%v, password=%v, ssh_key=%v", artifacts.Credential.Type, hasUser, hasPass, hasKey)
	} else {
		logger.Infof("Credential: none")
	}
	if len(artifacts.ExtraVars) > 0 {
		keys := make([]string, 0, len(artifacts.ExtraVars))
		for k := range artifacts.ExtraVars {
			keys = append(keys, k)
		}
		logger.Infof("Extra vars keys: %v (any ansible_* here overrides inventory)", keys)
	}

	// SSH password auth requires sshpass on the runner; Ansible cannot use ansible_password without it
	if artifacts.Credential != nil && artifacts.Credential.Password != "" && artifacts.Credential.SSHKey == "" {
		if _, err := exec.LookPath("sshpass"); err != nil {
			return "failed", "SSH password authentication requires sshpass on the runner. Install it (e.g. apt install sshpass or yum install sshpass) and try again."
		}
	}

	// Write extra vars if provided
	var extraVarsPath string
	if len(artifacts.ExtraVars) > 0 {
		extraVarsPath = workDir + "/extra_vars.json"
		varsJSON, err := json.Marshal(artifacts.ExtraVars)
		if err != nil {
			return "failed", "Failed to marshal extra vars: " + err.Error()
		}
		if err := os.WriteFile(extraVarsPath, varsJSON, 0o600); err != nil {
			return "failed", "Failed to write extra vars: " + err.Error()
		}
	}

	// Write SSH key if provided
	var sshKeyPath string
	if artifacts.Credential != nil && artifacts.Credential.SSHKey != "" {
		sshKeyPath = workDir + "/ssh_key"
		if err := os.WriteFile(sshKeyPath, []byte(artifacts.Credential.SSHKey), 0o600); err != nil {
			return "failed", "Failed to write SSH key: " + err.Error()
		}
	}

	// Ad hoc jobs ship the generated transient playbook as content instead of
	// a repository to clone; write it into the workspace (repoDir == workDir
	// when no VCS artifact is present, so the path resolution below finds it).
	if artifacts.PlaybookContent != "" {
		contentName := artifacts.PlaybookPath
		if contentName == "" {
			contentName = "adhoc.yml"
		}
		if err := os.WriteFile(filepath.Join(workDir, contentName), []byte(artifacts.PlaybookContent), 0o600); err != nil {
			return "failed", "Failed to write playbook content: " + err.Error()
		}
	}

	// Build ansible-playbook command
	args := []string{"-i", inventoryPath}

	// Resolve playbook path - relative paths are resolved within the cloned repo
	playbookPath := artifacts.PlaybookPath
	if playbookPath == "" {
		playbookPath = "site.yml"
	}
	// Strip leading slash, playbook paths are relative to the cloned repo root.
	// Azure DevOps file listing returns paths with a leading "/" which would cause
	// filepath.IsAbs() to treat them as absolute system paths instead of repo-relative.
	playbookPath = strings.TrimPrefix(playbookPath, "/")
	// Build absolute path to playbook within the repo
	if !filepath.IsAbs(playbookPath) {
		playbookPath = filepath.Join(repoDir, playbookPath)
	}

	// Verify the playbook file exists before running ansible-playbook
	if _, err := os.Stat(playbookPath); os.IsNotExist(err) {
		if artifacts.VCS == nil || artifacts.VCS.RepoURL == "" {
			return "failed", fmt.Sprintf("Playbook file not found at %s - no VCS repository was configured for this playbook, so the repository could not be cloned", playbookPath)
		}
		return "failed", fmt.Sprintf("Playbook file not found at %s - check that the playbook path is correct relative to the repository root", playbookPath)
	}

	// Add job config options
	if artifacts.JobConfig != nil {
		if artifacts.JobConfig.Limit != "" {
			args = append(args, "-l", artifacts.JobConfig.Limit)
		}
		if artifacts.JobConfig.Tags != "" {
			args = append(args, "-t", artifacts.JobConfig.Tags)
		}
		if artifacts.JobConfig.SkipTags != "" {
			args = append(args, "--skip-tags", artifacts.JobConfig.SkipTags)
		}
		for i := 0; i < artifacts.JobConfig.Verbosity; i++ {
			args = append(args, "-v")
		}
		if artifacts.JobConfig.Forks > 0 {
			args = append(args, "-f", fmt.Sprintf("%d", artifacts.JobConfig.Forks))
		}
		if artifacts.JobConfig.BecomeEnabled {
			args = append(args, "-b")
		}
		if artifacts.JobConfig.DiffMode {
			args = append(args, "-D")
		}
	}

	// Add extra vars file
	if extraVarsPath != "" {
		args = append(args, "-e", "@"+extraVarsPath)
	}

	// Vault credentials: one file per vault, labeled with --vault-id. A single
	// unlabeled vault keeps the historical env-var behavior (set below).
	var unlabeledVaultFile string
	for i, vault := range artifacts.Vaults {
		vaultFile := filepath.Join(workDir, fmt.Sprintf("vault_pass_%d", i))
		if err := os.WriteFile(vaultFile, []byte(vault.Password), 0o600); err != nil {
			return "failed", "Failed to write vault password file: " + err.Error()
		}
		if vault.VaultID != "" {
			args = append(args, "--vault-id", vault.VaultID+"@"+vaultFile)
		} else {
			unlabeledVaultFile = vaultFile
		}
	}

	// Become password: injected via a 0600 extra-vars file when escalating.
	if artifacts.JobConfig != nil && artifacts.JobConfig.BecomeEnabled &&
		artifacts.Credential != nil && artifacts.Credential.BecomePassword != "" {
		becomeVars, merr := json.Marshal(map[string]string{"ansible_become_pass": artifacts.Credential.BecomePassword})
		if merr == nil {
			becomeFile := filepath.Join(workDir, "become_vars.json")
			if werr := os.WriteFile(becomeFile, becomeVars, 0o600); werr == nil {
				args = append(args, "-e", "@"+becomeFile)
			} else {
				logger.Warnf("Failed to write become password file: %v", werr)
			}
		}
	}

	// Add SSH key
	if sshKeyPath != "" {
		args = append(args, "--private-key", sshKeyPath)
	}

	// Add username if provided
	if artifacts.Credential != nil && artifacts.Credential.Username != "" {
		args = append(args, "-u", artifacts.Credential.Username)
	}

	args = append(args, playbookPath)

	// Set working directory to the playbook's parent so Ansible can find
	// relative paths (roles/, group_vars/, etc.)
	ansibleWorkDir := filepath.Dir(playbookPath)

	logger.Infof("Running: ansible-playbook %v (workdir: %s)", args, ansibleWorkDir)

	// Cancellable context: poll API for job cancellation and cancel to stop the
	// playbook. The template's job timeout (when set) becomes the deadline.
	runCtx, cancelRun := context.WithCancel(context.Background())
	if artifacts.JobConfig != nil && artifacts.JobConfig.TimeoutSeconds > 0 {
		runCtx, cancelRun = context.WithTimeout(context.Background(), time.Duration(artifacts.JobConfig.TimeoutSeconds)*time.Second)
	}
	defer cancelRun()
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				s, pollErr := a.getJobStatus(runCtx, jobID)
				if pollErr != nil {
					continue
				}
				if s == "canceled" {
					logger.Infof("Job %s was canceled, stopping ansible-playbook", jobID)
					cancelRun()
					return
				}
			}
		}
	}()

	// Execute ansible-playbook (context is cancelled when job is canceled via API)
	// #nosec G204 -- args are constructed from trusted sources
	cmd := exec.CommandContext(runCtx, "ansible-playbook", args...)
	cmd.Dir = ansibleWorkDir

	// Set environment for ansible-playbook
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, "ANSIBLE_CONFIG="+workDir+"/ansible.cfg")
	// Use credential username so Ansible does not fall back to process user (e.g. iac in Docker)
	if artifacts.Credential != nil && artifacts.Credential.Username != "" {
		cmd.Env = append(cmd.Env, "ANSIBLE_REMOTE_USER="+artifacts.Credential.Username)
	}
	// Inject cloud auth environment variables (OIDC workload identity, etc.)
	for k, v := range artifacts.EnvironmentVars {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// A single unlabeled vault keeps the historical env-var behavior.
	if unlabeledVaultFile != "" {
		cmd.Env = append(cmd.Env, "ANSIBLE_VAULT_PASSWORD_FILE="+unlabeledVaultFile)
	}
	// GCP service-account credentials are file-based.
	if artifacts.GCPServiceAccount != "" {
		gcpFile := filepath.Join(workDir, "gcp_credentials.json")
		if werr := os.WriteFile(gcpFile, []byte(artifacts.GCPServiceAccount), 0o600); werr == nil {
			cmd.Env = append(cmd.Env, "GOOGLE_APPLICATION_CREDENTIALS="+gcpFile)
			cmd.Env = append(cmd.Env, "GCP_AUTH_KIND=serviceaccount")
		} else {
			logger.Warnf("Failed to write GCP credentials file: %v", werr)
		}
	}
	// Use JSONL callback for structured output (events as they happen)
	// This enables host facts, task details, and stats parsing on the server
	cmd.Env = append(cmd.Env, "ANSIBLE_STDOUT_CALLBACK=ansible.posix.jsonl")
	cmd.Env = append(cmd.Env, "ANSIBLE_LOAD_CALLBACK_PLUGINS=true")
	cmd.Env = append(cmd.Env, "ANSIBLE_HOST_KEY_CHECKING="+ansibleHostKeyCheckingValue()) // AUD-116: operator-overridable
	cmd.Env = append(cmd.Env, "ANSIBLE_RETRY_FILES_ENABLED=false")

	// Capture output
	var stdout, stderr bytes.Buffer
	cmd.Stdout = io.MultiWriter(&stdout, &streamWriter{agent: a, jobID: jobID, stream: "stdout"})
	cmd.Stderr = io.MultiWriter(&stderr, &streamWriter{agent: a, jobID: jobID, stream: "stderr"})

	err := cmd.Run()
	if runCtx.Err() == context.Canceled {
		return "canceled", ""
	}
	if runCtx.Err() == context.DeadlineExceeded {
		timeout := 0
		if artifacts.JobConfig != nil {
			timeout = artifacts.JobConfig.TimeoutSeconds
		}
		return "failed", fmt.Sprintf("job timeout exceeded (%ds): ansible-playbook was killed", timeout)
	}
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if ok {
			return "failed", fmt.Sprintf("Exit code %d: %s", exitErr.ExitCode(), stderr.String())
		}
		return "failed", err.Error()
	}

	return "completed", ""
}

// cloneRepo clones a git repository to the target directory
func (a *AgentRunner) cloneRepo(repoURL, branch, targetDir string) error {
	if err := os.MkdirAll(targetDir, 0o750); err != nil {
		return fmt.Errorf("failed to create clone directory: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// AUD-043: branch/URL originate from user-set VCS config; git parses "-"-leading args as options
	// (CVE-2017-1000117 class). Guard inline at the sink and place "--" before the positionals.
	if !gitargs.RefNameRe.MatchString(branch) {
		return fmt.Errorf("refusing unsafe branch name %q", branch)
	}
	if !gitargs.GitURLRe.MatchString(repoURL) {
		return fmt.Errorf("refusing unsafe repository URL")
	}
	args := []string{"clone", "--depth", "1", "--single-branch", "--branch", branch, "--", repoURL, targetDir}
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: branch + repoURL validated inline above; positionals guarded by "--"
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed: %w: %s", err, string(output))
	}

	return nil
}

// streamWriter implements io.Writer to stream output to the server
type streamWriter struct {
	agent  *AgentRunner
	jobID  string
	stream string
	buffer bytes.Buffer
}

func (w *streamWriter) Write(p []byte) (n int, err error) {
	w.buffer.Write(p)

	// Send output in chunks (when we have a newline or buffer is large enough)
	for {
		line, err := w.buffer.ReadString('\n')
		if err != nil {
			// No complete line yet, put it back
			w.buffer.WriteString(line)
			break
		}

		// Send this line to the server
		_ = w.agent.sendJobOutput(w.jobID, line, w.stream)
	}

	return len(p), nil
}

// sendJobOutput sends job output to the server
func (a *AgentRunner) sendJobOutput(jobID, output, stream string) error {
	payload := map[string]string{
		"runner_id": a.runnerID,
		"output":    output,
		"stream":    stream,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", a.config.ServerURL+"/api/v2/runner/jobs/"+jobID+"/output", bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+a.authToken())
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req) //nolint:gosec // G704: URL is from trusted config, not user input
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	return nil
}

// notifyJobStart tells the server the job is starting
func (a *AgentRunner) notifyJobStart(jobID string) error {
	payload := map[string]string{
		"runner_id": a.runnerID,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", a.config.ServerURL+"/api/v2/runner/jobs/"+jobID+"/start", bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+a.authToken())
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req) //nolint:gosec // G704: URL is from trusted config, not user input
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusConflict {
		// Another runner already claimed this job (or it is no longer claimable).
		// This runner must skip it rather than execute it.
		return errJobClaimLost
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("job start notification failed with status %d", resp.StatusCode)
	}

	return nil
}

// notifyJobComplete tells the server the job is done
func (a *AgentRunner) notifyJobComplete(jobID, status, errorMsg string) error {
	payload := map[string]interface{}{
		"runner_id":     a.runnerID,
		"status":        status,
		"error_message": errorMsg,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", a.config.ServerURL+"/api/v2/runner/jobs/"+jobID+"/complete", bytes.NewReader(body))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+a.authToken())
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req) //nolint:gosec // G704: URL is the operator-configured server endpoint, not user-controlled
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("job complete notification failed with status %d", resp.StatusCode)
	}

	return nil
}

// deregister notifies the server that the runner is shutting down
func (a *AgentRunner) deregister() {
	payload := map[string]string{
		"runner_id": a.runnerID,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		logger.Warnf("Failed to marshal deregister payload: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", a.config.ServerURL+"/api/v2/runner/deregister", bytes.NewReader(body))
	if err != nil {
		logger.Warnf("Failed to create deregister request: %v", err)
		return
	}

	req.Header.Set("Authorization", "Bearer "+a.authToken())
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req) //nolint:gosec // G704: URL is the operator-configured server endpoint, not user-controlled
	if err != nil {
		logger.Warnf("Failed to deregister: %v", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK {
		logger.Info("Successfully deregistered from server")
	}
}

// Helper functions

func splitAndTrim(s string, sep string) []string {
	parts := []string{}
	for _, p := range splitString(s, sep) {
		trimmed := trimSpace(p)
		if trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return parts
}

func splitString(s, sep string) []string {
	// Simple split implementation
	result := []string{}
	current := ""
	for _, c := range s {
		if string(c) == sep {
			result = append(result, current)
			current = ""
		} else {
			current += string(c)
		}
	}
	result = append(result, current)
	return result
}

func trimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

func getAnsibleVersion() string {
	// Try to get ansible version
	out, err := runCommand("ansible", "--version")
	if err != nil {
		return ""
	}
	// First line typically contains version
	lines := splitString(out, "\n")
	if len(lines) > 0 {
		// Parse "ansible [core X.Y.Z]" or "ansible X.Y.Z"
		return extractVersion(lines[0])
	}
	return ""
}

func getInstalledCollections() []string {
	// Try to list installed collections
	out, err := runCommand("ansible-galaxy", "collection", "list", "--format", "json")
	if err != nil {
		return []string{}
	}

	// Parse JSON output
	var collections map[string]interface{}
	if err := json.Unmarshal([]byte(out), &collections); err != nil {
		return []string{}
	}

	result := []string{}
	for name := range collections {
		result = append(result, name)
	}
	return result
}

func getOSVersion() string {
	// Try to get OS version
	if data, err := os.ReadFile("/etc/os-release"); err == nil {
		lines := splitString(string(data), "\n")
		for _, line := range lines {
			if len(line) > 12 && line[:12] == "PRETTY_NAME=" {
				// Remove quotes
				version := line[12:]
				if len(version) > 2 && version[0] == '"' {
					version = version[1 : len(version)-1]
				}
				return version
			}
		}
	}
	return runtime.GOOS + "/" + runtime.GOARCH
}

func runCommand(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: args are internally controlled
	out, err := cmd.Output()
	return string(out), err
}

func extractVersion(s string) string {
	// Simple version extraction - look for X.Y.Z pattern
	// This is a naive implementation
	return s // Return as-is for now, the server will parse it
}
