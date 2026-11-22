// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

func TestRequiredAcquisition_AllEntryPointsAllowSlowHeaders(t *testing.T) {
	payload := `{"contract_version":1}`
	sum := sha256.Sum256([]byte(payload))
	ref := runnercontract.BlobRef{Path: "/required", SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(payload))}
	for _, name := range []string{"artifacts", "get", "blob", "download"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := NewClient("http://control.example.test", "job-token", "runner-1")
				calls := 0
				client.http.Transport = controlRoundTrip(func(req *http.Request) (*http.Response, error) {
					calls++
					select {
					case <-time.After(30 * time.Second):
						return controlAnswer(req, http.StatusOK, payload), nil
					case <-req.Context().Done():
						return nil, req.Context().Err()
					}
				})
				started := time.Now()
				var err error
				switch name {
				case "artifacts":
					var artifacts runnercontract.TofuArtifacts
					err = client.Artifacts(t.Context(), "run-1", &artifacts, nil)
				case "get":
					_, err = client.Get(t.Context(), "/required")
				case "blob":
					_, err = client.Blob(t.Context(), ref)
				case "download":
					err = client.Download(t.Context(), "/required", filepath.Join(t.TempDir(), "body"), 1024)
				}
				if err != nil || calls != 1 || time.Since(started) != 30*time.Second {
					t.Fatalf("slow header read = %v, %d requests, %s", err, calls, time.Since(started))
				}
			})
		})
	}
}

func TestRequiredAcquisition_ValidBodyOutlivesFiveMinutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
		defer cancel()
		client := NewClient("http://control.example.test", "job-token", "runner-1")
		client.http.Transport = controlRoundTrip(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: progressingTestBody(req.Context(), `{"contract_version":1}`), Request: req}, nil
		})
		var artifacts runnercontract.TofuArtifacts
		start := time.Now()
		if err := client.Artifacts(ctx, "run-1", &artifacts, nil); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) != 6*time.Minute {
			t.Errorf("body completed in %s", time.Since(start))
		}
	})
}

func TestRequiredAcquisition_InterruptedBodyRetainsOriginalDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := NewClient("http://control.example.test", "job-token", "runner-1")
		calls := 0
		client.http.Transport = controlRoundTrip(func(req *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				time.Sleep(4*time.Minute + 30*time.Second)
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(brokenCompletionBody{}), Request: req}, nil
			}
			select {
			case <-time.After(45 * time.Second):
				return controlAnswer(req, http.StatusOK, `{"contract_version":1}`), nil
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		})
		started := time.Now()
		var artifacts runnercontract.TofuArtifacts
		if err := client.Artifacts(t.Context(), "run-1", &artifacts, nil); err == nil {
			t.Fatal("interrupted body reset the acquisition deadline")
		}
		if time.Since(started) != 5*time.Minute || calls != 2 {
			t.Errorf("body retry = %s, %d attempts, want one shared five-minute budget", time.Since(started), calls)
		}
	})
}

func TestRequiredAcquisition_BlobRetriesTruncationWithSameValidator(t *testing.T) {
	fastRetry(t)
	payload := []byte("verified complete configuration")
	client := NewClient("http://control.example.test", "job-token", "runner-1")
	calls := 0
	client.http.Transport = controlRoundTrip(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("If-Match") != "\""+hexSum(payload)+"\"" {
			t.Error("blob retry changed or omitted its validator")
		}
		body := payload
		if calls == 1 {
			body = payload[:len(payload)-1]
		}
		return controlAnswer(req, http.StatusOK, string(body)), nil
	})
	got, err := client.Blob(t.Context(), runnercontract.BlobRef{Path: "/required", SHA256: hexSum(payload), SizeBytes: int64(len(payload))})
	if err != nil || string(got) != string(payload) || calls != 2 {
		t.Fatalf("truncated blob recovery = %v, %q, %d requests", err, got, calls)
	}
}

func TestRequiredAcquisition_BlobRejectsOversizeAndWrongDigest(t *testing.T) {
	for _, name := range []string{"oversize", "wrong_digest"} {
		t.Run(name, func(t *testing.T) {
			client := NewClient("http://control.example.test", "job-token", "runner-1")
			payload := []byte("original")
			calls := 0
			client.http.Transport = controlRoundTrip(func(req *http.Request) (*http.Response, error) {
				calls++
				return controlAnswer(req, http.StatusOK, "modified"), nil
			})
			size := int64(len(payload))
			if name == "oversize" {
				size--
			}
			if _, err := client.Blob(t.Context(), runnercontract.BlobRef{Path: "/required", SHA256: hexSum(payload), SizeBytes: size}); err == nil {
				t.Fatal("unverified content was returned")
			}
			if calls != 1 || client.retired.Load() {
				t.Errorf("integrity refusal retried or retired the job: %d, %v", calls, client.retired.Load())
			}
		})
	}
}

func TestRequiredAcquisition_PreservesCompleteCodedRefusal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := NewClient("http://control.example.test", "job-token", "runner-1")
		calls := 0
		client.http.Transport = controlRoundTrip(func(req *http.Request) (*http.Response, error) {
			calls++
			return controlAnswer(req, http.StatusUnauthorized, `{"errors":[{"code":"invalid-credential","detail":"`+strings.Repeat("explanation ", 100)+`"}]}`), nil
		})
		var artifacts runnercontract.TofuArtifacts
		err := client.Artifacts(t.Context(), "run-1", &artifacts, nil)
		var status *StatusError
		if calls != 1 || !errors.As(err, &status) || status.ErrorCode != "invalid-credential" {
			t.Fatalf("coded refusal = %v after %d requests", err, calls)
		}
	})
}
