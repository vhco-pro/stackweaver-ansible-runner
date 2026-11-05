// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// Package driver is where a launcher places an executor (#933, D4): in its own process, in a
// container on a Docker or Podman socket, or in a Kubernetes pod. Every placement implements one
// interface, and the launcher asks a driver what it isolates rather than assuming it. A new
// placement (Nomad, ECS, Firecracker) is a new package and a new RUNNER_DRIVER value, nothing else.
package driver

import (
	"context"
	"errors"
	"io"
	"time"
)

// Driver names, the RUNNER_DRIVER values.
const (
	NameInProcess  = "inprocess"
	NameContainer  = "container"
	NameKubernetes = "kubernetes"
)

// Labels every placement carries, so a restarted launcher finds its own placements and any
// launcher of a pool can find the placements of one that went away (D3).
const (
	LabelManaged = "sh.stackweaver.managed"
	LabelPool    = "sh.stackweaver.pool"
	LabelRunner  = "sh.stackweaver.runner"
	LabelJob     = "sh.stackweaver.job"
	LabelAttempt = "sh.stackweaver.attempt"
	LabelKind    = "sh.stackweaver.kind"
	LabelRunType = "sh.stackweaver.run-type"
)

// Capabilities is what a driver can promise. The pool page prints them, and the launcher refuses a
// pool setting a driver cannot honour (limits marked required, a sandboxed runtime).
type Capabilities struct {
	EnforcesLimits     bool
	IsolatesNetwork    bool
	IsolatesFilesystem bool
	SupportsReattach   bool
	SupportsRuntime    bool
}

// Resources are a placement's requests and limits. Class names a resource class a resolver may
// later map to a node pool or a runtime (future-proofing scenario 7); drivers ignore what they
// cannot apply and say so in Placement.Applied.
type Resources struct {
	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string
	// Required refuses a placement whose limits the driver cannot enforce.
	Required bool
	Class    string
}

// Identity is a job's identity binding: its mechanism (aws-irsa, aws-pod-identity,
// azure-workload-identity, gcp-workload-identity) and the ServiceAccount it runs as, with the
// mechanism's annotations; PerJob clones that account for each job (D8, AC43).
type Identity struct {
	Mechanism      string
	ServiceAccount string
	Annotations    map[string]string
	PerJob         bool
}

// Mount is a host directory or volume made visible to the job.
type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
}

// CacheMountPath is where a job reads its organization's node-local cache (#933, D7, decision 19),
// mounted read-only; the executor finds it through STACKWEAVER_CACHE_DIR.
const CacheMountPath = "/var/cache/stackweaver"

// CacheMount is the launcher's node-local copy of the job's organization's cache.
type CacheMount struct {
	// Dir is the organization's directory on the launcher's filesystem: what the in-process driver
	// hands the executor, and the container driver's bind source when Volume is empty.
	Dir string
	// Volume is the launcher's named volume holding every organization's directory; the container
	// driver mounts its Subpath, so a job sees its organization's entries and no other.
	Volume  string
	Subpath string
}

// RegistryAuth is a run image's registry credential, which the launcher fetched for the pull and
// which never reaches the job.
type RegistryAuth struct {
	Registry string
	Username string
	Password string //nolint:gosec // G117: the registry credential the pull needs
}

// Request is everything a driver needs to place one executor.
type Request struct {
	JobID   string
	Attempt int
	// Kind is the engine: "tofu" or "ansible".
	Kind    string
	RunType string
	// Image is the job image and Digest its resolved digest, when known.
	Image  string
	Digest string
	// Command is the executor's command line inside the image; drivers that run no image ignore it.
	Command []string
	// Env is the executor's whole environment (launcher.ExecutorEnv), carrying the job token.
	Env         []string
	Mounts      []Mount
	NetworkMode string
	Labels      map[string]string
	Resources   Resources
	// KeepFailedFor keeps a failed job's container or pod for inspection this long (the pool's
	// keep_failed_jobs_for); zero removes it at once.
	KeepFailedFor time.Duration
	// Identity is the identity binding the job runs with (D8), on drivers that place pods; nil
	// is the platform identity.
	Identity *Identity
	// Runtime is a sandboxed runtime name (runsc, kata) on drivers that support one.
	Runtime string
	// Cache is the node-local read-only cache to mount, nil for none.
	Cache *CacheMount
	// PullAuth is the registry credential the job's image is pulled with, nil for none (#933, D6).
	PullAuth *RegistryAuth
	// PullSecret is the pool's long-lived pull Secret for the image's registry (#933, D8): a pod
	// names it instead of a per-job Secret. A driver that cannot use one pulls with PullAuth.
	PullSecret string
	// Server, RunnerID and Token are the invocation again, for a driver that calls the executor in
	// memory rather than through Env (in-process), so the token never enters an environment.
	Server   string
	RunnerID string
	Token    string
}

// Placement is a placed executor.
type Placement struct {
	// ID is the container id or pod name; empty in-process.
	ID     string
	Node   string
	Labels map[string]string
	// Applied says which of the requested limits the driver enforced, for the job's events.
	Applied []string
	// Ended is set by List for a placement that has already stopped, such as a container that died
	// while its launcher was down.
	Ended bool
	// Canceled is set by the launcher on a placement it stopped because its job was canceled. A
	// cancel is not a failure, so Remove deletes the placement whatever keep_failed_jobs_for says,
	// and the container or pod is gone within the cancel's ten seconds (AC12).
	Canceled bool
}

// Exit is how a placement ended.
type Exit struct {
	// Code is the executor's exit code: 0 completed, 1 failed, 2 unreported, 3 stopped (executor.ExitCode).
	Code int
	// Reason is the driver's word for an abnormal end (OOMKilled, Evicted), empty otherwise.
	Reason string
}

// ErrNotFound is returned for a placement that no longer exists.
var ErrNotFound = errors.New("placement not found")

// Driver places executors.
// ImageDigester is a driver that learns a placement's image digest only once the placement runs:
// the Kubernetes driver, whose kubelet pulls the image (#933, D4, AC77). ImageDigest answers ""
// until the digest is known.
type ImageDigester interface {
	ImageDigest(ctx context.Context, p Placement) (string, error)
}

type Driver interface {
	Name() string
	Capabilities() Capabilities
	// Ping reports whether the driver can place now: its socket or API answers.
	Ping(ctx context.Context) error
	Place(ctx context.Context, req Request) (Placement, error)
	// Wait blocks until the placement ends or ctx ends.
	Wait(ctx context.Context, p Placement) (Exit, error)
	// Kill stops a placement; grace is how long the executor gets to report before it is killed.
	Kill(ctx context.Context, p Placement, grace time.Duration) error
	// Remove deletes a placement that has ended, unless it failed on a pool that keeps failures
	// (keep_failed_jobs_for) and was not canceled.
	Remove(ctx context.Context, p Placement) error
	// List returns the placements carrying every label in selector, running or ended.
	List(ctx context.Context, selector map[string]string) ([]Placement, error)
	// Logs returns a placement's own log, the executor's lines.
	Logs(ctx context.Context, p Placement) (io.ReadCloser, error)
}
