// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansible

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/repository"
	"github.com/michielvha/logger"
	"github.com/robfig/cron/v3"
)

// SchedulerService handles scheduled job execution
type SchedulerService struct {
	scheduleRepo  *repository.AnsibleScheduleRepository
	jobRepo       *repository.AnsibleJobRepository
	templateRepo  *repository.AnsibleJobTemplateRepository
	playbookRepo  *repository.AnsiblePlaybookRepository
	sourceService *InventorySourceService
	jobService    *JobService

	cronParser    cron.Parser
	mu            sync.RWMutex
	running       bool
	stopCh        chan struct{}
	ticker        *time.Ticker
	checkInterval time.Duration
}

// NewSchedulerService creates a new scheduler service
func NewSchedulerService(
	scheduleRepo *repository.AnsibleScheduleRepository,
	jobRepo *repository.AnsibleJobRepository,
	templateRepo *repository.AnsibleJobTemplateRepository,
	playbookRepo *repository.AnsiblePlaybookRepository,
	sourceService *InventorySourceService,
	jobService *JobService,
) *SchedulerService {
	return &SchedulerService{
		scheduleRepo:  scheduleRepo,
		jobRepo:       jobRepo,
		templateRepo:  templateRepo,
		playbookRepo:  playbookRepo,
		sourceService: sourceService,
		jobService:    jobService,
		cronParser:    cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow),
		checkInterval: 30 * time.Second, // Check every 30 seconds
	}
}

// Start starts the scheduler background worker
func (s *SchedulerService) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return
	}

	s.running = true
	s.stopCh = make(chan struct{})
	s.ticker = time.NewTicker(s.checkInterval)

	go s.run()
	logger.Info("Scheduler service started")
}

// Stop stops the scheduler background worker
func (s *SchedulerService) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return
	}

	close(s.stopCh)
	s.ticker.Stop()
	s.running = false
	logger.Info("Scheduler service stopped")
}

// run is the main scheduler loop
func (s *SchedulerService) run() {
	// Process any due schedules on startup
	s.processDueSchedules()

	for {
		select {
		case <-s.stopCh:
			return
		case <-s.ticker.C:
			s.processDueSchedules()
		}
	}
}

// processDueSchedules finds and executes all due schedules
func (s *SchedulerService) processDueSchedules() {
	ctx := context.Background()

	// Get all enabled schedules
	schedules, err := s.scheduleRepo.ListEnabled()
	if err != nil {
		logger.Infof("Error listing enabled schedules: %v", err)
		return
	}

	now := time.Now()

	for _, schedule := range schedules {
		// Calculate next run time if not set
		if schedule.NextRunAt == nil {
			nextRun, err := s.calculateNextRun(schedule.CronExpression, schedule.Timezone, now)
			if err != nil {
				logger.Infof("Error calculating next run for schedule %s: %v", schedule.ID, err)
				continue
			}
			if err := s.scheduleRepo.UpdateNextRun(schedule.ID, nextRun); err != nil {
				logger.Infof("Error updating next run for schedule %s: %v", schedule.ID, err)
			}
			continue
		}

		// Check if schedule is due
		if schedule.NextRunAt.After(now) {
			continue
		}

		// Execute the schedule
		if err := s.executeSchedule(ctx, &schedule); err != nil {
			logger.Infof("Error executing schedule %s: %v", schedule.ID, err)
		}

		// Calculate and update next run time
		nextRun, err := s.calculateNextRun(schedule.CronExpression, schedule.Timezone, now)
		if err != nil {
			logger.Infof("Error calculating next run for schedule %s: %v", schedule.ID, err)
			continue
		}
		if err := s.scheduleRepo.UpdateNextRun(schedule.ID, nextRun); err != nil {
			logger.Infof("Error updating next run for schedule %s: %v", schedule.ID, err)
		}
	}
}

// executeSchedule executes a scheduled job/sync
func (s *SchedulerService) executeSchedule(ctx context.Context, schedule *models.AnsibleSchedule) error {
	logger.Infof("Executing schedule: %s (%s)", schedule.Name, schedule.ID)

	var jobID *uuid.UUID
	status := "successful"

	switch schedule.Type {
	case models.ScheduleTypeJobTemplate:
		if schedule.JobTemplateID == nil {
			return fmt.Errorf("no job template ID for schedule")
		}
		job, err := s.launchJobFromTemplate(ctx, *schedule.JobTemplateID, schedule.Config)
		if err != nil {
			status = "failed"
			logger.Infof("Error launching job from template: %v", err)
		} else {
			jobID = &job.ID
		}

	case models.ScheduleTypeInventorySource:
		if schedule.InventorySourceID == nil {
			return fmt.Errorf("no inventory source ID for schedule")
		}
		_, err := s.sourceService.SyncInventorySource(ctx, *schedule.InventorySourceID)
		if err != nil {
			status = "failed"
			logger.Infof("Error syncing inventory source: %v", err)
		}

	case models.ScheduleTypePlaybookSync:
		if schedule.PlaybookID == nil {
			return fmt.Errorf("no playbook ID for schedule")
		}
		// Trigger playbook VCS sync
		if err := s.syncPlaybook(ctx, *schedule.PlaybookID); err != nil {
			status = "failed"
			logger.Infof("Error syncing playbook: %v", err)
		}

	default:
		return fmt.Errorf("unknown schedule type: %s", schedule.Type)
	}

	// Update last run information
	if err := s.scheduleRepo.UpdateLastRun(schedule.ID, time.Now(), jobID, status); err != nil {
		logger.Infof("Error updating last run for schedule %s: %v", schedule.ID, err)
	}

	return nil
}

// launchJobFromTemplate creates and queues a job from a template
func (s *SchedulerService) launchJobFromTemplate(ctx context.Context, templateID uuid.UUID, config models.ScheduleConfig) (*models.AnsibleJob, error) {
	// Get template
	template, err := s.templateRepo.GetByID(templateID)
	if err != nil {
		return nil, fmt.Errorf("template not found: %w", err)
	}

	// Merge extra_vars from config if provided
	extraVars := make(models.JobExtraVars)
	for k, v := range template.ExtraVars {
		extraVars[k] = v
	}
	if configExtraVars, ok := config["extra_vars"].(map[string]interface{}); ok {
		for k, v := range configExtraVars {
			extraVars[k] = v
		}
	}

	// Create job
	job := &models.AnsibleJob{
		ProjectID:     template.ProjectID,
		PlaybookID:    template.PlaybookID,
		InventoryID:   template.InventoryID,
		TemplateID:    &template.ID,
		Name:          fmt.Sprintf("Scheduled: %s", template.Name),
		JobType:       models.AnsibleJobTypeRun,
		Status:        models.AnsibleJobStatusPending,
		ExtraVars:     extraVars,
		Limit:         template.Limit,
		Tags:          template.Tags,
		SkipTags:      template.SkipTags,
		Verbosity:     template.Verbosity,
		Forks:         template.Forks,
		CredentialID:  template.CredentialID,
		BecomeEnabled: template.BecomeEnabled,
		DiffMode:      template.DiffMode,
	}

	if err := s.jobRepo.Create(job); err != nil {
		return nil, fmt.Errorf("failed to create job: %w", err)
	}

	// Queue job for execution (this would use the job service queue)
	// For now, we just create the job record
	logger.Infof("Created scheduled job: %s", job.ID)

	return job, nil
}

// syncPlaybook triggers a VCS sync for a playbook
func (s *SchedulerService) syncPlaybook(ctx context.Context, playbookID uuid.UUID) error {
	// This would trigger the playbook sync worker
	// For now, just update the playbook sync status
	playbook, err := s.playbookRepo.GetByID(playbookID)
	if err != nil {
		return fmt.Errorf("playbook not found: %w", err)
	}

	logger.Infof("Triggered sync for playbook: %s (%s)", playbook.Name, playbook.ID)
	// The actual sync would be handled by the ansible-runner sync worker
	return nil
}

// calculateNextRun calculates the next run time based on cron expression
func (s *SchedulerService) calculateNextRun(cronExpr, timezone string, from time.Time) (time.Time, error) {
	// Parse cron expression
	schedule, err := s.cronParser.Parse(cronExpr)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid cron expression: %w", err)
	}

	// Load timezone
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}

	// Convert from time to the schedule's timezone
	fromInTz := from.In(loc)

	// Get next run time
	next := schedule.Next(fromInTz)

	// Convert back to UTC for storage
	return next.UTC(), nil
}

// CreateSchedule creates a new schedule
func (s *SchedulerService) CreateSchedule(
	orgID uuid.UUID,
	name, description string,
	scheduleType models.ScheduleType,
	cronExpression, timezone string,
	jobTemplateID, inventorySourceID, playbookID *uuid.UUID,
	config models.ScheduleConfig,
	createdBy *uuid.UUID,
	startDateTime, endDateTime *time.Time,
) (*models.AnsibleSchedule, error) {
	// Validate cron expression
	if _, err := s.cronParser.Parse(cronExpression); err != nil {
		return nil, fmt.Errorf("invalid cron expression: %w", err)
	}

	// Validate timezone
	if _, err := time.LoadLocation(timezone); err != nil {
		return nil, fmt.Errorf("invalid timezone: %w", err)
	}

	// Validate target based on type
	switch scheduleType {
	case models.ScheduleTypeJobTemplate:
		if jobTemplateID == nil {
			return nil, fmt.Errorf("job_template_id is required for job_template schedules")
		}
		if _, err := s.templateRepo.GetByID(*jobTemplateID); err != nil {
			return nil, fmt.Errorf("job template not found: %w", err)
		}
	case models.ScheduleTypeInventorySource:
		if inventorySourceID == nil {
			return nil, fmt.Errorf("inventory_source_id is required for inventory_source schedules")
		}
		// Validate source exists - would need source repo
	case models.ScheduleTypePlaybookSync:
		if playbookID == nil {
			return nil, fmt.Errorf("playbook_id is required for playbook_sync schedules")
		}
		if _, err := s.playbookRepo.GetByID(*playbookID); err != nil {
			return nil, fmt.Errorf("playbook not found: %w", err)
		}
	}

	// Calculate next run time
	nextRun, err := s.calculateNextRun(cronExpression, timezone, time.Now())
	if err != nil {
		return nil, err
	}

	schedule := &models.AnsibleSchedule{
		OrganizationID:    orgID,
		Name:              name,
		Description:       description,
		Type:              scheduleType,
		Status:            models.ScheduleStatusEnabled,
		JobTemplateID:     jobTemplateID,
		InventorySourceID: inventorySourceID,
		PlaybookID:        playbookID,
		CronExpression:    cronExpression,
		Timezone:          timezone,
		StartDateTime:     startDateTime,
		EndDateTime:       endDateTime,
		Config:            config,
		NextRunAt:         &nextRun,
		CreatedBy:         createdBy,
	}

	if err := s.scheduleRepo.Create(schedule); err != nil {
		return nil, fmt.Errorf("failed to create schedule: %w", err)
	}

	return schedule, nil
}

// GetSchedule retrieves a schedule by ID
func (s *SchedulerService) GetSchedule(id uuid.UUID) (*models.AnsibleSchedule, error) {
	return s.scheduleRepo.GetByID(id)
}

// ListSchedules lists schedules for an organization
func (s *SchedulerService) ListSchedules(orgID uuid.UUID, limit, offset int) ([]models.AnsibleSchedule, int64, error) {
	return s.scheduleRepo.ListByOrganization(orgID, limit, offset)
}

// UpdateSchedule updates a schedule
func (s *SchedulerService) UpdateSchedule(
	id uuid.UUID,
	name, description *string,
	cronExpression, timezone *string,
	config *models.ScheduleConfig,
) (*models.AnsibleSchedule, error) {
	schedule, err := s.scheduleRepo.GetByID(id)
	if err != nil {
		return nil, fmt.Errorf("schedule not found: %w", err)
	}

	if name != nil {
		schedule.Name = *name
	}
	if description != nil {
		schedule.Description = *description
	}
	if cronExpression != nil {
		if _, err := s.cronParser.Parse(*cronExpression); err != nil {
			return nil, fmt.Errorf("invalid cron expression: %w", err)
		}
		schedule.CronExpression = *cronExpression
	}
	if timezone != nil {
		if _, err := time.LoadLocation(*timezone); err != nil {
			return nil, fmt.Errorf("invalid timezone: %w", err)
		}
		schedule.Timezone = *timezone
	}
	if config != nil {
		schedule.Config = *config
	}

	// Recalculate next run time
	tz := schedule.Timezone
	if timezone != nil {
		tz = *timezone
	}
	cronExpr := schedule.CronExpression
	if cronExpression != nil {
		cronExpr = *cronExpression
	}
	nextRun, err := s.calculateNextRun(cronExpr, tz, time.Now())
	if err != nil {
		return nil, err
	}
	schedule.NextRunAt = &nextRun

	if err := s.scheduleRepo.Update(schedule); err != nil {
		return nil, fmt.Errorf("failed to update schedule: %w", err)
	}

	return schedule, nil
}

// EnableSchedule enables a schedule
func (s *SchedulerService) EnableSchedule(id uuid.UUID) error {
	schedule, err := s.scheduleRepo.GetByID(id)
	if err != nil {
		return fmt.Errorf("schedule not found: %w", err)
	}

	// Recalculate next run time when enabling
	nextRun, err := s.calculateNextRun(schedule.CronExpression, schedule.Timezone, time.Now())
	if err != nil {
		return err
	}

	schedule.Status = models.ScheduleStatusEnabled
	schedule.NextRunAt = &nextRun

	return s.scheduleRepo.Update(schedule)
}

// DisableSchedule disables a schedule
func (s *SchedulerService) DisableSchedule(id uuid.UUID) error {
	return s.scheduleRepo.UpdateStatus(id, models.ScheduleStatusDisabled)
}

// DeleteSchedule deletes a schedule
func (s *SchedulerService) DeleteSchedule(id uuid.UUID) error {
	return s.scheduleRepo.Delete(id)
}

// GetNextRunTime calculates the next run time for a cron expression
func (s *SchedulerService) GetNextRunTime(cronExpression, timezone string) (time.Time, error) {
	return s.calculateNextRun(cronExpression, timezone, time.Now())
}

// ValidateCronExpression validates a cron expression
func (s *SchedulerService) ValidateCronExpression(cronExpression string) error {
	_, err := s.cronParser.Parse(cronExpression)
	return err
}

// RunScheduleNow triggers immediate execution of a schedule
func (s *SchedulerService) RunScheduleNow(id uuid.UUID) error {
	schedule, err := s.scheduleRepo.GetByID(id)
	if err != nil {
		return fmt.Errorf("schedule not found: %w", err)
	}

	// Execute the schedule immediately
	ctx := context.Background()
	if err := s.executeSchedule(ctx, schedule); err != nil {
		return fmt.Errorf("failed to execute schedule: %w", err)
	}

	return nil
}
