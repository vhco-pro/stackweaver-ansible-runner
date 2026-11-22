// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/michielvha/stackweaver/backend/internal/launcher/cache"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

func prefetchBudgetEntry(ctx context.Context, client *Client, directory, payload string) (string, error) {
	sum := sha256.Sum256([]byte(payload))
	digest := hex.EncodeToString(sum[:])
	descriptor := &runnercontract.CacheDescriptor{
		OrganizationID: "11111111-1111-4111-8111-111111111111",
		Entries:        []runnercontract.CacheEntryRef{{CacheEntry: runnercontract.CacheEntry{Kind: runnercontract.CacheKindProvider, SHA256: digest}}},
	}
	readThrough := &cache.ReadThrough{Dir: directory, Source: clientSource{client}}
	orgDirectory, err := readThrough.Prepare(ctx, descriptor)
	return filepath.Join(orgDirectory, "provider", digest), err
}

func TestLauncherPrefetch_RetriesInterruptedBody(t *testing.T) {
	directory := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		payload := "complete provider content"
		client := NewClient("http://control.example.test", "launcher-token")
		calls := 0
		client.http.Transport = claimRecoveryRoundTrip(func(req *http.Request) (*http.Response, error) {
			calls++
			response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload)), Request: req}
			if calls == 1 {
				reader, writer := io.Pipe()
				response.Body = reader
				go func() {
					if _, err := io.WriteString(writer, payload[:1]); err != nil {
						_ = writer.CloseWithError(err)
						return
					}
					<-req.Context().Done()
					_ = writer.CloseWithError(req.Context().Err())
				}()
			}
			return response, nil
		})
		started := time.Now()
		path, err := prefetchBudgetEntry(t.Context(), client, directory, payload)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path) //nolint:gosec // G304: path contains only this test's temp directory, fixed organization and fixture digest.
		if err != nil || string(got) != payload || calls != 2 || time.Since(started) < 30*time.Second || time.Since(started) >= 31*time.Second {
			t.Fatalf("prefetch did not recover the verified entry: calls=%d elapsed=%s bytes=%q error=%v", calls, time.Since(started), got, err)
		}
	})
}

func TestLauncherPrefetch_OneAcquisitionBudget(t *testing.T) {
	for _, mode := range []string{"no headers", "server errors"} {
		t.Run(mode, func(t *testing.T) {
			directory := t.TempDir()
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
				defer cancel()
				client := NewClient("http://control.example.test", "launcher-token")
				calls := 0
				client.http.Transport = claimRecoveryRoundTrip(func(req *http.Request) (*http.Response, error) {
					calls++
					if mode == "no headers" {
						<-req.Context().Done()
						return nil, req.Context().Err()
					}
					return &http.Response{StatusCode: http.StatusInternalServerError, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("unavailable")), Request: req}, nil
				})
				started := time.Now()
				_, err := prefetchBudgetEntry(ctx, client, directory, "provider content")
				if err != nil || ctx.Err() != nil || time.Since(started) != 2*time.Minute || calls < 2 {
					t.Fatalf("prefetch acquisition: calls=%d elapsed=%s error=%v parent=%v", calls, time.Since(started), err, ctx.Err())
				}
			})
		})
	}
}

func TestLauncherPrefetch_FailedBodiesDoNotResetAcquisition(t *testing.T) {
	directory := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		client := NewClient("http://control.example.test", "launcher-token")
		var attempts []time.Time
		client.http.Transport = claimRecoveryRoundTrip(func(req *http.Request) (*http.Response, error) {
			attempts = append(attempts, time.Now())
			reader, writer := io.Pipe()
			go func() {
				if _, err := io.WriteString(writer, "p"); err != nil {
					_ = writer.CloseWithError(err)
					return
				}
				<-req.Context().Done()
				_ = writer.CloseWithError(req.Context().Err())
			}()
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: reader, Request: req}, nil
		})
		started := time.Now()
		_, err := prefetchBudgetEntry(ctx, client, directory, "provider content")
		if err != nil || ctx.Err() != nil || len(attempts) < 3 || time.Since(started) < 2*time.Minute || time.Since(started) > 150*time.Second {
			t.Fatalf("failed-body budget: attempts=%d elapsed=%s error=%v parent=%v", len(attempts), time.Since(started), err, ctx.Err())
		}
		for _, attempt := range attempts {
			if !attempt.Before(started.Add(2 * time.Minute)) {
				t.Error("a retry began after the original acquisition deadline")
			}
		}
	})
}

func TestLauncherPrefetch_ProgressingBodyOutlivesAcquisition(t *testing.T) {
	directory := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
		defer cancel()
		payload := strings.Repeat("p", 18)
		client := NewClient("http://control.example.test", "launcher-token")
		calls := 0
		client.http.Transport = claimRecoveryRoundTrip(func(req *http.Request) (*http.Response, error) {
			calls++
			reader, writer := io.Pipe()
			go func() {
				timer := time.NewTimer(20 * time.Second)
				defer timer.Stop()
				for range 18 {
					select {
					case <-req.Context().Done():
						_ = writer.CloseWithError(req.Context().Err())
						return
					case <-timer.C:
					}
					if _, err := io.WriteString(writer, "p"); err != nil {
						_ = writer.CloseWithError(err)
						return
					}
					timer.Reset(20 * time.Second)
				}
				_ = writer.Close()
			}()
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: reader, Request: req}, nil
		})
		started := time.Now()
		path, err := prefetchBudgetEntry(ctx, client, directory, payload)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path) //nolint:gosec // G304: path contains only this test's temp directory, fixed organization and fixture digest.
		if err != nil || string(got) != payload || calls != 1 || time.Since(started) != 6*time.Minute {
			t.Fatalf("progressing prefetch: calls=%d elapsed=%s bytes=%q error=%v", calls, time.Since(started), got, err)
		}
	})
}

func TestLauncherPrefetch_RefusalsAndIntegrityRemainFinal(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			directory := t.TempDir()
			synctest.Test(t, func(t *testing.T) {
				client := NewClient("http://control.example.test", "launcher-token")
				calls := 0
				client.http.Transport = claimRecoveryRoundTrip(func(req *http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("different content")), Request: req}, nil
				})
				started := time.Now()
				path, err := prefetchBudgetEntry(t.Context(), client, directory, "verified content")
				if err != nil || calls != 1 || time.Since(started) != 0 {
					t.Fatalf("final cache disposition retried: calls=%d elapsed=%s error=%v", calls, time.Since(started), err)
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("unverified/refused content was installed: %v", err)
				}
			})
		})
	}
}
