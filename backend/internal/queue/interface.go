// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package queue

import (
	"context"
	"time"
)

type Queue interface {
	Enqueue(ctx context.Context, queueName string, job interface{}) error
	Dequeue(ctx context.Context, queueName string, timeout time.Duration) ([]byte, error)
	Close() error
}
