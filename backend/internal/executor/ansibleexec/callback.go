// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansibleexec

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

// CallbackName is the stdout callback every play reports through (#933, D6).
const CallbackName = "stackweaver_jsonl"

// callbackSource is the callback plugin, which the executor writes into each job, so a job in any
// image, an execution environment without ansible.posix included, reports the same events.
//
//go:embed callback/stackweaver_jsonl.py
var callbackSource string

// ansibleCallbackDefaults are Ansible's own callback plugin paths, kept after the job's.
const ansibleCallbackDefaults = "~/.ansible/plugins/callback:/usr/share/ansible/plugins/callback"

// writeCallback writes the callback plugin into dir and returns the ANSIBLE_CALLBACK_PLUGINS value
// that finds it first.
func writeCallback(dir string) (string, error) {
	plugins := filepath.Join(dir, "callback_plugins")
	if err := os.MkdirAll(plugins, 0o700); err != nil {
		return "", fmt.Errorf("creating the callback directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(plugins, CallbackName+".py"), []byte(callbackSource), 0o600); err != nil {
		return "", fmt.Errorf("writing the callback plugin: %w", err)
	}
	return plugins + ":" + ansibleCallbackDefaults, nil
}
