// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// Package launcher is the long-lived half of a runner (#933, D3): it registers once, holds one
// long-poll heartbeat, claims what it is offered, asks for a job token and has a driver place the
// executor with it, then supervises the placement until the job ends. It never fetches a job's
// artifacts: decrypted credentials travel from the API to the executor only (D1, AC4), which is
// why this package's client has no method for them.
package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// ErrClaimLost is returned by Start when another runner claimed the job first, or the job can no
// longer be claimed: the launcher skips it.
var ErrClaimLost = errors.New("the job was claimed by another launcher")

// capabilities are what a launcher announces on every request and at registration.
var capabilities = []string{
	runnercontract.CapabilityPlaybookSnapshot,
	runnercontract.CapabilityBlob,
	runnercontract.CapabilityJobToken,
	runnercontract.CapabilityWorkItems,
}

// Backoff is the retry schedule for calls that must eventually succeed: registration and
// deregistration.
type Backoff struct {
	Initial time.Duration
	Max     time.Duration
}

// next returns the wait after attempt n (from 0), doubled each time up to Max, with jitter of up
// to a quarter so a fleet that lost the API together does not return together.
func (b Backoff) next(n int) time.Duration {
	d := b.Initial
	for i := 0; i < n && d < b.Max; i++ {
		d *= 2
	}
	d = min(d, b.Max)
	if q := int64(d / 4); q > 0 {
		d += time.Duration(rand.Int64N(q)) //nolint:gosec // G404: jitter, not a secret
	}
	return d
}

var defaultBackoff = Backoff{Initial: time.Second, Max: time.Minute}

// Client speaks the runner control plane for a launcher. It authenticates its registration with
// the registration token (a customer pool's agent token or the deployment's hosted secret) and
// every later call with the runner token the registration returned.
type Client struct {
	serverURL         string
	registrationToken string
	http              *http.Client
	backoff           Backoff

	// Log reports what the launcher does and every retried failure; nil discards it.
	Log func(format string, args ...any)

	mu          sync.Mutex
	runnerID    string
	runnerToken string
}

func (c *Client) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log(format, args...)
	}
}

// NewClient builds a launcher client for serverURL.
func NewClient(serverURL, registrationToken string) *Client {
	return &Client{
		serverURL:         strings.TrimRight(serverURL, "/"),
		registrationToken: registrationToken,
		http:              &http.Client{},
		backoff:           defaultBackoff,
	}
}

// RunnerID is the runner the launcher registered as, or "" before it has.
func (c *Client) RunnerID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runnerID
}

func (c *Client) token() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runnerToken != "" {
		return c.runnerToken
	}
	return c.registrationToken
}

// StatusError is a non-2xx answer.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string {
	body := e.Body
	if len(body) > 300 {
		body = body[:300] + "..."
	}
	return fmt.Sprintf("status %d: %s", e.Code, strings.TrimSpace(body))
}

// call sends one request with a JSON body (nil for none) and decodes a 2xx answer into out (nil to
// discard it). timeout bounds the whole call; zero means ctx alone does.
func (c *Client) call(ctx context.Context, method, path, bearer string, in, out any, timeout time.Duration) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encoding %s: %w", path, err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.serverURL+path, body)
	if err != nil {
		return fmt.Errorf("building %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set(runnercontract.CapabilitiesHeader, strings.Join(capabilities, ","))
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req) //nolint:gosec // G704: the server URL is the operator-configured endpoint
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("reading %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &StatusError{Code: resp.StatusCode, Body: string(data)}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decoding %s %s: %w", method, path, err)
		}
	}
	return nil
}

// retry runs fn until it succeeds or ctx ends, waiting by the backoff between attempts, and calls
// onErr with each failure so the launcher can log it.
func (c *Client) retry(ctx context.Context, fn func() error, onErr func(attempt int, err error)) error {
	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		if onErr != nil {
			onErr(attempt, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("giving up: %w (last error: %w)", ctx.Err(), err)
		case <-time.After(c.backoff.next(attempt)):
		}
	}
}

// Register registers the launcher, retrying with backoff until the API answers or ctx ends, so a
// launcher started before the API simply waits for it (AC15). The capabilities are added to req.
func (c *Client) Register(ctx context.Context, req runnercontract.RegisterRequest) (runnercontract.RegisterResponse, error) {
	req.Capabilities = append(req.Capabilities, capabilities...)
	var resp runnercontract.RegisterResponse
	err := c.retry(ctx, func() error {
		return c.call(ctx, http.MethodPost, "/api/v2/runner/register", c.registrationToken, req, &resp, 30*time.Second)
	}, c.logRetry("registration"))
	if err != nil {
		return resp, err
	}
	c.mu.Lock()
	c.runnerID, c.runnerToken = resp.RunnerID, resp.RunnerAPIKey
	c.mu.Unlock()
	return resp, nil
}

func (c *Client) logRetry(what string) func(int, error) {
	return func(attempt int, err error) {
		c.logf("%s failed (attempt %d), retrying: %v", what, attempt+1, err)
	}
}

// Heartbeat reports the launcher's state and returns its offers. wait asks the API to hold the
// request until an offer exists or wait elapses (the long poll, AC70); the call's own deadline is
// a little longer than wait, so a cut connection surfaces as an error the caller answers by
// polling again.
func (c *Client) Heartbeat(ctx context.Context, req runnercontract.HeartbeatRequest, wait time.Duration) (runnercontract.HeartbeatResponse, error) {
	req.RunnerID = c.RunnerID()
	req.WaitSeconds = int(wait / time.Second)
	var resp runnercontract.HeartbeatResponse
	err := c.call(ctx, http.MethodPost, "/api/v2/runner/heartbeat", c.token(), req, &resp, wait+30*time.Second)
	return resp, err
}

// Start claims an offered job (the atomic claim, the cap check and, for a run, the state lock),
// reporting what the launcher knows of the placement. ErrClaimLost means skip it.
func (c *Client) Start(ctx context.Context, jobID string, placement *runnercontract.Placement) error {
	err := c.call(ctx, http.MethodPost, "/api/v2/runner/jobs/"+jobID+"/start", c.token(),
		runnercontract.JobStartRequest{RunnerID: c.RunnerID(), Placement: placement}, nil, 30*time.Second)
	var se *StatusError
	if errors.As(err, &se) && se.Code == http.StatusConflict {
		return ErrClaimLost
	}
	return err
}

// JobToken asks for the token the executor of a claimed job runs with, and checks its contract
// version.
func (c *Client) JobToken(ctx context.Context, jobID string) (runnercontract.JobToken, error) {
	var tok runnercontract.JobToken
	if err := c.call(ctx, http.MethodPost, "/api/v2/runner/jobs/"+jobID+"/token", c.token(), nil, &tok, 30*time.Second); err != nil {
		return tok, err
	}
	if err := runnercontract.CheckVersion(tok.ContractVersion); err != nil {
		return tok, fmt.Errorf("refusing the job token: %w", err)
	}
	return tok, nil
}

// Status reads a job's status, which the launcher's supervision uses to stop a placement the API
// canceled.
func (c *Client) Status(ctx context.Context, jobID string) (string, error) {
	var out struct {
		Status string `json:"status"`
	}
	err := c.call(ctx, http.MethodGet, "/api/v2/runner/jobs/"+jobID+"/status", c.token(), nil, &out, 10*time.Second)
	return out.Status, err
}

// Fail reports a job the launcher could not run, or whose placement died, as failed.
func (c *Client) Fail(ctx context.Context, jobID, message string) error {
	return c.FailWith(ctx, jobID, c.token(), message)
}

// FailWith reports a job failed with the job's own token: a runner that declared the job-token
// capability completes a job only with it (the API's JobData guard), so every failure the launcher
// reports for a job it holds a token for goes through here.
func (c *Client) FailWith(ctx context.Context, jobID, token, message string) error {
	if token == "" {
		token = c.token()
	}
	return c.call(ctx, http.MethodPost, "/api/v2/runner/jobs/"+jobID+"/complete", token,
		runnercontract.JobCompleteRequest{RunnerID: c.RunnerID(), Status: "failed", ExitCode: 1, ErrorMessage: message}, nil, 30*time.Second)
}

// ReportPlacement reports what the launcher learnt of a placement after the claim, such as the image
// digest a kubelet pulled, with the job's token.
func (c *Client) ReportPlacement(ctx context.Context, jobID, token string, p runnercontract.Placement) error {
	return c.call(ctx, http.MethodPost, "/api/v2/runner/jobs/"+jobID+"/output", token,
		runnercontract.JobOutputRequest{RunnerID: c.RunnerID(), Placement: &p}, nil, 30*time.Second)
}

// Deregister tells the API the launcher is leaving. It is retried briefly, because a launcher that
// leaves without it is marked offline only by the monitor.
func (c *Client) Deregister(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return c.retry(ctx, func() error {
		return c.call(ctx, http.MethodPost, "/api/v2/runner/deregister", c.token(), map[string]string{"runner_id": c.RunnerID()}, nil, 10*time.Second)
	}, c.logRetry("deregistration"))
}
