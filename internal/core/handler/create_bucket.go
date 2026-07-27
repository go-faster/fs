package handler

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/s3err"
)

// CreateBucket implements PUT /{bucket}. Any LocationConstraint in the request
// body is ignored (this server is single-region); on success it echoes the
// bucket path in the Location header as S3 does. A canned x-amz-acl (e.g.
// public-read) is recorded on the new bucket.
//
// Re-creating a bucket you already own succeeds, which is what S3 does in its
// default region and what makes a retried create safe. Re-creating someone
// else's is BucketAlreadyExists: the name is taken, and saying so is the whole
// point of a global namespace.
func (h *handler) CreateBucket(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/")
	name, _, _ := strings.Cut(path, "/")

	if err := h.createBucket(ctx, name); err != nil {
		renderError(ctx, w, r, err)
		return
	}

	// x-amz-object-ownership sets the ownership rule at create time, which is
	// the only moment BucketOwnerEnforced can be chosen before any object
	// exists to be owned.
	if ownership := r.Header.Get("X-Amz-Object-Ownership"); ownership != "" {
		if !validObjectOwnership(ownership) {
			renderAPIError(ctx, w, r, s3err.InvalidArgument,
				errors.Errorf("unknown object ownership %q", ownership))

			return
		}

		if store, ok := h.service.(fs.BucketSettingsStore); ok {
			if err := store.SetBucketObjectOwnership(ctx, name, ownership); err != nil {
				renderError(ctx, w, r, err)
				return
			}
		}
	}

	if acl := fs.ParseACL(r.Header.Get("X-Amz-Acl")); acl != fs.ACLPrivate {
		if err := h.service.SetBucketACL(ctx, name, acl); err != nil {
			renderError(ctx, w, r, err)
			return
		}
	}

	w.Header().Set("Location", "/"+name)
	w.WriteHeader(http.StatusOK)
}

// createBucket creates the bucket for the calling principal, treating a
// re-create by its owner as success.
func (h *handler) createBucket(ctx context.Context, name string) error {
	ownership, ok := h.service.(fs.BucketOwnership)
	if !ok {
		// A backend that does not record owners cannot tell a re-create by the
		// owner from anyone else's, so it keeps the older answer.
		return h.service.CreateBucket(ctx, name)
	}

	owner := callerOwner(ctx)

	err := ownership.CreateBucketOwned(ctx, name, owner)
	if !errors.Is(err, fs.ErrBucketAlreadyExists) {
		return err
	}

	current, ownerErr := ownership.BucketOwner(ctx, name)
	if ownerErr != nil {
		return err
	}

	// An unowned bucket predates ownership; there is no one to conflict with,
	// so treat the caller as its owner rather than locking everyone out.
	if current.IsZero() || current.ID == owner.ID {
		return nil
	}

	return errors.Wrap(fs.ErrBucketOwnedBySomeoneElse, name)
}
