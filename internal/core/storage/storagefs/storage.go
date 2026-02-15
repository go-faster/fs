// Package storagefs implements fs.Storage.
package storagefs

import (
	"fmt"
	"os"

	"github.com/go-faster/fs"
)

var _ fs.Storage = (*Storage)(nil)

func New(root string) (*Storage, error) {
	if err := os.MkdirAll(root, 0750); err != nil {
		return nil, fmt.Errorf("failed to create root directory: %w", err)
	}

	return &Storage{
		root:      root,
		multipart: newMultipartManager(),
	}, nil
}

type Storage struct {
	root      string
	multipart *multipartManager
}
