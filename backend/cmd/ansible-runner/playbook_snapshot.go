// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// buildPlaybookSnapshot produces a gzipped tar of the playbook's dependencies for
// caching. It uses a hybrid strategy: a static walk of the playbook collects the
// roles it uses (captured whole, plus their meta dependencies) and the
// conventional Ansible content directories; if the playbook uses constructs we
// cannot statically resolve (loose task-file includes, vars_files, or Jinja in a
// role/include reference), it falls back to capturing the whole repository so a
// cached run never misses a dependency. Returns the tarball bytes and whether the
// whole-repo fallback was used (for logging).
func buildPlaybookSnapshot(repoDir, playbookPath string) ([]byte, bool, error) {
	paths, wholeRepo := resolvePlaybookSnapshotPaths(repoDir, playbookPath)
	var (
		data []byte
		err  error
	)
	if wholeRepo {
		data, err = tarGzDir(repoDir, nil)
	} else {
		data, err = tarGzDir(repoDir, paths)
	}
	return data, wholeRepo, err
}

// resolvePlaybookSnapshotPaths returns the repo-relative paths to include in a
// snapshot, or wholeRepo=true when the playbook can't be statically scoped safely.
func resolvePlaybookSnapshotPaths(repoDir, playbookPath string) (paths []string, wholeRepo bool) {
	set := map[string]struct{}{}
	add := func(rel string) {
		rel = strings.TrimPrefix(filepath.ToSlash(rel), "/")
		if rel == "" || rel == "." {
			return
		}
		if _, err := os.Stat(filepath.Join(repoDir, filepath.FromSlash(rel))); err == nil {
			set[rel] = struct{}{}
		}
	}

	roleNames := map[string]struct{}{}
	visitedPlaybooks := map[string]bool{}
	var dynamic, loose bool

	var parsePlaybook func(rel string)
	parsePlaybook = func(rel string) {
		rel = strings.TrimPrefix(filepath.ToSlash(rel), "/")
		if visitedPlaybooks[rel] {
			return
		}
		visitedPlaybooks[rel] = true
		add(rel) // the playbook file itself

		raw, err := os.ReadFile(filepath.Join(repoDir, filepath.FromSlash(rel))) //nolint:gosec // rel derived from repo content, read-only
		if err != nil {
			dynamic = true // can't read it - be safe
			return
		}
		var plays []map[string]interface{}
		if err := yaml.Unmarshal(raw, &plays); err != nil {
			dynamic = true
			return
		}
		playbookDir := path.Dir(rel)
		for _, play := range plays {
			if v, ok := play["import_playbook"]; ok {
				s, _ := v.(string)
				if s == "" || strings.Contains(s, "{{") {
					dynamic = true
					continue
				}
				parsePlaybook(path.Join(playbookDir, s))
				continue
			}
			if rv, ok := play["roles"]; ok {
				collectRoleNames(rv, roleNames, &dynamic)
			}
			for _, key := range []string{"tasks", "pre_tasks", "post_tasks", "handlers"} {
				if tv, ok := play[key]; ok {
					scanTasks(tv, roleNames, &dynamic, &loose)
				}
			}
			// vars_files reference loose files that may live anywhere in the repo.
			if _, ok := play["vars_files"]; ok {
				loose = true
			}
		}
	}

	parsePlaybook(playbookPath)

	if dynamic || loose {
		return nil, true
	}

	// Resolve role names to directories (whole-dir capture) plus their transitive
	// meta dependencies. Roles not found locally are external (Galaxy) and skipped.
	resolveRoleDirs(repoDir, playbookPath, roleNames, add)

	// Conventional Ansible content, at the repo root and adjacent to the playbook.
	conv := []string{
		"group_vars", "host_vars", "files", "templates", "vars",
		"library", "module_utils", "filter_plugins", "requirements.yml",
		"ansible.cfg", "collections",
	}
	pbDir := path.Dir(strings.TrimPrefix(filepath.ToSlash(playbookPath), "/"))
	for _, c := range conv {
		add(c)
		if pbDir != "." && pbDir != "" {
			add(path.Join(pbDir, c))
		}
	}

	for p := range set {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, false
}

// collectRoleNames extracts role names from a play `roles:` value. Each entry is a
// string or a map with a `role`/`name` key. Jinja in a name marks the set dynamic.
func collectRoleNames(rolesVal interface{}, out map[string]struct{}, dynamic *bool) {
	list, ok := rolesVal.([]interface{})
	if !ok {
		return
	}
	for _, item := range list {
		switch v := item.(type) {
		case string:
			markRole(v, out, dynamic)
		case map[string]interface{}:
			name := stringField(v, "role")
			if name == "" {
				name = stringField(v, "name")
			}
			markRole(name, out, dynamic)
		}
	}
}

// scanTasks walks a task list, treating include_role/import_role like roles, and
// loose task-file includes / vars references as a whole-repo trigger. Nested
// block/rescue/always lists are recursed.
func scanTasks(tasksVal interface{}, roleNames map[string]struct{}, dynamic, loose *bool) {
	list, ok := tasksVal.([]interface{})
	if !ok {
		return
	}
	for _, item := range list {
		task, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		for _, rk := range []string{"include_role", "import_role"} {
			if rv, ok := task[rk]; ok {
				if m, ok := rv.(map[string]interface{}); ok {
					markRole(stringField(m, "name"), roleNames, dynamic)
				} else {
					*dynamic = true
				}
			}
		}
		// Loose task-file includes and var loads can reference anything; fall back.
		for _, lk := range []string{"include_tasks", "import_tasks", "include", "include_vars", "vars_files"} {
			if _, ok := task[lk]; ok {
				*loose = true
			}
		}
		for _, nk := range []string{"block", "rescue", "always"} {
			if nv, ok := task[nk]; ok {
				scanTasks(nv, roleNames, dynamic, loose)
			}
		}
	}
}

func markRole(name string, out map[string]struct{}, dynamic *bool) {
	if name == "" {
		return
	}
	if strings.Contains(name, "{{") {
		*dynamic = true
		return
	}
	out[name] = struct{}{}
}

func stringField(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// resolveRoleDirs finds each role's directory (searching roles/ adjacent to the
// playbook and at the repo root), adds it whole, and recurses meta/main.yml
// dependencies. Roles absent locally are external (Galaxy) and skipped.
func resolveRoleDirs(repoDir, playbookPath string, roleNames map[string]struct{}, add func(string)) {
	pbDir := path.Dir(strings.TrimPrefix(filepath.ToSlash(playbookPath), "/"))
	searchRoots := []string{"roles"}
	if pbDir != "." && pbDir != "" {
		searchRoots = append(searchRoots, path.Join(pbDir, "roles"))
	}

	resolved := map[string]bool{}
	var resolve func(name string)
	resolve = func(name string) {
		if name == "" || resolved[name] {
			return
		}
		resolved[name] = true
		for _, root := range searchRoots {
			relDir := path.Join(root, name)
			info, err := os.Stat(filepath.Join(repoDir, filepath.FromSlash(relDir)))
			if err != nil || !info.IsDir() {
				continue
			}
			add(relDir) // capture the whole role directory
			// Follow meta dependencies.
			metaRaw, err := os.ReadFile(filepath.Join(repoDir, filepath.FromSlash(path.Join(relDir, "meta", "main.yml")))) //nolint:gosec // repo content, read-only
			if err != nil {
				return
			}
			var meta struct {
				Dependencies []interface{} `yaml:"dependencies"`
			}
			if yaml.Unmarshal(metaRaw, &meta) != nil {
				return
			}
			for _, dep := range meta.Dependencies {
				switch d := dep.(type) {
				case string:
					resolve(d)
				case map[string]interface{}:
					n := stringField(d, "role")
					if n == "" {
						n = stringField(d, "name")
					}
					resolve(n)
				}
			}
			return
		}
	}

	for name := range roleNames {
		resolve(name)
	}
}

// tarGzDir builds a gzipped tar of root. When includePaths is nil the whole tree
// is captured; otherwise only the listed repo-relative paths (files or dirs). The
// .git directory is always excluded.
func tarGzDir(root string, includePaths []string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	writeOne := func(absPath string) error {
		return filepath.Walk(absPath, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if rel == "." {
				return nil
			}
			// Never capture VCS metadata.
			if rel == ".git" || strings.HasPrefix(rel, ".git/") {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			// Only regular files and directories; skip symlinks/devices.
			if !info.Mode().IsRegular() && !info.IsDir() {
				return nil
			}
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = rel
			if info.IsDir() {
				hdr.Name = rel + "/"
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			f, err := os.Open(p) //nolint:gosec // p from a controlled repo walk
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			_, err = io.Copy(tw, f) //nolint:gosec // copying repo file into archive
			return err
		})
	}

	if includePaths == nil {
		if err := writeOne(root); err != nil {
			return nil, fmt.Errorf("failed to archive repo: %w", err)
		}
	} else {
		seen := map[string]bool{}
		for _, rel := range includePaths {
			if seen[rel] {
				continue
			}
			seen[rel] = true
			abs := filepath.Join(root, filepath.FromSlash(rel))
			if _, err := os.Stat(abs); err != nil {
				continue // path vanished or never existed; skip
			}
			if err := writeOne(abs); err != nil {
				return nil, fmt.Errorf("failed to archive %s: %w", rel, err)
			}
		}
	}

	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("failed to close tar writer: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("failed to close gzip writer: %w", err)
	}
	return buf.Bytes(), nil
}
