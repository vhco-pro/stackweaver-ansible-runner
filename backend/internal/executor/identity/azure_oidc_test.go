// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package identity

import (
	"os"
	"path/filepath"
	"testing"
)

// The Azure federated token is written to a file of the job and named by
// AZURE_FEDERATED_TOKEN_FILE, which the Azure SDKs and azure.azcollection's workload identity read;
// the token variable stays for the azcollection modules that take it directly.
func TestMaterializeAzureFederatedToken(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"AZURE_CLIENT_ID": "c", "AZURE_FEDERATED_TOKEN": "header.payload.sig"}
	if err := MaterializeAzureFederatedToken(env, dir); err != nil {
		t.Fatal(err)
	}
	path := env["AZURE_FEDERATED_TOKEN_FILE"]
	if path != filepath.Join(dir, azureFederatedTokenFile) {
		t.Fatalf("AZURE_FEDERATED_TOKEN_FILE = %q", path)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "header.payload.sig" { //nolint:gosec // the test's own directory
		t.Fatalf("token file = %q (%v)", data, err)
	}
	if env["AZURE_FEDERATED_TOKEN"] != "header.payload.sig" {
		t.Error("the token variable was removed")
	}

	// A file the environment already names (AKS workload identity) is left alone.
	env = map[string]string{"AZURE_FEDERATED_TOKEN": "t", "AZURE_FEDERATED_TOKEN_FILE": "/var/run/secrets/azure/tokens/azure-identity-token"} //nolint:gosec // a test path, not a credential
	if err := MaterializeAzureFederatedToken(env, dir); err != nil || env["AZURE_FEDERATED_TOKEN_FILE"] != "/var/run/secrets/azure/tokens/azure-identity-token" {
		t.Errorf("an existing token file was replaced: %v %v", env, err)
	}
	if err := MaterializeAzureFederatedToken(map[string]string{}, dir); err != nil {
		t.Errorf("no token: %v", err)
	}
}
