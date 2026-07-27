package sse

import (
	"io"

	"github.com/go-faster/errors"
)

// Writer encrypts a body as it is written, one chunk at a time.
//
// It buffers up to ChunkSize of plaintext, because a chunk cannot be sealed
// until it is known to be full or final — that is the whole cost of the
// format, and it is one buffer regardless of object size, which is what keeps
// the write path constant-memory.
//
// Close seals the trailing partial chunk. It must be called, and its error
// must be checked: without it the last chunk is not written at all.
type Writer struct {
	w      io.Writer
	c      *Cipher
	buf    []byte
	n      int   // plaintext bytes held in buf
	chunk  int64 // index of the chunk buf is accumulating
	out    []byte
	closed bool
}

// NewWriter returns a Writer sealing into w.
func NewWriter(w io.Writer, c *Cipher) *Writer {
	return &Writer{
		w:   w,
		c:   c,
		buf: make([]byte, ChunkSize),
		out: make([]byte, 0, ChunkSize+TagSize),
	}
}

func (w *Writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errors.New("write after close")
	}

	written := 0

	for len(p) > 0 {
		n := copy(w.buf[w.n:], p)
		w.n += n
		p = p[n:]
		written += n

		// Only flush a full chunk: a short one here would be sealed as if it
		// were final, and every later offset would be wrong.
		if w.n == ChunkSize {
			if err := w.flush(); err != nil {
				return written, err
			}
		}
	}

	return written, nil
}

// flush seals whatever is buffered as the current chunk.
func (w *Writer) flush() error {
	w.out = w.c.Seal(w.out[:0], w.buf[:w.n], w.chunk)

	if _, err := w.w.Write(w.out); err != nil {
		return errors.Wrap(err, "write chunk")
	}

	w.n = 0
	w.chunk++

	return nil
}

// Close seals the trailing partial chunk, if any. It does not close the
// underlying writer, which the caller owns.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}

	w.closed = true

	// A zero-length body is zero chunks: sealing here would write a tag for a
	// chunk that does not exist and give the object a non-zero stored size.
	if w.n == 0 {
		return nil
	}

	return w.flush()
}

// Reader decrypts a body, supporting seeks expressed in *plaintext* offsets.
//
// It reads from an io.ReaderAt rather than a stream because that is what makes
// a range GET cheap: seeking to an offset reads only the chunk covering it.
type Reader struct {
	r    io.ReaderAt
	c    *Cipher
	size int64 // plaintext size, from the sidecar

	pos int64 // plaintext read offset

	// chunk holds the decrypted chunk at index held, so sequential reads and
	// short reads within a chunk do not decrypt it again.
	chunk []byte
	held  int64
	valid bool

	sealed []byte
}

// NewReader returns a Reader over ciphertext, serving plainSize bytes.
//
// plainSize comes from the sidecar, not from the length of r: taking it from
// the file would make a truncated object look like a shorter one that opens
// cleanly, since the per-chunk tags cannot see a chunk that is simply absent.
func NewReader(r io.ReaderAt, c *Cipher, plainSize int64) *Reader {
	return &Reader{
		r:      r,
		c:      c,
		size:   plainSize,
		chunk:  make([]byte, 0, ChunkSize),
		sealed: make([]byte, ChunkSize+TagSize),
		held:   -1,
	}
}

// Size returns the plaintext size.
func (r *Reader) Size() int64 { return r.size }

func (r *Reader) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}

	idx := r.pos / ChunkSize
	if !r.valid || r.held != idx {
		if err := r.load(idx); err != nil {
			return 0, err
		}
	}

	n := copy(p, r.chunk[r.pos-idx*ChunkSize:])
	r.pos += int64(n)

	return n, nil
}

// load decrypts chunk idx into r.chunk.
func (r *Reader) load(idx int64) error {
	offset, plainLen := Locate(r.size, idx)
	if plainLen == 0 {
		return io.EOF
	}

	sealed := r.sealed[:plainLen+TagSize]

	// ReadFull, not Read: a short read here is a truncated object, and letting
	// it through would surface as an authentication failure that blames the
	// wrong thing.
	if _, err := io.ReadFull(newSectionReader(r.r, offset), sealed); err != nil {
		return errors.Wrapf(err, "read chunk %d", idx)
	}

	out, err := r.c.Open(r.chunk[:0], sealed, idx)
	if err != nil {
		return err
	}

	r.chunk = out
	r.held = idx
	r.valid = true

	return nil
}

func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	var abs int64

	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.pos + offset
	case io.SeekEnd:
		abs = r.size + offset
	default:
		return 0, errors.Errorf("invalid whence %d", whence)
	}

	if abs < 0 {
		return 0, errors.New("seek before start of object")
	}

	r.pos = abs

	return abs, nil
}

// sectionReader adapts an io.ReaderAt to the io.Reader io.ReadFull wants,
// without allocating an io.SectionReader per chunk.
type sectionReader struct {
	r   io.ReaderAt
	off int64
}

func newSectionReader(r io.ReaderAt, off int64) *sectionReader {
	return &sectionReader{r: r, off: off}
}

func (s *sectionReader) Read(p []byte) (int, error) {
	n, err := s.r.ReadAt(p, s.off)
	s.off += int64(n)

	return n, err //nolint:wrapcheck // Pass the reader's error through unchanged.
}
