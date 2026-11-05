// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import "fmt"

// CheckPlatform refuses every host but Linux (#933, decision 4, AC26): network namespaces and
// firewall rules, cgroup limits, read-only bind mounts, PR_SET_DUMPABLE and the Ansible control
// node itself are all Linux, so a launcher elsewhere would run jobs without the isolation it
// promises.
func CheckPlatform(goos string) error {
	if goos == "linux" {
		return nil
	}
	return fmt.Errorf("the launcher runs on Linux only (runner architecture decision 4): this host is %s, where the job isolation it relies on does not exist", goos)
}
