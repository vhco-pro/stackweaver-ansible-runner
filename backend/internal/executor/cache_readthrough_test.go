// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// blobServer is the API's blob route for cache entries, counting what it served.
func blobServer(t *testing.T, objects map[string][]byte) (*httptest.Server, *int) {
	t.Helper()
	served := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := objects[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		served++
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv, &served
}

func ref(kind runnercontract.CacheKind, body []byte) runnercontract.CacheEntryRef {
	sum := hexSum(body)
	return runnercontract.CacheEntryRef{
		CacheEntry: runnercontract.CacheEntry{Kind: kind, SHA256: sum},
		Path:       "/api/v2/runner/jobs/run-x/blob/cache/" + string(kind) + "/" + sum,
	}
}

func TestCache_ReadsTheLocalCopyWhenItVerifies(t *testing.T) {
	body := []byte("tofu release zip")
	e := ref(runnercontract.CacheKindTofu, body)
	local := t.TempDir()
	if err := os.MkdirAll(filepath.Join(local, "tofu"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "tofu", e.SHA256), body, 0o400); err != nil {
		t.Fatal(err)
	}
	srv, served := blobServer(t, map[string][]byte{e.Path: body})
	c := &Cache{Dir: local, Client: NewClient(srv.URL, "job-token", "runner")}
	dst := filepath.Join(t.TempDir(), "tofu.zip")
	src, err := c.Fetch(t.Context(), e, dst)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst) //nolint:gosec // G304: the test's own directory
	if !bytes.Equal(got, body) || src != SourceLocal {
		t.Errorf("fetched %q from %s, want the local copy", got, src)
	}
	if *served != 0 {
		t.Errorf("the blob route served %d times though the local copy verified", *served)
	}
}

func TestCache_FallsBackToTheBlobRouteOnAMissingOrAlteredCopy(t *testing.T) {
	body := []byte("galaxy collection tarball")
	e := ref(runnercontract.CacheKindCollection, body)
	srv, served := blobServer(t, map[string][]byte{e.Path: body})

	// Missing.
	c := &Cache{Dir: t.TempDir(), Client: NewClient(srv.URL, "job-token", "runner")}
	dst := filepath.Join(t.TempDir(), "c.tar.gz")
	if src, err := c.Fetch(t.Context(), e, dst); err != nil || src != SourceBlob {
		t.Fatalf("missing local copy: %v from %s", err, src)
	}

	// Altered.
	local := t.TempDir()
	_ = os.MkdirAll(filepath.Join(local, "collection"), 0o750)
	_ = os.WriteFile(filepath.Join(local, "collection", e.SHA256), []byte("altered"), 0o400)
	c = &Cache{Dir: local, Client: NewClient(srv.URL, "job-token", "runner")}
	if src, err := c.Fetch(t.Context(), e, dst); err != nil || src != SourceBlob {
		t.Fatalf("altered local copy: %v from %s", err, src)
	}
	got, _ := os.ReadFile(dst) //nolint:gosec // G304: the test's own directory
	if !bytes.Equal(got, body) {
		t.Errorf("the altered copy was used: %q", got)
	}
	if *served != 2 {
		t.Errorf("blob route served %d, want 2", *served)
	}
	// The executor leaves the local copy as it found it: it is the launcher's to repair.
	left, _ := os.ReadFile(filepath.Join(local, "collection", e.SHA256)) //nolint:gosec // G304: the test's own directory
	if string(left) != "altered" {
		t.Error("the executor wrote the launcher's local copy")
	}
}

func TestCache_RefusesABlobThatDoesNotVerify(t *testing.T) {
	body := []byte("the published provider")
	e := ref(runnercontract.CacheKindProvider, body)
	srv, _ := blobServer(t, map[string][]byte{e.Path: []byte("a substituted body")})
	c := &Cache{Client: NewClient(srv.URL, "job-token", "runner")}
	dst := filepath.Join(t.TempDir(), "p.zip")
	if _, err := c.Fetch(t.Context(), e, dst); err == nil {
		t.Fatal("a blob that does not hash to its key was accepted")
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("the unverified body was left at the destination")
	}
}

func TestCache_UnavailableEntryIsNotAnError(t *testing.T) {
	e := ref(runnercontract.CacheKindTofu, []byte("never cached"))
	srv, _ := blobServer(t, nil)
	c := &Cache{Dir: t.TempDir(), Client: NewClient(srv.URL, "job-token", "runner")}
	src, err := c.Fetch(t.Context(), e, filepath.Join(t.TempDir(), "x"))
	if err != nil || src != SourceNone {
		t.Errorf("an entry the job cannot get: %v from %s, want none and no error (the job fetches from the source)", err, src)
	}
}

// TestCache_HasNoWritePath holds the executor to reading the cache only (AC34): no file in the
// executor's packages opens the local copy for writing or reaches the cache's write route.
func TestCache_HasNoWritePath(t *testing.T) {
	fset := token.NewFileSet()
	for _, dir := range []string{".", "tofuexec", "ansibleexec", "syncexec"} {
		names, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, name, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if ok && lit.Kind == token.STRING && strings.Contains(lit.Value, "/runner/cache") {
					t.Errorf("%s names the cache write route %s", name, lit.Value)
				}
				return true
			})
		}
	}
	src, err := os.ReadFile("cache.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, write := range []string{"os.Create(", "os.WriteFile(", "os.OpenFile(", "os.Rename(", "os.Remove(", "os.Chmod("} {
		for _, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, write) && strings.Contains(line, "c.Dir") {
				t.Errorf("cache.go writes under the local copy: %s", strings.TrimSpace(line))
			}
		}
	}
}
