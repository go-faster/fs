package storagefs

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/go-faster/fs"
)

func (s *Storage) ListBuckets(ctx context.Context) ([]fs.Bucket, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("failed to read buckets: %w", err)
	}

	var buckets []fs.Bucket

	for _, entry := range entries {
		if entry.IsDir() {
			// Internal directories (.meta, .multipart) are never buckets: S3
			// bucket names cannot start with a dot-prefix like these.
			if strings.HasPrefix(entry.Name(), ".") {
				continue
			}

			info, err := entry.Info()
			if err != nil {
				continue
			}

			buckets = append(buckets, fs.Bucket{
				Name:         entry.Name(),
				CreationDate: info.ModTime(),
			})
		}
	}

	return buckets, nil
}
