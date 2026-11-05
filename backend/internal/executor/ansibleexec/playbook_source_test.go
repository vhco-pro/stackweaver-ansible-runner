// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansibleexec

import (
	"testing"
	"time"
)

// The banner texts are a user-visible contract shared by the platform runner and the
// self-hosted agent, and the E2E suite matches on them, so they are pinned verbatim.
func TestPlaybookSourceMessage(t *testing.T) {
	captured := time.Date(2026, 10, 11, 14, 5, 59, 0, time.FixedZone("CEST", 2*60*60))
	tests := []struct {
		name       string
		commit     string
		capturedAt *time.Time
		want       string
	}{
		{
			name:       "long commit is shortened and the capture time is rendered in UTC",
			commit:     "0123456789abcdef0123456789abcdef01234567",
			capturedAt: &captured,
			want: "Source: cached snapshot - running commit 0123456 captured 2026-10-11 12:05 UTC, not the remote's current HEAD. " +
				"To run the latest commit, set the playbook source mode to \"fresh\" or trigger a sync.\n",
		},
		{
			name:       "short commit is kept as is",
			commit:     "abc123",
			capturedAt: &captured,
			want: "Source: cached snapshot - running commit abc123 captured 2026-10-11 12:05 UTC, not the remote's current HEAD. " +
				"To run the latest commit, set the playbook source mode to \"fresh\" or trigger a sync.\n",
		},
		{
			name:       "missing commit and capture time fall back",
			commit:     "",
			capturedAt: nil,
			want: "Source: cached snapshot - running commit unknown captured an earlier sync, not the remote's current HEAD. " +
				"To run the latest commit, set the playbook source mode to \"fresh\" or trigger a sync.\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PlaybookSourceMessage(tt.commit, tt.capturedAt); got != tt.want {
				t.Errorf("PlaybookSourceMessage() =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestPlaybookSourceEvent(t *testing.T) {
	want := Event{Event: "playbook_source", Task: "Playbook source", Stdout: "banner\n"}
	if got := PlaybookSourceEvent("banner\n"); got != want {
		t.Errorf("PlaybookSourceEvent() = %+v, want %+v", got, want)
	}
}
