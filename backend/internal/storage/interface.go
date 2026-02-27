// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package storage

import (
	"context"
	"io"
	"time"
)

type Client interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
	GetStream(ctx context.Context, key string) (io.ReadCloser, error)
	PutStream(ctx context.Context, key string, reader io.Reader) error
	List(ctx context.Context, prefix string) ([]string, error)
	// PresignGet returns a presigned URL for GET (e.g. state download). expiry typically 1–24h.
	PresignGet(ctx context.Context, key string, expiry time.Duration) (string, error)
}
