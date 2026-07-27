package storagefs

import (
	"crypto/md5" //nolint:gosec // Bit-rot detection, not a security boundary.
	"encoding/hex"
	"hash"
	"io"
	"os"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/sse"
)

// WithEncryption gives the store the keyring it needs to encrypt objects at
// rest. Without one, a write asking for encryption is refused rather than
// stored in the clear.
//
// Objects already written in the clear stay readable: whether a body is
// encrypted is recorded per object in its sidecar, not per store, so turning
// encryption on affects new writes only and turning it off does not strand
// what is already there — as long as the keyring still holds the master key
// those objects name.
func WithEncryption(kr *sse.Keyring) Option {
	return func(s *Storage) { s.keyring = kr }
}

// encryptionInfo is the sidecar record of how an object body is encrypted.
// Absent means the body is stored in the clear.
type encryptionInfo struct {
	// Algorithm is the S3 name reported to clients; only "AES256" exists.
	Algorithm string `json:"algorithm"`
	// Key is the object's data key, sealed by a master key it names.
	Key sse.WrappedKey `json:"key"`
	// NonceBase is the per-object random the chunk nonces derive from.
	NonceBase []byte `json:"nonce_base"`
	// PlainSize is the object's logical size. It is recorded rather than
	// derived from the file so that a truncated body is detectable: per-chunk
	// tags authenticate the chunks that are present and can say nothing about
	// chunks that are simply gone.
	PlainSize int64 `json:"plain_size"`
	// CipherChecksum is the MD5 of the bytes as stored. The scrubber and
	// verify-on-read compare against this rather than the plaintext checksum,
	// so bit-rot detection needs no key at all — and the AEAD tag already
	// catches on read anything this would have caught, per chunk, whether or
	// not verification is enabled.
	CipherChecksum string `json:"cipher_checksum"`
}

// encrypting reports whether the store can encrypt.
func (s *Storage) encrypting() bool { return s.keyring != nil }

// beginEncryption mints an object's data key and returns the cipher that seals
// its body together with the sidecar record needed to open it again.
//
// part is the multipart part number, 0 for a single PUT; it separates the
// nonce space of parts that share one data key.
func (s *Storage) beginEncryption(part uint32) (*sse.Cipher, *encryptionInfo, error) {
	if !s.encrypting() {
		return nil, nil, errors.Wrap(fs.ErrUnsupportedOperation,
			"server-side encryption requested but no master key is configured")
	}

	dek, err := sse.NewKey()
	if err != nil {
		return nil, nil, err
	}

	base, err := sse.NewNonceBase()
	if err != nil {
		return nil, nil, err
	}

	wrapped, err := s.keyring.Wrap(dek)
	if err != nil {
		return nil, nil, err
	}

	c, err := sse.New(dek, base, part)
	if err != nil {
		return nil, nil, err
	}

	return c, &encryptionInfo{
		Algorithm: sse.Algorithm,
		Key:       wrapped,
		NonceBase: base,
	}, nil
}

// cipherFor rebuilds the cipher that opens an object's body.
func (s *Storage) cipherFor(info *encryptionInfo, part uint32) (*sse.Cipher, error) {
	if !s.encrypting() {
		// The object says it is encrypted and the store has no keys, so the
		// bytes are unreadable. Saying so beats serving ciphertext as content.
		return nil, errors.Wrap(fs.ErrUnsupportedOperation,
			"object is encrypted but no master key is configured")
	}

	dek, err := s.keyring.Unwrap(info.Key)
	if err != nil {
		return nil, err
	}

	c, err := sse.New(dek, info.NonceBase, part)
	if err != nil {
		return nil, err
	}

	return c, nil
}

// objectWriter is the plaintext sink an object body is copied into, plus
// whatever has to be recorded about it once the copy finishes.
//
// It exists so the write path reads the same whether or not the store
// encrypts: PutObject copies into it, closes it, and asks it for the sidecar
// record, and the unencrypted case is the one where all three do nothing.
type objectWriter struct {
	w io.Writer

	enc        *sse.Writer
	info       *encryptionInfo
	cipherHash hash.Hash
}

func (o *objectWriter) Write(p []byte) (int, error) {
	return o.w.Write(p) //nolint:wrapcheck // Pass the sink's error through unchanged.
}

// Close seals the trailing chunk. It does not close the file underneath, which
// PutObject owns.
func (o *objectWriter) Close() error {
	if o.enc == nil {
		return nil
	}

	return o.enc.Close() //nolint:wrapcheck // Already annotated by the writer.
}

// finish returns the sidecar record for the body just written, or nil when it
// was written in the clear.
func (o *objectWriter) finish(plainSize int64) *encryptionInfo {
	if o.info == nil {
		return nil
	}

	o.info.PlainSize = plainSize
	o.info.CipherChecksum = hex.EncodeToString(o.cipherHash.Sum(nil))

	return o.info
}

// encryptTo returns the sink a body should be written to: dst itself when the
// request asks for no encryption, or a sealing stream over dst when it does.
//
// part is the multipart part number, 0 for a single PUT.
func (s *Storage) encryptTo(dst io.Writer, algorithm string, part uint32) (*objectWriter, error) {
	if algorithm == "" {
		return &objectWriter{w: dst}, nil
	}

	// Only one algorithm exists. Refusing an unknown one matters more than it
	// looks: accepting it would acknowledge a request to encrypt with
	// something this server does not have, and store the body in the clear.
	if algorithm != sse.Algorithm {
		return nil, errors.Wrapf(fs.ErrUnsupportedOperation,
			"unsupported server-side encryption algorithm %q", algorithm)
	}

	c, info, err := s.beginEncryption(part)
	if err != nil {
		return nil, err
	}

	// The stored bytes are hashed as they are produced, so the scrubber has
	// something to check without ever holding a key.
	cipherHash := md5.New() //nolint:gosec // Bit-rot detection, not a security boundary.
	enc := sse.NewWriter(io.MultiWriter(dst, cipherHash), c)

	return &objectWriter{w: enc, enc: enc, info: info, cipherHash: cipherHash}, nil
}

// decryptingFile serves an encrypted body as the seekable stream the read path
// expects, and closes the file underneath it.
//
// http.ServeContent asserts io.ReadSeeker on what GetObject returns and seeks
// it to satisfy a Range request, so the seek has to land in plaintext
// coordinates. That is what the chunked format buys, and this is where the two
// meet.
type decryptingFile struct {
	*sse.Reader

	f *os.File
}

func (d *decryptingFile) Close() error {
	return d.f.Close() //nolint:wrapcheck // Pass the file's error through unchanged.
}

// openEncrypted wraps an open object file in its decrypting reader.
func (s *Storage) openEncrypted(f *os.File, info *encryptionInfo, part uint32) (io.ReadCloser, error) {
	c, err := s.cipherFor(info, part)
	if err != nil {
		return nil, err
	}

	return &decryptingFile{Reader: sse.NewReader(f, c, info.PlainSize), f: f}, nil
}
