package validate

import (
	"unicode/utf8"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

// Prefix validates an S3 object prefix for listing operations.
//
// A prefix is a pure filter — backends match it against stored keys with a
// string comparison and never resolve it as a path — so it carries none of the
// traversal constraints Key does. S3 accepts any byte sequence here, including
// control characters and ".." segments, and simply returns no matches when
// nothing starts with it. The only limits are the 1024-byte key length and
// valid UTF-8. An empty prefix is valid and lists everything.
func Prefix(prefix string) error {
	if prefix == "" {
		return nil
	}

	if len(prefix) > maxKeyLen {
		return errors.Wrap(fs.ErrInvalidKey, "prefix length cannot exceed 1024 bytes")
	}

	if !utf8.ValidString(prefix) {
		return errors.Wrap(fs.ErrInvalidKey, "prefix must be valid UTF-8")
	}

	return nil
}
