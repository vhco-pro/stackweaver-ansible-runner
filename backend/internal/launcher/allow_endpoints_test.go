// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"slices"
	"testing"
)

// Beside the API, a job reaches the endpoints the installer lists in RUNNER_ALLOW_TCP and nothing
// else in the denied ranges: an air-gapped install's mirrors on the same engine or cluster, which
// the jobs network otherwise refuses (#933, D2, D5, AC76).
func TestAllowedEndpoints(t *testing.T) {
	got, err := allowedEndpoints(t.Context(), "10.0.0.1:8022", " 10.1.2.3:8080, http://10.1.2.4:9000/mirror/ ,https://10.1.2.5,,")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.0.0.1:8022", "10.1.2.3:8080", "10.1.2.4:9000", "10.1.2.5:443"}
	if !slices.Equal(got, want) {
		t.Errorf("allowed = %v, want %v", got, want)
	}
	if got, _ := allowedEndpoints(t.Context(), "", ""); len(got) != 0 {
		t.Errorf("nothing configured = %v, want none", got)
	}
	for _, bad := range []string{"no-port", "10.1.2.3:", "ftp://10.1.2.3"} {
		if _, err := allowedEndpoints(t.Context(), "", bad); err == nil {
			t.Errorf("RUNNER_ALLOW_TCP=%q was accepted", bad)
		}
	}
}
