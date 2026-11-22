// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

// progressingTestBody transfers the same payload over six minutes, with positive bytes every
// twenty seconds. Long-transfer fixtures must distinguish progress from an idle connection.
func progressingTestBody(ctx context.Context, payload string) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		timer := time.NewTimer(20 * time.Second)
		defer timer.Stop()
		for i := range 18 {
			select {
			case <-ctx.Done():
				_ = writer.CloseWithError(ctx.Err())
				return
			case <-timer.C:
			}
			if _, err := io.WriteString(writer, payload[len(payload)*i/18:len(payload)*(i+1)/18]); err != nil {
				_ = writer.CloseWithError(err)
				return
			}
			timer.Reset(20 * time.Second)
		}
		_ = writer.Close()
	}()
	return reader
}

type idleTrackedBody struct {
	io.ReadCloser
	closed *atomic.Bool
}

func (body idleTrackedBody) Close() error {
	body.closed.Store(true)
	return body.ReadCloser.Close()
}

func TestRunnerReadIdle_StalledBodyRetriesAndDiscardsPartialData(t *testing.T) {
	for _, operation := range []string{"get", "artifacts", "blob", "download", "cache"} {
		t.Run(operation, func(t *testing.T) {
			directory := t.TempDir()
			synctest.Test(t, func(t *testing.T) {
				client := NewClient("http://control.example.test", "job-token", "runner-1")
				payload := `{"contract_version":1}`
				calls := 0
				var closed atomic.Bool
				producerDone := make(chan struct{})
				client.http.Transport = controlRoundTrip(func(req *http.Request) (*http.Response, error) {
					calls++
					response := controlAnswer(req, http.StatusOK, payload)
					if calls != 1 {
						return response, nil
					}
					reader, writer := io.Pipe()
					response.Body = idleTrackedBody{ReadCloser: reader, closed: &closed}
					go func() {
						defer close(producerDone)
						if _, err := io.WriteString(writer, payload[:1]); err != nil {
							_ = writer.CloseWithError(err)
							return
						}
						timer := time.NewTimer(45 * time.Second)
						defer timer.Stop()
						select {
						case <-req.Context().Done():
							_ = writer.CloseWithError(req.Context().Err())
						case <-timer.C:
							_, err := io.WriteString(writer, payload[1:])
							_ = writer.CloseWithError(err)
						}
					}()
					return response, nil
				})
				started := time.Now()
				var err error
				switch operation {
				case "get":
					_, err = client.Get(t.Context(), "/required")
				case "artifacts":
					var artifacts runnercontract.TofuArtifacts
					err = client.Artifacts(t.Context(), "run-1", &artifacts, nil)
				case "blob":
					_, err = client.Blob(t.Context(), runnercontract.BlobRef{Path: "/required", SHA256: hexSum([]byte(payload)), SizeBytes: int64(len(payload))})
				case "download":
					err = client.Download(t.Context(), "/required", filepath.Join(directory, "content"), 1<<20)
				case "cache":
					cache := &Cache{Client: client}
					_, err = cache.Fetch(t.Context(), runnercontract.CacheEntryRef{Path: "/cache", CacheEntry: runnercontract.CacheEntry{Kind: runnercontract.CacheKindProvider, SHA256: hexSum([]byte(payload))}}, filepath.Join(directory, "cache"))
				}
				if err != nil {
					t.Fatal(err)
				}
				if elapsed := time.Since(started); calls != 2 || elapsed < 30*time.Second || elapsed > 30*time.Second+retryFirstWait {
					t.Errorf("stalled body delivery: calls=%d elapsed=%s, want one retry after 30s", calls, elapsed)
				}
				synctest.Wait()
				select {
				case <-producerDone:
				default:
					t.Error("stalled producer outlived the request")
				}
				if !closed.Load() {
					t.Error("stalled attempt body was not closed")
				}
			})
		})
	}
}

func TestRunnerReadIdle_CacheBodyOutlivesAcquisition(t *testing.T) {
	directory := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
		defer cancel()
		payload := "verified progressing provider content"
		client := NewClient("http://control.example.test", "job-token", "runner-1")
		calls := 0
		client.http.Transport = controlRoundTrip(func(req *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: progressingTestBody(req.Context(), payload), Request: req}, nil
		})
		cache := &Cache{Client: client}
		started := time.Now()
		got, err := cache.Fetch(ctx, runnercontract.CacheEntryRef{Path: "/cache", CacheEntry: runnercontract.CacheEntry{Kind: runnercontract.CacheKindProvider, SHA256: hexSum([]byte(payload))}}, filepath.Join(directory, "provider"))
		if err != nil || got != SourceBlob || calls != 1 || time.Since(started) != 6*time.Minute {
			t.Fatalf("healthy cache body: source=%s calls=%d elapsed=%s error=%v", got, calls, time.Since(started), err)
		}
	})
}
