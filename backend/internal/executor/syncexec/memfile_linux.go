// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

//go:build linux

package syncexec

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// memFile is an anonymous memory-backed file holding data, mode 0600, with no path on any
// filesystem: ssh reads it as /dev/fd/N of the clone, which inherits it (decision 18, AC68).
func memFile(name string, data []byte) (*os.File, error) {
	fd, err := unix.MemfdCreate(name, 0)
	if err != nil {
		return nil, fmt.Errorf("creating a memory file for %s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("restricting the memory file for %s: %w", name, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("writing the memory file for %s: %w", name, err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
