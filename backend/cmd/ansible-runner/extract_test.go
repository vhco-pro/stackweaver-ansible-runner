// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildTarGz packs the given files, in order, into a gzip-compressed tarball.
func buildTarGz(t *testing.T, names []string, content func(name string) []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range names {
		data := content(name)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("writing header for %s: %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}
	return buf.Bytes()
}

// withExtractLimits shrinks the extraction limits for one test.
func withExtractLimits(t *testing.T, perFile, total int64, entries int) {
	t.Helper()
	old := extractLimits
	extractLimits.perFile, extractLimits.total, extractLimits.entries = perFile, total, entries
	t.Cleanup(func() { extractLimits = old })
}

func TestExtractTarGz_Limits(t *testing.T) {
	sized := func(n int) func(string) []byte {
		return func(string) []byte { return bytes.Repeat([]byte("x"), n) }
	}
	tests := []struct {
		name    string
		files   []string
		content func(string) []byte
		wantErr string // "" means the archive must extract
	}{
		{"everything within the limits", []string{"site.yml", "roles/web/tasks/main.yml"}, sized(100), ""},
		{"a file exactly at the per-file limit", []string{"site.yml"}, sized(1000), ""},
		// A file over the limit used to be cut off silently, and the truncated playbook ran.
		{"a file over the per-file limit", []string{"site.yml"}, sized(1001), "larger than the 1000 byte per-file limit"},
		{"files that together exceed the total limit", []string{"a.yml", "b.yml", "c.yml"}, sized(900), "more than the 2500 byte limit"},
		{"more entries than the entry limit", []string{"1", "2", "3", "4", "5", "6"}, sized(1), "more than 5 entries"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withExtractLimits(t, 1000, 2500, 5)
			target := t.TempDir()

			err := extractTarGz(buildTarGz(t, tt.files, tt.content), target)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("extractTarGz() error = %v, want the archive extracted", err)
				}
				for _, name := range tt.files {
					info, statErr := os.Stat(filepath.Join(target, name))
					if statErr != nil || info.Size() != int64(len(tt.content(name))) {
						t.Errorf("%s: stat = %v, %v; want the whole file", name, info, statErr)
					}
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("extractTarGz() error = %v, want one mentioning %q", err, tt.wantErr)
			}
		})
	}
}
