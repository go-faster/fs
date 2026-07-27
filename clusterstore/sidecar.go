package clusterstore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/sse"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/scheme"
)

// sidecarVersion stamps the sidecar format; bump on incompatible changes and
// keep readers tolerant of older versions.
const sidecarVersion = 1

// Sidecar is the per-object commit record, replicated to the object's
// placement targets alongside its fragments. It is what makes a generation
// visible: a fragment set without a committed sidecar does not exist, and the
// sidecar carries everything the read path needs to re-plan the fragments —
// the scheme, the exact size and the generation stamp — plus the S3-level
// metadata the fs.Storage layer serves without touching payload bytes.
type Sidecar struct {
	Version int    `json:"version"`
	Bucket  string `json:"bucket"`
	Key     string `json:"key"`

	// Scheme is the replication scheme the object was written with, in its
	// config form ("rf2.5", "rf3", "ec:k,m"). Recording it makes reads immune
	// to later per-bucket scheme changes.
	Scheme string `json:"scheme"`
	// Size is the exact length of the bytes as *stored*, needed to re-plan
	// fragment sizes and to unpad EC reconstructions. For an encrypted object
	// that is the ciphertext length, not the object's logical size — which is
	// deliberate: every fragment, parity and repair calculation works on what
	// is actually on disk, so none of them needs a key. The logical size lives
	// in Encryption.PlainSize.
	Size int64 `json:"size"`
	// Encryption records how the stored bytes are encrypted, absent when they
	// are stored in the clear.
	Encryption *EncryptionInfo `json:"encryption,omitempty"`
	// RequestedEncryption is the algorithm a multipart upload asked for when
	// it was created, carried on the upload's own record so that each part and
	// the completed object are sealed the same way. It is only ever set on an
	// upload record, which has no body of its own to encrypt.
	RequestedEncryption string `json:"requested_encryption,omitempty"`
	// Generation names the fragment set this sidecar commits.
	Generation string `json:"generation"`
	// Seq is a per-object write sequence (previous committed Seq + 1): the
	// primary "newest wins" ordering for list-merge and repair
	// reconciliation. Wall clocks are too coarse to order two writes of the
	// same key (observed on Windows), so time is only the cross-writer
	// tie-break.
	Seq int64 `json:"seq,omitempty"`
	// Modified is the write time; orders records with equal Seq (concurrent
	// writers that read the same previous state), with the generation string
	// as the final deterministic tie-break.
	Modified time.Time `json:"modified"`

	ETag string `json:"etag,omitempty"`
	// Checksum is the hex MD5 of the full object content (scrubber and
	// verify-on-read input; equal to ETag for single-part writes).
	Checksum string `json:"checksum,omitempty"`

	ContentType        string            `json:"content_type,omitempty"`
	CacheControl       string            `json:"cache_control,omitempty"`
	ContentDisposition string            `json:"content_disposition,omitempty"`
	ContentEncoding    string            `json:"content_encoding,omitempty"`
	Expires            string            `json:"expires,omitempty"`
	UserMetadata       map[string]string `json:"user_metadata,omitempty"`
	Tags               []fs.Tag          `json:"tags,omitempty"`
	ACL                fs.ACL            `json:"acl,omitempty"`
	// Owner is the principal that wrote the object; absent in sidecars written
	// before owners were modeled.
	Owner fs.Owner `json:"owner,omitzero"`
	// Parts is the part layout a multipart object was assembled from. Absent
	// for single PUTs and for multipart objects written before the layout was
	// recorded, both of which read as a single part.
	Parts []fs.ObjectPart `json:"parts,omitempty"`
	// UploadID names the multipart upload that produced the object, so a
	// retried completion can be told from a stale one. Empty for a single PUT.
	UploadID string `json:"upload_id,omitempty"`
}

// ObjectMetadata converts the sidecar's header fields to the domain type.
func (sc *Sidecar) ObjectMetadata() fs.ObjectMetadata {
	return fs.ObjectMetadata{
		ContentType:        sc.ContentType,
		CacheControl:       sc.CacheControl,
		ContentDisposition: sc.ContentDisposition,
		ContentEncoding:    sc.ContentEncoding,
		Expires:            sc.Expires,
		UserMetadata:       sc.UserMetadata,
	}
}

// ParseScheme returns the scheme the object was written with.
func (sc *Sidecar) ParseScheme() (scheme.Scheme, error) {
	return scheme.Parse(sc.Scheme)
}

// Supersedes reports whether this record is newer than other: Seq first,
// then Modified, then Generation — a total, deterministic order.
func (sc *Sidecar) Supersedes(other *Sidecar) bool {
	if sc.Seq != other.Seq {
		return sc.Seq > other.Seq
	}

	if !sc.Modified.Equal(other.Modified) {
		return sc.Modified.After(other.Modified)
	}

	return sc.Generation > other.Generation
}

// encode marshals the sidecar for storage.
func (sc *Sidecar) encode() ([]byte, error) {
	data, err := json.Marshal(sc)
	if err != nil {
		return nil, errors.Wrap(err, "marshal sidecar")
	}

	return data, nil
}

// decodeSidecar parses a stored sidecar. Unlike the single-node store a
// corrupt sidecar is an error here, not "absent with defaults": without it the
// generation and scheme are unknown, so the fragments are unreachable anyway —
// better a loud failure (and a repair from another target) than a phantom
// missing object.
func decodeSidecar(data []byte) (*Sidecar, error) {
	var sc Sidecar
	if err := json.Unmarshal(data, &sc); err != nil {
		return nil, errors.Wrap(err, "unmarshal sidecar")
	}

	return &sc, nil
}

// bucketObjectsPrefix is the store-name prefix holding every object of a
// bucket. The bucket gets its own hashed namespace segment so a per-bucket
// listing is a single prefix scan.
func bucketObjectsPrefix(bucket string) string {
	sum := sha256.Sum256([]byte(bucket))

	return "obj/" + hex.EncodeToString(sum[:]) + "/"
}

// objectBase is the per-object fragment namespace: bucket names and object
// keys are arbitrary unicode (and may collide with path syntax), so both
// segments are hashes and the human-readable bucket/key live in the sidecar.
func objectBase(bucket, key string) string {
	sum := sha256.Sum256([]byte(bucket + "\x00" + key))

	return bucketObjectsPrefix(bucket) + hex.EncodeToString(sum[:])
}

// sidecarName is the sidecar's fragment name. It is generation-less: replacing
// it atomically at the store is the commit that flips readers to the new
// generation.
func sidecarName(bucket, key string) string {
	return objectBase(bucket, key) + "/meta"
}

// fragmentName names one payload fragment of a generation.
func fragmentName(bucket, key, generation string, index int) string {
	return objectBase(bucket, key) + "/" + generation + ".f" + strconv.Itoa(index)
}

// newGeneration mints a random generation stamp.
func newGeneration() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.Wrap(err, "generation entropy")
	}

	return hex.EncodeToString(b[:]), nil
}

// EncryptionInfo is the sidecar record of an object's encryption, mirroring
// what storagefs keeps so both backends read the same way.
type EncryptionInfo struct {
	// Algorithm is the S3 name reported to clients; only "AES256" exists.
	Algorithm string `json:"algorithm"`
	// Key is the object's data key, sealed by a master key it names.
	Key sse.WrappedKey `json:"key"`
	// NonceBase is the per-object random the chunk nonces derive from.
	NonceBase []byte `json:"nonce_base"`
	// PlainSize is the object's logical size: what a client is told, and what
	// a read must be given so a truncated body is detectable.
	PlainSize int64 `json:"plain_size"`
}

// LogicalSize is the object's size as a client sees it: the plaintext length
// for an encrypted object, and the stored length otherwise.
//
// Every client-facing answer — listings, HEAD, conditional writes, attributes
// — goes through this rather than reading Size, because Size is the stored
// length and reporting it would leak one tag per 64 KiB into the object size.
func (sc *Sidecar) LogicalSize() int64 {
	if sc.Encryption != nil {
		return sc.Encryption.PlainSize
	}

	return sc.Size
}
