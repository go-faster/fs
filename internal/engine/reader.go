package engine

import (
	"bytes"
	"context"
	"io"

	"github.com/go-faster/errors"
)

// reader returns the version's content as an io.ReadSeekCloser, so the
// handler serves ranges by seeking rather than reading what it skips.
func (e *Engine) reader(ctx context.Context, p payload) (io.ReadSeekCloser, error) {
	if p.Blocks == nil {
		inline := bytes.NewReader(p.Inline)
		if p.Enc == nil {
			return nopCloser{inline}, nil
		}

		return e.decrypting(inline, nopCloser{inline}, p)
	}

	stored := &blockReader{ctx: ctx, e: e, blocks: p.Blocks, size: storedSize(p.Blocks)}
	if p.Enc == nil {
		return stored, nil
	}

	return e.decrypting(stored, stored, p)
}

func storedSize(blocks []blockLoc) int64 {
	var n int64
	for _, b := range blocks {
		n += b.Size
	}

	return n
}

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }

// blockReader reads a version's blocks in order, fetching one at a time and
// only the ones a read reaches.
type blockReader struct {
	ctx    context.Context //nolint:containedctx // The reader outlives the call that made it, like an http.Response body.
	e      *Engine
	blocks []blockLoc
	size   int64
	off    int64

	// cur is the block holding the last read, starting at curOff.
	cur    []byte
	curOff int64
	curIdx int
}

func (r *blockReader) Read(p []byte) (int, error) {
	if r.off >= r.size {
		return 0, io.EOF
	}

	if r.cur == nil || r.off < r.curOff || r.off >= r.curOff+int64(len(r.cur)) {
		if err := r.load(); err != nil {
			return 0, err
		}
	}

	n := copy(p, r.cur[r.off-r.curOff:])
	r.off += int64(n)

	return n, nil
}

// load fetches the block holding r.off.
func (r *blockReader) load() error {
	var start int64

	for i, b := range r.blocks {
		if r.off < start+b.Size {
			data, err := r.e.blocks.Get(r.ctx, b.Hash)
			if err != nil {
				return errors.Wrapf(err, "read block %d", i)
			}

			if int64(len(data)) != b.Size {
				return errors.Errorf("block %d is %d bytes, the version says %d", i, len(data), b.Size)
			}

			r.cur, r.curOff, r.curIdx = data, start, i

			return nil
		}

		start += b.Size
	}

	return io.EOF
}

// ReadAt reads the stored bytes at off, crossing blocks as it goes. It shares
// the one-block cache with Read, so a sequential pass fetches each block once.
func (r *blockReader) ReadAt(p []byte, off int64) (int, error) {
	n := 0

	for n < len(p) {
		if off >= r.size {
			return n, io.EOF
		}

		if r.cur == nil || off < r.curOff || off >= r.curOff+int64(len(r.cur)) {
			saved := r.off
			r.off = off

			err := r.load()
			r.off = saved

			if err != nil {
				return n, err
			}
		}

		c := copy(p[n:], r.cur[off-r.curOff:])
		n += c
		off += int64(c)
	}

	return n, nil
}

func (r *blockReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64

	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.off + offset
	case io.SeekEnd:
		abs = r.size + offset
	default:
		return 0, errors.New("invalid whence")
	}

	if abs < 0 {
		return 0, errors.New("negative position")
	}

	r.off = abs

	return abs, nil
}

func (r *blockReader) Close() error {
	r.cur = nil

	return nil
}
