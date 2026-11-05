// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package launcher

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// launcherSourceRoots are the source trees that run in a launcher process: this package, its
// driver and cache subpackages, and the shared Kubernetes placement package its Kubernetes driver
// calls.
var launcherSourceRoots = []string{".", filepath.Join("..", "..", "..", "core", "runnerdriver")}

// The launcher never receives a job's decrypted credentials (#933, D1, AC4): no source file that
// runs in a launcher process names the artifacts route in a string, decodes an artifacts document
// or imports an executor package, so the only process that fetches artifacts is the executor, with
// the job token, wherever the driver placed it. The network half is the capture procedure in
// docs/internal/plans/infrastructure/runners/procedures/launcher-artifacts-capture.md.
func TestLauncherHasNoArtifactsPath(t *testing.T) {
	fset := token.NewFileSet()
	checked := map[string]int{}
	for _, root := range launcherSourceRoots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			checked[filepath.Dir(path)]++
			checkLauncherSource(t, fset, path)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// The walk reaches every tree a launcher runs, not only the top package.
	for _, dir := range []string{".", "driver", "cache", filepath.Join("..", "..", "..", "core", "runnerdriver", "kubernetes")} {
		if checked[dir] == 0 {
			t.Errorf("no source of %s was checked", dir)
		}
	}
}

// artifactsDocuments are the runnercontract types an artifacts response decodes into.
var artifactsDocuments = map[string]bool{"TofuArtifacts": true, "AnsibleArtifacts": true, "WorkItemArtifacts": true}

func checkLauncherSource(t *testing.T, fset *token.FileSet, path string) {
	t.Helper()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if strings.Contains(p, "/internal/executor") {
			t.Errorf("%s imports %s: a launcher must not run or reach the executor itself", path, p)
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BasicLit:
			if n.Kind != token.STRING {
				return true
			}
			if s, err := strconv.Unquote(n.Value); err == nil && strings.Contains(strings.ToLower(s), "artifacts") {
				t.Errorf("%s names the artifacts route: %q", fset.Position(n.Pos()), s)
			}
		case *ast.SelectorExpr:
			if artifactsDocuments[n.Sel.Name] {
				t.Errorf("%s uses %s: a launcher never decodes an artifacts document", fset.Position(n.Pos()), n.Sel.Name)
			}
		}
		return true
	})
}
