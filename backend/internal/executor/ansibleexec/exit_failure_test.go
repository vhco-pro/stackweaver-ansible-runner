// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansibleexec

import "testing"

// TestExitFailure: a failed ansible-playbook is reported by its exit code, followed by what it
// wrote to stderr when it wrote anything, so a run with an empty stderr reads "Exit code 4", not
// "Exit code 4:".
func TestExitFailure(t *testing.T) {
	for _, tc := range []struct {
		code   int
		stderr string
		want   string
	}{
		{4, "", "Exit code 4"},
		{4, " \n", "Exit code 4"},
		{2, "ERROR! the playbook could not be found\n", "Exit code 2: ERROR! the playbook could not be found"},
	} {
		if got := exitFailure(tc.code, tc.stderr); got != tc.want {
			t.Errorf("exitFailure(%d, %q) = %q, want %q", tc.code, tc.stderr, got, tc.want)
		}
	}
}
