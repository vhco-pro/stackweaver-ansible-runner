// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import "testing"

// The kubelet pulls a job pod's image, so its digest is known only from the pod's status once the
// executor container exists (#933, D4, AC27, AC77): a pulled image reports its repository digest
// (`<repo>@sha256:...`), a node-loaded one its image id (`sha256:...`), and a container still
// waiting for its image none.
func TestKubernetesImageDigestComesFromTheExecutorsStatus(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name    string
		imageID string
		want    string
	}{
		{"pulled from a registry", "sw-e2e-registry:5000/stackweaver-runner-private@" + digest, digest},
		{"a docker-pullable reference", "docker-pullable://registry.example.test/tofu@" + digest, digest},
		{"loaded onto the node", digest, digest},
		{"not pulled yet", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, d := newKubeDriver(t)
			p, err := d.Place(t.Context(), kubeRequest())
			if err != nil {
				t.Fatal(err)
			}
			obj := f.get("/api/v1/namespaces/jobs/pods/" + p.ID)
			f.mu.Lock()
			obj["status"] = map[string]any{"phase": "Running", "containerStatuses": []any{
				map[string]any{"name": "sidecar", "imageID": "sha256:" + "f"},
				map[string]any{"name": "executor", "imageID": tc.imageID},
			}}
			f.mu.Unlock()
			got, err := d.ImageDigest(t.Context(), p)
			if err != nil || got != tc.want {
				t.Errorf("ImageDigest = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
