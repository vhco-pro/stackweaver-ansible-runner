// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

// engineAPIVersion is the Docker Engine API version the container driver speaks: v1.40 is the one
// Podman's compatibility service implements, and every supported Docker serves it (D4, Sources).
const engineAPIVersion = "v1.40"

// DefaultSockets are where the container driver looks for an engine, in order: Docker, rootful
// Podman, rootless Podman.
func DefaultSockets() []string {
	socks := []string{"/var/run/docker.sock", "/run/podman/podman.sock"}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		socks = append(socks, dir+"/podman/podman.sock")
	}
	return socks
}

// engine is a minimal Docker Engine API client over a unix socket: the handful of calls a job
// container needs, so the driver carries no SDK and Docker and Podman are one code path.
type engine struct {
	socket string
	http   *http.Client
}

func newEngine(socket string) *engine {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &engine{socket: socket, http: &http.Client{Transport: tr}}
}

// APIError is an engine's error answer.
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("engine API %d: %s", e.Code, e.Message) }

func (e *engine) do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	return e.doAt(ctx, engineAPIVersion, method, path, query, body, out)
}

// subpathAPIVersion is the first Engine API version that honours a volume mount's Subpath; an
// older version ignores the field and would mount the whole volume.
const subpathAPIVersion = "v1.45"

func (e *engine) doAt(ctx context.Context, version, method, path string, query url.Values, body any, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding %s: %w", path, err)
		}
		rd = bytes.NewReader(data)
	}
	u := "http://engine/" + version + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd) //nolint:gosec // G704: a request to the local engine socket
	if err != nil {
		return fmt.Errorf("building %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.http.Do(req) //nolint:gosec // G704: a request to the local engine socket
	if err != nil {
		return fmt.Errorf("%s %s on %s: %w", method, path, e.socket, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &msg)
		if msg.Message == "" {
			msg.Message = strings.TrimSpace(string(data))
		}
		return &APIError{Code: resp.StatusCode, Message: msg.Message}
	}
	if out != nil {
		if w, ok := out.(io.Writer); ok {
			_, err = io.Copy(w, resp.Body)
			return err
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("decoding %s: %w", path, err)
		}
	}
	return nil
}

func isNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == http.StatusNotFound
}

func (e *engine) ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return e.do(ctx, http.MethodGet, "/_ping", nil, nil, io.Discard)
}

// engineInfo is the part of GET /info the driver reads.
type engineInfo struct {
	// Name is the engine host's name, the node a job container runs on.
	Name            string
	CgroupVersion   string
	SecurityOptions []string
	MemoryLimit     bool
	CPUShares       bool
	NCPU            int
}

func (e *engine) info(ctx context.Context) (engineInfo, error) {
	var out engineInfo
	err := e.do(ctx, http.MethodGet, "/info", nil, nil, &out)
	return out, err
}

// The container-create body: only the members the driver sets.
type containerConfig struct {
	Image        string
	Cmd          []string          `json:",omitempty"`
	Entrypoint   []string          `json:",omitempty"`
	Env          []string          `json:",omitempty"`
	Labels       map[string]string `json:",omitempty"`
	User         string            `json:",omitempty"`
	WorkingDir   string            `json:",omitempty"`
	HostConfig   hostConfig
	AttachStdout bool
	AttachStderr bool
}

type hostConfig struct {
	Binds          []string          `json:",omitempty"`
	Mounts         []mountSpec       `json:",omitempty"`
	Tmpfs          map[string]string `json:",omitempty"`
	NetworkMode    string            `json:",omitempty"`
	CapDrop        []string          `json:",omitempty"`
	CapAdd         []string          `json:",omitempty"`
	SecurityOpt    []string          `json:",omitempty"`
	ReadonlyRootfs bool
	Privileged     bool
	NanoCPUs       int64  `json:",omitempty"`
	Memory         int64  `json:",omitempty"`
	PidsLimit      *int64 `json:",omitempty"`
	Runtime        string `json:",omitempty"`
	ExtraHosts     []string
	Init           *bool `json:",omitempty"`
}

type mountSpec struct {
	Type          string // "volume" or "bind"
	Source        string
	Target        string
	ReadOnly      bool
	VolumeOptions *volumeOptions `json:",omitempty"`
}

// volumeOptions mounts a subdirectory of a volume (Docker Engine API 1.45, Podman 5).
type volumeOptions struct {
	Subpath string `json:",omitempty"`
}

// composeLabelPrefix is the prefix of Docker Compose's labels. Compose finds a project's containers
// by com.docker.compose.project and com.docker.compose.service, and an image it built carries both
// (with the version and the project's directory), which every container created from it inherits.
// Docker Compose uses the same labels against a Podman socket; podman-compose uses its own prefix.
const composeLabelPrefix = "com.docker.compose."

// composeLabelPrefixes are the label prefixes by which a Compose tool claims a container.
var composeLabelPrefixes = []string{composeLabelPrefix, "io.podman.compose."}

func isComposeLabel(k string) bool {
	for _, p := range composeLabelPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// composeIdentity are the labels by which Compose counts a container as one of its own, cleared on
// every container whether or not its image carries them.
var composeIdentity = []string{"project", "service", "oneoff", "container-number", "config-hash"}

// withoutComposeIdentity returns labels with every Compose label of the image, and Compose's
// identity labels, set to an empty value. A container cannot drop a label its image carries, only
// override it, and an empty value matches no project or service filter: without this, a job placed
// from a Compose-built launcher image is taken for a replica of the launcher service, which
// `docker compose up`, `down` and `--remove-orphans` stop or remove, and `ps` lists.
func withoutComposeIdentity(labels, imageLabels map[string]string) map[string]string {
	out := make(map[string]string, len(labels)+len(composeIdentity)+len(imageLabels))
	for _, k := range composeIdentity {
		out[composeLabelPrefix+k] = ""
	}
	for k := range imageLabels {
		if isComposeLabel(k) {
			out[k] = ""
		}
	}
	for k, v := range labels {
		if !isComposeLabel(k) {
			out[k] = v
		}
	}
	return out
}

// imageLabels are a local image's labels; an image the engine does not have yet has none to read.
func (e *engine) imageLabels(ctx context.Context, ref string) map[string]string {
	var img struct {
		Config struct {
			Labels map[string]string
		}
	}
	if ref == "" || e.do(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil, &img) != nil {
		return nil
	}
	return img.Config.Labels
}

// create creates a container. Every container the driver creates (a job, the executor copy, the
// firewall writer) goes through it, so none carries Compose's identity (withoutComposeIdentity).
func (e *engine) create(ctx context.Context, name string, cfg containerConfig) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	cfg.Labels = withoutComposeIdentity(cfg.Labels, e.imageLabels(ctx, cfg.Image))
	q := url.Values{}
	if name != "" {
		q.Set("name", name)
	}
	version := engineAPIVersion
	for _, m := range cfg.HostConfig.Mounts {
		if m.VolumeOptions != nil && m.VolumeOptions.Subpath != "" {
			version = subpathAPIVersion
		}
	}
	err := e.doAt(ctx, version, http.MethodPost, "/containers/create", q, cfg, &out)
	return out.ID, err
}

func (e *engine) start(ctx context.Context, id string) error {
	return e.do(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, nil)
}

// wait blocks until the container stops and returns its exit code.
func (e *engine) wait(ctx context.Context, id string) (int, error) {
	var out struct {
		StatusCode int
	}
	err := e.do(ctx, http.MethodPost, "/containers/"+id+"/wait", url.Values{"condition": {"not-running"}}, nil, &out)
	return out.StatusCode, err
}

func (e *engine) stop(ctx context.Context, id string, grace time.Duration) error {
	return e.do(ctx, http.MethodPost, "/containers/"+id+"/stop", url.Values{"t": {fmt.Sprint(int(grace.Seconds()))}}, nil, nil)
}

func (e *engine) remove(ctx context.Context, id string) error {
	return e.do(ctx, http.MethodDelete, "/containers/"+id, url.Values{"force": {"1"}, "v": {"1"}}, nil, nil)
}

// containerState is the part of GET /containers/{id}/json the driver reads.
type containerState struct {
	ID string `json:"Id"`
	// Mounts is what the container has mounted, binds given as strings included.
	Mounts []struct {
		Type        string
		Source      string
		Destination string
		RW          bool
	}
	Config struct {
		Labels map[string]string
	}
	State struct {
		Running   bool
		ExitCode  int
		OOMKilled bool
		Error     string
	}
	HostConfig struct {
		CapDrop        []string
		SecurityOpt    []string
		ReadonlyRootfs bool
		Privileged     bool
		NanoCPUs       int64
		Memory         int64
		NetworkMode    string
		Mounts         []struct {
			Type          string
			Source        string
			Target        string
			ReadOnly      bool
			VolumeOptions *volumeOptions
		}
	}
}

// boundReadOnly reports whether a created container has source bound read-only at target.
func (s containerState) boundReadOnly(target, source string) bool {
	for _, m := range s.Mounts {
		if m.Destination == target {
			return m.Type == "bind" && m.Source == source && !m.RW
		}
	}
	return false
}

// volumeSubdir is the path on the engine's host of sub inside a local volume, when sub is one plain
// path element (an organization's ID): a bind of it shows exactly what a volume subpath would.
func (e *engine) volumeSubdir(ctx context.Context, volume, sub string) (string, bool) {
	if sub == "" || sub == "." || sub == ".." || strings.ContainsAny(sub, "/\\:") {
		return "", false
	}
	var v struct {
		Driver     string
		Mountpoint string
	}
	if err := e.do(ctx, http.MethodGet, "/volumes/"+url.PathEscape(volume), nil, nil, &v); err != nil {
		return "", false
	}
	if v.Driver != "local" || !path.IsAbs(v.Mountpoint) || strings.Contains(v.Mountpoint, ":") {
		return "", false
	}
	return path.Join(v.Mountpoint, sub), true
}

// subpathHonoured reports whether a created container's mount at target carries subpath, so an
// engine that ignored the field is never trusted with a volume shared by every organization.
func (s containerState) subpathHonoured(target, subpath string) bool {
	for _, m := range s.HostConfig.Mounts {
		if m.Target == target {
			return m.VolumeOptions != nil && m.VolumeOptions.Subpath == subpath && m.ReadOnly
		}
	}
	return false
}

func (e *engine) inspect(ctx context.Context, id string) (containerState, error) {
	var out containerState
	err := e.do(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &out)
	return out, err
}

// listed is one entry of GET /containers/json.
type listed struct {
	ID     string `json:"Id"`
	Labels map[string]string
	State  string
}

func (e *engine) list(ctx context.Context, labels map[string]string) ([]listed, error) {
	var filter []string
	for k, v := range labels {
		filter = append(filter, k+"="+v)
	}
	f, err := json.Marshal(map[string][]string{"label": filter})
	if err != nil {
		return nil, err
	}
	var out []listed
	err = e.do(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"1"}, "filters": {string(f)}}, nil, &out)
	return out, err
}

func (e *engine) logs(ctx context.Context, id string) (io.ReadCloser, error) {
	u := "http://engine/" + engineAPIVersion + "/containers/" + id + "/logs?stdout=1&stderr=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.http.Do(req) //nolint:gosec // G704: a request to the local engine socket
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, &APIError{Code: resp.StatusCode, Message: "reading logs"}
	}
	return resp.Body, nil
}

// imageDigest is the content digest of a local image: the registry digest it was pulled by, or
// its image id (both `sha256:`) for an image built or loaded locally.
func (e *engine) imageDigest(ctx context.Context, ref string) (string, error) {
	var img struct {
		ID          string `json:"Id"`
		RepoDigests []string
	}
	if err := e.do(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil, &img); err != nil {
		return "", err
	}
	for _, rd := range img.RepoDigests {
		if _, d, ok := strings.Cut(rd, "@"); ok {
			return d, nil
		}
	}
	return img.ID, nil
}

func (e *engine) imageExists(ctx context.Context, ref string) bool {
	return e.do(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil, io.Discard) == nil
}

// pull fetches an image; the engine streams progress, which is read to the end.
func (e *engine) pull(ctx context.Context, ref string, auth *RegistryAuth) error {
	name, tag := splitImageRef(ref)
	q := url.Values{"fromImage": {name}, "tag": {tag}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://engine/"+engineAPIVersion+"/images/create?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	if auth != nil {
		header, err := json.Marshal(struct { //nolint:gosec // G117: the registry auth header the engine reads
			Username      string `json:"username"`
			Password      string `json:"password"` //nolint:gosec // G117: the engine's registry auth header
			ServerAddress string `json:"serveraddress"`
		}{auth.Username, auth.Password, auth.Registry})
		if err != nil {
			return err
		}
		req.Header.Set("X-Registry-Auth", base64.URLEncoding.EncodeToString(header))
	}
	resp, err := e.http.Do(req) //nolint:gosec // G704: a request to the local engine socket
	if err != nil {
		return fmt.Errorf("pulling %s on %s: %w", ref, e.socket, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &msg)
		if msg.Message == "" {
			msg.Message = strings.TrimSpace(string(data))
		}
		return &APIError{Code: resp.StatusCode, Message: msg.Message}
	}
	// The engine answers 200 and reports a failed pull inside the progress stream.
	return streamError(resp.Body)
}

// splitImageRef splits a reference into the repository and the tag or digest the engine's create
// call takes: repo@sha256:... pulls the digest, repo:tag the tag, a bare repo latest.
func splitImageRef(ref string) (name, tag string) {
	if n, digest, ok := strings.Cut(ref, "@"); ok {
		if i := strings.LastIndex(n, ":"); i > strings.LastIndex(n, "/") {
			n = n[:i]
		}
		return n, digest
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}

// streamError reads an engine progress stream and returns the error it reports, if any.
func streamError(r io.Reader) error {
	dec := json.NewDecoder(r)
	for {
		var m struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if m.Error != "" {
			return errors.New(m.Error)
		}
		if m.ErrorDetail.Message != "" {
			return errors.New(m.ErrorDetail.Message)
		}
	}
}

// networkState is the part of GET /networks/{id} the driver reads.
type networkState struct {
	ID       string `json:"Id"`
	Name     string
	Internal bool
	Labels   map[string]string
	IPAM     struct {
		Config []struct {
			Subnet  string
			Gateway string
		}
	}
	Options map[string]string
}

// subnetsExcept lists the subnets of every network on the engine but the named one and the
// host and none networks, which have none.
func (e *engine) subnetsExcept(ctx context.Context, name string) ([]string, error) {
	var nets []networkState
	if err := e.do(ctx, http.MethodGet, "/networks", nil, nil, &nets); err != nil {
		return nil, err
	}
	var out []string
	for _, n := range nets {
		if n.Name == name {
			continue
		}
		for _, c := range n.IPAM.Config {
			if c.Subnet != "" {
				out = append(out, c.Subnet)
			}
		}
	}
	return out, nil
}

func (e *engine) network(ctx context.Context, name string) (networkState, error) {
	var out networkState
	err := e.do(ctx, http.MethodGet, "/networks/"+url.PathEscape(name), nil, nil, &out)
	return out, err
}

func (e *engine) createNetwork(ctx context.Context, name string, labels map[string]string, options map[string]string) error {
	return e.do(ctx, http.MethodPost, "/networks/create", nil, map[string]any{
		"Name": name, "Driver": "bridge", "CheckDuplicate": true, "Labels": labels, "Options": options,
	}, nil)
}

func (e *engine) createVolume(ctx context.Context, name string, labels map[string]string) error {
	return e.do(ctx, http.MethodPost, "/volumes/create", nil, map[string]any{"Name": name, "Labels": labels}, nil)
}

func (e *engine) removeVolume(ctx context.Context, name string) error {
	return e.do(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), url.Values{"force": {"1"}}, nil, nil)
}
