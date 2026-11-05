// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// DefaultHeartbeatWait is how long a launcher asks the API to hold its heartbeat; the API caps it.
const DefaultHeartbeatWait = 25 * time.Second

// quietFailures is how many heartbeats in a row may fail before the launcher logs it: one cut by a
// proxy's idle timeout is routine and costs one round trip (AC70).
const quietFailures = 2

// Poller holds the launcher's one heartbeat, whatever it serves.
type Poller struct {
	Client *Client
	// Wait is the long-poll wait asked for; zero means DefaultHeartbeatWait.
	Wait time.Duration
}

// wait is the long-poll wait the poller asks for.
func (p *Poller) wait() time.Duration {
	if p.Wait <= 0 {
		return DefaultHeartbeatWait
	}
	return p.Wait
}

// Poll sends heartbeats until one is answered, and returns its answer. A heartbeat that fails is
// sent again: at once after the first failure in a row, since a proxy cutting an idle connection is
// the common cause, and with the client's backoff after that, so an API that is down is not
// hammered. Repeated failures are logged; Poll returns an error only when ctx ends.
func (p *Poller) Poll(ctx context.Context, state runnercontract.HeartbeatRequest) (runnercontract.HeartbeatResponse, error) {
	wait := p.wait()
	for failures := 0; ; failures++ {
		resp, err := p.Client.Heartbeat(ctx, state, wait)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return resp, fmt.Errorf("heartbeat: %w", errors.Join(ctx.Err(), err))
		}
		if failures >= quietFailures-1 {
			p.Client.logf("heartbeat failed %d times in a row, retrying: %v", failures+1, err)
		}
		if failures == 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return resp, fmt.Errorf("heartbeat: %w", errors.Join(ctx.Err(), err))
		case <-time.After(p.Client.backoff.next(failures - 1)):
		}
	}
}
