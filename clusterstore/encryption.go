package clusterstore

import (
	"io"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/sse"
)

// Encryption on the cluster sits above the fragmenter and below nothing else:
// the coordinator seals the body before it is split, so shards, parity,
// repair, rebalance, peer transfer and the scrubber all move ciphertext and
// none of them holds a key. A node repairing a shard does not need to be
// trusted with object content, which is the property that makes this worth
// doing at the coordinator rather than at each disk.

// beginEncryption mints an object's key when the write asks for encryption,
// returning the sidecar record and the cipher that seals the body. Both are
// nil when the write asks for none.
func (c *Coordinator) beginEncryption(algorithm string) (*EncryptionInfo, *sse.Cipher, error) {
	if algorithm == "" {
		return nil, nil, nil
	}

	if algorithm != sse.Algorithm {
		return nil, nil, errors.Wrapf(fs.ErrUnsupportedOperation,
			"unsupported server-side encryption algorithm %q", algorithm)
	}

	if c.keyring == nil {
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

	wrapped, err := c.keyring.Wrap(dek)
	if err != nil {
		return nil, nil, err
	}

	cipher, err := sse.New(dek, base, 0)
	if err != nil {
		return nil, nil, err
	}

	return &EncryptionInfo{
		Algorithm: sse.Algorithm,
		Key:       wrapped,
		NonceBase: base,
	}, cipher, nil
}

// openEncrypted wraps a fragment stream in the reader that decrypts it.
//
// A cluster read arrives as a stream over peers, with no random access, so
// this is forward-only; a range GET is served by seeking forward through the
// plaintext, which is what the server already does for any body it cannot
// seek.
func (c *Coordinator) openEncrypted(rc io.ReadCloser, info *EncryptionInfo) (io.ReadCloser, error) {
	if c.keyring == nil {
		// The object says it is encrypted and this node has no keys. Saying so
		// beats handing the caller ciphertext as content.
		return nil, errors.Wrap(fs.ErrUnsupportedOperation,
			"object is encrypted but no master key is configured")
	}

	dek, err := c.keyring.Unwrap(info.Key)
	if err != nil {
		return nil, err
	}

	cipher, err := sse.New(dek, info.NonceBase, 0)
	if err != nil {
		return nil, err
	}

	return &decryptingStream{
		Reader: sse.NewDecryptingReader(rc, cipher, info.PlainSize),
		closer: rc,
	}, nil
}

// decryptingStream is the decrypted view of a fragment stream, closing the
// stream underneath it.
type decryptingStream struct {
	io.Reader

	closer io.Closer
}

func (d *decryptingStream) Close() error {
	return d.closer.Close() //nolint:wrapcheck // Pass the stream's error through unchanged.
}

// encryptionAlgorithm reports the algorithm an object is stored under, empty
// when it is stored in the clear.
func encryptionAlgorithm(sc *Sidecar) string {
	if sc.Encryption == nil {
		return ""
	}

	return sc.Encryption.Algorithm
}
