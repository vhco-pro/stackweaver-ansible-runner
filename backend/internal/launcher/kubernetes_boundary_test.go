// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The launcher places pods only through core/runnerdriver/kubernetes (#933, D11, AC45): no source
// under backend/ speaks the Kubernetes API itself, and only the launcher's Kubernetes driver
// imports the shared package.
func TestKubernetesPlacementGoesThroughTheCorePackage(t *testing.T) {
	root := filepath.Join("..", "..")
	const shared = "github.com/michielvha/stackweaver/core/runnerdriver/kubernetes"
	importers := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path) //nolint:gosec // G304: a source file of this module
		if err != nil {
			return err
		}
		s := string(src)
		if strings.Contains(s, "/api/v1/namespaces") || strings.Contains(s, "k8s.io/client-go") {
			t.Errorf("%s speaks the Kubernetes API itself; place pods through %s", path, shared)
		}
		if strings.Contains(s, `"`+shared+`"`) {
			importers++
			if !strings.HasSuffix(filepath.ToSlash(path), "internal/launcher/driver/kubernetes.go") {
				t.Errorf("%s imports %s; only the launcher's Kubernetes driver does", path, shared)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if importers == 0 {
		t.Error("no Kubernetes driver imports the shared package yet")
	}
}
