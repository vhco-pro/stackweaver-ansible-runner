// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package terraform

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/iac-platform/backend/internal/plugins"
)

type Plugin struct {
	terraformVersion string
	binaryPath       string // resolved path to the terraform binary for this version
}

// NewPlugin creates a plugin that uses the exact terraform version specified.
// Version must not be empty -- it should be resolved from workspace or org before calling this.
func NewPlugin(terraformVersion string) *Plugin {
	if terraformVersion == "" {
		log.Printf("WARNING: NewPlugin called with empty terraform version -- runs will fail")
	}
	p := &Plugin{
		terraformVersion: terraformVersion,
	}
	p.binaryPath = p.resolveBinary()
	return p
}

// resolveBinary finds the exact terraform binary for the configured version.
// Checks versioned path first, downloads if not found. Never falls back to a different version.
func (p *Plugin) resolveBinary() string {
	// Check for versioned binary: /usr/local/bin/terraform-X.Y.Z
	versioned := "/usr/local/bin/terraform-" + p.terraformVersion
	if _, err := os.Stat(versioned); err == nil {
		log.Printf("Using terraform %s from %s", p.terraformVersion, versioned)
		return versioned
	}

	// Not installed locally - download it
	downloaded, err := downloadTerraform(p.terraformVersion)
	if err != nil {
		log.Printf("FATAL: failed to get terraform %s: %v", p.terraformVersion, err)
		// Return the versioned path anyway - commands will fail with a clear
		// "binary not found" error rather than silently using a wrong version
		return versioned
	}
	return downloaded
}

// downloadTerraform downloads the exact Terraform version and installs it as
// /usr/local/bin/terraform-X.Y.Z (or ~/.terraform-versions/ if not writable).
func downloadTerraform(version string) (string, error) {
	arch := runtime.GOARCH
	osName := runtime.GOOS
	url := fmt.Sprintf("https://releases.hashicorp.com/terraform/%s/terraform_%s_%s_%s.zip", version, version, osName, arch)
	destBin := "/usr/local/bin/terraform-" + version

	log.Printf("Downloading Terraform %s from %s", version, url)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil) //nolint:gosec // intentional: downloading terraform binary
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: URL is constructed from trusted HashiCorp release URLs
	if err != nil {
		return "", fmt.Errorf("download failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download terraform %s returned status %d", version, resp.StatusCode)
	}

	// Write zip to temp file
	tmpZip, err := os.CreateTemp("", "terraform-*.zip")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmpZip.Name()) }() //nolint:gosec // G703: removing temp file we just created

	if _, err := io.Copy(tmpZip, resp.Body); err != nil {
		_ = tmpZip.Close()
		return "", fmt.Errorf("write zip: %w", err)
	}
	_ = tmpZip.Close()

	// Unzip
	tmpDir, err := os.MkdirTemp("", "terraform-unzip-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	unzipCmd := exec.CommandContext(context.Background(), "unzip", "-o", tmpZip.Name(), "-d", tmpDir) //nolint:gosec // G204: args are safe literals + temp paths
	if out, err := unzipCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("unzip failed: %s: %w", string(out), err)
	}

	extracted := filepath.Join(tmpDir, "terraform")
	if _, err := os.Stat(extracted); err != nil {
		return "", fmt.Errorf("terraform binary not found in zip")
	}

	// Install to versioned path
	if err := copyFile(extracted, destBin); err != nil {
		// /usr/local/bin not writable - use home directory
		homeDir, _ := os.UserHomeDir()
		destBin = filepath.Join(homeDir, ".terraform-versions", "terraform-"+version)
		if mkErr := os.MkdirAll(filepath.Dir(destBin), 0o750); mkErr != nil {
			return "", fmt.Errorf("create version dir: %w", mkErr)
		}
		if err := copyFile(extracted, destBin); err != nil {
			return "", fmt.Errorf("install binary: %w", err)
		}
	}

	if err := os.Chmod(destBin, 0o755); err != nil { //nolint:gosec // G302: terraform binary must be executable
		return "", fmt.Errorf("chmod: %w", err)
	}

	log.Printf("Installed Terraform %s at %s", version, destBin)
	return destBin, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst) //nolint:gosec
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	_, err = io.Copy(out, in)
	return err
}

// PlanOptions contains optional parameters for the Plan method
type PlanOptions struct {
	// OnOutputLine is called for each line of output as it streams
	// If nil, output is only collected at the end (backward compatible)
	OnOutputLine func(line string)
	// Destroy causes the plan to be a destroy plan (terraform plan -destroy)
	// Used for destroy runs which follow the same two-phase flow as plan-and-apply
	Destroy bool
}

// ApplyOptions contains optional parameters for the Apply method
type ApplyOptions struct {
	// OnOutputLine is called for each line of output as it streams
	// If nil, output is only collected at the end (backward compatible)
	OnOutputLine func(line string)
}

func (p *Plugin) Init(ctx context.Context, workspaceDir string, config map[string]interface{}, envVars map[string]string) (*plugins.CommandResult, error) {
	// Use the config's lock file when present so plan and apply use the same provider versions.
	// Otherwise "saved plan" and current lock file can disagree and apply fails with
	// "Inconsistent dependency lock file".
	initArgs := []string{"init", "-input=false"}
	lockPath := filepath.Join(workspaceDir, ".terraform.lock.hcl")
	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		initArgs = append(initArgs, "-upgrade")
	}
	cmd := exec.CommandContext(ctx, p.binaryPath, initArgs...) //nolint:gosec // G204: intentional terraform execution
	cmd.Dir = workspaceDir

	// TFE-compatible: Enable JSON structured logging (JSONL format)
	// This matches TFE behavior where logs are in JSON Lines format
	// Preserve parent environment (especially PATH) and set TF_LOG
	// Also set environment variables (category == "env")
	cmd.Env = p.buildEnvironment(envVars)

	output, err := cmd.CombinedOutput()
	logs := string(output)
	if err != nil {
		return &plugins.CommandResult{
			Output: logs,
			Logs:   logs,
		}, fmt.Errorf("terraform init failed: %s: %w", logs, err)
	}
	return &plugins.CommandResult{
		Output: logs,
		Logs:   logs,
	}, nil
}

func (p *Plugin) Plan(ctx context.Context, workspaceDir string, variables map[string]string, envVars map[string]string) (*plugins.PlanResult, error) {
	return p.PlanWithOptions(ctx, workspaceDir, variables, envVars, nil)
}

// PlanWithOptions executes terraform plan with optional streaming callback
func (p *Plugin) PlanWithOptions(ctx context.Context, workspaceDir string, variables map[string]string, envVars map[string]string, options *PlanOptions) (*plugins.PlanResult, error) {
	// Write variables to stackweaver.auto.tfvars so we never overwrite the user's terraform.tfvars.
	// Terraform auto-loads *.auto.tfvars after terraform.tfvars; our values override for overlapping keys.
	varFile := filepath.Join(workspaceDir, "stackweaver.auto.tfvars")
	if err := p.writeVariablesFile(varFile, variables); err != nil {
		return nil, fmt.Errorf("failed to write variables file: %w", err)
	}

	// Check if using remote backend by looking for backend "remote" in terraform config
	// For remote backends, we can't use -out flag, so use -json instead
	hasRemoteBackend := p.hasRemoteBackend(workspaceDir)

	// Build plan command arguments
	// For destroy plans, add -destroy flag (TFE-compatible: destroy runs use plan -destroy + apply)
	isDestroy := options != nil && options.Destroy

	var cmd *exec.Cmd
	if hasRemoteBackend {
		// Remote backend: use -json flag to get JSON output directly
		// Don't use -out flag as it's not supported with remote backends
		args := []string{"plan", "-input=false", "-detailed-exitcode", "-json"}
		if isDestroy {
			args = append(args, "-destroy")
		}
		cmd = exec.CommandContext(ctx, p.binaryPath, args...) //nolint:gosec // G204: intentional terraform execution
	} else {
		// Local backend: can use -out flag
		planFile := filepath.Join(workspaceDir, "plan.out")
		args := []string{"plan", "-out", planFile, "-input=false", "-detailed-exitcode"}
		if isDestroy {
			args = append(args, "-destroy")
		}
		cmd = exec.CommandContext(ctx, p.binaryPath, args...) //nolint:gosec // intentional: executing terraform command
	}
	cmd.Dir = workspaceDir

	// TFE-compatible: Set environment variables
	// Environment variables (category == "env") are set as actual environment variables
	// Terraform variables (category == "terraform") are in stackweaver.auto.tfvars, no need for TF_VAR_ prefix
	cmd.Env = p.buildEnvironment(envVars)

	// Support streaming output if OnOutputLine callback is provided
	if options != nil && options.OnOutputLine != nil {
		return p.planWithStreaming(ctx, cmd, hasRemoteBackend, workspaceDir, options)
	}

	// Backward compatible: use CombinedOutput (blocking until completion)
	output, err := cmd.CombinedOutput()

	// Safely get exit code - ProcessState may be nil if command failed to start
	var exitCode int
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	} else if err != nil {
		// If ProcessState is nil and there's an error, assume failure
		exitCode = 1
	}

	// TFE-compatible: Filter out local file path messages from logs
	// Remote execution doesn't show "Saved the plan to:" messages
	logs := filterLocalPathMessages(string(output))
	result := &plugins.PlanResult{
		Output: logs,
		Logs:   logs,
	}

	// Parse plan output for counts
	if err == nil || exitCode == 0 || exitCode == 2 {
		// Exit code 2 means changes detected
		if hasRemoteBackend {
			// For remote backend, output is already JSON
			var planData map[string]interface{}
			if err := json.Unmarshal(output, &planData); err == nil {
				result.JSONOutput = planData
				result.AddCount = p.countResources(planData, "create")
				result.ChangeCount = p.countResources(planData, "update")
				result.DestroyCount = p.countResources(planData, "delete")
				result.OutputChangeCount = p.countOutputChanges(planData)
			}
		} else {
			// For local backend, read from plan file
			planFile := filepath.Join(workspaceDir, "plan.out")
			jsonCmd := exec.CommandContext(ctx, p.binaryPath, "show", "-json", planFile) //nolint:gosec // intentional: executing terraform command
			jsonCmd.Dir = workspaceDir
			jsonOutput, err := jsonCmd.Output()
			if err == nil {
				var planData map[string]interface{}
				if err := json.Unmarshal(jsonOutput, &planData); err == nil {
					result.JSONOutput = planData
					result.AddCount = p.countResources(planData, "create")
					result.ChangeCount = p.countResources(planData, "update")
					result.DestroyCount = p.countResources(planData, "delete")
					result.OutputChangeCount = p.countOutputChanges(planData)
				}
			}
		}
	}

	if err != nil && exitCode != 2 {
		// Check if error is provider-related (undeclared resource, missing provider, etc.)
		// TFE-compatible: Re-run init if providers are missing
		if p.isProviderError(logs) {
			return nil, &ProviderError{
				Message:       fmt.Sprintf("terraform plan failed: %s: %v", logs, err),
				OriginalError: err,
			}
		}
		return nil, fmt.Errorf("terraform plan failed: %s: %w", logs, err)
	}

	return result, nil
}

// planWithStreaming executes terraform plan with streaming output support
func (p *Plugin) planWithStreaming(ctx context.Context, cmd *exec.Cmd, hasRemoteBackend bool, workspaceDir string, options *PlanOptions) (*plugins.PlanResult, error) {
	// Create pipes for stdout/stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	// Start the command
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start terraform plan: %w", err)
	}

	// Capture output for final result
	var outputBuffer strings.Builder
	var wg sync.WaitGroup

	// Stream stdout
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		// Increase buffer size for large lines
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			outputBuffer.WriteString(line)
			outputBuffer.WriteString("\n")
			if options.OnOutputLine != nil {
				options.OnOutputLine(line)
			}
		}
		if err := scanner.Err(); err != nil {
			// Log scanner error but don't fail the command
			_, _ = fmt.Fprintf(&outputBuffer, "Warning: Scanner error reading stdout: %v\n", err)
		}
	}()

	// Stream stderr
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			outputBuffer.WriteString(line)
			outputBuffer.WriteString("\n")
			if options.OnOutputLine != nil {
				options.OnOutputLine(line)
			}
		}
		if err := scanner.Err(); err != nil {
			// Log scanner error but don't fail the command
			_, _ = fmt.Fprintf(&outputBuffer, "Warning: Scanner error reading stderr: %v\n", err)
		}
	}()

	// Wait for output goroutines to finish
	wg.Wait()

	// Wait for command completion
	err = cmd.Wait()

	// Get exit code
	var exitCode int
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	} else if err != nil {
		exitCode = 1
	}

	// Get full output
	outputStr := outputBuffer.String()
	logs := filterLocalPathMessages(outputStr)
	result := &plugins.PlanResult{
		Output: logs,
		Logs:   logs,
	}

	// Parse plan output for counts
	if err == nil || exitCode == 0 || exitCode == 2 {
		// Exit code 2 means changes detected
		outputBytes := []byte(outputStr)
		if hasRemoteBackend {
			// For remote backend, output is already JSON (from -json flag)
			var planData map[string]interface{}
			if err := json.Unmarshal(outputBytes, &planData); err == nil {
				result.JSONOutput = planData
				result.AddCount = p.countResources(planData, "create")
				result.ChangeCount = p.countResources(planData, "update")
				result.DestroyCount = p.countResources(planData, "delete")
				result.OutputChangeCount = p.countOutputChanges(planData)
			}
		} else {
			// For local backend, read from plan file
			planFile := filepath.Join(workspaceDir, "plan.out")
			jsonCmd := exec.CommandContext(ctx, p.binaryPath, "show", "-json", planFile) //nolint:gosec // intentional: executing terraform command
			jsonCmd.Dir = workspaceDir
			jsonOutput, err := jsonCmd.Output()
			if err == nil {
				var planData map[string]interface{}
				if err := json.Unmarshal(jsonOutput, &planData); err == nil {
					result.JSONOutput = planData
					result.AddCount = p.countResources(planData, "create")
					result.ChangeCount = p.countResources(planData, "update")
					result.DestroyCount = p.countResources(planData, "delete")
					result.OutputChangeCount = p.countOutputChanges(planData)
				}
			}
		}
	}

	if err != nil && exitCode != 2 {
		// Check if error is provider-related
		if p.isProviderError(logs) {
			return nil, &ProviderError{
				Message:       fmt.Sprintf("terraform plan failed: %s: %v", logs, err),
				OriginalError: err,
			}
		}
		return nil, fmt.Errorf("terraform plan failed: %s: %w", logs, err)
	}

	return result, nil
}

// ProviderError indicates a provider-related error that requires re-initialization
type ProviderError struct {
	Message       string
	OriginalError error
}

func (e *ProviderError) Error() string {
	return e.Message
}

func (e *ProviderError) Unwrap() error {
	return e.OriginalError
}

// isProviderError checks if the error is related to missing providers
// TFE-compatible: Only retry init on actual provider initialization failures, not configuration errors
func (p *Plugin) isProviderError(logs string) bool {
	// Provider-related error patterns (actual provider initialization failures)
	// These indicate the provider plugin itself is missing or failed to load
	providerErrorPatterns := []string{
		"Failed to instantiate provider",
		"no provider exists",
		"provider not found",
		"provider plugin",
		"Failed to query provider",
		"Provider initialization",
		"Error loading provider",
		"Could not load plugin",
		"provider requirements are not satisfied",
		"required_providers block",
	}

	// Configuration error patterns (should NOT retry init)
	// These indicate the configuration is wrong, not the provider
	configErrorPatterns := []string{
		"Reference to undeclared resource", // This is a config error, not provider error
		"Resource not found",
		"Variable not found",
		"Module not found",
	}

	logsLower := strings.ToLower(logs)

	// First check if it's a configuration error (don't retry)
	for _, pattern := range configErrorPatterns {
		if strings.Contains(logsLower, strings.ToLower(pattern)) {
			return false // This is a config error, not a provider error
		}
	}

	// Then check if it's a provider error (retry init)
	for _, pattern := range providerErrorPatterns {
		if strings.Contains(logsLower, strings.ToLower(pattern)) {
			return true
		}
	}
	return false
}

// filterLocalPathMessages filters out local file path messages from logs
// TFE-compatible: Remote execution doesn't show "Saved the plan to: /path/to/plan.out" messages
func filterLocalPathMessages(logs string) string {
	lines := strings.Split(logs, "\n")
	var filtered []string
	skipNext := false
	for _, line := range lines {
		// Skip this line if previous line was "Saved the plan to:"
		if skipNext {
			skipNext = false
			continue
		}

		// Filter out "Saved the plan to:" messages (local backend artifact)
		if strings.Contains(line, "Saved the plan to:") {
			skipNext = true // Also skip the next line (the path)
			continue
		}
		// Filter out "To perform exactly these actions, run:" messages (not applicable for remote execution)
		// Terraform outputs: "To perform exactly these actions, run the following command to apply:"
		if strings.Contains(line, "To perform exactly these actions, run") {
			// Skip this line and the next line (the terraform apply command)
			skipNext = true
			continue
		}
		// Filter out terraform apply command lines (they reference local plan files)
		if strings.Contains(line, "terraform apply") && strings.Contains(line, "plan.out") {
			continue
		}
		// Filter out lines that are just paths (absolute paths starting with /)
		// But allow lines with colons (like "Saved the plan to: /path")
		trimmedLine := strings.TrimSpace(line)
		if strings.HasPrefix(trimmedLine, "/") && !strings.Contains(line, ":") {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.Join(filtered, "\n")
}

func (p *Plugin) Apply(ctx context.Context, workspaceDir string, planFile string, envVars map[string]string) (*plugins.CommandResult, error) {
	return p.ApplyWithOptions(ctx, workspaceDir, planFile, envVars, nil)
}

// configureGracefulCancel configures a command to send SIGINT on context cancellation
// instead of the default SIGKILL. This matches TFE behavior where cancelling a run
// sends an INT signal, allowing Terraform to update state for already-changed resources
// and wrap up safely. After a grace period (WaitDelay), SIGKILL is sent as a fallback.
func configureGracefulCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		// Send SIGINT (like Ctrl+C) to allow Terraform to save state gracefully
		if cmd.Process != nil {
			return cmd.Process.Signal(syscall.SIGINT)
		}
		return nil
	}
	// Give Terraform up to 60 seconds to finish saving state after SIGINT
	// before force-killing with SIGKILL (TFE force-cancel behavior)
	cmd.WaitDelay = 60 * time.Second
}

// ApplyWithOptions executes terraform apply with optional streaming callback
func (p *Plugin) ApplyWithOptions(ctx context.Context, workspaceDir string, planFile string, envVars map[string]string, options *ApplyOptions) (*plugins.CommandResult, error) {
	// Check if using remote backend
	hasRemoteBackend := p.hasRemoteBackend(workspaceDir)

	var cmd *exec.Cmd
	if hasRemoteBackend {
		// Remote backend: apply without plan file (uses remote plan)
		cmd = exec.CommandContext(ctx, p.binaryPath, "apply", "-auto-approve", "-input=false") //nolint:gosec // G204: intentional terraform execution
	} else {
		// Local backend: apply with plan file
		cmd = exec.CommandContext(ctx, p.binaryPath, "apply", "-auto-approve", planFile) //nolint:gosec // G204: intentional terraform execution
	}
	cmd.Dir = workspaceDir

	// TFE-compatible: Send SIGINT on cancel instead of SIGKILL
	// This allows Terraform to save state for already-changed resources
	configureGracefulCancel(cmd)

	// TFE-compatible: Enable JSON structured logging and set environment variables
	cmd.Env = p.buildEnvironment(envVars)

	// Support streaming output if OnOutputLine callback is provided
	if options != nil && options.OnOutputLine != nil {
		return p.applyWithStreaming(ctx, cmd, options)
	}

	// Backward compatible: use CombinedOutput (blocking until completion)
	output, err := cmd.CombinedOutput()
	logs := string(output)
	if err != nil {
		return &plugins.CommandResult{
			Output: logs,
			Logs:   logs,
		}, fmt.Errorf("terraform apply failed: %s: %w", logs, err)
	}
	return &plugins.CommandResult{
		Output: logs,
		Logs:   logs,
	}, nil
}

// applyWithStreaming executes terraform apply with streaming output support
func (p *Plugin) applyWithStreaming(ctx context.Context, cmd *exec.Cmd, options *ApplyOptions) (*plugins.CommandResult, error) {
	// Create pipes for stdout/stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	// Start the command
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start terraform apply: %w", err)
	}

	// Capture output for final result
	var outputBuffer strings.Builder
	var wg sync.WaitGroup

	// Stream stdout
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		// Increase buffer size for large lines
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			outputBuffer.WriteString(line)
			outputBuffer.WriteString("\n")
			if options.OnOutputLine != nil {
				options.OnOutputLine(line)
			}
		}
		if err := scanner.Err(); err != nil {
			// Log scanner error but don't fail the command
			_, _ = fmt.Fprintf(&outputBuffer, "Warning: Scanner error reading stdout: %v\n", err)
		}
	}()

	// Stream stderr
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			outputBuffer.WriteString(line)
			outputBuffer.WriteString("\n")
			if options.OnOutputLine != nil {
				options.OnOutputLine(line)
			}
		}
		if err := scanner.Err(); err != nil {
			// Log scanner error but don't fail the command
			_, _ = fmt.Fprintf(&outputBuffer, "Warning: Scanner error reading stderr: %v\n", err)
		}
	}()

	// Wait for output goroutines to finish
	wg.Wait()

	// Wait for command completion
	err = cmd.Wait()

	logs := outputBuffer.String()
	if err != nil {
		return &plugins.CommandResult{
			Output: logs,
			Logs:   logs,
		}, fmt.Errorf("terraform apply failed: %s: %w", logs, err)
	}
	return &plugins.CommandResult{
		Output: logs,
		Logs:   logs,
	}, nil
}

func (p *Plugin) Destroy(ctx context.Context, workspaceDir string, envVars map[string]string) (*plugins.CommandResult, error) {
	cmd := exec.CommandContext(ctx, p.binaryPath, "destroy", "-auto-approve", "-input=false") //nolint:gosec // intentional: executing terraform destroy
	cmd.Dir = workspaceDir

	// TFE-compatible: Send SIGINT on cancel instead of SIGKILL
	configureGracefulCancel(cmd)

	// TFE-compatible: Enable JSON structured logging and set environment variables
	cmd.Env = p.buildEnvironment(envVars)

	output, err := cmd.CombinedOutput()
	logs := string(output)
	if err != nil {
		return &plugins.CommandResult{
			Output: logs,
			Logs:   logs,
		}, fmt.Errorf("terraform destroy failed: %s: %w", logs, err)
	}
	return &plugins.CommandResult{
		Output: logs,
		Logs:   logs,
	}, nil
}

func (p *Plugin) Validate(ctx context.Context, workspaceDir string, envVars map[string]string) error {
	cmd := exec.CommandContext(ctx, p.binaryPath, "validate") //nolint:gosec // intentional: executing terraform validate
	cmd.Dir = workspaceDir
	cmd.Env = p.buildEnvironment(envVars)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("terraform validate failed: %s: %w", string(output), err)
	}
	return nil
}

// hasRemoteBackend checks if the Terraform configuration uses a remote backend
func (p *Plugin) hasRemoteBackend(workspaceDir string) bool {
	// Check for backend "remote" block in terraform config files
	terraformFiles := []string{"main.tf", "terraform.tf", "backend.tf", "providers.tf"}
	for _, filename := range terraformFiles {
		filePath := filepath.Join(workspaceDir, filename)
		// Security: Validate that filePath is within workspaceDir to prevent path traversal
		cleanWorkspaceDir := filepath.Clean(workspaceDir)
		cleanFilePath := filepath.Clean(filePath)
		if !strings.HasPrefix(cleanFilePath, cleanWorkspaceDir+string(filepath.Separator)) && cleanFilePath != cleanWorkspaceDir {
			continue // Skip invalid paths
		}
		content, err := os.ReadFile(filePath) //nolint:gosec // G304: path is validated against workspace directory above
		if err != nil {
			continue
		}
		// Simple check for "backend \"remote\"" pattern
		contentStr := string(content)
		if strings.Contains(contentStr, "backend \"remote\"") {
			return true
		}
	}
	return false
}

// buildEnvironment builds the environment variable array for Terraform commands.
// TFE-compatible: Sets TF_LOG=JSON and includes environment variables (category == "env").
// Environment variables are set with their key as-is (not prefixed with TF_VAR_).
func (p *Plugin) buildEnvironment(envVars map[string]string) []string {
	// Start with parent environment (especially PATH)
	env := os.Environ()
	// Add TF_LOG=JSON for structured logging (TFE-compatible)
	env = append(env, "TF_LOG=JSON")
	// Add environment variables (category == "env")
	// These are set as actual environment variables, not as TF_VAR_ prefixed vars
	for key, value := range envVars {
		env = append(env, fmt.Sprintf("%s=%s", key, value))
	}
	return env
}

func (p *Plugin) writeVariablesFile(path string, variables map[string]string) error {
	var lines []string
	for key, value := range variables {
		lines = append(lines, fmt.Sprintf("%s = %q", key, value))
	}
	// Use restrictive permissions (0600) to protect sensitive data
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
}

func (p *Plugin) countResources(planData map[string]interface{}, action string) int {
	count := 0
	if resourceChanges, ok := planData["resource_changes"].([]interface{}); ok {
		for _, change := range resourceChanges {
			if changeMap, ok := change.(map[string]interface{}); ok {
				if changeData, ok := changeMap["change"].(map[string]interface{}); ok {
					if actions, ok := changeData["actions"].([]interface{}); ok {
						// Check if the action matches any of the actions in the array
						for _, a := range actions {
							if actionStr, ok := a.(string); ok && actionStr == action {
								count++
								break // Count each resource only once
							}
						}
					}
				}
			}
		}
	}
	return count
}

// countOutputChanges counts non-no-op output changes in the plan JSON.
// Terraform's plan JSON has "output_changes" as a map of output name to change object.
// Each change object has "actions" (e.g. ["create"], ["update"], ["delete"], ["no-op"]).
func (p *Plugin) countOutputChanges(planData map[string]interface{}) int {
	count := 0
	outputChanges, ok := planData["output_changes"].(map[string]interface{})
	if !ok {
		return 0
	}
	for _, change := range outputChanges {
		changeMap, ok := change.(map[string]interface{})
		if !ok {
			continue
		}
		actions, ok := changeMap["actions"].([]interface{})
		if !ok {
			continue
		}
		for _, a := range actions {
			if actionStr, ok := a.(string); ok && actionStr != "no-op" {
				count++
				break
			}
		}
	}
	return count
}
