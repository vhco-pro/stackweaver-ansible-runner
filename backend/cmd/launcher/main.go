// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// Command launcher is stackweaver-launcher: a launcher that registers with an agent pool (or as a
// hosted launcher), holds one heartbeat and places an executor for each OpenTofu run, Ansible job
// and sync it claims (#933, D3; one-launcher spec, A1), and the `executor` subcommand that runs one
// of them with a job token in a container or pod (D1, D2). It holds no database, object storage,
// Redis or platform secret.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/michielvha/logger"
	"github.com/michielvha/stackweaver/backend/internal/executor"
	"github.com/michielvha/stackweaver/backend/internal/executor/ansibleexec"
	"github.com/michielvha/stackweaver/backend/internal/executor/syncexec"
	"github.com/michielvha/stackweaver/backend/internal/executor/tofuexec"
	"github.com/michielvha/stackweaver/backend/internal/launcher"
	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
	"github.com/michielvha/stackweaver/core/security/runenv"
)

// version is the release this binary reports at registration, stamped at build time with
// -ldflags "-X main.version=<release>".
var version string

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
	bridgeAzureTenant(os.Getenv, os.Setenv)

	// `stackweaver-launcher executor` runs exactly one job with a job token (#933, D1): what a
	// launcher's container and Kubernetes drivers start.
	if len(os.Args) > 1 && os.Args[1] == "executor" {
		os.Exit(runExecutor())
	}
	// `stackweaver-launcher healthcheck` is the container healthcheck: the launcher's /readyz,
	// with nothing but this binary in the image (AC75).
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(launcher.Healthcheck(context.Background(), os.Getenv))
	}

	os.Exit(launcher.Main(launcher.Binary{
		Executors: map[string]driver.ExecutorFunc{launcher.EngineTofu: execute, launcher.EngineAnsible: execute},
		Version:   launcherVersion(),
	}))
}

// launcherVersion is the release this binary reports at registration: the stamped version, else
// STACKWEAVER_VERSION, else "dev" for a local build.
func launcherVersion() string {
	if version != "" {
		return version
	}
	if v := os.Getenv("STACKWEAVER_VERSION"); v != "" {
		return v
	}
	return "dev"
}

// bridgeAzureTenant is the Azure Workload Identity compatibility shim: the AKS webhook injects
// AZURE_TENANT_ID, and the image-bundled azure.azcollection >= 3.20.0 reads it natively
// (ansible-collections/azure#2274), but a playbook repo's own galaxy requirements can pin an older
// collection, which is installed per job and shadows the bundled one via ANSIBLE_COLLECTIONS_PATH.
// It aliases the tenant once at startup so those jobs keep working; an explicit AZURE_TENANT
// always wins, and without AZURE_TENANT_ID it does nothing.
func bridgeAzureTenant(getenv func(string) string, setenv func(string, string) error) {
	if getenv("AZURE_TENANT") != "" {
		return
	}
	if tid := getenv("AZURE_TENANT_ID"); tid != "" {
		_ = setenv("AZURE_TENANT", tid)
		logger.Info("Bridged AZURE_TENANT_ID -> AZURE_TENANT for Azure Workload Identity")
	}
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

// jobExecutor runs one job of its kind and returns the status it reported.
type jobExecutor func(ctx context.Context, client *executor.Client, inv driver.Invocation) string

// executors runs each kind of job: a sync work item in the sync executor, an OpenTofu run in the
// OpenTofu executor, an Ansible job in the Ansible executor.
var executors = map[executor.Kind]jobExecutor{
	executor.KindSync: func(ctx context.Context, client *executor.Client, inv driver.Invocation) string {
		return syncexec.Execute(ctx, client, inv.JobID, syncexec.Options{}).Status
	},
	executor.KindTofu: func(ctx context.Context, client *executor.Client, inv driver.Invocation) string {
		return tofuexec.Execute(ctx, client, tofuexec.Job{ID: inv.JobID, RunType: inv.RunType}, tofuexec.Options{CacheDir: inv.CacheDir}).Status
	},
	executor.KindAnsible: func(ctx context.Context, client *executor.Client, inv driver.Invocation) string {
		return ansibleexec.Execute(ctx, client, ansibleexec.Job{ID: inv.JobID}, ansibleexec.Options{CacheDir: inv.CacheDir}).Status
	},
}

// execute runs one job, in this process (the in-process driver) or as the `executor` subcommand,
// in the executor its invocation names, after the preflight on that job's stream.
func execute(ctx context.Context, inv driver.Invocation) int {
	client := executor.NewClient(inv.ServerURL, inv.Token, inv.RunnerID)
	client.SetPlacement(inv.Placement)
	client.SetLimitsReport(inv.LimitsReport)
	if !client.Preflight(ctx, inv.JobID, executor.PreflightStream(inv.JobID, inv.RunType)) {
		return client.ExitCode("failed")
	}
	return client.ExitCode(executors[executor.KindOf(inv.JobID, inv.RunType)](ctx, client, inv))
}
