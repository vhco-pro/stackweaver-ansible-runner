// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package identity

import (
	"fmt"
	"os"
	"path/filepath"
)

const azureFederatedTokenFile = "azure-federated-token.jwt"

// MaterializeAzureFederatedToken writes the per-run Azure federated token (AZURE_FEDERATED_TOKEN,
// minted by the API for the organization's Azure OIDC configuration) to a file in dir and names it
// in AZURE_FEDERATED_TOKEN_FILE, which the Azure SDKs and azure.azcollection's workload identity
// read. A token file the environment already names, as AKS workload identity injects, is kept.
func MaterializeAzureFederatedToken(env map[string]string, dir string) error {
	token := env["AZURE_FEDERATED_TOKEN"]
	if token == "" || env["AZURE_FEDERATED_TOKEN_FILE"] != "" {
		return nil
	}
	path := filepath.Join(dir, azureFederatedTokenFile)
	//nolint:gosec // G703: path is <the job's secret dir>/<constant filename>
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return fmt.Errorf("writing the Azure federated token file: %w", err)
	}
	env["AZURE_FEDERATED_TOKEN_FILE"] = path
	return nil
}
