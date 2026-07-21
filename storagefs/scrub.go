package storagefs

import (
	"context"
	"crypto/md5" //nolint:gosec // MD5 is the stored object checksum (S3 ETag).
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

// quarantineSubdir holds objects the scrubber found corrupt, moved aside so they
// stop being served.
const quarantineSubdir = ".quarantine"

// ObjectRef identifies an object by bucket and key.
type ObjectRef struct {
	Bucket string
	Key    string
}

// ScrubReport summarizes a scrub pass.
type ScrubReport struct {
	// Scanned is the number of objects examined.
	Scanned int
	// OK is the number whose content matched its stored checksum.
	OK int
	// Corrupt lists objects whose content did not match (bit-rot).
	Corrupt []ObjectRef
	// Unverifiable is the number with no stored checksum to compare against
	// (e.g. pre-checksum data directories).
	Unverifiable int
	// Quarantined is the number of corrupt objects moved aside.
	Quarantined int
}

// Healthy reports whether the pass found no corruption.
func (r *ScrubReport) Healthy() bool { return len(r.Corrupt) == 0 }

// ScrubOptions configures a scrub pass.
type ScrubOptions struct {
	// Quarantine moves each corrupt object (and its sidecar) into
	// <root>/.quarantine so it is no longer served. When false, corruption is
	// only reported.
	Quarantine bool
}

// Scrub walks every object, recomputing its MD5 and comparing to the stored
// content checksum, reporting (and optionally quarantining) any mismatch. It is
// safe to run concurrently with serving; it stops early if ctx is canceled.
func (s *Storage) Scrub(ctx context.Context, opts ScrubOptions) (*ScrubReport, error) {
	buckets, err := s.ListBuckets(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "list buckets")
	}

	report := &ScrubReport{}

	for _, b := range buckets {
		objects, err := s.ListObjects(ctx, b.Name, "")
		if err != nil {
			return nil, errors.Wrapf(err, "list objects in %q", b.Name)
		}

		for _, o := range objects {
			if err := ctx.Err(); err != nil {
				return report, err
			}

			s.scrubObject(b.Name, o.Key, opts, report)
		}
	}

	return report, nil
}

// scrubObject verifies one object and updates the report.
func (s *Storage) scrubObject(bucket, key string, opts ScrubOptions, report *ScrubReport) {
	report.Scanned++

	expected, ok := s.storedChecksum(bucket, key)
	if !ok {
		report.Unverifiable++
		return
	}

	actual, err := fileMD5(filepath.Join(s.root, bucket, toOSPath(key)))
	if err != nil {
		// A read error on the object path is itself a corruption signal.
		report.Corrupt = append(report.Corrupt, ObjectRef{bucket, key})
		return
	}

	if actual == expected {
		report.OK++
		return
	}

	report.Corrupt = append(report.Corrupt, ObjectRef{bucket, key})

	if opts.Quarantine && s.quarantineObject(bucket, key) == nil {
		report.Quarantined++
	}
}

// storedChecksum returns the object's expected full-content MD5 from its
// sidecar. It falls back to the ETag when that is a plain MD5 (single-part
// objects written before checksums were stored). ok is false when there is
// nothing to verify against.
func (s *Storage) storedChecksum(bucket, key string) (string, bool) {
	sc, err := s.readSidecar(bucket, key)
	if err != nil || sc == nil {
		return "", false
	}

	if sc.Checksum != "" {
		return sc.Checksum, true
	}

	if isPlainMD5(sc.ETag) {
		return sc.ETag, true
	}

	return "", false
}

// isPlainMD5 reports whether s is a 32-hex-char MD5 (not a multipart "-N" ETag).
func isPlainMD5(s string) bool {
	if len(s) != 32 {
		return false
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}

// fileMD5 returns the hex MD5 of the file at path.
func fileMD5(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // Path built from a validated bucket/key under root.
	if err != nil {
		return "", errors.Wrap(err, "open object")
	}
	defer func() { _ = f.Close() }()

	h := md5.New() //nolint:gosec // MD5 is the stored object checksum (S3 ETag).
	if _, err := io.Copy(h, f); err != nil {
		return "", errors.Wrap(err, "hash object")
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// quarantineObject moves a corrupt object and its sidecar under
// <root>/.quarantine/<bucket>/, mirroring the key path, so it stops serving.
func (s *Storage) quarantineObject(bucket, key string) error {
	dst := filepath.Join(s.root, quarantineSubdir, bucket, toOSPath(key))
	if err := os.MkdirAll(filepath.Dir(dst), defaultDirPermissions); err != nil {
		return errors.Wrap(err, "create quarantine dir")
	}

	src := filepath.Join(s.root, bucket, toOSPath(key))
	if err := os.Rename(src, dst); err != nil {
		return errors.Wrap(err, "quarantine object")
	}

	// Best-effort: move the sidecar alongside (its absence is tolerated).
	sidecarSrc := s.sidecarPath(bucket, key)
	sidecarDst := filepath.Join(s.root, quarantineSubdir, "sidecars", bucket, filepath.Base(sidecarSrc))

	if err := os.MkdirAll(filepath.Dir(sidecarDst), defaultDirPermissions); err == nil {
		_ = os.Rename(sidecarSrc, sidecarDst)
	}

	pruneEmptyDirs(filepath.Dir(src), filepath.Join(s.root, bucket))

	return nil
}

// verifyContent recomputes an object's MD5 and compares it to the stored
// checksum, returning fs.ErrIntegrity on mismatch. Used by verify-on-read.
func (s *Storage) verifyContent(bucket, key, path string) error {
	expected, ok := s.storedChecksum(bucket, key)
	if !ok {
		return nil // nothing to verify against
	}

	actual, err := fileMD5(path)
	if err != nil {
		return errors.Wrap(err, "verify content")
	}

	if actual != expected {
		return errors.Wrapf(fs.ErrIntegrity, "%s/%s: stored %s, computed %s",
			bucket, strings.TrimPrefix(key, "/"), expected, actual)
	}

	return nil
}
