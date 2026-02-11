package validate

import (
	"strings"
	"unicode/utf8"

	"github.com/go-faster/errors"
)

// Prefix validates S3 object prefix for listing operations.
//
// Prefixes are used to filter objects in ListObjects operations.
// They follow similar rules to keys but are more lenient since they're for filtering.
// An empty prefix is valid (lists all objects).
func Prefix(prefix string) error {
	// Empty prefix is valid - means list all objects
	if prefix == "" {
		return nil
	}

	// Check length (same limit as keys: 1024 bytes)
	if len(prefix) > 1024 {
		return errors.New("prefix length cannot exceed 1024 bytes")
	}

	// Validate UTF-8 encoding
	if !utf8.ValidString(prefix) {
		return errors.New("prefix must be valid UTF-8")
	}

	// Security: Prevent path traversal attacks
	if strings.Contains(prefix, "..") {
		return errors.New("prefix cannot contain '..'")
	}

	// Security: Prevent Windows absolute paths
	if len(prefix) >= 2 && prefix[1] == ':' {
		return errors.New("prefix cannot be a Windows absolute path")
	}

	// Security: Prevent backslashes (Windows-style paths)
	if strings.Contains(prefix, "\\") {
		return errors.New("prefix cannot contain backslashes")
	}

	// Security: Prevent relative path references
	if strings.HasPrefix(prefix, "./") || strings.HasPrefix(prefix, "../") {
		return errors.New("prefix cannot start with './' or '../'")
	}

	// Security: Prevent /./ patterns
	if strings.Contains(prefix, "/./") {
		return errors.New("prefix cannot contain '/./'")
	}

	// Check for null bytes (security issue)
	if strings.Contains(prefix, "\x00") {
		return errors.New("prefix cannot contain null bytes")
	}

	// Check for control characters
	for _, ch := range prefix {
		if ch < 32 && ch != '\t' {
			return errors.New("prefix cannot contain control characters")
		}
		if ch == 127 {
			return errors.New("prefix cannot contain DEL character")
		}
	}

	return nil
}
