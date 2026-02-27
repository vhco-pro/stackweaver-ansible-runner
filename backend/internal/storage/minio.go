// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package storage

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/michielvha/logger"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type MinIOClient struct {
	client *minio.Client
	bucket string
}

func NewMinIOClient(endpoint, accessKey, secretKey, bucket string, useSSL bool) (*MinIOClient, error) {
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

	return &MinIOClient{
		client: client,
		bucket: bucket,
	}, nil
}

func (c *MinIOClient) Put(ctx context.Context, key string, data []byte) error {
	_, err := c.client.PutObject(ctx, c.bucket, key, strings.NewReader(string(data)), int64(len(data)), minio.PutObjectOptions{})
	return err
}

func (c *MinIOClient) Get(ctx context.Context, key string) ([]byte, error) {
	obj, err := c.client.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := obj.Close(); err != nil {
			logger.Warnf("Failed to close object: %v", err)
		}
	}()

	data, err := io.ReadAll(obj)
	if err != nil {
		return nil, err
	}

	return data, nil
}

func (c *MinIOClient) Delete(ctx context.Context, key string) error {
	return c.client.RemoveObject(ctx, c.bucket, key, minio.RemoveObjectOptions{})
}

func (c *MinIOClient) GetStream(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := c.client.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	return obj, nil
}

func (c *MinIOClient) PutStream(ctx context.Context, key string, reader io.Reader) error {
	_, err := c.client.PutObject(ctx, c.bucket, key, reader, -1, minio.PutObjectOptions{})
	return err
}

func (c *MinIOClient) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	objectCh := c.client.ListObjects(ctx, c.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	})

	for obj := range objectCh {
		if obj.Err != nil {
			return nil, obj.Err
		}
		keys = append(keys, obj.Key)
	}

	return keys, nil
}

func (c *MinIOClient) PresignGet(ctx context.Context, key string, expiry time.Duration) (string, error) {
	u, err := c.client.PresignedGetObject(ctx, c.bucket, key, expiry, nil)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}
