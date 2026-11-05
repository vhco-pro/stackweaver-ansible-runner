// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// Package ansibleexec is the Ansible executor (#933, D2): it runs one Ansible job from the
// artifacts the API serves it, streams the play's output and events, and reports the result. The
// same code runs in the launcher's process for the in-process driver and inside a container or
// pod, as the ansible-runner binary's `executor` subcommand, for every other driver.
package ansibleexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/michielvha/logger"
	"github.com/michielvha/stackweaver/backend/internal/executor"
	"github.com/michielvha/stackweaver/backend/internal/executor/identity"
	"github.com/michielvha/stackweaver/core/runnercontract"
	"github.com/michielvha/stackweaver/core/services/ansible"
)

// Job is the Ansible job the executor runs.
type Job struct {
	ID string
}

// Options tune an execution; the zero value is what production uses.
type Options struct {
	// CacheDir is the launcher's read-only copy of the organization's cache; empty for none.
	CacheDir string
	// WorkRoot is where the job's work directory is made; empty means the system temp directory.
	WorkRoot string
	// StatusInterval is how often the job's status is polled for a stop; zero means two seconds.
	StatusInterval time.Duration
}

// Result is the outcome of a job.
type Result struct {
	// Status is what was reported: completed, failed or canceled.
	Status       string
	ErrorMessage string
	// Manifest is what a completed job installed beyond its cache descriptor.
	Manifest *runnercontract.CacheManifest
}

// maxPlaybookSnapshotBytes caps a snapshot download. A snapshot is at most a whole repository;
// anything near this size is a mistake or an attack on the host's disk.
const maxPlaybookSnapshotBytes = 1 << 30

// Execute runs one Ansible job end to end: it fetches the artifacts, runs the play and reports the
// completion, and returns what it reported.
func Execute(ctx context.Context, c *executor.Client, job Job, opts Options) Result {
	var arts runnercontract.AnsibleArtifacts
	var res Result
	if err := c.Artifacts(ctx, job.ID, &arts, func() int { return arts.ContractVersion }); err != nil {
		if errors.Is(err, runnercontract.ErrUnknownVersion) {
			c.ReportRefusal(ctx, job.ID, "stderr", err)
		}
		res = Result{Status: "failed", ErrorMessage: "Failed to download job artifacts: " + err.Error()}
	} else {
		res = run(ctx, c, job, &arts, opts)
	}
	// The completion is reported even after a stop: a canceled job still records how it ended.
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	req := runnercontract.JobCompleteRequest{Status: res.Status, ErrorMessage: res.ErrorMessage}
	if res.Status == "completed" {
		req.CacheManifest = res.Manifest
	}
	if err := c.Complete(reportCtx, job.ID, req); err != nil {
		logger.Warnf("Failed to report the completion of job %s: %v", job.ID, err)
	}
	return res
}

// run executes the job. It watches the job's status and stops whichever step is running, a
// snapshot download or a Galaxy install as much as the play, when the API says the job is over for
// this executor.
func run(ctx context.Context, c *executor.Client, job Job, arts *runnercontract.AnsibleArtifacts, opts Options) Result {
	failed := func(msg string) Result { return Result{Status: "failed", ErrorMessage: msg} }

	root := opts.WorkRoot
	if root == "" {
		root = os.TempDir()
	}
	workDir := filepath.Join(root, "ansible-job-"+job.ID)
	// A work directory left by an earlier attempt on this host is discarded, not reused.
	_ = os.RemoveAll(workDir)
	if err := os.Mkdir(workDir, 0o700); err != nil {
		return failed("Failed to create work directory: " + err.Error())
	}
	defer func() { _ = os.RemoveAll(workDir) }()
	// Every credential the job materialises (the inventory with injected credentials, vault and SSH
	// key files, extra and become vars, ansible.cfg, cloud identity files, the CA bundle) goes into
	// one secret directory outside the working tree, shredded on every exit path (AC25).
	secrets, err := executor.NewSecretDir(root)
	if err != nil {
		return failed("Failed to create the secret directory: " + err.Error())
	}
	defer func() { _ = secrets.Shred() }()

	interval := opts.StatusInterval
	if interval == 0 {
		interval = 2 * time.Second
	}
	jobCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	go c.WatchCancellation(jobCtx, job.ID, interval, stop)
	stopped := func() bool { return jobCtx.Err() != nil }
	events := apiSink{client: c, jobID: job.ID}

	// The deployment's mirrors, proxy and CA, for the clone, the Galaxy install and the play alike.
	mirrorEnv, err := executor.MirrorEnv(arts.Mirror, secrets.Dir())
	if err != nil {
		return failed("Failed to apply the deployment's mirror settings: " + err.Error())
	}
	toolEnv := make([]string, 0, len(mirrorEnv)+2)
	for k, v := range mirrorEnv {
		toolEnv = append(toolEnv, k+"="+v)
	}

	// The playbook files: the cached snapshot when the playbook has one, otherwise a clone.
	repoDir := workDir
	checkoutDir := filepath.Join(workDir, "repo")
	switch {
	case arts.PlaybookSource.HasSnapshot():
		source := arts.PlaybookSource
		logger.Infof("Running the playbook snapshot (mode %s, commit %s)", source.Mode, source.Commit)
		snapshotPath := filepath.Join(workDir, "snapshot.tar.gz")
		dlCtx, cancelDL := context.WithTimeout(jobCtx, 10*time.Minute)
		err := c.Download(dlCtx, "/api/v2/runner/jobs/"+job.ID+"/playbook-snapshot", snapshotPath, maxPlaybookSnapshotBytes)
		cancelDL()
		if err != nil {
			if stopped() {
				return Result{Status: "canceled"}
			}
			return failed("Failed to download playbook snapshot: " + err.Error())
		}
		if err := extractFile(snapshotPath, checkoutDir); err != nil {
			return failed("Failed to extract playbook snapshot: " + err.Error())
		}
		repoDir = checkoutDir
		banner := PlaybookSourceMessage(source.Commit, source.CapturedAt)
		if source.Mode == runnercontract.PlaybookSourceFresh {
			banner = FreshSourceMessage(source.Commit)
		}
		EmitEvent(jobCtx, events, PlaybookSourceEvent(banner))
	default:
		// The executor never clones (#933, decision 18): a VCS-backed playbook runs from its
		// snapshot, and only an ad hoc job carries its playbook inline.
		logger.Warnf("Job %s carries no playbook snapshot - its playbook files are not available", job.ID)
	}

	cfgPath := secrets.Path("ansible.cfg")
	if arts.AnsibleCfg != "" {
		if err := os.WriteFile(cfgPath, []byte(arts.AnsibleCfg), 0o600); err != nil {
			return failed("Failed to write ansible.cfg: " + err.Error())
		}
		// ansible-galaxy reads its server list from ansible.cfg, so the Galaxy install below must
		// see the job's config too: that is how a private hub is reached.
		toolEnv = append(toolEnv, "ANSIBLE_CONFIG="+cfgPath)
	}
	// The deployment's Galaxy servers are the default list: a job whose ansible.cfg names its own
	// server_list keeps it.
	if list := executor.GalaxyServerList(arts.Mirror); list != "" && !galaxyServerListConfigured(ansibleCfgCandidates(cfgPath, repoDir, "")) {
		toolEnv = append(toolEnv, "ANSIBLE_GALAXY_SERVER_LIST="+list)
	}

	// The repository's Galaxy requirements, if it has any. A failed install is reported in the job
	// output and the play still runs.
	galaxyCache := &GalaxyCache{Reader: &executor.Cache{Dir: opts.CacheDir, Client: c}}
	if arts.Cache != nil {
		galaxyCache.Entries = arts.Cache.Entries
	}
	galaxyCollections, galaxyRoles, galaxyErr := InstallGalaxyRequirements(jobCtx, events, GalaxyInstall{
		JobDir:      workDir,
		PlaybookDir: repoDir,
		Env:         toolEnv,
		Cache:       galaxyCache,
	})
	if galaxyErr != nil {
		logger.Warnf("Failed to install Galaxy requirements: %v", galaxyErr)
	}
	// Staged means the repository has a requirements file and its install succeeded: the helper
	// returns directories only then.
	galaxyStaged := galaxyCollections != ""
	if stopped() {
		return Result{Status: "canceled"}
	}

	// The inventory is written as .json so Ansible parses it as JSON and picks up host vars such as
	// ansible_password.
	inventoryPath := secrets.Path("inventory.json")
	if arts.InventoryContent != "" {
		if err := os.WriteFile(inventoryPath, []byte(arts.InventoryContent), 0o600); err != nil {
			return failed("Failed to write inventory: " + err.Error())
		}
	}
	logCredentialShape(arts)

	// SSH password authentication needs sshpass; Ansible cannot use ansible_password without it.
	if arts.Credential != nil && arts.Credential.Password != "" && arts.Credential.SSHKey == "" {
		if _, err := exec.LookPath("sshpass"); err != nil {
			return failed("SSH password authentication requires sshpass on the runner. Install it (e.g. apt install sshpass or yum install sshpass) and try again.")
		}
	}

	var extraVarsPath string
	if len(arts.ExtraVars) > 0 {
		extraVarsPath = secrets.Path("extra_vars.json")
		varsJSON, err := json.Marshal(arts.ExtraVars)
		if err != nil {
			return failed("Failed to marshal extra vars: " + err.Error())
		}
		if err := os.WriteFile(extraVarsPath, varsJSON, 0o600); err != nil {
			return failed("Failed to write extra vars: " + err.Error())
		}
	}

	var sshKeyPath string
	if arts.Credential != nil && arts.Credential.SSHKey != "" {
		sshKeyPath = secrets.Path("ssh_key")
		if err := os.WriteFile(sshKeyPath, []byte(arts.Credential.SSHKey), 0o600); err != nil {
			return failed("Failed to write SSH key: " + err.Error())
		}
	}

	// Ad hoc jobs ship the generated transient playbook as content instead of a repository; it is
	// written into the work directory, which is repoDir when no repository was prepared.
	if arts.PlaybookContent != "" {
		contentName := arts.PlaybookPath
		if contentName == "" {
			contentName = "adhoc.yml"
		}
		if err := os.WriteFile(filepath.Join(workDir, contentName), []byte(arts.PlaybookContent), 0o600); err != nil {
			return failed("Failed to write playbook content: " + err.Error())
		}
	}

	playbookPath, msg := resolvePlaybook(arts, repoDir)
	if msg != "" {
		return failed(msg)
	}

	args := []string{"-i", inventoryPath}
	args = append(args, jobConfigArgs(arts.JobConfig)...)
	if extraVarsPath != "" {
		args = append(args, "-e", "@"+extraVarsPath)
	}

	// Vault credentials: one file per vault, labelled with --vault-id. A single unlabelled vault
	// keeps the historical environment variable.
	var unlabeledVaultFile string
	for i, vault := range arts.Vaults {
		vaultFile := secrets.Path(fmt.Sprintf("vault_pass_%d", i))
		if err := os.WriteFile(vaultFile, []byte(vault.Password), 0o600); err != nil {
			return failed("Failed to write vault password file: " + err.Error())
		}
		if vault.VaultID != "" {
			args = append(args, "--vault-id", vault.VaultID+"@"+vaultFile)
		} else {
			unlabeledVaultFile = vaultFile
		}
	}

	// The become password travels in a 0600 extra-vars file when the job escalates.
	if arts.JobConfig != nil && arts.JobConfig.BecomeEnabled && arts.Credential != nil && arts.Credential.BecomePassword != "" {
		becomeVars, merr := json.Marshal(map[string]string{"ansible_become_pass": arts.Credential.BecomePassword})
		if merr == nil {
			becomeFile := secrets.Path("become_vars.json")
			if werr := os.WriteFile(becomeFile, becomeVars, 0o600); werr == nil {
				args = append(args, "-e", "@"+becomeFile)
			} else {
				logger.Warnf("Failed to write become password file: %v", werr)
			}
		}
	}
	if sshKeyPath != "" {
		args = append(args, "--private-key", sshKeyPath)
	}
	if arts.Credential != nil && arts.Credential.Username != "" {
		args = append(args, "-u", arts.Credential.Username)
	}
	args = append(args, playbookPath)

	// The play runs in the playbook's directory, so relative paths (roles/, group_vars/) resolve.
	ansibleWorkDir := filepath.Dir(playbookPath)

	env, msg := playEnv(jobCtx, arts, secrets.Dir(), unlabeledVaultFile, toolEnv)
	if msg != "" {
		return failed(msg)
	}
	if galaxyStaged {
		env = WithJobSearchPaths(env, ansibleWorkDir, galaxyCollections, galaxyRoles)
	}

	logger.Infof("Running: ansible-playbook %v (workdir: %s)", args, ansibleWorkDir)

	// The template's job timeout, when set, is the play's deadline; it covers the play only.
	runCtx, cancelRun := jobCtx, context.CancelFunc(func() {})
	if arts.JobConfig != nil && arts.JobConfig.TimeoutSeconds > 0 {
		runCtx, cancelRun = context.WithTimeout(jobCtx, time.Duration(arts.JobConfig.TimeoutSeconds)*time.Second)
	}
	defer cancelRun()

	cmd := exec.CommandContext(runCtx, "ansible-playbook", args...) //nolint:gosec // G204: arguments are built from the job's artifacts, not a shell string
	cmd.Dir = ansibleWorkDir
	cmd.Env = env
	var stderr bytes.Buffer
	stdoutW := &streamWriter{client: c, jobID: job.ID, stream: "stdout"}
	stderrW := &streamWriter{client: c, jobID: job.ID, stream: "stderr"}
	cmd.Stdout = stdoutW
	cmd.Stderr = io.MultiWriter(&stderr, stderrW)

	err = cmd.Run()
	stdoutW.flush(jobCtx)
	stderrW.flush(jobCtx)
	if stopped() {
		return Result{Status: "canceled"}
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return failed(fmt.Sprintf("job timeout exceeded (%ds): ansible-playbook was killed", arts.JobConfig.TimeoutSeconds))
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return failed(exitFailure(exitErr.ExitCode(), stderr.String()))
		}
		return failed(err.Error())
	}
	res := Result{Status: "completed"}
	if len(galaxyCache.Manifest) > 0 {
		res.Manifest = &runnercontract.CacheManifest{Entries: galaxyCache.Manifest}
	}
	return res
}

// playEnv is the environment ansible-playbook runs with. It starts from what ansible.InheritedEnv
// admits of the executor's environment, never the whole of it, because the play runs code its
// author controls; then the job's own variables: the mirror block, its credential's user, its
// cloud identity materialised into files, its vault and the structured-output callback. A cloud
// identity that cannot be materialised fails the job, because a play without it would run its
// cloud tasks as whatever identity the machine has. The second result is that failure's message.
func playEnv(ctx context.Context, arts *runnercontract.AnsibleArtifacts, secretDir, unlabeledVaultFile string, toolEnv []string) ([]string, string) {
	env := ansible.InheritedEnv()
	env = append(env, toolEnv...)
	if arts.AnsibleCfg == "" {
		env = append(env, "ANSIBLE_CONFIG="+filepath.Join(secretDir, "ansible.cfg"))
	}
	// The credential's user, so Ansible does not fall back to the process's user.
	if arts.Credential != nil && arts.Credential.Username != "" {
		env = append(env, "ANSIBLE_REMOTE_USER="+arts.Credential.Username)
	}

	// The per-run identities arrive as tokens; the files the cloud SDKs read are written into the
	// secret directory, which is shredded when the job ends.
	cloud := make(map[string]string, len(arts.EnvironmentVars))
	for k, v := range arts.EnvironmentVars {
		cloud[k] = v
	}
	if err := identity.MaterializeAWSWebIdentityToken(cloud, secretDir); err != nil {
		return nil, "Failed to write the AWS web identity token: " + err.Error()
	}
	if err := identity.MaterializeAzureFederatedToken(cloud, secretDir); err != nil {
		return nil, "Failed to write the Azure federated token: " + err.Error()
	}
	if err := identity.MaterializeGCPWorkloadIdentity(cloud, secretDir); err != nil {
		return nil, "Failed to write the GCP workload identity configuration: " + err.Error()
	}
	vaultCtx, cancelVault := context.WithTimeout(ctx, 30*time.Second)
	err := identity.MaterializeVaultToken(vaultCtx, cloud, secretDir)
	cancelVault()
	if err != nil {
		return nil, "Failed to log in to Vault with the job's identity: " + err.Error()
	}
	for k, v := range cloud {
		env = append(env, k+"="+v)
	}
	if unlabeledVaultFile != "" {
		env = append(env, "ANSIBLE_VAULT_PASSWORD_FILE="+unlabeledVaultFile)
	}
	// GCP service-account credentials are file-based: the key goes into the secret directory and
	// the variables are the shared helper's, the same the queue runner sets.
	if arts.GCPServiceAccount != "" {
		gcpEnv, err := ansible.GCPCredentialEnv(secretDir, arts.GCPServiceAccount)
		if err != nil {
			return nil, "Failed to write GCP credentials: " + err.Error()
		}
		for k, v := range gcpEnv {
			env = append(env, k+"="+v)
		}
	}
	// The JSONL callback gives structured events as they happen, which the API parses into host
	// facts, task details and stats. The executor brings it, so every image reports alike (#933, D6).
	callbackPlugins, err := writeCallback(secretDir)
	if err != nil {
		return nil, err.Error()
	}
	env = append(env,
		"ANSIBLE_CALLBACK_PLUGINS="+callbackPlugins,
		"ANSIBLE_STDOUT_CALLBACK="+CallbackName,
		"ANSIBLE_LOAD_CALLBACK_PLUGINS=true",
		"ANSIBLE_HOST_KEY_CHECKING="+HostKeyCheckingValue(),
		"ANSIBLE_SSH_ARGS="+SSHArgs(),
		"ANSIBLE_RETRY_FILES_ENABLED=false",
	)
	// Ansible writes under its home and temporary directories. The platform image points them
	// under the writable /tmp; an execution environment may leave them at its user's home on the
	// read-only root filesystem, so the job gets its own unless the image or operator set them.
	for name, sub := range map[string]string{"ANSIBLE_HOME": ".ansible", "ANSIBLE_REMOTE_TMP": ".ansible/tmp", "ANSIBLE_LOCAL_TEMP": ".ansible/local"} {
		if envLookup(env, name) == "" {
			env = append(env, name+"="+filepath.Join(secretDir, sub))
		}
	}
	return env, ""
}

// resolvePlaybook is the absolute path of the job's playbook inside repoDir, or a message saying why
// there is none.
func resolvePlaybook(arts *runnercontract.AnsibleArtifacts, repoDir string) (string, string) {
	playbookPath := arts.PlaybookPath
	if playbookPath == "" {
		playbookPath = "site.yml"
	}
	// Playbook paths are relative to the repository root. Azure DevOps lists files with a leading
	// "/", which filepath.IsAbs would take for an absolute system path.
	playbookPath = strings.TrimPrefix(playbookPath, "/")
	if !filepath.IsAbs(playbookPath) {
		playbookPath = filepath.Join(repoDir, playbookPath)
	}
	if _, err := os.Stat(playbookPath); os.IsNotExist(err) {
		if arts.PlaybookSource.HasSnapshot() {
			return "", fmt.Sprintf("Playbook file not found at %s - check that the playbook path is correct relative to the repository root", playbookPath)
		}
		return "", fmt.Sprintf("Playbook file not found at %s - the playbook has no snapshot: sync it, then launch the job again", playbookPath)
	}
	return playbookPath, ""
}

// jobConfigArgs are the ansible-playbook flags for a job's launch options.
func jobConfigArgs(cfg *runnercontract.AnsibleJobConfig) []string {
	if cfg == nil {
		return nil
	}
	var args []string
	if cfg.Limit != "" {
		args = append(args, "-l", cfg.Limit)
	}
	if cfg.Tags != "" {
		args = append(args, "-t", cfg.Tags)
	}
	if cfg.SkipTags != "" {
		args = append(args, "--skip-tags", cfg.SkipTags)
	}
	for range cfg.Verbosity {
		args = append(args, "-v")
	}
	if cfg.Forks > 0 {
		args = append(args, "-f", fmt.Sprintf("%d", cfg.Forks))
	}
	if cfg.BecomeEnabled {
		args = append(args, "-b")
	}
	if cfg.DiffMode {
		args = append(args, "-D")
	}
	return args
}

// logCredentialShape logs which parts of the job's inventory and credential are present, never
// their values, so an operator can tell a missing password from a wrong one.
func logCredentialShape(arts *runnercontract.AnsibleArtifacts) {
	if arts.InventoryContent != "" {
		logger.Infof("Inventory: %d bytes, has ansible_user=%v, has ansible_password=%v", len(arts.InventoryContent),
			strings.Contains(arts.InventoryContent, `"ansible_user"`), strings.Contains(arts.InventoryContent, `"ansible_password"`))
	}
	if cred := arts.Credential; cred != nil {
		logger.Infof("Credential: type=%s, username=%v, password=%v, ssh_key=%v", cred.Type, cred.Username != "", cred.Password != "", cred.SSHKey != "")
	} else {
		logger.Infof("Credential: none")
	}
	if len(arts.ExtraVars) > 0 {
		keys := make([]string, 0, len(arts.ExtraVars))
		for k := range arts.ExtraVars {
			keys = append(keys, k)
		}
		logger.Infof("Extra vars keys: %v (any ansible_* here overrides inventory)", keys)
	}
}

// galaxyServerListConfigured reports whether the ansible.cfg Ansible will read for the job, the
// first of candidates that exists, names a Galaxy server_list.
func galaxyServerListConfigured(candidates []string) bool {
	for _, candidate := range candidates {
		f, err := os.Open(candidate) //nolint:gosec // G304: candidates are ansible.cfg locations of the job
		if err != nil {
			continue
		}
		value := readIniValue(f, "galaxy", []string{"server_list"})
		_ = f.Close()
		return value != ""
	}
	return false
}

// extractFile unpacks a downloaded tarball into targetDir.
func extractFile(path, targetDir string) error {
	f, err := os.Open(path) //nolint:gosec // G304: path is inside the job's own work directory
	if err != nil {
		return fmt.Errorf("opening snapshot file: %w", err)
	}
	defer func() { _ = f.Close() }()
	return executor.ExtractTarGz(f, targetDir)
}

// streamWriter streams the play's output to the API one line at a time. A line is sent even after
// the job is stopped, so the output shows where the play was when it ended.
type streamWriter struct {
	client *executor.Client
	jobID  string
	stream string

	mu     sync.Mutex
	buffer bytes.Buffer
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buffer.Write(p)
	for {
		line, err := w.buffer.ReadString('\n')
		if err != nil {
			// No complete line yet: keep it for the next write.
			w.buffer.WriteString(line)
			break
		}
		w.send(context.Background(), line)
	}
	return len(p), nil
}

// flush sends a last line that has no newline.
func (w *streamWriter) flush(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buffer.Len() > 0 {
		w.send(ctx, w.buffer.String())
		w.buffer.Reset()
	}
}

func (w *streamWriter) send(ctx context.Context, line string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := w.client.Output(ctx, w.jobID, runnercontract.JobOutputRequest{Output: line, Stream: w.stream}); err != nil {
		logger.Debugf("Failed to stream output of job %s: %v", w.jobID, err)
	}
}

// exitFailure is the failure of an ansible-playbook that exited non-zero: its exit code, then what
// it wrote to stderr when it wrote anything.
func exitFailure(code int, stderr string) string {
	msg := fmt.Sprintf("Exit code %d", code)
	if s := strings.TrimSpace(stderr); s != "" {
		msg += ": " + s
	}
	return msg
}
