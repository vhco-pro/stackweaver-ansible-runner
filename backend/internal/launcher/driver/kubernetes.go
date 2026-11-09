// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
	k8s "github.com/michielvha/stackweaver/core/runnerdriver/kubernetes"
)

// Kubernetes places each job in a pod of its own through core/runnerdriver/kubernetes (#933, D4,
// D8, D11): the only Kubernetes placement there is, so an operator places pods the same way. The
// job token goes to a per-job Secret owned by the pod, a per_job_sa binding gets a per-job
// ServiceAccount owned by it too, and deleting the pod removes both.
type Kubernetes struct {
	Client *k8s.Client
	// Namespace is the job namespace.
	Namespace string
	// JobImages maps each engine to its job image (one-launcher spec, A3): a job naming no image
	// runs its engine's, and a run image's executor-copy init container runs it.
	JobImages map[string]string
	// ExecutorPath is the executor binary inside the job images, for a request that carries no
	// Command.
	ExecutorPath string
	// JobImagePull is the job pod's imagePullPolicy (RUNNER_JOB_IMAGE_PULL); the zero value leaves
	// the cluster's default.
	JobImagePull PullPolicy
	// Auth is the launcher's own registry credential (RUNNER_DOCKER_CONFIG, the chart's
	// launcher.jobImages.pullSecrets): a job pod pulls its job image with it through a per-job
	// Secret keyed by the job image's repository (one-launcher spec, A3, OD26).
	Auth RegistryCredentials
	// Template is the pod template an operator may own (a ConfigMap, D11).
	Template k8s.Template
	// RuntimeClass is the runtimeClassName every job pod gets unless the request names one.
	RuntimeClass string
	// RunAsUser is the uid jobs run as when the image's user is not numeric; zero keeps it.
	RunAsUser int64
	// KeepFailedFor is the keep window of a job whose pool sets none.
	KeepFailedFor time.Duration
	// PollInterval is how often Wait reads the pod; zero means two seconds.
	PollInterval time.Duration
	// PlacementTimeout is how long a job pod may fail to pull an image (ErrImagePull,
	// ImagePullBackOff) before its job fails; zero means DefaultPlacementTimeout.
	PlacementTimeout time.Duration

	mu sync.Mutex
	// killed are the pods Kill deleted: once gone, Wait reports them stopped, not lost.
	killed map[string]bool
}

// DefaultPlacementTimeout is how long a job pod may fail to pull an image when PlacementTimeout is
// unset: long enough to ride out a brief registry outage under always-pull, which the kubelet's
// back-off retries, short enough that an image that cannot be pulled fails its job (OD21).
const DefaultPlacementTimeout = 5 * time.Minute

func (d *Kubernetes) placementTimeout() time.Duration {
	if d.PlacementTimeout > 0 {
		return d.PlacementTimeout
	}
	return DefaultPlacementTimeout
}

// kubePullPolicy maps a pull policy to the pod's imagePullPolicy; the zero value maps to none.
var kubePullPolicy = map[PullPolicy]string{PullAlways: "Always", PullMissing: "IfNotPresent", PullNever: "Never"}

// labelKeys maps the driver's placement labels to the pod's.
var labelKeys = map[string]string{
	LabelPool: k8s.LabelPool, LabelRunner: k8s.LabelRunner, LabelJob: k8s.LabelJob,
	LabelAttempt: k8s.LabelAttempt, LabelKind: k8s.LabelKind, LabelRunType: k8s.LabelRunType,
}

const labelPodKeepFailedFor = "stackweaver.io/keep-failed-for"

// The executor's own exit codes (executor.ExitCode): it reported the job itself.
const (
	exitCodeCompleted = 0
	exitCodeFailed    = 1
	exitCodeStopped   = 3
)

// exitCodeUnreported is the code of a pod that ended without its executor reporting: an init
// container that could not start or failed, an eviction, a pod never released for scheduling. The
// launcher then reports the job failed with the exit's reason.
const exitCodeUnreported = 127

// gateAbandonedAfter is how long a job pod may stay gated before Wait takes it for one a launcher
// stopped placing: Place lifts the gate within its own few writes.
const gateAbandonedAfter = 2 * time.Minute

// unownedSweepAfter is how old an unowned per-job object must be before Reap removes it: older
// than any placement in progress.
const unownedSweepAfter = 5 * time.Minute

// Name is "kubernetes".
func (d *Kubernetes) Name() string { return NameKubernetes }

// Capabilities: a pod is isolated, limited, re-attachable and takes a runtime class.
func (d *Kubernetes) Capabilities() Capabilities {
	return Capabilities{EnforcesLimits: true, IsolatesNetwork: true, IsolatesFilesystem: true, SupportsReattach: true, SupportsRuntime: true}
}

// Ping lists the job namespace's pods, which the launcher's Role allows.
func (d *Kubernetes) Ping(ctx context.Context) error {
	var list k8s.PodList
	return d.Client.List(ctx, k8s.Pods, d.Namespace, map[string]string{k8s.LabelManagedBy: k8s.ManagedByLauncher}, &list)
}

func (d *Kubernetes) podRequest(req Request) k8s.PodRequest {
	env := make([]k8s.EnvVar, 0, len(req.Env))
	for _, kv := range req.Env {
		name, value, _ := strings.Cut(kv, "=")
		if name == runnercontract.EnvJobToken {
			continue // into the job Secret, never the pod spec
		}
		env = append(env, k8s.EnvVar{Name: name, Value: value})
	}
	// A pod enforces every limit it asks for, so the report names them all as enforced (AC24).
	if report := LimitsReport(req.Resources, podLimits(req.Resources)); report != "" {
		env = append(env, k8s.EnvVar{Name: EnvLimitsReport, Value: report})
	}
	labels := map[string]string{}
	for k, v := range req.Labels {
		if pk, ok := labelKeys[k]; ok && k != LabelJob && k != LabelAttempt && k != LabelKind && k != LabelRunType {
			labels[pk] = v
		}
	}
	if keep := firstPositive(req.KeepFailedFor, d.KeepFailedFor); keep > 0 {
		labels[labelPodKeepFailedFor] = strconv.Itoa(int(keep.Seconds()))
	}
	runtime := req.Runtime
	if runtime == "" {
		runtime = d.RuntimeClass
	}
	var identity *k8s.Identity
	if id := req.Identity; id != nil {
		identity = &k8s.Identity{Mechanism: k8s.Mechanism(id.Mechanism), ServiceAccount: id.ServiceAccount, Annotations: id.Annotations, PerJob: id.PerJob}
	}
	executorPath := d.ExecutorPath
	if len(req.Command) > 0 {
		executorPath = req.Command[0]
	}
	return k8s.PodRequest{
		Identity:  identity,
		Namespace: d.Namespace, JobID: req.JobID, Attempt: req.Attempt, Kind: req.Kind, RunType: req.RunType,
		Image: req.Image, PlatformImage: d.JobImages[req.Kind], ExecutorPath: executorPath,
		ImagePullPolicy: kubePullPolicy[d.JobImagePull],
		Env:             env, Token: req.Token, Labels: labels,
		Resources:    k8s.Resources{CPURequest: req.Resources.CPURequest, CPULimit: req.Resources.CPULimit, MemoryRequest: req.Resources.MemoryRequest, MemoryLimit: req.Resources.MemoryLimit},
		RuntimeClass: runtime, RunAsUser: d.RunAsUser,
	}
}

// Place writes the job's pod and its per-job objects so that no Secret exists without the pod as
// its owner (D8, AC46): a per-job ServiceAccount first, since the API server admits a pod only
// with its ServiceAccount present; then the pod, held from scheduling by a gate; then each Secret,
// once, owned by the pod; then the ServiceAccount's owner; and last the gate lifted, so the kubelet
// never starts the pod before its Secrets exist. A Secret written before its owner would be a
// top-level object Argo CD reports as orphaned and that leaks if the launcher dies between two
// writes. A placement that fails part-way removes what it wrote.
func (d *Kubernetes) Place(ctx context.Context, req Request) (Placement, error) {
	pr := d.podRequest(req)
	var per *k8s.ServiceAccount
	var written []jobObject
	undo := func() {
		bg := context.WithoutCancel(ctx)
		for i := len(written) - 1; i >= 0; i-- {
			_ = d.Client.Delete(bg, written[i].res, d.Namespace, written[i].name, 0)
		}
	}
	if pr.Identity != nil && pr.Identity.PerJob {
		sa := k8s.RenderJobServiceAccount(pr, k8s.ServiceAccountTemplate{Annotations: pr.Identity.Annotations})
		if err := d.Client.Apply(ctx, k8s.ServiceAccounts, d.Namespace, sa.Name, sa, nil); err != nil {
			return Placement{}, fmt.Errorf("creating the job's service account: %w", err)
		}
		written = append(written, struct {
			res  k8s.Resource
			name string
		}{k8s.ServiceAccounts, sa.Name})
		per = &sa
	}
	secret := k8s.RenderJobSecret(pr)
	// The per-job pull Secrets, each written once with the pod as its owner.
	var pulls []k8s.Secret
	if req.PullSecret != "" {
		// The pool's long-lived pull Secret, which the launcher or the operator keeps (D8).
		pr.PullSecrets = append(pr.PullSecrets, req.PullSecret)
	} else if a := req.PullAuth; a != nil {
		pulls = append(pulls, k8s.RenderJobPullSecret(pr, a.Registry, a.Username, a.Password))
	}
	// The launcher's own credential for the job image, which the job container or the executor
	// copy runs; keyed by the job image's repository, so no run image ever matches it (OD26).
	if s, ok := d.jobImagePullSecret(pr, d.JobImages[req.Kind]); ok {
		pulls = append(pulls, s)
	}
	for _, s := range pulls {
		pr.PullSecrets = append(pr.PullSecrets, s.Name)
	}
	pod := k8s.RenderPod(pr, d.Template)
	gated := pod
	gated.Spec.SchedulingGates = append(append([]k8s.PodSchedulingGate{}, pod.Spec.SchedulingGates...), k8s.PodSchedulingGate{Name: k8s.SchedulingGateJobObjects})
	var placed k8s.Pod
	if err := d.Client.Apply(ctx, k8s.Pods, d.Namespace, pod.Name, gated, &placed); err != nil {
		undo()
		return Placement{}, fmt.Errorf("creating the job pod: %w", err)
	}
	written = append(written, jobObject{k8s.Pods, pod.Name})
	// The per-job objects belong to the pod from their first write: deleting it garbage-collects them.
	k8s.OwnedBy(&secret.ObjectMeta, placed)
	if err := d.Client.Apply(ctx, k8s.Secrets, d.Namespace, secret.Name, secret, nil); err != nil {
		undo()
		return Placement{}, fmt.Errorf("creating the job secret: %w", err)
	}
	written = append(written, jobObject{k8s.Secrets, secret.Name})
	for _, pull := range pulls {
		k8s.OwnedBy(&pull.ObjectMeta, placed)
		if err := d.Client.Apply(ctx, k8s.Secrets, d.Namespace, pull.Name, pull, nil); err != nil {
			undo()
			return Placement{}, fmt.Errorf("creating the job's pull secret: %w", err)
		}
		written = append(written, jobObject{k8s.Secrets, pull.Name})
	}
	if per != nil {
		k8s.OwnedBy(&per.ObjectMeta, placed)
		if err := d.Client.Apply(ctx, k8s.ServiceAccounts, d.Namespace, per.Name, per, nil); err != nil {
			undo()
			return Placement{}, fmt.Errorf("handing the service account to its pod: %w", err)
		}
	}
	// Lifting the gate: the same apply without it, which drops the gate the launcher owns.
	if err := d.Client.Apply(ctx, k8s.Pods, d.Namespace, pod.Name, pod, &placed); err != nil {
		undo()
		return Placement{}, fmt.Errorf("releasing the job pod for scheduling: %w", err)
	}
	return Placement{ID: placed.Name, Node: placed.Spec.NodeName, Labels: placementLabels(placed), Applied: podLimits(req.Resources)}, nil
}

// jobImagePullSecret is the per-job Secret that carries the launcher's credential for jobImage's
// registry, when it has one. An identitytoken entry is left out: a dockerconfigjson Secret carries
// a username and password, and the launcher warns about it at start.
func (d *Kubernetes) jobImagePullSecret(pr k8s.PodRequest, jobImage string) (k8s.Secret, bool) {
	if jobImage == "" {
		return k8s.Secret{}, false
	}
	a := d.Auth.For(jobImage)
	if a == nil || a.Username == "" {
		return k8s.Secret{}, false
	}
	return k8s.RenderJobImagePullSecret(pr, ImageRepository(jobImage), a.Username, a.Password), true
}

// jobObject is one object Place wrote, which a failed placement deletes.
type jobObject struct {
	res  k8s.Resource
	name string
}

// podLimits are the limits a pod enforces: every one the request sets, as "cpu=1" and "memory=1Gi".
func podLimits(r Resources) []string {
	var applied []string
	if r.CPULimit != "" {
		applied = append(applied, "cpu="+r.CPULimit)
	}
	if r.MemoryLimit != "" {
		applied = append(applied, "memory="+r.MemoryLimit)
	}
	return applied
}

// placementLabels reads a pod's labels back into the driver's keys; the job id is the
// annotation's, unchanged.
func placementLabels(p k8s.Pod) map[string]string {
	out := map[string]string{LabelManaged: "true"}
	for dk, pk := range labelKeys {
		if v, ok := p.Labels[pk]; ok {
			out[dk] = v
		}
	}
	if id := p.Annotations[k8s.AnnotationJobID]; id != "" {
		out[LabelJob] = id
	}
	return out
}

func (d *Kubernetes) pollInterval() time.Duration {
	if d.PollInterval > 0 {
		return d.PollInterval
	}
	return 2 * time.Second
}

// exitOf reads a finished pod's executor exit, or false while it runs. A pod that ends without
// its executor ending (an init container that cannot start or failed, an eviction, a pod a
// launcher stopped placing and never released) ends with exitCodeUnreported and the reason, so
// the launcher reports the job failed rather than waiting for its timeout.
func exitOf(p k8s.Pod, pullTimeout time.Duration) (Exit, bool) {
	pullExpired := podAge(p) > pullTimeout
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name == "executor" && cs.State.Terminated != nil {
			reason := ""
			if r := cs.State.Terminated.Reason; r != "" && r != "Completed" && r != "Error" {
				reason = r
			}
			return Exit{Code: cs.State.Terminated.ExitCode, Reason: reason}, true
		}
		if cs.Name == "executor" && cs.State.Waiting != nil && isStuck(cs.State.Waiting.Reason, pullExpired) {
			return Exit{Code: exitCodeUnreported, Reason: cs.State.Waiting.Reason + ": " + cs.State.Waiting.Message}, true
		}
	}
	for _, cs := range p.Status.InitContainerStatuses {
		if w := cs.State.Waiting; w != nil && isStuck(w.Reason, pullExpired) {
			return Exit{Code: exitCodeUnreported, Reason: fmt.Sprintf("init container %s: %s: %s", cs.Name, w.Reason, w.Message)}, true
		}
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
			reason := fmt.Sprintf("init container %s ended with exit code %d", cs.Name, t.ExitCode)
			if detail := strings.TrimSpace(strings.Trim(t.Reason+": "+t.Message, ": ")); detail != "" && detail != "Error" {
				reason += " (" + detail + ")"
			}
			return Exit{Code: exitCodeUnreported, Reason: reason}, true
		}
	}
	switch p.Status.Phase {
	case "Succeeded":
		return Exit{Code: 0}, true
	case "Failed":
		// The executor did not end on its own: it never reported.
		reason := strings.TrimSpace(strings.Trim(p.Status.Reason+": "+p.Status.Message, ": "))
		if reason == "" {
			reason = "the pod failed before its job ended"
		}
		return Exit{Code: exitCodeUnreported, Reason: reason}, true
	}
	if gatedBy(p, k8s.SchedulingGateJobObjects) {
		if created, err := time.Parse(time.RFC3339, p.CreationTimestamp); err == nil && time.Since(created) > gateAbandonedAfter {
			return Exit{Code: exitCodeUnreported, Reason: "the pod was never released for scheduling: its launcher stopped while placing it"}, true
		}
	}
	return Exit{}, false
}

// gatedBy reports whether a pod still carries the named scheduling gate.
func gatedBy(p k8s.Pod, gate string) bool {
	for _, g := range p.Spec.SchedulingGates {
		if g.Name == gate {
			return true
		}
	}
	return false
}

// isStuck is a waiting reason a pod does not recover from by itself. A failed pull is one only once
// pullExpired: the kubelet retries it, and a registry that comes back lets the pod start (OD21).
func isStuck(reason string, pullExpired bool) bool {
	switch reason {
	case "ErrImagePull", "ImagePullBackOff":
		return pullExpired
	case "InvalidImageName", "CreateContainerConfigError", "CreateContainerError":
		return true
	}
	return false
}

// isPulling reports whether a container of the pod waits on a failed pull.
func isPulling(p k8s.Pod) bool {
	for _, cs := range slices.Concat(p.Status.ContainerStatuses, p.Status.InitContainerStatuses) {
		if w := cs.State.Waiting; w != nil && (w.Reason == "ErrImagePull" || w.Reason == "ImagePullBackOff") {
			return true
		}
	}
	return false
}

// podAge is how long ago the pod was created; a pod without a readable creation time counts as
// old, so a pull error on it ends its job as it always did.
func podAge(p k8s.Pod) time.Duration {
	created, err := time.Parse(time.RFC3339, p.CreationTimestamp)
	if err != nil {
		return time.Duration(math.MaxInt64)
	}
	return time.Since(created)
}

// Wait polls the pod until its executor ends.
func (d *Kubernetes) Wait(ctx context.Context, p Placement) (Exit, error) {
	t := time.NewTicker(d.pollInterval())
	defer t.Stop()
	for {
		var pod k8s.Pod
		err := d.Client.Get(ctx, k8s.Pods, d.Namespace, p.ID, &pod)
		if errors.Is(err, k8s.ErrNotFound) {
			d.mu.Lock()
			killed := d.killed[p.ID]
			d.mu.Unlock()
			if killed {
				return Exit{Code: exitCodeStopped}, nil
			}
			return Exit{}, ErrNotFound
		}
		if err == nil {
			if exit, done := exitOf(pod, d.placementTimeout()); done {
				// A pod that never got its image has nothing to inspect, and would keep pulling.
				if isPulling(pod) {
					_ = d.Kill(context.WithoutCancel(ctx), p, 0)
				}
				return exit, nil
			}
		}
		select {
		case <-ctx.Done():
			return Exit{}, ctx.Err()
		case <-t.C:
		}
	}
}

// ImageDigest is the digest of the image the pod's executor runs, from its container status once
// the kubelet has the image: a pulled image's repository digest, or a node-loaded image's id.
func (d *Kubernetes) ImageDigest(ctx context.Context, p Placement) (string, error) {
	var pod k8s.Pod
	if err := d.Client.Get(ctx, k8s.Pods, d.Namespace, p.ID, &pod); err != nil {
		return "", err
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != "executor" {
			continue
		}
		id := cs.ImageID
		if _, digest, ok := strings.Cut(id, "@"); ok {
			return digest, nil
		}
		if strings.HasPrefix(id, "sha256:") {
			return id, nil
		}
	}
	return "", nil
}

// Kill deletes the pod with grace for the executor to report.
func (d *Kubernetes) Kill(ctx context.Context, p Placement, grace time.Duration) error {
	d.mu.Lock()
	if d.killed == nil {
		d.killed = map[string]bool{}
	}
	d.killed[p.ID] = true
	d.mu.Unlock()
	err := d.Client.Delete(ctx, k8s.Pods, d.Namespace, p.ID, int(grace.Seconds()))
	if errors.Is(err, k8s.ErrNotFound) {
		return nil
	}
	return err
}

// Remove deletes an ended pod (and with it its Secret and ServiceAccount), except a failed one
// kept for inspection, which Reap removes later (AC25).
func (d *Kubernetes) Remove(ctx context.Context, p Placement) error {
	var pod k8s.Pod
	err := d.Client.Get(ctx, k8s.Pods, d.Namespace, p.ID, &pod)
	if errors.Is(err, k8s.ErrNotFound) {
		return nil
	}
	if err == nil {
		if exit, done := exitOf(pod, d.placementTimeout()); done && !p.Canceled && exit.Code != 0 && keptFor(pod) > 0 {
			return nil
		}
	}
	return d.Kill(ctx, p, 0)
}

func keptFor(p k8s.Pod) time.Duration {
	n, err := strconv.Atoi(p.Labels[labelPodKeepFailedFor])
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// Reap removes this launcher's ended pods whose keep window has passed, and its per-job Secrets
// and ServiceAccounts a placement left without an owner (a launcher that stopped between writing
// one and handing it to its pod), once older than any placement in progress (D8, AC46).
func (d *Kubernetes) Reap(ctx context.Context, selector map[string]string) error {
	var list k8s.PodList
	if err := d.Client.List(ctx, k8s.Pods, d.Namespace, podSelector(selector), &list); err != nil {
		return err
	}
	for _, pod := range list.Items {
		exit, done := exitOf(pod, d.placementTimeout())
		if !done {
			continue
		}
		finished := finishedAt(pod)
		if exit.Code == 0 || finished.IsZero() || time.Since(finished) >= keptFor(pod) {
			_ = d.Kill(ctx, Placement{ID: pod.Name}, 0)
		}
	}
	for _, res := range []k8s.Resource{k8s.Secrets, k8s.ServiceAccounts} {
		var objs struct {
			Items []struct {
				Metadata k8s.ObjectMeta `json:"metadata"`
			} `json:"items"`
		}
		if err := d.Client.List(ctx, res, d.Namespace, podSelector(selector), &objs); err != nil {
			return err
		}
		for _, o := range objs.Items {
			created, err := time.Parse(time.RFC3339, o.Metadata.CreationTimestamp)
			if len(o.Metadata.OwnerReferences) > 0 || err != nil || time.Since(created) < unownedSweepAfter {
				continue
			}
			_ = d.Client.Delete(ctx, res, d.Namespace, o.Metadata.Name, 0)
		}
	}
	return nil
}

func finishedAt(p k8s.Pod) time.Time {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Terminated != nil {
			if t, err := time.Parse(time.RFC3339, cs.State.Terminated.FinishedAt); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

// podSelector maps the driver's label selector onto the pod's labels.
func podSelector(selector map[string]string) map[string]string {
	out := map[string]string{k8s.LabelManagedBy: k8s.ManagedByLauncher, k8s.LabelRole: k8s.RoleJob}
	for k, v := range selector {
		if pk, ok := labelKeys[k]; ok && k != LabelJob {
			out[pk] = v
		}
	}
	return out
}

// List returns the job pods carrying every label in selector, running or ended.
func (d *Kubernetes) List(ctx context.Context, selector map[string]string) ([]Placement, error) {
	var list k8s.PodList
	if err := d.Client.List(ctx, k8s.Pods, d.Namespace, podSelector(selector), &list); err != nil {
		return nil, err
	}
	out := make([]Placement, 0, len(list.Items))
	for _, pod := range list.Items {
		if job := selector[LabelJob]; job != "" && pod.Annotations[k8s.AnnotationJobID] != job {
			continue
		}
		_, ended := exitOf(pod, d.placementTimeout())
		out = append(out, Placement{ID: pod.Name, Node: pod.Spec.NodeName, Labels: placementLabels(pod), Ended: ended})
	}
	return out, nil
}

// Logs streams the executor container's log.
func (d *Kubernetes) Logs(ctx context.Context, p Placement) (io.ReadCloser, error) {
	return d.Client.Logs(ctx, d.Namespace, p.ID, "executor")
}

// KubernetesOptions configures the Kubernetes driver from the launcher's environment.
type KubernetesOptions struct {
	// Namespace is the job namespace; empty uses the launcher's own.
	Namespace string
	// JobImages maps each engine to its job image.
	JobImages    map[string]string
	ExecutorPath string
	// JobImagePull is the job pod's imagePullPolicy.
	JobImagePull PullPolicy
	// Auth is the launcher's own registry credential, which job pods pull their job image with.
	Auth RegistryCredentials
	// PlacementTimeout is how long a job pod may fail to pull an image before its job fails.
	PlacementTimeout time.Duration
	// TemplateConfigMap names the ConfigMap holding the pod template, if any.
	TemplateConfigMap string
	RuntimeClass      string
	RunAsUser         int64
	KeepFailedFor     time.Duration
	// APIEndpoint is the API's address:port as job pods reach it, opened in the network policy.
	APIEndpoint string
	// AllowEndpoints are further address:port pairs job pods reach inside the cluster's ranges: an
	// air-gapped install's in-cluster mirrors (RUNNER_ALLOW_TCP).
	AllowEndpoints []string
	// ClusterCIDRs are the pod and service ranges no job reaches but for the API and DNS.
	ClusterCIDRs []string
	// HostIdentity is the launcher's host-identity network mode: the metadata ranges stay open.
	HostIdentity bool
	// LeaveNetworkPolicy leaves the job namespace's policies to whoever installed them (the
	// chart renders its own), so the launcher applies none.
	LeaveNetworkPolicy bool
}

// NewKubernetes builds the driver in the launcher's pod: the in-cluster client, the pod template
// from its ConfigMap, and the job namespace's network policies applied (D5).
func NewKubernetes(ctx context.Context, o KubernetesOptions) (*Kubernetes, error) {
	client, own, err := k8s.InCluster()
	if err != nil {
		return nil, err
	}
	ns := o.Namespace
	if ns == "" {
		ns = own
	}
	d := &Kubernetes{
		Client: client, Namespace: ns, JobImages: o.JobImages, ExecutorPath: o.ExecutorPath, JobImagePull: o.JobImagePull, Auth: o.Auth, PlacementTimeout: o.PlacementTimeout,
		RuntimeClass: o.RuntimeClass, RunAsUser: o.RunAsUser, KeepFailedFor: o.KeepFailedFor,
	}
	if o.TemplateConfigMap != "" {
		var cm k8s.ConfigMap
		if err := client.Get(ctx, k8s.ConfigMaps, ns, o.TemplateConfigMap, &cm); err != nil {
			return nil, fmt.Errorf("reading the pod template %s: %w", o.TemplateConfigMap, err)
		}
		if d.Template, err = k8s.TemplateFromConfigMap(cm); err != nil {
			return nil, err
		}
	}
	cfg := k8s.NetworkConfig{Namespace: ns, ClusterCIDRs: o.ClusterCIDRs, HostIdentity: o.HostIdentity}
	for _, ep := range append([]string{o.APIEndpoint}, o.AllowEndpoints...) {
		if p, ok := endpointPeer(ep); ok {
			cfg.APIPeers = append(cfg.APIPeers, p)
		}
	}
	if o.LeaveNetworkPolicy {
		return d, nil
	}
	for _, pol := range k8s.RenderNetworkPolicies(cfg) {
		var existing k8s.NetworkPolicy
		err := client.Get(ctx, k8s.NetworkPolicies, ns, pol.Name, &existing)
		if err == nil {
			if werr := k8s.CheckWrite(&existing.ObjectMeta); werr != nil {
				// Declared outside Stackweaver (a GitOps-managed policy): used as it is (D8).
				continue
			}
		} else if !errors.Is(err, k8s.ErrNotFound) {
			return nil, fmt.Errorf("reading the job network policy %s: %w", pol.Name, err)
		}
		if err := client.Apply(ctx, k8s.NetworkPolicies, ns, pol.Name, pol, nil); err != nil {
			return nil, fmt.Errorf("applying the job network policy: %w", err)
		}
	}
	return d, nil
}

// Inventory reports the job namespace's long-lived ServiceAccounts and Secrets, and who manages
// each (D8): the per-job objects the driver creates for its pods are left out.
func (d *Kubernetes) Inventory(ctx context.Context) ([]runnercontract.InventoryEntry, error) {
	var out []runnercontract.InventoryEntry
	for _, r := range []struct {
		res  k8s.Resource
		kind string
	}{{k8s.ServiceAccounts, "ServiceAccount"}, {k8s.Secrets, "Secret"}} {
		var list struct {
			Items []struct {
				Metadata k8s.ObjectMeta `json:"metadata"`
			} `json:"items"`
		}
		if err := d.Client.List(ctx, r.res, d.Namespace, nil, &list); err != nil {
			return nil, err
		}
		for _, it := range list.Items {
			if it.Metadata.Labels[k8s.LabelRole] == k8s.RoleJob {
				continue
			}
			e := k8s.InventoryEntry(r.kind, d.Namespace, it.Metadata.Name, &it.Metadata)
			out = append(out, runnercontract.InventoryEntry{
				Kind: e.Kind, Namespace: e.Namespace, Name: e.Name, Present: e.Present, ManagedBy: e.ManagedBy, ExternalManager: e.ExternalManager,
			})
		}
	}
	return out, nil
}

// Ensure creates the identity ServiceAccounts and registry pull Secrets the pool needs, each only
// while Stackweaver alone manages its name: the pre-write check runs again here, so a stale report
// never causes a write to an object another manager took over (D8).
func (d *Kubernetes) Ensure(ctx context.Context, objs []runnercontract.EnsureObject) error {
	var errs []error
	for _, o := range objs {
		var res k8s.Resource
		var obj any
		var meta *k8s.ObjectMeta
		switch o.Kind {
		case "ServiceAccount":
			sa := k8s.RenderBindingServiceAccount(d.Namespace, o.Name, o.PoolID, o.Annotations)
			res, obj, meta = k8s.ServiceAccounts, &sa, &sa.ObjectMeta
		case "Secret":
			secret := k8s.RenderPullSecret(d.Namespace, o.Name, o.PoolID, k8s.DockerConfigJSON(o.Registry, o.Username, o.Password))
			res, obj, meta = k8s.Secrets, &secret, &secret.ObjectMeta
		default:
			continue
		}
		var existing struct {
			Metadata k8s.ObjectMeta `json:"metadata"`
		}
		err := d.Client.Get(ctx, res, d.Namespace, o.Name, &existing)
		switch {
		case errors.Is(err, k8s.ErrNotFound):
			// A create, not an apply: an object another manager creates after the read is refused
			// as AlreadyExists rather than taken over, and the next report sees that manager and
			// refuses the clash (D8). The launcher's later writes apply under the same field
			// manager and take the created fields over as their own.
			if err := d.Client.Create(ctx, res, d.Namespace, o.Name, obj, nil); err != nil && !isConflict(err) {
				errs = append(errs, err)
			}
			continue
		case err == nil:
			if werr := k8s.CheckWrite(&existing.Metadata); werr != nil {
				continue
			}
			// The write is conditional on the object as it was read: if another manager took it
			// over in between, the API server refuses the apply instead of letting the launcher
			// force the fields back, and the next report sees the new manager (D8).
			meta.ResourceVersion = existing.Metadata.ResourceVersion
		default:
			errs = append(errs, err)
			continue
		}
		if err := d.Client.Apply(ctx, res, d.Namespace, o.Name, obj, nil); err != nil && !isConflict(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// isConflict reports a 409 from the API server: an object created or changed by someone else
// since the launcher read it, which the next report decides on.
func isConflict(err error) bool {
	var se *k8s.StatusError
	return errors.As(err, &se) && se.Code == http.StatusConflict
}

// endpointPeer is the network policy peer of an address:port.
func endpointPeer(ep string) (k8s.Peer, bool) {
	host, port, err := net.SplitHostPort(ep)
	if err != nil || host == "" {
		return k8s.Peer{}, false
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return k8s.Peer{}, false
	}
	cidr := host + "/32"
	if strings.Contains(host, ":") {
		cidr = host + "/128"
	}
	return k8s.Peer{CIDR: cidr, Port: n}, true
}
