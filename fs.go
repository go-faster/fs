// Package fs is a S3-compatible storage server implementation.
package fs

import (
	"io"
	"sort"
	"strings"
	"time"
)

// Bucket represents an S3 bucket.
type Bucket struct {
	Name         string
	CreationDate time.Time
}

// Owner identifies the S3 principal that owns a bucket or object. It is
// recorded when the object is written and reported in ACL and listing
// responses; clients compare it to decide what a caller owns, so it must not
// change when a different credential reads the object.
type Owner struct {
	// ID is the canonical user ID.
	ID string
	// DisplayName is the human-readable owner name.
	DisplayName string
}

// IsZero reports whether no owner was recorded (objects written before owners
// were modeled, or by an unauthenticated server).
func (o Owner) IsZero() bool {
	return o.ID == "" && o.DisplayName == ""
}

// Object represents an S3 object.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
	ETag         string
	// Owner is the principal that wrote the object; zero when unrecorded.
	Owner Owner
}

// ObjectMetadata holds the user-controlled metadata stored with an object:
// the standard HTTP representation headers plus x-amz-meta-* pairs.
type ObjectMetadata struct {
	ContentType        string
	CacheControl       string
	ContentDisposition string
	ContentEncoding    string
	// Expires is the raw Expires header value, stored and returned verbatim.
	// S3 keeps whatever the client sent rather than reformatting it, and a
	// client that round-trips the header expects its own bytes back.
	Expires string
	// UserMetadata holds x-amz-meta-* pairs, keyed by the lowercase name
	// without the prefix (e.g. "color" for x-amz-meta-color).
	UserMetadata map[string]string
}

// IsZero reports whether no metadata field is set.
func (m ObjectMetadata) IsZero() bool {
	return m.ContentType == "" && m.CacheControl == "" && m.ContentDisposition == "" &&
		m.ContentEncoding == "" && m.Expires == "" && len(m.UserMetadata) == 0
}

// ObjectVersion is one entry of a version listing: a stored version, or a
// delete marker recording that the key was deleted at that point.
type ObjectVersion struct {
	Key       string
	VersionID string
	// IsLatest reports that this is the key's current version.
	IsLatest bool
	// DeleteMarker reports that this entry records a deletion, not content.
	DeleteMarker bool
	Size         int64
	ETag         string
	LastModified time.Time
	Owner        Owner
}

// ListObjectVersionsRequest describes one page of a version listing. It mirrors
// ListObjectsRequest, with the version-aware cursor S3 uses: a key marker and,
// within that key, a version marker.
type ListObjectVersionsRequest struct {
	Bucket    string
	Prefix    string
	Delimiter string
	// KeyMarker and VersionIDMarker are the exclusive lower bound: listing
	// resumes after that version of that key.
	KeyMarker       string
	VersionIDMarker string
	Limit           int
}

// ListObjectVersionsResponse is one page of a version listing, newest version
// first within each key.
type ListObjectVersionsResponse struct {
	Versions       []ObjectVersion
	CommonPrefixes []string
	IsTruncated    bool
	// NextKeyMarker and NextVersionIDMarker are where the next page resumes.
	NextKeyMarker       string
	NextVersionIDMarker string
}

// CORSRule allows cross-origin requests matching AllowedOrigins and
// AllowedMethods. It lives here, rather than in the cors package, because a
// bucket's rules are stored with the bucket; the cors package aliases it.
type CORSRule struct {
	AllowedOrigins []string
	AllowedMethods []string
	AllowedHeaders []string
	ExposeHeaders  []string
	MaxAgeSeconds  int
}

// AllowsHeaders reports whether the rule permits every requested header (the
// comma-separated Access-Control-Request-Headers value).
func (r *CORSRule) AllowsHeaders(requested string) bool {
	if requested == "" {
		return true
	}

	for h := range strings.SplitSeq(requested, ",") {
		if h = strings.TrimSpace(h); h == "" {
			continue
		}

		if !corsHeaderAllowed(r.AllowedHeaders, h) {
			return false
		}
	}

	return true
}

// corsHeaderAllowed reports whether one requested header is permitted.
func corsHeaderAllowed(allowed []string, header string) bool {
	for _, a := range allowed {
		if a == "*" || strings.EqualFold(a, header) {
			return true
		}
	}

	return false
}

// VersioningState is a bucket's versioning setting. The zero value means the
// bucket was never versioned, which is a third state and not a synonym for
// Suspended: a never-versioned bucket reports no status at all, and its
// objects have no version IDs until the first enable adopts them as "null".
//
// A bucket moves unversioned -> Enabled -> Suspended -> Enabled -> ... and can
// never go back to unversioned; that is S3's model, and it is what lets the
// read path assume that a bucket with any versioning history keeps its
// versions forever.
type VersioningState string

// The versioning states a bucket can be in.
const (
	// VersioningUnset is a bucket that has never had versioning configured.
	VersioningUnset VersioningState = ""
	// VersioningEnabled makes every write create a new version.
	VersioningEnabled VersioningState = "Enabled"
	// VersioningSuspended sends writes to the "null" version while retaining
	// the versions written while it was enabled.
	VersioningSuspended VersioningState = "Suspended"
)

// NullVersionID is the version ID of an object written while versioning was
// suspended, and of one that predates the first enable.
//
// It is a real version ID, not a sentinel for "no version": clients send it,
// listings report it, and a delete addressed at it removes exactly that
// version. Code that treats it as equivalent to the empty string will work
// until the first suspended write.
const NullVersionID = "null"

// PublicAccessBlock is a bucket's public-access-block configuration: four
// independent switches over the ways a bucket can become publicly readable.
// Absent (a nil *PublicAccessBlock) means no configuration, which S3 reports
// as NoSuchPublicAccessBlockConfiguration rather than as all-false.
type PublicAccessBlock struct {
	BlockPublicACLs       bool
	IgnorePublicACLs      bool
	BlockPublicPolicy     bool
	RestrictPublicBuckets bool
}

// Object-ownership settings, which decide who owns objects another principal
// writes into a bucket. Empty means the bucket has no configuration.
const (
	// OwnershipBucketOwnerEnforced disables ACLs entirely; the bucket owner
	// owns every object.
	OwnershipBucketOwnerEnforced = "BucketOwnerEnforced"
	// OwnershipBucketOwnerPreferred gives the bucket owner objects written
	// with the bucket-owner-full-control canned ACL.
	OwnershipBucketOwnerPreferred = "BucketOwnerPreferred"
	// OwnershipObjectWriter leaves each object owned by its writer.
	OwnershipObjectWriter = "ObjectWriter"
)

// Tag is a single object tag.
type Tag struct {
	Key   string
	Value string
}

type PutObjectRequest struct {
	Reader   io.Reader
	Bucket   string
	Key      string
	Size     int64
	Metadata ObjectMetadata
	Tags     []Tag
	// ACL is the canned access-control level for the object (default
	// ACLPrivate). Governs anonymous access only.
	ACL ACL
	// Owner is the principal writing the object, recorded with it and reported
	// in ACL and listing responses. Zero leaves the object unowned.
	Owner Owner

	// IfNoneMatch and IfMatch carry the raw conditional-write header values
	// (e.g. "*" or a quoted ETag list). When set, the storage backend must
	// evaluate them atomically with the write — see PreconditionFailed — so
	// concurrent conditional PUTs resolve to a single winner. Empty means no
	// condition.
	IfNoneMatch string
	IfMatch     string

	// ContentMD5 is the hex-encoded MD5 the client claims the body has
	// (decoded from the base64 Content-MD5 header). When set, the backend must
	// compare it against what it actually received and refuse the write with
	// ErrBadDigest before the object becomes visible — verifying afterwards
	// would leave corrupt content readable in the window between.
	ContentMD5 string

	// ChecksumAlgorithm names the client-visible checksum to compute over the
	// body ("SHA256", "CRC32", ...). Empty computes none.
	//
	// This is a second checksum, distinct from ContentMD5 and from the ETag:
	// the client chose it, sent it, and expects it back unchanged.
	ChecksumAlgorithm string
	// Checksum is the base64 digest the client claims the body has. When set,
	// the backend must compare it against what it actually received and refuse
	// the write with ErrBadDigest before the object becomes visible.
	Checksum string

	// ServerSideEncryption asks for the object to be encrypted at rest, and
	// carries the algorithm ("AES256"). Empty stores the body as-is.
	//
	// The decision is made above the backend, because it is the bucket's
	// default and the request's header that settle it, and a backend must not
	// have to know about either. A backend that cannot encrypt refuses a
	// non-empty value with ErrUnsupportedOperation rather than storing
	// plaintext — silently ignoring it would report an object as encrypted
	// when it is not.
	ServerSideEncryption string
}

// PutObjectResponse reports the stored object's ETag.
type PutObjectResponse struct {
	ETag string
	// ChecksumAlgorithm and Checksum echo the client-visible checksum stored
	// with the object.
	ChecksumAlgorithm string
	Checksum          string
	// ServerSideEncryption echoes the algorithm the object was encrypted
	// with, empty when it was stored in the clear.
	ServerSideEncryption string
	// VersionID names the version this write created, empty on a bucket that
	// is not versioned.
	VersionID string
}

// GetObjectResponse represents the response for GetObject operation.
type GetObjectResponse struct {
	Reader       io.ReadCloser
	Size         int64
	LastModified time.Time
	ETag         string
	Metadata     ObjectMetadata
	// VersionID names the version served, empty on a bucket that is not
	// versioned.
	VersionID string
	// TagCount is how many tags the object carries, reported on GET and HEAD
	// as x-amz-tagging-count. Backends fill it from metadata they already read;
	// zero means untagged (the header is then omitted).
	TagCount int

	// ServerSideEncryption names the algorithm the object is encrypted with at
	// rest, echoed to the client as x-amz-server-side-encryption. Empty for an
	// object stored in the clear. Size is always the plaintext size, so a
	// client never learns the on-disk size.
	ServerSideEncryption string

	// ChecksumAlgorithm and Checksum carry the client-visible checksum stored
	// with the object, reported only when the request asks for it. ChecksumType
	// distinguishes a multipart COMPOSITE value — a digest of the part digests
	// — from a FULL_OBJECT one.
	ChecksumAlgorithm string
	Checksum          string
	ChecksumType      string
}

// MultipartUpload represents an in-progress multipart upload.
type MultipartUpload struct {
	UploadID  string
	Bucket    string
	Key       string
	Initiated time.Time
}

// CreateMultipartUploadRequest represents a request to start a multipart
// upload. Metadata and tags are applied to the object at completion.
type CreateMultipartUploadRequest struct {
	Bucket   string
	Key      string
	Metadata ObjectMetadata
	Tags     []Tag
	ACL      ACL
	// ServerSideEncryption asks for the completed object to be encrypted at
	// rest, and carries the algorithm ("AES256"). It is settled when the
	// upload starts, not when it completes, because the parts are already on
	// disk by then and would have been staged in the clear.
	ServerSideEncryption string
	// Owner is the principal starting the upload; it owns the completed object.
	Owner Owner
	// ChecksumAlgorithm names the client-visible checksum every part of this
	// upload carries, and ChecksumType whether the completed object's digest is
	// a digest of the part digests (COMPOSITE) or of the whole body
	// (FULL_OBJECT).
	//
	// Settled when the upload starts rather than at completion, for the same
	// reason encryption is: the parts are digested as they arrive, so what to
	// digest them with has to be known before the first one does.
	ChecksumAlgorithm string
	ChecksumType      string
}

// Part represents a part of a multipart upload.
type Part struct {
	PartNumber   int
	ETag         string
	Size         int64
	LastModified time.Time
	// Checksum is the part's own client-visible digest, base64, empty when the
	// upload asked for none. It is the part's alone: the completed object's is
	// composed from these rather than taken over the assembled body.
	Checksum string
}

// ObjectPart is one part of a *completed* multipart object, retained after the
// upload finishes so the object can still be read and described a part at a
// time (GetObjectAttributes, ?partNumber=N).
type ObjectPart struct {
	PartNumber int
	Size       int64
	ETag       string
	// Checksum is the digest this part carried when it was uploaded, kept so a
	// ranged read of one part can still report it — which is what a client
	// verifying a download part by part asks for.
	Checksum string
}

// ObjectAttributes describes an object without opening its body: what
// GetObjectAttributes reports, plus the part layout when the object was
// assembled from a multipart upload (nil for a single PUT).
type ObjectAttributes struct {
	ETag         string
	Size         int64
	LastModified time.Time
	// UploadID names the multipart upload this object was completed from, when
	// it was. It is what lets a retried CompleteMultipartUpload be recognized
	// as a retry rather than answered with NoSuchUpload; empty for a single
	// PUT.
	UploadID string
	// Parts is the completed part layout in ascending part-number order, or
	// nil when the object was written by a single PUT.
	Parts []ObjectPart
}

// PartRange returns the byte range covered by part number n (1-based) and
// whether it exists. For an object with no recorded parts, part 1 is the whole
// object, which is what S3 reports for a single PUT.
func (a *ObjectAttributes) PartRange(n int) (offset, length int64, ok bool) {
	if len(a.Parts) == 0 {
		if n != 1 {
			return 0, 0, false
		}

		return 0, a.Size, true
	}

	for _, p := range a.Parts {
		if p.PartNumber == n {
			return offset, p.Size, true
		}

		offset += p.Size
	}

	return 0, 0, false
}

// PartsCount reports how many parts the object was assembled from; zero when
// it was written by a single PUT.
func (a *ObjectAttributes) PartsCount() int {
	return len(a.Parts)
}

// UploadPartRequest represents a request to upload a part.
type UploadPartRequest struct {
	Bucket     string
	Key        string
	UploadID   string
	PartNumber int
	Reader     io.Reader
	Size       int64
	// ChecksumAlgorithm and Checksum are as on PutObjectRequest: what to
	// compute over this part, and what the client claims it will be. A claim
	// that does not match is BadDigest and the part is not stored.
	ChecksumAlgorithm string
	Checksum          string
}

// CompletedPart represents a completed part for completing multipart upload.
type CompletedPart struct {
	PartNumber int
	ETag       string
	// Checksum is the digest the client says this part had. Checked against
	// what the part actually carried, for the same reason the ETag is: a
	// completion naming parts it did not upload must not assemble them.
	Checksum string
}

// CompleteMultipartUploadRequest represents a request to complete multipart upload.
type CompleteMultipartUploadRequest struct {
	Bucket   string
	Key      string
	UploadID string
	Parts    []CompletedPart
	// Conditions are the conditional-write headers carried by the completion
	// request. A backend that supports them must evaluate them atomically with
	// the write, exactly as it does for a conditional PutObject; the zero value
	// imposes no condition.
	Conditions Conditions
	// Checksum is the completed object's digest as the client computed it, and
	// ChecksumType which kind it is. Both are claims: the server composes its
	// own from the parts and refuses a completion that disagrees.
	Checksum     string
	ChecksumType string
}

// CompleteMultipartUploadResponse represents the response for completing multipart upload.
type CompleteMultipartUploadResponse struct {
	Location string
	Bucket   string
	Key      string
	ETag     string
	// ServerSideEncryption echoes the algorithm the completed object was
	// encrypted with, empty when it was stored in the clear.
	ServerSideEncryption string
	// ChecksumAlgorithm, Checksum and ChecksumType describe the completed
	// object's client-visible digest. A completion echoes them unconditionally,
	// as a write does: the client just supplied the parts and is entitled to
	// see what they added up to.
	ChecksumAlgorithm string
	Checksum          string
	ChecksumType      string
}

// FoldVersionPage turns a bucket's gathered versions into one page: applies
// the prefix and delimiter, orders keys ascending with each key's versions
// newest-first, applies the key/version marker, and cuts at Limit.
//
// It lives here for the same reason FoldPage does: the rules are subtle enough
// to get wrong in three places, and backends outside this repository implement
// the same interface. The ordering is S3's — a version listing is a flat
// sequence ordered by key, then by version age within the key.
func (r *ListObjectVersionsRequest) FoldVersionPage(byKey map[string][]ObjectVersion) *ListObjectVersionsResponse {
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		if r.Prefix == "" || strings.HasPrefix(key, r.Prefix) {
			keys = append(keys, key)
		}
	}

	sort.Strings(keys)

	out := &ListObjectVersionsResponse{}
	seenPrefix := make(map[string]struct{})

	limit := r.Limit
	if limit <= 0 {
		limit = 1000
	}

	for _, key := range keys {
		// Delimiter folding collapses a whole keyspace into one entry, exactly
		// as it does for an object listing.
		if r.Delimiter != "" {
			rest := strings.TrimPrefix(key, r.Prefix)
			if idx := strings.Index(rest, r.Delimiter); idx >= 0 {
				folded := r.Prefix + rest[:idx+len(r.Delimiter)]
				if _, ok := seenPrefix[folded]; ok {
					continue
				}

				if folded <= r.KeyMarker {
					continue
				}

				if len(out.Versions)+len(out.CommonPrefixes) >= limit {
					out.IsTruncated = true
					return out
				}

				seenPrefix[folded] = struct{}{}
				out.CommonPrefixes = append(out.CommonPrefixes, folded)

				continue
			}
		}

		if key < r.KeyMarker {
			continue
		}

		for _, v := range byKey[key] {
			// Within the marker's own key, resume after the named version.
			if key == r.KeyMarker && r.VersionIDMarker != "" && v.VersionID <= r.VersionIDMarker {
				continue
			}

			if key == r.KeyMarker && r.VersionIDMarker == "" && r.KeyMarker != "" {
				continue
			}

			if len(out.Versions)+len(out.CommonPrefixes) >= limit {
				out.IsTruncated = true
				return out
			}

			out.Versions = append(out.Versions, v)
			out.NextKeyMarker, out.NextVersionIDMarker = v.Key, v.VersionID
		}
	}

	return out
}
