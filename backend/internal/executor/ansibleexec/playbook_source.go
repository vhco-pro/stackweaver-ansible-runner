// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansibleexec

import (
	"fmt"
	"time"
)

// The playbook source banners. Both execution paths of this binary emit them as a
// playbook_source job event, so the texts live here once: the job output of a cached
// run reads the same on the platform runner and on a self-hosted agent.

// PlaybookSourceEvent wraps a banner text in the job event both paths record.
func PlaybookSourceEvent(stdout string) Event {
	return Event{Event: "playbook_source", Task: "Playbook source", Stdout: stdout}
}

// PlaybookSourceMessage is the banner for a run that used the cached snapshot. It
// names the snapshot's commit and capture time so an operator can see at a glance
// that the job ran captured-at-sync-time code, not the remote's current HEAD.
func PlaybookSourceMessage(commit string, capturedAt *time.Time) string {
	if commit == "" {
		commit = "unknown"
	} else if len(commit) > 7 {
		commit = commit[:7]
	}
	captured := "an earlier sync"
	if capturedAt != nil {
		captured = capturedAt.UTC().Format("2006-01-02 15:04 MST")
	}
	return fmt.Sprintf(
		"Source: cached snapshot - running commit %s captured %s, not the remote's current HEAD. "+
			"To run the latest commit, set the playbook source mode to \"fresh\" or trigger a sync.\n",
		commit, captured,
	)
}

// FreshSourceMessage is the banner for a fresh-mode run: the playbook was captured at its branch
// head when the job was launched, so the run uses the remote's latest commit (#933, decision 18).
func FreshSourceMessage(commit string) string {
	if commit == "" {
		commit = "unknown"
	} else if len(commit) > 7 {
		commit = commit[:7]
	}
	return fmt.Sprintf("Source: fresh - running commit %s, captured from the branch head when this job was launched.\n", commit)
}
