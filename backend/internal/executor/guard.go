// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"
)

// EnvNetworkGuard is set by the container driver: the executor then refuses to run while a cloud
// metadata address answers from its network (#933, D5). On a rootless engine the driver cannot
// program the host firewall, so this check is what makes a misconfigured host fail closed.
const EnvNetworkGuard = "STACKWEAVER_NETWORK_GUARD"

// metadataProbes are the instance metadata endpoints a platform-identity job must not reach.
var metadataProbes = []string{"169.254.169.254:80", "169.254.170.23:80", "[fd00:ec2::254]:80", "[fd20:ce::254]:80"}

// GuardNetwork returns an error when the guard is on and any metadata endpoint accepts a
// connection.
func GuardNetwork(ctx context.Context) error {
	if os.Getenv(EnvNetworkGuard) != "1" {
		return nil
	}
	return guardAgainst(ctx, metadataProbes)
}

func guardAgainst(ctx context.Context, addrs []string) error {
	d := net.Dialer{Timeout: time.Second}
	for _, addr := range addrs {
		c, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = c.Close()
			return fmt.Errorf("the cloud metadata endpoint %s is reachable from this job's network; the launcher host's network does not isolate jobs, so the job is refused", addr)
		}
	}
	return nil
}
