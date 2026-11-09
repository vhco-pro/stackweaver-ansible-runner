// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

//go:build !linux

package syncexec

import (
	"errors"
	"os"
)

// memFile needs Linux memory files; an SSH clone is refused elsewhere rather than writing the key
// to a filesystem.
func memFile(string, []byte) (*os.File, error) {
	return nil, errors.New("an SSH clone runs only on Linux, which holds the key in a memory file")
}
