// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// How the sync executor runs ansible-inventory: the environment it hands the engine and what it
// makes of the engine's exit (#886). The executor is the only process that runs the engine (#885),
// so these tests live beside it.
//
// ansible-core treats an inventory source no plugin could parse as a warning: it exits 0 and
// prints an empty inventory. A sync that trusts that exit records a wrong password or an
// unreachable endpoint as a successful sync that found nothing, and a source with overwrite on
// then removes every host and group it owns. The engine has a setting that turns the warning
// into a failure, and these tests pin that the executor sets it and that the failure it produces
// is an error carrying the engine's own words.
//
// The engine here is a shell script standing in for ansible-inventory. What the real one does
// with the same environment is checked by runner-images/ansible/tests/.

package syncexec

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeEnginePluginFailure behaves like ansible-core 2.21 when the plugin a source names raises:
// the plugin's error is a warning on stderr, and whether the run then fails depends on the
// unparsed-source setting alone.
const fakeEnginePluginFailure = `#!/bin/sh
echo "[WARNING]: Failed to parse inventory with 'auto' plugin: [Errno 111] Connection refused" >&2
if [ "$ANSIBLE_INVENTORY_ANY_UNPARSED_IS_FAILED" = "true" ]; then
	echo "[ERROR]: Completely failed to parse inventory source $2" >&2
	exit 1
fi
echo "[WARNING]: No inventory was parsed, only implicit localhost is available" >&2
printf '%s' '{"_meta": {"hostvars": {}}, "all": {"children": ["ungrouped"]}}'
`

// fakeEngineEmpty is a plugin that reached its provider and was told there is nothing: exit 0, an
// empty inventory and nothing on stderr, whatever the unparsed-source setting says.
const fakeEngineEmpty = `#!/bin/sh
printf '%s' '{"_meta": {"hostvars": {}}, "all": {"children": ["ungrouped"]}}'
`

// fakeEngineGarbage exits 0 without printing an inventory.
const fakeEngineGarbage = `#!/bin/sh
echo "not json"
`

// fakeAnsibleInventory puts script first on PATH as ansible-inventory for the rest of the test.
func fakeAnsibleInventory(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ansible-inventory"), []byte(script), 0o700); err != nil { //nolint:gosec // an executable is the point
		t.Fatalf("writing the fake ansible-inventory: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestInventoryCommandEnv(t *testing.T) {
	const want = "ANSIBLE_INVENTORY_ANY_UNPARSED_IS_FAILED=true"

	tests := []struct {
		name        string
		base        []string
		credentials []string
	}{
		{name: "nothing inherited"},
		{name: "runner environment and credentials", base: []string{"PATH=/usr/bin", "ANSIBLE_HOME=/tmp/.ansible"}, credentials: []string{"VMWARE_HOST=vc.example.com"}},
		{name: "the runner turned it off", base: []string{"ANSIBLE_INVENTORY_ANY_UNPARSED_IS_FAILED=false"}},
		{name: "a credential turned it off", credentials: []string{"ANSIBLE_INVENTORY_ANY_UNPARSED_IS_FAILED=false"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := slices.Clone(tt.base)
			got := InventoryCommandEnv(tt.base, tt.credentials)

			// os/exec keeps the last value of a duplicated key, so the setting holds only as the
			// final entry.
			if len(got) == 0 || got[len(got)-1] != want {
				t.Errorf("last entry = %q, want %q (env: %q)", lastOf(got), want, got)
			}
			for _, kv := range slices.Concat(tt.base, tt.credentials) {
				if !slices.Contains(got, kv) {
					t.Errorf("env dropped %q (env: %q)", kv, got)
				}
			}
			if !slices.Equal(tt.base, base) {
				t.Errorf("base was modified: %q, want %q", tt.base, base)
			}
		})
	}
}

func lastOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}

func TestRunAnsibleInventory_PluginFailureIsAnError(t *testing.T) {
	fakeAnsibleInventory(t, fakeEnginePluginFailure)

	// The runner's own environment says "only warn"; the sync must not inherit that.
	env := InventoryCommandEnv(
		append(os.Environ(), "ANSIBLE_INVENTORY_ANY_UNPARSED_IS_FAILED=false"), nil,
	)
	var diagnostics bytes.Buffer
	inventory, stderr, err := RunAnsibleInventory(t.Context(), []string{"-i", "/tmp/vmware.yml", "--list"}, env, &diagnostics)

	if err == nil {
		t.Fatalf("err = nil with inventory %v, want the engine's failure", inventory)
	}
	if inventory != nil {
		t.Errorf("inventory = %v, want nil: a failed run has nothing to reconcile", inventory)
	}
	for _, want := range []string{"Connection refused", "Completely failed to parse inventory source /tmp/vmware.yml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to carry %q", err, want)
		}
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to carry %q", stderr, want)
		}
		if !strings.Contains(diagnostics.String(), want) {
			t.Errorf("diagnostics = %q, want it to carry %q", diagnostics.String(), want)
		}
	}
}

func TestRunAnsibleInventory_EmptyResultIsNotAnError(t *testing.T) {
	fakeAnsibleInventory(t, fakeEngineEmpty)

	inventory, stderr, err := RunAnsibleInventory(
		t.Context(), []string{"-i", "/tmp/vmware.yml", "--list"}, InventoryCommandEnv(os.Environ(), nil), &bytes.Buffer{},
	)
	if err != nil {
		t.Fatalf("err = %v, want nil: a source that has no hosts is a successful sync", err)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	if inventory == nil {
		t.Fatal("inventory = nil, want the empty inventory the engine printed")
	}
	meta, _ := inventory["_meta"].(map[string]any)
	if hostvars, _ := meta["hostvars"].(map[string]any); len(hostvars) != 0 {
		t.Errorf("hostvars = %v, want none", hostvars)
	}
}

func TestRunAnsibleInventory_UnreadableOutputIsAnError(t *testing.T) {
	fakeAnsibleInventory(t, fakeEngineGarbage)

	inventory, _, err := RunAnsibleInventory(
		t.Context(), []string{"-i", "/tmp/vmware.yml", "--list"}, InventoryCommandEnv(os.Environ(), nil), &bytes.Buffer{},
	)
	if err == nil {
		t.Fatalf("err = nil with inventory %v, want a parse error", inventory)
	}
	if inventory != nil {
		t.Errorf("inventory = %v, want nil", inventory)
	}
}

func TestRunAnsibleInventory_MissingEngineIsAnError(t *testing.T) {
	// Nothing on PATH: what a process sees when Ansible is not installed beside it.
	t.Setenv("PATH", t.TempDir())

	inventory, _, err := RunAnsibleInventory(
		t.Context(), []string{"-i", "/tmp/vmware.yml", "--list"}, InventoryCommandEnv(os.Environ(), nil), &bytes.Buffer{},
	)
	if err == nil {
		t.Fatalf("err = nil with inventory %v, want the exec failure", inventory)
	}
	if inventory != nil {
		t.Errorf("inventory = %v, want nil", inventory)
	}
}
