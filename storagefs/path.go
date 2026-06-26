package storagefs

import (
	"path/filepath"
	"strings"
)

// toOSPath converts an S3-style key (using forward slashes) to a native OS path.
// On Windows, this converts "path/to/file.txt" to "path\to\file.txt".
// On Unix, forward slashes are already the separator, so it returns the key as-is.
func toOSPath(key string) string {
	if filepath.Separator == '/' {
		// Unix-like systems already use forward slashes.
		return key
	}

	// On Windows, convert forward slashes to backslashes.
	return strings.ReplaceAll(key, "/", string(filepath.Separator))
}
