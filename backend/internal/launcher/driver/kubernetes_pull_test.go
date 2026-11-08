// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
	k8s "github.com/michielvha/stackweaver/core/runnerdriver/kubernetes"
)

// A job whose run image is pulled with a registry credential gets a per-job pull Secret its pod
// names and owns; the credential never appears in the pod spec (#933, D6, D8, AC27).
func TestKubernetesPullsTheRunImageWithItsCredential(t *testing.T) {
	f, d := newKubeDriver(t)
	req := kubeRequest()
	req.Image = "registry.internal:5000/acme/tofu-aws@sha256:" + strings.Repeat("ab", 32)
	req.PullAuth = &RegistryAuth{Registry: "registry.internal:5000", Username: "robot", Password: "pull-s3cret"}
	p, err := d.Place(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	pod := f.get("/api/v1/namespaces/jobs/pods/" + p.ID)
	spec := pod["spec"].(map[string]any)
	secrets, _ := spec["imagePullSecrets"].([]any)
	if len(secrets) != 1 || secrets[0].(map[string]any)["name"] != p.ID+"-pull" {
		t.Fatalf("imagePullSecrets = %v", spec["imagePullSecrets"])
	}
	pull := f.get("/api/v1/namespaces/jobs/secrets/" + p.ID + "-pull")
	if pull == nil || pull["type"] != "kubernetes.io/dockerconfigjson" {
		t.Fatalf("pull secret = %v", pull)
	}
	if !strings.Contains(pull["stringData"].(map[string]any)[".dockerconfigjson"].(string), "pull-s3cret") {
		t.Error("the pull secret does not carry the credential")
	}
	owners, _ := pull["metadata"].(map[string]any)["ownerReferences"].([]any)
	if len(owners) != 1 || owners[0].(map[string]any)["uid"] != "uid-"+p.ID {
		t.Errorf("the pull secret is not owned by the pod: %v", owners)
	}
	containers := spec["containers"].([]any)
	if img := containers[0].(map[string]any)["image"]; img != req.Image {
		t.Errorf("the job container runs %v, want the run image", img)
	}
}

// A job whose pool offers a long-lived pull Secret for its image's registry runs with that Secret:
// the pod names it and no per-job Secret is written (#933, D8, AC48).
func TestKubernetesPullsWithThePoolsPullSecret(t *testing.T) {
	f, d := newKubeDriver(t)
	req := kubeRequest()
	req.Image = "ghcr.io/acme/tofu-aws:1.9"
	req.PullSecret = "registry-ghcr"
	p, err := d.Place(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	spec := f.get("/api/v1/namespaces/jobs/pods/" + p.ID)["spec"].(map[string]any)
	secrets, _ := spec["imagePullSecrets"].([]any)
	if len(secrets) != 1 || secrets[0].(map[string]any)["name"] != "registry-ghcr" {
		t.Fatalf("imagePullSecrets = %v, want the pool's Secret", spec["imagePullSecrets"])
	}
	if s := f.get("/api/v1/namespaces/jobs/secrets/" + p.ID + "-pull"); s != nil {
		t.Errorf("a per-job pull Secret was written beside the pool's: %v", s)
	}
}

// The launcher creates the pull Secrets its pool asks for, from their credential, under its own
// field manager and label; a Secret another manager holds is never written (#933, D8, AC49a).
func TestKubernetesEnsuresPullSecrets(t *testing.T) {
	f, d := newKubeDriver(t)
	f.put("/api/v1/namespaces/jobs/secrets/sw-pull-flux", map[string]any{"metadata": map[string]any{
		"name": "sw-pull-flux", "namespace": "jobs", "labels": map[string]any{"kustomize.toolkit.fluxcd.io/name": "infra", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"},
	}})
	err := d.Ensure(t.Context(), []runnercontract.EnsureObject{
		{Kind: "Secret", Name: "sw-pull-flux", PoolID: "pool-1", Registry: "ghcr.io", Username: "robot", Password: "x"},
		{Kind: "Secret", Name: "sw-pull-ghcr", PoolID: "pool-1", Registry: "ghcr.io", Username: "robot", Password: "pull-s3cret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if flux := f.get("/api/v1/namespaces/jobs/secrets/sw-pull-flux"); flux["stringData"] != nil {
		t.Error("the driver wrote to a Flux-managed Secret")
	}
	ours := f.get("/api/v1/namespaces/jobs/secrets/sw-pull-ghcr")
	if ours == nil || ours["type"] != "kubernetes.io/dockerconfigjson" {
		t.Fatalf("created pull Secret = %v", ours)
	}
	if !strings.Contains(ours["stringData"].(map[string]any)[".dockerconfigjson"].(string), "pull-s3cret") {
		t.Error("the pull Secret does not carry its credential")
	}
	meta := ours["metadata"].(map[string]any)
	if meta["labels"].(map[string]any)[k8s.LabelManagedBy] != k8s.ManagedByLauncher || meta["annotations"].(map[string]any)[k8s.AnnotationPoolID] != "pool-1" {
		t.Errorf("created pull Secret metadata = %v", meta)
	}
}

// A long-lived object another manager takes over between the launcher's read and its write is not
// written: the apply carries the resourceVersion the launcher read, so the API server refuses it,
// and the launcher leaves the object to the next report, which sees the new manager (#933, D8,
// AC49b). A write without the precondition would force the fields back from Flux.
func TestKubernetesEnsureNeverOverwritesAHandOverMadeAfterItsRead(t *testing.T) {
	f, d := newKubeDriver(t)
	path := "/api/v1/namespaces/jobs/secrets/sw-pull-ghcr"
	f.put(path, map[string]any{"metadata": map[string]any{
		"name": "sw-pull-ghcr", "namespace": "jobs", "resourceVersion": "7",
		"labels": map[string]any{k8s.LabelManagedBy: k8s.ManagedByLauncher},
	}})
	flux := map[string]any{"metadata": map[string]any{
		"name": "sw-pull-ghcr", "namespace": "jobs", "resourceVersion": "8",
		"labels": map[string]any{"kustomize.toolkit.fluxcd.io/name": "infra", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"},
	}, "stringData": map[string]any{".dockerconfigjson": "flux"}}
	f.beforePatch = func(p string) {
		if p == path {
			f.objects[p] = flux
		}
	}
	err := d.Ensure(t.Context(), []runnercontract.EnsureObject{
		{Kind: "Secret", Name: "sw-pull-ghcr", PoolID: "pool-1", Registry: "ghcr.io", Username: "robot", Password: "pull-s3cret"},
	})
	if err != nil {
		t.Fatalf("a lost race is the next report's to decide, not an error: %v", err)
	}
	got := f.get(path)
	if got["stringData"].(map[string]any)[".dockerconfigjson"] != "flux" {
		t.Fatalf("the launcher overwrote the object Flux took over: %v", got)
	}
}

// The launcher's write of an object it already manages carries the resourceVersion it read.
func TestKubernetesEnsureWritesWhatItReadOnly(t *testing.T) {
	f, d := newKubeDriver(t)
	path := "/api/v1/namespaces/jobs/serviceaccounts/sw-sa"
	f.put(path, map[string]any{"metadata": map[string]any{
		"name": "sw-sa", "namespace": "jobs", "resourceVersion": "3",
		"labels": map[string]any{k8s.LabelManagedBy: k8s.ManagedByLauncher},
	}})
	if err := d.Ensure(t.Context(), []runnercontract.EnsureObject{{Kind: "ServiceAccount", Name: "sw-sa", PoolID: "pool-1"}}); err != nil {
		t.Fatal(err)
	}
	if rv := f.get(path)["metadata"].(map[string]any)["resourceVersion"]; rv == "3" {
		t.Fatalf("the launcher's write did not land (resourceVersion still %v)", rv)
	}
}

// An object another manager creates between the launcher's read (not found) and its write is not
// taken: the launcher creates rather than applies, the API server answers AlreadyExists, and the
// next report sees the other manager and refuses the clash (#933, D8, AC49a).
func TestKubernetesEnsureNeverTakesAnObjectCreatedAfterItsRead(t *testing.T) {
	f, d := newKubeDriver(t)
	path := "/api/v1/namespaces/jobs/secrets/sw-pull-ghcr"
	flux := map[string]any{"metadata": map[string]any{
		"name": "sw-pull-ghcr", "namespace": "jobs", "resourceVersion": "5",
		"labels": map[string]any{"kustomize.toolkit.fluxcd.io/name": "infra", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"},
	}, "stringData": map[string]any{".dockerconfigjson": "flux"}}
	race := func(p string) {
		if p == path {
			f.objects[p] = flux
		}
	}
	f.beforePost, f.beforePatch = race, race
	err := d.Ensure(t.Context(), []runnercontract.EnsureObject{
		{Kind: "Secret", Name: "sw-pull-ghcr", PoolID: "pool-1", Registry: "ghcr.io", Username: "robot", Password: "pull-s3cret"},
	})
	if err != nil {
		t.Fatalf("a lost race is the next report's to decide, not an error: %v", err)
	}
	got := f.get(path)
	if got["stringData"].(map[string]any)[".dockerconfigjson"] != "flux" || got["metadata"].(map[string]any)["labels"].(map[string]any)[k8s.LabelManagedBy] != nil {
		t.Fatalf("the launcher took an object Flux created after its read: %v", got)
	}
}

// waitingPod is a pod whose named container waits for reason, created at created.
func waitingPod(init bool, reason, message string, created time.Time) k8s.Pod {
	st := []k8s.ContainerStatus{{Name: "executor", State: k8s.ContainerState{Waiting: &k8s.WaitingContainer{Reason: reason, Message: message}}}}
	status := k8s.PodStatus{Phase: "Pending", ContainerStatuses: st}
	if init {
		st[0].Name = k8s.InjectExecutorContainer
		status = k8s.PodStatus{Phase: "Pending", InitContainerStatuses: st}
	}
	return pod(status, false, created)
}

// Under always-pull a few seconds of registry trouble must not fail a job: ErrImagePull and
// ImagePullBackOff stay non-terminal until the placement timeout, then end the job with the
// kubelet's reason and message; InvalidImageName and the CreateContainer reasons stay immediate
// (one-launcher spec, A3, OD21, AC43).
func TestKubernetesPullErrorsWaitForThePlacementTimeout(t *testing.T) {
	const timeout = 5 * time.Minute
	fresh, expired := time.Now(), time.Now().Add(-timeout-time.Minute)
	msg := `failed to pull image "ghcr.io/vhco-pro/stackweaver-opentofu-runner:latest": dial tcp: i/o timeout`
	cases := []struct {
		name   string
		pod    k8s.Pod
		done   bool
		reason []string
	}{
		{"pull error, fresh", waitingPod(false, "ErrImagePull", msg, fresh), false, nil},
		{"pull back-off, fresh", waitingPod(false, "ImagePullBackOff", msg, fresh), false, nil},
		{"executor copy pull back-off, fresh", waitingPod(true, "ImagePullBackOff", msg, fresh), false, nil},
		{"pull error, past the timeout", waitingPod(false, "ErrImagePull", msg, expired), true, []string{"ErrImagePull", "stackweaver-opentofu-runner:latest"}},
		{"pull back-off, past the timeout", waitingPod(false, "ImagePullBackOff", msg, expired), true, []string{"ImagePullBackOff", "i/o timeout"}},
		{"executor copy pull back-off, past the timeout", waitingPod(true, "ImagePullBackOff", msg, expired), true, []string{k8s.InjectExecutorContainer, "ImagePullBackOff"}},
		{"invalid image name", waitingPod(false, "InvalidImageName", "couldn't parse image name", fresh), true, []string{"InvalidImageName"}},
		{"create container config error", waitingPod(false, "CreateContainerConfigError", "secret not found", fresh), true, []string{"CreateContainerConfigError"}},
		{"create container error", waitingPod(true, "CreateContainerError", "no such file", fresh), true, []string{"CreateContainerError"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exit, done := exitOf(c.pod, timeout)
			if done != c.done {
				t.Fatalf("done = %t (%+v), want %t", done, exit, c.done)
			}
			for _, s := range c.reason {
				if !strings.Contains(exit.Reason, s) {
					t.Errorf("reason %q does not name %q", exit.Reason, s)
				}
			}
		})
	}
}

// A pod stuck pulling past the placement timeout is removed and its job fails naming the image; a
// pod whose registry recovers before the timeout runs to completion (AC43).
func TestKubernetesWaitRemovesAPodStuckPulling(t *testing.T) {
	msg := `failed to pull image "ghcr.io/vhco-pro/stackweaver-ansible-runner:latest": connect: connection refused`
	setStatus := func(f *fakeKube, path string, created time.Time, status map[string]any) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.objects[path]["metadata"].(map[string]any)["creationTimestamp"] = created.UTC().Format(time.RFC3339)
		f.objects[path]["status"] = status
	}
	pulling := map[string]any{"phase": "Pending", "containerStatuses": []any{map[string]any{
		"name": "executor", "state": map[string]any{"waiting": map[string]any{"reason": "ImagePullBackOff", "message": msg}},
	}}}

	t.Run("past the timeout", func(t *testing.T) {
		f, d := newKubeDriver(t)
		d.PlacementTimeout = time.Minute
		p, err := d.Place(t.Context(), kubeRequest())
		if err != nil {
			t.Fatal(err)
		}
		path := "/api/v1/namespaces/jobs/pods/" + p.ID
		setStatus(f, path, time.Now().Add(-2*time.Minute), pulling)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		exit, err := d.Wait(ctx, p)
		if err != nil || exit.Code != exitCodeUnreported || !strings.Contains(exit.Reason, "stackweaver-ansible-runner:latest") {
			t.Fatalf("wait = %+v, %v; want an unreported failure naming the image", exit, err)
		}
		if f.get(path) != nil {
			t.Error("the pod stuck pulling was left in place")
		}
	})
	t.Run("the registry recovers", func(t *testing.T) {
		f, d := newKubeDriver(t)
		d.PlacementTimeout = time.Hour
		p, err := d.Place(t.Context(), kubeRequest())
		if err != nil {
			t.Fatal(err)
		}
		path := "/api/v1/namespaces/jobs/pods/" + p.ID
		setStatus(f, path, time.Now(), pulling)
		done := make(chan Exit, 1)
		go func() {
			exit, _ := d.Wait(t.Context(), p)
			done <- exit
		}()
		select {
		case exit := <-done:
			t.Fatalf("wait ended on a pull error within the timeout: %+v", exit)
		case <-time.After(200 * time.Millisecond):
		}
		f.finish(p.ID, 0, 0)
		select {
		case exit := <-done:
			if exit.Code != exitCodeCompleted {
				t.Errorf("exit = %+v, want the job completed", exit)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("wait did not end once the pod completed")
		}
	})
}
