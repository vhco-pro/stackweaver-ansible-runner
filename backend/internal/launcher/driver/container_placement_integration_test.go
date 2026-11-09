// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

//go:build integration

package driver

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A job container is told the digest of the image it runs and the host it runs on, which its
// executor reports as the job's placement (#933, D4, AC77).
func TestContainerTellsTheJobItsImageDigestAndNode(t *testing.T) {
	sfx := uuid.NewString()[:8]
	d := newTestDriver(t, "sw-probe-"+sfx+":1")
	buildProbeImage(t, d.engine(), d.JobImages["tofu"], "1001:1001")

	res, _, exit := runProbe(t, d, Request{JobID: "digest-" + sfx, Attempt: 1, Kind: "tofu", Labels: map[string]string{LabelJob: "digest-" + sfx}})
	if exit.Code != 0 {
		t.Fatalf("exit = %+v", exit)
	}
	// The probe reports variable names only, never values.
	names := map[string]bool{}
	for _, name := range res.Env {
		names[name] = true
	}
	if !names["STACKWEAVER_IMAGE_DIGEST"] || !names["STACKWEAVER_NODE_NAME"] {
		t.Errorf("job environment %v lacks STACKWEAVER_IMAGE_DIGEST or STACKWEAVER_NODE_NAME", res.Env)
	}
	if digest, err := d.engine().imageDigest(t.Context(), d.JobImages["tofu"]); err != nil || !strings.HasPrefix(digest, "sha256:") {
		t.Errorf("image digest = %q, %v; want a sha256 digest", digest, err)
	}
}

// A job placed from an image Compose built (the launcher's own image, which carries Compose's
// project and service labels) is not a container of that Compose project: Compose's lookups by
// project or service do not find it, so `docker compose up`, `down` or `--remove-orphans` never
// stop or remove a running job, and the driver's own labels are kept.
func TestContainerJobCarriesNoComposeIdentity(t *testing.T) {
	sfx := uuid.NewString()[:8]
	project := "swtestcompose" + sfx
	d := newTestDriver(t, "sw-probe-compose-"+sfx+":1")
	e := d.engine()
	buildLabelledProbeImage(t, e, d.JobImages["tofu"], "1001:1001", map[string]string{
		LabelManaged:                 "true",
		"com.docker.compose.project": project,
		"com.docker.compose.service": "launcher",
		"com.docker.compose.version": "5.4.0",
	})

	_, p, exit := runProbe(t, d, Request{JobID: "compose-" + sfx, Attempt: 1, Kind: "tofu", Labels: map[string]string{LabelJob: "compose-" + sfx}})
	if exit.Code != 0 {
		t.Fatalf("exit = %+v", exit)
	}
	var st struct {
		Config struct{ Labels map[string]string }
	}
	if err := e.do(t.Context(), http.MethodGet, "/containers/"+p.ID+"/json", nil, nil, &st); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"com.docker.compose.project", "com.docker.compose.service", "com.docker.compose.version"} {
		if v := st.Config.Labels[key]; v != "" {
			t.Errorf("the job container carries %s=%q from its image", key, v)
		}
	}
	if st.Config.Labels[LabelManaged] != "true" || st.Config.Labels[LabelJob] != "compose-"+sfx {
		t.Errorf("the driver's labels are missing: %v", st.Config.Labels)
	}
	// Compose's own lookup: the project's containers, and its launcher service's.
	for _, filter := range []string{"com.docker.compose.project=" + project, "com.docker.compose.service=launcher"} {
		var found []struct {
			ID string `json:"Id"`
		}
		q := url.Values{"all": {"1"}, "filters": {`{"label":["` + filter + `"]}`}}
		if err := e.do(t.Context(), http.MethodGet, "/containers/json", q, nil, &found); err != nil {
			t.Fatal(err)
		}
		for _, c := range found {
			if c.ID == p.ID {
				t.Errorf("a Compose lookup by %s finds the job container", filter)
			}
		}
	}
}

// newEngineDriver is a container driver whose two engines have their own job images, each the
// probe on Alpine stamped with its engine, so a test can tell which image a job's executor came
// from. The driver's own ExecutorPath leads nowhere: the executor's command comes with each
// request, as the launcher sends it per engine (one-launcher spec, A3).
func newEngineDriver(t *testing.T, sfx string) *Container {
	t.Helper()
	d := &Container{Socket: testSocket(t), ExecutorPath: "/not-the-executor", JobImages: map[string]string{
		"tofu":    "sw-probe-tofu-" + sfx + ":1",
		"ansible": "sw-probe-ansible-" + sfx + ":1",
	}}
	for engine, image := range d.JobImages {
		buildStampedProbeImage(t, d.engine(), "alpine:3.22", image, "1001:1001", engine, map[string]string{LabelManaged: "true"})
	}
	return d
}

// A job that names no run image runs in its engine's job image, started with the command the
// launcher sends for that engine (one-launcher spec, A3, AC2).
func TestContainerRunsEachEngineInItsJobImage(t *testing.T) {
	sfx := uuid.NewString()[:8]
	d := newEngineDriver(t, sfx)
	for _, engine := range []string{"tofu", "ansible"} {
		job := engine + "-" + sfx
		res, p, exit := runProbe(t, d, Request{JobID: job, Attempt: 1, Kind: engine, Command: []string{"/probe", "executor"}, Labels: map[string]string{LabelJob: job}})
		if exit.Code != 0 {
			t.Fatalf("%s job exit = %+v", engine, exit)
		}
		if res.Stamp != engine {
			t.Errorf("the %s job ran in the image stamped %q, want its own job image", engine, res.Stamp)
		}
		var st struct {
			Config struct{ Image string }
		}
		if err := d.engine().do(t.Context(), http.MethodGet, "/containers/"+p.ID+"/json", nil, nil, &st); err != nil {
			t.Fatal(err)
		}
		// Podman records a short name with the registry it resolved it on.
		if image := strings.TrimPrefix(st.Config.Image, "docker.io/library/"); image != d.JobImages[engine] {
			t.Errorf("the %s job's container runs %s, want %s", engine, image, d.JobImages[engine])
		}
		if !slices.Equal(res.Args, []string{"executor"}) {
			t.Errorf("the %s job ran with %v, want the executor subcommand", engine, res.Args)
		}
	}
}

// A custom run image of either engine gets the executor injected from that engine's job image and
// runs to completion as /stackweaver/stackweaver-executor, with the invocation the executor works
// its engine out from (one-launcher spec, A1, A3, AC6, AC7).
func TestContainerInjectsTheEnginesExecutorIntoACustomImage(t *testing.T) {
	sfx := uuid.NewString()[:8]
	d := newEngineDriver(t, sfx)
	custom := "sw-probe-runimage-" + sfx + ":1"
	buildStampedProbeImage(t, d.engine(), "scratch", custom, "1001:1001", "run-image", map[string]string{LabelManaged: "true"})
	for _, tc := range []struct{ engine, runType string }{{"tofu", "plan"}, {"ansible", ""}} {
		job := "inj-" + tc.engine + "-" + sfx
		env := []string{"STACKWEAVER_JOB_ID=" + job}
		if tc.runType != "" {
			env = append(env, "STACKWEAVER_JOB_RUN_TYPE="+tc.runType)
		}
		res, p, exit := runProbe(t, d, Request{JobID: job, Attempt: 1, Kind: tc.engine, RunType: tc.runType, Image: custom, Env: env, Command: []string{"/probe", "executor"}, Labels: map[string]string{LabelJob: job}})
		t.Cleanup(func() { _ = d.removeContainer(context.Background(), p.ID, p.Labels) })
		if exit.Code != 0 {
			t.Fatalf("the %s run image's job exit = %+v", tc.engine, exit)
		}
		if res.Stamp != tc.engine {
			t.Errorf("the %s run image's executor came from the image stamped %q, want the %s job image", tc.engine, res.Stamp, tc.engine)
		}
		var st struct {
			Config struct{ Entrypoint []string }
		}
		if err := d.engine().do(t.Context(), http.MethodGet, "/containers/"+p.ID+"/json", nil, nil, &st); err != nil {
			t.Fatal(err)
		}
		if want := []string{ExecutorMountPath + "/stackweaver-executor"}; !slices.Equal(st.Config.Entrypoint, want) || !slices.Equal(res.Args, []string{"executor"}) {
			t.Errorf("the %s job ran %v %v, want the injected executor %v with executor", tc.engine, st.Config.Entrypoint, res.Args, want)
		}
		if got := slices.Contains(res.Env, "STACKWEAVER_JOB_RUN_TYPE"); got != (tc.runType != "") {
			t.Errorf("the %s job's invocation carries a run type: %v, want %v", tc.engine, got, tc.runType != "")
		}
	}
}
