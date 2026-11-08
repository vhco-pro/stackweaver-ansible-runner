// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package main

import (
	"testing"

	"github.com/michielvha/stackweaver/backend/internal/executor"
	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
)

// The driver and the executor it places name the limits report the same way (AC24).
func TestDriverAndExecutorShareTheLimitsReportName(t *testing.T) {
	if driver.EnvLimitsReport != executor.EnvLimitsReport {
		t.Errorf("driver sets %s, executor reads %s", driver.EnvLimitsReport, executor.EnvLimitsReport)
	}
}
