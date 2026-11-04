// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

// The runner binary is a launcher (#933, D10, AC57, AC58): it holds no Redis client, consumes no
// queue and has no in-binary job path; every job runs through the launcher and an executor.

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestBinary_HasNoQueuePath(t *testing.T) {
	forbiddenImports := map[string]bool{
		"github.com/michielvha/stackweaver/core/queue": true,
		"github.com/redis/go-redis/v9":                 true,
	}
	forbiddenFuncs := map[string]bool{
		"processJob": true, "processSyncJob": true, "consumeReliable": true, "RunAgentMode": true,
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			if path, _ := strconv.Unquote(imp.Path.Value); forbiddenImports[path] {
				t.Errorf("%s imports %s", name, path)
			}
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && forbiddenFuncs[fn.Name.Name] {
				t.Errorf("%s still declares %s", name, fn.Name.Name)
			}
		}
	}
}
