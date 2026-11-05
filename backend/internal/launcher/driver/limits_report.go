// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"slices"
	"strings"
)

// EnvLimitsReport carries a placed job's limits report to its executor, which writes it into the
// job's output (#933, D4, AC24). The executor reads the same name (executor.EnvLimitsReport).
const EnvLimitsReport = "STACKWEAVER_LIMITS_REPORT"

// LimitsReport says which of the limits a job's pool asked for the driver enforced (applied, as
// "cpu=1" and "memory=1Gi") and which it could not, or "" when the pool asked for none.
func LimitsReport(r Resources, applied []string) string {
	var requested []string
	if r.CPULimit != "" {
		requested = append(requested, "cpu="+r.CPULimit)
	}
	if r.MemoryLimit != "" {
		requested = append(requested, "memory="+r.MemoryLimit)
	}
	if len(requested) == 0 {
		return ""
	}
	var enforced, missing []string
	for _, l := range requested {
		if slices.Contains(applied, l) {
			enforced = append(enforced, l)
		} else {
			missing = append(missing, l)
		}
	}
	var parts []string
	if len(enforced) > 0 {
		parts = append(parts, "resource limits enforced: "+strings.Join(enforced, ", "))
	}
	if len(missing) > 0 {
		parts = append(parts, "resource limits not enforced by this driver or engine: "+strings.Join(missing, ", "))
	}
	return strings.Join(parts, "; ")
}
