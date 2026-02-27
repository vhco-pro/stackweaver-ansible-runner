// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package main

import (
	"testing"

	"github.com/iac-platform/backend/internal/models"
	"github.com/iac-platform/backend/internal/services/vcs"
)

func TestExtractPRNumber(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int
	}{
		{"valid PR #1", "PR #1", 1},
		{"valid PR #42", "PR #42", 42},
		{"valid PR #12345", "PR #12345", 12345},
		{"empty string", "", 0},
		{"just committer name", "John Doe <john@example.com>", 0},
		{"PR without hash", "PR 5", 0},
		{"lowercase pr", "pr #5", 0},
		{"PR # with no number", "PR #", 0},
		{"PR # with text", "PR #abc", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractPRNumber(tt.input)
			if result != tt.expected {
				t.Errorf("extractPRNumber(%q) = %d, want %d", tt.input, result, tt.expected)
			}
		})
	}
}

func TestMapRunStatusToCheckState(t *testing.T) {
	tests := []struct {
		name      string
		status    models.RunStatus
		wantState vcs.StatusState
		wantOK    bool
	}{
		{"pending", models.RunStatusPending, vcs.StatusStatePending, true},
		{"planning", models.RunStatusPlanning, vcs.StatusStatePending, true},
		{"planned", models.RunStatusPlanned, vcs.StatusStateSuccess, true},
		{"failed", models.RunStatusFailed, vcs.StatusStateFailure, true},
		{"cancelled", models.RunStatusCancelled, vcs.StatusStateError, true},
		{"applying", models.RunStatusApplying, "", false},
		{"applied", models.RunStatusApplied, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := &models.Run{Status: tt.status}
			state, _, ok := mapRunStatusToCheckState(run)
			if ok != tt.wantOK {
				t.Errorf("mapRunStatusToCheckState(%s) ok = %v, want %v", tt.status, ok, tt.wantOK)
			}
			if ok && state != tt.wantState {
				t.Errorf("mapRunStatusToCheckState(%s) state = %q, want %q", tt.status, state, tt.wantState)
			}
		})
	}
}

func TestBuildPlannedDescription(t *testing.T) {
	tests := []struct {
		name       string
		planOutput map[string]interface{}
		expected   string
	}{
		{"nil plan output", nil, "planned: no changes"},
		{"no changes", map[string]interface{}{}, "planned: no changes"},
		{"only adds", map[string]interface{}{"AddCount": float64(3)}, "planned: +3"},
		{"only changes", map[string]interface{}{"ChangeCount": float64(2)}, "planned: ~2"},
		{"only destroys", map[string]interface{}{"DestroyCount": float64(1)}, "planned: -1"},
		{"all three", map[string]interface{}{"AddCount": float64(1), "ChangeCount": float64(2), "DestroyCount": float64(3)}, "planned: +1, ~2, -3"},
		{"add and destroy", map[string]interface{}{"AddCount": float64(5), "DestroyCount": float64(2)}, "planned: +5, -2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := &models.Run{PlanOutput: tt.planOutput}
			result := buildPlannedDescription(run)
			if result != tt.expected {
				t.Errorf("buildPlannedDescription() = %q, want %q", result, tt.expected)
			}
		})
	}
}
