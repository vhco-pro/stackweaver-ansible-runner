// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package syncexec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"slices"
)

// failOnUnparsedSource is ansible-core's INVENTORY_ANY_UNPARSED_IS_FAILED setting, switched on.
//
// Left at its default, the engine treats a source that no inventory plugin could parse as a
// warning: it exits 0 and prints an empty inventory, which is also exactly what a plugin prints
// after reaching its provider and finding no hosts. A wrong password or an unreachable endpoint
// would then be a successful sync of nothing, and a source with overwrite on would remove every
// host and group it owns (#886). With the setting on, the engine exits non-zero in the first
// case and the two can be told apart by the exit code, with no guessing from the host count.
//
// It is the "any" setting and not INVENTORY_UNPARSED_IS_FAILED (ANSIBLE_INVENTORY_UNPARSED_FAILED):
// that one fails a run only when no source at all was parsed. A source sync passes one source,
// where the two agree, but a constructed inventory build passes each input and its rules file as
// separate sources, and there only the "any" setting notices that one of them was skipped.
const failOnUnparsedSource = "ANSIBLE_INVENTORY_ANY_UNPARSED_IS_FAILED=true"

// InventoryCommandEnv is the environment for an ansible-inventory run whose output will be
// reconciled into the database: base (ansible.InheritedEnv, never the launcher's whole
// environment), then the credentials of the source being synced, then failOnUnparsedSource. The
// order matters, because os/exec keeps the last value of a duplicated key: neither a variable set
// on the launcher nor one a credential carries can turn the setting back off.
func InventoryCommandEnv(base, credentials []string) []string {
	return slices.Concat(base, credentials, []string{failOnUnparsedSource})
}

// RunAnsibleInventory runs ansible-inventory with args and returns the inventory it printed. The
// engine's stderr is copied to diagnostics while it runs and returned whole, error or not.
//
// An error means there is no inventory to act on: the engine could not be run, exited non-zero
// (the error carries its stderr, which is where a plugin says what went wrong), or printed
// something that is not an inventory. An empty inventory with a nil error is a real answer.
// Whether a source the engine could not parse is an error is decided by env: pass
// InventoryCommandEnv, or the engine's default turns such a source into a warning.
//
// This executor is the one place the engine is run (#885): the API ships no Ansible, and a scan
// in core (TestNoEngineExecOutsideTheExecutor) keeps any other caller from appearing.
func RunAnsibleInventory(ctx context.Context, args, env []string, diagnostics io.Writer) (inventory map[string]any, stderr string, err error) {
	cmd := exec.CommandContext(ctx, "ansible-inventory", args...) //nolint:gosec // intentional: executing ansible command
	cmd.Env = env

	var stdout, captured bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.MultiWriter(&captured, diagnostics)

	if err := cmd.Run(); err != nil {
		return nil, captured.String(), fmt.Errorf("running ansible-inventory: %w\nstderr: %s", err, captured.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &inventory); err != nil {
		return nil, captured.String(), fmt.Errorf("parsing ansible-inventory output: %w", err)
	}
	return inventory, captured.String(), nil
}
