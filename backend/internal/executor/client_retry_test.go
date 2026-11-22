// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// During a rolling upgrade the executor's requests reach API replicas of two releases (#933, D14,
// AC72). A replica that predates job tokens answers a valid one 401, and a proxy whose replica is
// going away answers 502, 503 or 504; the executor sends the same request again rather than lose
// its output, its plan, its state or its completion. A coded 403 stops the job, and every other
// answer is final, as before.

// flakyAPI answers the first len(first) requests with those statuses and every later one with
// 200, recording each request body.
type flakyAPI struct {
	mu     sync.Mutex
	first  []int
	bodies []string
	srv    *httptest.Server
}

func newFlakyAPI(t *testing.T, first ...int) *flakyAPI {
	t.Helper()
	f := &flakyAPI{first: first}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(body))
		n := len(f.bodies)
		f.mu.Unlock()
		if n <= len(f.first) {
			w.WriteHeader(f.first[n-1])
			_, _ = w.Write([]byte(`{"errors":[{"status":"` + http.StatusText(f.first[n-1]) + `"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","contract_version":1}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *flakyAPI) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

// fastRetry shortens the waits between attempts for the test.
func fastRetry(t *testing.T) {
	t.Helper()
	first, maxWait := retryFirstWait, retryMaxWait
	retryFirstWait, retryMaxWait = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { retryFirstWait, retryMaxWait = first, maxWait })
}

func TestClient_SendsAgainThroughAnUpgrade(t *testing.T) {
	fastRetry(t)
	calls := map[string]func(c *Client) error{
		"output": func(c *Client) error {
			return c.Output(t.Context(), "run-1", runnercontract.JobOutputRequest{Output: "line\n", Stream: "plan"})
		},
		"event": func(c *Client) error {
			return c.Event(t.Context(), "job-1", runnercontract.JobEventRequest{Event: "runner_on_ok", Task: "task-1", Stdout: "done"})
		},
		"complete": func(c *Client) error {
			return c.Complete(t.Context(), "run-1", runnercontract.JobCompleteRequest{Status: "completed"})
		},
		"plan":     func(c *Client) error { return c.UploadPlan(t.Context(), "run-1", []byte("PK plan")) },
		"state":    func(c *Client) error { return c.UploadState(t.Context(), "run-1", []byte(`{"version":4}`)) },
		"snapshot": func(c *Client) error { return c.UploadSnapshot(t.Context(), "wi-1", "abc", []byte("tarball")) },
		"artifacts": func(c *Client) error {
			var arts runnercontract.TofuArtifacts
			return c.Artifacts(t.Context(), "run-1", &arts, func() int { return arts.ContractVersion })
		},
		"get": func(c *Client) error { _, err := c.Get(t.Context(), "/api/v2/runner/jobs/run-1/plan"); return err },
		"download": func(c *Client) error {
			return c.Download(t.Context(), "/api/v2/runner/jobs/1/playbook-snapshot", filepath.Join(t.TempDir(), "snap"), 1<<20)
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			api := newFlakyAPI(t, http.StatusUnauthorized, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusUnauthorized)
			if err := call(NewClient(api.srv.URL, "swj-token", "runner-1")); err != nil {
				t.Fatalf("%s through an upgrade: %v, want it delivered once a replica of the new release answers", name, err)
			}
			got := api.requests()
			if len(got) != 6 {
				t.Fatalf("%s was sent %d times, want 6: five refusals of an upgrade, then the delivery", name, len(got))
			}
			for i, b := range got {
				if b != got[0] {
					t.Errorf("attempt %d sent %q, want the same body as the first, %q", i+1, b, got[0])
				}
			}
		})
	}
}

func TestClient_AFinalAnswerIsNotSentAgain(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		wantStop bool
	}{
		{"job-token-revoked", http.StatusForbidden, `{"errors":[{"status":"403","code":"job-token-revoked"}]}`, true},
		{"job-terminal", http.StatusForbidden, `{"errors":[{"status":"403","code":"job-terminal"}]}`, true},
		{"a bare 403", http.StatusForbidden, `{"errors":[{"status":"403"}]}`, false},
		{"a conflict", http.StatusConflict, `{"errors":[{"status":"409"}]}`, false},
		{"too large", http.StatusRequestEntityTooLarge, `{"errors":[{"status":"413"}]}`, false},
		{"not found", http.StatusNotFound, `{"errors":[{"status":"404"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var n int
			var mu sync.Mutex
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				n++
				mu.Unlock()
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			err := NewClient(srv.URL, "swj-token", "runner-1").Complete(t.Context(), "run-1", runnercontract.JobCompleteRequest{Status: "completed"})
			var se *StatusError
			if !errors.As(err, &se) || se.Code != tc.status || se.Stops() != tc.wantStop {
				t.Fatalf("Complete = %v, want status %d (stops %t)", err, tc.status, tc.wantStop)
			}
			mu.Lock()
			defer mu.Unlock()
			if n != 1 {
				t.Errorf("sent %d times, want once: %s is the API's answer", n, tc.name)
			}
		})
	}
}

func TestClient_StopsSendingAgainWhenItsContextEnds(t *testing.T) {
	fastRetry(t)
	api := newFlakyAPI(t, slices.Repeat([]int{http.StatusUnauthorized}, 1_000_000)...) // a replica that never learns job tokens
	ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := NewClient(api.srv.URL, "swj-token", "runner-1").Output(ctx, "run-1", runnercontract.JobOutputRequest{Output: "x", Stream: "plan"})
	if err == nil {
		t.Fatal("Output succeeded against a replica that refuses every request")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Output returned after %s, want it bounded by its context", elapsed)
	}
	if n := len(api.requests()); n < 2 {
		t.Errorf("Output was sent %d times before its context ended, want it sent again", n)
	}
}

// The status poll is not sent again inside one poll: the watcher's interval is its retry, so a
// stop answer is acted on at the next tick, never later.
func TestClient_StatusIsOnePollPerTick(t *testing.T) {
	api := newFlakyAPI(t, http.StatusUnauthorized)
	if _, err := NewClient(api.srv.URL, "swj-token", "runner-1").Status(t.Context(), "run-1"); err == nil {
		t.Fatal("Status hid a 401 from an older replica, want it returned for the next tick")
	}
	if got := len(api.requests()); got != 1 {
		t.Errorf("Status sent %d requests in one poll, want 1", got)
	}
}

// Executors that start retrying together must not stay in step: behind a round-robin balancer
// with one replica of the old release, executors retrying on the same schedule can keep reaching
// that replica. Each wait is drawn between half the backoff and the whole of it.
func TestRetryWaitIsJittered(t *testing.T) {
	const wait = 400 * time.Millisecond
	seen := map[time.Duration]bool{}
	for range 200 {
		d := jittered(wait)
		if d < wait/2 || d > wait {
			t.Fatalf("jittered(%s) = %s, want within [%s, %s]", wait, d, wait/2, wait)
		}
		seen[d] = true
	}
	if len(seen) < 50 {
		t.Errorf("200 draws gave %d distinct waits: the retries would stay in step", len(seen))
	}
}

type controlRoundTrip func(*http.Request) (*http.Response, error)

func (f controlRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type controlOperation struct {
	name string
	call func(context.Context, *Client) error
}

func controlOperations(t *testing.T) []controlOperation {
	t.Helper()
	return []controlOperation{
		{"complete", func(ctx context.Context, client *Client) error {
			return client.Complete(ctx, "run-1", runnercontract.JobCompleteRequest{Status: "completed"})
		}},
		{"artifacts", func(ctx context.Context, client *Client) error {
			var artifacts runnercontract.TofuArtifacts
			return client.Artifacts(ctx, "run-1", &artifacts, func() int { return artifacts.ContractVersion })
		}},
		{"get", func(ctx context.Context, client *Client) error {
			_, err := client.Get(ctx, "/api/v2/runner/jobs/run-1/plan")
			return err
		}},
		{"blob", func(ctx context.Context, client *Client) error {
			_, err := client.Blob(ctx, runnercontract.BlobRef{Path: "/api/v2/runner/jobs/run-1/blob/config"})
			return err
		}},
		{"download", func(ctx context.Context, client *Client) error {
			return client.Download(ctx, "/api/v2/runner/jobs/run-1/playbook-snapshot", filepath.Join(t.TempDir(), "snapshot"), 1<<20)
		}},
	}
}

func controlAnswer(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func TestControlOperations_RetryInternalServerFailure(t *testing.T) {
	fastRetry(t)
	for _, operation := range controlOperations(t) {
		t.Run(operation.name, func(t *testing.T) {
			api := newFlakyAPI(t, http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusUnauthorized)
			if err := operation.call(t.Context(), NewClient(api.srv.URL, "job-token", "runner-1")); err != nil {
				t.Fatalf("control operation did not recover: %v", err)
			}
			bodies := api.requests()
			if len(bodies) != 4 {
				t.Fatalf("request count = %d, want four", len(bodies))
			}
			for _, body := range bodies {
				if body != bodies[0] {
					t.Fatal("control retry changed the original payload")
				}
			}
		})
	}
}

func TestControlOperations_RetryNetworkFailure(t *testing.T) {
	fastRetry(t)
	for _, operation := range controlOperations(t) {
		t.Run(operation.name, func(t *testing.T) {
			client := NewClient("http://control.example.test", "job-token", "runner-1")
			calls := 0
			client.http.Transport = controlRoundTrip(func(request *http.Request) (*http.Response, error) {
				calls++
				if calls < 3 {
					return nil, errors.New("control plane unreachable")
				}
				return controlAnswer(request, http.StatusOK, `{"contract_version":1}`), nil
			})
			if err := operation.call(t.Context(), client); err != nil {
				t.Fatalf("control operation did not recover: %v", err)
			}
			if calls != 3 {
				t.Fatalf("request count = %d, want three", calls)
			}
		})
	}
}

func TestControlOperations_InterruptedRequestLeavesTimeForRecovery(t *testing.T) {
	for _, operation := range controlOperations(t) {
		t.Run(operation.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
				defer cancel()
				client := NewClient("http://control.example.test", "job-token", "runner-1")
				calls := 0
				var firstCanceledAfter time.Duration
				client.http.Transport = controlRoundTrip(func(request *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 {
						attemptStarted := time.Now()
						if operation.name == "complete" {
							<-request.Context().Done()
						} else {
							time.Sleep(30 * time.Second)
						}
						firstCanceledAfter = time.Since(attemptStarted)
						return nil, errors.New("response interrupted before headers")
					}
					return controlAnswer(request, http.StatusOK, `{"contract_version":1}`), nil
				})
				started := time.Now()
				if err := operation.call(ctx, client); err != nil {
					t.Fatalf("stalled first attempt prevented recovery: %v", err)
				}
				if calls != 2 {
					t.Fatalf("request count = %d, want two", calls)
				}
				firstAllowance, budget := 30*time.Second, 5*time.Minute
				if operation.name == "complete" {
					firstAllowance, budget = 20*time.Second, 2*time.Minute
				}
				if firstCanceledAfter != firstAllowance || time.Since(started) >= budget {
					t.Fatalf("first attempt lasted %s and total %s exceeded the control budgets", firstCanceledAfter, time.Since(started))
				}
			})
		})
	}
}

func TestControlDownload_SuccessfulBodyOutlivesAcquisitionBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
		defer cancel()
		client := NewClient("http://control.example.test", "job-token", "runner-1")
		client.http.Transport = controlRoundTrip(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: progressingTestBody(request.Context(), "large successful snapshot"), Request: request}, nil
		})
		started := time.Now()
		if err := client.Download(ctx, "/api/v2/runner/jobs/job-1/playbook-snapshot", filepath.Join(t.TempDir(), "snapshot"), 1<<20); err != nil {
			t.Fatalf("valid body was cut off by acquisition timers: %v", err)
		}
		if time.Since(started) != 6*time.Minute {
			t.Fatalf("body transfer took %s, want six minutes", time.Since(started))
		}
	})
}

func TestControlArtifacts_EachAttemptAdvancesSequence(t *testing.T) {
	fastRetry(t)
	client := NewClient("http://control.example.test", "job-token", "runner-1")
	var sequences []int64
	client.http.Transport = controlRoundTrip(func(request *http.Request) (*http.Response, error) {
		sequence, err := strconv.ParseInt(request.Header.Get(runnercontract.ArtifactSequenceHeader), 10, 64)
		if err != nil || sequence <= 0 {
			t.Error("artifact request has no positive sequence")
		}
		sequences = append(sequences, sequence)
		if len(sequences) == 1 {
			return nil, errors.New("artifact response lost")
		}
		if len(sequences) == 2 {
			response := controlAnswer(request, http.StatusConflict, `{"errors":[{"code":"artifact-read-superseded"}]}`)
			response.Header.Set(runnercontract.ArtifactSequenceHeader, "8")
			return response, nil
		}
		return controlAnswer(request, http.StatusOK, `{"contract_version":1}`), nil
	})
	var artifacts runnercontract.TofuArtifacts
	if err := client.Artifacts(t.Context(), "run-1", &artifacts, func() int { return artifacts.ContractVersion }); err != nil {
		t.Fatalf("ordered artifact retry did not recover: %v", err)
	}
	if len(sequences) != 3 || sequences[0] != 1 || sequences[1] != 2 || sequences[2] != 9 {
		t.Fatalf("artifact sequences = %v, want [1 2 9]", sequences)
	}
}

type brokenCompletionBody struct{}

func (brokenCompletionBody) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

func TestControlCompletion_SeparatesDeliveryAndTerminalDisposition(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		code      string
		confirmed bool
		exit      int
	}{
		{"committed", 200, "", true, ExitCompleted},
		{"acceptedWithoutCommit", 202, "", false, ExitUnreported},
		{"emptyNonCommit", 204, "", false, ExitUnreported},
		{"bareConflict", 409, "", false, ExitUnreported},
		{"bareNotFound", 404, "", false, ExitUnreported},
		{"revoked", 403, runnercontract.ErrorCodeJobTokenRevoked, false, ExitStopped},
		{"terminal", 403, runnercontract.ErrorCodeJobTerminal, false, ExitStopped},
		{"settledConflict", 409, runnercontract.ErrorCodeJobTerminal, false, ExitStopped},
		{"deleted", 404, runnercontract.ErrorCodeJobDeleted, false, ExitStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := NewClient("http://control.example.test", "job-token", "runner-1")
			calls := 0
			client.http.Transport = controlRoundTrip(func(request *http.Request) (*http.Response, error) {
				calls++
				body := `{"errors":[{"code":"` + tc.code + `"}]}`
				return controlAnswer(request, tc.status, body), nil
			})
			err := client.Complete(t.Context(), "run-1", runnercontract.JobCompleteRequest{Status: "completed"})
			if (err == nil) != tc.confirmed || client.reported.Load() != tc.confirmed {
				t.Fatalf("completion err %v, confirmed %t, want confirmed %t", err, client.reported.Load(), tc.confirmed)
			}
			if got := client.ExitCode("completed"); got != tc.exit {
				t.Fatalf("completion exit = %d, want %d", got, tc.exit)
			}
			if calls != 1 {
				t.Fatalf("terminal response sent %d times, want once", calls)
			}
		})
	}
}

func TestControlCompletion_CommittedHeadersConfirmDespiteLostBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		client := NewClient("http://control.example.test", "job-token", "runner-1")
		calls := 0
		client.http.Transport = controlRoundTrip(func(request *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(brokenCompletionBody{}), Request: request}, nil
		})
		if err := client.Complete(ctx, "run-1", runnercontract.JobCompleteRequest{Status: "completed"}); err != nil {
			t.Fatalf("committed HTTP200 headers were discarded: %v", err)
		}
		if !client.reported.Load() || client.ExitCode("completed") != ExitCompleted || calls != 1 {
			t.Fatalf("confirmed %t, exit %d, calls %d, want confirmed completion once", client.reported.Load(), client.ExitCode("completed"), calls)
		}
	})
}

func TestExcludedWrites_RetainNetworkAndInternalServerFailure(t *testing.T) {
	operations := []controlOperation{
		{"state", func(ctx context.Context, client *Client) error {
			return client.UploadState(ctx, "run-1", []byte(`{"version":4}`))
		}},
		{"plan", func(ctx context.Context, client *Client) error {
			return client.UploadPlan(ctx, "run-1", []byte("saved plan"))
		}},
		{"snapshot", func(ctx context.Context, client *Client) error {
			return client.UploadSnapshot(ctx, "wi-1", "commit-1", []byte("snapshot"))
		}},
		{"output", func(ctx context.Context, client *Client) error {
			return client.Output(ctx, "run-1", runnercontract.JobOutputRequest{Output: "line", Stream: "stdout"})
		}},
		{"event", func(ctx context.Context, client *Client) error {
			return client.Event(ctx, "job-1", runnercontract.JobEventRequest{Event: "runner_on_ok", Task: "task-1", Stdout: "done"})
		}},
	}
	for _, operation := range operations {
		for _, network := range []bool{false, true} {
			t.Run(operation.name+"/network="+strconv.FormatBool(network), func(t *testing.T) {
				client := NewClient("http://control.example.test", "job-token", "runner-1")
				calls := 0
				client.http.Transport = controlRoundTrip(func(request *http.Request) (*http.Response, error) {
					calls++
					if network {
						return nil, errors.New("write transport interrupted")
					}
					return controlAnswer(request, http.StatusInternalServerError, "write failed"), nil
				})
				if err := operation.call(t.Context(), client); err == nil {
					t.Fatal("excluded write hid its failure")
				}
				if calls != 1 {
					t.Fatalf("excluded write sent %d attempts, want one", calls)
				}
			})
		}
	}
}

func TestControlOperations_PermanentServerFailureExhaustsDeliveryBudget(t *testing.T) {
	for _, operation := range controlOperations(t) {
		t.Run(operation.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := NewClient("http://control.example.test", "job-token", "runner-1")
				calls := 0
				client.http.Transport = controlRoundTrip(func(request *http.Request) (*http.Response, error) {
					calls++
					return controlAnswer(request, http.StatusInternalServerError, "database unavailable"), nil
				})
				started := time.Now()
				if err := operation.call(t.Context(), client); err == nil {
					t.Fatal("persistent HTTP 500 was treated as delivered")
				}
				budget := 5 * time.Minute
				if operation.name == "complete" {
					budget = 2 * time.Minute
				}
				if elapsed := time.Since(started); elapsed != budget || calls < 2 {
					t.Fatalf("delivery took %s with %d attempts, want the operation-specific retry budget", elapsed, calls)
				}
				if client.reported.Load() || client.ExitCode("completed") != ExitUnreported {
					t.Fatal("persistent failure fabricated completion or retirement")
				}
			})
		})
	}
}

func TestControlOperations_AttemptUsesRemainingParentBudget(t *testing.T) {
	for _, operation := range controlOperations(t) {
		t.Run(operation.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 23*time.Second)
				defer cancel()
				client := NewClient("http://control.example.test", "job-token", "runner-1")
				var attempts []time.Duration
				client.http.Transport = controlRoundTrip(func(request *http.Request) (*http.Response, error) {
					started := time.Now()
					<-request.Context().Done()
					attempts = append(attempts, time.Since(started))
					return nil, request.Context().Err()
				})
				started := time.Now()
				if err := operation.call(ctx, client); err == nil {
					t.Fatal("stalled operation succeeded")
				}
				if time.Since(started) != 23*time.Second {
					t.Fatalf("elapsed %s, want the parent deadline", time.Since(started))
				}
				if operation.name == "complete" {
					if len(attempts) != 2 || attempts[0] != 20*time.Second || attempts[1] >= 3*time.Second {
						t.Fatalf("control attempts %v, want twenty seconds then remaining budget", attempts)
					}
				} else if len(attempts) != 1 || attempts[0] != 23*time.Second {
					t.Fatalf("required-content attempts %v, want the full remaining parent budget", attempts)
				}
			})
		})
	}
}

func TestControlArtifacts_RejectsInvalidOrExhaustedSequenceWithoutDowngrade(t *testing.T) {
	for _, latest := range []string{"", "0", "-1", "garbled", "9223372036854775808", "9223372036854775807"} {
		t.Run(latest, func(t *testing.T) {
			fastRetry(t)
			client := NewClient("http://control.example.test", "job-token", "runner-1")
			calls := 0
			client.http.Transport = controlRoundTrip(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.Header.Get(runnercontract.ArtifactSequenceHeader) != "1" {
					t.Error("request downgraded or wrapped its sequence")
				}
				response := controlAnswer(request, http.StatusConflict, `{"errors":[{"code":"artifact-read-superseded"}]}`)
				response.Header.Set(runnercontract.ArtifactSequenceHeader, latest)
				return response, nil
			})
			var artifacts runnercontract.TofuArtifacts
			if err := client.Artifacts(t.Context(), "run-1", &artifacts, nil); err == nil {
				t.Fatal("invalid or exhausted sequence was accepted")
			}
			if calls != 1 || client.ExitCode("failed") != ExitUnreported {
				t.Fatalf("calls %d, exit %d, want one failed acquisition without retirement", calls, client.ExitCode("failed"))
			}
			if latest == "9223372036854775807" {
				if err := client.Artifacts(t.Context(), "run-1", &artifacts, nil); err == nil || calls != 1 {
					t.Fatal("exhausted client reset its sequence on a new call")
				}
			}
		})
	}
}

func TestControlOperations_CodedUnauthorizedIsFinal(t *testing.T) {
	for _, operation := range controlOperations(t) {
		t.Run(operation.name, func(t *testing.T) {
			client := NewClient("http://control.example.test", "job-token", "runner-1")
			calls := 0
			client.http.Transport = controlRoundTrip(func(request *http.Request) (*http.Response, error) {
				calls++
				return controlAnswer(request, http.StatusUnauthorized, `{"errors":[{"code":"invalid-credential"}]}`), nil
			})
			if err := operation.call(t.Context(), client); err == nil || calls != 1 {
				t.Fatalf("coded authentication refusal = %v after %d attempts, want final refusal", err, calls)
			}
		})
	}
}

func TestControlArtifacts_TransportReplayAdvancesSequenceAfterWarmConnection(t *testing.T) {
	fastRetry(t)
	var mu sync.Mutex
	var warmAddresses, sequences []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		mu.Lock()
		if strings.HasSuffix(request.URL.Path, "/output") {
			warmAddresses = append(warmAddresses, request.RemoteAddr)
			mu.Unlock()
			_, _ = w.Write([]byte(`{}`))
			return
		}
		sequences = append(sequences, request.Header.Get(runnercontract.ArtifactSequenceHeader))
		attempt := len(sequences)
		mu.Unlock()
		if attempt == 1 {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = connection.Close()
			return
		}
		_, _ = w.Write([]byte(`{"contract_version":1}`))
	}))
	defer server.Close()
	client := NewClient(server.URL, "job-token", "runner-1")
	for range 2 {
		if err := client.Output(t.Context(), "run-1", runnercontract.JobOutputRequest{Output: "warm connection"}); err != nil {
			t.Fatal(err)
		}
	}
	var artifacts runnercontract.TofuArtifacts
	if err := client.Artifacts(t.Context(), "run-1", &artifacts, func() int { return artifacts.ContractVersion }); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(warmAddresses) != 2 || warmAddresses[0] != warmAddresses[1] {
		t.Fatal("fixture did not establish ordinary write connection reuse")
	}
	if len(sequences) != 2 || sequences[0] != "1" || sequences[1] != "2" {
		t.Fatalf("physical artifact request sequences = %v, want [1 2] after a reused-connection failure", sequences)
	}
}

func TestControlArtifacts_FollowedRedirectAdvancesSequenceAndPreservesPolicy(t *testing.T) {
	var sequences []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		mu.Lock()
		sequences = append(sequences, request.Header.Get(runnercontract.ArtifactSequenceHeader))
		mu.Unlock()
		if strings.HasSuffix(request.URL.Path, "/artifacts") {
			http.Redirect(w, request, "/artifact-target", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`{"contract_version":1}`))
	}))
	defer server.Close()
	client := NewClient(server.URL, "job-token", "runner-1")
	redirects := 0
	client.http.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		redirects++
		if len(via) != 1 || request.Header.Get("Authorization") != "Bearer job-token" {
			t.Error("artifact transport changed the configured redirect inputs")
		}
		return nil
	}
	var artifacts runnercontract.TofuArtifacts
	if err := client.Artifacts(t.Context(), "run-1", &artifacts, nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if redirects != 1 || len(sequences) != 2 || sequences[0] != "1" || sequences[1] != "2" {
		t.Fatalf("redirect policy calls %d, physical artifact sequences %v, want one callback and [1 2]", redirects, sequences)
	}
}

func TestControlArtifacts_RedirectSequenceExhaustionIsFinal(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		http.Redirect(w, request, "/redirected-artifacts", http.StatusFound)
	}))
	defer server.Close()
	client := NewClient(server.URL, "job-token", "runner-1")
	client.artifactSequence = 9223372036854775806
	var artifacts runnercontract.TofuArtifacts
	err := client.Artifacts(t.Context(), "run-1", &artifacts, nil)
	if err == nil || !strings.Contains(err.Error(), "sequence exhausted") || calls.Load() != 1 {
		t.Fatalf("exhausted redirect error %v after %d physical requests, want one request and final exhaustion", err, calls.Load())
	}
	if err := client.Artifacts(t.Context(), "run-1", &artifacts, nil); err == nil || calls.Load() != 1 {
		t.Fatal("redirect exhaustion reset or resent the sequence")
	}
}

func TestControlArtifacts_StandardTransportKeepsProtocolRetriesExplicit(t *testing.T) {
	var mu sync.Mutex
	var artifactProtocols, outputProtocols []int
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		mu.Lock()
		if strings.HasSuffix(request.URL.Path, "/artifacts") {
			artifactProtocols = append(artifactProtocols, request.ProtoMajor)
		} else {
			outputProtocols = append(outputProtocols, request.ProtoMajor)
		}
		mu.Unlock()
		_, _ = w.Write([]byte(`{"contract_version":1}`))
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	client := NewClient(server.URL, "job-token", "runner-1")
	client.http = server.Client()
	if err := client.Output(t.Context(), "run-1", runnercontract.JobOutputRequest{Output: "before artifacts"}); err != nil {
		t.Fatal(err)
	}
	var artifacts runnercontract.TofuArtifacts
	if err := client.Artifacts(t.Context(), "run-1", &artifacts, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Output(t.Context(), "run-1", runnercontract.JobOutputRequest{Output: "after artifacts"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(artifactProtocols) != 1 || artifactProtocols[0] != 1 {
		t.Fatalf("artifact protocols %v, want HTTP1 with fresh per-attempt connections", artifactProtocols)
	}
	if len(outputProtocols) != 2 || outputProtocols[0] != 2 || outputProtocols[1] != 2 {
		t.Fatalf("ordinary output protocols %v, want preserved HTTP2 policy", outputProtocols)
	}
}

func TestControlArtifacts_RedirectRefusalPreservesLastResponse(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		http.Redirect(w, request, "/refused-target", http.StatusFound)
	}))
	defer server.Close()
	client := NewClient(server.URL, "job-token", "runner-1")
	client.http.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	var artifacts runnercontract.TofuArtifacts
	err := client.Artifacts(t.Context(), "run-1", &artifacts, nil)
	var status *StatusError
	if !errors.As(err, &status) || status.Code != http.StatusFound || calls.Load() != 1 {
		t.Fatalf("redirect refusal = %v after %d requests, want the original 302 once", err, calls.Load())
	}
}

func TestControlArtifacts_ConcurrentRedirectAndSupersessionAdvanceTogether(t *testing.T) {
	fastRetry(t)
	entered, resume := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(resume) }) }
	var mu sync.Mutex
	var sequences []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		sequence := request.Header.Get(runnercontract.ArtifactSequenceHeader)
		mu.Lock()
		sequences = append(sequences, sequence)
		mu.Unlock()
		switch sequence {
		case "1":
			close(entered)
			<-resume
			w.Header().Set(runnercontract.ArtifactSequenceHeader, "20")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"errors":[{"code":"artifact-read-superseded"}]}`))
		case "2":
			http.Redirect(w, request, "/concurrent-target", http.StatusTemporaryRedirect)
		default:
			_, _ = w.Write([]byte(`{"contract_version":1}`))
		}
	}))
	defer func() {
		release()
		server.Close()
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	client := NewClient(server.URL, "job-token", "runner-1")
	first := make(chan error, 1)
	go func() {
		var artifacts runnercontract.TofuArtifacts
		first <- client.Artifacts(ctx, "run-1", &artifacts, nil)
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first request did not reach the hold")
	}
	var artifacts runnercontract.TofuArtifacts
	if err := client.Artifacts(ctx, "run-1", &artifacts, nil); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(sequences, []string{"1", "2", "3", "21"}) {
		t.Fatalf("concurrent physical request sequences = %v, want [1 2 3 21]", sequences)
	}
}

func TestControlArtifacts_StandardTransportReleasesCanceledAcquisition(t *testing.T) {
	for _, stalledBody := range []bool{false, true} {
		t.Run("body="+strconv.FormatBool(stalledBody), func(t *testing.T) {
			entered, released := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if stalledBody {
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				close(entered)
				<-request.Context().Done()
				close(released)
			}))
			defer server.Close()
			client := NewClient(server.URL, "job-token", "runner-1")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			completed := make(chan error, 1)
			go func() {
				var artifacts runnercontract.TofuArtifacts
				completed <- client.Artifacts(ctx, "run-1", &artifacts, nil)
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("standard transport did not reach the stalled acquisition")
			}
			cancel()
			select {
			case err := <-completed:
				if err == nil {
					t.Fatal("canceled acquisition returned complete artifacts")
				}
			case <-time.After(time.Second):
				t.Fatal("canceled acquisition retained its client")
			}
			select {
			case <-released:
			case <-time.After(time.Second):
				t.Fatal("standard transport retained the canceled server request")
			}
		})
	}
}
