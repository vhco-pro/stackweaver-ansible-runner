// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-redis/redis/v8"
)

type RedisQueue struct {
	client *redis.Client
}

func NewRedisQueue(host string, port int, password string, db int) (*RedisQueue, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     fmt.Sprintf("%s:%d", host, port),
		Password: password,
		DB:       db,
	})

	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("failed to connect to redis: %w", err)
	}

	return &RedisQueue{client: client}, nil
}

func (q *RedisQueue) Enqueue(ctx context.Context, queueName string, job interface{}) error {
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}

	return q.client.LPush(ctx, queueName, data).Err()
}

func (q *RedisQueue) Dequeue(ctx context.Context, queueName string, timeout time.Duration) ([]byte, error) {
	result, err := q.client.BRPop(ctx, timeout, queueName).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, ErrQueueEmpty
		}
		return nil, err
	}

	if len(result) < 2 {
		return nil, fmt.Errorf("invalid queue result")
	}

	return []byte(result[1]), nil
}

func (q *RedisQueue) Close() error {
	return q.client.Close()
}

// Client returns the underlying Redis client for use by other services
// This allows sharing the Redis connection for log buffering, etc.
func (q *RedisQueue) Client() *redis.Client {
	return q.client
}

var ErrQueueEmpty = fmt.Errorf("queue is empty")
