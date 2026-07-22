package clusterstore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	"github.com/go-faster/errors"

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
	// Size is the exact object length, needed to re-plan fragment sizes and
	// to unpad EC reconstructions.
	Size int64 `json:"size"`
	// Generation names the fragment set this sidecar commits.
	Generation string `json:"generation"`
	// Modified orders concurrent writers during list-merge and repair
	// reconciliation (newest wins).
	Modified time.Time `json:"modified"`

	ETag string `json:"etag,omitempty"`
	// Checksum is the hex MD5 of the full object content (scrubber and
	// verify-on-read input; equal to ETag for single-part writes).
	Checksum string `json:"checksum,omitempty"`

	ContentType        string            `json:"content_type,omitempty"`
	CacheControl       string            `json:"cache_control,omitempty"`
	ContentDisposition string            `json:"content_disposition,omitempty"`
	ContentEncoding    string            `json:"content_encoding,omitempty"`
	UserMetadata       map[string]string `json:"user_metadata,omitempty"`
	Tags               []fs.Tag          `json:"tags,omitempty"`
	ACL                fs.ACL            `json:"acl,omitempty"`
}

// ObjectMetadata converts the sidecar's header fields to the domain type.
func (sc *Sidecar) ObjectMetadata() fs.ObjectMetadata {
	return fs.ObjectMetadata{
		ContentType:        sc.ContentType,
		CacheControl:       sc.CacheControl,
		ContentDisposition: sc.ContentDisposition,
		ContentEncoding:    sc.ContentEncoding,
		UserMetadata:       sc.UserMetadata,
	}
}

// ParseScheme returns the scheme the object was written with.
func (sc *Sidecar) ParseScheme() (scheme.Scheme, error) {
	return scheme.Parse(sc.Scheme)
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
