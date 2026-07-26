package diskstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
)

// WalkFragments streams a disk's fragment names to fn in lexicographic order,
// holding one directory in memory at a time.
//
// It exists because List cannot be used on a full disk: it returns every name
// at once, and a disk holding tens of millions of fragments needs gigabytes of
// strings before the caller sees the first one. The scrubber consumes names in
// order and finishes with each object namespace before the next begins, so it
// never needed the whole list — only the order.
//
// Names arrive sorted because the tree is fixed-width by construction:
// "obj/<64 hex>/<64 hex>/<file>", where every sibling at a level has the same
// length. Directory-at-a-time traversal is therefore the same sequence as
// sorting the full paths would give, without doing the sort.
//
// after is a hint, not a filter. Names at or before it may be skipped — whole
// subtrees are pruned when they sort entirely before it, which is what makes a
// resumed scrub cheap — so a caller that needs an exact boundary must apply it
// itself.
//
// Returning an error from fn stops the walk and surfaces that error unchanged.
func (s *Store) WalkFragments(ctx context.Context, disk cluster.DiskID, after string, fn func(name string) error) error {
	root, ok := s.roots[disk]
	if !ok {
		return errors.Errorf("unknown disk %q", disk)
	}

	var stop error

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				// Racing a delete and its directory prune is expected.
				return nil
			}

			return err
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return errors.Wrap(err, "relativize fragment path")
		}

		name := filepath.ToSlash(rel)
		if name == "." {
			return nil
		}

		if d.IsDir() {
			if skipEntry(d.Name()) || prunable(name, after) {
				return filepath.SkipDir
			}

			return nil
		}

		if skipEntry(d.Name()) || name <= after {
			return nil
		}

		if err := fn(name); err != nil {
			stop = err

			return filepath.SkipAll
		}

		return nil
	})

	if stop != nil {
		return stop
	}

	if err != nil {
		return errors.Wrapf(err, "walk disk %q", disk)
	}

	return nil
}

// prunable reports whether a directory's entire subtree sorts at or before
// after, and so can be skipped without descending.
//
// Everything under dir is prefixed by dir+"/". If after starts with that
// prefix, the boundary falls inside the subtree and it must be walked. If dir
// sorts before after and the boundary is not inside it, every descendant sorts
// before after too — the fixed-width layout is what makes that true, since no
// sibling name can be a prefix of another.
func prunable(dir, after string) bool {
	if after == "" || dir >= after {
		return false
	}

	return !strings.HasPrefix(after, dir+"/")
}
