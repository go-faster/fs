package engine

import (
	"context"
	"encoding/json"
	"io"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/meta"
	"github.com/go-faster/fs/internal/sse"
)

// SSE-S3: an encrypted version carries its own data key, sealed by the
// master key ring, and its stored bytes are the chunked AEAD stream of
// internal/sse. Nothing in the metadata or the blocks is plaintext.
//
// A multipart object is not re-encrypted at completion. Each part was sealed
// as it arrived, under the upload's key with its part number in the nonce
// domain, and the object's stored bytes are the parts' streams end to end —
// which is exact to map back, since a stream's length follows from its
// plaintext's. Reading chains one decrypting reader per part.

var _ fs.BucketEncrypter = (*Engine)(nil)

const settingEncryption = "encryption"

// encInfo is how a version or an upload is encrypted. Its data key is not in
// the payload, which is written once, but in the version's Key register, so
// a master key rotation can rewrap it; withKey and loadKey move it.
type encInfo struct {
	Key       sse.WrappedKey `json:"-"`
	NonceBase []byte         `json:"nonce_base"`
}

// withKey sets v's Key register from info, written at ts.
func withKey(v meta.Version, info *encInfo, ts int64) meta.Version {
	if info != nil {
		v.Key = meta.LWW[json.RawMessage]{TS: ts, V: mustJSON(info.Key)}
	}

	return v
}

// loadKey fills info's data key from v's Key register.
func loadKey(v meta.Version, info *encInfo) error {
	if info == nil {
		return nil
	}

	// Stores from before keys were registers carry none: refuse rather than
	// answer as if the object were unreadable for another reason.
	if len(v.Key.V) == 0 {
		return errors.Errorf("encrypted version %s has no data key register; the store predates key rotation and must be rebuilt", v.ID)
	}

	if err := json.Unmarshal(v.Key.V, &info.Key); err != nil {
		return errors.Wrapf(err, "decode data key of %s", v.ID)
	}

	return nil
}

// beginEncryption validates algorithm and mints a data key for a new object
// or upload; nil when algorithm is empty.
func (e *Engine) beginEncryption(algorithm string) (*encInfo, error) {
	if algorithm == "" {
		return nil, nil
	}

	// Only one algorithm exists. Accepting another would acknowledge a
	// request to encrypt with something this server does not have.
	if algorithm != sse.Algorithm {
		return nil, errors.Wrapf(fs.ErrUnsupportedOperation, "unsupported server-side encryption algorithm %q", algorithm)
	}

	if e.keyring == nil {
		return nil, errors.Wrap(fs.ErrUnsupportedOperation, "server-side encryption requested but no master key is configured")
	}

	dek, err := sse.NewKey()
	if err != nil {
		return nil, err
	}

	base, err := sse.NewNonceBase()
	if err != nil {
		return nil, err
	}

	wrapped, err := e.keyring.Wrap(dek)
	if err != nil {
		return nil, err
	}

	return &encInfo{Key: wrapped, NonceBase: base}, nil
}

// cipher returns the cipher of info for one part; 0 for a single PUT.
func (e *Engine) cipher(info *encInfo, part int) (*sse.Cipher, error) {
	if info == nil {
		return nil, nil
	}

	if e.keyring == nil {
		// The object says it is encrypted and there are no keys: the bytes are
		// unreadable, and saying so beats serving ciphertext as content.
		return nil, errors.Wrap(fs.ErrUnsupportedOperation, "object is encrypted but no master key is configured")
	}

	dek, err := e.keyring.Unwrap(info.Key)
	if err != nil {
		return nil, err
	}

	return sse.New(dek, info.NonceBase, uint32(part)) //nolint:gosec // Part numbers are 1..10000.
}

// algorithm is what S3 reports for info.
func algorithm(info *encInfo) string {
	if info == nil {
		return ""
	}

	return sse.Algorithm
}

// sealed returns r's content sealed under c, or r itself when c is nil. The
// sealing runs as the result is read.
func sealed(r io.Reader, c *sse.Cipher) io.ReadCloser {
	if c == nil {
		return io.NopCloser(r)
	}

	pr, pw := io.Pipe()

	go func() {
		enc := sse.NewWriter(pw, c)

		_, err := io.Copy(enc, r)
		if err == nil {
			err = enc.Close()
		}

		pw.CloseWithError(err)
	}()

	return pr
}

// decrypting returns p's plaintext: one decrypting reader per section of the
// stored stream, chained.
func (e *Engine) decrypting(stored io.ReaderAt, closer io.Closer, p payload) (io.ReadSeekCloser, error) {
	sections := []section{{part: 0, plain: p.Size}}

	if len(p.Parts) > 0 {
		sections = sections[:0]
		for _, part := range p.Parts {
			sections = append(sections, section{part: part.PartNumber, plain: part.Size})
		}
	}

	readers := make([]io.ReadSeeker, len(sections))
	sizes := make([]int64, len(sections))

	var off int64

	for i, s := range sections {
		c, err := e.cipher(p.Enc, s.part)
		if err != nil {
			return nil, err
		}

		n := sse.CipherSize(s.plain)
		readers[i] = sse.NewReader(io.NewSectionReader(stored, off, n), c, s.plain)
		sizes[i] = s.plain
		off += n
	}

	return &chain{readers: readers, sizes: sizes, closer: closer}, nil
}

type section struct {
	part  int
	plain int64
}

// chain reads seekable readers one after another as one seekable stream.
type chain struct {
	readers []io.ReadSeeker
	sizes   []int64
	closer  io.Closer
	off     int64
}

func (c *chain) total() int64 {
	var n int64
	for _, s := range c.sizes {
		n += s
	}

	return n
}

func (c *chain) Read(p []byte) (int, error) {
	start := int64(0)

	for i, r := range c.readers {
		if c.off < start+c.sizes[i] {
			if _, err := r.Seek(c.off-start, io.SeekStart); err != nil {
				return 0, err
			}

			n, err := r.Read(p[:min(int64(len(p)), start+c.sizes[i]-c.off)])
			c.off += int64(n)

			if errors.Is(err, io.EOF) && n > 0 {
				err = nil
			}

			return n, err
		}

		start += c.sizes[i]
	}

	return 0, io.EOF
}

func (c *chain) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += c.off
	case io.SeekEnd:
		offset += c.total()
	default:
		return 0, errors.New("invalid whence")
	}

	if offset < 0 {
		return 0, errors.New("negative position")
	}

	c.off = offset

	return offset, nil
}

func (c *chain) Close() error { return c.closer.Close() }

// BucketEncryption implements fs.BucketEncrypter: the algorithm writes to the
// bucket default to, or empty. The S3 layer applies it.
func (e *Engine) BucketEncryption(ctx context.Context, bucket string) (string, error) {
	var alg string

	_, err := e.setting(ctx, bucket, settingEncryption, &alg)

	return alg, err
}

// SetBucketEncryption implements fs.BucketEncrypter; empty clears it.
func (e *Engine) SetBucketEncryption(ctx context.Context, bucket, alg string) error {
	if alg != "" && alg != sse.Algorithm {
		return errors.Wrapf(fs.ErrUnsupportedOperation, "unsupported server-side encryption algorithm %q", alg)
	}

	return e.setSetting(ctx, bucket, settingEncryption, alg)
}
