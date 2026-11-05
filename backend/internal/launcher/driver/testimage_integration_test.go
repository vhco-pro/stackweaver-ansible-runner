// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

//go:build integration

package driver

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// testImageBuildDeadline bounds one test image build. A healthy engine builds the probe, tools and
// conformance images in well under a minute; a daemon whose build session is stuck never answers,
// and without a deadline the test would wait for the package's whole timeout.
const testImageBuildDeadline = 5 * time.Minute

// buildTestImage builds a test image and removes it when the test ends. A build past its deadline
// fails the test, naming the image; any other failure is returned, for the caller to fail or skip
// on.
func buildTestImage(t *testing.T, e *engine, tag string, buildContext *bytes.Buffer, labels map[string]string) error {
	t.Helper()
	err := postImageBuild(t.Context(), e, tag, buildContext, labels, testImageBuildDeadline)
	if errors.Is(err, errImageBuildDeadline) {
		t.Fatal(err)
	}
	if err != nil {
		return err
	}
	t.Cleanup(func() {
		r, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete, "http://engine/"+engineAPIVersion+"/images/"+tag+"?force=1", nil)
		if resp, err := e.http.Do(r); err == nil {
			_ = resp.Body.Close()
		}
	})
	return nil
}
