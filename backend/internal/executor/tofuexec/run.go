// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// Package tofuexec is the OpenTofu executor (#933, D2): it runs one phase of one run, plan or apply,
// from the artifacts the API serves it, and reports the result. The same code runs in the
// launcher's process for the in-process driver and in a container or pod for every other driver.
package tofuexec

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/michielvha/logger"
	"github.com/michielvha/stackweaver/backend/internal/executor"
	"github.com/michielvha/stackweaver/backend/internal/executor/cachedesc"
	"github.com/michielvha/stackweaver/backend/internal/executor/identity"
	"github.com/michielvha/stackweaver/core/runnercontract"
	"github.com/michielvha/stackweaver/core/services/logparser"
	"github.com/michielvha/stackweaver/core/tofu"
)

// Job is the phase of a run the executor runs.
type Job struct {
	ID string
	// RunType is the offer's run type: plan, plan-only, plan-destroy, apply or apply-destroy.
	RunType string
}

// Options tune an execution; the zero value is what production uses.
type Options struct {
	// Console receives a copy of the engine's output; nil means the process's stdout and stderr.
	Console io.Writer
	// StatusInterval is how often the job's status is polled for a stop; zero means two seconds.
	StatusInterval time.Duration
	// CacheDir is the launcher's read-only copy of the organization's cache; empty for none.
	CacheDir string
}

// Result is the outcome of a phase.
type Result struct {
	// Status is what was reported: completed, failed or canceled.
	Status   string
	ExitCode int
	Output   string
	Err      error
	// Manifest is what a completed run installed beyond its cache descriptor.
	Manifest *runnercontract.CacheManifest
}

// Execute runs one phase of an OpenTofu run end to end: it fetches the artifacts, runs the engine
// and reports the completion, and returns what it reported.
func Execute(ctx context.Context, c *executor.Client, job Job, opts Options) Result {
	var arts runnercontract.TofuArtifacts
	if err := c.Artifacts(ctx, job.ID, &arts, func() int { return arts.ContractVersion }); err != nil {
		if errors.Is(err, runnercontract.ErrUnknownVersion) {
			_, _, phase := phaseArgs(job.RunType, "", "")
			c.ReportRefusal(ctx, job.ID, phase, err)
		}
		res := Result{Status: "failed", ExitCode: 1, Err: err, Output: fmt.Sprintf("Failed to download artifacts: %v", err)}
		complete(ctx, c, job, res, "")
		return res
	}
	res, planJSON := run(ctx, c, job, &arts, opts)
	complete(ctx, c, job, res, planJSON)
	return res
}

// complete reports a phase's result, with the plan's resource counts after a successful plan.
func complete(ctx context.Context, c *executor.Client, job Job, res Result, planJSON string) {
	req := runnercontract.JobCompleteRequest{Status: res.Status, ExitCode: res.ExitCode, Output: res.Output}
	if res.Status == "completed" {
		req.CacheManifest = res.Manifest
	}
	if res.Status == "failed" {
		msg := res.Output
		if res.Err != nil {
			msg = fmt.Sprintf("Execution failed: %v", res.Err)
			if res.Output != "" {
				msg += "\n\nOutput:\n" + res.Output
			}
		}
		req.ErrorMessage = msg
	}
	if planJSON != "" && res.Status == "completed" {
		meta := parsePlanJSON(planJSON)
		req.ResourceAdditions = meta.Additions
		req.ResourceChanges = meta.Changes
		req.ResourceDestructions = meta.Destructions
		req.OutputChanges = meta.OutputChanges
		req.HasChanges = meta.HasChanges
		req.PlanJSON = planJSON
	}
	// The completion is reported even after a stop: a canceled job still records how it ended.
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := c.Complete(reportCtx, job.ID, req); err != nil {
		logger.Warnf("Failed to report the completion of job %s: %v", job.ID, err)
	}
}

// run executes the phase. It watches the job's status and stops the engine when the API says the
// job is over for this executor.
func run(ctx context.Context, c *executor.Client, job Job, arts *runnercontract.TofuArtifacts, opts Options) (Result, string) {
	failed := func(output string, err error) (Result, string) {
		return Result{Status: "failed", ExitCode: 1, Output: output, Err: err}, ""
	}
	workDir, err := os.MkdirTemp("", "terraform-job-"+job.ID)
	if err != nil {
		return failed("", fmt.Errorf("creating the work directory: %w", err))
	}
	defer func() { _ = os.RemoveAll(workDir) }()
	// Every credential the run materialises (tfvars, identity tokens, the CLI configuration, the CA
	// bundle, the saved plan) goes into one secret directory outside the working tree, shredded on
	// every exit path (AC25).
	secrets, err := executor.NewSecretDir("")
	if err != nil {
		return failed("", err)
	}
	defer func() { _ = secrets.Shred() }()
	planPath := secrets.Path(savedPlanFile)

	if err := materializeSource(ctx, c, job, arts, workDir); err != nil {
		return failed("", err)
	}
	// The organization's cache: fetched entries land beside the work directory, never in it, so
	// nothing of the cache reaches the run's state or a saved plan.
	scratch, err := os.MkdirTemp("", "terraform-cache-"+job.ID)
	if err != nil {
		return failed("", fmt.Errorf("creating the cache directory: %w", err))
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	cache := newCacheUse(c, opts.CacheDir, arts.Cache, scratch)
	cache.placeProviders(ctx)

	terraformDir := workDir
	if arts.WorkingDirectory != "" && arts.WorkingDirectory != "." && arts.WorkingDirectory != "/" {
		terraformDir = filepath.Join(workDir, strings.TrimPrefix(arts.WorkingDirectory, "/"))
		// AUD-074: reject a working directory that escapes the per-job temp dir (e.g. "../../etc").
		if terraformDir != workDir && !strings.HasPrefix(terraformDir, workDir+string(os.PathSeparator)) {
			return failed("", fmt.Errorf("invalid working directory (escapes job root): %s", arts.WorkingDirectory))
		}
	}

	// The run's own variables, then what the executor adds: the per-run identities materialised into
	// files, the deployment's mirror block and the job's registry tokens.
	vars, err := runVars(ctx, arts, secrets, cache)
	if err != nil {
		return failed("", err)
	}

	varFile, err := writeTFVars(secrets, arts)
	if err != nil {
		return failed("", err)
	}
	replaceRemoteBackend(terraformDir)

	if err := restoreState(ctx, c, arts, terraformDir); err != nil {
		logger.Warnf("Failed to restore state for job %s: %v", job.ID, err)
	}

	releases := ""
	if arts.Mirror != nil {
		releases = arts.Mirror.TofuReleasesURL
		// The executor's own download of the engine trusts the mirror block's CA, as the engine and
		// the run's tools do through SSL_CERT_FILE: an air-gapped mirror behind the install's CA.
		if arts.Mirror.CACertificates != "" {
			if err := tofu.SetExtraRootCAs([]byte(arts.Mirror.CACertificates)); err != nil {
				logger.Warnf("The mirror block's CA bundle is unusable: %v", err)
			}
		}
	}
	tfBinary := cache.tofuBinary(ctx, arts.TofuVersion, releases)
	tfEnv := tofu.RunEnv(vars)

	args, isPlan, logPhase := phaseArgs(job.RunType, planPath, varFile)

	// The apply phase applies the plan phase's saved plan as it is (AC6): never a second plan,
	// which would apply something nobody reviewed. OpenTofu refuses a saved plan the state moved
	// past, and that refusal is the run's failure. The plan is fetched before init so init installs
	// the providers the plan selected, under the dependency lock file the plan recorded.
	if !isPlan {
		if arts.Plan == nil {
			return failed("", errors.New("the run has no saved plan from its plan phase to apply; start a new run to plan it"))
		}
		dlCtx, cancelDL := context.WithTimeout(ctx, 10*time.Minute)
		err := c.Download(dlCtx, arts.Plan.Path, planPath, maxSavedPlanBytes)
		cancelDL()
		if err != nil {
			return failed("", fmt.Errorf("downloading the saved plan: %w", err))
		}
		restorePlanLockFile(planPath, terraformDir)
	}

	initCmd := exec.CommandContext(ctx, tfBinary, "init", "-no-color", "-input=false", "-reconfigure") //nolint:gosec // G204: intentional opentofu execution
	initCmd.Dir = terraformDir
	// The run's environment never carries the executor's own (its job token, whatever the host
	// holds): tofu.RunEnv admits only what OpenTofu needs and what the operator passes through.
	initCmd.Env = tfEnv
	if out, err := initCmd.CombinedOutput(); err != nil {
		return failed(string(out), fmt.Errorf("opentofu init failed: %s: %w", string(out), err))
	}
	cache.reportProviders(terraformDir)

	var planned []logparser.PlannedResource
	if !isPlan {
		planned = plannedResources(ctx, tfBinary, terraformDir, tfEnv, planPath)
	}

	runCtx, cancelRun := context.WithCancelCause(ctx)
	defer cancelRun(nil)
	interval := opts.StatusInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	go c.WatchCancellation(runCtx, job.ID, interval, cancelRun)

	cmd := exec.CommandContext(runCtx, tfBinary, args...) //nolint:gosec // G204: intentional opentofu execution
	cmd.Dir = terraformDir
	cmd.Env = tfEnv
	// TFE-compatible: SIGINT on cancel, so OpenTofu saves state for resources it already changed.
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return cmd.Process.Signal(syscall.SIGINT)
		}
		return nil
	}
	cmd.WaitDelay = 60 * time.Second

	fullOutput := &bytes.Buffer{}
	streamer := &streamWriter{ctx: runCtx, client: c, jobID: job.ID, phase: logPhase, buffer: &bytes.Buffer{}, lastSend: time.Now(), planned: planned}
	stdout, stderr := io.Writer(os.Stdout), io.Writer(os.Stderr)
	if opts.Console != nil {
		stdout, stderr = opts.Console, opts.Console
	}
	cmd.Stdout, cmd.Stderr = jobOutputWriters(streamer, fullOutput, stdout, stderr)
	// The log opens with what the organization's cache supplied (AC33).
	if line := cache.summary(); line != "" {
		_, _ = cmd.Stdout.Write([]byte(line))
	}

	runErr := cmd.Run()
	streamer.final = true
	streamer.flush() //nolint:contextcheck // the writer carries the run's context

	// Stopped by the API (canceled, revoked) or by the driver stopping the placement (SIGTERM).
	if errors.Is(context.Cause(runCtx), executor.ErrJobStopped) || ctx.Err() != nil {
		// Upload what a stopped apply already changed, so state does not drift from the resources.
		if !isPlan {
			uploadState(ctx, c, terraformDir, job.ID)
		}
		return Result{Status: "canceled", ExitCode: 1, Output: fullOutput.String(), Err: runErr}, ""
	}

	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return failed(fullOutput.String(), runErr)
		}
		exitCode = exitErr.ExitCode()
	}

	var planJSON string
	if isPlan && exitCode == 0 {
		planJSON = extractPlanJSON(ctx, tfBinary, terraformDir, tfEnv, planPath)
		// The saved plan waits in the API for the apply phase; a plan-only run is never applied.
		if job.RunType != "plan-only" {
			if err := uploadPlan(ctx, c, planPath, job.ID); err != nil {
				return failed(fullOutput.String(), err)
			}
		}
	}
	if !isPlan && exitCode == 0 {
		uploadState(ctx, c, terraformDir, job.ID)
	}
	status := "completed"
	if exitCode != 0 {
		status = "failed"
	}
	return Result{Status: status, ExitCode: exitCode, Output: fullOutput.String(), Manifest: cache.report()}, planJSON
}

// phaseArgs maps an offer's run type to the OpenTofu command. Destroy runs follow the same
// two-phase flow as plan-and-apply: the plan phase saves its plan, the apply phase applies it.
func phaseArgs(runType, planPath, varFile string) (args []string, isPlan bool, logPhase string) {
	plan := func(extra ...string) []string {
		a := append([]string{"plan"}, extra...)
		a = append(a, "-no-color", "-input=false", "-out="+planPath)
		if varFile != "" {
			a = append(a, "-var-file="+varFile)
		}
		return a
	}
	switch runType {
	case "plan-destroy":
		return plan("-destroy"), true, "plan"
	case "apply", "apply-destroy":
		// A destroy and the variables are recorded in the saved plan, so both apply it the same way.
		return []string{"apply", "-no-color", "-input=false", planPath}, false, "apply"
	default: // plan, plan-only
		return plan(), true, "plan"
	}
}

// materializeSource places the configuration in workDir: the tarball (inline or as a blob), or a
// clone for an agent that predates snapshots.
func materializeSource(ctx context.Context, c *executor.Client, job Job, arts *runnercontract.TofuArtifacts, workDir string) error {
	switch {
	case arts.ConfigTarballBlob != nil:
		data, err := c.Blob(ctx, *arts.ConfigTarballBlob)
		if err != nil {
			return err
		}
		if err := ExtractTarGz(data, workDir); err != nil {
			return fmt.Errorf("extracting the configuration tarball: %w", err)
		}
	case arts.ConfigTarball != "":
		data, err := base64.StdEncoding.DecodeString(arts.ConfigTarball)
		if err != nil {
			return fmt.Errorf("decoding the configuration tarball: %w", err)
		}
		if err := ExtractTarGz(data, workDir); err != nil {
			return fmt.Errorf("extracting the configuration tarball: %w", err)
		}
	default:
		// The executor never clones (#933, decision 18): a VCS-backed run starts from the
		// configuration version a configuration_snapshot work item captured.
		logger.Warnf("No configuration for job %s", job.ID)
	}
	return nil
}

// writeTFVars writes the run's variables into the secret directory and returns the file the plan
// reads with -var-file, or "" when the run has none. AUD-022: HCL-typed variables are written
// unquoted.
func writeTFVars(secrets *executor.SecretDir, arts *runnercontract.TofuArtifacts) (string, error) {
	if len(arts.Variables) == 0 {
		return "", nil
	}
	hcl := make(map[string]struct{}, len(arts.VariablesHCL))
	for _, k := range arts.VariablesHCL {
		hcl[k] = struct{}{}
	}
	var b strings.Builder
	for key, value := range arts.Variables {
		if _, isHCL := hcl[key]; isHCL {
			_, _ = fmt.Fprintf(&b, "%s = %s\n", key, value)
		} else {
			_, _ = fmt.Fprintf(&b, "%s = %q\n", key, value)
		}
	}
	return secrets.Write("stackweaver.tfvars", []byte(b.String()))
}

// restoreState writes the workspace's latest state into dir, inline or from a blob.
func restoreState(ctx context.Context, c *executor.Client, arts *runnercontract.TofuArtifacts, dir string) error {
	var data []byte
	switch {
	case arts.StateBlob != nil:
		b, err := c.Blob(ctx, *arts.StateBlob)
		if err != nil {
			return err
		}
		data = b
	case arts.StateJSON != "":
		b, err := base64.StdEncoding.DecodeString(arts.StateJSON)
		if err != nil {
			return fmt.Errorf("decoding the state: %w", err)
		}
		data = b
	default:
		return nil
	}
	return os.WriteFile(filepath.Join(dir, "terraform.tfstate"), data, 0o600)
}

// extractPlanJSON runs `tofu show -json` on the saved plan.
func extractPlanJSON(ctx context.Context, tfBinary, dir string, env []string, planPath string) string {
	if _, err := os.Stat(planPath); err != nil {
		return ""
	}
	show := exec.CommandContext(ctx, tfBinary, "show", "-json", planPath) //nolint:gosec // G204: intentional opentofu execution
	show.Dir = dir
	show.Env = env
	out, err := show.CombinedOutput()
	if err != nil {
		logger.Warnf("Failed to extract plan JSON: %v", err)
		return ""
	}
	return string(out)
}

// uploadState uploads the state an apply left in dir.
func uploadState(ctx context.Context, c *executor.Client, dir, jobID string) {
	data, err := os.ReadFile(filepath.Join(dir, "terraform.tfstate")) //nolint:gosec // G304: inside the job's own work dir
	if err != nil || len(data) == 0 {
		return
	}
	upCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	if err := c.UploadState(upCtx, jobID, data); err != nil {
		logger.Warnf("Failed to upload state for job %s: %v", jobID, err)
	}
}

// streamWriter streams engine output to the API, at most once a second or every 4 KiB, with the
// phase's per-resource state parsed from everything so far (AC8): at most every phaseStateEvery and
// on the final flush, which the API stores as the run's RunPhaseState.
type streamWriter struct {
	ctx      context.Context //nolint:containedctx // the writer lives exactly as long as the phase it streams
	client   *executor.Client
	jobID    string
	phase    string
	buffer   *bytes.Buffer
	lastSend time.Time
	// planned are the saved plan's resource changes, which an apply's state starts from.
	planned []logparser.PlannedResource
	// final is set before the last flush, which always carries the phase state.
	final     bool
	all       bytes.Buffer
	lastState time.Time
}

// phaseStateEvery bounds how often a phase's whole output is parsed for its state.
const phaseStateEvery = 2 * time.Second

func (w *streamWriter) Write(p []byte) (int, error) {
	w.all.Write(p)
	n, err := w.buffer.Write(p)
	if time.Since(w.lastSend) > time.Second || w.buffer.Len() > 4096 {
		w.flush()
	}
	return n, err
}

// flush sends what is buffered. AUD-028: synchronously, so one executor is the sole, ordered writer
// of its run's log.
func (w *streamWriter) flush() {
	if w.buffer.Len() == 0 && !w.final {
		return
	}
	req := runnercontract.JobOutputRequest{Output: w.buffer.String(), Stream: w.phase}
	w.buffer.Reset()
	w.lastSend = time.Now()
	if w.final || time.Since(w.lastState) >= phaseStateEvery {
		w.lastState = time.Now()
		if parsed, err := logparser.ParseApplyLogs(w.all.String(), w.planned); err == nil {
			req.PhaseState = &runnercontract.PhaseState{Phase: w.phase, Resources: parsed.Resources, Summary: parsed.Summary}
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(w.ctx), 10*time.Second)
	defer cancel()
	if err := w.client.Output(ctx, w.jobID, req); err != nil {
		logger.Warnf("Failed to send job output: %v", err)
	}
}

// savedPlanFile is the saved plan's name in the run's secret directory.
const savedPlanFile = "tfplan"

// maxSavedPlanBytes bounds a saved plan download; the API refuses larger uploads.
const maxSavedPlanBytes = 256 << 20

// uploadPlan hands the plan phase's saved plan to the API for the apply phase. A plan that cannot
// be stored fails the phase: the apply would otherwise have nothing to apply.
func uploadPlan(ctx context.Context, c *executor.Client, planPath, jobID string) error {
	data, err := os.ReadFile(planPath) //nolint:gosec // G304: inside the job's own secret dir
	if err != nil {
		return fmt.Errorf("reading the saved plan: %w", err)
	}
	upCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	if err := c.UploadPlan(upCtx, jobID, data); err != nil {
		return fmt.Errorf("uploading the saved plan: %w", err)
	}
	return nil
}

// plannedResources are the saved plan's resource changes, read with `tofu show -json`, which an
// apply's phase state starts from.
// restorePlanLockFile writes the dependency lock file a saved plan recorded into the working
// directory, so the apply phase's init selects exactly the provider packages the plan phase did.
// Without it a provider the cache supplied in one phase and the registry in the other leaves lock
// files with different hash sets, which OpenTofu refuses as an inconsistent dependency lock file.
// A plan that is no zip, or holds no lock file, leaves the configuration's.
func restorePlanLockFile(planPath, terraformDir string) {
	zr, err := zip.OpenReader(planPath)
	if err != nil {
		return
	}
	defer func() { _ = zr.Close() }()
	for _, f := range zr.File {
		if f.Name != cachedesc.LockFileName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return
		}
		data, err := io.ReadAll(io.LimitReader(rc, 1<<20))
		_ = rc.Close()
		if err != nil {
			return
		}
		if err := os.WriteFile(filepath.Join(terraformDir, cachedesc.LockFileName), data, 0o600); err != nil {
			logger.Warnf("Restoring the saved plan's dependency lock file: %v", err)
		}
		return
	}
}

func plannedResources(ctx context.Context, tfBinary, dir string, env []string, planPath string) []logparser.PlannedResource {
	planJSON := extractPlanJSON(ctx, tfBinary, dir, env, planPath)
	if planJSON == "" {
		return nil
	}
	var plan map[string]any
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		return nil
	}
	return logparser.ExtractPlannedResourcesFromPlanOutput(plan)
}

// runVars is the environment the run's tofu processes get beyond what tofu.RunEnv inherits: the
// run's own variables, its per-run identities materialised into files in the working directory,
// the deployment's mirror block and the job's registry tokens. A run variable wins over the mirror
// block; the job's token for the platform's own registry wins over a run variable, which could only
// be a stale long-lived token. An identity that cannot be materialised fails the run, because a run
// without it would act as whatever identity the machine has.
func runVars(ctx context.Context, arts *runnercontract.TofuArtifacts, secrets *executor.SecretDir, cache *cacheUse) (map[string]string, error) {
	vars := map[string]string{}
	mirror, err := executor.MirrorEnv(arts.Mirror, secrets.Dir())
	if err != nil {
		return nil, err
	}
	maps.Copy(vars, mirror)
	networkMirror := ""
	if arts.Mirror != nil {
		networkMirror = arts.Mirror.ProviderNetworkMirror
	}
	if cfg := providerInstallation(networkMirror, cache.mirror, cache.include); cfg != "" {
		path, err := secrets.Write("stackweaver.tofurc", []byte(cfg))
		if err != nil {
			return nil, fmt.Errorf("writing the CLI configuration: %w", err)
		}
		if inherited := os.Getenv("TF_CLI_CONFIG_FILE"); inherited != "" {
			logger.Warnf("The run's provider installation replaces the runner's TF_CLI_CONFIG_FILE %s for this run", inherited)
		}
		vars["TF_CLI_CONFIG_FILE"] = path
	}
	maps.Copy(vars, arts.EnvironmentVars)
	if err := identity.MaterializeAWSWebIdentityToken(vars, secrets.Dir()); err != nil {
		return nil, fmt.Errorf("writing the AWS web identity token: %w", err)
	}
	if err := identity.MaterializeAzureFederatedToken(vars, secrets.Dir()); err != nil {
		return nil, err
	}
	if err := identity.MaterializeGCPWorkloadIdentity(vars, secrets.Dir()); err != nil {
		return nil, fmt.Errorf("writing the GCP workload identity configuration: %w", err)
	}
	vaultCtx, cancelVault := context.WithTimeout(ctx, 30*time.Second)
	defer cancelVault()
	if err := identity.MaterializeVaultToken(vaultCtx, vars, secrets.Dir()); err != nil {
		return nil, fmt.Errorf("logging in to Vault with the run's identity: %w", err)
	}
	if arts.Registry != nil {
		for _, h := range arts.Registry.Hosts {
			if h.Host != "" && h.Token != "" {
				vars[runnercontract.TofuTokenEnvName(h.Host)] = h.Token
			}
		}
	}
	return vars, nil
}

// cliConfig is an OpenTofu CLI configuration installing every provider from the network mirror at
// url, which OpenTofu requires to end with a slash.
func cliConfig(url string) string {
	if !strings.HasSuffix(url, "/") {
		url += "/"
	}
	return fmt.Sprintf("provider_installation {\n  network_mirror {\n    url = %q\n  }\n}\n", url)
}
