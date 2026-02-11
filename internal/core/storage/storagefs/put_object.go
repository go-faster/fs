package storagefs

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

func (s *Storage) PutObject(ctx context.Context, req *fs.PutObjectRequest) error {
	objectPath := filepath.Join(s.root, req.Bucket, req.Key)
	if err := os.MkdirAll(filepath.Dir(objectPath), defaultDirPermissions); err != nil {
		return errors.Wrap(err, "create object directory")
	}

	// #nosec G304 -- objectPath is constructed from validated bucket and key.
	f, err := os.Create(objectPath)
	if err != nil {
		return errors.Wrap(err, "create object")
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = errors.Join(err, closeErr)
		}
	}()

	if _, err := io.Copy(f, req.Reader); err != nil {
		return fmt.Errorf("failed to write object: %w", err)
	}

	return nil
}
