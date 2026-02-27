// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package registry

import (
	"bytes"
	"context"
	"errors"
	"io"
	"time"
)

// MockStorage is an in-memory storage implementation for testing
type MockStorage struct {
	objects map[string][]byte
}

// NewMockStorage creates a new mock storage
func NewMockStorage() *MockStorage {
	return &MockStorage{
		objects: make(map[string][]byte),
	}
}

var ErrObjectNotFound = errors.New("object not found")

func (m *MockStorage) PutObject(ctx context.Context, bucket, key string, data io.Reader, size int64) error {
	buf := make([]byte, size)
	if _, err := io.ReadFull(data, buf); err != nil {
		return err
	}
	m.objects[bucket+"/"+key] = buf
	return nil
}

func (m *MockStorage) GetObject(ctx context.Context, bucket, key string) (io.Reader, error) {
	data, ok := m.objects[bucket+"/"+key]
	if !ok {
		return nil, ErrObjectNotFound
	}
	return bytes.NewReader(data), nil
}

func (m *MockStorage) PresignGetObject(ctx context.Context, bucket, key string, expiry time.Duration) (string, error) {
	if _, ok := m.objects[bucket+"/"+key]; !ok {
		return "", ErrObjectNotFound
	}
	return "http://mock-storage.example.com/" + bucket + "/" + key, nil
}

func (m *MockStorage) DeleteObject(ctx context.Context, bucket, key string) error {
	delete(m.objects, bucket+"/"+key)
	return nil
}

func (m *MockStorage) ListObjects(ctx context.Context, bucket, prefix string) ([]ObjectInfo, error) {
	var results []ObjectInfo
	for key := range m.objects {
		if len(key) > len(bucket+"/"+prefix) && key[:len(bucket+"/"+prefix)] == bucket+"/"+prefix {
			results = append(results, ObjectInfo{
				Key:  key,
				Size: int64(len(m.objects[key])),
			})
		}
	}
	return results, nil
}
