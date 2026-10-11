// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// envValue returns the value exec would use for key (the last assignment wins) and how
// many assignments of key the environment holds.
func envValue(env []string, key string) (value string, count int) {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			value = v
			count++
		}
	}
	return value, count
}

func TestPrependAnsiblePath(t *testing.T) {
	const (
		staged = "/tmp/ansible-job-1/galaxy/collections"
		local  = "/tmp/ansible-job-1/repo/collections"
	)
	tests := []struct {
		name string
		env  []string
		key  string
		dirs []string
		want string
	}{
		{
			name: "collections path set on the host is kept after the job's dirs",
			env:  []string{"PATH=/usr/bin", "ANSIBLE_COLLECTIONS_PATH=/opt/site/collections:/srv/more"},
			key:  "ANSIBLE_COLLECTIONS_PATH",
			dirs: []string{staged, local},
			want: staged + ":" + local + ":/opt/site/collections:/srv/more",
		},
		{
			name: "unset collections path falls back to Ansible's defaults",
			env:  []string{"PATH=/usr/bin"},
			key:  "ANSIBLE_COLLECTIONS_PATH",
			dirs: []string{staged, local},
			want: staged + ":" + local + ":~/.ansible/collections:/usr/share/ansible/collections",
		},
		{
			name: "empty collections path counts as unset",
			env:  []string{"ANSIBLE_COLLECTIONS_PATH="},
			key:  "ANSIBLE_COLLECTIONS_PATH",
			dirs: []string{staged},
			want: staged + ":~/.ansible/collections:/usr/share/ansible/collections",
		},
		{
			name: "roles path set on the host is kept after the job's dirs",
			env:  []string{"ANSIBLE_ROLES_PATH=/opt/site/roles"},
			key:  "ANSIBLE_ROLES_PATH",
			dirs: []string{"/job/galaxy/roles", "/job/repo/roles"},
			want: "/job/galaxy/roles:/job/repo/roles:/opt/site/roles",
		},
		{
			name: "unset roles path falls back to Ansible's defaults",
			env:  nil,
			key:  "ANSIBLE_ROLES_PATH",
			dirs: []string{"/job/galaxy/roles", "/job/repo/roles"},
			want: "/job/galaxy/roles:/job/repo/roles:~/.ansible/roles:/usr/share/ansible/roles:/etc/ansible/roles",
		},
		{
			name: "the last assignment is the one in effect, and empty dirs are skipped",
			env:  []string{"ANSIBLE_COLLECTIONS_PATH=/first", "HOME=/home/op", "ANSIBLE_COLLECTIONS_PATH=/second"},
			key:  "ANSIBLE_COLLECTIONS_PATH",
			dirs: []string{"", local},
			want: local + ":/second",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := prependAnsiblePath(tt.env, tt.key, tt.dirs...)
			value, count := envValue(got, tt.key)
			if value != tt.want {
				t.Errorf("%s = %q, want %q", tt.key, value, tt.want)
			}
			if count != 1 {
				t.Errorf("%s is assigned %d times in the result, want exactly once", tt.key, count)
			}
			for _, kv := range tt.env {
				if strings.HasPrefix(kv, tt.key+"=") {
					continue
				}
				if !slices.Contains(got, kv) {
					t.Errorf("unrelated variable %q was dropped", kv)
				}
			}
		})
	}
}

// Both runner modes hand a job with a staged Galaxy install to withJobSearchPaths, so
// this is the one place the order is pinned: the staged directory, the playbook-adjacent
// directory, then whatever was in effect before (the variable in env, else what the
// effective ansible.cfg configures, else Ansible's default).
func TestWithJobSearchPaths(t *testing.T) {
	const (
		stagedCollections  = "/job/galaxy/collections"
		stagedRoles        = "/job/galaxy/roles"
		defaultCollections = "~/.ansible/collections:/usr/share/ansible/collections"
		defaultRoles       = "~/.ansible/roles:/usr/share/ansible/roles:/etc/ansible/roles"
		cfgWithBothPaths   = "[defaults]\ncollections_path = vendor/collections\nroles_path = vendor/roles:/opt/shared/roles\n"
	)
	// A ~/.ansible.cfg on the machine running the tests must not decide the outcome.
	t.Setenv("HOME", t.TempDir())

	tests := []struct {
		name           string
		playbookCfg    string // ansible.cfg in the playbook's directory, "" for none
		jobCfg         string // ansible.cfg the job is pointed at through ANSIBLE_CONFIG, "" for none
		jobHomeCfg     string // .ansible.cfg under a directory handed to the job as HOME, "" for no HOME in env
		processHomeCfg string // .ansible.cfg under this process's own home, "" for none
		env            []string
		collections    string // staged collections dir handed in
		roles          string // staged roles dir handed in
		// want is built from the playbook directory, which only exists once the case runs.
		wantCollections func(workDir string) string
		wantRoles       func(workDir string) string
	}{
		{
			name:        "nothing configured falls back to Ansible's defaults",
			env:         []string{"PATH=/usr/bin"},
			collections: stagedCollections,
			roles:       stagedRoles,
			wantCollections: func(workDir string) string {
				return stagedCollections + ":" + filepath.Join(workDir, "collections") + ":" + defaultCollections
			},
			wantRoles: func(workDir string) string {
				return stagedRoles + ":" + filepath.Join(workDir, "roles") + ":" + defaultRoles
			},
		},
		{
			name:        "ansible.cfg collections_path is kept behind the job's dirs",
			playbookCfg: cfgWithBothPaths,
			env:         []string{"PATH=/usr/bin"},
			collections: stagedCollections,
			roles:       stagedRoles,
			wantCollections: func(workDir string) string {
				return stagedCollections + ":" + filepath.Join(workDir, "collections") + ":" + filepath.Join(workDir, "vendor", "collections")
			},
			wantRoles: func(workDir string) string {
				return stagedRoles + ":" + filepath.Join(workDir, "roles") + ":" + filepath.Join(workDir, "vendor", "roles") + ":/opt/shared/roles"
			},
		},
		{
			name:        "a variable already in env is kept behind the job's dirs",
			playbookCfg: cfgWithBothPaths,
			env:         []string{"ANSIBLE_COLLECTIONS_PATH=/opt/site/collections", "PATH=/usr/bin", "ANSIBLE_ROLES_PATH=/opt/site/roles"},
			collections: stagedCollections,
			roles:       stagedRoles,
			wantCollections: func(workDir string) string {
				return stagedCollections + ":" + filepath.Join(workDir, "collections") + ":/opt/site/collections"
			},
			wantRoles: func(workDir string) string {
				return stagedRoles + ":" + filepath.Join(workDir, "roles") + ":/opt/site/roles"
			},
		},
		{
			name:        "a variable that is set but empty counts as unset",
			playbookCfg: cfgWithBothPaths,
			env:         []string{"ANSIBLE_COLLECTIONS_PATH=", "ANSIBLE_ROLES_PATH="},
			collections: stagedCollections,
			roles:       stagedRoles,
			wantCollections: func(workDir string) string {
				return stagedCollections + ":" + filepath.Join(workDir, "collections") + ":" + filepath.Join(workDir, "vendor", "collections")
			},
			wantRoles: func(workDir string) string {
				return stagedRoles + ":" + filepath.Join(workDir, "roles") + ":" + filepath.Join(workDir, "vendor", "roles") + ":/opt/shared/roles"
			},
		},
		{
			name: "an empty staged dir is skipped",
			env:  []string{"PATH=/usr/bin"},
			wantCollections: func(workDir string) string {
				return filepath.Join(workDir, "collections") + ":" + defaultCollections
			},
			wantRoles: func(workDir string) string {
				return filepath.Join(workDir, "roles") + ":" + defaultRoles
			},
		},
		{
			name:        "the ansible.cfg named by ANSIBLE_CONFIG is the effective one",
			playbookCfg: cfgWithBothPaths,
			jobCfg:      "[defaults]\ncollections_path = /from/job/cfg\n",
			env:         []string{"PATH=/usr/bin"},
			collections: stagedCollections,
			roles:       stagedRoles,
			wantCollections: func(workDir string) string {
				return stagedCollections + ":" + filepath.Join(workDir, "collections") + ":/from/job/cfg"
			},
			// Ansible reads one file and does not merge: the job's file sets no roles_path,
			// so the playbook directory's roles_path does not apply.
			wantRoles: func(workDir string) string {
				return stagedRoles + ":" + filepath.Join(workDir, "roles") + ":" + defaultRoles
			},
		},
		{
			// The platform runner points a job's HOME at a per-job directory, and Ansible
			// looks for ~/.ansible.cfg there.
			name:        "the home ansible.cfg is read from the job's HOME, not the process home",
			jobHomeCfg:  "[defaults]\ncollections_path = /from/job/home\n",
			env:         []string{"PATH=/usr/bin"},
			collections: stagedCollections,
			roles:       stagedRoles,
			wantCollections: func(workDir string) string {
				return stagedCollections + ":" + filepath.Join(workDir, "collections") + ":/from/job/home"
			},
			wantRoles: func(workDir string) string {
				return stagedRoles + ":" + filepath.Join(workDir, "roles") + ":" + defaultRoles
			},
		},
		{
			name:           "a .ansible.cfg in the process home is ignored when the job's HOME differs",
			jobHomeCfg:     "# the job's home has a file, and it configures no search path\n",
			processHomeCfg: "[defaults]\ncollections_path = /from/process/home\nroles_path = /from/process/home/roles\n",
			env:            []string{"PATH=/usr/bin"},
			collections:    stagedCollections,
			roles:          stagedRoles,
			wantCollections: func(workDir string) string {
				return stagedCollections + ":" + filepath.Join(workDir, "collections") + ":" + defaultCollections
			},
			wantRoles: func(workDir string) string {
				return stagedRoles + ":" + filepath.Join(workDir, "roles") + ":" + defaultRoles
			},
		},
		{
			// An agent sets no per-job HOME, so the job inherits the process's.
			name:           "without a HOME in env the process home is the job's home",
			processHomeCfg: "[defaults]\ncollections_path = /from/process/home\n",
			env:            []string{"PATH=/usr/bin"},
			collections:    stagedCollections,
			roles:          stagedRoles,
			wantCollections: func(workDir string) string {
				return stagedCollections + ":" + filepath.Join(workDir, "collections") + ":/from/process/home"
			},
			wantRoles: func(workDir string) string {
				return stagedRoles + ":" + filepath.Join(workDir, "roles") + ":" + defaultRoles
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir := t.TempDir()
			if tt.playbookCfg != "" {
				writeFile(t, workDir, "ansible.cfg", tt.playbookCfg)
			}
			env := slices.Clone(tt.env)
			if tt.processHomeCfg != "" {
				processHome := t.TempDir()
				writeFile(t, processHome, ".ansible.cfg", tt.processHomeCfg)
				t.Setenv("HOME", processHome)
			}
			if tt.jobHomeCfg != "" {
				jobHome := t.TempDir()
				writeFile(t, jobHome, ".ansible.cfg", tt.jobHomeCfg)
				env = append(env, "HOME=/overridden/by/the/next/assignment", "HOME="+jobHome)
			}
			if tt.jobCfg != "" {
				jobDir := t.TempDir()
				writeFile(t, jobDir, "ansible.cfg", tt.jobCfg)
				env = append(env, "ANSIBLE_CONFIG="+filepath.Join(jobDir, "ansible.cfg"))
			}

			got := withJobSearchPaths(env, workDir, tt.collections, tt.roles)

			for name, want := range map[string]string{
				"ANSIBLE_COLLECTIONS_PATH": tt.wantCollections(workDir),
				"ANSIBLE_ROLES_PATH":       tt.wantRoles(workDir),
			} {
				value, count := envValue(got, name)
				if value != want {
					t.Errorf("%s = %q, want %q", name, value, want)
				}
				if count != 1 {
					t.Errorf("%s is assigned %d times in the result, want exactly once", name, count)
				}
			}
			for _, kv := range env {
				if strings.HasPrefix(kv, "ANSIBLE_COLLECTIONS_PATH=") || strings.HasPrefix(kv, "ANSIBLE_ROLES_PATH=") {
					continue
				}
				if !slices.Contains(got, kv) {
					t.Errorf("unrelated variable %q was dropped", kv)
				}
			}
		})
	}
}

// The candidates follow Ansible's own order, and the home entry comes from the home
// directory handed in, never from this process's environment.
func TestAnsibleCfgCandidates(t *testing.T) {
	t.Setenv("HOME", "/home/of/the/runner/process")
	tests := []struct {
		name string
		home string
		want []string
	}{
		{
			name: "the job's home is the one used",
			home: "/tmp/ansible-home/job",
			want: []string{"/job/ansible.cfg", "/job/playbook/ansible.cfg", "/tmp/ansible-home/job/.ansible.cfg", "/etc/ansible/ansible.cfg"},
		},
		{
			name: "no home means no home entry",
			home: "",
			want: []string{"/job/ansible.cfg", "/job/playbook/ansible.cfg", "/etc/ansible/ansible.cfg"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ansibleCfgCandidates("/job/ansible.cfg", "/job/playbook", tt.home)
			if !slices.Equal(got, tt.want) {
				t.Errorf("ansibleCfgCandidates() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAnsibleCfgSearchPath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "first/ansible.cfg", "# comment\n[galaxy]\nroles_path = /wrong/section\n\n[defaults]\n; another comment\nroles_path = vendor/roles:~/shared:/abs/roles\ncollections_paths: legacy/collections\n")
	writeFile(t, dir, "second/ansible.cfg", "[defaults]\nroles_path = /from/second\n")
	first := filepath.Join(dir, "first", "ansible.cfg")
	second := filepath.Join(dir, "second", "ansible.cfg")
	missing := filepath.Join(dir, "missing", "ansible.cfg")

	tests := []struct {
		name       string
		candidates []string
		keys       []string
		want       string
	}{
		{"relative entries resolve against the file, home and absolute ones are kept", []string{first}, []string{"roles_path"}, filepath.Join(dir, "first", "vendor", "roles") + ":~/shared:/abs/roles"},
		{"the deprecated alias is honoured", []string{first}, []string{"collections_path", "collections_paths"}, filepath.Join(dir, "first", "legacy", "collections")},
		{"only the first existing file counts, as in Ansible", []string{missing, second, first}, []string{"roles_path"}, "/from/second"},
		{"a key the file does not set", []string{second}, []string{"collections_path", "collections_paths"}, ""},
		{"no file at all", []string{missing}, []string{"roles_path"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ansibleCfgSearchPath(tt.candidates, tt.keys...); got != tt.want {
				t.Errorf("ansibleCfgSearchPath() = %q, want %q", got, tt.want)
			}
		})
	}
}
