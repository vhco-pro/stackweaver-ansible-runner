// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
	k8s "github.com/michielvha/stackweaver/core/runnerdriver/kubernetes"
)

// fakeKube stores what the driver applies, serves it back, lists by label and deletes, and lets a
// test finish a pod.
type fakeKube struct {
	mu      sync.Mutex
	objects map[string]map[string]any
	order   []string
	// beforePatch and beforePost run before a PATCH or a POST is applied, under the lock, as
	// another writer would act between a read and the launcher's write.
	beforePatch func(path string)
	beforePost  func(path string)
}

func writeJSON(w http.ResponseWriter, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil { //nolint:errchkjson // a test fake's answer
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (f *fakeKube) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	switch r.Method {
	case http.MethodPatch:
		var obj map[string]any
		_ = json.NewDecoder(r.Body).Decode(&obj)
		meta := obj["metadata"].(map[string]any)
		if f.beforePatch != nil {
			f.beforePatch(p)
		}
		// Server-side apply honours metadata.resourceVersion as a precondition.
		if want, ok := meta["resourceVersion"].(string); ok && want != "" {
			current := ""
			if old, ok := f.objects[p]; ok {
				current, _ = old["metadata"].(map[string]any)["resourceVersion"].(string)
			}
			if current != want {
				w.WriteHeader(http.StatusConflict)
				writeJSON(w, map[string]any{"kind": "Status", "message": "the object has been modified", "code": http.StatusConflict})
				return
			}
		}
		meta["resourceVersion"] = fmt.Sprintf("%d", len(f.order)+1)
		meta["uid"] = "uid-" + meta["name"].(string)
		if old, ok := f.objects[p]; ok {
			if st, ok := old["status"]; ok {
				obj["status"] = st
			}
		}
		f.objects[p] = obj
		f.order = append(f.order, r.Method+" "+p)
		writeJSON(w, obj)
	case http.MethodPost:
		var obj map[string]any
		_ = json.NewDecoder(r.Body).Decode(&obj)
		meta := obj["metadata"].(map[string]any)
		path := p + "/" + meta["name"].(string)
		if f.beforePost != nil {
			f.beforePost(path)
		}
		if _, ok := f.objects[path]; ok {
			w.WriteHeader(http.StatusConflict)
			writeJSON(w, map[string]any{"kind": "Status", "reason": "AlreadyExists", "message": "already exists", "code": http.StatusConflict})
			return
		}
		meta["uid"] = "uid-" + meta["name"].(string)
		meta["resourceVersion"] = fmt.Sprintf("%d", len(f.order)+1)
		f.objects[path] = obj
		f.order = append(f.order, r.Method+" "+path)
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, obj)
	case http.MethodGet:
		if strings.HasSuffix(p, "/log") {
			_, _ = io.WriteString(w, "log\n")
			return
		}
		if obj, ok := f.objects[p]; ok {
			writeJSON(w, obj)
			return
		}
		if !strings.HasSuffix(p, "/pods") && !strings.HasSuffix(p, "/serviceaccounts") && !strings.HasSuffix(p, "/secrets") {
			http.NotFound(w, r)
			return
		}
		items := []any{}
		for path, obj := range f.objects {
			if !strings.HasPrefix(path, p+"/") {
				continue
			}
			labels, _ := obj["metadata"].(map[string]any)["labels"].(map[string]any)
			ok := true
			for _, kv := range strings.Split(r.URL.Query().Get("labelSelector"), ",") {
				k, v, _ := strings.Cut(kv, "=")
				if kv != "" && labels[k] != v {
					ok = false
				}
			}
			if ok {
				items = append(items, obj)
			}
		}
		writeJSON(w, map[string]any{"items": items})
	case http.MethodDelete:
		if _, ok := f.objects[p]; !ok {
			http.NotFound(w, r)
			return
		}
		delete(f.objects, p)
		f.order = append(f.order, r.Method+" "+p)
		_, _ = io.WriteString(w, "{}")
	}
}

func (f *fakeKube) finish(pod string, code int, finishedAgo time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj := f.objects["/api/v1/namespaces/jobs/pods/"+pod]
	obj["status"] = map[string]any{"phase": "Failed", "containerStatuses": []any{map[string]any{
		"name": "executor", "state": map[string]any{"terminated": map[string]any{"exitCode": code, "finishedAt": time.Now().Add(-finishedAgo).UTC().Format(time.RFC3339)}},
	}}}
}

func (f *fakeKube) put(path string, obj map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[path] = obj
}

func (f *fakeKube) get(path string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objects[path]
}

func newKubeDriver(t *testing.T) (*fakeKube, *Kubernetes) {
	t.Helper()
	f := &fakeKube{objects: map[string]map[string]any{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, &Kubernetes{
		Client:    &k8s.Client{Server: srv.URL, HTTP: srv.Client(), Token: func() (string, error) { return "t", nil }},
		Namespace: "jobs", JobImages: sameJobImage("runner:1"), ExecutorPath: "/usr/local/bin/runner", PollInterval: 10 * time.Millisecond,
	}
}

func kubeRequest() Request {
	return Request{
		JobID: "run-Abc", Attempt: 1, Kind: "tofu", RunType: "plan",
		Env:    []string{"STACKWEAVER_SERVER=http://api", "STACKWEAVER_JOB_TOKEN=swj-secret"},
		Token:  "swj-secret",
		Labels: map[string]string{LabelManaged: "true", LabelRunner: "runner-1", LabelPool: "hosted-default", LabelJob: "run-Abc", LabelAttempt: "1"},
	}
}

// A job pod is placed with its token in a Secret the pod owns, never in the pod spec, and the
// placement reads back with the job id unchanged (#933, D4, D8, AC46).
func TestKubernetesPlaceKeepsTheTokenInTheJobSecret(t *testing.T) {
	f, d := newKubeDriver(t)
	p, err := d.Place(t.Context(), kubeRequest())
	if err != nil {
		t.Fatal(err)
	}
	pod := f.get("/api/v1/namespaces/jobs/pods/" + p.ID)
	if pod == nil {
		t.Fatalf("no pod %s; applied %v", p.ID, f.order)
	}
	raw, err := json.Marshal(pod) //nolint:errchkjson // the fake's own decoded document
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "swj-secret") {
		t.Error("the job token is in the pod spec")
	}
	secret := f.get("/api/v1/namespaces/jobs/secrets/" + p.ID)
	owners, _ := secret["metadata"].(map[string]any)["ownerReferences"].([]any)
	if secret["stringData"].(map[string]any)["token"] != "swj-secret" || len(owners) != 1 || owners[0].(map[string]any)["uid"] != "uid-"+p.ID {
		t.Errorf("job secret = %v", secret)
	}
	// The pod comes first, held by a scheduling gate, so the Secret is written once with its owner
	// and never exists unowned (an orphan to Argo CD, a leak if the launcher dies between two
	// writes); the gate is lifted last. This replaced the secret-before-pod order (AC46).
	if len(f.order) != 3 || !strings.HasPrefix(f.order[0], "PATCH /api/v1/namespaces/jobs/pods/") ||
		!strings.HasPrefix(f.order[1], "PATCH /api/v1/namespaces/jobs/secrets/") || !strings.HasPrefix(f.order[2], "PATCH /api/v1/namespaces/jobs/pods/") {
		t.Errorf("objects applied in order %v, want the gated pod, the owned secret, then the pod released", f.order)
	}
	list, err := d.List(t.Context(), map[string]string{LabelManaged: "true", LabelRunner: "runner-1"})
	if err != nil || len(list) != 1 || list[0].Labels[LabelJob] != "run-Abc" || list[0].Labels[LabelAttempt] != "1" {
		t.Errorf("list = %+v, %v", list, err)
	}
}

// Wait reads the executor's exit; a pod the driver killed and that is gone reads as stopped, not
// lost (#933, D3).
func TestKubernetesWaitAndKill(t *testing.T) {
	f, d := newKubeDriver(t)
	p, err := d.Place(t.Context(), kubeRequest())
	if err != nil {
		t.Fatal(err)
	}
	f.finish(p.ID, 1, 0)
	exit, err := d.Wait(t.Context(), p)
	if err != nil || exit.Code != 1 {
		t.Errorf("wait = %+v, %v", exit, err)
	}
	q, _ := d.Place(t.Context(), func() Request { r := kubeRequest(); r.JobID = "run-Def"; return r }())
	if err := d.Kill(t.Context(), q, time.Second); err != nil {
		t.Fatal(err)
	}
	if exit, err := d.Wait(t.Context(), q); err != nil || exit.Code != exitCodeStopped {
		t.Errorf("wait after kill = %+v, %v", exit, err)
	}
}

// A failed pod on a pool that keeps failures stays until its window has passed and the launcher
// sweeps it (#933, D4, AC25).
func TestKubernetesKeepsAFailedPodThenReapsIt(t *testing.T) {
	f, d := newKubeDriver(t)
	req := kubeRequest()
	req.KeepFailedFor = time.Hour
	p, err := d.Place(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	f.finish(p.ID, 1, time.Minute)
	if err := d.Remove(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if f.get("/api/v1/namespaces/jobs/pods/"+p.ID) == nil {
		t.Fatal("the failed pod was removed inside its keep window")
	}
	_ = d.Reap(t.Context(), map[string]string{LabelRunner: "runner-1"})
	if f.get("/api/v1/namespaces/jobs/pods/"+p.ID) == nil {
		t.Fatal("reaped inside its keep window")
	}
	f.finish(p.ID, 1, 2*time.Hour)
	_ = d.Reap(t.Context(), map[string]string{LabelRunner: "runner-1"})
	if f.get("/api/v1/namespaces/jobs/pods/"+p.ID) != nil {
		t.Error("the kept pod outlived its window")
	}
}

// A canceled job is not a failure: its stopped pod is removed at once even on a pool that keeps
// failures, so the cancel removes it within its ten seconds (#933, AC12).
func TestKubernetesRemovesACanceledPodDespiteTheKeepWindow(t *testing.T) {
	f, d := newKubeDriver(t)
	req := kubeRequest()
	req.KeepFailedFor = time.Hour
	p, err := d.Place(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	f.finish(p.ID, exitCodeStopped, 0)
	p.Canceled = true
	if err := d.Remove(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if f.get("/api/v1/namespaces/jobs/pods/"+p.ID) != nil {
		t.Error("the canceled job's pod was kept as a failure")
	}
}

// A per_job_sa binding gets a ServiceAccount of its own, owned by the pod (#933, D8, AC43).
func TestKubernetesPerJobServiceAccount(t *testing.T) {
	f, d := newKubeDriver(t)
	req := kubeRequest()
	req.Identity = &Identity{Mechanism: "aws-irsa", ServiceAccount: "sw-identity-deploy", PerJob: true, Annotations: map[string]string{"eks.amazonaws.com/role-arn": "arn:aws:iam::1:role/deploy"}}
	p, err := d.Place(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	sa := f.get("/api/v1/namespaces/jobs/serviceaccounts/" + p.ID)
	if sa == nil {
		t.Fatal("no per-job service account")
	}
	meta := sa["metadata"].(map[string]any)
	if meta["annotations"].(map[string]any)["eks.amazonaws.com/role-arn"] == nil || len(meta["ownerReferences"].([]any)) != 1 {
		t.Errorf("per-job service account = %v", meta)
	}
	pod := f.get("/api/v1/namespaces/jobs/pods/" + p.ID)
	if pod["spec"].(map[string]any)["serviceAccountName"] != p.ID {
		t.Errorf("the pod runs as %v", pod["spec"].(map[string]any)["serviceAccountName"])
	}
}

// The driver reports the job namespace's long-lived ServiceAccounts and Secrets with who manages
// them, and creates a pool's identity ServiceAccount only while nothing else manages that name
// (#933, D8, AC49a, AC49b).
func TestKubernetesInventoryAndEnsure(t *testing.T) {
	f, d := newKubeDriver(t)
	f.put("/api/v1/namespaces/jobs/serviceaccounts/sw-argo", map[string]any{"metadata": map[string]any{
		"name": "sw-argo", "namespace": "jobs", "annotations": map[string]any{"argocd.argoproj.io/tracking-id": "platform:/ServiceAccount:jobs/sw-argo"},
	}})
	if _, err := d.Place(t.Context(), kubeRequest()); err != nil {
		t.Fatal(err)
	}
	inv, err := d.Inventory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) != 1 || inv[0].Name != "sw-argo" || inv[0].ManagedBy != "external" || inv[0].ExternalManager != "Argo CD application platform" {
		t.Errorf("inventory = %+v, want the Argo CD account alone (per-job objects are not reported)", inv)
	}
	err = d.Ensure(t.Context(), []runnercontract.EnsureObject{
		{Kind: "ServiceAccount", Name: "sw-argo", PoolID: "pool-1", Annotations: map[string]string{"x": "y"}},
		{Kind: "ServiceAccount", Name: "sw-deploy", PoolID: "pool-1", Annotations: map[string]string{"eks.amazonaws.com/role-arn": "arn"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	argo := f.get("/api/v1/namespaces/jobs/serviceaccounts/sw-argo")
	if _, touched := argo["metadata"].(map[string]any)["annotations"].(map[string]any)["x"]; touched {
		t.Error("the driver wrote to an Argo CD object")
	}
	ours := f.get("/api/v1/namespaces/jobs/serviceaccounts/sw-deploy")
	meta := ours["metadata"].(map[string]any)
	if meta["labels"].(map[string]any)[k8s.LabelManagedBy] != k8s.ManagedByLauncher || meta["annotations"].(map[string]any)[k8s.AnnotationPoolID] != "pool-1" {
		t.Errorf("created account = %v", meta)
	}
}

// podContainers reads a placed pod's containers (or init containers) from the fake.
func podContainers(t *testing.T, f *fakeKube, pod, field string) []map[string]any {
	t.Helper()
	obj := f.get("/api/v1/namespaces/jobs/pods/" + pod)
	if obj == nil {
		t.Fatalf("no pod %s", pod)
	}
	list, _ := obj["spec"].(map[string]any)[field].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, c := range list {
		out = append(out, c.(map[string]any))
	}
	return out
}

// Each job runs in its engine's job image, started with the command the launcher sends for that
// engine, and a run image's executor-copy init container runs that engine's job image (one-launcher
// spec, A3, AC7).
func TestKubernetesPlacesEachEngineInItsJobImage(t *testing.T) {
	f, d := newKubeDriver(t)
	d.JobImages = map[string]string{"tofu": "ghcr.io/vhco-pro/stackweaver-opentofu-runner:latest", "ansible": "ghcr.io/vhco-pro/stackweaver-ansible-runner:latest"}
	d.ExecutorPath = "/not-the-executor"
	command := []string{"/usr/local/bin/stackweaver-launcher", "executor"}
	for i, tc := range []struct{ kind, runType, image string }{
		{"tofu", "plan", ""}, {"ansible", "", ""}, {"tofu", "apply", "registry.example.test/acme/tofu-custom:2"}, {"ansible", "", "registry.example.test/acme/ee:3"},
	} {
		req := kubeRequest()
		req.JobID, req.Attempt, req.Kind, req.RunType, req.Image, req.Command = fmt.Sprintf("job-%d", i), 1, tc.kind, tc.runType, tc.image, command
		p, err := d.Place(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		main := podContainers(t, f, p.ID, "containers")[0]
		inits := podContainers(t, f, p.ID, "initContainers")
		if tc.image == "" {
			if main["image"] != d.JobImages[tc.kind] {
				t.Errorf("the %s job runs %v, want its job image %s", tc.kind, main["image"], d.JobImages[tc.kind])
			}
			if got := fmt.Sprint(main["command"]); got != fmt.Sprint(command) {
				t.Errorf("the %s job's command = %s, want %v", tc.kind, got, command)
			}
			if len(inits) != 0 {
				t.Errorf("the %s job in its own job image has init containers %v", tc.kind, inits)
			}
			continue
		}
		if main["image"] != tc.image {
			t.Errorf("the %s run image job runs %v, want %s", tc.kind, main["image"], tc.image)
		}
		if len(inits) != 1 || inits[0]["image"] != d.JobImages[tc.kind] {
			t.Errorf("the %s run image's executor copy = %v, want one from %s", tc.kind, inits, d.JobImages[tc.kind])
		} else if got := fmt.Sprint(inits[0]["command"]); !strings.Contains(got, command[0]) {
			t.Errorf("the executor copy runs %s, want it to copy %s", got, command[0])
		}
	}
}

// RUNNER_JOB_IMAGE_PULL becomes the job pod's imagePullPolicy, on the job container and the
// executor-copy init container; unset leaves the cluster's default (OD18, AC40).
func TestKubernetesSetsTheImagePullPolicy(t *testing.T) {
	for pull, want := range map[PullPolicy]string{PullAlways: "Always", PullMissing: "IfNotPresent", PullNever: "Never", "": ""} {
		f, d := newKubeDriver(t)
		d.JobImagePull = pull
		req := kubeRequest()
		req.Image = "registry.example.test/acme/tofu-custom:2"
		p, err := d.Place(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		main := podContainers(t, f, p.ID, "containers")[0]
		inits := podContainers(t, f, p.ID, "initContainers")
		got, _ := main["imagePullPolicy"].(string)
		if got != want {
			t.Errorf("%q: job container imagePullPolicy = %q, want %q", pull, got, want)
		}
		if len(inits) != 1 {
			t.Fatalf("%q: init containers = %v", pull, inits)
		}
		if got, _ := inits[0]["imagePullPolicy"].(string); got != want {
			t.Errorf("%q: executor copy imagePullPolicy = %q, want %q", pull, got, want)
		}
	}
}
