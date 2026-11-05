// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// recordedCreate is the network create request a fake engine received.
type recordedCreate struct {
	path string
	body map[string]any
}

func recordNetworkCreate(t *testing.T) (*engine, *recordedCreate) {
	t.Helper()
	got := &recordedCreate{}
	e := fakeBuildEngine(t, func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &got.body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"Id":"n"}`))
	})
	return e, got
}

// Podman's Docker API creates every network with isolate=true, for Docker compatibility, and
// netavark then drops traffic between isolated networks in a table of its own, where no accept of
// ours can undo it: a job could not reach an allowed endpoint published by a container on another
// network. On rootful Podman the jobs network is created through Podman's own API without
// isolation, with its DNS, and the jobs table is the whole policy.
func TestRootfulPodmanCreatesTheJobsNetworkWithoutEngineIsolation(t *testing.T) {
	e, got := recordNetworkCreate(t)
	if err := createJobsNetwork(t.Context(), e, "sw-jobs-x", firewallNFTables, true, false); err != nil {
		t.Fatal(err)
	}
	if got.path != "/"+libpodAPIVersion+"/libpod/networks/create" {
		t.Fatalf("created through %s, want Podman's own API", got.path)
	}
	if got.body["name"] != "sw-jobs-x" || got.body["driver"] != "bridge" || got.body["dns_enabled"] != true {
		t.Errorf("body = %v, want a bridge network of that name with its DNS", got.body)
	}
	if opts, _ := got.body["options"].(map[string]any); opts["isolate"] != nil {
		t.Errorf("options = %v, want no engine isolation", opts)
	}
	if labels, _ := got.body["labels"].(map[string]any); labels[LabelManaged] != "true" {
		t.Errorf("labels = %v, want the managed label", labels)
	}
}

func TestOtherEnginesCreateTheJobsNetworkThroughTheDockerAPI(t *testing.T) {
	for _, tc := range []struct {
		name     string
		backend  firewallBackend
		podman   bool
		rootless bool
	}{
		{"docker iptables", firewallIPTables, false, false},
		{"docker nftables", firewallNFTables, false, false},
		{"rootless podman", firewallNFTables, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, got := recordNetworkCreate(t)
			if err := createJobsNetwork(t.Context(), e, "sw-jobs-x", tc.backend, tc.podman, tc.rootless); err != nil {
				t.Fatal(err)
			}
			if got.path != "/"+engineAPIVersion+"/networks/create" || got.body["Name"] != "sw-jobs-x" {
				t.Errorf("created %v through %s, want the Docker API", got.body, got.path)
			}
		})
	}
}
