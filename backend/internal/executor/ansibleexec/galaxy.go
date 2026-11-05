// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansibleexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/michielvha/logger"
	"github.com/michielvha/stackweaver/backend/internal/executor"
	"github.com/michielvha/stackweaver/core/runnercontract"
	"github.com/michielvha/stackweaver/core/services/ansible"
)

// GalaxyInstall locates one job's Galaxy install.
type GalaxyInstall struct {
	// JobDir is the job's private workspace; the install is staged under it.
	JobDir string
	// PlaybookDir is the root of the prepared playbook repository.
	PlaybookDir string
	// Env is extra environment for the ansible-galaxy commands, added to what
	// ansible.InheritedEnv admits of the runner's environment. The executor uses it to point
	// ansible-galaxy at the job's ansible.cfg, which is where a private Galaxy server is
	// configured, and at the deployment's mirror block.
	Env []string
	// Cache is the job's organization's verified cache; nil for none.
	Cache *GalaxyCache
	// ResolveRef resolves a git source's branch or tag to the commit it names now, so the source
	// is cached and reused by that commit (#933, D7). Nil runs git ls-remote.
	ResolveRef func(ctx context.Context, repo, ref string) (string, error)
}

// GalaxyCache is a job's use of its organization's cache (#933, D7): the collections, roles and
// git sources its descriptor names, read verified, and what the install added beyond them.
type GalaxyCache struct {
	Reader  *executor.Cache
	Entries []runnercontract.CacheEntryRef
	// Manifest receives what the install put in place that the cache did not supply, which the
	// job reports with its completion for the launcher's populator to verify and promote.
	Manifest []runnercontract.CacheEntry
}

// galaxyInstallTimeout bounds the Galaxy install of one job, on both runner paths. A
// requirement that never resolves (a git source on a host that drops packets, a hub
// that stalls) would otherwise hold a job, and on a self-hosted agent its only slot,
// indefinitely. A variable so tests can shorten it.
var galaxyInstallTimeout = 15 * time.Minute

// galaxyKillGrace is how long a stopped ansible-galaxy gets to release its output pipes.
// Its children (git, for one) inherit them, and without a bound the wait for the output
// would last as long as the slowest child.
const galaxyKillGrace = 5 * time.Second

// galaxyStopReason explains, for the job output, why ctx ended an install early.
func galaxyStopReason(ctx context.Context) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Sprintf("ansible-galaxy did not finish within %s and was stopped.\n", galaxyInstallTimeout)
	case ctx.Err() != nil:
		return "ansible-galaxy was stopped because the job was cancelled.\n"
	default:
		return ""
	}
}

// galaxyCommand builds one ansible-galaxy invocation for the install.
func (in GalaxyInstall) galaxyCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "ansible-galaxy", args...) //nolint:gosec // intentional: executing ansible command
	cmd.Dir = in.PlaybookDir
	cmd.WaitDelay = galaxyKillGrace
	// Always set: a nil Env would hand ansible-galaxy, and the requirements it resolves, the
	// runner's whole environment.
	cmd.Env = append(ansible.InheritedEnv(), in.Env...)
	return cmd
}

// findGalaxyRequirements returns the requirements file a playbook repository carries,
// looking in the common locations in order, or "" when it has none.
func findGalaxyRequirements(playbookDir string) string {
	for _, path := range []string{
		filepath.Join(playbookDir, "requirements.yml"),
		filepath.Join(playbookDir, "collections", "requirements.yml"),
		filepath.Join(playbookDir, "roles", "requirements.yml"),
	} {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

// InstallGalaxyRequirements checks for requirements.yml in the playbook directory
// and installs any Galaxy collections/roles defined there.
//
// Collections and roles are staged inside the per-job workspace, which is private to the job
// and works under a read-only root filesystem. What the job's organization's verified cache holds
// is installed from it first, verified, and the requirements then install the rest from their
// sources; nothing the job installs is written back anywhere (#933, D7: only the launcher's
// populator promotes an entry, and only once it verified it against the source).
//
// It returns the staged collections and roles directories when the repository's
// requirements installed, and empty strings otherwise: a repository without a
// requirements file stages nothing (and that is not an error), and a failed install
// returns the error. A non-empty collections directory is therefore how a caller knows
// there is a staged install to put on the job's search paths.
func InstallGalaxyRequirements(ctx context.Context, sink EventSink, in GalaxyInstall) (collectionsDir, rolesDir string, err error) {
	foundPath := findGalaxyRequirements(in.PlaybookDir)
	if foundPath == "" {
		return "", "", nil
	}

	logger.Infof("Found Galaxy requirements at: %s", foundPath)

	stageCollections := filepath.Join(in.JobDir, "galaxy", "collections")
	stageRoles := filepath.Join(in.JobDir, "galaxy", "roles")
	for _, dir := range []string{stageCollections, stageRoles} {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil { //nolint:gosec // workspace directories need 0o755 for compatibility
			return "", "", fmt.Errorf("creating the Galaxy staging dir: %w", mkErr)
		}
	}

	// Events are recorded on a context that outlives the install: the one that says the
	// install was stopped has to be written after the context that stopped it has ended.
	eventCtx := context.WithoutCancel(ctx)
	ctx, cancel := context.WithTimeout(ctx, galaxyInstallTimeout)
	defer cancel()

	reqs, _ := readRequirements(foundPath)
	commits := in.gitCommits(ctx, reqs)
	seeded := in.seedFromCache(ctx, foundPath, reqs, commits, stageCollections, stageRoles)
	if summary := seeded.summary(); summary != "" {
		EmitEvent(eventCtx, sink, Event{
			Event:  "galaxy_cache_seeded",
			Task:   "Galaxy cache reused",
			Stdout: "Installed " + summary + " from the organization's verified cache\n",
		})
	}

	EmitEvent(eventCtx, sink, Event{
		Event:  "galaxy_install",
		Task:   "Installing Galaxy Requirements",
		Stdout: fmt.Sprintf("Installing collections/roles from %s\n", filepath.Base(foundPath)),
	})

	requirements := foundPath
	if seeded.rewritten != "" {
		requirements = seeded.rewritten
	}
	// Run ansible-galaxy collection install into the staged directory.
	output, err := in.galaxyCommand(ctx, "collection", "install", "-r", requirements, "-p", stageCollections).CombinedOutput()
	if err != nil {
		EmitEvent(eventCtx, sink, Event{
			Event:  "galaxy_install_failed",
			Task:   "Galaxy Installation Failed",
			Stderr: string(output) + galaxyStopReason(ctx),
			Failed: true,
		})
		return "", "", fmt.Errorf("running ansible-galaxy collection install: %w: %s", err, string(output))
	}

	// Also install roles if the requirements file contains roles. A failure here is
	// ignored: its error only decides whether the output joins the success event.
	rolesOutput, rolesErr := in.galaxyCommand(ctx, "role", "install", "-r", requirements, "-p", stageRoles).CombinedOutput()

	resultStdout := string(output)
	if rolesErr == nil && len(rolesOutput) > 0 {
		resultStdout += "\n" + string(rolesOutput)
	}

	EmitEvent(eventCtx, sink, Event{
		Event:  "galaxy_install_complete",
		Task:   "Galaxy Requirements Installed",
		Stdout: resultStdout,
	})

	if in.Cache != nil {
		in.Cache.Manifest = append(in.Cache.Manifest, installedBeyond(stageCollections, stageRoles, reqs, commits, seeded)...)
	}
	logger.Infof("Galaxy requirements installed successfully")
	return stageCollections, stageRoles, nil
}
