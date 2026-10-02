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

// blockReader reads a version's blocks, fetching only the ones a read
// reaches, and reading ahead: once block i is read, the next few are fetched
// and verified concurrently, so a sequential read is not one hash wide.
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

	// ahead holds fetches started for the blocks after curIdx, by index.
	ahead map[int]chan fetched
}

// readAhead is how many blocks past the current one are fetched early.
const readAhead = 8

type fetched struct {
	data []byte
	err  error
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

// load fetches the block holding r.off, and starts fetching the ones after.
func (r *blockReader) load() error {
	var start int64

	for i, b := range r.blocks {
		if r.off < start+b.Size {
			res := <-r.take(i)
			if res.err != nil {
				return errors.Wrapf(res.err, "read block %d", i)
			}

			if int64(len(res.data)) != b.Size {
				return errors.Errorf("block %d is %d bytes, the version says %d", i, len(res.data), b.Size)
			}

			r.cur, r.curOff, r.curIdx = res.data, start, i

			// Drop fetches the reader has moved past or away from; start the
			// ones ahead of it.
			for k := range r.ahead {
				if k <= i || k > i+readAhead {
					delete(r.ahead, k)
				}
			}

			for k := i + 1; k <= i+readAhead && k < len(r.blocks); k++ {
				if _, ok := r.ahead[k]; !ok {
					r.ahead[k] = r.start(k)
				}
			}

			return nil
		}

		start += b.Size
	}

	return io.EOF
}

// take returns block i's fetch: the one started ahead, or a new one.
func (r *blockReader) take(i int) <-chan fetched {
	if ch, ok := r.ahead[i]; ok {
		delete(r.ahead, i)

		return ch
	}

	return r.start(i)
}

// start fetches block i in the background. The channel is buffered, so a
// fetch nobody takes still finishes and is collected.
func (r *blockReader) start(i int) chan fetched {
	if r.ahead == nil {
		r.ahead = map[int]chan fetched{}
	}

	ch := make(chan fetched, 1)

	go func() {
		data, err := r.e.blocks.Get(r.ctx, r.blocks[i].Hash)
		ch <- fetched{data, err}
	}()

	return ch
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
	r.cur, r.ahead = nil, nil

	return nil
}
