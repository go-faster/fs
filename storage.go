package fs

import (
	"context"
	"sort"
	"strings"
)

// ListObjectsRequest describes one page of a bucket listing.
//
// Listing is paged and delimiter-folded at this seam because a bucket is
// unbounded: at the design target of 100M objects per node, returning a whole
// bucket is not a slow answer but an impossible one, and the S3 layer above
// only ever renders 1000 entries at a time regardless.
//
// The delimiter belongs here rather than above for the same reason. Folding
// collapses a whole keyspace into one entry, so a caller that folded after
// paging would have to keep asking for more keys until enough of them
// collapsed — several requests per page, over a backend that may walk the
// bucket for each. A backend folds in the pass it was already making, and one
// with an index can seek past a folded prefix instead of reading it.
type ListObjectsRequest struct {
	Bucket string
	// Prefix restricts the listing; empty lists the bucket.
	Prefix string
	// Delimiter folds every key sharing a prefix up to its first occurrence
	// into a single common prefix. Empty returns keys unfolded.
	Delimiter string
	// StartAfter is an exclusive lower bound in the folded order: only entries
	// sorting strictly after it are returned. It may name a common prefix, in
	// which case every key under that prefix is already accounted for.
	StartAfter string
	// Limit caps the page, counting objects and common prefixes together the
	// way S3 counts max-keys. Zero or negative asks for everything, which only
	// callers that know the answer is small should do — an emptiness check, a
	// test fixture. The S3 listing path always sets it.
	Limit int
}

// ListObjectsResponse is one page of a bucket listing.
type ListObjectsResponse struct {
	// Objects are the unfolded keys of this page, sorted ascending.
	Objects []Object
	// CommonPrefixes are the folded ones, sorted ascending. Both lists are
	// interleaved by key for the purposes of Limit and NextStartAfter, then
	// reported separately because that is how S3 renders them.
	CommonPrefixes []string
	// IsTruncated reports that entries exist after the last one returned.
	IsTruncated bool
	// NextStartAfter is the last entry on this page — an object key or a
	// common prefix — and is what the next request passes as StartAfter.
	NextStartAfter string
}

// FoldPage turns a bucket's matching objects into one page: folds them by the
// request's delimiter, orders them, applies StartAfter, and cuts at Limit.
//
// It exists so that every backend folds and pages identically. The rules are
// subtle enough to get wrong in three places — common prefixes count toward
// the limit, a page exactly at the limit is truncated only if something
// follows, and a StartAfter naming a common prefix must skip every key beneath
// it — and backends outside this repository implement the same interface.
//
// objects must already be filtered by Prefix; any order will do.
func (r *ListObjectsRequest) FoldPage(objects []Object) *ListObjectsResponse {
	type entry struct {
		key      string
		object   Object
		isPrefix bool
	}

	entries := make([]entry, 0, len(objects))
	seen := make(map[string]struct{})

	for _, o := range objects {
		if r.Delimiter != "" {
			rest := strings.TrimPrefix(o.Key, r.Prefix)

			if idx := strings.Index(rest, r.Delimiter); idx >= 0 {
				folded := r.Prefix + rest[:idx+len(r.Delimiter)]
				if _, ok := seen[folded]; ok {
					continue
				}

				seen[folded] = struct{}{}
				entries = append(entries, entry{key: folded, isPrefix: true})

				continue
			}
		}

		entries = append(entries, entry{key: o.Key, object: o})
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].key < entries[j].key })

	out := &ListObjectsResponse{}

	for _, e := range entries {
		if e.key <= r.StartAfter {
			continue
		}

		if r.Limit > 0 && len(out.Objects)+len(out.CommonPrefixes) >= r.Limit {
			out.IsTruncated = true

			break
		}

		if e.isPrefix {
			out.CommonPrefixes = append(out.CommonPrefixes, e.key)
		} else {
			out.Objects = append(out.Objects, e.object)
		}

		out.NextStartAfter = e.key
	}

	return out
}

// Storage defines the interface for S3-compatible storage operations.
//
//go:generate go tool moq -fmt goimports -out ./internal/mock/storage.go -pkg mock . Storage
//go:generate go run ./scripts/gencompat
type Storage interface {
	ListBuckets(ctx context.Context) ([]Bucket, error)
	CreateBucket(ctx context.Context, bucket string) error
	DeleteBucket(ctx context.Context, bucket string) error
	BucketExists(ctx context.Context, bucket string) (bool, error)
	// ListObjects returns one page of a bucket's objects, sorted by key.
	ListObjects(ctx context.Context, req *ListObjectsRequest) (*ListObjectsResponse, error)
	PutObject(ctx context.Context, req *PutObjectRequest) (*PutObjectResponse, error)
	GetObject(ctx context.Context, bucket, key string) (*GetObjectResponse, error)
	DeleteObject(ctx context.Context, bucket, key string) error

	// GetObjectTagging returns the object's tag set (empty when untagged).
	GetObjectTagging(ctx context.Context, bucket, key string) ([]Tag, error)
	// PutObjectTagging replaces the object's tag set.
	PutObjectTagging(ctx context.Context, bucket, key string, tags []Tag) error
	// DeleteObjectTagging removes the object's tag set.
	DeleteObjectTagging(ctx context.Context, bucket, key string) error

	// SetBucketACL records the bucket's canned ACL.
	SetBucketACL(ctx context.Context, bucket string, acl ACL) error
	// BucketACL returns the bucket's canned ACL (ACLPrivate default);
	// ErrBucketNotFound when the bucket is absent.
	BucketACL(ctx context.Context, bucket string) (ACL, error)
	// ObjectACL returns the object's canned ACL (ACLPrivate default);
	// ErrBucketNotFound/ErrObjectNotFound when absent.
	ObjectACL(ctx context.Context, bucket, key string) (ACL, error)
	// SetObjectACL records the object's canned ACL, leaving its content and
	// other metadata untouched; ErrBucketNotFound/ErrObjectNotFound when absent.
	SetObjectACL(ctx context.Context, bucket, key string, acl ACL) error
	// ObjectOwner returns the principal recorded when the object was written
	// (zero when unrecorded); ErrBucketNotFound/ErrObjectNotFound when absent.
	ObjectOwner(ctx context.Context, bucket, key string) (Owner, error)

	CreateMultipartUpload(ctx context.Context, req *CreateMultipartUploadRequest) (*MultipartUpload, error)
	UploadPart(ctx context.Context, req *UploadPartRequest) (*Part, error)
	// ListParts returns the parts uploaded so far for an in-progress multipart
	// upload, sorted by ascending part number.
	ListParts(ctx context.Context, bucket, key, uploadID string) ([]Part, error)
	// ListMultipartUploads returns the in-progress multipart uploads for a
	// bucket, sorted by object key (then upload ID for equal keys).
	ListMultipartUploads(ctx context.Context, bucket string) ([]MultipartUpload, error)
	CompleteMultipartUpload(ctx context.Context, req *CompleteMultipartUploadRequest) (*CompleteMultipartUploadResponse, error)
	AbortMultipartUpload(ctx context.Context, bucket, key, uploadID string) error
}

// ConditionalDeleter is the optional capability of deleting an object only if
// it still matches a condition. Backends implement it by evaluating cond under
// the same lock that serializes writes to the key, so the check and the delete
// are atomic; a backend that cannot do that must not implement it, and the S3
// layer rejects conditional deletes against it rather than racing.
//
// DeleteObjectIf returns ErrObjectNotFound when the object is absent (deletion
// is idempotent: the caller reports that as success) and ErrPreconditionFailed
// when the object is present but cond does not hold.
type ConditionalDeleter interface {
	DeleteObjectIf(ctx context.Context, bucket, key string, cond Conditions) error
}
