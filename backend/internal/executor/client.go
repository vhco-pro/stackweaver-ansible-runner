// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// Package executor runs exactly one job (#933, D1, D2). It is given the server URL, the job id and
// a token valid for that job alone, fetches the job's artifacts itself, runs the engine with an
// allowlisted environment, streams output, uploads the plan, the state and the completion, and
// returns the job's outcome. The launcher calls it in its own process for the in-process driver and
// runs it as the `executor` subcommand of the runner binaries inside a container or pod for every
// other driver; either way it is the same code. Decrypted credentials travel API to executor only.
package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/michielvha/stackweaver/backend/internal/launcher/readbody"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// ErrJobStopped is the cause a job's context is canceled with when the API says the job is over for
// this executor: canceled, deleted, terminal, or its token revoked because a later attempt exists.
var ErrJobStopped = errors.New("the job was stopped")

var errArtifactSequenceExhausted = errors.New("artifact request sequence exhausted")

// capabilities are what this executor announces on every request.
var capabilities = strings.Join([]string{
	runnercontract.CapabilityPlaybookSnapshot,
	runnercontract.CapabilityBlob,
	runnercontract.CapabilityJobToken,
	runnercontract.CapabilityClaimRecovery,
}, ",")

// Client speaks the runner control plane for one job.
type Client struct {
	verifiedPlaybookSnapshots atomic.Bool
	serverURL                 string
	token                     string
	// runnerID fills the runner_id member several request bodies still carry; the API trusts the
	// token, not the body.
	runnerID string
	http     *http.Client
	// placed is reported with the first output: where the executor runs (AC77). placedBy is the
	// placement a driver handed the executor in-process, which wins over the environment's.
	placedOnce sync.Once
	placedBy   *runnercontract.Placement
	// limitsReport is the in-process driver's limits report; a placed executor reads its own from
	// EnvLimitsReport.
	limitsReport string
	// reported records received committed-completion headers. retired records a terminal
	// disposition independently, including revocation after an acknowledgement was lost.
	reported         atomic.Bool
	retired          atomic.Bool
	sequenceMu       sync.Mutex
	artifactSequence int64
}

// EnvPlaced is set by a driver that places the executor in a container or pod: the executor then
// reports its hostname (the container id or the pod name) and STACKWEAVER_NODE_NAME as the job's
// placement with its first output.
const EnvPlaced = "STACKWEAVER_PLACED"

// placement is what the executor knows of where it runs, or nil in-process.
func placement() *runnercontract.Placement {
	if os.Getenv(EnvPlaced) != "1" {
		return nil
	}
	host, _ := os.Hostname()
	return &runnercontract.Placement{PlacementID: host, Node: os.Getenv(EnvNodeName), ImageDigest: os.Getenv(EnvImageDigest)}
}

// EnvNodeName and EnvImageDigest are set by a driver that places the executor: the node or host
// the job runs on and the digest of the image it runs (#933, D4, AC77).
const (
	EnvNodeName    = "STACKWEAVER_NODE_NAME"
	EnvImageDigest = "STACKWEAVER_IMAGE_DIGEST"
)

// SetPlacement sets the placement the executor reports with its first output, for a driver that
// knows it better than the environment does (the in-process driver).
func (c *Client) SetPlacement(p *runnercontract.Placement) { c.placedBy = p }

// EnvLimitsReport is set by a driver that places the executor: which of the pool's limits it
// enforced and which it could not (#933, D4, AC24). The driver sets the same name
// (driver.EnvLimitsReport).
const EnvLimitsReport = "STACKWEAVER_LIMITS_REPORT"

// SetLimitsReport sets the limits report the executor writes before the job, for the in-process
// driver, which hands it over in memory.
func (c *Client) SetLimitsReport(report string) { c.limitsReport = report }

// NewClient builds a client for serverURL authenticating with token (a job token, or the runner
// token of an agent that predates job tokens).
func NewClient(serverURL, token, runnerID string) *Client {
	return &Client{
		serverURL: strings.TrimRight(serverURL, "/"),
		token:     token,
		runnerID:  runnerID,
		http:      &http.Client{Timeout: 5 * time.Minute},
	}
}

// RunnerID is the runner the client reports for.
func (c *Client) RunnerID() string { return c.runnerID }

func (c *Client) newRequest(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.serverURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("building %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	announced := capabilities
	if c.verifiedPlaybookSnapshots.Load() {
		announced += "," + runnercontract.CapabilityVerifiedPlaybookSnapshot
	}
	req.Header.Set(runnercontract.CapabilitiesHeader, announced)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req, nil
}

// requestPolicy keeps replay-safe control requests separate from excluded writes.
type requestPolicy int

const (
	writeRequest requestPolicy = iota
	readRequest
	artifactRequest
	completionRequest
	cacheRequest
)

// ControlRetryBudget bounds delivery of an idempotent control operation.
const ControlRetryBudget = 2 * time.Minute

const controlRequestTimeout = 20 * time.Second

const requiredReadBudget = 5 * time.Minute

// do returns response headers even when the response refuses the request. Completion only
// needs committed HTTP 200 headers, so a lost optional body cannot erase the acknowledgement.
func (c *Client) do(req *http.Request, policy requestPolicy) ([]byte, http.Header, error) {
	resp, err := c.http.Do(req) //nolint:gosec // G704: the server URL is the operator-configured endpoint
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if policy == completionRequest && resp.StatusCode == http.StatusOK {
		return nil, resp.Header, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.Header, fmt.Errorf("reading %s %s: %w", req.Method, req.URL.Path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 || policy == completionRequest {
		code, detail := runnercontract.DecodeError(body)
		return body, resp.Header, &StatusError{Code: resp.StatusCode, ErrorCode: code, Detail: detail, Body: string(body)}
	}
	return body, resp.Header, nil
}

// artifactHTTPClient keeps each physical artifact request under sequence control. Standard
// transports use a fresh HTTP/1 connection so neither reused-connection nor HTTP/2 stream
// recovery can silently replay a request. Redirects retain their policy and get a fresh sequence.
func (c *Client) artifactHTTPClient() (*http.Client, func()) {
	client := *c.http
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	closeTransport := func() {}
	if standard, ok := transport.(*http.Transport); ok {
		isolated := standard.Clone()
		isolated.DisableKeepAlives = true
		isolated.Protocols = new(http.Protocols)
		isolated.Protocols.SetHTTP1(true)
		if isolated.TLSClientConfig != nil {
			isolated.TLSClientConfig.NextProtos = []string{"http/1.1"}
		}
		transport = isolated
		closeTransport = isolated.CloseIdleConnections
	}
	client.Transport = transport
	redirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if redirect != nil {
			if err := redirect(req, via); err != nil {
				return err
			}
		} else if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		sequence, err := c.nextArtifactSequence(0)
		if err != nil {
			return err
		}
		req.Header.Set(runnercontract.ArtifactSequenceHeader, strconv.FormatInt(sequence, 10))
		return nil
	}
	return &client, closeTransport
}

// StatusError is an HTTP answer that does not satisfy the operation's response contract.
type StatusError struct {
	Code      int
	ErrorCode string
	// Detail is the API's own sentence for a person, when the answer carries one.
	Detail string
	Body   string
}

func (e *StatusError) Error() string {
	if e.ErrorCode != "" {
		return fmt.Sprintf("status %d (%s)", e.Code, e.ErrorCode)
	}
	if e.Detail != "" {
		return fmt.Sprintf("status %d: %s", e.Code, e.Detail)
	}
	return fmt.Sprintf("status %d: %s", e.Code, truncate(e.Body, 300))
}

// Stops reports whether the answer means the job is over for this executor.
func (e *StatusError) Stops() bool {
	return e.Code == http.StatusForbidden &&
		(e.ErrorCode == runnercontract.ErrorCodeJobTokenRevoked || e.ErrorCode == runnercontract.ErrorCodeJobTerminal) ||
		e.Code == http.StatusNotFound && e.ErrorCode == runnercontract.ErrorCodeJobDeleted
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Excluded writes retain the rolling-upgrade retry policy for 401, 502, 503 and 504.
// Replay-safe control requests additionally use the bounded policy below.
var (
	retryFirstWait = 100 * time.Millisecond
	retryMaxWait   = 5 * time.Second
	retryFor       = 2 * time.Minute
)

// retryable reports whether err is an answer of a rolling upgrade rather than of the API.
func retryable(err error) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code {
	case http.StatusUnauthorized, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// jittered is a wait drawn between half of wait and all of it. Executors that start retrying
// together (every job of a launcher, when a replica restarts) would otherwise send in step, and
// behind a round-robin balancer the same ones could keep reaching the replica of the old release.
func jittered(wait time.Duration) time.Duration {
	if wait <= 1 {
		return wait
	}
	return wait/2 + rand.N(wait/2+1) //nolint:gosec // G404: retry jitter, nothing secret depends on it
}

// retryWait waits before the next attempt and returns the wait after it, or false when ctx ends or
// the attempt would start past deadline.
func retryWait(ctx context.Context, wait time.Duration, deadline time.Time) (time.Duration, bool) {
	if time.Now().Add(wait).After(deadline) {
		return wait, false
	}
	t := time.NewTimer(jittered(wait))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return wait, false
	case <-t.C:
	}
	return min(wait*2, retryMaxWait), true
}

// controlRetryable includes interrupted responses and uncoded authentication failures.
func controlRetryable(err error) bool {
	if errors.Is(err, errArtifactSequenceExhausted) {
		return false
	}
	var status *StatusError
	if !errors.As(err, &status) {
		return true
	}
	return status.Code == http.StatusUnauthorized && status.ErrorCode == "" ||
		status.Code == http.StatusInternalServerError ||
		status.Code == http.StatusBadGateway || status.Code == http.StatusServiceUnavailable ||
		status.Code == http.StatusGatewayTimeout
}

func (c *Client) recordRetirement(err error, policy requestPolicy) {
	var status *StatusError
	if errors.As(err, &status) && (status.Stops() ||
		policy == completionRequest && status.Code == http.StatusConflict && status.ErrorCode == runnercontract.ErrorCodeJobTerminal) {
		c.retired.Store(true)
	}
}

// nextArtifactSequence also adopts an authorized superseded response's sequence floor.
func (c *Client) nextArtifactSequence(latest int64) (int64, error) {
	c.sequenceMu.Lock()
	defer c.sequenceMu.Unlock()
	c.artifactSequence = max(c.artifactSequence, latest)
	if c.artifactSequence == math.MaxInt64 {
		return 0, errArtifactSequenceExhausted
	}
	c.artifactSequence++
	return c.artifactSequence, nil
}

func controlWait(ctx context.Context, wait time.Duration, deadline time.Time) (time.Duration, bool) {
	remaining := time.Until(deadline)
	if remaining <= 0 || ctx.Err() != nil {
		return wait, false
	}
	timer := time.NewTimer(min(jittered(wait), remaining))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return wait, false
	case <-timer.C:
		return min(wait*2, retryMaxWait), time.Now().Before(deadline) && ctx.Err() == nil
	}
}

// send retains the write policy and bounds every replay-safe control attempt separately.
func (c *Client) send(ctx context.Context, policy requestPolicy, method, path, contentType string, body []byte, header http.Header) ([]byte, error) {
	if policy == readRequest || policy == artifactRequest {
		return c.readContent(ctx, path, policy, header, -1)
	}
	deadline := time.Now().Add(retryFor)
	if policy != writeRequest {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ControlRetryBudget)
		defer cancel()
		deadline, _ = ctx.Deadline()
	}
	wait := retryFirstWait
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		requestCtx := ctx
		cancel := func() {}
		if policy != writeRequest {
			requestCtx, cancel = context.WithTimeout(ctx, controlRequestTimeout)
		}
		req, err := c.newRequest(requestCtx, method, path, contentType, reader)
		if err != nil {
			cancel()
			return nil, err
		}
		for k, v := range header {
			req.Header[k] = v
		}
		out, _, err := c.do(req, policy)
		cancel()
		if err == nil {
			return out, nil
		}
		shouldRetry := retryable(err)
		waitAgain := retryWait
		if policy != writeRequest {
			c.recordRetirement(err, policy)
			shouldRetry = controlRetryable(err)
			waitAgain = controlWait
		}
		if !shouldRetry {
			return out, err
		}
		next, ok := waitAgain(ctx, wait, deadline)
		if !ok {
			return out, err
		}
		wait = next
	}
}

func (c *Client) postJSON(ctx context.Context, policy requestPolicy, path string, v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encoding %s: %w", path, err)
	}
	return c.send(ctx, policy, http.MethodPost, path, "application/json", body, nil)
}

func jobPath(jobID, rest string) string {
	return "/api/v2/runner/jobs/" + jobID + rest
}

// Status polls the job's status. stop is true when the job is over for this executor: the API
// reports it canceled, refuses the token with job-token-revoked or job-terminal, or answers 404
// with job-deleted. Every other failure is returned as an error to retry.
func (c *Client) Status(ctx context.Context, jobID string) (stop bool, err error) {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := c.newRequest(reqCtx, http.MethodGet, jobPath(jobID, "/status"), "", nil)
	if err != nil {
		return false, err
	}
	body, _, err := c.do(req, readRequest)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) && se.Stops() {
			return true, nil
		}
		return false, err
	}
	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return false, fmt.Errorf("decoding the job status: %w", err)
	}
	return out.Status == "canceled", nil
}

// WatchCancellation polls the job's status every interval until ctx ends, and cancels the job with
// ErrJobStopped once the API says it is over. Errors are retried: the job's own timeout bounds how
// long an executor runs without an answer.
func (c *Client) WatchCancellation(ctx context.Context, jobID string, interval time.Duration, cancel context.CancelCauseFunc) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if stop, err := c.Status(ctx, jobID); err == nil && stop {
				cancel(ErrJobStopped)
				return
			}
		}
	}
}

// Artifacts fetches the job's artifacts into out and checks their contract version.
func (c *Client) Artifacts(ctx context.Context, jobID string, out any, version func() int) error {
	body, err := c.send(ctx, artifactRequest, http.MethodGet, jobPath(jobID, "/artifacts"), "", nil, nil)
	if err != nil {
		return fmt.Errorf("fetching artifacts: %w", err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding artifacts: %w", err)
	}
	if version != nil {
		if err := runnercontract.CheckVersion(version()); err != nil {
			return fmt.Errorf("refusing the job's artifacts: %w", err)
		}
	}
	return nil
}

// Output streams output lines, a typed event, or both.
func (c *Client) Output(ctx context.Context, jobID string, req runnercontract.JobOutputRequest) error {
	req.RunnerID = c.runnerID
	c.placedOnce.Do(func() {
		req.Placement = c.placedBy
		if req.Placement == nil {
			req.Placement = placement()
		}
	})
	_, err := c.postJSON(ctx, writeRequest, jobPath(jobID, "/output"), req)
	return err
}

// Event records an event the executor writes itself (Ansible preparation events).
func (c *Client) Event(ctx context.Context, jobID string, ev runnercontract.JobEventRequest) error {
	_, err := c.postJSON(ctx, writeRequest, jobPath(jobID, "/events"), ev)
	return err
}

// Complete reports the job's outcome.
func (c *Client) Complete(ctx context.Context, jobID string, req runnercontract.JobCompleteRequest) error {
	req.RunnerID = c.runnerID
	_, err := c.postJSON(ctx, completionRequest, jobPath(jobID, "/complete"), req)
	if err == nil {
		c.reported.Store(true)
	}
	return err
}

// ExitCode returns the outcome after confirmed delivery, ExitStopped after a terminal
// disposition, or ExitUnreported so the launcher can recover a missing completion.
// Cancellation also retains the existing stopped exit.
func (c *Client) ExitCode(status string) int {
	if c.retired.Load() {
		return ExitStopped
	}
	if status != "canceled" && !c.reported.Load() {
		return ExitUnreported
	}
	return ExitCode(status)
}

func (c *Client) postBytes(ctx context.Context, path string, data []byte) error {
	_, err := c.send(ctx, writeRequest, http.MethodPost, path, "application/octet-stream", data, nil)
	return err
}

// UploadState uploads a state file as a raw body.
func (c *Client) UploadState(ctx context.Context, jobID string, state []byte) error {
	return c.postBytes(ctx, jobPath(jobID, "/state"), state)
}

// UploadPlan uploads the plan phase's saved plan.
func (c *Client) UploadPlan(ctx context.Context, jobID string, plan []byte) error {
	return c.postBytes(ctx, jobPath(jobID, "/plan"), plan)
}

// UploadSnapshot uploads the snapshot a work item captured, with the commit it was captured at
// (#933, D2).
func (c *Client) UploadSnapshot(ctx context.Context, jobID, commit string, data []byte) error {
	header := http.Header{}
	header.Set(runnercontract.HeaderSnapshotCommit, commit)
	if _, err := c.send(ctx, writeRequest, http.MethodPost, jobPath(jobID, "/snapshot"), "application/gzip", data, header); err != nil {
		return fmt.Errorf("uploading the snapshot: %w", err)
	}
	return nil
}

// Get fetches an API path, such as a plan descriptor's or a playbook snapshot's.
func (c *Client) Get(ctx context.Context, path string) ([]byte, error) {
	return c.send(ctx, readRequest, http.MethodGet, path, "", nil, nil)
}

// Blob fetches an artifact too large to inline and checks it against its sha256 before returning
// it: the executor trusts neither the transport nor the store.
func (c *Client) Blob(ctx context.Context, ref runnercontract.BlobRef) ([]byte, error) {
	header := make(http.Header)
	expectedSize := int64(-1)
	if ref.SHA256 != "" {
		digest, err := hex.DecodeString(ref.SHA256)
		if err != nil || len(digest) != sha256.Size || ref.SHA256 != strings.ToLower(ref.SHA256) || ref.SizeBytes < 0 {
			return nil, errors.New("blob reference has an invalid digest or size")
		}
		header.Set("If-Match", "\""+ref.SHA256+"\"")
		expectedSize = ref.SizeBytes
	}
	data, err := c.readContent(ctx, ref.Path, readRequest, header, expectedSize)
	if err != nil {
		return nil, fmt.Errorf("fetching blob %s: %w", ref.Path, err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); ref.SHA256 != "" && got != ref.SHA256 {
		return nil, fmt.Errorf("blob %s has sha256 %s, want %s", ref.Path, got, ref.SHA256)
	}
	return data, nil
}

// Download streams required content to dest, bounded by maxBytes and the job context.
// Acquisition retries share five minutes. A successful body can outlive that allowance.
func (c *Client) Download(ctx context.Context, path, dest string, maxBytes int64) error {
	return c.download(ctx, path, dest, maxBytes, readRequest, nil)
}

// ErrDownloadContentChanged means that the complete body differs from the assigned artifact.
var ErrDownloadContentChanged = errors.New("download content changed")

// RequireVerifiedPlaybookSnapshots opts the Ansible job image into snapshot validation.
func (c *Client) RequireVerifiedPlaybookSnapshots() { c.verifiedPlaybookSnapshots.Store(true) }

type downloadVerification struct {
	runnercontract.BlobRef
	validator string
}

// DownloadVerified streams an assigned artifact to disk with one validator and acquisition deadline.
func (c *Client) DownloadVerified(ctx context.Context, ref runnercontract.BlobRef, validator, dest string, maxBytes int64) error {
	digest, err := hex.DecodeString(ref.SHA256)
	if err != nil || len(digest) != sha256.Size || ref.SHA256 != strings.ToLower(ref.SHA256) || ref.SizeBytes <= 0 || ref.SizeBytes > maxBytes ||
		len(validator) < 3 || strings.Count(validator, "\"") != 2 || validator[0] != '"' || validator[len(validator)-1] != '"' {
		return errors.New("download reference has an invalid digest or size")
	}
	return c.download(ctx, ref.Path, dest, ref.SizeBytes, readRequest, &downloadVerification{BlobRef: ref, validator: validator})
}

func acquisitionDeadline(ctx context.Context, budget time.Duration) time.Time {
	deadline := time.Now().Add(budget)
	if parent, ok := ctx.Deadline(); ok && parent.Before(deadline) {
		return parent
	}
	return deadline
}

func acquisitionError(ctx context.Context, path string, last error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("acquiring %s: %w", path, errors.Join(context.DeadlineExceeded, last))
}

// openContent retries acquisition against one deadline. Required reads may spend the remaining
// allowance in storage or hashing before headers. Optional cache attempts retain twenty seconds.
func (c *Client) openContent(ctx context.Context, path string, policy requestPolicy, header http.Header, deadline time.Time) (*http.Response, error) {
	wait := retryFirstWait
	var latest int64
	var last error
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, acquisitionError(ctx, path, last)
		}
		requestCtx, cancel := context.WithCancel(ctx)
		req, err := c.newRequest(requestCtx, http.MethodGet, path, "", nil)
		if err != nil {
			cancel()
			return nil, err
		}
		for key, values := range header {
			req.Header[key] = values
		}
		if policy == artifactRequest {
			sequence, err := c.nextArtifactSequence(latest)
			if err != nil {
				cancel()
				return nil, err
			}
			req.Header.Set(runnercontract.ArtifactSequenceHeader, strconv.FormatInt(sequence, 10))
		}
		client := *c.http
		closeTransport := func() {}
		if policy == artifactRequest {
			isolated, closeIsolated := c.artifactHTTPClient()
			client = *isolated
			closeTransport = closeIsolated
		}
		client.Timeout = 0
		allowance := remaining
		if policy == cacheRequest {
			allowance = min(controlRequestTimeout, remaining)
		}
		timedOut := make(chan struct{})
		timer := time.AfterFunc(allowance, func() { cancel(); close(timedOut) })
		resp, err := client.Do(req) //nolint:gosec // G704: the server URL is the operator-configured endpoint
		if err == nil && (resp.StatusCode < 200 || resp.StatusCode > 299) {
			// A complete error document preserves explicit refusal codes with long details.
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			code, detail := runnercontract.DecodeError(body)
			err = &StatusError{Code: resp.StatusCode, ErrorCode: code, Detail: detail, Body: strings.TrimSpace(string(body))}
			if readErr != nil {
				err = fmt.Errorf("reading %s: %w", path, readErr)
			}
		}
		if !timer.Stop() {
			<-timedOut
		}
		if err == nil && requestCtx.Err() == nil {
			resp.Body = readbody.New(requestCtx, resp.Body, func() { cancel(); closeTransport() })
			return resp, nil
		}
		if err == nil {
			_ = resp.Body.Close()
			err = requestCtx.Err()
		}
		cancel()
		closeTransport()
		c.recordRetirement(err, policy)
		shouldRetry := controlRetryable(err)
		var status *StatusError
		if policy == artifactRequest && errors.As(err, &status) && status.Code == http.StatusConflict && status.ErrorCode == runnercontract.ErrorCodeArtifactReadSuperseded {
			parsed, parseErr := strconv.ParseInt(resp.Header.Get(runnercontract.ArtifactSequenceHeader), 10, 64)
			if parseErr != nil || parsed <= 0 {
				return nil, errors.New("superseded artifact response has an invalid sequence")
			}
			latest = parsed
			shouldRetry = true
		}
		if !shouldRetry {
			return nil, err
		}
		last = err
		next, ok := controlWait(ctx, wait, deadline)
		if !ok {
			return nil, acquisitionError(ctx, path, last)
		}
		wait = next
	}
}

// readContent retries interrupted bodies against the original acquisition deadline. A nonnegative
// expectedSize also requires a complete body of exactly that size before a blob can be used.
func (c *Client) readContent(ctx context.Context, path string, policy requestPolicy, header http.Header, expectedSize int64) ([]byte, error) {
	deadline := acquisitionDeadline(ctx, requiredReadBudget)
	wait := retryFirstWait
	for {
		resp, err := c.openContent(ctx, path, policy, header, deadline)
		if err != nil {
			return nil, err
		}
		var reader io.Reader = resp.Body
		if expectedSize >= 0 && expectedSize < math.MaxInt64 {
			reader = io.LimitReader(reader, expectedSize+1)
		}
		data, err := io.ReadAll(reader)
		_ = resp.Body.Close()
		if err == nil && expectedSize >= 0 {
			if int64(len(data)) > expectedSize {
				return nil, fmt.Errorf("%s exceeds its %d byte reference", path, expectedSize)
			}
			if int64(len(data)) < expectedSize {
				err = io.ErrUnexpectedEOF
			}
		}
		if err == nil {
			return data, nil
		}
		next, ok := controlWait(ctx, wait, deadline)
		if !ok {
			return nil, acquisitionError(ctx, path, err)
		}
		wait = next
	}
}

func (c *Client) download(ctx context.Context, path, dest string, maxBytes int64, policy requestPolicy, ref *downloadVerification) error {
	if maxBytes < 0 {
		return errors.New("download size limit must be nonnegative")
	}
	budget := requiredReadBudget
	if policy == cacheRequest {
		budget = ControlRetryBudget
	}
	deadline := acquisitionDeadline(ctx, budget)
	wait := retryFirstWait
	var header http.Header
	if ref != nil {
		header = http.Header{"If-Match": {ref.validator}}
	}
	for {
		resp, err := c.openContent(ctx, path, policy, header, deadline)
		if err != nil {
			return err
		}
		out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // G304: dest is inside the job's own work directory
		if err != nil {
			_ = resp.Body.Close()
			return fmt.Errorf("creating %s: %w", dest, err)
		}
		var writer io.Writer = out
		digest := sha256.New()
		if ref != nil {
			writer = io.MultiWriter(out, digest)
		}
		written, copyErr := io.Copy(writer, io.LimitReader(resp.Body, min(maxBytes, math.MaxInt64-1)+1))
		closeErr := out.Close()
		_ = resp.Body.Close()
		if ref != nil && copyErr == nil {
			switch {
			case written < ref.SizeBytes:
				copyErr = io.ErrUnexpectedEOF
			case written > ref.SizeBytes || hex.EncodeToString(digest.Sum(nil)) != ref.SHA256:
				_ = os.Remove(dest)
				return ErrDownloadContentChanged
			}
		}
		if copyErr == nil && closeErr == nil && written <= maxBytes {
			return nil
		}
		_ = os.Remove(dest)
		if written > maxBytes {
			return fmt.Errorf("%s is larger than the %d byte limit", path, maxBytes)
		}
		if closeErr != nil {
			return fmt.Errorf("closing %s: %w", dest, closeErr)
		}
		var fileErr *os.PathError
		if errors.As(copyErr, &fileErr) {
			return fmt.Errorf("writing %s: %w", dest, copyErr)
		}
		next, ok := controlWait(ctx, wait, deadline)
		if !ok {
			return acquisitionError(ctx, path, copyErr)
		}
		wait = next
	}
}

// ReportRefusal writes why the executor refuses its job into the job's output, so the run or job
// shows the reason beside its failure (AC72). A refusal over the contract version names the fix.
func (c *Client) ReportRefusal(ctx context.Context, jobID, stream string, err error) {
	line := "Stackweaver: refusing this job: " + err.Error() + "\n"
	if errors.Is(err, runnercontract.ErrUnknownVersion) {
		line = fmt.Sprintf("Stackweaver: refusing this job: %v. The API speaks a job contract this job image does not; upgrade the job image to the API's release.\n", err)
	}
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = c.Output(reportCtx, jobID, runnercontract.JobOutputRequest{Output: line, Stream: stream})
}

// Preflight runs what an executor checks before it starts a job, and on a refusal reports it as
// the job's output and failure. It reports whether the job may run.
func (c *Client) Preflight(ctx context.Context, jobID, stream string) bool {
	err := GuardNetwork(ctx)
	if err == nil {
		report := c.limitsReport
		if report == "" {
			report = os.Getenv(EnvLimitsReport)
		}
		if report != "" {
			_ = c.Output(ctx, jobID, runnercontract.JobOutputRequest{Output: "Stackweaver: " + report + "\n", Stream: stream})
		}
		return true
	}
	c.ReportRefusal(ctx, jobID, stream, err)
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ControlRetryBudget)
	defer cancel()
	_ = c.Complete(reportCtx, jobID, runnercontract.JobCompleteRequest{Status: "failed", ExitCode: 1, ErrorMessage: err.Error()})
	return false
}
