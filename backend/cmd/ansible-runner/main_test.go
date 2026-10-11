// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package main

import (
	"path/filepath"
	"slices"
	"testing"
)

// The platform runner hands ansible-playbook the environment playbookEnv assembles. A
// job whose repository has no requirements file gets no search path variable from the
// runner at all: an environment variable beats ansible.cfg, so setting one would
// override a collections_path or roles_path configured there, and it would replace a
// value an operator set on the runner container. That is the rule the self-hosted agent
// already follows.
func TestPlaybookEnv_NoGalaxyLeavesAnsiblePathsAlone(t *testing.T) {
	// A real job with nothing staged hands in empty directories. Directories are handed in
	// here anyway, to pin that galaxyStaged alone decides whether the paths are set.
	const (
		warmCollections = "/workspaces/galaxy-cache/project/collections"
		warmRoles       = "/workspaces/galaxy-cache/project/roles"
	)
	tests := []struct {
		name            string
		base            []string
		wantCollections string // "" for unset
		wantRoles       string // "" for unset
	}{
		{
			name: "neither variable is set by the runner",
			base: []string{"PATH=/usr/bin"},
		},
		{
			name:            "a variable set on the runner container is passed through untouched",
			base:            []string{"PATH=/usr/bin", "ANSIBLE_COLLECTIONS_PATH=/opt/site/collections", "ANSIBLE_ROLES_PATH=/opt/site/roles"},
			wantCollections: "/opt/site/collections",
			wantRoles:       "/opt/site/roles",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir := t.TempDir()
			// A collections_path in the job's ansible.cfg must stay in charge, which it
			// only does while the variable is absent.
			writeFile(t, workDir, "ansible.cfg", "[defaults]\ncollections_path = vendor/collections\nroles_path = vendor/roles\n")
			jobVars := map[string]string{"ANSIBLE_HOME": "/job/.ansible"}

			env := playbookEnv(tt.base, jobVars, workDir, false, warmCollections, warmRoles)

			for name, want := range map[string]string{
				"ANSIBLE_COLLECTIONS_PATH": tt.wantCollections,
				"ANSIBLE_ROLES_PATH":       tt.wantRoles,
			} {
				value, count := envValue(env, name)
				if want == "" && count != 0 {
					t.Errorf("ansible-playbook would see %s=%q, want it unset so ansible.cfg stays in charge", name, value)
				}
				if want != "" && (value != want || count != 1) {
					t.Errorf("ansible-playbook would see %s=%q (%d assignments), want %q exactly once", name, value, count, want)
				}
			}
		})
	}
}

// A job whose requirements file installed gets the staged directory first, then the
// playbook-adjacent directory, then what was in effect before: the same value the
// self-hosted agent computes, because it is the same function.
func TestPlaybookEnv_StagedGalaxyComesFirst(t *testing.T) {
	const (
		stagedCollections = "/job/galaxy/collections"
		stagedRoles       = "/job/galaxy/roles"
	)
	t.Setenv("HOME", t.TempDir()) // a ~/.ansible.cfg on this machine must not decide the outcome
	workDir := t.TempDir()
	writeFile(t, workDir, "ansible.cfg", "[defaults]\ncollections_path = vendor/collections\n")
	base := []string{"PATH=/usr/bin", "ANSIBLE_ROLES_PATH=/opt/site/roles"}

	env := playbookEnv(base, map[string]string{"ANSIBLE_HOME": "/job/.ansible"}, workDir, true, stagedCollections, stagedRoles)

	// collections: staged, playbook-adjacent, then the ansible.cfg value.
	wantCollections := stagedCollections + ":" + filepath.Join(workDir, "collections") + ":" + filepath.Join(workDir, "vendor", "collections")
	if got, count := envValue(env, "ANSIBLE_COLLECTIONS_PATH"); got != wantCollections || count != 1 {
		t.Errorf("ANSIBLE_COLLECTIONS_PATH = %q (%d assignments), want %q exactly once", got, count, wantCollections)
	}
	// roles: staged, playbook-adjacent, then the value set on the runner container.
	wantRoles := stagedRoles + ":" + filepath.Join(workDir, "roles") + ":/opt/site/roles"
	if got, count := envValue(env, "ANSIBLE_ROLES_PATH"); got != wantRoles || count != 1 {
		t.Errorf("ANSIBLE_ROLES_PATH = %q (%d assignments), want %q exactly once", got, count, wantRoles)
	}
}

// Everything else in the environment is unchanged by where it is assembled: the runner's
// own variables are kept, every job gets the JSONL callback its output is parsed from,
// and a job variable wins over an inherited one of the same name.
func TestPlaybookEnv_JobVariablesWinOverTheRunnerEnvironment(t *testing.T) {
	t.Setenv("ANSIBLE_HOST_KEY_CHECKING", "")
	base := []string{"PATH=/usr/bin", "HOME=/home/stackweaver"}
	jobVars := map[string]string{"HOME": "/tmp/ansible-home/job", "ANSIBLE_HOME": "/job/.ansible"}

	env := playbookEnv(base, jobVars, t.TempDir(), false, "", "")

	for name, want := range map[string]string{
		"PATH":                          "/usr/bin",
		"HOME":                          "/tmp/ansible-home/job",
		"ANSIBLE_HOME":                  "/job/.ansible",
		"ANSIBLE_STDOUT_CALLBACK":       "ansible.posix.jsonl",
		"ANSIBLE_LOAD_CALLBACK_PLUGINS": "true",
		"ANSIBLE_HOST_KEY_CHECKING":     "false",
		"ANSIBLE_RETRY_FILES_ENABLED":   "false",
	} {
		if got, _ := envValue(env, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if !slices.Equal(base, []string{"PATH=/usr/bin", "HOME=/home/stackweaver"}) {
		t.Errorf("the runner environment handed in was modified: %v", base)
	}
}
