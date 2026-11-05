// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// probe stands in for the executor inside a job container in the container driver's integration
// tests: it reports what the container lets it do, as one JSON line, and exits.
package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	out := map[string]any{"args": os.Args[1:], "uid": os.Getuid()}
	if status, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(status), "\n") {
			for _, k := range []string{"CapEff", "NoNewPrivs", "Seccomp"} {
				if v, ok := strings.CutPrefix(line, k+":"); ok {
					out[k] = strings.TrimSpace(v)
				}
			}
		}
	}
	out["root_writable"] = os.WriteFile("/probe-write", []byte("x"), 0o600) == nil
	out["tmp_writable"] = os.WriteFile("/tmp/probe-write", []byte("x"), 0o600) == nil
	if dir := os.Getenv("STACKWEAVER_CACHE_DIR"); dir != "" {
		out["cache_dir"] = dir
		var files []string
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				rel, _ := filepath.Rel(dir, path)
				files = append(files, rel)
			}
			return nil
		})
		out["cache_files"] = files
		out["cache_writable"] = os.WriteFile(filepath.Join(dir, "probe-write"), []byte("x"), 0o600) == nil
	}
	_, sockErr := os.Stat("/var/run/docker.sock")
	out["docker_socket"] = sockErr == nil
	reach := map[string]bool{}
	for _, addr := range strings.Split(os.Getenv("PROBE_DIAL"), ",") {
		if addr == "" {
			continue
		}
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		reach[addr] = err == nil
		if c != nil {
			_ = c.Close()
		}
	}
	out["reach"] = reach
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		env = append(env, name)
	}
	out["env"] = env
	data, _ := json.Marshal(out)
	fmt.Println(string(data))
	if s := os.Getenv("PROBE_SLEEP"); s != "" {
		d, _ := time.ParseDuration(s)
		time.Sleep(d)
	}
	if os.Getenv("PROBE_EXIT") != "" {
		os.Exit(7)
	}
}
