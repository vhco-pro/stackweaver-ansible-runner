// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package executor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// SecretDir is the job-private directory every credential the executor materialises goes into
// (#933, D2 "Secrets on disk", AC25): the inventory with injected credentials, vault password and
// key files, cloud identity tokens and configuration, tfvars, the CLI configuration, the CA bundle
// and the saved plan. Files are 0600 in a 0700 directory, never in a cache or the job's working
// tree, and Shred overwrites and removes all of them on every exit path, so a container kept for
// inspection under keep_failed_jobs_for holds logs and the working tree but no credential.
type SecretDir struct {
	dir  string
	once sync.Once
	err  error
}

// NewSecretDir creates a fresh secret directory under root (the system temp directory when empty).
func NewSecretDir(root string) (*SecretDir, error) {
	dir, err := os.MkdirTemp(root, "stackweaver-secrets-")
	if err != nil {
		return nil, fmt.Errorf("creating the job's secret directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its search bit; 0700 is owner-only
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("protecting the job's secret directory: %w", err)
	}
	return &SecretDir{dir: dir}, nil
}

// Dir is the directory's path, for helpers that write into a directory they are given.
func (s *SecretDir) Dir() string { return s.dir }

// Path is where name lives in the directory.
func (s *SecretDir) Path(name string) string { return filepath.Join(s.dir, name) }

// Write writes data to name, 0600, and returns its path.
func (s *SecretDir) Write(name string, data []byte) (string, error) {
	p := s.Path(name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return "", fmt.Errorf("writing %s: %w", name, err)
	}
	return p, nil
}

// Shred overwrites every regular file in the directory with zeros, syncs it, and removes the
// directory. It is safe to call more than once and from a deferred call during a panic; only the
// first call does the work.
func (s *SecretDir) Shred() error {
	s.once.Do(func() {
		var errs []error
		_ = filepath.WalkDir(s.dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			errs = append(errs, overwrite(path))
			return nil
		})
		errs = append(errs, os.RemoveAll(s.dir))
		s.err = errors.Join(errs...)
	})
	return s.err
}

func overwrite(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0) //nolint:gosec // G304: a file of the job's own secret directory
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	zeros := make([]byte, 32<<10)
	for left := info.Size(); left > 0; {
		n := min(left, int64(len(zeros)))
		if _, err := f.Write(zeros[:n]); err != nil {
			return err
		}
		left -= n
	}
	return f.Sync()
}
