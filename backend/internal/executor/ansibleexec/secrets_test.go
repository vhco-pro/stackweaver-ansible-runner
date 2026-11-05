// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package ansibleexec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The inventory with its injected credentials, the vault password and the SSH key are written into
// the job's secret directory, never its working tree, and that directory is gone when the job ends,
// whether the play succeeded or failed (#933, D2, AC25).
func TestRunAnsiblePlaybook_CredentialsLiveInTheShreddedSecretDir(t *testing.T) {
	for _, outcome := range []string{"completed", "failed"} {
		t.Run(outcome, func(t *testing.T) {
			stubs := newJobStubs(t)
			if outcome == "failed" {
				if err := os.WriteFile(filepath.Join(stubs.dir, "fail"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cp := &fakeControlPlane{artifacts: map[string]any{
				"playbook_path":     "site.yml",
				"playbook_content":  "- hosts: all\n",
				"inventory_content": `{"all":{"hosts":{"h1":{"ansible_password":"placeholder-host-password"}}}}`,
				"vaults":            []map[string]any{{"vault_id": "prod", "password": "placeholder-vault-password"}},
				"credential":        map[string]any{"type": "ssh", "username": "deploy", "ssh_key": "placeholder-ssh-key"},
			}}

			run := runJob(t, cp, Options{})

			if run.result.Status != outcome {
				t.Fatalf("result = %+v, want %s", run.result, outcome)
			}
			args := strings.Fields(stubs.read(t, "args"))
			var files []string
			for i, a := range args {
				switch a {
				case "-i", "--private-key":
					files = append(files, args[i+1])
				case "--vault-id":
					_, f, _ := strings.Cut(args[i+1], "@")
					files = append(files, f)
				}
			}
			if len(files) != 3 {
				t.Fatalf("ansible-playbook args %q name %d credential files, want 3", args, len(files))
			}
			for _, f := range files {
				if !inSecretDir(run.root, f) {
					t.Errorf("%s is not in the job's secret directory", f)
				}
				if _, err := os.Stat(f); !os.IsNotExist(err) {
					t.Errorf("%s outlived the job (stat error %v)", f, err)
				}
			}
		})
	}
}
