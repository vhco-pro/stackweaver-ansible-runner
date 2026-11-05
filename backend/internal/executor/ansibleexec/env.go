// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansibleexec

import (
	"os"
	"strings"
)

// HostKeyCheckingValue is the ANSIBLE_HOST_KEY_CHECKING a play runs with: off unless the operator
// set ANSIBLE_HOST_KEY_CHECKING=true on the runner (AUD-116: it was hardcoded off, which silently
// overrode a host_key_checking set in a project or organization ansible.cfg). Job hosts are often
// ephemeral and have no stable host keys, which is why off stays the default.
func HostKeyCheckingValue() string {
	if strings.EqualFold(os.Getenv("ANSIBLE_HOST_KEY_CHECKING"), "true") {
		return "true"
	}
	return "false"
}

// SSHArgs is the ANSIBLE_SSH_ARGS a play runs with: connections multiplexed, and while host-key
// checking is off ssh neither reads nor writes a known_hosts file, since job hosts are often
// ephemeral (AUD-116). With checking on, ssh and the job's ansible.cfg govern known_hosts. The
// platform runner's queue path set the same, so the launcher path keeps it (#933, D2).
func SSHArgs() string {
	if HostKeyCheckingValue() == "true" {
		return "-o ControlMaster=auto -o ControlPersist=60s"
	}
	return "-o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no -o ControlMaster=auto -o ControlPersist=60s"
}
