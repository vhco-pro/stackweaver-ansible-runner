// Copyright (c) 2026 VH & Co BV. Licensed under the Business Source License 1.1. See LICENSE for details.

package driver

import (
	"encoding/binary"
	"errors"
	"io"
)

// DemuxLogs copies an engine's multiplexed log stream (an 8-byte header per frame: the stream, three
// zero bytes, the frame length) to w, stdout and stderr interleaved as they were written.
func DemuxLogs(r io.Reader, w io.Writer) error {
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		n := binary.BigEndian.Uint32(hdr[4:])
		if _, err := io.CopyN(w, r, int64(n)); err != nil {
			return err
		}
	}
}
