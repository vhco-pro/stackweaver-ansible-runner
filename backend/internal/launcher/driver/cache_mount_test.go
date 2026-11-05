// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// cacheEngine is a fake engine for Place: it keeps every container create, answers an inspect of a
// container from what was created, the way the engine it stands in for would, and holds one local
// volume.
type cacheEngine struct {
	// honoursSubpath is whether the engine keeps a volume mount's subpath, as Docker does; Podman's
	// Docker-compatible API drops it.
	honoursSubpath bool
	// keepsBinds is whether the engine reports a bound directory among the container's mounts.
	keepsBinds bool
	volume     map[string]any

	mu      sync.Mutex
	created []containerConfig
	started []string
	removed []string
}

func (f *cacheEngine) serve(t *testing.T) *engine {
	t.Helper()
	dir, err := os.MkdirTemp("", "swc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "e.sock")
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(f.handle)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return newEngine(sock)
}

func (f *cacheEngine) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && strings.Contains(p, "/images/"):
		answerJSON(w, map[string]any{"Id": "sha256:abc", "Config": map[string]any{"User": "1001:1001"}})
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/info"):
		answerJSON(w, map[string]any{"Name": "node-1"})
	case r.Method == http.MethodGet && strings.Contains(p, "/volumes/"):
		if f.volume == nil {
			http.NotFound(w, r)
			return
		}
		answerJSON(w, f.volume)
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/containers/create"):
		var cfg containerConfig
		_ = json.NewDecoder(r.Body).Decode(&cfg)
		f.created = append(f.created, cfg)
		w.WriteHeader(http.StatusCreated)
		answerJSON(w, map[string]string{"Id": "c" + strconv.Itoa(len(f.created))})
	case r.Method == http.MethodGet && strings.Contains(p, "/containers/c") && strings.HasSuffix(p, "/json"):
		n, _ := strconv.Atoi(strings.TrimSuffix(p[strings.LastIndex(p, "/containers/c")+len("/containers/c"):], "/json"))
		answerJSON(w, f.inspect(f.created[n-1]))
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/start"):
		f.started = append(f.started, p)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete && strings.Contains(p, "/containers/"):
		f.removed = append(f.removed, p)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// inspect is what the engine reports for a container created from cfg.
func (f *cacheEngine) inspect(cfg containerConfig) map[string]any {
	var hostMounts []map[string]any
	var mounts []map[string]any
	for _, m := range cfg.HostConfig.Mounts {
		hm := map[string]any{"Type": m.Type, "Source": m.Source, "Target": m.Target, "ReadOnly": m.ReadOnly}
		if m.VolumeOptions != nil && f.honoursSubpath {
			hm["VolumeOptions"] = map[string]any{"Subpath": m.VolumeOptions.Subpath}
		}
		hostMounts = append(hostMounts, hm)
	}
	if f.keepsBinds {
		for _, b := range cfg.HostConfig.Binds {
			parts := strings.Split(b, ":")
			mounts = append(mounts, map[string]any{"Type": "bind", "Source": parts[0], "Destination": parts[1], "RW": !strings.Contains(parts[2], "ro")})
		}
	}
	return map[string]any{"Id": "c", "Mounts": mounts, "HostConfig": map[string]any{"Mounts": hostMounts}}
}

func (f *cacheEngine) last(t *testing.T) containerConfig {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.started) != 1 {
		t.Fatalf("started %v, want exactly one container", f.started)
	}
	return f.created[len(f.created)-1]
}

func cacheEnv(cfg containerConfig) bool {
	return slices.Contains(cfg.Env, "STACKWEAVER_CACHE_DIR="+CacheMountPath)
}

func cacheMounted(cfg containerConfig) bool {
	return slices.ContainsFunc(cfg.HostConfig.Mounts, func(m mountSpec) bool { return m.Target == CacheMountPath }) ||
		slices.ContainsFunc(cfg.HostConfig.Binds, func(b string) bool { return strings.Contains(b, ":"+CacheMountPath+":") })
}

func placeWithCache(t *testing.T, f *cacheEngine, c *CacheMount) containerConfig {
	t.Helper()
	e := f.serve(t)
	d := &Container{Socket: e.socket, PlatformImage: "img", ExecutorPath: "/x"}
	if _, err := d.Place(t.Context(), Request{JobID: "j", Attempt: 1, Cache: c}); err != nil {
		t.Fatalf("Place: %v", err)
	}
	return f.last(t)
}

// On an engine that confines its containers with SELinux (rootless Podman on Fedora, or a Docker
// daemon with SELinux on), a bound directory keeps its host label and a job cannot read it. The
// launcher's cache directory is bound with the shared container label, read-only, so every job of
// the organization can read it at once; an engine without SELinux ignores the label.
func TestContainerBindsTheCacheDirectoryWithTheSharedContainerLabel(t *testing.T) {
	f := &cacheEngine{}
	cfg := placeWithCache(t, f, &CacheMount{Dir: "/srv/cache/org-1", Subpath: "org-1"})
	if want := []string{"/srv/cache/org-1:" + CacheMountPath + ":ro,z"}; !slices.Equal(cfg.HostConfig.Binds, want) {
		t.Errorf("binds = %v, want %v", cfg.HostConfig.Binds, want)
	}
	if slices.ContainsFunc(cfg.HostConfig.Mounts, func(m mountSpec) bool { return m.Target == CacheMountPath }) {
		t.Errorf("the cache is mounted twice: %v", cfg.HostConfig.Mounts)
	}
	if !cacheEnv(cfg) {
		t.Errorf("env %v lacks STACKWEAVER_CACHE_DIR", cfg.Env)
	}
}

// A directory whose path a bind string cannot carry is still mounted read-only, without the label.
func TestContainerMountsACacheDirectoryWithAColonInItsPath(t *testing.T) {
	f := &cacheEngine{}
	cfg := placeWithCache(t, f, &CacheMount{Dir: "/srv/ca:che/org-1", Subpath: "org-1"})
	if len(cfg.HostConfig.Binds) != 0 {
		t.Errorf("binds = %v, want none", cfg.HostConfig.Binds)
	}
	if !slices.ContainsFunc(cfg.HostConfig.Mounts, func(m mountSpec) bool {
		return m.Target == CacheMountPath && m.Type == "bind" && m.Source == "/srv/ca:che/org-1" && m.ReadOnly
	}) {
		t.Errorf("mounts = %v, want the directory bound read-only", cfg.HostConfig.Mounts)
	}
}

// An engine that keeps the subpath gets the volume mount as asked, and nothing else.
func TestContainerMountsTheCacheVolumeSubpathWhereTheEngineKeepsIt(t *testing.T) {
	f := &cacheEngine{honoursSubpath: true, keepsBinds: true, volume: map[string]any{"Name": "cache", "Driver": "local", "Mountpoint": "/var/lib/docker/volumes/cache/_data"}}
	cfg := placeWithCache(t, f, &CacheMount{Volume: "cache", Subpath: "org-1"})
	if len(f.created) != 1 || len(cfg.HostConfig.Binds) != 0 {
		t.Fatalf("created %d containers, binds %v; want one container and the volume mount", len(f.created), cfg.HostConfig.Binds)
	}
	if !slices.ContainsFunc(cfg.HostConfig.Mounts, func(m mountSpec) bool {
		return m.Target == CacheMountPath && m.VolumeOptions != nil && m.VolumeOptions.Subpath == "org-1" && m.ReadOnly
	}) || !cacheEnv(cfg) {
		t.Errorf("mounts %v env %v, want the organization's subpath read-only", cfg.HostConfig.Mounts, cfg.Env)
	}
}

// Podman's Docker-compatible API takes no volume subpath and would mount every organization's
// cache. The job gets its organization's directory bound read-only from the local volume's path on
// the engine's host instead, and the container created with the whole volume is never started.
func TestContainerBindsTheVolumeSubdirectoryWhereTheEngineDropsTheSubpath(t *testing.T) {
	f := &cacheEngine{keepsBinds: true, volume: map[string]any{"Name": "cache", "Driver": "local", "Mountpoint": "/home/u/.local/share/containers/storage/volumes/cache/_data"}}
	cfg := placeWithCache(t, f, &CacheMount{Volume: "cache", Subpath: "org-1"})
	if want := []string{"/home/u/.local/share/containers/storage/volumes/cache/_data/org-1:" + CacheMountPath + ":ro,z"}; !slices.Equal(cfg.HostConfig.Binds, want) {
		t.Errorf("binds = %v, want %v", cfg.HostConfig.Binds, want)
	}
	if slices.ContainsFunc(cfg.HostConfig.Mounts, func(m mountSpec) bool { return m.Target == CacheMountPath }) {
		t.Errorf("the whole volume is still mounted: %v", cfg.HostConfig.Mounts)
	}
	if !cacheEnv(cfg) {
		t.Errorf("env %v lacks STACKWEAVER_CACHE_DIR", cfg.Env)
	}
	if len(f.removed) != 1 || !strings.HasSuffix(f.removed[0], "/containers/c1") || !strings.HasSuffix(f.started[0], "/containers/c2/start") {
		t.Errorf("removed %v started %v, want the whole-volume container removed unstarted", f.removed, f.started)
	}
}

// Where neither the subpath nor a bound subdirectory can be trusted, the job gets no cache mount and
// reads its entries through its blob route: a subpath that is not one path element, a volume of
// another driver, or an engine that does not report the bound directory.
func TestContainerLeavesTheCacheOutWhenNeitherMountCanBeTrusted(t *testing.T) {
	local := map[string]any{"Name": "cache", "Driver": "local", "Mountpoint": "/var/lib/containers/storage/volumes/cache/_data"}
	for name, tc := range map[string]struct {
		f   *cacheEngine
		sub string
	}{
		"a subpath leaving the volume":   {&cacheEngine{keepsBinds: true, volume: local}, "../other"},
		"a nested subpath":               {&cacheEngine{keepsBinds: true, volume: local}, "org-1/tofu"},
		"a volume of another driver":     {&cacheEngine{keepsBinds: true, volume: map[string]any{"Name": "cache", "Driver": "nfs", "Mountpoint": "/mnt/x"}}, "org-1"},
		"a volume the engine cannot see": {&cacheEngine{keepsBinds: true}, "org-1"},
		"an engine hiding the bind":      {&cacheEngine{volume: local}, "org-1"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := placeWithCache(t, tc.f, &CacheMount{Volume: "cache", Subpath: tc.sub})
			if cacheMounted(cfg) || cacheEnv(cfg) {
				t.Errorf("binds %v mounts %v env %v, want no cache", cfg.HostConfig.Binds, cfg.HostConfig.Mounts, cfg.Env)
			}
		})
	}
}
