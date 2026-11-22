// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

func TestLauncherCacheIdle_StalledBodyClosesItsRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := NewClient("http://control.example.test", "launcher-token")
		var requestContext context.Context
		producerDone := make(chan struct{})
		client.http.Transport = claimRecoveryRoundTrip(func(req *http.Request) (*http.Response, error) {
			requestContext = req.Context()
			reader, writer := io.Pipe()
			go func() {
				defer close(producerDone)
				if _, err := io.WriteString(writer, "c"); err != nil {
					_ = writer.CloseWithError(err)
					return
				}
				timer := time.NewTimer(45 * time.Second)
				defer timer.Stop()
				select {
				case <-req.Context().Done():
					_ = writer.CloseWithError(req.Context().Err())
				case <-timer.C:
					_, err := io.WriteString(writer, "ache")
					_ = writer.CloseWithError(err)
				}
			}()
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: reader, Request: req}, nil
		})
		started := time.Now()
		body, err := client.FetchCacheEntry(t.Context(), "organization", runnercontract.CacheKindProvider, strings.Repeat("a", 64))
		if err != nil {
			t.Fatal(err)
		}
		_, readErr := io.ReadAll(body)
		if err := body.Close(); err != nil {
			t.Error(err)
		}
		if !errors.Is(readErr, context.DeadlineExceeded) || time.Since(started) != 30*time.Second || requestContext.Err() == nil {
			t.Errorf("stalled prefetch: error=%v elapsed=%s canceled=%v", readErr, time.Since(started), requestContext.Err())
		}
		synctest.Wait()
		select {
		case <-producerDone:
		default:
			t.Error("prefetch producer outlived the request")
		}
	})
}
