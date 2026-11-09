// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	k8s "github.com/michielvha/stackweaver/core/runnerdriver/kubernetes"
)

const privateJobImage = "ghcr.io/vhco-pro/stackweaver-opentofu-runner:latest"

// pullSecretNames are the imagePullSecrets a placed pod lists.
func pullSecretNames(t *testing.T, f *fakeKube, pod string) []string {
	t.Helper()
	obj := f.get("/api/v1/namespaces/jobs/pods/" + pod)
	list, _ := obj["spec"].(map[string]any)["imagePullSecrets"].([]any)
	var names []string
	for _, s := range list {
		names = append(names, s.(map[string]any)["name"].(string))
	}
	return names
}

func dockerConfigAuths(t *testing.T, secret map[string]any) map[string]map[string]string {
	t.Helper()
	var cfg struct {
		Auths map[string]map[string]string `json:"auths"`
	}
	if err := json.Unmarshal([]byte(secret["stringData"].(map[string]any)[".dockerconfigjson"].(string)), &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.Auths
}

// The Kubernetes driver carries the launcher's own registry credential into the job namespace as a
// per-job pull Secret of its own (one-launcher spec, A3, OD26, AC42): created with the pod as its
// owner before the scheduling gate lifts, keyed by the job image's repository, recorded so a failed
// placement removes it, listed beside a private run image's per-job Secret, and never made from an
// identitytoken entry, which a dockerconfigjson Secret cannot carry.
func TestKubernetesJobImagePullSecret(t *testing.T) {
	launcherAuth := RegistryCredentials{
		"ghcr.io":          {Registry: "ghcr.io", Username: "launcher", Password: "s3cret"},
		"token.example.io": {Registry: "token.example.io", IdentityToken: "tok"},
	}
	newDriver := func(t *testing.T) (*fakeKube, *Kubernetes) {
		t.Helper()
		f, d := newKubeDriver(t)
		d.JobImages = map[string]string{"tofu": privateJobImage, "ansible": "ghcr.io/vhco-pro/stackweaver-ansible-runner:latest"}
		d.Auth = launcherAuth
		return f, d
	}

	t.Run("a job in its private job image", func(t *testing.T) {
		f, d := newDriver(t)
		p, err := d.Place(t.Context(), kubeRequest())
		if err != nil {
			t.Fatal(err)
		}
		name := p.ID + "-job-image-pull"
		secret := f.get("/api/v1/namespaces/jobs/secrets/" + name)
		if secret == nil {
			t.Fatalf("no job image pull secret %s; applied %v", name, f.order)
		}
		meta := secret["metadata"].(map[string]any)
		owners, _ := meta["ownerReferences"].([]any)
		if len(owners) != 1 || owners[0].(map[string]any)["uid"] != "uid-"+p.ID {
			t.Errorf("owners = %v, want the pod", owners)
		}
		if meta["labels"].(map[string]any)[k8s.LabelManagedBy] != k8s.ManagedByLauncher {
			t.Errorf("labels = %v", meta["labels"])
		}
		auths := dockerConfigAuths(t, secret)
		if len(auths) != 1 || auths["ghcr.io/vhco-pro/stackweaver-opentofu-runner"]["username"] != "launcher" {
			t.Errorf("auths = %v, want one entry keyed by the job image's repository", auths)
		}
		if names := pullSecretNames(t, f, p.ID); !slices.Contains(names, name) {
			t.Errorf("the pod lists %v, want %s", names, name)
		}
		// The gated pod, the job secret, the job image pull secret, then the gate lifted.
		secretAt, releasedAt := slices.Index(f.order, "PATCH /api/v1/namespaces/jobs/secrets/"+name), len(f.order)-1
		if secretAt < 1 || secretAt >= releasedAt || !strings.HasPrefix(f.order[releasedAt], "PATCH /api/v1/namespaces/jobs/pods/") {
			t.Errorf("objects applied in order %v, want the job image pull secret after the gated pod and before the gate lifts", f.order)
		}
	})

	t.Run("a private run image on a private job image lists both secrets", func(t *testing.T) {
		f, d := newDriver(t)
		req := kubeRequest()
		req.Image = "ghcr.io/acme/tofu-custom:2"
		req.PullAuth = &RegistryAuth{Registry: "ghcr.io", Username: "tenant", Password: "theirs"}
		p, err := d.Place(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		names := pullSecretNames(t, f, p.ID)
		if !slices.Contains(names, p.ID+"-pull") || !slices.Contains(names, p.ID+"-job-image-pull") {
			t.Errorf("the pod lists %v, want the run image's and the job image's pull secrets", names)
		}
		if auths := dockerConfigAuths(t, f.get("/api/v1/namespaces/jobs/secrets/"+p.ID+"-pull")); auths["ghcr.io"]["username"] != "tenant" {
			t.Errorf("the run image's secret = %v, want the tenant's own credential", auths)
		}
	})

	t.Run("a failed placement removes it", func(t *testing.T) {
		f, d := newDriver(t)
		podPatches := 0
		f.failPatch = func(path string) bool {
			if strings.Contains(path, "/pods/") {
				podPatches++
				return podPatches >= 2 // lifting the gate, retried included
			}
			return false
		}
		if _, err := d.Place(t.Context(), kubeRequest()); err == nil {
			t.Fatal("the placement succeeded despite the failed release")
		}
		for path := range f.objects {
			if strings.Contains(path, "/secrets/") {
				t.Errorf("%s left behind by a failed placement", path)
			}
		}
	})

	t.Run("an identitytoken entry makes no secret", func(t *testing.T) {
		f, d := newDriver(t)
		d.JobImages = sameJobImage("token.example.io/stackweaver/opentofu:1")
		p, err := d.Place(t.Context(), kubeRequest())
		if err != nil {
			t.Fatal(err)
		}
		if f.get("/api/v1/namespaces/jobs/secrets/"+p.ID+"-job-image-pull") != nil {
			t.Error("an identitytoken entry became a pull secret")
		}
		if names := pullSecretNames(t, f, p.ID); len(names) != 0 {
			t.Errorf("the pod lists %v, want none", names)
		}
	})

	t.Run("a job image with no credential makes no secret", func(t *testing.T) {
		f, d := newDriver(t)
		d.JobImages = sameJobImage("quay.io/stackweaver/opentofu:1")
		p, err := d.Place(t.Context(), kubeRequest())
		if err != nil {
			t.Fatal(err)
		}
		if f.get("/api/v1/namespaces/jobs/secrets/"+p.ID+"-job-image-pull") != nil {
			t.Error("a job image on a registry without a credential got a pull secret")
		}
	})
}
