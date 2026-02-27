// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package runner

import (
	"context"
	"time"

	"github.com/iac-platform/backend/internal/repository"
	"github.com/michielvha/logger"
)

// MonitorService handles background tasks for runner management
type MonitorService struct {
	runnerRepo *repository.RunnerRepository
	interval   time.Duration
	threshold  time.Duration
	stopCh     chan struct{}
}

// NewMonitorService creates a new runner monitor service
func NewMonitorService(runnerRepo *repository.RunnerRepository) *MonitorService {
	return &MonitorService{
		runnerRepo: runnerRepo,
		interval:   30 * time.Second, // Check every 30 seconds
		threshold:  30 * time.Second, // Mark offline after 30s without heartbeat
		stopCh:     make(chan struct{}),
	}
}

// Start begins the background monitoring loop
func (s *MonitorService) Start(ctx context.Context) {
	logger.Info("Starting runner monitor service")

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("Runner monitor service stopped (context cancelled)")
			return
		case <-s.stopCh:
			logger.Info("Runner monitor service stopped")
			return
		case <-ticker.C:
			s.markStaleRunners()
		}
	}
}

// Stop stops the monitoring loop
func (s *MonitorService) Stop() {
	close(s.stopCh)
}

// markStaleRunners marks runners as offline if they haven't sent a heartbeat
func (s *MonitorService) markStaleRunners() {
	count, err := s.runnerRepo.MarkOfflineIfStale(s.threshold)
	if err != nil {
		logger.Warnf("Failed to mark stale runners: %v", err)
		return
	}
	if count > 0 {
		logger.Infof("Marked %d runner(s) as offline (no heartbeat for %v)", count, s.threshold)
	}
}

// SetInterval sets the check interval
func (s *MonitorService) SetInterval(interval time.Duration) {
	s.interval = interval
}

// SetThreshold sets the heartbeat timeout threshold
func (s *MonitorService) SetThreshold(threshold time.Duration) {
	s.threshold = threshold
}
