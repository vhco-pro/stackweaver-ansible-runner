// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// errImageBuildDeadline marks a build the engine did not finish within its deadline.
var errImageBuildDeadline = errors.New("the image build deadline passed")

// postImageBuild builds tag from buildContext, a tar with a Dockerfile, with the given image labels
// (nil for none), and waits at most deadline for the engine to finish the build stream.
func postImageBuild(ctx context.Context, e *engine, tag string, buildContext *bytes.Buffer, labels map[string]string, deadline time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	query := url.Values{"t": {tag}, "rm": {"1"}}
	if labels != nil {
		labelJSON, err := json.Marshal(labels)
		if err != nil {
			return fmt.Errorf("encoding the labels of %s: %w", tag, err)
		}
		query.Set("labels", string(labelJSON))
	}
	stalled := func(err error) error {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("the engine did not finish building %s within %s: %w", tag, deadline, errImageBuildDeadline)
		}
		return fmt.Errorf("building %s: %w", tag, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://engine/"+engineAPIVersion+"/build?"+query.Encode(), buildContext)
	if err != nil {
		return fmt.Errorf("building %s: %w", tag, err)
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := e.http.Do(req)
	if err != nil {
		return stalled(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return stalled(err)
	}
	if resp.StatusCode != http.StatusOK || bytes.Contains(body, []byte(`"error"`)) {
		return fmt.Errorf("building %s: %d %s", tag, resp.StatusCode, body)
	}
	return nil
}
