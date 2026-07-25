package clusterstore

import (
	"bytes"
	"io"
	"os"

	"github.com/go-faster/errors"
)

// spoolThreshold is how much of a length-less body is held in memory before
// spilling to a temp file. Small enough that many concurrent uploads cannot
// exhaust the node, large enough that the common case — an SDK streaming a
// modest body it has not measured — never touches the disk.
const spoolThreshold = 1 << 20 // 1 MiB

// spool materializes a body whose length is not known in advance, returning a
// reader positioned at the start and the exact size.
//
// The coordinator has to place fragments before it can write any, and placement
// needs the object size (fragment.Plan refuses a negative one). A request with
// Transfer-Encoding: chunked and no X-Amz-Decoded-Content-Length carries no
// length at all — http.Request.ContentLength is -1 — so the only way to serve
// it is to draw the body first and measure it.
//
// Small bodies stay in memory; anything past spoolThreshold spills to a temp
// file, which is unlinked as soon as it is created so a crash cannot leak it.
// cleanup is never nil.
func spool(r io.Reader) (body io.Reader, size int64, cleanup func(), err error) {
	buf := make([]byte, spoolThreshold)

	n, err := io.ReadFull(r, buf)
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		// The whole body fit; no temp file needed.
		return bytes.NewReader(buf[:n]), int64(n), func() {}, nil
	case err != nil:
		return nil, 0, func() {}, errors.Wrap(err, "read body")
	}

	f, err := os.CreateTemp("", "fs-spool-*")
	if err != nil {
		return nil, 0, func() {}, errors.Wrap(err, "create spool file")
	}

	cleanup = func() { _ = f.Close() }

	// Unlink now: the open descriptor keeps the data reachable, and nothing is
	// left behind if this process dies mid-upload.
	if err := os.Remove(f.Name()); err != nil {
		cleanup()
		return nil, 0, func() {}, errors.Wrap(err, "unlink spool file")
	}

	if _, err := f.Write(buf); err != nil {
		cleanup()
		return nil, 0, func() {}, errors.Wrap(err, "write spool file")
	}

	rest, err := io.Copy(f, r)
	if err != nil {
		cleanup()
		return nil, 0, func() {}, errors.Wrap(err, "spool body")
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, 0, func() {}, errors.Wrap(err, "rewind spool file")
	}

	return f, int64(n) + rest, cleanup, nil
}
