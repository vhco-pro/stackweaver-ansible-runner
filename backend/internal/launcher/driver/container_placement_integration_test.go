// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

//go:build integration

package driver

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A job container is told the digest of the image it runs and the host it runs on, which its
// executor reports as the job's placement (#933, D4, AC77).
func TestContainerTellsTheJobItsImageDigestAndNode(t *testing.T) {
	sfx := uuid.NewString()[:8]
	d := newTestDriver(t, "sw-probe-"+sfx+":1")
	buildProbeImage(t, d.engine(), d.PlatformImage, "1001:1001")

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
	if digest, err := d.engine().imageDigest(t.Context(), d.PlatformImage); err != nil || !strings.HasPrefix(digest, "sha256:") {
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
	buildLabelledProbeImage(t, e, d.PlatformImage, "1001:1001", map[string]string{
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
