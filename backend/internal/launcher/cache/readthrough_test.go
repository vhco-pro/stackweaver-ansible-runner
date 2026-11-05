// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/michielvha/stackweaver/core/runnercontract"
)

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fakeSource is the API's GET /runner/cache route: what it holds per organization, and how often it
// was asked.
type fakeSource struct {
	objects map[string][]byte // org/kind/sha -> body
	fetches int
}

func (s *fakeSource) Fetch(_ context.Context, org string, kind runnercontract.CacheKind, sum string) (io.ReadCloser, error) {
	s.fetches++
	b, ok := s.objects[org+"/"+string(kind)+"/"+sum]
	if !ok {
		return nil, ErrNotCached
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func descriptor(org string, entries ...runnercontract.CacheEntry) *runnercontract.CacheDescriptor {
	d := &runnercontract.CacheDescriptor{OrganizationID: org}
	for _, e := range entries {
		d.Entries = append(d.Entries, runnercontract.CacheEntryRef{CacheEntry: e})
	}
	return d
}

const orgA = "11111111-1111-1111-1111-111111111111"

func TestReadThrough_PlacesOnlyVerifiedEntriesUnderTheOrganization(t *testing.T) {
	good := []byte("tofu 1.10.0 linux_amd64 zip")
	forged := []byte("not what the key says")
	src := &fakeSource{objects: map[string][]byte{
		orgA + "/tofu/" + digest(good):                            good,
		orgA + "/provider/" + digest([]byte("the real provider")): forged,
	}}
	rt := &ReadThrough{Dir: t.TempDir(), Source: src}
	d := descriptor(orgA,
		runnercontract.CacheEntry{Kind: runnercontract.CacheKindTofu, Name: "tofu", Version: "1.10.0", SHA256: digest(good)},
		runnercontract.CacheEntry{Kind: runnercontract.CacheKindProvider, Name: "registry.opentofu.org/hashicorp/null", SHA256: digest([]byte("the real provider"))},
		runnercontract.CacheEntry{Kind: runnercontract.CacheKindCollection, Name: "ansible.posix", SHA256: digest([]byte("absent"))},
	)
	dir, err := rt.Prepare(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(rt.Dir, orgA) {
		t.Errorf("the mount source is %s, want the organization's directory", dir)
	}
	got, err := os.ReadFile(filepath.Join(dir, "tofu", digest(good))) //nolint:gosec // G304: the test's own directory
	if err != nil || !bytes.Equal(got, good) {
		t.Fatalf("the verified entry was not placed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "provider", digest([]byte("the real provider")))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an entry whose body does not hash to its key was placed (%v)", err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "provider"))
	if len(entries) != 0 {
		t.Errorf("the refused entry left %d files behind", len(entries))
	}
	if _, err := os.Stat(filepath.Join(dir, "collection", digest([]byte("absent")))); !errors.Is(err, os.ErrNotExist) {
		t.Error("an entry the API does not hold appeared")
	}
}

func TestReadThrough_SecondJobFetchesNothing(t *testing.T) {
	body := []byte("ansible.posix-2.2.2.tar.gz")
	src := &fakeSource{objects: map[string][]byte{orgA + "/collection/" + digest(body): body}}
	rt := &ReadThrough{Dir: t.TempDir(), Source: src}
	d := descriptor(orgA, runnercontract.CacheEntry{Kind: runnercontract.CacheKindCollection, Name: "ansible.posix", Version: "2.2.2", SHA256: digest(body)})
	if _, err := rt.Prepare(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Prepare(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if src.fetches != 1 {
		t.Errorf("two jobs on one node fetched %d times, want 1", src.fetches)
	}
}

func TestReadThrough_AnAlteredEntryIsRefetched(t *testing.T) {
	body := []byte("terraform-provider-null zip")
	src := &fakeSource{objects: map[string][]byte{orgA + "/provider/" + digest(body): body}}
	rt := &ReadThrough{Dir: t.TempDir(), Source: src}
	d := descriptor(orgA, runnercontract.CacheEntry{Kind: runnercontract.CacheKindProvider, Name: "null", SHA256: digest(body)})
	dir, err := rt.Prepare(t.Context(), d)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "provider", digest(body))
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered on the node"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Prepare(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path) //nolint:gosec // G304: the test's own directory
	if !bytes.Equal(got, body) {
		t.Errorf("the altered entry was kept: %q", got)
	}
	if src.fetches != 2 {
		t.Errorf("fetches = %d, want the altered entry fetched again", src.fetches)
	}
}

func TestReadThrough_OrganizationsNeverShareADirectory(t *testing.T) {
	const orgB = "22222222-2222-2222-2222-222222222222"
	body := []byte("same public content")
	src := &fakeSource{objects: map[string][]byte{
		orgA + "/tofu/" + digest(body): body,
		orgB + "/tofu/" + digest(body): body,
	}}
	rt := &ReadThrough{Dir: t.TempDir(), Source: src}
	e := runnercontract.CacheEntry{Kind: runnercontract.CacheKindTofu, Name: "tofu", SHA256: digest(body)}
	dirA, _ := rt.Prepare(t.Context(), descriptor(orgA, e))
	dirB, _ := rt.Prepare(t.Context(), descriptor(orgB, e))
	if dirA == dirB {
		t.Fatal("two organizations got one directory")
	}
	if src.fetches != 2 {
		t.Errorf("fetches = %d: each organization's entry comes from its own prefix", src.fetches)
	}
	if _, err := rt.Prepare(t.Context(), descriptor("../etc", e)); err == nil {
		t.Error("an organization id that is not a UUID was accepted as a directory name")
	}
}

func TestReadThrough_EvictsLeastRecentlyUsedOverTheLimit(t *testing.T) {
	one, two, three := bytes.Repeat([]byte("1"), 40), bytes.Repeat([]byte("2"), 40), bytes.Repeat([]byte("3"), 40)
	src := &fakeSource{objects: map[string][]byte{}}
	for _, b := range [][]byte{one, two, three} {
		src.objects[orgA+"/tofu/"+digest(b)] = b
	}
	rt := &ReadThrough{Dir: t.TempDir(), Source: src, Limit: 100}
	entry := func(b []byte) runnercontract.CacheEntry {
		return runnercontract.CacheEntry{Kind: runnercontract.CacheKindTofu, Name: "tofu", SHA256: digest(b)}
	}
	dir, _ := rt.Prepare(t.Context(), descriptor(orgA, entry(one)))
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(filepath.Join(dir, "tofu", digest(one)), old, old)
	_, _ = rt.Prepare(t.Context(), descriptor(orgA, entry(two)))
	_, _ = rt.Prepare(t.Context(), descriptor(orgA, entry(three)))
	if _, err := os.Stat(filepath.Join(dir, "tofu", digest(one))); !errors.Is(err, os.ErrNotExist) {
		t.Error("the least recently used entry survived going over the limit")
	}
	for _, b := range [][]byte{two, three} {
		if _, err := os.Stat(filepath.Join(dir, "tofu", digest(b))); err != nil {
			t.Errorf("a recent entry was evicted: %v", err)
		}
	}
}

func TestReadThrough_PlacedEntriesAreReadOnly(t *testing.T) {
	body := []byte("read only")
	src := &fakeSource{objects: map[string][]byte{orgA + "/tofu/" + digest(body): body}}
	rt := &ReadThrough{Dir: t.TempDir(), Source: src}
	dir, _ := rt.Prepare(t.Context(), descriptor(orgA, runnercontract.CacheEntry{Kind: runnercontract.CacheKindTofu, SHA256: digest(body)}))
	info, err := os.Stat(filepath.Join(dir, "tofu", digest(body)))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o222 != 0 {
		t.Errorf("a placed entry is writable: %v", info.Mode())
	}
}
