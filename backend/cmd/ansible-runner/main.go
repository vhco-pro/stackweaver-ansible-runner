// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// Command ansible-runner is the Ansible runner: a launcher that registers with its pool (or as a
// hosted launcher), holds one heartbeat and places an executor for each job and sync it claims
// (#933, D3), and the `executor` subcommand that runs one of them with a job token in a container
// or pod (D1, D2). It holds no database, object storage, Redis or platform secret.
package main

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/michielvha/logger"
	"github.com/michielvha/stackweaver/backend/internal/executor"
	"github.com/michielvha/stackweaver/backend/internal/executor/ansibleexec"
	"github.com/michielvha/stackweaver/backend/internal/executor/syncexec"
	"github.com/michielvha/stackweaver/backend/internal/launcher"
	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
	"github.com/michielvha/stackweaver/core/runnercontract"
	"github.com/michielvha/stackweaver/core/security/runenv"
)

func main() {
	// Started by git as a sync's askpass helper (#933, D2): answer one prompt and exit, before
	// anything else runs.
	if syncexec.IsAskpass() {
		os.Exit(syncexec.AskpassMain(os.Args[1:]))
	}
	logger.Init(os.Getenv("LOG_LEVEL"))

	// A job runs as this process's user; without this it could read the launcher's token from its
	// /proc environ (#936).
	if err := runenv.DenySameUserInspection(); err != nil {
		logger.Fatalf("Refusing to start: %v", err)
	}

	// Azure Workload Identity compatibility shim: the AKS webhook injects AZURE_TENANT_ID, and the
	// image-bundled azure.azcollection >= 3.20.0 reads it natively (ansible-collections/azure#2274)
	// - but a playbook repo's own galaxy requirements can pin an older collection, which is
	// installed per job and shadows the bundled one via ANSIBLE_COLLECTIONS_PATH. Alias the tenant
	// once at startup so those jobs keep working; an explicit AZURE_TENANT always wins.
	if os.Getenv("AZURE_TENANT") == "" {
		if tid := os.Getenv("AZURE_TENANT_ID"); tid != "" {
			_ = os.Setenv("AZURE_TENANT", tid)
			logger.Info("Bridged AZURE_TENANT_ID -> AZURE_TENANT for Azure Workload Identity")
		}
	}

	// `ansible-runner executor` runs exactly one job with a job token (#933, D1): what a launcher's
	// container and Kubernetes drivers start.
	if len(os.Args) > 1 && os.Args[1] == "executor" {
		os.Exit(runExecutor())
	}
	// `<binary> healthcheck` is the container healthcheck: the launcher's /readyz, with nothing
	// but this binary in the image (AC75).
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(launcher.Healthcheck(context.Background(), os.Getenv))
	}

	self, err := os.Executable()
	if err != nil {
		logger.Fatalf("Locating this binary: %v", err)
	}
	os.Exit(launcher.Main(launcher.Binary{
		Engine:         "ansible",
		Executor:       runInProcess,
		ExecutorPath:   self,
		Version:        runnerVersion(),
		AnsibleVersion: ansibleVersion(),
	}))
}

// runnerVersion is the release this binary reports at registration.
func runnerVersion() string {
	if v := os.Getenv("STACKWEAVER_VERSION"); v != "" {
		return v
	}
	return "dev"
}

// ansibleVersion is the first line of `ansible --version`, which the API parses.
func ansibleVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ansible", "--version").Output()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(line)
}

// runExecutor runs the `executor` subcommand: one job, from the invocation a driver set.
func runExecutor() int {
	inv, err := executor.InvocationFromEnv()
	if err != nil {
		// Without an invocation there is no job to report on: the launcher reports it.
		logger.Errorf("executor: %v", err)
		return executor.ExitUnreported
	}
	// SIGTERM from a driver stopping the placement cancels the job, which the executor reports.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return execute(ctx, driver.Invocation{ServerURL: inv.ServerURL, JobID: inv.JobID, Token: inv.Token, RunType: inv.RunType, RunnerID: inv.RunnerID, CacheDir: inv.CacheDir})
}

// runInProcess is the in-process driver's executor: the same job, in the launcher's process.
func runInProcess(ctx context.Context, inv driver.Invocation) int { return execute(ctx, inv) }

// execute runs one job: a sync work item in the sync executor, an Ansible job in the Ansible
// executor.
func execute(ctx context.Context, inv driver.Invocation) int {
	client := executor.NewClient(inv.ServerURL, inv.Token, inv.RunnerID)
	client.SetPlacement(inv.Placement)
	client.SetLimitsReport(inv.LimitsReport)
	if !client.Preflight(ctx, inv.JobID, "stderr") {
		return client.ExitCode("failed")
	}
	if strings.HasPrefix(inv.JobID, runnercontract.WorkItemPrefix) {
		return client.ExitCode(syncexec.Execute(ctx, client, inv.JobID, syncexec.Options{}).Status)
	}
	res := ansibleexec.Execute(ctx, client, ansibleexec.Job{ID: inv.JobID}, ansibleexec.Options{CacheDir: inv.CacheDir})
	return client.ExitCode(res.Status)
}
