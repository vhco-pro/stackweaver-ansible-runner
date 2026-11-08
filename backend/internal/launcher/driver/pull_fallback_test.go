// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// Only a pull that failed to reach the registry may fall back to a cached job image: a dial, DNS
// or timeout failure, a stalled pull, or a registry 5xx. A refusal (401, 403, 404, which the
// engine often reports as its own 500) and anything not positively identified never does (OD27).
func TestTransportPullErrors(t *testing.T) {
	transport := []error{
		errors.New(`Get "https://ghcr.io/v2/": dial tcp 140.82.121.34:443: connect: connection refused`),
		errors.New(`Get "https://registry.internal/v2/": dial tcp: lookup registry.internal on 127.0.0.53:53: no such host`),
		errors.New(`Get "https://ghcr.io/v2/": net/http: TLS handshake timeout`),
		errors.New(`Get "https://ghcr.io/v2/": dial tcp 140.82.121.34:443: i/o timeout`),
		errors.New(`Get "https://ghcr.io/v2/": context deadline exceeded`),
		errors.New(`received unexpected HTTP status: 503 Service Unavailable`),
		errors.New(`received unexpected HTTP status: 502 Bad Gateway`),
		&APIError{Code: 500, Message: `Get "https://ghcr.io/v2/": dial tcp 140.82.121.34:443: connect: no route to host`},
		fmt.Errorf("pulling: %w", errPullStalled),
		fmt.Errorf("pulling: %w", context.DeadlineExceeded),
	}
	refused := []error{
		&APIError{Code: 500, Message: `Head "https://ghcr.io/v2/vhco-pro/stackweaver-opentofu-runner/manifests/latest": unauthorized`},
		errors.New(`Head "https://registry.internal/v2/x/manifests/latest": no basic auth credentials`),
		errors.New(`denied: requested access to the resource is denied`),
		&APIError{Code: 403, Message: `denied`},
		errors.New(`manifest unknown: manifest unknown`),
		&APIError{Code: 404, Message: `pull access denied for x, repository does not exist or may require 'docker login'`},
		errors.New(`toomanyrequests: rate limit exceeded`),
		errors.New(`something else entirely`),
		// A refusal whose text also carries a timeout word is still a refusal.
		errors.New(`unauthorized: token expired, request timeout`),
	}
	for _, err := range transport {
		if !isTransportPullError(err) {
			t.Errorf("%q is a transport failure", err)
		}
	}
	for _, err := range refused {
		if isTransportPullError(err) {
			t.Errorf("%q must never fall back", err)
		}
	}
}
