// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package registry

import (
	"context"
	"io"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// StorageBackend interface for registry storage operations
type StorageBackend interface {
	PutObject(ctx context.Context, bucket, key string, data io.Reader, size int64) error
	GetObject(ctx context.Context, bucket, key string) (io.Reader, error)
	PresignGetObject(ctx context.Context, bucket, key string, expiry time.Duration) (string, error)
	DeleteObject(ctx context.Context, bucket, key string) error
	ListObjects(ctx context.Context, bucket, prefix string) ([]ObjectInfo, error)
}

// ObjectInfo represents metadata about a stored object
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// MinIOStorage implements StorageBackend using MinIO
type MinIOStorage struct {
	client *minio.Client
	bucket string
}

// NewMinIOStorage creates a new MinIO storage backend
func NewMinIOStorage(endpoint, accessKey, secretKey, bucket string, useSSL bool) (*MinIOStorage, error) {
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, err
	}

	// Ensure bucket exists
	ctx := context.Background()
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return nil, err
	}

	if !exists {
		if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			return nil, err
		}
	}

	return &MinIOStorage{
		client: client,
		bucket: bucket,
	}, nil
}

func (s *MinIOStorage) PutObject(ctx context.Context, bucket, key string, data io.Reader, size int64) error {
	_, err := s.client.PutObject(ctx, bucket, key, data, size, minio.PutObjectOptions{})
	return err
}

func (s *MinIOStorage) GetObject(ctx context.Context, bucket, key string) (io.Reader, error) {
	obj, err := s.client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	return obj, nil
}

func (s *MinIOStorage) PresignGetObject(ctx context.Context, bucket, key string, expiry time.Duration) (string, error) {
	url, err := s.client.PresignedGetObject(ctx, bucket, key, expiry, nil)
	if err != nil {
		return "", err
	}
	return url.String(), nil
}

func (s *MinIOStorage) DeleteObject(ctx context.Context, bucket, key string) error {
	return s.client.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{})
}

func (s *MinIOStorage) ListObjects(ctx context.Context, bucket, prefix string) ([]ObjectInfo, error) {
	var objects []ObjectInfo
	objectCh := s.client.ListObjects(ctx, bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	})

	for obj := range objectCh {
		if obj.Err != nil {
			return nil, obj.Err
		}
		objects = append(objects, ObjectInfo{
			Key:          obj.Key,
			Size:         obj.Size,
			LastModified: obj.LastModified,
		})
	}

	return objects, nil
}
