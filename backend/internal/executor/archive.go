// Copyright (c) 2025 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/michielvha/logger"
	"github.com/michielvha/stackweaver/core/security/archive"
)

// extractLimits bound what one archive may unpack to, against decompression bombs and
// runaway snapshots. A snapshot download is capped at 1 GiB compressed
// (the snapshot download cap), so the total allows a few times that once unpacked; the
// per-file limit is the long-standing one. Variables so tests can shrink them.
var extractLimits = struct {
	perFile int64 // bytes one entry may unpack to
	total   int64 // bytes the whole archive may unpack to
	entries int   // entries (files, directories, anything) the archive may hold
}{
	perFile: 100 << 20,
	total:   4 << 30,
	entries: 200_000,
}

// ExtractTarGz unpacks a gzip-compressed tarball read from r into targetDir: a job's configuration,
// a playbook snapshot or any other archive the executor is served. Every entry name is validated
// before a path is built from it, and the unpacked size is bounded per file, in total and in
// entries; an archive over a limit fails the extraction rather than leaving a truncated tree,
// because a truncated playbook or configuration would run.
func ExtractTarGz(r io.Reader, targetDir string) error {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("creating gzip reader: %w", err)
	}
	defer func() {
		if err := gzr.Close(); err != nil {
			logger.Warnf("Failed to close gzip reader: %v", err)
		}
	}()

	tr := tar.NewReader(gzr)

	var (
		entries   int
		extracted int64
	)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar entry: %w", err)
		}
		entries++
		if entries > extractLimits.entries {
			return fmt.Errorf("archive has more than %d entries", extractLimits.entries)
		}

		// Validate the entry name BEFORE constructing any filesystem path.
		// archive.SafeEntryName uses filepath.IsLocal which CodeQL's
		// go/zipslip query recognises as a sanitiser (Wave 8 / D3).
		safeName, err := archive.SafeEntryName(header.Name)
		if err != nil {
			return fmt.Errorf("invalid file path in archive: %w", err)
		}
		target := filepath.Join(targetDir, safeName) //nolint:gosec // safeName validated by archive.SafeEntryName

		// Defence-in-depth: also verify the joined path stays under targetDir.
		cleanTarget := filepath.Clean(target)
		cleanTargetDir := filepath.Clean(targetDir)
		if !strings.HasPrefix(cleanTarget, cleanTargetDir+string(filepath.Separator)) && cleanTarget != cleanTargetDir {
			return fmt.Errorf("invalid file path in archive (directory traversal attempt): %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil { //nolint:gosec // archive directory permissions
				return fmt.Errorf("creating directory: %w", err)
			}
		case tar.TypeReg:
			// Ensure parent directory exists
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return fmt.Errorf("creating parent directory: %w", err)
			}

			// Security: Validate file mode to prevent integer overflow
			fileMode := header.Mode & 0o777 // Only use permission bits (0-777 octal)
			if fileMode > 0o777 {
				fileMode = 0o644 // Default to safe permissions if invalid
			}

			f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, os.FileMode(fileMode)) //nolint:gosec // fileMode is validated above
			if err != nil {
				return fmt.Errorf("creating file: %w", err)
			}

			// Security: bound what one entry may decompress to. One byte past the limit is
			// read so a file at the limit is told apart from one over it: an oversized file
			// fails the extraction, because cutting it off would run a truncated playbook.
			written, err := io.Copy(f, io.LimitReader(tr, extractLimits.perFile+1))
			if closeErr := f.Close(); closeErr != nil {
				logger.Warnf("Failed to close file %s: %v", target, closeErr)
			}
			if err != nil {
				return fmt.Errorf("writing file: %w", err)
			}
			if written > extractLimits.perFile {
				return fmt.Errorf("archive entry %s is larger than the %d byte per-file limit", header.Name, extractLimits.perFile)
			}
			extracted += written
			if extracted > extractLimits.total {
				return fmt.Errorf("archive unpacks to more than the %d byte limit", extractLimits.total)
			}
		}
	}

	return nil
}
