package validate

import (
	"path/filepath"
	"strings"

	"github.com/go-faster/errors"
)

// BucketName validates the bucket name according to AWS S3 naming rules
// and protects against path traversal attacks.
func BucketName(name string) error {
	// Check for empty name
	if name == "" {
		return errors.New("bucket name cannot be empty")
	}

	// Check length (3-63 characters for AWS S3)
	if len(name) < 3 || len(name) > 63 {
		return errors.New("bucket name must be between 3 and 63 characters")
	}

	// Prevent path traversal attacks
	if strings.Contains(name, "..") {
		return errors.New("bucket name cannot contain '..'")
	}
	if strings.Contains(name, "/") {
		return errors.New("bucket name cannot contain '/'")
	}
	if strings.Contains(name, "\\") {
		return errors.New("bucket name cannot contain '\\'")
	}

	// Check for absolute paths
	if filepath.IsAbs(name) {
		return errors.New("bucket name cannot be an absolute path")
	}

	// Ensure the name doesn't try to escape root
	// Clean path and compare with original
	cleaned := filepath.Clean(name)
	if cleaned != name {
		return errors.New("bucket name contains invalid path elements")
	}

	// AWS S3 rules: lowercase letters, numbers, dots, and hyphens
	// Must start and end with letter or number
	if !isValidS3BucketName(name) {
		return errors.New("bucket name must start and end with lowercase letter or number, and contain only lowercase letters, numbers, dots, and hyphens")
	}

	return nil
}

// isValidS3BucketName checks if name follows AWS S3 bucket naming rules
func isValidS3BucketName(name string) bool {
	// Must start with lowercase letter or number
	first := name[0]
	if (first < 'a' || first > 'z') && (first < '0' || first > '9') {
		return false
	}

	// Must end with lowercase letter or number
	last := name[len(name)-1]
	if (last < 'a' || last > 'z') && (last < '0' || last > '9') {
		return false
	}

	// Check all characters
	for _, ch := range name {
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '.' && ch != '-' {
			return false
		}
	}

	// Cannot contain consecutive periods (already checked in BucketName but double-check)
	if strings.Contains(name, "..") {
		return false
	}

	// Cannot be formatted as an IP address (simple check)
	if strings.Count(name, ".") == 3 {
		parts := strings.Split(name, ".")
		if len(parts) == 4 {
			allNumeric := true
			for _, part := range parts {
				if part == "" || len(part) > 3 {
					allNumeric = false
					break
				}
				for _, ch := range part {
					if ch < '0' || ch > '9' {
						allNumeric = false
						break
					}
				}
			}
			if allNumeric {
				return false
			}
		}
	}

	return true
}
