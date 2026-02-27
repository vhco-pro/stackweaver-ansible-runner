// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansible

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/queue"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/michielvha/logger"
)

// JobService handles Ansible job operations
type JobService struct {
	jobRepo         *repository.AnsibleJobRepository
	playbookRepo    *repository.AnsiblePlaybookRepository
	inventoryRepo   *repository.AnsibleInventoryRepository
	templateRepo    *repository.AnsibleJobTemplateRepository
	projectRepo     *repository.ProjectRepository
	variableService interface { // Variable service interface for getting variables
		GetVariablesForAnsibleJob(ctx context.Context, projectID uuid.UUID, templateID *uuid.UUID, templateExtraVars map[string]interface{}, jobExtraVars map[string]interface{}, inventoryID *uuid.UUID, playbookID *uuid.UUID) (map[string]interface{}, error)
	}
	queue queue.Queue
}

// NewJobService creates a new job service
func NewJobService(
	jobRepo *repository.AnsibleJobRepository,
	playbookRepo *repository.AnsiblePlaybookRepository,
	inventoryRepo *repository.AnsibleInventoryRepository,
	templateRepo *repository.AnsibleJobTemplateRepository,
	projectRepo *repository.ProjectRepository,
	queue queue.Queue,
) *JobService {
	return &JobService{
		jobRepo:       jobRepo,
		playbookRepo:  playbookRepo,
		inventoryRepo: inventoryRepo,
		templateRepo:  templateRepo,
		projectRepo:   projectRepo,
		queue:         queue,
	}
}

// NewJobServiceWithVariables creates a new job service with variable set support
func NewJobServiceWithVariables(
	jobRepo *repository.AnsibleJobRepository,
	playbookRepo *repository.AnsiblePlaybookRepository,
	inventoryRepo *repository.AnsibleInventoryRepository,
	templateRepo *repository.AnsibleJobTemplateRepository,
	projectRepo *repository.ProjectRepository,
	variableService interface {
		GetVariablesForAnsibleJob(ctx context.Context, projectID uuid.UUID, templateID *uuid.UUID, templateExtraVars map[string]interface{}, jobExtraVars map[string]interface{}, inventoryID *uuid.UUID, playbookID *uuid.UUID) (map[string]interface{}, error)
	},
	queue queue.Queue,
) *JobService {
	return &JobService{
		jobRepo:         jobRepo,
		playbookRepo:    playbookRepo,
		inventoryRepo:   inventoryRepo,
		templateRepo:    templateRepo,
		projectRepo:     projectRepo,
		variableService: variableService,
		queue:           queue,
	}
}

// LaunchJobInput represents the input for launching a job
type LaunchJobInput struct {
	ProjectID      uuid.UUID
	PlaybookID     uuid.UUID
	InventoryID    uuid.UUID
	TemplateID     *uuid.UUID
	Name           string
	JobType        models.AnsibleJobType
	ExtraVars      models.JobExtraVars
	Limit          string
	Tags           string
	SkipTags       string
	Verbosity      int
	Forks          int
	CredentialID   *uuid.UUID
	AgentPoolID    *uuid.UUID
	BecomeEnabled  bool
	DiffMode       bool
	AnsibleVersion string
	CreatedBy      *uuid.UUID
}

// AnsibleJobMessage represents the message sent to the job queue
type AnsibleJobMessage struct {
	JobID       uuid.UUID `json:"job_id"`
	PlaybookID  uuid.UUID `json:"playbook_id"`
	InventoryID uuid.UUID `json:"inventory_id"`
	JobType     string    `json:"job_type"`
}

// LaunchJob creates and queues a new Ansible job
func (s *JobService) LaunchJob(ctx context.Context, input LaunchJobInput) (*models.AnsibleJob, error) {
	// Validate project exists
	project, err := s.projectRepo.GetByID(input.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("project not found: %w", err)
	}

	// Validate playbook exists and belongs to project
	playbook, err := s.playbookRepo.GetByID(input.PlaybookID)
	if err != nil {
		return nil, fmt.Errorf("playbook not found: %w", err)
	}
	if playbook.ProjectID != input.ProjectID {
		return nil, fmt.Errorf("playbook does not belong to this project")
	}

	// Validate inventory exists
	inventory, err := s.inventoryRepo.GetByID(input.InventoryID)
	if err != nil {
		return nil, fmt.Errorf("inventory not found: %w", err)
	}
	// Verify inventory belongs to the same organization
	if inventory.OrganizationID != project.OrganizationID {
		return nil, fmt.Errorf("inventory does not belong to this organization")
	}

	// Validate template if specified
	if input.TemplateID != nil {
		template, err := s.templateRepo.GetByID(*input.TemplateID)
		if err != nil {
			return nil, fmt.Errorf("template not found: %w", err)
		}
		if template.ProjectID != input.ProjectID {
			return nil, fmt.Errorf("template does not belong to this project")
		}
	}

	// Set defaults
	if input.JobType == "" {
		input.JobType = models.AnsibleJobTypeRun
	}
	if input.Forks == 0 {
		input.Forks = 5
	}
	if input.Verbosity < 0 {
		input.Verbosity = 0
	}
	if input.Verbosity > 4 {
		input.Verbosity = 4
	}

	// Get template extra_vars if template is specified
	var templateExtraVars map[string]interface{}
	if input.TemplateID != nil {
		template, err := s.templateRepo.GetByID(*input.TemplateID)
		if err == nil {
			templateExtraVars = template.ExtraVars
		}
	}

	// Merge variables: platform vars → variable sets → template vars → job vars
	var finalExtraVars models.JobExtraVars
	if s.variableService != nil {
		// Convert input.ExtraVars to map[string]interface{} for variable service
		jobExtraVarsMap := make(map[string]interface{})
		for k, v := range input.ExtraVars {
			jobExtraVarsMap[k] = v
		}

		mergedVars, err := s.variableService.GetVariablesForAnsibleJob(
			ctx,
			input.ProjectID,
			input.TemplateID,
			templateExtraVars,
			jobExtraVarsMap,
			&input.InventoryID,
			&input.PlaybookID,
		)
		if err == nil {
			// Convert back to JobExtraVars
			finalExtraVars = make(models.JobExtraVars)
			for k, v := range mergedVars {
				finalExtraVars[k] = v
			}
		} else {
			// Fallback to original behavior if variable service fails
			logger.Warnf("Failed to get variables for Ansible job: %v, using original extra_vars", err)
			finalExtraVars = input.ExtraVars
		}
	} else {
		// No variable service available, use original extra_vars
		finalExtraVars = input.ExtraVars
	}

	// Create job
	job := &models.AnsibleJob{
		ProjectID:      input.ProjectID,
		PlaybookID:     input.PlaybookID,
		InventoryID:    input.InventoryID,
		TemplateID:     input.TemplateID,
		Name:           input.Name,
		JobType:        input.JobType,
		Status:         models.AnsibleJobStatusPending,
		ExtraVars:      finalExtraVars,
		Limit:          input.Limit,
		Tags:           input.Tags,
		SkipTags:       input.SkipTags,
		Verbosity:      input.Verbosity,
		Forks:          input.Forks,
		CredentialID:   input.CredentialID,
		AgentPoolID:    input.AgentPoolID,
		BecomeEnabled:  input.BecomeEnabled,
		DiffMode:       input.DiffMode,
		AnsibleVersion: input.AnsibleVersion,
		CreatedBy:      input.CreatedBy,
	}

	if job.ExtraVars == nil {
		job.ExtraVars = make(models.JobExtraVars)
	}

	if job.Name == "" {
		job.Name = fmt.Sprintf("Job #%d - %s", time.Now().Unix(), playbook.Name)
	}

	if err := s.jobRepo.Create(job); err != nil {
		return nil, fmt.Errorf("failed to create job: %w", err)
	}

	// Route job to the appropriate executor:
	// - Jobs with an AgentPoolID are picked up by self-hosted runners via heartbeat polling
	//   (they query for pending jobs matching their pool)
	// - Jobs without an AgentPoolID go to the Redis queue for platform-hosted runners
	if job.AgentPoolID == nil {
		if s.queue != nil {
			jobMessage := AnsibleJobMessage{
				JobID:       job.ID,
				PlaybookID:  job.PlaybookID,
				InventoryID: job.InventoryID,
				JobType:     string(job.JobType),
			}
			if err := s.queue.Enqueue(ctx, "ansible_jobs", jobMessage); err != nil {
				// Update job status to error if queueing fails
				job.Status = models.AnsibleJobStatusError
				job.ErrorMessage = fmt.Sprintf("failed to queue job: %v", err)
				if updateErr := s.jobRepo.Update(job); updateErr != nil {
					logger.Warnf("Failed to update job status: %v", updateErr)
				}
				return nil, fmt.Errorf("failed to queue job: %w", err)
			}
		}
	}
	// Jobs with AgentPoolID remain in 'pending' status and will be discovered
	// by self-hosted runners during their next heartbeat poll

	return job, nil
}

// LaunchFromTemplate creates a job from a template
func (s *JobService) LaunchFromTemplate(ctx context.Context, templateID uuid.UUID, overrideExtraVars models.JobExtraVars, createdBy *uuid.UUID) (*models.AnsibleJob, error) {
	template, err := s.templateRepo.GetByID(templateID)
	if err != nil {
		return nil, fmt.Errorf("template not found: %w", err)
	}

	// Merge variables: platform vars → variable sets → template vars → override vars
	var extraVars models.JobExtraVars
	if s.variableService != nil {
		// Convert overrideExtraVars to map[string]interface{} for variable service
		overrideExtraVarsMap := make(map[string]interface{})
		for k, v := range overrideExtraVars {
			overrideExtraVarsMap[k] = v
		}

		mergedVars, err := s.variableService.GetVariablesForAnsibleJob(
			ctx,
			template.ProjectID,
			&templateID,
			template.ExtraVars,
			overrideExtraVarsMap,
			&template.InventoryID,
			&template.PlaybookID,
		)
		if err == nil {
			// Convert back to JobExtraVars
			extraVars = make(models.JobExtraVars)
			for k, v := range mergedVars {
				extraVars[k] = v
			}
		} else {
			// Fallback to original behavior if variable service fails
			logger.Warnf("Failed to get variables for Ansible job: %v, using original merge", err)
			extraVars = make(models.JobExtraVars)
			for k, v := range template.ExtraVars {
				extraVars[k] = v
			}
			for k, v := range overrideExtraVars {
				extraVars[k] = v
			}
		}
	} else {
		// No variable service available, use original merge
		extraVars = make(models.JobExtraVars)
		for k, v := range template.ExtraVars {
			extraVars[k] = v
		}
		for k, v := range overrideExtraVars {
			extraVars[k] = v
		}
	}

	input := LaunchJobInput{
		ProjectID:     template.ProjectID,
		PlaybookID:    template.PlaybookID,
		InventoryID:   template.InventoryID,
		TemplateID:    &templateID,
		Name:          fmt.Sprintf("%s - %s", template.Name, time.Now().Format("2006-01-02 15:04:05")),
		JobType:       models.AnsibleJobTypeRun,
		ExtraVars:     extraVars,
		Limit:         template.Limit,
		Tags:          template.Tags,
		SkipTags:      template.SkipTags,
		Verbosity:     template.Verbosity,
		Forks:         template.Forks,
		CredentialID:  template.CredentialID,
		AgentPoolID:   template.AgentPoolID,
		BecomeEnabled: template.BecomeEnabled,
		DiffMode:      template.DiffMode,
		CreatedBy:     createdBy,
	}

	return s.LaunchJob(ctx, input)
}

// GetJob retrieves a job by ID
func (s *JobService) GetJob(id uuid.UUID) (*models.AnsibleJob, error) {
	return s.jobRepo.GetByID(id)
}

// ListJobsByProject lists jobs for a project
func (s *JobService) ListJobsByProject(projectID uuid.UUID, limit, offset int) ([]models.AnsibleJob, int64, error) {
	return s.jobRepo.ListByProject(projectID, limit, offset)
}

// ListJobsByOrganization lists jobs for an organization
func (s *JobService) ListJobsByOrganization(orgID uuid.UUID, limit, offset int) ([]models.AnsibleJob, int64, error) {
	return s.jobRepo.ListByOrganization(orgID, limit, offset)
}

// ListJobsByPlaybook lists jobs for a playbook
func (s *JobService) ListJobsByPlaybook(playbookID uuid.UUID, limit, offset int) ([]models.AnsibleJob, int64, error) {
	return s.jobRepo.ListByPlaybook(playbookID, limit, offset)
}

// ListJobsByInventory lists jobs for an inventory
func (s *JobService) ListJobsByInventory(inventoryID uuid.UUID, limit, offset int) ([]models.AnsibleJob, int64, error) {
	return s.jobRepo.ListByInventory(inventoryID, limit, offset)
}

// GetJobQueue retrieves queued jobs for an organization
func (s *JobService) GetJobQueue(orgID uuid.UUID, limit int) ([]models.AnsibleJob, error) {
	return s.jobRepo.ListQueued(orgID, limit)
}

// CancelJob cancels a pending or running job
func (s *JobService) CancelJob(id uuid.UUID) (*models.AnsibleJob, error) {
	job, err := s.jobRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("job not found: %w", err)
	}

	// Can only cancel pending or running jobs
	if job.Status != models.AnsibleJobStatusPending && job.Status != models.AnsibleJobStatusRunning {
		return nil, fmt.Errorf("job cannot be canceled in status: %s", job.Status)
	}

	now := time.Now()
	job.Status = models.AnsibleJobStatusCanceled
	job.FinishedAt = &now

	if err := s.jobRepo.Update(job); err != nil {
		return nil, fmt.Errorf("failed to cancel job: %w", err)
	}

	return job, nil
}

// UpdateJobStatus updates the status of a job (used by runner)
func (s *JobService) UpdateJobStatus(id uuid.UUID, status models.AnsibleJobStatus, exitCode *int, errorMessage string) (*models.AnsibleJob, error) {
	job, err := s.jobRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("job not found: %w", err)
	}

	now := time.Now()

	// Update started_at when transitioning to running
	if status == models.AnsibleJobStatusRunning && job.StartedAt == nil {
		job.StartedAt = &now
	}

	// Update finished_at when transitioning to a terminal state
	if status == models.AnsibleJobStatusSuccessful ||
		status == models.AnsibleJobStatusFailed ||
		status == models.AnsibleJobStatusCanceled ||
		status == models.AnsibleJobStatusError {
		job.FinishedAt = &now
	}

	job.Status = status
	job.ExitCode = exitCode
	if errorMessage != "" {
		job.ErrorMessage = errorMessage
	}

	if err := s.jobRepo.Update(job); err != nil {
		return nil, fmt.Errorf("failed to update job: %w", err)
	}

	return job, nil
}

// UpdateJobStats updates job statistics (used by runner after completion)
func (s *JobService) UpdateJobStats(id uuid.UUID, hostsTotal, hostsOk, hostsChanged, hostsFailed, hostsUnreachable, hostsSkipped, hostsRescued, hostsIgnored int) error {
	job, err := s.jobRepo.GetByID(id)
	if err != nil {
		return fmt.Errorf("job not found: %w", err)
	}

	job.HostsTotal = hostsTotal
	job.HostsOk = hostsOk
	job.HostsChanged = hostsChanged
	job.HostsFailed = hostsFailed
	job.HostsUnreachable = hostsUnreachable
	job.HostsSkipped = hostsSkipped
	job.HostsRescued = hostsRescued
	job.HostsIgnored = hostsIgnored

	return s.jobRepo.Update(job)
}

// AddJobEvent adds an event to a job
func (s *JobService) AddJobEvent(jobID uuid.UUID, event string, eventData models.JobExtraVars, host, task, play, role, stdout, stderr string, changed, failed, skipped bool) (*models.AnsibleJobEvent, error) {
	// Get next counter
	counter, err := s.jobRepo.GetLastEventCounter(jobID)
	if err != nil {
		return nil, fmt.Errorf("failed to get event counter: %w", err)
	}

	jobEvent := &models.AnsibleJobEvent{
		JobID:     jobID,
		Counter:   counter + 1,
		Event:     event,
		EventData: eventData,
		Host:      host,
		Task:      task,
		Play:      play,
		Role:      role,
		Stdout:    stdout,
		Stderr:    stderr,
		Changed:   changed,
		Failed:    failed,
		Skipped:   skipped,
		Timestamp: time.Now(),
	}

	if jobEvent.EventData == nil {
		jobEvent.EventData = make(models.JobExtraVars)
	}

	if err := s.jobRepo.CreateEvent(jobEvent); err != nil {
		return nil, fmt.Errorf("failed to create event: %w", err)
	}

	return jobEvent, nil
}

// AddJobEventsBatch adds multiple events to a job in batch
func (s *JobService) AddJobEventsBatch(events []models.AnsibleJobEvent) error {
	return s.jobRepo.CreateEventsBatch(events)
}

// GetJobEvents retrieves events for a job
func (s *JobService) GetJobEvents(jobID uuid.UUID, limit, offset int) ([]models.AnsibleJobEvent, int64, error) {
	return s.jobRepo.ListEventsByJob(jobID, limit, offset)
}

// GetJobOutput returns the combined stdout from all job events
func (s *JobService) GetJobOutput(jobID uuid.UUID) (string, error) {
	events, _, err := s.jobRepo.ListEventsByJob(jobID, 10000, 0)
	if err != nil {
		return "", fmt.Errorf("failed to get job events: %w", err)
	}

	var output string
	for _, event := range events {
		if event.Stdout != "" {
			output += event.Stdout
		}
	}

	return output, nil
}

// RelaunchJob creates a new job with the same configuration as an existing job
func (s *JobService) RelaunchJob(ctx context.Context, jobID uuid.UUID, createdBy *uuid.UUID) (*models.AnsibleJob, error) {
	originalJob, err := s.jobRepo.GetByID(jobID)
	if err != nil {
		return nil, fmt.Errorf("job not found: %w", err)
	}

	input := LaunchJobInput{
		ProjectID:      originalJob.ProjectID,
		PlaybookID:     originalJob.PlaybookID,
		InventoryID:    originalJob.InventoryID,
		TemplateID:     originalJob.TemplateID,
		Name:           fmt.Sprintf("Relaunch: %s", originalJob.Name),
		JobType:        originalJob.JobType,
		ExtraVars:      originalJob.ExtraVars,
		Limit:          originalJob.Limit,
		Tags:           originalJob.Tags,
		SkipTags:       originalJob.SkipTags,
		Verbosity:      originalJob.Verbosity,
		Forks:          originalJob.Forks,
		CredentialID:   originalJob.CredentialID,
		AgentPoolID:    originalJob.AgentPoolID,
		BecomeEnabled:  originalJob.BecomeEnabled,
		DiffMode:       originalJob.DiffMode,
		AnsibleVersion: originalJob.AnsibleVersion,
		CreatedBy:      createdBy,
	}

	return s.LaunchJob(ctx, input)
}

// DeleteJob deletes a job and its associated events
func (s *JobService) DeleteJob(jobID uuid.UUID) error {
	// First verify the job exists
	_, err := s.jobRepo.GetByID(jobID)
	if err != nil {
		return fmt.Errorf("job not found: %w", err)
	}

	// Delete job events first
	if err := s.jobRepo.DeleteEventsByJob(jobID); err != nil {
		return fmt.Errorf("failed to delete job events: %w", err)
	}

	// Then delete the job
	if err := s.jobRepo.Delete(jobID); err != nil {
		return fmt.Errorf("failed to delete job: %w", err)
	}

	return nil
}

// ParseAnsibleJSONOutput parses Ansible JSON callback output into job events
func ParseAnsibleJSONOutput(jsonOutput string) ([]models.AnsibleJobEvent, error) {
	var rawEvents []map[string]interface{}
	if err := json.Unmarshal([]byte(jsonOutput), &rawEvents); err != nil {
		// Try parsing as a single object (Ansible sometimes outputs single events)
		var singleEvent map[string]interface{}
		if err := json.Unmarshal([]byte(jsonOutput), &singleEvent); err != nil {
			return nil, fmt.Errorf("failed to parse Ansible output: %w", err)
		}
		rawEvents = []map[string]interface{}{singleEvent}
	}

	var events []models.AnsibleJobEvent
	for i, rawEvent := range rawEvents {
		event := models.AnsibleJobEvent{
			Counter:   i + 1,
			EventData: make(models.JobExtraVars),
			Timestamp: time.Now(),
		}

		// Extract event type
		if eventType, ok := rawEvent["event"].(string); ok {
			event.Event = eventType
		}

		// Extract host
		if host, ok := rawEvent["host"].(string); ok {
			event.Host = host
		}

		// Extract task
		if task, ok := rawEvent["task"].(string); ok {
			event.Task = task
		}

		// Extract play
		if play, ok := rawEvent["play"].(string); ok {
			event.Play = play
		}

		// Extract role
		if role, ok := rawEvent["role"].(string); ok {
			event.Role = role
		}

		// Extract stdout
		if stdout, ok := rawEvent["stdout"].(string); ok {
			event.Stdout = stdout
		}

		// Extract stderr
		if stderr, ok := rawEvent["stderr"].(string); ok {
			event.Stderr = stderr
		}

		// Extract changed/failed/skipped
		if result, ok := rawEvent["result"].(map[string]interface{}); ok {
			if changed, ok := result["changed"].(bool); ok {
				event.Changed = changed
			}
			if failed, ok := result["failed"].(bool); ok {
				event.Failed = failed
			}
			if skipped, ok := result["skipped"].(bool); ok {
				event.Skipped = skipped
			}
		}

		// Store full event data
		event.EventData = models.JobExtraVars(rawEvent)

		events = append(events, event)
	}

	return events, nil
}
