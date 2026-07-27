package storagefs

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/go-faster/errors"
)

// migrateLayout moves objects written under the old key-as-path layout into
// the current one, where a key names a directory holding a reserved content
// leaf (see path.go).
//
// It runs at open. A store written by an older binary would otherwise read as
// empty — every object file sits where the new code looks for a directory —
// so this is not an optimization but the difference between finding the data
// and losing it. It is idempotent: an already-migrated store has nothing that
// matches, and an interrupted run resumes on the next open.
//
// The move is per object and crash-safe in the same way a write is: the file
// is renamed aside, the directory it will live in is created, and only then is
// it renamed into place. A crash leaves at most a staged file to be picked up
// next time, never a half-created key.
func (s *Storage) migrateLayout() error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return errors.Wrap(err, "read root")
	}

	for _, entry := range entries {
		// Internal directories are not buckets and hold no object keys.
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		if err := s.migrateBucket(filepath.Join(s.root, entry.Name())); err != nil {
			return errors.Wrapf(err, "migrate bucket %q", entry.Name())
		}
	}

	return nil
}

// migrateBucket converts every legacy object file in one bucket.
func (s *Storage) migrateBucket(bucketPath string) error {
	var legacy []string

	err := filepath.Walk(bucketPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() || info.Name() == objectFile {
			return nil
		}

		rel, relErr := filepath.Rel(bucketPath, path)
		if relErr != nil {
			return errors.Wrap(relErr, "relative path")
		}

		// Collect rather than convert in place: rewriting the tree underneath
		// a walk that is still reading it invites missed and re-visited
		// entries.
		legacy = append(legacy, rel)

		return nil
	})
	if err != nil {
		return errors.Wrap(err, "scan bucket")
	}

	for _, rel := range legacy {
		if err := s.migrateObject(bucketPath, rel); err != nil {
			return errors.Wrapf(err, "migrate %q", rel)
		}
	}

	return nil
}

// migrateObject moves one legacy object file, whose path under the bucket was
// the key itself, to the content leaf of its key directory.
func (s *Storage) migrateObject(bucketPath, rel string) error {
	key := filepath.ToSlash(rel)
	src := filepath.Join(bucketPath, rel)
	dst := filepath.Join(bucketPath, objectRelPath(key))

	if src == dst {
		return nil
	}

	// The key's directory cannot be created while the file occupies its name,
	// so stage the content aside first.
	staged, err := os.CreateTemp(s.stagingDir(), "migrate-*")
	if err != nil {
		return errors.Wrap(err, "create staging file")
	}

	stagedName := staged.Name()

	if err := staged.Close(); err != nil {
		return errors.Wrap(err, "close staging file")
	}

	if err := os.Remove(stagedName); err != nil {
		return errors.Wrap(err, "clear staging name")
	}

	if err := os.Rename(src, stagedName); err != nil {
		return errors.Wrap(err, "stage object")
	}

	if err := os.MkdirAll(filepath.Dir(dst), defaultDirPermissions); err != nil {
		return errors.Wrap(err, "create key directory")
	}

	if err := os.Rename(stagedName, dst); err != nil {
		return errors.Wrap(err, "install object")
	}

	// The old path may have left empty directories behind it once its file
	// moved; they are indistinguishable from key directories, so leave them.
	// A later listing ignores a directory with no content leaf under it.
	return nil
}
