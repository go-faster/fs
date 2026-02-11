package storagefs

import (
	"context"
	"os"
	"path/filepath"

	"github.com/go-faster/errors"
)

// DeleteObject deletes the specified object from the bucket.
//
// NB: bucket and key are already sanitized.
func (s *Storage) DeleteObject(ctx context.Context, bucket, key string) error {
	objectPath := filepath.Join(s.root, bucket, key)

	if err := os.Remove(objectPath); err != nil {
		if os.IsNotExist(err) {
			return errors.New("object not found")
		}
		return errors.Wrap(err, "delete object")
	}

	return nil
}
