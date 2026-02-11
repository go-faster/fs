package validate

import (
	"strings"
	"unicode/utf8"

	"github.com/go-faster/errors"
)

// Key validates S3 object key according to AWS S3 specifications.
//
// AWS S3 object key naming rules:
// - Keys can be up to 1024 bytes in length
// - Keys can contain any UTF-8 character
// - However, we add security constraints to prevent path traversal attacks
func Key(key string) error {
	// Check for empty key
	if key == "" {
		return errors.New("key cannot be empty")
	}

	// Check length (AWS S3 allows up to 1024 bytes)
	if len(key) > 1024 {
		return errors.New("key length cannot exceed 1024 bytes")
	}

	// Validate UTF-8 encoding
	if !utf8.ValidString(key) {
		return errors.New("key must be valid UTF-8")
	}

	// Security: Prevent path traversal attacks
	// Check for parent directory references
	if strings.Contains(key, "..") {
		return errors.New("key cannot contain '..'")
	}

	// Security: Prevent Windows absolute paths (C:, D:, etc.)
	// Unix-style leading slash is allowed by S3 (though not recommended)
	if len(key) >= 2 && key[1] == ':' {
		// Looks like Windows drive letter
		return errors.New("key cannot be a Windows absolute path")
	}

	// Security: Backslashes are converted to forward slashes on some systems
	// We reject them to avoid confusion and prevent Windows-style paths
	if strings.Contains(key, "\\") {
		return errors.New("key cannot contain backslashes")
	}

	// Security: Prevent relative path references
	if strings.HasPrefix(key, "./") || strings.HasPrefix(key, "../") {
		return errors.New("key cannot start with './' or '../'")
	}

	// Security: Prevent /./ patterns in the middle of paths
	if strings.Contains(key, "/./") {
		return errors.New("key cannot contain '/./'")
	}

	// Check for null bytes (security issue)
	if strings.Contains(key, "\x00") {
		return errors.New("key cannot contain null bytes")
	}

	// AWS best practices: avoid certain characters even though they're technically allowed
	// We'll be more permissive than bucket names but still enforce some restrictions

	// Leading slash is technically allowed but discouraged
	// We allow it but document it
	if strings.HasPrefix(key, "/") {
		// AWS allows this but it's generally not recommended
		// We'll allow it but validate the rest of the path
		if key == "/" {
			return errors.New("key cannot be just '/'")
		}
	}

	// Check for control characters (except tab and newline which S3 allows but are problematic)
	for _, ch := range key {
		// Reject control characters that could cause issues
		if ch < 32 && ch != '\t' { // Allow printable chars, disallow most control chars
			return errors.New("key cannot contain control characters")
		}
		// Also reject DEL character
		if ch == 127 {
			return errors.New("key cannot contain DEL character")
		}
	}

	return nil
}
