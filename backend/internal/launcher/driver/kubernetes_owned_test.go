// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	k8s "github.com/michielvha/stackweaver/core/runnerdriver/kubernetes"
)

// write is one object write the driver sent to the API server.
type write struct {
	method, path string
	owners       int
	gated        bool
}

// recordingKube serves the fake API server and records every write's ownerReferences and, for a
// pod, whether it carries a scheduling gate. It answers 422, which the client does not retry, to
// the failNth write (counting from 1)
// whose path starts with failPrefix.
type recordingKube struct {
	*fakeKube
	mu         sync.Mutex
	writes     []write
	failPrefix string
	failNth    int
	seen       int
}

func (r *recordingKube) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodPatch || req.Method == http.MethodPost {
		body, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
		var obj struct {
			Metadata struct {
				Name            string `json:"name"`
				OwnerReferences []any  `json:"ownerReferences"`
			} `json:"metadata"`
			Spec struct {
				SchedulingGates []any `json:"schedulingGates"`
			} `json:"spec"`
		}
		_ = json.Unmarshal(body, &obj)
		path := req.URL.Path
		if req.Method == http.MethodPost {
			path += "/" + obj.Metadata.Name
		}
		r.mu.Lock()
		fail := false
		if r.failPrefix != "" && strings.HasPrefix(path, r.failPrefix) {
			r.seen++
			fail = r.seen == r.failNth
		}
		r.writes = append(r.writes, write{method: req.Method, path: path, owners: len(obj.Metadata.OwnerReferences), gated: len(obj.Spec.SchedulingGates) > 0})
		r.mu.Unlock()
		if fail {
			http.Error(w, `{"kind":"Status","message":"injected failure","code":422}`, http.StatusUnprocessableEntity)
			return
		}
	}
	r.fakeKube.ServeHTTP(w, req)
}

func newRecordingDriver(t *testing.T) (*recordingKube, *Kubernetes) {
	t.Helper()
	r := &recordingKube{fakeKube: &fakeKube{objects: map[string]map[string]any{}}}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return r, &Kubernetes{
		Client:    &k8s.Client{Server: srv.URL, HTTP: srv.Client(), Token: func() (string, error) { return "t", nil }},
		Namespace: "jobs", PlatformImage: "runner:1", ExecutorPath: "/usr/local/bin/runner", PollInterval: 10 * time.Millisecond,
	}
}

func (r *recordingKube) writesTo(kind string) []write {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []write
	for _, w := range r.writes {
		if strings.Contains(w.path, "/"+kind+"/") {
			out = append(out, w)
		}
	}
	return out
}

func (r *recordingKube) indexOf(match func(write) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, w := range r.writes {
		if match(w) {
			return i
		}
	}
	return -1
}

// A job's Secrets never exist without their pod as owner: the pod is created first, held from
// scheduling by a gate, each Secret is written once with its ownerReference, and the gate is
// lifted only after them, so the kubelet never starts a pod whose Secret is missing (#933, D8,
// AC46). A Secret without an owner would be a top-level object Argo CD reports as orphaned, and
// leaked if the launcher died between two writes.
func TestKubernetesJobSecretsAreNeverWithoutTheirPod(t *testing.T) {
	r, d := newRecordingDriver(t)
	req := kubeRequest()
	req.Image = "registry.example/tofu:1"
	req.PullAuth = &RegistryAuth{Registry: "registry.example", Username: "u", Password: "p"}
	p, err := d.Place(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	secrets := r.writesTo("secrets")
	if len(secrets) != 2 {
		t.Fatalf("secret writes %+v, want the token Secret and the pull Secret once each", secrets)
	}
	for _, w := range secrets {
		if w.owners != 1 {
			t.Errorf("%s %s was written with %d owners, want its pod", w.method, w.path, w.owners)
		}
	}
	pods := r.writesTo("pods")
	if len(pods) != 2 || !pods[0].gated || pods[1].gated {
		t.Fatalf("pod writes %+v, want a gated create and then the gate lifted", pods)
	}
	firstPod := r.indexOf(func(w write) bool { return strings.Contains(w.path, "/pods/") })
	lifted := r.indexOf(func(w write) bool { return strings.Contains(w.path, "/pods/") && !w.gated })
	for _, s := range secrets {
		i := r.indexOf(func(w write) bool { return w == s })
		if i < firstPod || i > lifted {
			t.Errorf("%s written at %d, want between the pod's create (%d) and its release (%d)", s.path, i, firstPod, lifted)
		}
	}
	pod := r.get("/api/v1/namespaces/jobs/pods/" + p.ID)
	if gates, _ := pod["spec"].(map[string]any)["schedulingGates"].([]any); len(gates) != 0 {
		t.Errorf("the placed pod is still gated: %v", gates)
	}
}

// A per-job ServiceAccount must exist before its pod, which the API server admits only with its
// ServiceAccount present; it is owned by the pod before the pod is released, so it never outlives
// a pod that ran (#933, D8, AC43, AC46).
func TestKubernetesPerJobServiceAccountIsOwnedBeforeThePodRuns(t *testing.T) {
	r, d := newRecordingDriver(t)
	req := kubeRequest()
	req.Identity = &Identity{Mechanism: "aws-irsa", ServiceAccount: "sw-identity-deploy", PerJob: true, Annotations: map[string]string{"eks.amazonaws.com/role-arn": "arn:aws:iam::1:role/deploy"}}
	p, err := d.Place(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	sas := r.writesTo("serviceaccounts")
	if len(sas) != 2 || sas[1].owners != 1 {
		t.Fatalf("service account writes %+v, want a create before the pod and then its owner", sas)
	}
	created := r.indexOf(func(w write) bool { return w == sas[0] })
	firstPod := r.indexOf(func(w write) bool { return strings.Contains(w.path, "/pods/") })
	owned := r.indexOf(func(w write) bool { return w == sas[1] })
	lifted := r.indexOf(func(w write) bool { return strings.Contains(w.path, "/pods/") && !w.gated })
	if created >= firstPod || owned <= firstPod || owned >= lifted {
		t.Errorf("writes %+v: want the ServiceAccount created before the pod and owned before its release", r.writes)
	}
	pod := r.get("/api/v1/namespaces/jobs/pods/" + p.ID)
	if pod["spec"].(map[string]any)["serviceAccountName"] != p.ID {
		t.Errorf("the pod runs as %v, want its per-job ServiceAccount", pod["spec"].(map[string]any)["serviceAccountName"])
	}
}

// A placement that fails part-way leaves nothing behind: no pod, no Secret, no ServiceAccount,
// whichever write failed (#933, D8).
func TestKubernetesFailedPlacementLeavesNothing(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		nth    int
	}{
		{"the gated pod create", "/api/v1/namespaces/jobs/pods/", 1},
		{"the token Secret", "/api/v1/namespaces/jobs/secrets/", 1},
		{"the pull Secret", "/api/v1/namespaces/jobs/secrets/", 2},
		{"the ServiceAccount's owner", "/api/v1/namespaces/jobs/serviceaccounts/", 2},
		{"the pod's release", "/api/v1/namespaces/jobs/pods/", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, d := newRecordingDriver(t)
			r.failPrefix, r.failNth = c.prefix, c.nth
			req := kubeRequest()
			req.Image = "registry.example/tofu:1"
			req.PullAuth = &RegistryAuth{Registry: "registry.example", Username: "u", Password: "p"}
			req.Identity = &Identity{Mechanism: "aws-irsa", ServiceAccount: "sw-identity-deploy", PerJob: true}
			if _, err := d.Place(t.Context(), req); err == nil {
				t.Fatal("the placement succeeded despite the failed write")
			}
			r.fakeKube.mu.Lock()
			defer r.fakeKube.mu.Unlock()
			for path := range r.objects {
				t.Errorf("%s left behind", path)
			}
		})
	}
}

func pod(status k8s.PodStatus, gated bool, created time.Time) k8s.Pod {
	p := k8s.Pod{Status: status}
	p.CreationTimestamp = created.UTC().Format(time.RFC3339)
	if gated {
		p.Spec.SchedulingGates = []k8s.PodSchedulingGate{{Name: k8s.SchedulingGateJobObjects}}
	}
	return p
}

// An init container that cannot start or that fails ends the job with its reason, never leaves it
// pending until its timeout; a pod that failed without its executor ending is a failure the
// launcher reports, not one the executor did (F3, #933, D4, AC71).
func TestKubernetesExitOfReadsInitContainersAndUnreportedFailures(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name   string
		pod    k8s.Pod
		done   bool
		reason string
	}{
		{"init refused", pod(k8s.PodStatus{Phase: "Pending", InitContainerStatuses: []k8s.ContainerStatus{{Name: "inject-executor", State: k8s.ContainerState{Waiting: &k8s.WaitingContainer{Reason: "CreateContainerConfigError", Message: "container has runAsNonRoot and image has non-numeric user (stackweaver), cannot verify user is non-root"}}}}}, false, now), true, "inject-executor"},
		{"init image missing", pod(k8s.PodStatus{Phase: "Pending", InitContainerStatuses: []k8s.ContainerStatus{{Name: "inject-executor", State: k8s.ContainerState{Waiting: &k8s.WaitingContainer{Reason: "ImagePullBackOff", Message: "back-off pulling"}}}}}, false, now), true, "ImagePullBackOff"},
		{"init failed", pod(k8s.PodStatus{Phase: "Failed", InitContainerStatuses: []k8s.ContainerStatus{{Name: "inject-executor", State: k8s.ContainerState{Terminated: &k8s.TerminatedState{ExitCode: 1, Reason: "Error", Message: "cp: can't create"}}}}}, false, now), true, "exit code 1"},
		{"init starting", pod(k8s.PodStatus{Phase: "Pending", InitContainerStatuses: []k8s.ContainerStatus{{Name: "inject-executor", State: k8s.ContainerState{Waiting: &k8s.WaitingContainer{Reason: "PodInitializing"}}}}}, false, now), false, ""},
		{"init done, executor running", pod(k8s.PodStatus{Phase: "Running", InitContainerStatuses: []k8s.ContainerStatus{{Name: "inject-executor", State: k8s.ContainerState{Terminated: &k8s.TerminatedState{ExitCode: 0, Reason: "Completed"}}}}, ContainerStatuses: []k8s.ContainerStatus{{Name: "executor", State: k8s.ContainerState{Running: &struct{}{}}}}}, false, now), false, ""},
		{"evicted", pod(k8s.PodStatus{Phase: "Failed", Reason: "Evicted", Message: "the node was low on memory"}, false, now), true, "Evicted"},
		{"gated, fresh", pod(k8s.PodStatus{Phase: "Pending"}, true, now), false, ""},
		{"gated, abandoned", pod(k8s.PodStatus{Phase: "Pending"}, true, now.Add(-10*time.Minute)), true, "never released"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exit, done := exitOf(c.pod)
			if done != c.done {
				t.Fatalf("done = %t (%+v), want %t", done, exit, c.done)
			}
			if !done {
				return
			}
			if exit.Code == exitCodeCompleted || exit.Code == exitCodeFailed || exit.Code == exitCodeStopped {
				t.Errorf("exit code %d is one the executor reports itself, so the launcher would never report this failure", exit.Code)
			}
			if !strings.Contains(exit.Reason, c.reason) {
				t.Errorf("reason %q does not name %q", exit.Reason, c.reason)
			}
		})
	}
}

// Wait ends at once on a refused init container, with its reason, instead of polling the pending
// pod until the job's timeout (F3).
func TestKubernetesWaitEndsOnARefusedInitContainer(t *testing.T) {
	f, d := newKubeDriver(t)
	req := kubeRequest()
	req.Image = "registry.example/tofu:1"
	p, err := d.Place(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/namespaces/jobs/pods/" + p.ID
	f.mu.Lock()
	f.objects[path]["status"] = map[string]any{"phase": "Pending", "initContainerStatuses": []any{map[string]any{
		"name": "inject-executor", "state": map[string]any{"waiting": map[string]any{"reason": "CreateContainerConfigError", "message": "image has non-numeric user (stackweaver), cannot verify user is non-root"}},
	}}}
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	exit, err := d.Wait(ctx, p)
	if err != nil || exit.Code != 127 || !strings.Contains(exit.Reason, "inject-executor") || !strings.Contains(exit.Reason, "non-numeric user") {
		t.Errorf("wait = %+v, %v; want exit 127 naming the init container and its message", exit, err)
	}
}

// The sweep removes a per-job Secret or ServiceAccount of this launcher left without an owner by
// a launcher that died mid-placement, once it is older than a placement takes, and leaves owned
// and fresh ones to their pod (#933, D8, AC46).
func TestKubernetesReapSweepsUnownedJobObjects(t *testing.T) {
	f, d := newKubeDriver(t)
	labels := map[string]any{k8s.LabelManagedBy: k8s.ManagedByLauncher, k8s.LabelRole: k8s.RoleJob, k8s.LabelRunner: "runner-1"}
	old := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	fresh := time.Now().UTC().Format(time.RFC3339)
	put := func(kind, name, created string, owned bool, l map[string]any) {
		meta := map[string]any{"name": name, "labels": l, "creationTimestamp": created}
		if owned {
			meta["ownerReferences"] = []any{map[string]any{"kind": "Pod", "name": "p", "uid": "u"}}
		}
		f.put("/api/v1/namespaces/jobs/"+kind+"/"+name, map[string]any{"metadata": meta})
	}
	put("secrets", "sw-job-leaked-1-aaaaaa", old, false, labels)
	put("serviceaccounts", "sw-job-leaked-1-aaaaaa", old, false, labels)
	put("secrets", "sw-job-placing-1-bbbbbb", fresh, false, labels)
	put("secrets", "sw-job-owned-1-cccccc", old, true, labels)
	other := map[string]any{k8s.LabelManagedBy: k8s.ManagedByLauncher, k8s.LabelRole: k8s.RoleJob, k8s.LabelRunner: "runner-2"}
	put("secrets", "sw-job-other-1-dddddd", old, false, other)
	pool := map[string]any{k8s.LabelManagedBy: k8s.ManagedByLauncher}
	put("secrets", "sw-pull-ghcr", old, false, pool)

	if err := d.Reap(t.Context(), map[string]string{LabelRunner: "runner-1"}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{
		"secrets/sw-job-leaked-1-aaaaaa":         false,
		"serviceaccounts/sw-job-leaked-1-aaaaaa": false,
		"secrets/sw-job-placing-1-bbbbbb":        true,
		"secrets/sw-job-owned-1-cccccc":          true,
		"secrets/sw-job-other-1-dddddd":          true,
		"secrets/sw-pull-ghcr":                   true,
	} {
		if got := f.get("/api/v1/namespaces/jobs/"+path) != nil; got != want {
			t.Errorf("%s present = %t, want %t", path, got, want)
		}
	}
}
