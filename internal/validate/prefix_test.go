package validate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPrefix(t *testing.T) {
	tests := []struct {
		name      string
		prefix    string
		wantError bool
	}{
		// Valid prefixes - empty is valid
		{
			name:      "empty prefix",
			prefix:    "",
			wantError: false,
		},

		// Valid prefixes - simple cases
		{
			name:      "simple prefix",
			prefix:    "folder",
			wantError: false,
		},
		{
			name:      "prefix with slash",
			prefix:    "folder/",
			wantError: false,
		},
		{
			name:      "nested prefix",
			prefix:    "folder/subfolder/",
			wantError: false,
		},
		{
			name:      "deep nesting",
			prefix:    "a/b/c/d/e/",
			wantError: false,
		},
		{
			name:      "prefix without trailing slash",
			prefix:    "folder/subfolder",
			wantError: false,
		},
		{
			name:      "single character",
			prefix:    "a",
			wantError: false,
		},
		{
			name:      "with numbers",
			prefix:    "logs/2023/01/",
			wantError: false,
		},

		// Valid prefixes - special characters
		{
			name:      "with hyphen",
			prefix:    "my-folder/",
			wantError: false,
		},
		{
			name:      "with underscore",
			prefix:    "my_folder/",
			wantError: false,
		},
		{
			name:      "with dots",
			prefix:    "folder.backup/",
			wantError: false,
		},
		{
			name:      "with spaces",
			prefix:    "my folder/",
			wantError: false,
		},
		{
			name:      "with special chars",
			prefix:    "data-2023_v1/",
			wantError: false,
		},
		{
			name:      "with parentheses",
			prefix:    "folder(1)/",
			wantError: false,
		},
		{
			name:      "with brackets",
			prefix:    "folder[test]/",
			wantError: false,
		},
		{
			name:      "with at symbol",
			prefix:    "user@domain/",
			wantError: false,
		},
		{
			name:      "with hash",
			prefix:    "tag#1/",
			wantError: false,
		},
		{
			name:      "with dollar",
			prefix:    "price$100/",
			wantError: false,
		},
		{
			name:      "with percent",
			prefix:    "discount%/",
			wantError: false,
		},
		{
			name:      "with plus",
			prefix:    "item+extra/",
			wantError: false,
		},
		{
			name:      "with equals",
			prefix:    "key=value/",
			wantError: false,
		},

		// Valid prefixes - leading slash
		{
			name:      "with leading slash",
			prefix:    "/folder/",
			wantError: false,
		},
		{
			name:      "leading slash no trailing",
			prefix:    "/folder",
			wantError: false,
		},

		// Valid prefixes - tab character
		{
			name:      "with tab",
			prefix:    "folder\t/",
			wantError: false,
		},

		// Valid prefixes - Unicode
		{
			name:      "unicode characters",
			prefix:    "文件夹/",
			wantError: false,
		},
		{
			name:      "emoji prefix",
			prefix:    "📁folder/",
			wantError: false,
		},
		{
			name:      "accented characters",
			prefix:    "café/",
			wantError: false,
		},
		{
			name:      "cyrillic characters",
			prefix:    "папка/",
			wantError: false,
		},
		{
			name:      "mixed unicode",
			prefix:    "folder/文件/",
			wantError: false,
		},

		// Invalid prefixes - path traversal (SECURITY)
		{
			name:      "parent directory",
			prefix:    "../",
			wantError: true,
		},
		{
			name:      "double parent",
			prefix:    "../../",
			wantError: true,
		},
		{
			name:      "parent in middle",
			prefix:    "folder/../",
			wantError: true,
		},
		{
			name:      "parent without slash",
			prefix:    "..",
			wantError: true,
		},
		{
			name:      "parent at end",
			prefix:    "folder/..",
			wantError: true,
		},
		{
			name:      "current directory",
			prefix:    "./",
			wantError: true,
		},
		{
			name:      "current in middle",
			prefix:    "folder/./",
			wantError: true,
		},
		{
			name:      "hidden parent reference",
			prefix:    "folder/..hidden/",
			wantError: true,
		},

		// Invalid prefixes - backslashes
		{
			name:      "backslash",
			prefix:    "folder\\",
			wantError: true,
		},
		{
			name:      "multiple backslashes",
			prefix:    "folder\\subfolder\\",
			wantError: true,
		},
		{
			name:      "windows path",
			prefix:    "C:\\Users\\",
			wantError: true,
		},

		// Invalid prefixes - null bytes
		{
			name:      "null byte",
			prefix:    "folder\x00/",
			wantError: true,
		},
		{
			name:      "null byte at start",
			prefix:    "\x00folder/",
			wantError: true,
		},
		{
			name:      "null byte at end",
			prefix:    "folder/\x00",
			wantError: true,
		},

		// Invalid prefixes - control characters
		{
			name:      "newline",
			prefix:    "folder\n/",
			wantError: true,
		},
		{
			name:      "carriage return",
			prefix:    "folder\r/",
			wantError: true,
		},
		{
			name:      "bell character",
			prefix:    "folder\a/",
			wantError: true,
		},
		{
			name:      "escape character",
			prefix:    "folder\x1b/",
			wantError: true,
		},
		{
			name:      "DEL character",
			prefix:    "folder\x7f/",
			wantError: true,
		},
		{
			name:      "null character",
			prefix:    "folder\x00/",
			wantError: true,
		},

		// Invalid prefixes - invalid UTF-8
		{
			name:      "invalid UTF-8",
			prefix:    "folder\xff\xfe/",
			wantError: true,
		},
		{
			name:      "broken UTF-8",
			prefix:    "folder\xc0\xc1/",
			wantError: true,
		},

		// Length validation
		{
			name:      "at max length",
			prefix:    strings.Repeat("a", 1024),
			wantError: false,
		},
		{
			name:      "over max length",
			prefix:    strings.Repeat("a", 1025),
			wantError: true,
		},
		{
			name:      "way over max length",
			prefix:    strings.Repeat("a", 2000),
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Prefix(tt.prefix)
			if tt.wantError {
				require.Error(t, err, "Prefix(%q) expected error, got nil", tt.prefix)
			} else {
				require.NoError(t, err, "Prefix(%q) unexpected error", tt.prefix)
			}
		})
	}
}

func TestPrefixSecurityFocused(t *testing.T) {
	t.Run("prevents path traversal", func(t *testing.T) {
		attacks := []string{
			"../",
			"../../",
			"../../../",
			"folder/../",
			"./../",
			"folder/..",
			"..",
			"a/../b/",
		}

		for _, attack := range attacks {
			err := Prefix(attack)
			require.Error(t, err, "should reject path traversal: %q", attack)
		}
	})

	t.Run("prevents backslash usage", func(t *testing.T) {
		attacks := []string{
			"folder\\",
			"C:\\Windows\\",
			"folder\\subfolder\\",
			"\\folder\\",
		}

		for _, attack := range attacks {
			err := Prefix(attack)
			require.Error(t, err, "should reject backslashes: %q", attack)
		}
	})

	t.Run("prevents null byte injection", func(t *testing.T) {
		attacks := []string{
			"folder\x00/",
			"\x00folder/",
			"folder/\x00",
			"a\x00b/",
		}

		for _, attack := range attacks {
			err := Prefix(attack)
			require.Error(t, err, "should reject null bytes: %q", attack)
		}
	})

	t.Run("prevents control character injection", func(t *testing.T) {
		attacks := []string{
			"folder\n/",
			"folder\r/",
			"folder\a/",
			"folder\b/",
			"folder\f/",
			"folder\v/",
			"folder\x1b/",
			"folder\x7f/",
		}

		for _, attack := range attacks {
			err := Prefix(attack)
			require.Error(t, err, "should reject control characters: %q", attack)
		}
	})

	t.Run("prevents relative path references", func(t *testing.T) {
		attacks := []string{
			"./",
			"./folder/",
			"folder/./",
			"../folder/",
		}

		for _, attack := range attacks {
			err := Prefix(attack)
			require.Error(t, err, "should reject relative path references: %q", attack)
		}
	})
}

func TestPrefixS3Usage(t *testing.T) {
	t.Run("common S3 prefix patterns", func(t *testing.T) {
		patterns := []string{
			"",                         // List all objects
			"images/",                  // List images folder
			"logs/2023/",               // List logs by year
			"logs/2023/01/",            // List logs by month
			"logs/2023/01/15/",         // List logs by day
			"backups/database-",        // Prefix without trailing slash
			"uploads/user123/",         // User-specific prefix
			"documents/invoices/2023/", // Nested business documents
			"data/exports/",            // Data exports folder
			"/absolute/path/style/",    // Leading slash style
			"my-app/production/logs/",  // Multi-level with hyphens
			"folder_with_underscores/", // Underscores in name
			"folder.with.dots/",        // Dots in name
			"special!chars@folder#1/",  // Special characters
		}

		for _, pattern := range patterns {
			err := Prefix(pattern)
			require.NoError(t, err, "common S3 pattern should be accepted: %q", pattern)
		}
	})

	t.Run("empty prefix lists all", func(t *testing.T) {
		// Empty prefix is valid and means list all objects
		err := Prefix("")
		require.NoError(t, err, "empty prefix should be valid")
	})

	t.Run("trailing slash convention", func(t *testing.T) {
		// Both with and without trailing slash are valid
		require.NoError(t, Prefix("folder/"))
		require.NoError(t, Prefix("folder"))
		require.NoError(t, Prefix("folder/subfolder/"))
		require.NoError(t, Prefix("folder/subfolder"))
	})

	t.Run("date-based prefixes", func(t *testing.T) {
		// Common pattern for organizing by date
		datePrefixes := []string{
			"logs/2023/",
			"logs/2023/01/",
			"logs/2023/01/15/",
			"backups/2023-01-15/",
			"uploads/2023-01-15T10:30:00Z/",
		}

		for _, prefix := range datePrefixes {
			err := Prefix(prefix)
			require.NoError(t, err, "date prefix should be valid: %q", prefix)
		}
	})
}

func TestPrefixUTF8Validation(t *testing.T) {
	t.Run("valid UTF-8", func(t *testing.T) {
		validUTF8 := []string{
			"hello/",
			"世界/",
			"🌍🌎🌏/",
			"café/",
			"Москва/",
			"こんにちは/",
		}

		for _, prefix := range validUTF8 {
			err := Prefix(prefix)
			require.NoError(t, err, "valid UTF-8 should be accepted: %q", prefix)
		}
	})

	t.Run("invalid UTF-8", func(t *testing.T) {
		invalidUTF8 := []string{
			"hello\xff\xfe/",
			"world\xc0\xc1/",
			"\xf5\xf6\xf7/",
		}

		for _, prefix := range invalidUTF8 {
			err := Prefix(prefix)
			require.Error(t, err, "invalid UTF-8 should be rejected: %q", prefix)
		}
	})
}

func TestPrefixEdgeCases(t *testing.T) {
	t.Run("length boundaries", func(t *testing.T) {
		// Test at boundary
		require.NoError(t, Prefix(strings.Repeat("a", 1024)), "1024 bytes should be at max")
		require.Error(t, Prefix(strings.Repeat("a", 1025)), "1025 bytes should exceed max")

		// Test with multi-byte UTF-8
		emoji := "😀"
		require.Equal(t, 4, len(emoji), "emoji should be 4 bytes")

		// 256 emojis = 1024 bytes
		maxEmoji := strings.Repeat(emoji, 256)
		require.Equal(t, 1024, len(maxEmoji))
		require.NoError(t, Prefix(maxEmoji), "1024 bytes of emoji should be valid")

		// 257 emojis = 1028 bytes
		overEmoji := strings.Repeat(emoji, 257)
		require.Equal(t, 1028, len(overEmoji))
		require.Error(t, Prefix(overEmoji), "1028 bytes of emoji should be invalid")
	})

	t.Run("dots in prefix", func(t *testing.T) {
		// Single dots are fine
		require.NoError(t, Prefix("folder.backup/"))
		require.NoError(t, Prefix(".hidden/"))
		require.NoError(t, Prefix("folder/.hidden/"))

		// But .. is not allowed
		require.Error(t, Prefix("folder/../"))
		require.Error(t, Prefix(".."))
		require.Error(t, Prefix("../"))
	})

	t.Run("hidden folders", func(t *testing.T) {
		// Unix-style hidden folders starting with dot
		require.NoError(t, Prefix(".git/"))
		require.NoError(t, Prefix(".config/"))
		require.NoError(t, Prefix("folder/.hidden/"))
		require.NoError(t, Prefix("."))
	})

	t.Run("special prefixes", func(t *testing.T) {
		require.NoError(t, Prefix("~backup/"))
		require.NoError(t, Prefix("_internal/"))
		require.NoError(t, Prefix("$RECYCLE.BIN/"))
	})
}

func TestPrefixComparison(t *testing.T) {
	t.Run("prefix vs key validation differences", func(t *testing.T) {
		// Empty prefix is valid, but empty key is not
		require.NoError(t, Prefix(""))
		require.Error(t, Key(""))

		// Both should reject path traversal
		require.Error(t, Prefix("../"))
		require.Error(t, Key("../file.txt"))

		// Both should reject backslashes
		require.Error(t, Prefix("folder\\"))
		require.Error(t, Key("folder\\file.txt"))

		// Both should accept normal paths
		require.NoError(t, Prefix("folder/"))
		require.NoError(t, Key("folder/file.txt"))
	})
}

func BenchmarkPrefix(b *testing.B) {
	testCases := []struct {
		name   string
		prefix string
	}{
		{"empty", ""},
		{"simple", "folder/"},
		{"nested", "folder/subfolder/file-"},
		{"with spaces", "my folder/"},
		{"unicode", "文件夹/📁/"},
		{"invalid traversal", "../"},
		{"long prefix", strings.Repeat("a/", 100)},
	}

	for _, tc := range testCases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = Prefix(tc.prefix)
			}
		})
	}
}
