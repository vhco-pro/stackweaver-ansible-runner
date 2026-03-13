// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package models

import "testing"

func TestRunnerIsOnline(t *testing.T) {
	tests := []struct {
		name     string
		status   RunnerStatus
		expected bool
	}{
		{name: "online runner", status: RunnerStatusOnline, expected: true},
		{name: "busy runner", status: RunnerStatusBusy, expected: true},
		{name: "offline runner", status: RunnerStatusOffline, expected: false},
		{name: "error runner", status: RunnerStatusError, expected: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Runner{Status: tt.status}
			if got := r.IsOnline(); got != tt.expected {
				t.Errorf("IsOnline() = %v, want %v for status %q", got, tt.expected, tt.status)
			}
		})
	}
}

func TestRunnerIsAvailable(t *testing.T) {
	tests := []struct {
		name              string
		status            RunnerStatus
		currentJobs       int
		maxConcurrentJobs int
		expected          bool
	}{
		{name: "online with capacity", status: RunnerStatusOnline, currentJobs: 0, maxConcurrentJobs: 2, expected: true},
		{name: "online at capacity", status: RunnerStatusOnline, currentJobs: 2, maxConcurrentJobs: 2, expected: false},
		{name: "online over capacity", status: RunnerStatusOnline, currentJobs: 3, maxConcurrentJobs: 2, expected: false},
		{name: "busy runner", status: RunnerStatusBusy, currentJobs: 0, maxConcurrentJobs: 2, expected: false},
		{name: "offline runner", status: RunnerStatusOffline, currentJobs: 0, maxConcurrentJobs: 2, expected: false},
		{name: "error runner", status: RunnerStatusError, currentJobs: 0, maxConcurrentJobs: 2, expected: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Runner{
				Status:            tt.status,
				CurrentJobs:       tt.currentJobs,
				MaxConcurrentJobs: tt.maxConcurrentJobs,
			}
			if got := r.IsAvailable(); got != tt.expected {
				t.Errorf("IsAvailable() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestRunnerCanExecuteTerraform(t *testing.T) {
	tests := []struct {
		name       string
		runnerType RunnerType
		expected   bool
	}{
		{name: "terraform runner", runnerType: RunnerTypeTerraform, expected: true},
		{name: "combined runner", runnerType: RunnerTypeCombined, expected: true},
		{name: "ansible runner", runnerType: RunnerTypeAnsible, expected: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Runner{RunnerType: tt.runnerType}
			if got := r.CanExecuteTerraform(); got != tt.expected {
				t.Errorf("CanExecuteTerraform() = %v, want %v for type %q", got, tt.expected, tt.runnerType)
			}
		})
	}
}

func TestRunnerCanExecuteAnsible(t *testing.T) {
	tests := []struct {
		name       string
		runnerType RunnerType
		expected   bool
	}{
		{name: "ansible runner", runnerType: RunnerTypeAnsible, expected: true},
		{name: "combined runner", runnerType: RunnerTypeCombined, expected: true},
		{name: "terraform runner", runnerType: RunnerTypeTerraform, expected: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Runner{RunnerType: tt.runnerType}
			if got := r.CanExecuteAnsible(); got != tt.expected {
				t.Errorf("CanExecuteAnsible() = %v, want %v for type %q", got, tt.expected, tt.runnerType)
			}
		})
	}
}

func TestRunnerHasLabel(t *testing.T) {
	r := &Runner{Labels: RunnerLabels{"gpu", "linux", "us-east-1"}}

	tests := []struct {
		name     string
		label    string
		expected bool
	}{
		{name: "has label gpu", label: "gpu", expected: true},
		{name: "has label linux", label: "linux", expected: true},
		{name: "has label us-east-1", label: "us-east-1", expected: true},
		{name: "missing label windows", label: "windows", expected: false},
		{name: "empty string", label: "", expected: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.HasLabel(tt.label); got != tt.expected {
				t.Errorf("HasLabel(%q) = %v, want %v", tt.label, got, tt.expected)
			}
		})
	}
}

func TestRunnerHasAllLabels(t *testing.T) {
	r := &Runner{Labels: RunnerLabels{"gpu", "linux", "us-east-1"}}

	tests := []struct {
		name     string
		labels   []string
		expected bool
	}{
		{name: "all present", labels: []string{"gpu", "linux"}, expected: true},
		{name: "one missing", labels: []string{"gpu", "windows"}, expected: false},
		{name: "all missing", labels: []string{"windows", "arm"}, expected: false},
		{name: "empty required", labels: []string{}, expected: true},
		{name: "single present", labels: []string{"us-east-1"}, expected: true},
		{name: "single missing", labels: []string{"eu-west-1"}, expected: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.HasAllLabels(tt.labels); got != tt.expected {
				t.Errorf("HasAllLabels(%v) = %v, want %v", tt.labels, got, tt.expected)
			}
		})
	}
}

func TestRunnerHasLabel_EmptyLabels(t *testing.T) {
	r := &Runner{Labels: RunnerLabels{}}
	if r.HasLabel("anything") {
		t.Error("HasLabel should return false for runner with no labels")
	}
}

func TestRunnerHasAllLabels_EmptyRunnerLabels(t *testing.T) {
	r := &Runner{Labels: RunnerLabels{}}
	if r.HasAllLabels([]string{"gpu"}) {
		t.Error("HasAllLabels should return false when runner has no labels but labels are required")
	}
	if !r.HasAllLabels([]string{}) {
		t.Error("HasAllLabels should return true when no labels are required")
	}
}

func TestRunnerLabelsValueScan(t *testing.T) {
	// Test Value
	labels := RunnerLabels{"gpu", "linux"}
	val, err := labels.Value()
	if err != nil {
		t.Fatalf("Value() error: %v", err)
	}
	bs, ok := val.([]byte)
	if !ok {
		t.Fatalf("Value() returned %T, want []byte", val)
	}
	valStr := string(bs)
	if valStr != `["gpu","linux"]` {
		t.Errorf("Value() = %s, want %s", valStr, `["gpu","linux"]`)
	}

	// Test empty Value
	empty := RunnerLabels{}
	emptyVal, err := empty.Value()
	if err != nil {
		t.Fatalf("Value() error for empty: %v", err)
	}
	if emptyVal != "[]" {
		t.Errorf("Value() for empty = %v, want \"[]\"", emptyVal)
	}

	// Test Scan from []byte
	var scanned RunnerLabels
	if err := scanned.Scan([]byte(`["a","b"]`)); err != nil {
		t.Fatalf("Scan([]byte) error: %v", err)
	}
	if len(scanned) != 2 || scanned[0] != "a" || scanned[1] != "b" {
		t.Errorf("Scan([]byte) = %v, want [a b]", scanned)
	}

	// Test Scan from string
	var scanned2 RunnerLabels
	if err := scanned2.Scan(`["x","y"]`); err != nil {
		t.Fatalf("Scan(string) error: %v", err)
	}
	if len(scanned2) != 2 || scanned2[0] != "x" || scanned2[1] != "y" {
		t.Errorf("Scan(string) = %v, want [x y]", scanned2)
	}

	// Test Scan nil
	var scanned3 RunnerLabels
	if err := scanned3.Scan(nil); err != nil {
		t.Fatalf("Scan(nil) error: %v", err)
	}
	if len(scanned3) != 0 {
		t.Errorf("Scan(nil) = %v, want empty", scanned3)
	}
}

func TestRunnerCollectionsValueScan(t *testing.T) {
	colls := RunnerCollections{"community.general", "ansible.builtin"}
	val, err := colls.Value()
	if err != nil {
		t.Fatalf("Value() error: %v", err)
	}
	bs, ok := val.([]byte)
	if !ok {
		t.Fatalf("Value() returned %T, want []byte", val)
	}
	valStr := string(bs)
	if valStr != `["community.general","ansible.builtin"]` {
		t.Errorf("Value() = %s, want %s", valStr, `["community.general","ansible.builtin"]`)
	}

	// Empty
	empty := RunnerCollections{}
	emptyVal, err := empty.Value()
	if err != nil {
		t.Fatalf("Value() error: %v", err)
	}
	if emptyVal != "[]" {
		t.Errorf("Value() for empty = %v, want \"[]\"", emptyVal)
	}

	// Scan
	var scanned RunnerCollections
	if err := scanned.Scan([]byte(`["a.b"]`)); err != nil {
		t.Fatalf("Scan error: %v", err)
	}
	if len(scanned) != 1 || scanned[0] != "a.b" {
		t.Errorf("Scan = %v, want [a.b]", scanned)
	}

	// Scan nil
	var scanned2 RunnerCollections
	if err := scanned2.Scan(nil); err != nil {
		t.Fatalf("Scan(nil) error: %v", err)
	}
	if len(scanned2) != 0 {
		t.Errorf("Scan(nil) = %v, want empty", scanned2)
	}
}
