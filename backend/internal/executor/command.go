// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"os/exec"
	"time"
)

// orphanPipeWait is how long a stopped command's Wait waits for its output pipes to close.
var orphanPipeWait = 10 * time.Second

// BoundOrphanPipes bounds how long cmd's Wait waits for its output pipes once its context is
// done. A descendant that the stop does not reach, such as the SSH child of git, can hold the
// pipes open, and an executor that never returns keeps its launcher from deregistering.
func BoundOrphanPipes(cmd *exec.Cmd) *exec.Cmd {
	cmd.WaitDelay = orphanPipeWait
	return cmd
}
