// Package sse implements the chunked AEAD stream that backs SSE-S3:
// server-side encryption with a server-managed key.
//
// # Why chunks
//
// Encrypting a body as one AEAD message would be simpler and is wrong here for
// two reasons. A range GET would have to decrypt from byte zero to reach the
// requested offset, which breaks the constant-memory, seek-to-offset read path
// the rest of the server is built on; and the whole object would have to be
// buffered to verify its tag before any byte could be served, which breaks
// streaming. So the body is a sequence of independently sealed chunks of a
// fixed *plaintext* size, which makes the mapping between a plaintext offset
// and its ciphertext offset pure arithmetic — see Locate.
//
// # Format
//
// The ciphertext is the bare concatenation of sealed chunks, with no header:
// everything a reader needs (the wrapped key, the nonce base, the plaintext
// size) lives in the object's metadata sidecar, where it is already being
// written and replicated. A header would duplicate that and add a second
// source of truth about the size.
//
//	chunk i plaintext:  [i*ChunkSize, min((i+1)*ChunkSize, size))
//	chunk i ciphertext: at i*(ChunkSize+TagSize), of length len(plaintext)+TagSize
//
// A zero-length object is zero chunks and zero ciphertext bytes.
//
// # Nonces
//
// AES-GCM fails catastrophically if a nonce is ever reused with the same key,
// so nonces are derived rather than drawn: a random 96-bit base per object,
// XORed with a counter that is unique per (part, chunk). The base makes two
// objects collide only with birthday probability over 2^96, and the counter
// makes collision within one object impossible rather than improbable.
//
// Deriving the nonce from the chunk index also authenticates position for
// free: a chunk moved to a different index is sealed under a different nonce
// and fails to open. Truncation of trailing chunks is *not* caught by the tags
// — it is caught by the plaintext size in the sidecar, which is why a reader
// must be given that size rather than inferring it from the file length alone.
package sse

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"io"

	"github.com/go-faster/errors"
)

const (
	// ChunkSize is the plaintext size of every chunk but the last. It is fixed
	// forever: it is the unit the offset arithmetic assumes, so changing it
	// would make every previously written object unreadable.
	//
	// 64 KiB trades tag overhead (0.02%) against how much a one-byte range GET
	// has to decrypt.
	ChunkSize = 64 << 10

	// TagSize is the AES-GCM authentication tag appended to every chunk.
	TagSize = 16

	// NonceSize is the AES-GCM nonce width, and the width of the per-object
	// random base.
	NonceSize = 12

	// KeySize is the width of both the data key and the master key: AES-256.
	KeySize = 32
)

// Algorithm is the only server-side encryption algorithm this server accepts,
// and the value S3 uses for it on the wire.
const Algorithm = "AES256"

// storedChunk is the on-disk size of a full chunk.
const storedChunk = ChunkSize + TagSize

// NewKey returns a fresh random 256-bit key, for use as a data key or a master
// key.
func NewKey() ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, errors.Wrap(err, "generate key")
	}

	return key, nil
}

// NewNonceBase returns the random per-object nonce base.
func NewNonceBase() ([]byte, error) {
	base := make([]byte, NonceSize)
	if _, err := rand.Read(base); err != nil {
		return nil, errors.Wrap(err, "generate nonce base")
	}

	return base, nil
}

// CipherSize returns the stored size of a body of plainSize bytes.
func CipherSize(plainSize int64) int64 {
	if plainSize <= 0 {
		return 0
	}

	return plainSize + TagSize*chunkCount(plainSize)
}

// PlainSize returns the logical size of a stored body of cipherSize bytes, and
// whether cipherSize is a length the format could have produced at all.
//
// It exists for the scrubber and for repair, which see a file without its
// sidecar. The read path takes the plaintext size from the sidecar instead,
// because that is what makes a truncated object detectable.
func PlainSize(cipherSize int64) (int64, bool) {
	switch {
	case cipherSize == 0:
		return 0, true
	case cipherSize < 0:
		return 0, false
	}

	full, rem := cipherSize/storedChunk, cipherSize%storedChunk
	if rem == 0 {
		return full * ChunkSize, true
	}

	// A trailing chunk is a tag plus at least one byte of content; anything
	// shorter is not a body this format wrote.
	if rem <= TagSize {
		return 0, false
	}

	return full*ChunkSize + rem - TagSize, true
}

// chunkCount is the number of chunks a body of n plaintext bytes occupies.
func chunkCount(n int64) int64 {
	if n <= 0 {
		return 0
	}

	return (n + ChunkSize - 1) / ChunkSize
}

// Locate reports where chunk i of a body of plainSize bytes lives: its offset
// in the ciphertext, and how many plaintext bytes it carries.
func Locate(plainSize, i int64) (cipherOffset, plainLen int64) {
	cipherOffset = i * storedChunk

	plainLen = plainSize - i*ChunkSize
	if plainLen > ChunkSize {
		plainLen = ChunkSize
	}

	if plainLen < 0 {
		plainLen = 0
	}

	return cipherOffset, plainLen
}

// Cipher seals and opens the chunks of one object body.
//
// Part is the multipart part number the body belongs to, and 0 for a single
// PUT. It joins the chunk index in the nonce counter so that parts uploaded
// under one object key — which share a data key — never collide, and so that
// completing a multipart upload can concatenate the parts untouched.
type Cipher struct {
	aead  cipher.AEAD
	base  [NonceSize]byte
	part  uint32
	nonce [NonceSize]byte
	aad   [8]byte
}

// New builds a Cipher for one body from its data key and nonce base.
func New(key, nonceBase []byte, part uint32) (*Cipher, error) {
	if len(key) != KeySize {
		return nil, errors.Errorf("data key must be %d bytes, got %d", KeySize, len(key))
	}

	if len(nonceBase) != NonceSize {
		return nil, errors.Errorf("nonce base must be %d bytes, got %d", NonceSize, len(nonceBase))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.Wrap(err, "new cipher")
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.Wrap(err, "new gcm")
	}

	c := &Cipher{aead: aead, part: part}
	copy(c.base[:], nonceBase)

	return c, nil
}

// counter is the value distinguishing one chunk from every other chunk sealed
// under the same key: the part number in the high half, the chunk index in the
// low half.
func (c *Cipher) counter(chunk int64) uint64 {
	return uint64(c.part)<<32 | uint64(uint32(chunk)) //nolint:gosec // Chunk index is bounded by the object size.
}

// nonceFor derives chunk's nonce by XORing the counter into the tail of the
// per-object base.
func (c *Cipher) nonceFor(chunk int64) []byte {
	c.nonce = c.base
	binary.BigEndian.PutUint64(
		c.nonce[NonceSize-8:],
		binary.BigEndian.Uint64(c.base[NonceSize-8:])^c.counter(chunk),
	)

	return c.nonce[:]
}

// aadFor binds the chunk's position into the tag explicitly, so the intent is
// visible at the call site rather than implied by the nonce derivation.
func (c *Cipher) aadFor(chunk int64) []byte {
	binary.BigEndian.PutUint64(c.aad[:], c.counter(chunk))
	return c.aad[:]
}

// Seal encrypts one chunk of plaintext, appending to dst.
func (c *Cipher) Seal(dst, plaintext []byte, chunk int64) []byte {
	return c.aead.Seal(dst, c.nonceFor(chunk), plaintext, c.aadFor(chunk))
}

// Open decrypts one sealed chunk, appending to dst.
func (c *Cipher) Open(dst, sealed []byte, chunk int64) ([]byte, error) {
	out, err := c.aead.Open(dst, c.nonceFor(chunk), sealed, c.aadFor(chunk))
	if err != nil {
		// The tag says the bytes are not what was written. Which of the many
		// reasons applies — bit rot, a truncated chunk, a swapped chunk, the
		// wrong key — is not knowable from here, and saying so would be
		// guessing.
		return nil, errors.Wrap(err, "chunk failed authentication")
	}

	return out, nil
}

// Overhead is the number of bytes a sealed chunk adds to its plaintext.
func (c *Cipher) Overhead() int { return c.aead.Overhead() }

var (
	_ io.WriteCloser = (*Writer)(nil)
	_ io.ReadSeeker  = (*Reader)(nil)
)
