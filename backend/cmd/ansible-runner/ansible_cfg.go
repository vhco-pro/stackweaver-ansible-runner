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
// playbook's directory, the user's home, then the system-wide file.
func ansibleCfgCandidates(jobCfg, playbookDir string) []string {
	candidates := []string{jobCfg, filepath.Join(playbookDir, "ansible.cfg")}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".ansible.cfg"))
	}
	return append(candidates, "/etc/ansible/ansible.cfg")
}
