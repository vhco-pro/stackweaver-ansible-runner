// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/michielvha/logger"
	"github.com/michielvha/stackweaver/core/services/ansible"
)

// Playbook preparation shared by the two execution paths of this binary: the platform
// queue runner (main.go) and the self-hosted agent (agent_mode.go). Everything here is
// a free function parameterised on the one thing the paths do differently, which is how
// a job event reaches the job's output: the queue runner writes the row itself, the
// agent posts it to the control-plane API.

// jobEvent is one non-callback row in a job's output, such as the Galaxy install
// progress or the playbook source banner.
type jobEvent struct {
	Event  string
	Task   string
	Stdout string
	Stderr string
	Failed bool
}

// jobEventSink records a job event for the job being prepared.
type jobEventSink interface {
	Emit(ctx context.Context, ev jobEvent) error
}

// emitJobEvent records ev and logs a failure instead of returning it: an event that
// cannot be stored must never fail the job it describes.
func emitJobEvent(ctx context.Context, sink jobEventSink, ev jobEvent) {
	if err := sink.Emit(ctx, ev); err != nil {
		logger.Warnf("Failed to record %s job event: %v", ev.Event, err)
	}
}

// galaxyInstallInput locates one job's Galaxy install.
type galaxyInstallInput struct {
	// cacheBase is the warm cache for the job's project. Namespacing by project keeps
	// one project's collections from being served to another.
	cacheBase string
	// jobDir is the job's private workspace; the install is staged under it.
	jobDir string
	// playbookDir is the root of the prepared playbook repository.
	playbookDir string
	// env is extra environment for the ansible-galaxy commands, added to what
	// ansible.InheritedEnv admits of the runner's environment. The agent uses it to point
	// ansible-galaxy at the job's ansible.cfg, which is where a private Galaxy server is
	// configured.
	env []string
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
func (in galaxyInstallInput) galaxyCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "ansible-galaxy", args...) //nolint:gosec // intentional: executing ansible command
	cmd.Dir = in.playbookDir
	cmd.WaitDelay = galaxyKillGrace
	// Always set: a nil Env would hand ansible-galaxy, and the requirements it resolves, the
	// runner's whole environment.
	cmd.Env = append(ansible.InheritedEnv(), in.env...)
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

// installGalaxyRequirements checks for requirements.yml in the playbook directory
// and installs any Galaxy collections/roles defined there.
//
// Collections/roles are staged inside the per-job workspace (seeded from a warm,
// per-project cache) so the run is isolated from concurrent cache writes and works
// under a read-only root filesystem. After a successful install the staged result is
// published back to the per-project cache via a temp-dir + atomic rename so future
// jobs can reuse it.
//
// It returns the staged collections and roles directories when the repository's
// requirements installed, and empty strings otherwise: a repository without a
// requirements file stages nothing (and that is not an error), and a failed install
// returns the error. A non-empty collections directory is therefore how a caller knows
// there is a staged install to put on the job's search paths.
func installGalaxyRequirements(ctx context.Context, sink jobEventSink, in galaxyInstallInput) (collectionsDir, rolesDir string, err error) {
	foundPath := findGalaxyRequirements(in.playbookDir)
	if foundPath == "" {
		return "", "", nil
	}

	logger.Infof("Found Galaxy requirements at: %s", foundPath)

	// The warm per-project cache this install is seeded from and published back to.
	canonicalCollections := filepath.Join(in.cacheBase, "collections")
	canonicalRoles := filepath.Join(in.cacheBase, "roles")

	// Stage the install inside the per-job workspace, seeded from the warm cache.
	// The job runs against this isolated copy, so a concurrent cache publish can
	// never pull collections out from under it.
	stageCollections := filepath.Join(in.jobDir, "galaxy", "collections")
	stageRoles := filepath.Join(in.jobDir, "galaxy", "roles")
	if mkErr := os.MkdirAll(filepath.Join(in.jobDir, "galaxy"), 0o755); mkErr != nil { //nolint:gosec // workspace directories need 0o755 for compatibility
		return "", "", fmt.Errorf("creating galaxy staging dir: %w", mkErr)
	}
	var seededKinds []string
	if seedGalaxyStage("collections", canonicalCollections, stageCollections) {
		seededKinds = append(seededKinds, "collections")
	}
	if seedGalaxyStage("roles", canonicalRoles, stageRoles) {
		seededKinds = append(seededKinds, "roles")
	}
	if mkErr := os.MkdirAll(stageCollections, 0o755); mkErr != nil { //nolint:gosec // workspace directories need 0o755 for compatibility
		return "", "", fmt.Errorf("creating collections staging dir: %w", mkErr)
	}
	if mkErr := os.MkdirAll(stageRoles, 0o755); mkErr != nil { //nolint:gosec // workspace directories need 0o755 for compatibility
		return "", "", fmt.Errorf("creating roles staging dir: %w", mkErr)
	}

	// Events are recorded on a context that outlives the install: the one that says the
	// install was stopped has to be written after the context that stopped it has ended.
	eventCtx := context.WithoutCancel(ctx)
	ctx, cancel := context.WithTimeout(ctx, galaxyInstallTimeout)
	defer cancel()

	// Say when the staging directory came from the project's warm cache. It is its own
	// event, ahead of the install: ansible-galaxy's transcript cannot show cache reuse
	// (it reinstalls a git-sourced collection every time), and the three install events
	// keep their texts.
	if len(seededKinds) > 0 {
		emitJobEvent(eventCtx, sink, jobEvent{
			Event:  "galaxy_cache_seeded",
			Task:   "Galaxy cache reused",
			Stdout: fmt.Sprintf("Seeded the Galaxy staging directory from the project's warm cache (%s)\n", strings.Join(seededKinds, ", ")),
		})
	}

	emitJobEvent(eventCtx, sink, jobEvent{
		Event:  "galaxy_install",
		Task:   "Installing Galaxy Requirements",
		Stdout: fmt.Sprintf("Installing collections/roles from %s\n", filepath.Base(foundPath)),
	})

	// Run ansible-galaxy collection install into the staged directory.
	output, err := in.galaxyCommand(ctx, "collection", "install", "-r", foundPath, "-p", stageCollections).CombinedOutput()
	if err != nil {
		emitJobEvent(eventCtx, sink, jobEvent{
			Event:  "galaxy_install_failed",
			Task:   "Galaxy Installation Failed",
			Stderr: string(output) + galaxyStopReason(ctx),
			Failed: true,
		})
		return "", "", fmt.Errorf("running ansible-galaxy collection install: %w: %s", err, string(output))
	}

	// Also install roles if the requirements file contains roles. A failure here is
	// ignored: its error only decides whether the output joins the success event.
	rolesOutput, rolesErr := in.galaxyCommand(ctx, "role", "install", "-r", foundPath, "-p", stageRoles).CombinedOutput()

	resultStdout := string(output)
	if rolesErr == nil && len(rolesOutput) > 0 {
		resultStdout += "\n" + string(rolesOutput)
	}

	emitJobEvent(eventCtx, sink, jobEvent{
		Event:  "galaxy_install_complete",
		Task:   "Galaxy Requirements Installed",
		Stdout: resultStdout,
	})

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

// seedGalaxyStage seeds one staging directory from the warm cache. A seed that fails
// part way (a cache entry that cannot be read, a cache evicted mid-copy) leaves a partial
// tree behind; installing on top of that and publishing it would poison the project's
// cache for every later job. So a failed seed is discarded and the install starts cold.
//
// It reports whether the staging directory now holds content from the cache.
func seedGalaxyStage(kind, cache, stage string) bool {
	seeded, seedErr := seedGalaxyCache(cache, stage)
	if seedErr == nil {
		return seeded
	}
	logger.Warnf("Failed to seed the %s cache, installing from scratch: %v", kind, seedErr)
	if rmErr := os.RemoveAll(stage); rmErr != nil {
		logger.Warnf("Failed to discard the partially seeded %s staging dir %s: %v", kind, stage, rmErr)
	}
	return false
}

// seedGalaxyCache copies a warm cache directory into a fresh staging directory so
// ansible-galaxy can reuse already-downloaded collections. It is a no-op when the
// source cache does not yet exist. The destination must not already exist.
//
// It reports whether anything was copied: a cache that does not exist, or one a previous
// job published empty (a requirements file with collections but no roles, say), seeds
// nothing.
func seedGalaxyCache(src, dst string) (seeded bool, err error) {
	entries, readErr := os.ReadDir(src)
	if readErr != nil || len(entries) == 0 {
		return false, nil //nolint:nilerr // no warm cache to seed from is not an error
	}
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		return false, fmt.Errorf("seeding %s from %s: %w", dst, src, err)
	}
	return true, nil
}

// publishGalaxyCache atomically publishes a freshly installed staging directory to
// the shared per-project cache using a temp-dir copy followed by an atomic rename.
// It tolerates concurrent publishers without corrupting the cache.
func publishGalaxyCache(src, dst string) error {
	if info, err := os.Stat(src); err != nil || !info.IsDir() {
		return nil //nolint:nilerr // nothing staged to publish
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { //nolint:gosec // cache directories need 0o755 for compatibility
		return fmt.Errorf("creating cache parent: %w", err)
	}

	// Copy the staged tree to a sibling temp dir on the same filesystem so the
	// final swap is an atomic rename.
	tmp := dst + ".tmp-" + uuid.NewString()
	if err := os.CopyFS(tmp, os.DirFS(src)); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("staging cache copy: %w", err)
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
		return fmt.Errorf("publishing cache: %w", err)
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

// startGalaxyCacheJanitor periodically evicts per-project Galaxy caches under base
// that have gone idle past the TTL, bounding growth of the cache. The cache is
// regenerable (collections re-download on demand), so eviction never loses anything a
// job needs. A project's cache mtime is refreshed on every (re)publish, so an actively
// used cache is never evicted. The janitor goroutine exits when ctx is cancelled.
func startGalaxyCacheJanitor(ctx context.Context, base string) {
	ttl := galaxyCacheTTL()
	if ttl == 0 {
		logger.Info("Galaxy cache eviction disabled (GALAXY_CACHE_TTL_DAYS<=0)")
		return
	}
	// Sweep once at startup, before returning: callers start the janitor before they
	// take jobs, so the first sweep can never remove a cache a job is seeding from.
	pruneGalaxyCache(base, ttl)
	go func() {
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pruneGalaxyCache(base, ttl)
			}
		}
	}()
}

// pruneGalaxyCache removes per-project cache directories under base that were not
// refreshed within ttl.
func pruneGalaxyCache(base string, ttl time.Duration) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return // no cache yet - nothing to prune
	}
	cutoff := time.Now().Add(-ttl)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Only what the installer creates: a directory named after a project's UUID.
		// Anything else under the base is not the janitor's to delete, however stale.
		if _, parseErr := uuid.Parse(e.Name()); parseErr != nil {
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
