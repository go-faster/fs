package sse

import (
	"io"

	"github.com/go-faster/errors"
)

// The storagefs path pushes plaintext into a Writer and seeks a Reader over the
// file it produced. The cluster path can do neither: its coordinator *pulls*
// the body through a fragmenter, and what comes back from a read is a stream
// over peers with no random access. So the same chunk format is also exposed
// pull-side, as an io.Reader on each end.

// EncryptingReader turns a plaintext stream into the sealed chunk stream, for
// a consumer that pulls — the cluster coordinator, which reads the body as it
// fragments it.
//
// Encrypting here, before the fragmenter, is what keeps the rest of the data
// plane keyless: shards, parity, repair and peer transfer all carry ciphertext
// and none of them needs to decrypt anything to do its job.
type EncryptingReader struct {
	r io.Reader
	c *Cipher

	plain  []byte
	sealed []byte
	out    []byte // sealed bytes not yet handed to the caller
	chunk  int64
	done   bool
}

// NewEncryptingReader seals r's contents as they are read.
func NewEncryptingReader(r io.Reader, c *Cipher) *EncryptingReader {
	return &EncryptingReader{
		r:      r,
		c:      c,
		plain:  make([]byte, ChunkSize),
		sealed: make([]byte, 0, ChunkSize+TagSize),
	}
}

func (e *EncryptingReader) Read(p []byte) (int, error) {
	for len(e.out) == 0 {
		if e.done {
			return 0, io.EOF
		}

		// ReadFull, so a chunk is only short when the body has ended: a short
		// read in the middle would seal a partial chunk and shift every later
		// offset.
		n, err := io.ReadFull(e.r, e.plain)
		switch {
		case err == nil:
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			e.done = true
		default:
			return 0, errors.Wrap(err, "read body")
		}

		if n == 0 {
			// A body ending exactly on a chunk boundary, or an empty body:
			// either way there is no trailing chunk to seal.
			return 0, io.EOF
		}

		e.sealed = e.c.Seal(e.sealed[:0], e.plain[:n], e.chunk)
		e.out = e.sealed
		e.chunk++
	}

	n := copy(p, e.out)
	e.out = e.out[n:]

	return n, nil
}

// DecryptingReader turns a sealed chunk stream back into plaintext, for a
// consumer that can only read forward — a cluster read, which arrives as a
// stream over peers rather than a file.
//
// A range GET over one of these is served by seeking forward through the
// plaintext, which is what the server already does for any body it cannot seek.
type DecryptingReader struct {
	r    io.Reader
	c    *Cipher
	size int64 // plaintext size, from the sidecar

	sealed []byte
	plain  []byte
	out    []byte
	chunk  int64
	read   int64 // plaintext bytes produced so far
}

// NewDecryptingReader opens a sealed stream carrying plainSize bytes.
//
// plainSize comes from the sidecar rather than from the stream, for the same
// reason it does everywhere else: the per-chunk tags cannot notice a chunk that
// is simply missing from the end.
func NewDecryptingReader(r io.Reader, c *Cipher, plainSize int64) *DecryptingReader {
	return &DecryptingReader{
		r:      r,
		c:      c,
		size:   plainSize,
		sealed: make([]byte, ChunkSize+TagSize),
		plain:  make([]byte, 0, ChunkSize),
	}
}

func (d *DecryptingReader) Read(p []byte) (int, error) {
	if len(d.out) == 0 {
		if d.read >= d.size {
			return 0, io.EOF
		}

		_, plainLen := Locate(d.size, d.chunk)
		if plainLen == 0 {
			return 0, io.EOF
		}

		sealed := d.sealed[:plainLen+TagSize]
		if _, err := io.ReadFull(d.r, sealed); err != nil {
			return 0, errors.Wrapf(err, "read chunk %d", d.chunk)
		}

		out, err := d.c.Open(d.plain[:0], sealed, d.chunk)
		if err != nil {
			return 0, err
		}

		d.plain = out
		d.out = out
		d.chunk++
	}

	n := copy(p, d.out)
	d.out = d.out[n:]
	d.read += int64(n)

	return n, nil
}
