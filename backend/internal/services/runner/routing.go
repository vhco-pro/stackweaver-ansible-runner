// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package runner

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/michielvha/logger"
)

var (
	// ErrNoRunnersAvailable indicates no runners are available in the pool
	ErrNoRunnersAvailable = errors.New("no runners available in the specified agent pool")
	// ErrPoolNotFound indicates the agent pool doesn't exist
	ErrPoolNotFound = errors.New("agent pool not found")
	// ErrWorkspaceNotConfigured indicates the workspace doesn't have agent mode configured
	ErrWorkspaceNotConfigured = errors.New("workspace not configured for agent execution")
)

// RoutingService handles job routing to self-hosted runners
type RoutingService struct {
	runnerRepo    *repository.RunnerRepository
	poolRepo      *repository.AgentPoolRepository
	workspaceRepo *repository.WorkspaceRepository
}

// NewRoutingService creates a new job routing service
func NewRoutingService(
	runnerRepo *repository.RunnerRepository,
	poolRepo *repository.AgentPoolRepository,
	workspaceRepo *repository.WorkspaceRepository,
) *RoutingService {
	return &RoutingService{
		runnerRepo:    runnerRepo,
		poolRepo:      poolRepo,
		workspaceRepo: workspaceRepo,
	}
}

// JobRoutingResult contains the result of a job routing decision
type JobRoutingResult struct {
	UseSelfHosted bool           // Whether to use a self-hosted runner
	AgentPoolID   *uuid.UUID     // The agent pool to use (if self-hosted)
	RunnerID      *uuid.UUID     // The specific runner assigned (if available)
	Runner        *models.Runner // The runner details (if assigned)
}

// RouteJob determines where a job should be executed
// Returns routing result indicating whether to use self-hosted runner or platform
func (s *RoutingService) RouteJob(ctx context.Context, workspaceID string, jobType string, labels []string) (*JobRoutingResult, error) {
	// Get workspace to check execution mode
	workspace, err := s.workspaceRepo.GetByID(workspaceID)
	if err != nil {
		return nil, err
	}

	// Check if workspace is configured for agent execution
	if workspace.ExecutionMode != "agent" {
		// Use platform-hosted execution
		return &JobRoutingResult{UseSelfHosted: false}, nil
	}

	// Workspace requires agent execution
	if workspace.AgentPoolID == nil {
		logger.Warnf("Workspace %s has execution_mode=agent but no agent_pool_id set", workspaceID)
		return nil, ErrWorkspaceNotConfigured
	}

	// Verify the pool exists
	pool, err := s.poolRepo.GetByID(*workspace.AgentPoolID, false)
	if err != nil {
		return nil, ErrPoolNotFound
	}

	// Check pool scoping (allowed/excluded workspaces, allowed projects)
	if !s.isWorkspaceAllowedInPool(workspace, pool) {
		logger.Warnf("Workspace %s not allowed in pool %s", workspaceID, pool.Name)
		return nil, ErrWorkspaceNotConfigured
	}

	// Find an available runner in the pool
	var runnerJobType models.JobType
	if jobType == "terraform" {
		runnerJobType = models.JobTypeTerraformRun
	} else {
		runnerJobType = models.JobTypeAnsibleJob
	}

	runner, err := s.runnerRepo.FindAvailableRunner(*workspace.AgentPoolID, runnerJobType, labels)
	if err != nil {
		// No runner available, but job should still be queued for self-hosted
		logger.Infof("No runner immediately available in pool %s, job will be queued", pool.Name)
		return &JobRoutingResult{
			UseSelfHosted: true,
			AgentPoolID:   workspace.AgentPoolID,
			RunnerID:      nil,
			Runner:        nil,
		}, nil
	}

	// Runner found and assigned
	logger.Infof("Job routed to runner %s (pool: %s)", runner.Name, pool.Name)
	return &JobRoutingResult{
		UseSelfHosted: true,
		AgentPoolID:   workspace.AgentPoolID,
		RunnerID:      &runner.ID,
		Runner:        runner,
	}, nil
}

// AssignJobToRunner assigns a pending job to a specific runner
func (s *RoutingService) AssignJobToRunner(ctx context.Context, jobID uuid.UUID, runnerID uuid.UUID, jobType string) error {
	// Update runner status to busy
	if err := s.runnerRepo.UpdateStatus(runnerID, models.RunnerStatusBusy); err != nil {
		return err
	}

	logger.Infof("Assigned job %s to runner %s", jobID, runnerID)
	return nil
}

// GetPendingJobsForRunner returns jobs that can be executed by the specified runner
func (s *RoutingService) GetPendingJobsForRunner(ctx context.Context, runner *models.Runner) ([]PendingJobInfo, error) {
	// Get the pool to check workspace scoping
	pool, err := s.poolRepo.GetByID(runner.AgentPoolID, true) // Include relations
	if err != nil {
		return nil, err
	}

	// Query for pending ansible jobs in workspaces that use this pool
	pendingJobs := []PendingJobInfo{}

	// This is a simplified implementation - in production, you'd want to:
	// 1. Query ansible_jobs where status='pending' AND agent_pool_id=runner.AgentPoolID
	// 2. Filter by runner type compatibility
	// 3. Filter by label requirements
	// 4. Order by priority/created_at

	_ = pool // Pool scoping would be checked here

	return pendingJobs, nil
}

// PendingJobInfo contains information about a pending job
type PendingJobInfo struct {
	JobID         uuid.UUID
	JobType       string // "ansible_job" or "terraform_run"
	WorkspaceID   string
	WorkspaceName string
	Priority      int
	Labels        []string
}

// isWorkspaceAllowedInPool checks if a workspace is allowed to use the pool
func (s *RoutingService) isWorkspaceAllowedInPool(workspace *models.Workspace, pool *models.AgentPool) bool {
	// If pool has no scoping (neither allowed nor excluded), all workspaces are allowed
	if len(pool.AllowedWorkspaces) == 0 && len(pool.ExcludedWorkspaces) == 0 && len(pool.AllowedProjects) == 0 {
		return true
	}

	// Check if workspace is explicitly excluded
	for _, excluded := range pool.ExcludedWorkspaces {
		if excluded.ID == workspace.ID {
			return false
		}
	}

	// Check if pool has allowed workspaces and if this workspace is in the list
	if len(pool.AllowedWorkspaces) > 0 {
		for _, allowed := range pool.AllowedWorkspaces {
			if allowed.ID == workspace.ID {
				return true
			}
		}
		// Pool has allowed list but workspace isn't in it
		return false
	}

	// Check if pool has allowed projects and if workspace's project is in the list
	if len(pool.AllowedProjects) > 0 {
		for _, allowed := range pool.AllowedProjects {
			if allowed.ID == workspace.ProjectID {
				return true
			}
		}
		// Pool has allowed projects but workspace's project isn't in it
		return false
	}

	// No restrictions, workspace is allowed
	return true
}

// FindAvailableRunnerForJob finds an available runner that can execute the given job
func (s *RoutingService) FindAvailableRunnerForJob(ctx context.Context, poolID uuid.UUID, jobType string, labels []string) (*models.Runner, error) {
	var runnerJobType models.JobType
	if jobType == "terraform" {
		runnerJobType = models.JobTypeTerraformRun
	} else {
		runnerJobType = models.JobTypeAnsibleJob
	}

	return s.runnerRepo.FindAvailableRunner(poolID, runnerJobType, labels)
}
