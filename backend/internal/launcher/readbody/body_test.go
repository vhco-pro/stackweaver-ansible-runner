// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package readbody

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type zeroProgressBody struct {
	ctx    context.Context
	closed chan struct{}
	once   sync.Once
}

func (b *zeroProgressBody) Read([]byte) (int, error) {
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
		return 0, nil
	case <-b.closed:
		return 0, io.ErrClosedPipe
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
}

func (b *zeroProgressBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestZeroLengthReadsDoNotRenewIdleAllowance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		source := &zeroProgressBody{ctx: ctx, closed: make(chan struct{})}
		var cancellations atomic.Int32
		reader := New(ctx, source, func() { cancellations.Add(1); cancel() })
		started := time.Now()
		_, err := io.ReadAll(reader)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != 30*time.Second {
			t.Fatalf("zero-length reads kept the body alive: elapsed=%s error=%v", time.Since(started), err)
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Minute)
		if cancellations.Load() != 1 {
			t.Errorf("request cleanup called %d times, want once", cancellations.Load())
		}
		select {
		case <-source.closed:
		default:
			t.Error("the stalled underlying read remains open")
		}
	})
}

func TestParentCancellationClosesBlockedRead(t *testing.T) {
	for _, tc := range []struct {
		name      string
		immediate bool
	}{{"in flight", false}, {"already canceled", true}} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.immediate {
					cancel()
				}
				// A response wrapper need not observe the request context itself. The watcher
				// must close its blocked Read rather than wait for that wrapper to return.
				source := &zeroProgressBody{ctx: context.WithoutCancel(ctx), closed: make(chan struct{})}
				var cancellations atomic.Int32
				reader := New(ctx, source, func() { cancellations.Add(1); cancel() })
				if !tc.immediate {
					go func() { time.Sleep(7 * time.Second); cancel() }()
				}
				started := time.Now()
				_, err := io.ReadAll(reader)
				want := 7 * time.Second
				if tc.immediate {
					want = 0
				}
				if !errors.Is(err, context.Canceled) || time.Since(started) != want {
					t.Errorf("parent cancellation: elapsed=%s error=%v", time.Since(started), err)
				}
				if err := reader.Close(); err != nil {
					t.Error(err)
				}
				synctest.Wait()
				if cancellations.Load() != 1 {
					t.Errorf("request cancellation ran %d times", cancellations.Load())
				}
			})
		})
	}
}

func TestEOFReleasesRequestOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var cancellations atomic.Int32
		reader := New(ctx, io.NopCloser(strings.NewReader("complete")), func() { cancellations.Add(1); cancel() })
		t.Cleanup(func() {
			if err := reader.Close(); err != nil {
				t.Error(err)
			}
		})
		data, err := io.ReadAll(reader)
		if err != nil || string(data) != "complete" || cancellations.Load() != 1 {
			t.Fatalf("EOF did not release the successful request: bytes=%q error=%v cancellations=%d", data, err, cancellations.Load())
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
		if cancellations.Load() != 1 {
			t.Errorf("EOF cleanup repeated %d times", cancellations.Load())
		}
	})
}
