// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// ansibleCfgSearchPath returns a search path (roles_path, collections_path) as the
// effective ansible.cfg configures it, ready to be used in the matching environment
// variable, or "" when it is not configured.
//
// candidates are the places Ansible looks for its configuration, in its order. Ansible
// uses the first file that exists and does not merge files, so only that file is read.
// keys are the names the setting goes by in the [defaults] section. Ansible resolves a
// relative entry against the directory of the file that set it, while an environment
// variable is resolved against the working directory, so relative entries are made
// absolute here to keep their meaning.
func ansibleCfgSearchPath(candidates []string, keys ...string) string {
	for _, candidate := range candidates {
		f, err := os.Open(candidate) //nolint:gosec // G304: candidates are ansible.cfg locations chosen by the caller
		if err != nil {
			continue
		}
		value := readIniValue(f, "defaults", keys)
		_ = f.Close()
		if value == "" {
			return ""
		}
		base := filepath.Dir(candidate)
		entries := strings.Split(value, string(os.PathListSeparator))
		for i, entry := range entries {
			entry = strings.TrimSpace(entry)
			if entry != "" && !filepath.IsAbs(entry) && !strings.HasPrefix(entry, "~") {
				entry = filepath.Join(base, entry)
			}
			entries[i] = entry
		}
		return strings.Join(entries, string(os.PathListSeparator))
	}
	return ""
}

// readIniValue returns the value of the first of keys found in the given section of an
// INI file, in the order of keys. It understands what ansible.cfg uses: [section]
// headers, "key = value" and "key: value", and # or ; comment lines.
func readIniValue(f *os.File, section string, keys []string) string {
	found := map[string]string{}
	inSection := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inSection = strings.TrimSpace(line[1:len(line)-1]) == section
			continue
		}
		if !inSection {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if colonKey, colonValue, colonOK := strings.Cut(line, ":"); colonOK && (!ok || len(colonKey) < len(key)) {
			key, value, ok = colonKey, colonValue, true
		}
		if ok {
			found[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	for _, key := range keys {
		if value := found[key]; value != "" {
			return value
		}
	}
	return ""
}

// ansibleCfgCandidates lists where ansible-playbook will look for its configuration for
// a job, in Ansible's order: the job's own file (what ANSIBLE_CONFIG points at), the
// playbook's directory, the home directory the job runs with, then the system-wide file.
// home is the job's HOME, which is not necessarily this process's: an empty home leaves
// that entry out.
func ansibleCfgCandidates(jobCfg, playbookDir, home string) []string {
	candidates := []string{jobCfg, filepath.Join(playbookDir, "ansible.cfg")}
	if home != "" {
		candidates = append(candidates, filepath.Join(home, ".ansible.cfg"))
	}
	return append(candidates, "/etc/ansible/ansible.cfg")
}

// envLookup returns the value exec would hand a process for name from env: the last
// assignment wins. It returns "" when env does not assign name.
func envLookup(env []string, name string) string {
	value := ""
	prefix := name + "="
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, prefix); ok {
			value = v
		}
	}
	return value
}

// withJobSearchPaths returns env, the environment ansible-playbook is about to run with,
// with ANSIBLE_COLLECTIONS_PATH and ANSIBLE_ROLES_PATH set for a job that has a staged
// Galaxy install. Each becomes, in order: the staged directory, the directory of that
// name next to the playbook, then whatever was in effect before, which is the variable
// already in env, else the value the effective ansible.cfg configures, else Ansible's
// default. Prepended, not substituted, so collections an operator added to the runner
// and paths a repository configures in ansible.cfg stay visible.
//
// workDir is the playbook's directory, which is also the directory the play runs in.
// galaxyCollections and galaxyRoles are what installGalaxyRequirements returned. Both
// runner modes call this, and only for a job whose Galaxy install was staged: a job
// without one keeps both variables as it found them, because an environment variable
// beats ansible.cfg and would override the paths configured there.
func withJobSearchPaths(env []string, workDir, galaxyCollections, galaxyRoles string) []string {
	// The configuration consulted is the one Ansible will read for this job, so both the
	// file ANSIBLE_CONFIG names and the home directory come from the job's environment.
	// The platform runner gives each job its own HOME; only a job that inherits this
	// process's environment unchanged has this process's home.
	home := envLookup(env, "HOME")
	if home == "" {
		home, _ = os.UserHomeDir() // no home at all leaves the home entry out
	}
	cfgFiles := ansibleCfgCandidates(envLookup(env, "ANSIBLE_CONFIG"), workDir, home)
	env = prependAnsiblePath(
		withConfiguredAnsiblePath(env, "ANSIBLE_COLLECTIONS_PATH", ansibleCfgSearchPath(cfgFiles, "collections_path", "collections_paths")),
		"ANSIBLE_COLLECTIONS_PATH", galaxyCollections, filepath.Join(workDir, "collections"))
	return prependAnsiblePath(
		withConfiguredAnsiblePath(env, "ANSIBLE_ROLES_PATH", ansibleCfgSearchPath(cfgFiles, "roles_path")),
		"ANSIBLE_ROLES_PATH", galaxyRoles, filepath.Join(workDir, "roles"))
}

// ansibleDefaultPaths are Ansible's own defaults for the search paths a job with a
// staged Galaxy install extends. They are spelled out because setting the variable
// replaces the default.
var ansibleDefaultPaths = map[string]string{
	"ANSIBLE_COLLECTIONS_PATH": "~/.ansible/collections:/usr/share/ansible/collections",
	"ANSIBLE_ROLES_PATH":       "~/.ansible/roles:/usr/share/ansible/roles:/etc/ansible/roles",
}

// withConfiguredAnsiblePath returns env with the search-path variable name set to
// configured, the value the effective ansible.cfg gives it, unless env already sets the
// variable (the environment wins over the file in Ansible) or the file does not
// configure it. It lets prependAnsiblePath keep what ansible.cfg asked for.
func withConfiguredAnsiblePath(env []string, name, configured string) []string {
	if configured == "" {
		return env
	}
	prefix := name + "="
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, prefix); ok && v != "" {
			return env
		}
	}
	return append(env, prefix+configured)
}

// prependAnsiblePath returns env with the Ansible search-path variable name set to dirs
// followed by what was in effect before: the variable's current value in env, or
// Ansible's default when it is unset or empty. Empty dirs are skipped.
func prependAnsiblePath(env []string, name string, dirs ...string) []string {
	prefix := name + "="
	current := ""
	rest := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, prefix); ok {
			current = v // exec uses the last assignment, so it is the one in effect
			continue
		}
		rest = append(rest, kv)
	}
	if current == "" {
		current = ansibleDefaultPaths[name]
	}

	parts := make([]string, 0, len(dirs)+1)
	for _, dir := range dirs {
		if dir != "" {
			parts = append(parts, dir)
		}
	}
	if current != "" {
		parts = append(parts, current)
	}
	return append(rest, prefix+strings.Join(parts, string(os.PathListSeparator)))
}
