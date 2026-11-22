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
	runnercontract.CapabilityClaimRecovery,
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

var defaultBackoff = Backoff{Initial: time.Second, Max: runnercontract.LauncherBackoffMax}

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
	Code      int
	ErrorCode string
	Body      string
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
	_, err := c.callStatus(ctx, method, path, bearer, in, out, timeout, false)
	return err
}

// callStatus also retains received headers when the optional response body is lost.
func (c *Client) callStatus(ctx context.Context, method, path, bearer string, in, out any, timeout time.Duration, completion bool) (int, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return 0, fmt.Errorf("encoding %s: %w", path, err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.serverURL+path, body)
	if err != nil {
		return 0, fmt.Errorf("building %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set(runnercontract.CapabilitiesHeader, strings.Join(capabilities, ","))
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req) //nolint:gosec // G704: the server URL is the operator-configured endpoint
	if err != nil {
		return 0, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if completion && resp.StatusCode == http.StatusOK {
		return resp.StatusCode, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("reading %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		code, _ := runnercontract.DecodeError(data)
		return resp.StatusCode, &StatusError{Code: resp.StatusCode, ErrorCode: code, Body: string(data)}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decoding %s %s: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
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
	err := c.call(ctx, http.MethodPost, "/api/v2/runner/heartbeat", c.token(), req, &resp, wait+runnercontract.HeartbeatRequestOverhead)
	return resp, err
}

// Start claims one original offer and requires the matching enveloped acknowledgement.
func (c *Client) Start(ctx context.Context, offer runnercontract.PendingJob, placement *runnercontract.Placement) error {
	request := runnercontract.JobStartRequest{
		RunnerID: c.RunnerID(), Placement: placement,
		Attempt: offer.Attempt, Phase: offer.Phase, RunType: offer.RunType,
	}
	return c.control(ctx, "start", func(attempt context.Context) error {
		var response struct {
			Meta runnercontract.JobStartResponse `json:"meta"`
		}
		err := c.call(attempt, http.MethodPost, "/api/v2/runner/jobs/"+offer.JobID+"/start", c.token(), request, &response, 0)
		var status *StatusError
		if errors.As(err, &status) && (status.Code == http.StatusConflict || status.Code == http.StatusForbidden) {
			return ErrClaimLost
		}
		if err != nil {
			return err
		}
		if response.Meta.Status != "started" || response.Meta.Attempt != offer.Attempt || response.Meta.Phase != offer.Phase || response.Meta.RunType != offer.RunType {
			return errors.New("start acknowledgement does not match the original offer")
		}
		return nil
	})
}

// JobToken acquires supervision credentials. A supplied identity always binds the original claim.
// The empty form remains for reconciliation of placements created before claim recovery.
func (c *Client) JobToken(ctx context.Context, jobID string, expected ...runnercontract.JobTokenRequest) (runnercontract.JobToken, error) {
	var token runnercontract.JobToken
	var request any
	if len(expected) > 0 {
		request = expected[0]
	}
	err := c.control(ctx, "job token", func(attempt context.Context) error {
		token = runnercontract.JobToken{}
		if err := c.call(attempt, http.MethodPost, "/api/v2/runner/jobs/"+jobID+"/token", c.token(), request, &token, 0); err != nil {
			return err
		}
		if err := runnercontract.CheckVersion(token.ContractVersion); err != nil {
			return fmt.Errorf("refusing the job token: %w", err)
		}
		if len(expected) > 0 {
			claim := expected[0]
			if token.Token == "" || token.JobID != jobID || token.Attempt != claim.Attempt || token.Phase != claim.Phase || token.RunType != claim.RunType {
				return errors.New("job token does not match the original claim")
			}
		}
		return nil
	})
	return token, err
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

// Fail reports a failed startup using the runner key and the original expected claim.
func (c *Client) Fail(ctx context.Context, jobID, message string, expected ...runnercontract.JobTokenRequest) error {
	return c.FailWith(ctx, jobID, c.token(), message, expected...)
}

// FailWith retries the same failure payload. A committed acknowledgement or coded terminal
// refusal retires delivery without replacing an outcome already stored by the API.
func (c *Client) FailWith(ctx context.Context, jobID, token, message string, expected ...runnercontract.JobTokenRequest) error {
	if token == "" {
		token = c.token()
	}
	request := runnercontract.JobCompleteRequest{RunnerID: c.RunnerID(), Status: "failed", ExitCode: 1, ErrorMessage: message}
	if len(expected) > 0 {
		request.Attempt, request.Phase = expected[0].Attempt, expected[0].Phase
	}
	return c.control(ctx, "failure report", func(attempt context.Context) error {
		status, err := c.callStatus(attempt, http.MethodPost, "/api/v2/runner/jobs/"+jobID+"/complete", token, request, nil, 0, true)
		if status == http.StatusOK || terminalRefusal(err) {
			return nil
		}
		if err == nil {
			return &StatusError{Code: status, Body: "completion has no committed acknowledgement"}
		}
		return err
	})
}

// control bounds acquisition or delivery independently of a caller's earlier operation.
func (c *Client) control(parent context.Context, operation string, call func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	for failures := 0; ; failures++ {
		attempt, stop := context.WithTimeout(ctx, 20*time.Second)
		err := call(attempt)
		stop()
		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w", operation, errors.Join(ctx.Err(), err))
		}
		if err == nil || !retryControl(err) {
			return err
		}
		c.logf("%s failed (attempt %d), retrying: %v", operation, failures+1, err)
		// A short capped backoff leaves room to recover near the end of the delivery budget.
		backoff := Backoff{Initial: c.backoff.Initial, Max: min(c.backoff.Max, 5*time.Second)}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %w", operation, errors.Join(ctx.Err(), err))
		case <-time.After(backoff.next(failures)):
		}
	}
}

func retryControl(err error) bool {
	if errors.Is(err, ErrClaimLost) {
		return false
	}
	var status *StatusError
	if !errors.As(err, &status) {
		return true
	}
	switch status.Code {
	case http.StatusUnauthorized:
		return status.ErrorCode == ""
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func terminalRefusal(err error) bool {
	var status *StatusError
	if !errors.As(err, &status) {
		return false
	}
	switch status.Code {
	case http.StatusForbidden:
		return status.ErrorCode == runnercontract.ErrorCodeJobTokenRevoked || status.ErrorCode == runnercontract.ErrorCodeJobTerminal
	case http.StatusConflict:
		return status.ErrorCode == runnercontract.ErrorCodeJobTerminal
	case http.StatusNotFound:
		return status.ErrorCode == runnercontract.ErrorCodeJobDeleted
	}
	return false
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
