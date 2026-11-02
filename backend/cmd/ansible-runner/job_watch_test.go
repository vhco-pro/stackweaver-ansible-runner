// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/michielvha/stackweaver/core/models"
	"gorm.io/gorm"
)

// The platform runner stops a running play when its job is canceled through the API, and when its
// job is deleted while it runs (with its job template, or by a playbook force delete): either way
// nobody is waiting for the play any more. A lookup that fails for another reason is transient and
// the play goes on, as the agent's status poll does.
func TestWatchPlatformJob(t *testing.T) {
	old := platformJobPollInterval
	platformJobPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { platformJobPollInterval = old })

	cases := []struct {
		name     string
		lookup   func(uuid.UUID) (*models.AnsibleJob, error)
		stopping bool
	}{
		{"canceled", func(uuid.UUID) (*models.AnsibleJob, error) {
			return &models.AnsibleJob{Status: models.AnsibleJobStatusCanceled}, nil
		}, true},
		{"deleted", func(uuid.UUID) (*models.AnsibleJob, error) { return nil, gorm.ErrRecordNotFound }, true},
		{"running", func(uuid.UUID) (*models.AnsibleJob, error) {
			return &models.AnsibleJob{Status: models.AnsibleJobStatusRunning}, nil
		}, false},
		{"lookup failing", func(uuid.UUID) (*models.AnsibleJob, error) { return nil, errors.New("connection refused") }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run, stop := context.WithCancel(context.Background())
			defer stop()
			done := make(chan struct{})
			go func() {
				watchPlatformJob(run, uuid.New(), tc.lookup, stop)
				close(done)
			}()
			select {
			case <-run.Done():
				if !tc.stopping {
					t.Fatal("the play was stopped, want it to go on")
				}
			case <-time.After(300 * time.Millisecond):
				if tc.stopping {
					t.Fatal("the play went on, want it stopped")
				}
				stop()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("the watch did not return once the run ended")
			}
		})
	}
}
