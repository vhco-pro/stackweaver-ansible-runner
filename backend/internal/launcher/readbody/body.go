// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// Package readbody bounds successful runner response bodies by progress rather than total time.
package readbody

import (
	"context"
	"io"
	"sync"
	"time"
)

const idleTimeout = 30 * time.Second

// New closes source after thirty seconds without positive reads or when ctx ends. cancel aborts
// the associated request and releases its transport resources. Closing or finishing the body
// stops its timers and calls cancel once; successful progressing bodies have no total timeout.
func New(ctx context.Context, source io.ReadCloser, cancel func()) io.ReadCloser {
	b := &body{source: source, ctx: ctx, cancel: cancel, deadline: time.Now().Add(idleTimeout)}
	b.mu.Lock()
	b.timer = time.AfterFunc(idleTimeout, b.checkDeadline)
	b.stopContext = context.AfterFunc(ctx, func() { b.finish(context.Cause(ctx)) })
	b.mu.Unlock()
	return b
}

type body struct {
	source      io.ReadCloser
	ctx         context.Context
	cancel      func()
	stopContext func() bool
	mu          sync.Mutex
	timer       *time.Timer
	deadline    time.Time
	stopped     bool
	err         error
	once        sync.Once
	closeErr    error
}

func (b *body) finishLocked(err error) {
	if b.stopped {
		return
	}
	b.stopped = true
	b.err = err
	b.timer.Stop()
}

func (b *body) cleanup() error {
	b.once.Do(func() {
		b.stopContext()
		b.cancel()
		b.closeErr = b.source.Close()
	})
	return b.closeErr
}

func (b *body) finish(err error) {
	b.mu.Lock()
	b.finishLocked(err)
	b.mu.Unlock()
	_ = b.cleanup()
}

func (b *body) checkDeadline() {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return
	}
	if remaining := time.Until(b.deadline); remaining > 0 {
		b.timer.Reset(remaining)
		b.mu.Unlock()
		return
	}
	b.finishLocked(context.DeadlineExceeded)
	b.mu.Unlock()
	_ = b.cleanup()
}

func (b *body) Read(data []byte) (int, error) {
	b.mu.Lock()
	if b.stopped {
		err := b.err
		b.mu.Unlock()
		if err == nil {
			err = io.EOF
		}
		return 0, err
	}
	b.mu.Unlock()
	n, err := b.source.Read(data)
	b.mu.Lock()
	if !b.stopped {
		switch {
		case b.ctx.Err() != nil:
			b.finishLocked(context.Cause(b.ctx))
		case !time.Now().Before(b.deadline):
			b.finishLocked(context.DeadlineExceeded)
		case err == io.EOF:
			b.finishLocked(nil)
		case err != nil:
			b.finishLocked(err)
		case n > 0:
			b.deadline = time.Now().Add(idleTimeout)
			b.timer.Reset(idleTimeout)
		}
	}
	if b.err != nil {
		err = b.err
	}
	stopped := b.stopped
	b.mu.Unlock()
	if stopped {
		_ = b.cleanup()
	}
	return n, err
}

func (b *body) Close() error {
	b.mu.Lock()
	b.finishLocked(io.ErrClosedPipe)
	b.mu.Unlock()
	return b.cleanup()
}
