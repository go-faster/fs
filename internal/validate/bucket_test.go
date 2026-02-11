package validate

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBucketName(t *testing.T) {
	tests := []struct {
		name      string
		bucket    string
		wantError bool
	}{
		// Valid bucket names
		{
			name:      "simple lowercase",
			bucket:    "mybucket",
			wantError: false,
		},
		{
			name:      "with numbers",
			bucket:    "bucket123",
			wantError: false,
		},
		{
			name:      "with hyphens",
			bucket:    "my-bucket",
			wantError: false,
		},
		{
			name:      "with dots",
			bucket:    "my.bucket",
			wantError: false,
		},
		{
			name:      "complex valid name",
			bucket:    "my-bucket.123",
			wantError: false,
		},
		{
			name:      "minimum length",
			bucket:    "abc",
			wantError: false,
		},
		{
			name:      "maximum length",
			bucket:    "a123456789012345678901234567890123456789012345678901234567890bc",
			wantError: false,
		},
		{
			name:      "starts with number",
			bucket:    "123bucket",
			wantError: false,
		},
		{
			name:      "ends with number",
			bucket:    "bucket123",
			wantError: false,
		},
		{
			name:      "dots and hyphens mixed",
			bucket:    "my-bucket.example-123",
			wantError: false,
		},

		// Path traversal attacks - SECURITY CRITICAL
		{
			name:      "parent directory traversal",
			bucket:    "../etc",
			wantError: true,
		},
		{
			name:      "current directory reference",
			bucket:    "./bucket",
			wantError: true,
		},
		{
			name:      "double parent directory",
			bucket:    "../../etc",
			wantError: true,
		},
		{
			name:      "parent in middle",
			bucket:    "bucket/../etc",
			wantError: true,
		},
		{
			name:      "parent at end",
			bucket:    "bucket/..",
			wantError: true,
		},
		{
			name:      "forward slash",
			bucket:    "bucket/path",
			wantError: true,
		},
		{
			name:      "multiple forward slashes",
			bucket:    "bucket/path/to/file",
			wantError: true,
		},
		{
			name:      "backslash",
			bucket:    "bucket\\path",
			wantError: true,
		},
		{
			name:      "multiple backslashes",
			bucket:    "bucket\\path\\to\\file",
			wantError: true,
		},
		{
			name:      "absolute path unix",
			bucket:    "/etc/passwd",
			wantError: true,
		},
		{
			name:      "absolute path unix root",
			bucket:    "/",
			wantError: true,
		},
		{
			name:      "absolute path windows",
			bucket:    "C:\\Windows",
			wantError: true,
		},
		{
			name:      "windows drive letter",
			bucket:    "C:",
			wantError: true,
		},
		{
			name:      "null byte injection",
			bucket:    "bucket\x00evil",
			wantError: true,
		},

		// Empty and length validation
		{
			name:      "empty name",
			bucket:    "",
			wantError: true,
		},
		{
			name:      "too short - 1 char",
			bucket:    "a",
			wantError: true,
		},
		{
			name:      "too short - 2 chars",
			bucket:    "ab",
			wantError: true,
		},
		{
			name:      "too long - 64 chars",
			bucket:    "a1234567890123456789012345678901234567890123456789012345678901bc",
			wantError: true,
		},
		{
			name:      "too long - 100 chars",
			bucket:    "this-is-a-very-long-bucket-name-that-exceeds-the-maximum-allowed-length-of-sixty-three-characters-total",
			wantError: true,
		},

		// Invalid AWS S3 bucket names - character validation
		{
			name:      "uppercase letters",
			bucket:    "MyBucket",
			wantError: true,
		},
		{
			name:      "all uppercase",
			bucket:    "MYBUCKET",
			wantError: true,
		},
		{
			name:      "mixed case",
			bucket:    "myBucket",
			wantError: true,
		},
		{
			name:      "starts with hyphen",
			bucket:    "-bucket",
			wantError: true,
		},
		{
			name:      "ends with hyphen",
			bucket:    "bucket-",
			wantError: true,
		},
		{
			name:      "starts with dot",
			bucket:    ".bucket",
			wantError: true,
		},
		{
			name:      "ends with dot",
			bucket:    "bucket.",
			wantError: true,
		},
		{
			name:      "consecutive dots",
			bucket:    "my..bucket",
			wantError: true,
		},
		{
			name:      "triple dots",
			bucket:    "my...bucket",
			wantError: true,
		},

		// IP address format (not allowed)
		{
			name:      "ip address format",
			bucket:    "192.168.1.1",
			wantError: true,
		},
		{
			name:      "ip address format - zeros",
			bucket:    "0.0.0.0",
			wantError: true,
		},
		{
			name:      "ip address format - high values",
			bucket:    "255.255.255.255",
			wantError: true,
		},
		{
			name:      "almost ip but with letter",
			bucket:    "192.168.1.a",
			wantError: false, // This is valid because it has a letter
		},
		{
			name:      "almost ip but 4 digits",
			bucket:    "1234.168.1.1",
			wantError: false, // Invalid IP format, so valid bucket name with dots
		},

		// Special characters not allowed
		{
			name:      "underscore",
			bucket:    "my_bucket",
			wantError: true,
		},
		{
			name:      "multiple underscores",
			bucket:    "my_test_bucket",
			wantError: true,
		},
		{
			name:      "at symbol",
			bucket:    "bucket@name",
			wantError: true,
		},
		{
			name:      "hash symbol",
			bucket:    "bucket#name",
			wantError: true,
		},
		{
			name:      "dollar sign",
			bucket:    "bucket$name",
			wantError: true,
		},
		{
			name:      "percent sign",
			bucket:    "bucket%name",
			wantError: true,
		},
		{
			name:      "ampersand",
			bucket:    "bucket&name",
			wantError: true,
		},
		{
			name:      "asterisk",
			bucket:    "bucket*name",
			wantError: true,
		},
		{
			name:      "plus sign",
			bucket:    "bucket+name",
			wantError: true,
		},
		{
			name:      "equals sign",
			bucket:    "bucket=name",
			wantError: true,
		},
		{
			name:      "square brackets",
			bucket:    "bucket[name]",
			wantError: true,
		},
		{
			name:      "curly braces",
			bucket:    "bucket{name}",
			wantError: true,
		},
		{
			name:      "parentheses",
			bucket:    "bucket(name)",
			wantError: true,
		},
		{
			name:      "angle brackets",
			bucket:    "bucket<name>",
			wantError: true,
		},
		{
			name:      "pipe",
			bucket:    "bucket|name",
			wantError: true,
		},
		{
			name:      "semicolon",
			bucket:    "bucket;name",
			wantError: true,
		},
		{
			name:      "colon",
			bucket:    "bucket:name",
			wantError: true,
		},
		{
			name:      "quote",
			bucket:    "bucket'name",
			wantError: true,
		},
		{
			name:      "double quote",
			bucket:    `bucket"name`,
			wantError: true,
		},
		{
			name:      "comma",
			bucket:    "bucket,name",
			wantError: true,
		},
		{
			name:      "question mark",
			bucket:    "bucket?name",
			wantError: true,
		},
		{
			name:      "exclamation mark",
			bucket:    "bucket!name",
			wantError: true,
		},
		{
			name:      "tilde",
			bucket:    "bucket~name",
			wantError: true,
		},
		{
			name:      "backtick",
			bucket:    "bucket`name",
			wantError: true,
		},

		// Whitespace characters
		{
			name:      "space",
			bucket:    "my bucket",
			wantError: true,
		},
		{
			name:      "leading space",
			bucket:    " bucket",
			wantError: true,
		},
		{
			name:      "trailing space",
			bucket:    "bucket ",
			wantError: true,
		},
		{
			name:      "tab character",
			bucket:    "my\tbucket",
			wantError: true,
		},
		{
			name:      "newline character",
			bucket:    "my\nbucket",
			wantError: true,
		},
		{
			name:      "carriage return",
			bucket:    "my\rbucket",
			wantError: true,
		},

		// Edge cases with dots and hyphens
		{
			name:      "single dot between valid chars",
			bucket:    "my.bucket",
			wantError: false,
		},
		{
			name:      "multiple dots separated",
			bucket:    "my.test.bucket",
			wantError: false,
		},
		{
			name:      "single hyphen between valid chars",
			bucket:    "my-bucket",
			wantError: false,
		},
		{
			name:      "multiple hyphens separated",
			bucket:    "my-test-bucket",
			wantError: false,
		},
		{
			name:      "hyphen before dot",
			bucket:    "my-.bucket",
			wantError: false,
		},
		{
			name:      "dot before hyphen",
			bucket:    "my.-bucket",
			wantError: false,
		},

		// Unicode and non-ASCII characters
		{
			name:      "unicode characters",
			bucket:    "mybucket文字",
			wantError: true,
		},
		{
			name:      "emoji",
			bucket:    "mybucket🚀",
			wantError: true,
		},
		{
			name:      "accented characters",
			bucket:    "mybuckét",
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := BucketName(tt.bucket)
			if tt.wantError {
				require.Error(t, err, "BucketName(%q) expected error, got nil", tt.bucket)
			} else {
				require.NoError(t, err, "BucketName(%q) unexpected error", tt.bucket)
			}
		})
	}
}

func TestBucketNameSecurityFocused(t *testing.T) {
	t.Run("prevents directory traversal", func(t *testing.T) {
		// Various directory traversal attempts
		attacks := []string{
			"../etc",
			"../../etc",
			"../../../etc",
			"bucket/../etc",
			"./bucket",
			"./../bucket",
			"bucket/..",
			"bucket/./file",
		}

		for _, attack := range attacks {
			err := BucketName(attack)
			require.Error(t, err, "should reject path traversal: %q", attack)
		}
	})

	t.Run("prevents absolute paths", func(t *testing.T) {
		attacks := []string{
			"/",
			"/etc",
			"/etc/passwd",
			"/var/www",
			"C:",
			"C:\\",
			"C:\\Windows",
			"D:\\data",
		}

		for _, attack := range attacks {
			err := BucketName(attack)
			require.Error(t, err, "should reject absolute path: %q", attack)
		}
	})

	t.Run("prevents path with slashes", func(t *testing.T) {
		attacks := []string{
			"bucket/file",
			"path/to/bucket",
			"bucket\\file",
			"path\\to\\bucket",
		}

		for _, attack := range attacks {
			err := BucketName(attack)
			require.Error(t, err, "should reject path with slashes: %q", attack)
		}
	})

	t.Run("path cleaning detects invalid elements", func(t *testing.T) {
		// These should be caught by filepath.Clean comparison
		attacks := []string{
			"bucket//file",
			"bucket/./file",
			"./bucket",
		}

		for _, attack := range attacks {
			err := BucketName(attack)
			require.Error(t, err, "should reject invalid path elements: %q", attack)
		}
	})
}

func TestBucketNameAWSCompliance(t *testing.T) {
	t.Run("follows AWS S3 naming rules", func(t *testing.T) {
		// Test valid names that comply with AWS rules
		validNames := []string{
			"mybucket",
			"my-bucket",
			"my.bucket",
			"my-bucket.test",
			"bucket123",
			"123bucket",
			"abc", // minimum length
			"a123456789012345678901234567890123456789012345678901234567890bc", // maximum length
		}

		for _, name := range validNames {
			err := BucketName(name)
			require.NoError(t, err, "valid AWS bucket name should be accepted: %q", name)
		}
	})

	t.Run("rejects names not compliant with AWS rules", func(t *testing.T) {
		invalidNames := []string{
			"ab",                 // too short
			"MyBucket",           // uppercase
			"-bucket",            // starts with hyphen
			"bucket-",            // ends with hyphen
			".bucket",            // starts with dot
			"bucket.",            // ends with dot
			"my..bucket",         // consecutive dots
			"192.168.1.1",        // IP address format
			"my_bucket",          // underscore
			"bucket name",        // space
			"bucket@example.com", // special char
		}

		for _, name := range invalidNames {
			err := BucketName(name)
			require.Error(t, err, "invalid AWS bucket name should be rejected: %q", name)
		}
	})

	t.Run("length boundaries", func(t *testing.T) {
		// Test exact boundaries
		require.Error(t, BucketName("ab"), "2 chars should be too short")
		require.NoError(t, BucketName("abc"), "3 chars should be minimum")

		// 63 characters - exactly at limit
		maxValid := "a123456789012345678901234567890123456789012345678901234567890bc"
		require.Len(t, maxValid, 63, "test string should be 63 chars")
		require.NoError(t, BucketName(maxValid), "63 chars should be at maximum")

		// 64 characters - over limit
		tooLong := "a1234567890123456789012345678901234567890123456789012345678901bc"
		require.Len(t, tooLong, 64, "test string should be 64 chars")
		require.Error(t, BucketName(tooLong), "64 chars should be too long")
	})
}

func BenchmarkBucketName(b *testing.B) {
	testCases := []struct {
		name   string
		bucket string
	}{
		{"valid simple", "mybucket"},
		{"valid complex", "my-bucket.test-123"},
		{"invalid traversal", "../etc"},
		{"invalid uppercase", "MyBucket"},
	}

	for _, tc := range testCases {
		b.Run(tc.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				_ = BucketName(tc.bucket)
			}
		})
	}
}
