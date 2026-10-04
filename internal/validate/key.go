package validate

import (
	"unicode/utf8"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
)

// maxKeyLen is the S3 limit on an object key, in bytes. Prefixes share it.
const maxKeyLen = 1024

// Key validates an S3 object key: 1 to 1024 bytes of UTF-8, as S3 has it.
//
// A key is a name, never a path: the engine stores it as a table sort key and
// nothing resolves it on a filesystem, so "..", "//", backslashes and the
// like are ordinary characters, as they are on S3. What is refused is what
// the protocol cannot carry: NUL and other control characters, which XML
// listings cannot represent, and the keys ".", ".." and "/", which a client's
// URL normalization would never deliver intact.
func Key(key string) error {
	if key == "" {
		return errors.Wrap(fs.ErrInvalidKey, "key cannot be empty")
	}

	if len(key) > maxKeyLen {
		return errors.Wrap(fs.ErrInvalidKey, "key length cannot exceed 1024 bytes")
	}

	if !utf8.ValidString(key) {
		return errors.Wrap(fs.ErrInvalidKey, "key must be valid UTF-8")
	}

	switch key {
	case ".", "..", "/":
		return errors.Wrapf(fs.ErrInvalidKey, "key cannot be %q", key)
	}

	for _, ch := range key {
		if ch < 32 && ch != '\t' || ch == 127 {
			return errors.Wrap(fs.ErrInvalidKey, "key cannot contain control characters")
		}
	}

	return nil
}
