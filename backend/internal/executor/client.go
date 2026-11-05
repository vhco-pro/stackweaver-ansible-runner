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
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// ErrJobStopped is the cause a job's context is canceled with when the API says the job is over for
// this executor: canceled, terminal, or its token revoked because a later attempt exists.
var ErrJobStopped = errors.New("the job was stopped")

// capabilities are what this executor announces on every request.
var capabilities = strings.Join([]string{
	runnercontract.CapabilityPlaybookSnapshot,
	runnercontract.CapabilityBlob,
	runnercontract.CapabilityJobToken,
}, ",")

// Client speaks the runner control plane for one job.
type Client struct {
	serverURL string
	token     string
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
	// reported is set once the API has answered the job's completion (ExitCode).
	reported atomic.Bool
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
	req.Header.Set(runnercontract.CapabilitiesHeader, capabilities)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req, nil
}

// do sends a request and returns the body of a 2xx answer, or an error naming the status.
func (c *Client) do(req *http.Request) ([]byte, error) {
	resp, err := c.http.Do(req) //nolint:gosec // G704: the server URL is the operator-configured endpoint
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading %s %s: %w", req.Method, req.URL.Path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		code, detail := errorOf(body)
		return body, &StatusError{Code: resp.StatusCode, ErrorCode: code, Detail: detail, Body: string(body)}
	}
	return body, nil
}

// StatusError is a non-2xx answer.
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
		(e.ErrorCode == runnercontract.ErrorCodeJobTokenRevoked || e.ErrorCode == runnercontract.ErrorCodeJobTerminal)
}

func errorOf(body []byte) (code, detail string) {
	var doc struct {
		Errors []struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &doc) != nil || len(doc.Errors) == 0 {
		return "", ""
	}
	return doc.Errors[0].Code, doc.Errors[0].Detail
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Sending again through a rolling upgrade (#933, D14, AC72). The executor's requests can reach
// API replicas of two releases: a replica that predates job tokens answers a valid one 401, and a
// proxy whose replica is going away answers 502, 503 or 504. Those answers say nothing about the
// job, so the request is sent again, with the same body, until a replica of the new release
// answers, the request's context ends or retryFor passes. Every other answer is final: a coded 403
// stops the job (StatusError.Stops), and any other refusal is the API's verdict on the request.
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

// send sends a request, and sends it again while the answer is retryable.
func (c *Client) send(ctx context.Context, method, path, contentType string, body []byte, header http.Header) ([]byte, error) {
	deadline := time.Now().Add(retryFor)
	wait := retryFirstWait
	for {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := c.newRequest(ctx, method, path, contentType, reader)
		if err != nil {
			return nil, err
		}
		for k, v := range header {
			req.Header[k] = v
		}
		out, err := c.do(req)
		if err == nil || !retryable(err) {
			return out, err
		}
		next, ok := retryWait(ctx, wait, deadline)
		if !ok {
			return out, err
		}
		wait = next
	}
}

func (c *Client) postJSON(ctx context.Context, path string, v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encoding %s: %w", path, err)
	}
	return c.send(ctx, http.MethodPost, path, "application/json", body, nil)
}

func jobPath(jobID, rest string) string {
	return "/api/v2/runner/jobs/" + jobID + rest
}

// Status polls the job's status. stop is true when the job is over for this executor: the API
// reports it canceled, refuses the token with job-token-revoked or job-terminal, or answers 404
// because the job was deleted while it ran (with its job template, or by a playbook force delete),
// and nobody is waiting for it any more. Every other failure is returned as an error to retry.
func (c *Client) Status(ctx context.Context, jobID string) (stop bool, err error) {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := c.newRequest(reqCtx, http.MethodGet, jobPath(jobID, "/status"), "", nil)
	if err != nil {
		return false, err
	}
	body, err := c.do(req)
	if err != nil {
		var se *StatusError
		if errors.As(err, &se) && (se.Stops() || se.Code == http.StatusNotFound) {
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
	body, err := c.send(ctx, http.MethodGet, jobPath(jobID, "/artifacts"), "", nil, nil)
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
	_, err := c.postJSON(ctx, jobPath(jobID, "/output"), req)
	return err
}

// Event records an event the executor writes itself (Ansible preparation events).
func (c *Client) Event(ctx context.Context, jobID string, ev runnercontract.JobEventRequest) error {
	_, err := c.postJSON(ctx, jobPath(jobID, "/events"), ev)
	return err
}

// Complete reports the job's outcome.
func (c *Client) Complete(ctx context.Context, jobID string, req runnercontract.JobCompleteRequest) error {
	req.RunnerID = c.runnerID
	_, err := c.postJSON(ctx, jobPath(jobID, "/complete"), req)
	// The API holds an outcome once it accepted this one, or answers that it already holds one
	// (409) or that the job is gone (404).
	var se *StatusError
	if err == nil || errors.As(err, &se) && (se.Code == http.StatusConflict || se.Code == http.StatusNotFound) {
		c.reported.Store(true)
	}
	return err
}

// ExitCode is the executor's exit code for a job that ended with status: its outcome once the API
// has its completion, ExitUnreported while it has none, so the launcher reports the job instead of
// leaving it running. A stop exits as a stop either way: the job is over and nobody reports it.
func (c *Client) ExitCode(status string) int {
	if status != "canceled" && !c.reported.Load() {
		return ExitUnreported
	}
	return ExitCode(status)
}

func (c *Client) postBytes(ctx context.Context, path string, data []byte) error {
	_, err := c.send(ctx, http.MethodPost, path, "application/octet-stream", data, nil)
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
	if _, err := c.send(ctx, http.MethodPost, jobPath(jobID, "/snapshot"), "application/gzip", data, header); err != nil {
		return fmt.Errorf("uploading the snapshot: %w", err)
	}
	return nil
}

// Get fetches an API path, such as a plan descriptor's or a playbook snapshot's.
func (c *Client) Get(ctx context.Context, path string) ([]byte, error) {
	return c.send(ctx, http.MethodGet, path, "", nil, nil)
}

// Blob fetches an artifact too large to inline and checks it against its sha256 before returning
// it: the executor trusts neither the transport nor the store.
func (c *Client) Blob(ctx context.Context, ref runnercontract.BlobRef) ([]byte, error) {
	data, err := c.Get(ctx, ref.Path)
	if err != nil {
		return nil, fmt.Errorf("fetching blob %s: %w", ref.Path, err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); ref.SHA256 != "" && got != ref.SHA256 {
		return nil, fmt.Errorf("blob %s has sha256 %s, want %s", ref.Path, got, ref.SHA256)
	}
	return data, nil
}

// Download streams an API path, such as a playbook snapshot, to dest and refuses a body larger than
// maxBytes. It has no overall timeout of its own, because the control-plane client's would cut a
// large body short; ctx bounds it instead.
func (c *Client) Download(ctx context.Context, path, dest string, maxBytes int64) error {
	resp, err := c.openDownload(ctx, path)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // G304: dest is inside the job's own work directory
	if err != nil {
		return fmt.Errorf("creating %s: %w", dest, err)
	}
	// One byte past the cap, so a body exactly at the cap is told apart from one cut off by it.
	written, err := io.Copy(out, io.LimitReader(resp.Body, maxBytes+1))
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("writing %s: %w", dest, err)
	}
	if written > maxBytes {
		return fmt.Errorf("%s is larger than the %d byte limit", path, maxBytes)
	}
	return nil
}

// openDownload requests path for Download and returns the 200 answer, sending the request again
// while the answer is retryable.
func (c *Client) openDownload(ctx context.Context, path string) (*http.Response, error) {
	dl := &http.Client{Transport: c.http.Transport}
	deadline := time.Now().Add(retryFor)
	wait := retryFirstWait
	for {
		req, err := c.newRequest(ctx, http.MethodGet, path, "", nil)
		if err != nil {
			return nil, err
		}
		resp, err := dl.Do(req) //nolint:gosec // G704: the server URL is the operator-configured endpoint
		if err != nil {
			return nil, fmt.Errorf("requesting %s: %w", path, err)
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		code, detail := errorOf(body)
		err = &StatusError{Code: resp.StatusCode, ErrorCode: code, Detail: detail, Body: strings.TrimSpace(string(body))}
		if !retryable(err) {
			return nil, err
		}
		next, ok := retryWait(ctx, wait, deadline)
		if !ok {
			return nil, err
		}
		wait = next
	}
}

// ReportRefusal writes why the executor refuses its job into the job's output, so the run or job
// shows the reason beside its failure (AC72). A refusal over the contract version names the fix.
func (c *Client) ReportRefusal(ctx context.Context, jobID, stream string, err error) {
	line := "Stackweaver executor: refusing this job: " + err.Error() + "\n"
	if errors.Is(err, runnercontract.ErrUnknownVersion) {
		line = fmt.Sprintf("Stackweaver executor: refusing this job: %v. The API speaks a runner contract this runner image does not; upgrade the runner image to the API's release.\n", err)
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
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_ = c.Complete(reportCtx, jobID, runnercontract.JobCompleteRequest{Status: "failed", ExitCode: 1, ErrorMessage: err.Error()})
	return false
}
