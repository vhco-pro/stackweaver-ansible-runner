// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// Package syncexec is the sync executor (#933, D2, decision 18): it runs one work item, a sync of
// the platform's control plane, in the platform image with the platform identity for the
// project-update class and no user code. It is the only executor that clones from a VCS host: a
// playbook or configuration snapshot is captured here and every job starts from it. A token or an
// SSH deploy key is held in memory and handed to git without an argument, an environment variable
// or a file of the work directory.
package syncexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/michielvha/logger"
	"github.com/michielvha/stackweaver/backend/internal/executor"
	"github.com/michielvha/stackweaver/core/models"
	"github.com/michielvha/stackweaver/core/runnercontract"
	"github.com/michielvha/stackweaver/core/services/ansible"
	"gopkg.in/yaml.v3"
)

// Options tune an execution; the zero value is what production uses.
type Options struct {
	// WorkRoot is where the work and secret directories are made; empty means the temp directory.
	WorkRoot string
	// StatusInterval is how often the work item's status is polled for a stop; zero means two
	// seconds.
	StatusInterval time.Duration
	// Askpass is the binary git runs as its askpass helper; empty means this executable.
	Askpass string
}

// Result is the outcome of a work item.
type Result struct {
	Status       string
	ErrorMessage string
}

// Execute runs one work item end to end and reports its completion.
func Execute(ctx context.Context, c *executor.Client, jobID string, opts Options) Result {
	var arts runnercontract.WorkItemArtifacts
	var (
		res    Result
		result *runnercontract.WorkResult
	)
	if err := c.Artifacts(ctx, jobID, &arts, func() int { return arts.ContractVersion }); err != nil {
		if errors.Is(err, runnercontract.ErrUnknownVersion) {
			c.ReportRefusal(ctx, jobID, "stderr", err)
		}
		res = Result{Status: "failed", ErrorMessage: "Failed to download the work item's artifacts: " + err.Error()}
		var se *executor.StatusError
		if errors.As(err, &se) && (se.Code == http.StatusConflict || se.Stops()) {
			// The API already ended the work item with the reason.
			return res
		}
	} else {
		res, result = run(ctx, c, jobID, &arts, opts)
	}
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := c.Complete(reportCtx, jobID, runnercontract.JobCompleteRequest{Status: res.Status, ErrorMessage: res.ErrorMessage, WorkResult: result}); err != nil {
		logger.Warnf("Failed to report the completion of work item %s: %v", jobID, err)
	}
	return res
}

// run executes the work item's kind and returns its outcome and result.
func run(ctx context.Context, c *executor.Client, jobID string, arts *runnercontract.WorkItemArtifacts, opts Options) (Result, *runnercontract.WorkResult) {
	result := &runnercontract.WorkResult{}
	failed := func(err error) (Result, *runnercontract.WorkResult) {
		return Result{Status: "failed", ErrorMessage: err.Error()}, result
	}
	root := opts.WorkRoot
	if root == "" {
		root = os.TempDir()
	}
	workDir, err := os.MkdirTemp(root, "sync-")
	if err != nil {
		return failed(fmt.Errorf("creating the work directory: %w", err))
	}
	defer func() { _ = os.RemoveAll(workDir) }()
	secrets, err := executor.NewSecretDir(root)
	if err != nil {
		return failed(fmt.Errorf("creating the secret directory: %w", err))
	}
	defer func() { _ = secrets.Shred() }()

	timeout := time.Duration(arts.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = time.Hour
	}
	timed, cancelTimeout := context.WithTimeout(ctx, timeout)
	defer cancelTimeout()
	jobCtx, stop := context.WithCancelCause(timed)
	defer stop(nil)
	interval := opts.StatusInterval
	if interval == 0 {
		interval = 2 * time.Second
	}
	go c.WatchCancellation(jobCtx, jobID, interval, stop)

	out := &outputStream{ctx: jobCtx, client: c, jobID: jobID, last: time.Now()}
	defer out.Close(ctx)

	mirror, err := executor.MirrorEnv(arts.Mirror, secrets.Dir())
	if err != nil {
		return failed(fmt.Errorf("applying the deployment's mirror settings: %w", err))
	}
	// What the image's environment admits (never the launcher's secrets), Ansible's home in the work
	// directory so it runs under a read-only root, and the mirror block.
	env := append(ansible.InheritedEnv(), "HOME="+workDir, "ANSIBLE_HOME="+filepath.Join(workDir, ".ansible"))
	for k, v := range mirror {
		env = append(env, k+"="+v)
	}
	askpass := opts.Askpass
	if askpass == "" {
		if askpass, err = os.Executable(); err != nil {
			return failed(fmt.Errorf("locating the askpass helper: %w", err))
		}
	}
	cl := &cloner{askpass: askpass, ssh: "ssh", env: env, secretDir: secrets.Dir()}

	err = runKind(jobCtx, c, jobID, arts, cl, workDir, secrets, env, out, result)
	if cause := context.Cause(jobCtx); errors.Is(cause, executor.ErrJobStopped) || ctx.Err() != nil {
		return Result{Status: "canceled", ErrorMessage: "the work item was stopped"}, result
	}
	if timed.Err() != nil {
		return Result{Status: "failed", ErrorMessage: fmt.Sprintf("the work item exceeded its time limit of %s", timeout)}, result
	}
	if err != nil {
		_, _ = fmt.Fprintf(out, "\n%v\n", err)
		return failed(err)
	}
	return Result{Status: "completed"}, result
}

// runKind runs the step of the work item's kind.
func runKind(ctx context.Context, c *executor.Client, jobID string, arts *runnercontract.WorkItemArtifacts, cl *cloner, workDir string, secrets *executor.SecretDir, env []string, out io.Writer, result *runnercontract.WorkResult) error {
	repo := filepath.Join(workDir, "repo")
	cloneInto := func() error {
		_, _ = fmt.Fprintf(out, "Cloning %s (branch %s)\n", arts.Clone.URL, branchOf(arts.Clone))
		commit, err := cl.clone(ctx, arts.Clone, repo, out)
		if err != nil {
			return err
		}
		result.Commit = commit
		return nil
	}
	switch arts.Kind {
	case models.WorkItemPlaybookSnapshot:
		if arts.Clone == nil {
			return errors.New("the work item names no repository")
		}
		if err := cloneInto(); err != nil {
			return err
		}
		playbook, err := within(repo, strings.TrimPrefix(arts.PlaybookPath, "/"))
		if err != nil {
			return err
		}
		if _, err := os.Stat(playbook); err != nil {
			return fmt.Errorf("playbook file not found at path: %s", arts.PlaybookPath)
		}
		data, whole, err := BuildPlaybookSnapshot(repo, arts.PlaybookPath)
		if err != nil {
			return fmt.Errorf("building the playbook snapshot: %w", err)
		}
		if whole {
			_, _ = fmt.Fprintln(out, "The playbook's dependencies could not be scoped statically: the snapshot holds the whole repository.")
		}
		_, _ = fmt.Fprintf(out, "Captured %s at %s (%d bytes)\n", arts.PlaybookPath, result.Commit, len(data))
		return c.UploadSnapshot(ctx, jobID, result.Commit, data)
	case models.WorkItemConfigurationSnapshot:
		// The run's configuration: the repository at the branch head, without its .git, as a push
		// captures it; the run's working directory is applied by the OpenTofu executor.
		if arts.Clone == nil {
			return errors.New("the work item names no repository")
		}
		if err := cloneInto(); err != nil {
			return err
		}
		data, err := tarGzDir(repo, nil)
		if err != nil {
			return fmt.Errorf("archiving the configuration: %w", err)
		}
		_, _ = fmt.Fprintf(out, "Captured the configuration at %s (%d bytes)\n", result.Commit, len(data))
		return c.UploadSnapshot(ctx, jobID, result.Commit, data)
	case models.WorkItemInventoryFileSync:
		if arts.Clone == nil {
			return errors.New("the work item names no repository")
		}
		if err := cloneInto(); err != nil {
			return err
		}
		file, err := within(repo, arts.InventoryPath)
		if err != nil {
			return err
		}
		if _, err := os.Stat(file); err != nil {
			return fmt.Errorf("inventory file not found at path: %s", arts.InventoryPath)
		}
		// Azure Workload Identity reads the subscription from the environment: the inventory file's
		// own, else the organization's.
		var credentials []string
		if sub := azureSubscription(file, arts.AzureSubscriptionID); sub != "" {
			credentials = append(credentials, "AZURE_SUBSCRIPTION_ID="+sub)
		}
		_, _ = fmt.Fprintf(out, "Parsing %s @ %s\n$ ansible-inventory -i %s --list\n\n", arts.InventoryPath, result.Commit, arts.InventoryPath)
		return inventory(ctx, []string{"-i", file, "--list"}, ansible.InventoryCommandEnv(env, credentials), out, result)
	case models.WorkItemConstructedRebuild, models.WorkItemDynamicSourceSync:
		if arts.Inventory == nil {
			return errors.New("the work item carries no inventory command")
		}
		args, credentials, err := materialize(arts.Inventory, secrets)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "$ ansible-inventory %s\n\n", strings.ReplaceAll(strings.Join(args, " "), secrets.Dir(), "<secrets>"))
		return inventory(ctx, args, ansible.InventoryCommandEnv(env, credentials), out, result)
	}
	return fmt.Errorf("work item kind %q is not run by this executor", arts.Kind)
}

// inventory runs ansible-inventory and records its output and diagnostics in result.
func inventory(ctx context.Context, args, env []string, out io.Writer, result *runnercontract.WorkResult) error {
	parsed, stderr, err := ansible.RunAnsibleInventory(ctx, args, env, out)
	result.Stderr = stderr
	if err != nil {
		return err
	}
	raw, err := json.Marshal(parsed)
	if err != nil {
		return fmt.Errorf("encoding the inventory: %w", err)
	}
	result.Inventory = raw
	return nil
}

// materialize writes a command's files into the secret directory and puts its path in place of
// the placeholder in the arguments and environment.
func materialize(cmd *runnercontract.InventoryCommand, secrets *executor.SecretDir) (args, env []string, err error) {
	for name, content := range cmd.Files {
		path, err := within(secrets.Dir(), name)
		if err != nil {
			return nil, nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, nil, fmt.Errorf("creating the directory of %s: %w", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return nil, nil, fmt.Errorf("writing %s: %w", name, err)
		}
	}
	expand := func(s string) string {
		return strings.ReplaceAll(s, runnercontract.SecretDirPlaceholder, secrets.Dir())
	}
	for _, a := range cmd.Args {
		args = append(args, expand(a))
	}
	for _, e := range cmd.Env {
		env = append(env, expand(e))
	}
	return args, env, nil
}

// within joins rel onto dir and refuses a path that leaves it.
func within(dir, rel string) (string, error) {
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if r, err := filepath.Rel(dir, path); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing the path %q, which leaves its directory", rel)
	}
	return path, nil
}

func branchOf(src *runnercontract.CloneSource) string {
	if src.Branch == "" {
		return "main"
	}
	return src.Branch
}

// azureSubscription is the subscription_id an azure_rm inventory file names, else fallback.
func azureSubscription(path, fallback string) string {
	data, err := os.ReadFile(path) //nolint:gosec // G304: a file of this work item's own clone
	if err == nil {
		var doc map[string]any
		if yaml.Unmarshal(data, &doc) == nil {
			if v, ok := doc["subscription_id"].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	return strings.TrimSpace(fallback)
}

// outputStream sends a work item's output to the API in order, at most once a second or every
// 4 KiB, and the rest when closed.
type outputStream struct {
	ctx    context.Context //nolint:containedctx // the stream lives exactly as long as the work item
	client *executor.Client
	jobID  string

	mu   sync.Mutex
	buf  bytes.Buffer
	last time.Time
}

func (o *outputStream) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.buf.Write(p)
	if time.Since(o.last) > time.Second || o.buf.Len() > 4096 {
		o.flushLocked(o.ctx)
	}
	return len(p), nil
}

func (o *outputStream) flushLocked(ctx context.Context) {
	if o.buf.Len() == 0 {
		return
	}
	if err := o.client.Output(ctx, o.jobID, runnercontract.JobOutputRequest{Output: o.buf.String(), Stream: "stdout"}); err != nil {
		logger.Debugf("sending work item output: %v", err)
	}
	o.buf.Reset()
	o.last = time.Now()
}

// Close sends what is buffered, even after the work item's context ended.
func (o *outputStream) Close(ctx context.Context) {
	o.mu.Lock()
	defer o.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	o.flushLocked(ctx)
}
