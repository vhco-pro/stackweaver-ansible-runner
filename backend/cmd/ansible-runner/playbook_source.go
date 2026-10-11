// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package main

import (
	"fmt"
	"time"
)

// The playbook source banners. Both execution paths of this binary emit them as a
// playbook_source job event, so the texts live here once: the job output of a cached
// run reads the same on the platform runner and on a self-hosted agent.

// playbookSourceEvent wraps a banner text in the job event both paths record.
func playbookSourceEvent(stdout string) jobEvent {
	return jobEvent{Event: "playbook_source", Task: "Playbook source", Stdout: stdout}
}

// playbookSourceMessage is the banner for a run that used the cached snapshot. It
// names the snapshot's commit and capture time so an operator can see at a glance
// that the job ran captured-at-sync-time code, not the remote's current HEAD.
func playbookSourceMessage(commit string, capturedAt *time.Time) string {
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

// playbookSourceMissMessage is the banner for a cached-mode run on a self-hosted agent
// whose playbook has no snapshot yet. The agent cannot build one (it has no object
// storage), so the run clones once and the API queues a sync. The platform runner
// never emits this: it builds the missing snapshot inline before the run.
func playbookSourceMissMessage() string {
	return "Source: cached snapshot requested, but no snapshot exists yet - this run cloned the remote's current HEAD " +
		"and a sync has been queued so the next run uses the snapshot.\n"
}
