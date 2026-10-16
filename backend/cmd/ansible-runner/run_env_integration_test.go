// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// End to end on the platform runner's queue path (#932): a constructed inventory build runs
// ansible-inventory, and the stand-in plays the constructed rules a user wrote, recording
// everything it can read. It needs real rows, so it is gated behind `integration` and skips
// unless $TEST_DATABASE_URL is set. Run with:
//
//	go test -tags integration ./cmd/ansible-runner/ -run TestConstructedBuild_RulesCannotReadRunnerSecrets

//go:build integration

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConstructedBuild_RulesCannotReadRunnerSecrets(t *testing.T) {
	f := setupConstructedFixture(t)
	setRunnerSecrets(t)
	record := filepath.Join(t.TempDir(), "env")
	fakeAnsibleInventory(t, `#!/bin/sh
env > "`+record+`"
cat <<'INVENTORY'
`+f.inputsOnlyOutput()+`
INVENTORY
`)

	f.rebuild(t)

	seen, err := os.ReadFile(record) //nolint:gosec // G304: a file this test's own stand-in wrote
	if err != nil {
		t.Fatalf("the stand-in ansible-inventory recorded no environment: %v", err)
	}
	assertNoRunnerSecrets(t, "ansible-inventory (constructed inventory build)", strings.Split(strings.TrimSpace(string(seen)), "\n"))
}
