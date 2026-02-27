// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package logbuffer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/iac-platform/backend/internal/storage"
)

// RedisLogBuffer provides log buffering functionality using Redis
// Logs are stored in Redis during execution and can be copied to MinIO for persistence
type RedisLogBuffer struct {
	client *redis.Client
}

// NewRedisLogBuffer creates a new Redis log buffer service
func NewRedisLogBuffer(client *redis.Client) *RedisLogBuffer {
	return &RedisLogBuffer{
		client: client,
	}
}

// Append appends a line to the log buffer for a specific run and phase
// Key format: run:logs:{runID}:{phase}
// Sets TTL on first write (24 hours)
func (b *RedisLogBuffer) Append(ctx context.Context, runID, phase, line string) error {
	key := fmt.Sprintf("run:logs:%s:%s", runID, phase)

	// Append the line with newline
	err := b.client.Append(ctx, key, line+"\n").Err()
	if err != nil {
		return fmt.Errorf("failed to append log line: %w", err)
	}

	// Set TTL on first write (24 hours)
	// Using Expire with NX would be ideal, but we use Expire which is idempotent
	// The overhead is minimal since we're already doing a write
	b.client.Expire(ctx, key, 24*time.Hour)

	return nil
}

// Get retrieves logs from the buffer for a specific run and phase
// Supports offset and limit for pagination/streaming
// Returns empty string if key doesn't exist (not an error)
func (b *RedisLogBuffer) Get(ctx context.Context, runID, phase string, offset, limit int) (string, error) {
	key := fmt.Sprintf("run:logs:%s:%s", runID, phase)

	content, err := b.client.Get(ctx, key).Result()
	if err == redis.Nil {
		// Key doesn't exist - return empty string (not an error)
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to get log content: %w", err)
	}

	// Handle offset/limit
	if offset <= 0 && limit <= 0 {
		// No pagination - return all
		return content, nil
	}

	lines := strings.Split(content, "\n")
	if offset >= len(lines) {
		return "", nil
	}

	end := offset + limit
	if limit <= 0 {
		end = len(lines)
	} else if end > len(lines) {
		end = len(lines)
	}

	result := strings.Join(lines[offset:end], "\n")
	return result, nil
}

// CopyToMinIO copies logs from Redis to MinIO for long-term persistence
// Returns nil if key doesn't exist (logs may have already been copied or never existed)
func (b *RedisLogBuffer) CopyToMinIO(ctx context.Context, runID, phase string, storageClient storage.Client) error {
	key := fmt.Sprintf("run:logs:%s:%s", runID, phase)

	content, err := b.client.Get(ctx, key).Result()
	if err == redis.Nil {
		// No logs in Redis - skip (may have already been copied or never existed)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get logs from Redis: %w", err)
	}

	// Write to MinIO for long-term persistence
	logsKey := fmt.Sprintf("runs/%s/logs/%s.log", runID, phase)
	if err := storageClient.Put(ctx, logsKey, []byte(content)); err != nil {
		return fmt.Errorf("failed to copy logs to MinIO: %w", err)
	}

	// Note: We don't delete from Redis immediately - let TTL handle cleanup
	// This allows faster access for a period after completion
	return nil
}

// Delete removes logs from Redis (useful for cleanup or testing)
func (b *RedisLogBuffer) Delete(ctx context.Context, runID, phase string) error {
	key := fmt.Sprintf("run:logs:%s:%s", runID, phase)
	return b.client.Del(ctx, key).Err()
}

// Exists checks if logs exist in Redis for a specific run and phase
func (b *RedisLogBuffer) Exists(ctx context.Context, runID, phase string) (bool, error) {
	key := fmt.Sprintf("run:logs:%s:%s", runID, phase)
	count, err := b.client.Exists(ctx, key).Result()
	if err != nil {
		return false, err
	}
	return count > 0, nil
}
