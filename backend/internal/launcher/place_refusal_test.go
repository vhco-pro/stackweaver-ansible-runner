// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"fmt"
	"strings"
	"testing"

	"github.com/michielvha/stackweaver/backend/internal/launcher/driver"
	"github.com/michielvha/stackweaver/core/runnercontract"
)

// A job its driver refuses to place (an in-process pool given a custom run image) fails with the
// driver's message, which names the images and the way out, and no executor runs (#933, D4, AC30).
func TestARefusedPlacementFailsTheJobWithTheDriversMessage(t *testing.T) {
	api := newFakeAPI(t, runnercontract.CapabilityJobToken)
	d := newFakeDriver("inprocess")
	d.placeErr = fmt.Errorf("%w: the job resolved the image registry.example/custom:1, and this pool's launcher runs platform:1; use a container or Kubernetes pool for a custom run image", driver.ErrImageRefused)
	l := New(testConfig(api), d)
	l.goos = "linux"
	api.offer(runnercontract.PendingJob{JobID: "run-1", JobType: "tofu_run", RunType: "plan"})
	runLauncher(t, l)

	eventually(t, "the refused job failed", func() bool {
		_, _, failed, _ := api.snapshot()
		_, ok := failed["run-1"]
		return ok
	})
	_, _, failed, _ := api.snapshot()
	msg := failed["run-1"]
	for _, want := range []string{"could not be placed", "registry.example/custom:1", "use a container or Kubernetes pool"} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure message %q lacks %q", msg, want)
		}
	}
	if n := len(d.placements()); n != 0 {
		t.Errorf("%d placements were made for a refused job", n)
	}
}
