// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"strings"
	"testing"
)

// The launcher runs on Linux only (#933, decision 4, AC26), and says so when it refuses.
func TestCheckPlatform(t *testing.T) {
	if err := CheckPlatform("linux"); err != nil {
		t.Errorf("CheckPlatform(linux) = %v", err)
	}
	for _, goos := range []string{"windows", "darwin", "freebsd"} {
		err := CheckPlatform(goos)
		if err == nil || !strings.Contains(err.Error(), "decision 4") || !strings.Contains(err.Error(), "Linux") {
			t.Errorf("CheckPlatform(%s) = %v, want a refusal naming the Linux-only decision", goos, err)
		}
	}
}
