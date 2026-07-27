package storagefs

import (
	"context"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

// RotationReport is what one rotation pass did.
type RotationReport struct {
	// Scanned is the number of objects examined.
	Scanned int
	// Rewrapped is the number whose data key moved onto the current master
	// key.
	Rewrapped int
	// AlreadyCurrent is the number already wrapped by the current key —
	// what a second run of a finished rotation reports for everything.
	AlreadyCurrent int
	// Unencrypted is the number stored in the clear, which a rotation does
	// not touch.
	Unencrypted int
	// Failed lists objects whose key could not be rewrapped, which is what a
	// missing retired master key looks like.
	Failed []ObjectRef
}

// Done reports whether the store is fully on the current master key, so a
// retired key can be removed from the configuration.
func (r *RotationReport) Done() bool { return len(r.Failed) == 0 }

// RotateKeys moves every encrypted object's data key onto the current master
// key, rewriting sidecars and never touching object bodies.
//
// That is the whole reason for envelope encryption: the master key wraps a few
// dozen bytes per object rather than the objects themselves, so rotating it
// costs a metadata walk instead of re-encrypting the store. A rotation can be
// interrupted and resumed — an object is either wrapped by the old key or the
// new one, both readable while both keys are configured.
//
// The order of operations for an operator is therefore: add the new key as
// current and keep the old one in previous_key_files, run this until it
// reports no failures, then remove the old key. Removing it earlier is what
// makes objects unreadable, which is why Failed names them.
func (s *Storage) RotateKeys(ctx context.Context) (*RotationReport, error) {
	if !s.encrypting() {
		return nil, errors.Wrap(fs.ErrUnsupportedOperation,
			"no master key is configured, so there is nothing to rotate onto")
	}

	buckets, err := s.ListBuckets(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "list buckets")
	}

	report := &RotationReport{}

	for _, b := range buckets {
		objects, err := s.ListObjects(ctx, &fs.ListObjectsRequest{Bucket: b.Name})
		if err != nil {
			return nil, errors.Wrapf(err, "list objects in %q", b.Name)
		}

		for _, o := range objects.Objects {
			if err := ctx.Err(); err != nil {
				return report, err
			}

			s.rotateObject(b.Name, o.Key, report)
		}
	}

	return report, nil
}

// rotateObject rewraps one object's data key.
func (s *Storage) rotateObject(bucket, key string, report *RotationReport) {
	report.Scanned++

	// The sidecar is read and written under the same lock a tagging update
	// takes, so a rotation cannot lose a concurrent metadata change by writing
	// back a document it read before that change landed.
	s.metaMu.Lock()
	defer s.metaMu.Unlock()

	sc, err := s.readSidecar(bucket, key)
	if err != nil || sc == nil {
		if err != nil {
			report.Failed = append(report.Failed, ObjectRef{bucket, key})
		}

		return
	}

	if sc.Encryption == nil {
		report.Unencrypted++
		return
	}

	wrapped, changed, err := s.keyring.Rewrap(sc.Encryption.Key)
	if err != nil {
		report.Failed = append(report.Failed, ObjectRef{bucket, key})
		return
	}

	if !changed {
		report.AlreadyCurrent++
		return
	}

	sc.Encryption.Key = wrapped

	if err := s.writeSidecar(bucket, sc); err != nil {
		report.Failed = append(report.Failed, ObjectRef{bucket, key})
		return
	}

	report.Rewrapped++
}
